package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/wjsoj/cc-core/advisor"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/downstream"
	"github.com/wjsoj/cc-core/mimicry"
	"github.com/wjsoj/cc-core/relay"
	"github.com/wjsoj/cc-core/requestlog"
	"github.com/wjsoj/cc-core/sidecar"
	ccstream "github.com/wjsoj/cc-core/stream"
	"github.com/wjsoj/cc-core/thinkingsig"
	"github.com/wjsoj/cc-core/usage"
)

// hopHeaders are stripped when forwarding to upstream.
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	// Anthropic auth is set by us — strip anything the client sent.
	"Authorization": true,
	"X-Api-Key":     true,
	"X-Client-Ip":   true,
}

func (s *Server) handleMessages(c *gin.Context) {
	s.forward(c, auth.ProviderAnthropic, "/v1/messages")
}

// forward runs the per-provider retry loop and credential routing for a
// single client request. `provider` picks the credential pool subset; `path`
// is the provider-native upstream path. doForward still assumes Anthropic
// semantics for request shaping — Codex has its own doForward variant (see
// codex_proxy.go) which this dispatcher will call once provider != anthropic.
func (s *Server) forward(c *gin.Context, provider, path string) {
	clientTok, _ := c.Get("client_token")
	clientToken, _ := clientTok.(string)
	if clientToken == "" {
		clientToken = c.ClientIP()
	}
	clientNameV, _ := c.Get("client_name")
	clientName, _ := clientNameV.(string)
	start := time.Now()

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeAPIError(c, provider, APIError{Status: http.StatusBadRequest, Code: "invalid_request_body", Message: "The request body could not be read. Send valid JSON and try again."})
		return
	}

	// Per-window slot identity. Each Claude Code CLI window sends a distinct
	// X-Claude-Code-Session-Id, so the same user opening multiple windows is
	// scheduled as multiple independent slots (and can land on different
	// upstream credentials).
	slotID := clientSlotID(c)
	if slotID == "" {
		// Nothing on the wire named a session. Before falling back to one slot
		// per token — which pins every caller behind a third-party relay to a
		// single credential — try to recover the conversation from the body and
		// spread it over a bounded set of buckets. See relay_fanout.go.
		slotID = fanoutSlotID(body, sessionlessFanoutWidth)
	}

	// Parse minimal request metadata for usage reporting + streaming detection.
	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)
	model := peek.Model
	if model == "" {
		model = "unknown"
	}

	// Refuse a Codex model no credential can serve, here, rather than letting
	// the failover loop discover it. Upstream does not reject an unrecognised
	// model — it accepts the turn and never schedules it — so without this the
	// request burns the stall budget on credential after credential and answers
	// 503 minutes later without ever naming what was wrong. See
	// codex_model_guard.go.
	if auth.NormalizeProvider(provider) == auth.ProviderOpenAI {
		route := s.guardCodexModel(model)
		if route.reject != nil {
			writeAPIError(c, provider, APIError{
				Status: route.reject.Status, Code: route.reject.Code, Message: route.reject.Message,
				Details: map[string]any{"model": route.reject.Model, "suggestion": route.reject.Suggestion},
			})
			s.emitLog(requestlog.Record{
				Client: clientName, ClientToken: maskClientToken(clientToken), Provider: provider, Model: model,
				Stream: peek.Stream, Path: path, Status: route.reject.Status,
				DurationMs: time.Since(start).Milliseconds(), Error: route.reject.Message,
			})
			return
		}
		if route.apiKeyOnly {
			// A real model, but one no subscription tier serves. Handing it to
			// an OAuth credential is exactly what parks the turn.
			c.Set(codexAPIKeyOnlyModelKey, true)
		}
	}

	// SaaS path: balance + per-token caps pre-check; credential group from user's
	// pricing group. Legacy weekly-budget logic still applies to non-SaaS tokens.
	saasInfo, saasOK := saasInfoFrom(c)
	var clientGroup string
	if saasOK && s.saas != nil {
		clientGroup = s.saas.CredentialGroup(saasInfo)
		if pre := s.saas.PreCheck(c.Request.Context(), saasInfo); pre != nil {
			writeAPIError(c, provider, APIError{Status: pre.Status, Code: pre.Code, Message: pre.Message, Details: pre.Details})
			s.emitLog(requestlog.Record{
				Client: clientName, ClientToken: maskClientToken(clientToken), Provider: provider, Model: model,
				Stream: peek.Stream, Path: path, Status: pre.Status,
				DurationMs: time.Since(start).Milliseconds(), Error: "saas pre-check rejected",
			})
			return
		}
	} else {
		entry, tokOK := s.tokens.Lookup(clientToken)
		var weeklyLimit float64
		if tokOK {
			weeklyLimit = entry.WeeklyUSD
			clientGroup = entry.Group
		}
		if tokOK && weeklyLimit > 0 {
			spent := s.usage.WeeklyCostUSD(clientToken)
			if spent >= weeklyLimit {
				c.Header("Retry-After", "604800")
				writeAPIError(c, provider, APIError{Status: http.StatusTooManyRequests, Code: "api_key_weekly_limit_exceeded", Message: "This API key has reached its weekly spending limit. Increase the key limit or retry after the weekly reset.", Details: map[string]any{"spent_usd": spent, "limit_usd": weeklyLimit, "week": s.usage.CurrentWeekKey()}})
				s.emitLog(requestlog.Record{
					Client:      clientName,
					ClientToken: maskClientToken(clientToken),
					Provider:    provider,
					Model:       model,
					Stream:      peek.Stream,
					Path:        path,
					Status:      429,
					DurationMs:  time.Since(start).Milliseconds(),
					Error:       "weekly budget exceeded",
				})
				return
			}
		}
	}

	// There used to be a fail-fast here that rejected /v1/chat/completions
	// outright when no API-key credential could serve the model, because OAuth
	// Codex credentials only spoke /v1/responses. That is no longer true:
	// codex_chat_bridge.go translates the route onto the backend's
	// /codex/responses, so an OAuth credential is a legitimate candidate and
	// the normal failover loop (OAuth → model-unsupported rollback → API key)
	// resolves the route correctly.

	// Rate limit (RPM) per client token. Sliding 60s window; scoped
	// per-provider to match the inflight budget so Claude and Codex don't
	// share one cap. Checked before the concurrency gate so a burst of
	// 429s doesn't briefly occupy slots.
	rpmKey := auth.NormalizeProvider(provider) + "|" + clientToken
	if limit := s.clientRPM(c, provider, clientToken); limit > 0 {
		if ok, retry := s.rpm.Allow(rpmKey, limit); !ok {
			c.Header("Retry-After", strconv.Itoa(retry))
			writeAPIError(c, provider, APIError{Status: http.StatusTooManyRequests, Code: "api_key_rate_limit_exceeded", Message: "This API key has exceeded its requests-per-minute limit. Retry after the indicated delay.", Details: map[string]any{"rpm_limit": limit, "retry_after_seconds": retry}})
			s.emitLog(requestlog.Record{
				Client:      clientName,
				ClientToken: maskClientToken(clientToken),
				Provider:    provider,
				Model:       model,
				Stream:      peek.Stream,
				Path:        path,
				Status:      429,
				DurationMs:  time.Since(start).Milliseconds(),
				Error:       "rpm limit exceeded",
			})
			return
		}
	}

	// Concurrency limit per client token.
	maxConc := s.clientMaxConcurrent(c, provider, clientToken)
	if maxConc > 0 {
		// Scope the counter per provider so Claude and Codex share a token
		// but not a concurrency bucket — matches the per-provider session
		// keying in Pool.Acquire.
		inflightKey := auth.NormalizeProvider(provider) + "|" + clientToken
		cur, releaseSlot := s.inflight.Begin(inflightKey)
		defer releaseSlot()
		if int(cur) > maxConc {
			c.Header("Retry-After", "5")
			writeAPIError(c, provider, APIError{Status: http.StatusTooManyRequests, Code: "api_key_concurrency_limit_exceeded", Message: "This API key has too many requests in progress. Wait for an active request to finish and try again.", Details: map[string]any{"max_concurrent": maxConc, "in_flight": int(cur)}})
			s.emitLog(requestlog.Record{
				Client:      clientName,
				ClientToken: maskClientToken(clientToken),
				Provider:    provider,
				Model:       model,
				Stream:      peek.Stream,
				Path:        path,
				Status:      429,
				DurationMs:  time.Since(start).Milliseconds(),
				Error:       "concurrent limit exceeded",
			})
			return
		}
	}

	// Hand off to the credential-failover retry loop.
	s.forwardWithFailover(c, provider, path, model, clientToken, clientGroup, clientName, slotID, body, peek.Stream, start)
}

// forwardWithFailover runs the per-request retry loop: it acquires a
// credential, forwards via the provider-appropriate doForward, and on a
// credential-level error (429 quota/rate-limit, 401/403, account ban — which
// doForward withholds rather than writing through) transparently switches to
// another healthy credential. The user only ever sees an error when the pool
// has no slot left: excludeIDs narrows the candidate set each round so the
// loop terminates naturally once Acquire returns nil (every healthy credential
// tried). maxAttempts is only a backstop against a pathologically large
// all-failing fleet. When every credential is exhausted, the most recent
// withheld upstream error is replayed verbatim (e.g. a 429 + Retry-After)
// instead of a synthetic 503, so clients back off correctly.
func (s *Server) forwardWithFailover(c *gin.Context, provider, path, model, clientToken, clientGroup, clientName, slotID string, body []byte, stream bool, start time.Time) {
	// How many credentials one request may burn.
	//
	// Twelve for Anthropic, where a credential-level failure is usually a 429
	// or a 401 and the next credential genuinely is a fresh roll of the dice.
	//
	// Four for Codex, because production says the roll is not fresh at all.
	// Measured across an hour of an upstream capacity storm: requests served by
	// the FIRST credential produced output 32.4% of the time (58/179), and
	// requests that had been shed and rotated produced output 4.2% of the time
	// (20/472). Rotation is not rescuing these turns — something about the turn
	// itself is being refused everywhere — so the eight extra rounds buy almost
	// nothing and cost the caller the better part of two minutes. A client that
	// gets its answer quickly and asks again starts a fresh turn with the 32%
	// odds, which is eight times better than the rotation it replaces.
	maxAttempts := 12
	if auth.NormalizeProvider(provider) == auth.ProviderOpenAI {
		maxAttempts = 4
	}
	// A retry only helps while someone is still waiting for the answer.
	// maxAttempts alone assumed attempts were cheap, which held while a
	// credential-level failure was an immediate 429 or 401. It stopped holding
	// when the Codex WebSocket path learned to recognise a turn the backend
	// parked: that attempt costs the whole stall budget before it sheds, and
	// twelve of them in series is a twenty-minute request. Production recorded
	// one on attempt 10 at 18m25s, long after the client had given up, still
	// holding a pool slot and still burning credentials.
	//
	// Four minutes sits under the five-minute idle deadline Codex clients keep
	// on a Responses stream, so the loop stops before the caller does. It
	// bounds only the decision to *start* another attempt — a single long
	// stream that is actually producing output runs as long as it likes.
	// How long the whole request may spend hunting for a credential that will
	// serve it. Four minutes was set when a refusal was rare and worth waiting
	// out. During an upstream capacity storm it is the wrong number by a lot:
	// a doomed request holds the client for four minutes and then fails anyway,
	// while a client that gets its answer in two minutes can simply ask
	// again. Production during a 90%-refusal storm: 68.8% of the requests that
	// succeeded at all succeeded on the FIRST credential, and the ones that
	// needed five or more took two to three minutes to get there — long enough
	// that the user had given up either way.
	const failoverDeadline = 120 * time.Second

	// The deadline above gates only the START of an attempt, and that turned
	// out to be half a bound. Production, on the version that shipped it:
	// twelve attempts all began inside the 120s window, and then the twelfth
	// ran 464 seconds on its own — first content-bearing frame at 347s, no
	// output tokens, 584s total. The client had a silent socket for the whole
	// of it, which is what users report as "fifteen minutes, no response".
	//
	// So bound the request itself, but only while it has produced NOTHING. A
	// stream that is actually delivering tokens may run as long as it likes —
	// that is a real answer arriving slowly, and cutting it would be the
	// truncation bug all over again. `committed` is set by the relay's lazy
	// commit, i.e. the moment the first byte reaches the client.
	//
	// Scoped to Codex, and not to compaction. The Anthropic relay does not mark
	// commitment, so arming this there would cut a Claude turn that is merely
	// thinking; /v1/responses/compact answers with one JSON object after real
	// work and has no first byte to wait for.
	if auth.NormalizeProvider(provider) == auth.ProviderOpenAI && path != "/v1/responses/compact" {
		var committed atomic.Bool
		c.Set(committedFlagKey, &committed)
		reqCtx, cancelReq := context.WithCancel(c.Request.Context())
		defer cancelReq()
		c.Request = c.Request.WithContext(reqCtx)
		go func() {
			t := time.NewTimer(failoverDeadline)
			defer t.Stop()
			select {
			case <-reqCtx.Done():
			case <-t.C:
				if !committed.Load() {
					log.Warnf("proxy: %s produced no bytes in %s — ending the request so the caller can retry (model=%s)",
						path, failoverDeadline, model)
					cancelReq()
				}
			}
		}()
	}

	tried := make(map[string]bool)
	attempts := 0
	var lastDeferred *deferredResponse
	// Seeded by the Codex ingress guard for a model only an API-key relay
	// serves, so the loop never offers it to a subscription credential.
	apiKeyOnly := c.GetBool(codexAPIKeyOnlyModelKey)
	preparationFallbackPending := false
	// Credentials already given a second chance after a stale pooled socket.
	// One per credential, so a pool that keeps handing out dead sockets costs a
	// bounded number of extra rounds rather than spinning here.
	staleRetried := make(map[string]bool)
	// Same-credential retries already spent on a capacity refusal, per
	// credential. Bounded so a credential that is genuinely out of room still
	// yields to the rest of the pool.
	shedRetried := make(map[string]int)

	// Convert withheld service errors to the gateway's stable public taxonomy.
	// Raw bodies remain in operator logs because they may identify a vendor,
	// relay, account, or credential.
	surfaceDeferred := func(d *deferredResponse) {
		copySafeRetryHeaders(c, d.header)
		writeAPIError(c, provider, publicUpstreamError(d.status, d.body))
		s.emitLog(requestlog.Record{
			Client: clientName, ClientToken: maskClientToken(clientToken), Provider: provider,
			AuthID: d.authID, AuthLabel: d.authLabel, AuthKind: d.authKind,
			Model: model, Status: d.status, Attempts: attempts,
			Stream: stream, Path: path, DurationMs: time.Since(start).Milliseconds(),
			Error:       fmt.Sprintf("upstream %d (all credentials exhausted)", d.status),
			ClaudeAudit: d.claudeAudit,
		})
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 && time.Since(start) > failoverDeadline {
			log.Warnf("proxy: giving up after %s across %d credentials — past the failover deadline (model=%s)",
				time.Since(start).Round(time.Second), attempts, model)
			break
		}
		excludeIDs := make([]string, 0, len(tried))
		for id := range tried {
			excludeIDs = append(excludeIDs, id)
		}
		a := s.pool.AcquireWithOptions(c.Request.Context(), provider, clientToken, clientGroup, model, slotID, auth.AcquireOptions{
			AllowAPIKeyFallback: true,
			APIKeyOnly:          apiKeyOnly,
			ExcludeIDs:          excludeIDs,
		})
		if a == nil {
			// No healthy/untried credential left. If we withheld an upstream
			// error on the way here, surface that genuine status; otherwise
			// there was nothing in the pool to serve the request at all.
			if lastDeferred != nil {
				surfaceDeferred(lastDeferred)
				return
			}
			msg := "no upstream credentials available"
			if preparationFallbackPending {
				msg = "claude request preparation failed and no API-key fallback was available"
			} else if len(tried) > 0 {
				msg = "all upstream credentials exhausted"
			}
			writeAPIError(c, provider, APIError{Status: http.StatusServiceUnavailable, Code: "service_temporarily_unavailable", Message: "The requested model is temporarily unavailable. Please try again shortly."})
			s.emitLog(requestlog.Record{
				Client: clientName, ClientToken: maskClientToken(clientToken), Provider: provider, Model: model,
				Stream: stream, Path: path, Status: 503, Attempts: attempts,
				DurationMs: time.Since(start).Milliseconds(), Error: msg,
			})
			return
		}
		tried[a.ID] = true
		attempts++
		var preflightPrepared mimicry.BodyTransformResult

		// OAuth preparation is entirely local and deterministic. Validate it
		// before any sidecar or business request reaches Anthropic. A failure is
		// about request/identity binding rather than credential health, so switch
		// directly to an API key with the untouched original body.
		if auth.NormalizeProvider(a.Provider) == auth.ProviderAnthropic && a.Kind == auth.KindOAuth && path == "/v1/messages" {
			requestClass := mimicry.ClassifyClaudeCodeRequest(body)
			var prepErr error
			preflightPrepared, prepErr = prepareClaudeOAuthBody(body, model, a, mimicry.SimIdentity{
				AccountKey: a.AccountKey(), AccountUUID: a.AccountUUIDValue(), ClientToken: clientToken,
			})
			if prepErr == nil {
				// Validate the credential binding and complete header policy too. This
				// catches missing genuine betas, empty credentials, and any future
				// prepared-header invariant before sidecars or business traffic begin.
				preflightReq := &http.Request{Header: c.Request.Header.Clone()}
				stripIngressHeaders(preflightReq.Header)
				baseURL := s.cfg.AnthropicBaseURL
				if override := strings.TrimRight(a.Snapshot().BaseURL, "/"); override != "" {
					baseURL = override
				}
				prepErr = applyAnthropicPreparedHeaders(preflightReq, a, stream,
					strings.HasPrefix(strings.ToLower(baseURL), "https://api.anthropic.com"), preflightPrepared)
			}
			if prepErr != nil {
				reason := claudePreparationFailureReason(prepErr)
				audit := claudePreparationFailureAudit(requestClass, a.AccountKey(), reason, body, c.Request.Header, a.ProxyURL)
				log.Errorf("proxy: Claude %s preparation failed before upstream (account_hash=%s model=%s reason=%s fallback=apikey)",
					requestClass.String(), audit.AccountHash, model, reason)
				s.emitLog(requestlog.Record{
					Client: clientName, ClientToken: maskClientToken(clientToken), Provider: provider,
					AuthID: a.ID, AuthLabel: a.Label, AuthKind: "oauth", Model: model,
					Stream: stream, Path: path, Attempts: attempts, DurationMs: time.Since(start).Milliseconds(),
					Error: "claude request preparation failed; fallback=apikey", AttemptOnly: true, ClaudeAudit: audit,
				})
				apiKeyOnly = true
				preparationFallbackPending = true
				lastDeferred = nil
				continue
			}
		}
		if preparationFallbackPending && a.Kind == auth.KindAPIKey {
			log.Warnf("proxy: Claude preparation fallback selected API-key credential %s", a.ID)
			c.Set(claudePreparationAPIKeyFallbackKey, true)
			preparationFallbackPending = false
		}

		var retry, done bool
		var deferred *deferredResponse
		switch auth.NormalizeProvider(a.Provider) {
		case auth.ProviderOpenAI:
			retry, done = s.doForwardCodex(c, a, path, body, stream, model, clientToken, clientName, slotID, start, attempts)
			// The turn died on a WebSocket the backend had closed while it sat
			// in the pool. That is a fact about one socket, not about the
			// account: excluding the credential here would spend a failover
			// round and move the conversation to an account whose prompt cache
			// is cold, both for nothing. Put it back in the candidate set once.
			if c.GetBool(codexStaleSocketRetryKey) {
				c.Set(codexStaleSocketRetryKey, false)
				if retry && !staleRetried[a.ID] {
					staleRetried[a.ID] = true
					delete(tried, a.ID)
				}
			}
			// Upstream refused the turn for capacity. Ask the same credential
			// again rather than rotating: it is the only one holding this
			// conversation's prompt cache, so its retry is the cheap one to
			// schedule, and rotating would also hand the slot's sticky binding
			// to a cache-cold account for every turn that follows.
			if c.GetBool(codexCapacityShedKey) {
				c.Set(codexCapacityShedKey, false)
				if retry && shedRetried[a.ID] < codexSameCredShedRetries {
					shedRetried[a.ID]++
					delete(tried, a.ID)
					if !sleepCtx(c.Request.Context(), codexShedRetryBackoff()) {
						return
					}
				}
			}
		default:
			// attempt > 0 ⇒ this is a transparent retry; doForward skips the
			// blocking bootstrap-wait so the credential switch stays fast.
			retry, done, deferred = s.doForwardPrepared(c, a, path, body, stream, model, clientToken, slotID, clientName, start, attempts, attempt > 0, preflightPrepared)
		}
		if done {
			s.pool.Release(provider, clientToken, slotID)
			return
		}
		if !retry {
			s.pool.Release(provider, clientToken, slotID)
			return
		}
		// Credential-level error withheld from the client — remember the most
		// recent one so it can be surfaced if every credential is exhausted,
		// then loop on to the next credential.
		if deferred != nil {
			lastDeferred = deferred
		}
		log.Warnf("proxy: retrying with a different credential (last auth=%s)", a.ID)
	}
	// Backstop reached (maxAttempts) — surface the last withheld error if any.
	if lastDeferred != nil {
		surfaceDeferred(lastDeferred)
		return
	}
	writeAPIError(c, provider, APIError{Status: http.StatusServiceUnavailable, Code: "service_temporarily_unavailable", Message: "The requested model is temporarily unavailable after several attempts. Please try again shortly."})
	s.emitLog(requestlog.Record{
		Client: clientName, ClientToken: maskClientToken(clientToken), Provider: provider, Model: model,
		Stream: stream, Path: path, Status: 503, Attempts: attempts,
		DurationMs: time.Since(start).Milliseconds(), Error: "upstream retries exhausted",
	})
}

func (s *Server) emitLog(r requestlog.Record) {
	if s.reqLog == nil {
		return
	}
	s.reqLog.Log(r)
}

// clientSlotID derives a per-window slot identifier from the incoming request.
// Claude Code sends a stable per-window X-Claude-Code-Session-Id header (also
// mirrored in metadata.user_id.session_id); the Codex CLI sends a session_id
// header. Treating each distinct value as its own pool slot lets one user's
// multiple CLI windows occupy independent slots and be load-balanced across
// different credentials. Returns "" when the client supplies neither (raw API
// callers) — the pool then keeps one slot per client token.
func clientSlotID(c *gin.Context) string {
	// A trusted relay speaks for the caller behind it. Its declaration wins over
	// the session headers on the wire, which describe the relay's own hop rather
	// than the user — without this every user behind one relay token shares a
	// slot and therefore a single upstream credential.
	if id, ok := relayIdentity(c); ok {
		return id.SlotID()
	}
	if v := strings.TrimSpace(c.GetHeader("X-Claude-Code-Session-Id")); v != "" {
		return v
	}
	// "session-id", with a hyphen, is what a real Codex client sends — see
	// mimicry.CodexSessionIDHeader and crack/codexapp0.147.0/SPEC.md §2.1.
	// It must be checked BEFORE the underscore spelling below, which no genuine
	// client emits: Go canonicalizes "Session_id" to itself (an underscore is
	// not a header-name separator), so that branch could never match an inbound
	// "session-id" and every Codex session was landing on the empty slot. One
	// slot for all of a token's concurrent sessions means they share an upstream
	// credential AND, since the slot feeds the session anchor, a single
	// prompt_cache_key — upstream then sees one session carrying several
	// unrelated threads at once, which is a shape no real client produces.
	if v := strings.TrimSpace(c.GetHeader("session-id")); v != "" {
		return v
	}
	if v := strings.TrimSpace(c.GetHeader("Session_id")); v != "" {
		return v
	}
	return ""
}

func maskClientToken(t string) string {
	if len(t) <= 10 {
		return "***"
	}
	return t[:6] + "…" + t[len(t)-4:]
}

// flagStripThinking persists the strip-thinking decision on a credential after a
// thinking-signature recovery succeeds, so future requests on it sanitize prior
// thinking signatures proactively (ahead of the forward) instead of failing once
// per request and replaying. Generic across all credentials/providers (any relay
// that rotates backend accounts and rejects echoed signatures gets flagged on its
// first signature recovery). Idempotent + best-effort.
func flagStripThinking(a *auth.Auth) {
	if a.StripThinkingEnabled() {
		return
	}
	if err := a.MarkStripThinking(); err != nil {
		log.Warnf("proxy: %s strip-thinking persist failed: %v", a.ID, err)
		return
	}
	log.Infof("proxy: %s flagged strip-thinking (persisted) — prior thinking signatures will be sanitized proactively on future requests", a.ID)
}

// deferredResponse is an upstream error response withheld from the client so
// the request can be transparently retried on another credential. The forward
// loop keeps the most recent one; if every healthy credential is exhausted it
// replays this verbatim instead of synthesizing a 503, so the client still
// receives the genuine upstream status (e.g. a 429 with its Retry-After) and
// backs off correctly. nil when nothing was withheld.
type deferredResponse struct {
	status      int
	header      http.Header
	body        []byte
	authID      string
	authLabel   string
	authKind    string
	claudeAudit *requestlog.ClaudeAudit
}

const claudePreparationAPIKeyFallbackKey = "claude_preparation_apikey_fallback"

// codexAPIKeyOnlyModelKey marks a request whose model is served by an API-key
// relay but by no subscription tier, so forwardWithFailover skips OAuth
// entirely instead of parking a turn on each credential in turn.
const codexAPIKeyOnlyModelKey = "codex_apikey_only_model" //nolint:gosec // G101: a gin context key, not a credential.

// copySafeRetryHeaders carries the backoff signal from a withheld upstream
// response onto the synthesized error we return instead.
//
// Only Retry-After, and deliberately nothing else: the body here is our own
// APIError JSON, so copying the upstream Content-Type (text/event-stream on a
// streaming call) would misdescribe it.
//
// Going through the scrubber rather than reading the header directly is what
// gains the derivation — when upstream sent no Retry-After, one is computed from
// the unified reset timestamps before those are dropped. X-RateLimit-Reset is no
// longer forwarded: on the relay path it publishes that relay's window, which is
// the same class of disclosure as the Anthropic headers.
func copySafeRetryHeaders(c *gin.Context, h http.Header) {
	scrubbed := h.Clone()
	downstream.ScrubUpstreamHeaders(scrubbed, time.Now())
	if value := scrubbed.Get("Retry-After"); value != "" {
		c.Header("Retry-After", value)
	}
}

// doForward sends the request with one credential. Returns (retry, done, deferred):
//
//	retry=true   → caller should try another credential. When the retry was
//	               prompted by a credential-level upstream error (429 quota /
//	               rate-limit, 401/403, account ban) the response is withheld
//	               from the client and returned in deferred so the loop can
//	               surface it if no healthy credential remains. A nil deferred
//	               on retry=true means a transport error (nothing received).
//	done=true    → response was delivered to the client (status < 400 or a
//	               non-retryable error already written through).
//
// isRetry is true on the 2nd+ attempt of a request; it suppresses the
// blocking bootstrap-wait gate so a transparent credential switch doesn't
// re-stack the ≤5s sidecar wait on every alternate credential.
func (s *Server) doForwardPrepared(c *gin.Context, a *auth.Auth, path string, body []byte, stream bool, model, clientToken, slotID, clientName string, start time.Time, attempts int, isRetry bool, preflightPrepared mimicry.BodyTransformResult) (retry bool, done bool, deferred *deferredResponse) {
	if a.Kind == auth.KindAPIKey {
		// API-key relays keep the established thinking-signature behavior. The
		// genuine preserve/rewrite policy applies only to Anthropic OAuth.
		if path == "/v1/messages" {
			switched := s.switchTracker.Check(clientToken, body, a.ID)
			if switched || a.StripThinkingEnabled() {
				if switched {
					log.Infof("auth switch detected: clientToken=%s now on auth=%s — sanitizing prior thinking signatures",
						maskClientToken(clientToken), a.ID)
				}
				body = thinkingsig.SanitizeForSwitch(body)
			}
		}
		return s.doForwardAnthropicAPIKey(c, a, path, body, stream, model, clientToken, clientName, start, attempts)
	}

	originalBody := body
	accountKey := a.AccountKey()
	var requestPolicy mimicry.RequestPolicy
	preparedPath := false
	if path == "/v1/messages" {
		requestClass := mimicry.ClassifyClaudeCodeRequest(body)
		preparedPath = true
		switchKey := accountKey
		switch requestClass {
		case mimicry.RequestClassGenuine:
			var err error
			requestPolicy, err = mimicry.NewClaudeCodeRequestPolicy(requestClass, mimicry.GenuineRequestRewrite)
			if err != nil {
				log.Warnf("proxy: classify Claude request via %s: %v", a.ID, err)
				writeAPIError(c, auth.NormalizeProvider(a.Provider), APIError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "The request body is not a valid Claude messages request."})
				return false, true, nil
			}
			if strings.TrimSpace(c.GetHeader("Anthropic-Beta")) == "" {
				// Main, 1M, and title requests carry different feature vectors. With
				// no genuine downstream vector there is no safe 2.1.220 default.
				writeAPIError(c, auth.NormalizeProvider(a.Provider), APIError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "A genuine Claude Code request must include Anthropic-Beta."})
				return false, true, nil
			}
		case mimicry.RequestClassGeneric:
			requestPolicy = mimicry.NewGenericClaudeCodeSynthesizePolicy()
		}

		switched := s.switchTracker.Check(clientToken, body, switchKey)
		if switched || a.StripThinkingEnabled() {
			if switched {
				log.Infof("auth switch detected: clientToken=%s now on auth=%s — sanitizing prior thinking signatures",
					maskClientToken(clientToken), a.ID)
			}
			body = thinkingsig.SanitizeForSwitch(body)
		}
	}

	baseURL := s.cfg.AnthropicBaseURL
	// Per-credential base URL override (used for relay/midstream vendors on
	// API-key credentials).
	if ab := strings.TrimRight(a.Snapshot().BaseURL, "/"); ab != "" {
		baseURL = ab
	}
	url := baseURL + path + "?beta=true"
	isAnthropicBase := strings.HasPrefix(strings.ToLower(baseURL), "https://api.anthropic.com")

	upstreamBody := body
	id := mimicry.SimIdentity{
		AccountKey:  accountKey,
		AccountUUID: a.AccountUUIDValue(),
		ClientToken: clientToken,
	}
	var prepared mimicry.BodyTransformResult
	if preparedPath {
		var err error
		if preflightPrepared.IsValid() && bytes.Equal(body, originalBody) {
			prepared = preflightPrepared
		} else {
			prepared, err = prepareClaudePreparedBody(body, model, a, id, requestPolicy)
		}
		if err != nil {
			// Production preflights this path before doForward so it can switch to
			// an API key. Keep the direct-call guard fail-closed as a final backstop.
			log.Errorf("proxy: prepare Claude request via %s: %v", a.ID, err)
			writeAPIError(c, auth.NormalizeProvider(a.Provider), APIError{Status: http.StatusInternalServerError, Code: "request_preparation_failed", Message: "The request could not be prepared. Please try again."})
			return false, true, nil
		}
		upstreamBody = prepared.Body()
	}

	// Sidecar: dispatch the per-session bootstrap+quota_probe the first
	// time we see this (account, clientToken) pair. Real CC fires the
	// 9-step bootstrap (GrowthBook → settings → grove → bootstrap →
	// penguin → quota probe → mcp_servers → mcp_registry → releases)
	// BEFORE its first business /v1/messages — an OAuth bearer whose
	// very first observed traffic is /v1/messages with full system+tools
	// is a single-shot fingerprint of a non-CC client. Notify returns a
	// channel closed when bootstrap reaches the quota_probe step; we
	// gate the first business request on it, capped at bootstrapWaitCap
	// so a stuck sidecar can't hang user traffic.
	bootstrapReady := s.sidecar.Notify(a, clientToken)

	ctx := c.Request.Context()
	// Skip the blocking bootstrap-wait on retries: a transparent credential
	// switch (after a 429/auth error on the previous credential) must not
	// re-stack the ≤5s sidecar wait for every alternate credential. The
	// sidecar bootstrap still fires in the background via Notify above; we
	// just don't gate user traffic on it the second time around.
	if bootstrapReady != nil && !isRetry {
		select {
		case <-bootstrapReady:
		case <-ctx.Done():
			// client cancelled — let downstream layer handle it normally
		case <-time.After(sidecar.BootstrapWaitCap):
			log.Warnf("sidecar: bootstrap-wait timeout for %s — proceeding without preceding bootstrap traffic", a.ID)
		}
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upstreamBody))
	if err != nil {
		writeAPIError(c, auth.NormalizeProvider(a.Provider), APIError{Status: http.StatusInternalServerError, Code: "request_preparation_failed", Message: "The request could not be prepared. Please try again."})
		return false, true, nil
	}

	// Forward selected client headers.
	copyForwardableHeaders(c.Request.Header, upReq.Header)
	stripIngressHeaders(upReq.Header)

	// Genuine and generic OAuth requests consume the same atomic prepared
	// result; generic synthesis pins its own trusted feature/profile vector.
	if preparedPath {
		if err := applyAnthropicPreparedHeaders(upReq, a, stream, isAnthropicBase, prepared); err != nil {
			log.Errorf("proxy: prepare Claude headers via %s: %v", a.ID, err)
			writeAPIError(c, auth.NormalizeProvider(a.Provider), APIError{Status: http.StatusInternalServerError, Code: "request_preparation_failed", Message: "The request could not be prepared. Please try again."})
			return false, true, nil
		}
	} else {
		applyAnthropicHeaders(upReq, a, stream, isAnthropicBase, id, upstreamBody)
	}

	claudeAudit := claudeIdentityAudit(prepared, accountKey, upReq.Header, a.ProxyURL)
	client := auth.ClientFor(a.ProxyURL, s.cfg.UseUTLS)
	resp, err := client.Do(upReq)
	if err != nil {
		// Client went away (ctrl-C, closed connection, etc.) — not a
		// credential fault. Record a non-fatal hint for the admin panel,
		// skip retrying onto other credentials (they would all hit the
		// same dead context and get falsely blamed), and don't bother
		// writing a response body to the vanished client.
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			a.MarkClientCancel(err.Error())
			log.Infof("proxy: client canceled via %s: %v", a.ID, err)
			authKind := "oauth"
			if a.Kind == auth.KindAPIKey {
				authKind = "apikey"
			}
			s.emitLog(requestlog.Record{
				Client:      clientName,
				ClientToken: maskClientToken(clientToken),
				Provider:    auth.NormalizeProvider(a.Provider),
				AuthID:      a.ID,
				AuthLabel:   a.Label,
				AuthKind:    authKind,
				Model:       model,
				Stream:      stream,
				Path:        path,
				Status:      499, // nginx convention: client closed request
				DurationMs:  time.Since(start).Milliseconds(),
				Attempts:    attempts,
				Error:       "client canceled",
				ClaudeAudit: claudeAudit,
			})
			return false, true, nil
		}
		a.MarkFailure(err.Error())
		log.Warnf("proxy: upstream error via %s: %v", a.ID, err)
		s.emitLog(requestlog.Record{
			Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.NormalizeProvider(a.Provider),
			AuthID: a.ID, AuthLabel: a.Label, AuthKind: "oauth", Model: model, Status: http.StatusBadGateway,
			Stream: stream, Path: path, Attempts: attempts, DurationMs: time.Since(start).Milliseconds(),
			Error: "upstream transport error", AttemptOnly: true, ClaudeAudit: claudeAudit,
		})
		return true, false, &deferredResponse{
			status: http.StatusBadGateway, authID: a.ID, authLabel: a.Label, authKind: "oauth", claudeAudit: claudeAudit,
		}
	}

	// Decompress upstream gzip/br before reading anything — we asked for
	// gzip,br to match the real CC fingerprint, but every internal path
	// (usage parsing, SSE streamer, model rewrite, body forwarding) wants
	// plain bytes. The Content-Encoding header is also stripped so the
	// client receives identity even though upstream sent compressed.
	ccstream.Decompress(resp)

	// Upstream error — log, do lightweight credential bookkeeping, and
	// faithfully forward the original response to the client as-is.
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		// Reactive signature-error recovery. A 400 "Invalid signature
		// in thinking block" means the past assistant turns echoed in
		// messages[] carry signatures bound to a different account
		// than this credential. Causes include: switch detector miss
		// (first-touch on a continuing conversation, server restart,
		// 2h GC eviction), or signatures generated outside this proxy.
		// One stateless rescue: drop the signed thinking blocks from
		// the request and replay on the same credential. If it still
		// fails, fall through to normal error handling.
		recovered := false
		if path == "/v1/messages" && thinkingsig.IsSignatureError(errBody) {
			sanitized := thinkingsig.SanitizeForSwitch(body)
			if !bytes.Equal(sanitized, body) {
				retryUpstream := sanitized
				var retryPrepared mimicry.BodyTransformResult
				if preparedPath {
					var prepErr error
					retryPrepared, prepErr = prepareClaudePreparedBody(sanitized, model, a, id, requestPolicy)
					if prepErr != nil {
						log.Warnf("proxy: %s signature retry preparation failed: %v", a.ID, prepErr)
						goto signatureRecoveryDone
					}
					retryUpstream = retryPrepared.Body()
				}
				log.Warnf("proxy: %s returned 400 signature-in-thinking — sanitizing and retrying once on same credential", a.ID)
				if retryReq, rerr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(retryUpstream)); rerr == nil {
					copyForwardableHeaders(c.Request.Header, retryReq.Header)
					stripIngressHeaders(retryReq.Header)
					if preparedPath {
						if headerErr := applyAnthropicPreparedHeaders(retryReq, a, stream, isAnthropicBase, retryPrepared); headerErr != nil {
							log.Warnf("proxy: %s signature retry header preparation failed: %v", a.ID, headerErr)
							goto signatureRecoveryDone
						}
					} else {
						applyAnthropicHeaders(retryReq, a, stream, isAnthropicBase, id, retryUpstream)
					}
					if retryResp, rderr := client.Do(retryReq); rderr == nil {
						ccstream.Decompress(retryResp)
						if retryResp.StatusCode < 400 {
							log.Infof("proxy: %s signature retry succeeded", a.ID)
							resp = retryResp
							claudeAudit = claudeIdentityAudit(retryPrepared, accountKey, retryReq.Header, a.ProxyURL)
							recovered = true
							flagStripThinking(a)
						} else {
							_ = retryResp.Body.Close()
							log.Warnf("proxy: %s signature retry still %d — surfacing original error", a.ID, retryResp.StatusCode)
						}
					} else {
						log.Warnf("proxy: %s signature retry transport error: %v", a.ID, rderr)
					}
				}
			}
		}
	signatureRecoveryDone:
		if recovered {
			goto recoveredFromSignature
		}

		// Account-ban detection: Anthropic returns "organization has been
		// disabled" / "account has been disabled" on terminal bans, usually
		// with 401/403 but occasionally 400. These should hard-disable the
		// credential (manual clear required), not just cooldown.
		banned := isAccountBanBody(errBody)

		// Credential bookkeeping: mark the auth so the pool can make
		// smarter scheduling decisions, but never hide the error from
		// the client. Generic 4xx (400/404/413/422/...) are client-request
		// faults — credential is fine, so no MarkFailure.
		// retryable marks credential-level failures (this credential is bad
		// right now, but another might serve the request): 429 quota/rate-
		// limit, 401/403 auth rejection, account ban. For these we withhold
		// the response and let forward() transparently switch credentials so
		// the user never sees the error while the pool still has a slot.
		// Request-level faults (generic 4xx, "Extra usage is required") and
		// upstream-wide weather (5xx/529) stay non-retryable — every
		// credential would return the same thing, so forward it through.
		retryable := false
		switch {
		case banned:
			a.MarkHardFailure(fmt.Sprintf("account banned (upstream %d)", resp.StatusCode))
			log.Warnf("auth: %s hard-disabled — account ban detected (status %d)", a.ID, resp.StatusCode)
			retryable = true
		case resp.StatusCode == 429 && isLongContextRejection(errBody):
			// Request-level rejection (long context), not a credential
			// problem — no cooldown, no retry (every credential rejects it).
		case resp.StatusCode == 429:
			retryable = true
			// Four flavors of 429 from Anthropic, treated differently. Check
			// in this order — earlier checks are more specific signals:
			//
			//  1. Authoritative ratelimit headers — `anthropic-ratelimit-
			//     unified-status` (or `unified-5h-status` / `unified-7d-status`)
			//     == "rejected" together with `anthropic-ratelimit-unified-reset`
			//     (or per-bucket reset). This is the single most reliable quota
			//     signal Anthropic ships, present on every modern API call,
			//     regardless of body wording. Cool down until the stamped reset
			//     time so IsHealthy stays false until the credential genuinely
			//     recovers.
			//  2. Subscription usage limit ("Claude AI usage limit
			//     reached|<unix-ts>") — older / human-readable variant of (1).
			//     Honour the body timestamp.
			//  3. Stealth ban (no Retry-After, no anthropic-ratelimit-*
			//     headers, body is the generic rate_limit_error blurb):
			//     Anthropic occasionally serves bans this way. Hard-
			//     fail immediately so the credential stops cycling
			//     back into rotation every 30 seconds.
			//  4. Ordinary RPM/TPM rate limit: short cooldown +
			//     MarkRateLimited counter (15-strike escalation still
			//     applies as a backstop).
			//
			// Only (1) and (2) advance MarkUsageLimitReached (which deliberately
			// does NOT touch the consecutive-429 counter — those are real quota
			// signals, not stealth-ban candidates).
			if resetAt, scope, banned, ok := parseUnifiedRatelimitRejected(resp.Header, model); ok && !banned {
				if scope != "" {
					// Model-scoped rejection (fable's weekly allotment): cool down
					// only this model family so the credential keeps serving every
					// other model instead of being flagged account-wide.
					a.MarkModelRateLimited(scope, resetAt)
					log.Warnf("auth: %s model-scoped limit (%s) — cooldown until %s", a.ID, scope, resetAt.Format(time.RFC3339))
				} else {
					a.MarkUsageLimitReached(resetAt)
					log.Warnf("auth: %s usage limit (unified-ratelimit rejected) — cooldown until %s", a.ID, resetAt.Format(time.RFC3339))
				}
			} else if resetAt, ok := parseClaudeUsageLimitBody(errBody); ok {
				a.MarkUsageLimitReached(resetAt)
				log.Warnf("auth: %s subscription usage limit — cooldown until %s", a.ID, resetAt.Format(time.RFC3339))
			} else {
				// "No reset signal" 429s — either unified-ratelimit
				// rejected with every reset stamp past/missing, or no
				// ratelimit headers at all. We don't know if the account
				// is banned or just genuinely rate-limited with a buggy
				// upstream payload, so defer the hard-fail decision to
				// the 15-strike accumulator inside MarkRateLimited
				// (rateLimit429HardFailureThreshold). One bad reply
				// shouldn't be enough to take a credential offline.
				resetAt := parseRetryAfter(resp.Header)
				s.pool.ReportUpstreamError(a, resp.StatusCode, resetAt)
			}
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			retryable = true
			// A 401 with body {"type":"authentication_error", ...} /
			// "Invalid authentication credentials" is Anthropic rejecting this
			// credential — but a SINGLE one is almost never a dead account. A
			// proactive EnsureFresh mints a new access token and Anthropic
			// invalidates the old one server-side the instant refresh
			// completes; any request that captured the old bearer and is still
			// on the wire during that ~1-2s rotation window comes back 401. On
			// a busy account (many client tokens, always some request in
			// flight) every refresh orphans one or a few requests this way —
			// all transient, all followed by successes on the fresh token.
			// Immediately hard-disabling on the first such 401 (as we used to)
			// took healthy paying subscriptions offline until a manual
			// re-login. Instead cooldown-and-retry per strike and let the
			// dedicated Consecutive401s accumulator promote to a sticky
			// hard-failure only after a sustained run with no intervening
			// success (auth401HardFailureThreshold). A genuinely revoked
			// refresh token is still caught earlier and authoritatively by the
			// refresh path's invalid_grant hard-failure.
			if resp.StatusCode == 401 && isDefinitiveAuthRejection(errBody) {
				n := a.MarkAuthRejection(fmt.Sprintf("upstream 401 authentication rejected: %s", truncate(errBody, 200)))
				if a.IsHardFailed() {
					log.Warnf("auth: %s hard-disabled — %d consecutive 401s with no success (presumed revoked)", a.ID, n)
				} else {
					// Transient (token-rotation race). Short cooldown so it steps
					// out of rotation briefly, then self-recovers on the fresh token.
					s.pool.ReportUpstreamError(a, resp.StatusCode, time.Time{})
					log.Warnf("auth: %s transient 401 (rotation race, strike %d) — cooldown + retry", a.ID, n)
				}
			} else {
				resetAt := parseRetryAfter(resp.Header)
				s.pool.ReportUpstreamError(a, resp.StatusCode, resetAt)
			}
		case resp.StatusCode == 529, resp.StatusCode >= 500:
			a.MarkFailure(fmt.Sprintf("upstream %d", resp.StatusCode))
		}
		if claudeAudit != nil {
			claudeAudit.CredentialHardFailed = a.IsHardFailed()
		}

		authKind := "oauth"
		if a.Kind == auth.KindAPIKey {
			authKind = "apikey"
		}
		// Break sticky session so the retry (and any future request from this
		// client) can be assigned to a different, hopefully healthy credential.
		s.pool.Unstick(auth.NormalizeProvider(a.Provider), clientToken, slotID)

		if retryable {
			// Withhold the response and signal the caller to switch credentials.
			// The attempt-only row is excluded from normal dashboard aggregates,
			// but preserves per-account 401/403/429 and hard-failure evidence for
			// the seven-day experiment.
			log.Warnf("proxy: %s returned %d — retrying on another credential. body=%s", a.ID, resp.StatusCode, truncate(errBody, 500))
			s.emitLog(requestlog.Record{
				Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.NormalizeProvider(a.Provider),
				AuthID: a.ID, AuthLabel: a.Label, AuthKind: authKind, Model: model, Status: resp.StatusCode,
				Stream: stream, Path: path, Attempts: attempts, DurationMs: time.Since(start).Milliseconds(),
				Error:       fmt.Sprintf("upstream %d (credential attempt withheld)", resp.StatusCode),
				AttemptOnly: true, ClaudeAudit: claudeAudit,
			})
			return true, false, &deferredResponse{
				status: resp.StatusCode, header: resp.Header.Clone(), body: errBody,
				authID: a.ID, authLabel: a.Label, authKind: authKind, claudeAudit: claudeAudit,
			}
		}

		log.Warnf("proxy: %s returned %d — forwarding to client. body=%s", a.ID, resp.StatusCode, truncate(errBody, 500))
		s.emitLog(requestlog.Record{
			Client:      clientName,
			ClientToken: maskClientToken(clientToken),
			AuthID:      a.ID,
			AuthLabel:   a.Label,
			AuthKind:    authKind,
			Model:       model,
			Status:      resp.StatusCode,
			DurationMs:  time.Since(start).Milliseconds(),
			Stream:      stream,
			Path:        path,
			Attempts:    attempts,
			Error:       fmt.Sprintf("upstream %d", resp.StatusCode),
			ClaudeAudit: claudeAudit,
		})

		copySafeRetryHeaders(c, resp.Header)
		writeAPIError(c, auth.NormalizeProvider(a.Provider), publicUpstreamError(resp.StatusCode, errBody))
		return false, true, nil
	}

recoveredFromSignature:
	// Success or non-retryable error — stream response body to client.
	authKind := "oauth"
	if a.Kind == auth.KindAPIKey {
		authKind = "apikey"
	}

	// counts.Requests is left at ZERO here and set only where usage is actually
	// observed (usageJSON.toCounts / mergeSSEUsage). It used to be hard-set to 1
	// on this line, which quietly turned the downstream `counts.Requests > 0`
	// billing gate into a tautology: a 200 that carried no usage at all still
	// passed it, priced out to $0, and was served for free. A 2026-08 audit of
	// the production request log found 3,808 such rows — 3,805 of them belonging
	// to paying accounts, concentrated in the most expensive models (opus-4-8
	// ×2013, fable-5 ×1046, opus-4-7 ×335, opus-5 ×75).
	//
	// The auth-side ledger still counts every served request; see the `ledger`
	// copy below, which restores Requests=1 for that purpose only.
	var counts usage.Counts
	var sub advisor.SubUsage

	// When this credential rewrote the request's model name (relay vendors
	// with vendor-prefixed names), rewrite it back in the response so the
	// client keeps seeing the model it asked for. Claude Code uses the
	// model field on message_start to correlate conversation turns; a
	// vendor-prefixed name breaks multi-turn continuation.
	var rewriteClientModel string
	terminalError := ""
	if upstreamModel, ok := a.ResolveUpstreamModel(model); ok && upstreamModel != model && upstreamModel != "" {
		rewriteClientModel = model
	}

	if stream && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Headers commit lazily on the first byte, so a stream that breaks
		// before any output reaches the client can transparently fail over to
		// another credential (the common "RST right after 200" case).
		res := streamSSE(c, resp, &counts, &sub, rewriteClientModel, func() { writeSSEResponseHeaders(c, resp) })
		if !res.sawTerminal && !res.wroteAny {
			_ = resp.Body.Close()
			if isClientDisconnect(c.Request.Context(), res.err) {
				a.MarkClientCancel(errString(res.err))
				s.emitLog(requestlog.Record{
					Client: clientName, ClientToken: maskClientToken(clientToken),
					Provider: auth.NormalizeProvider(a.Provider), AuthID: a.ID, AuthLabel: a.Label,
					AuthKind: authKind, Model: model, Status: 499, Stream: stream, Path: path, Attempts: attempts,
					DurationMs: time.Since(start).Milliseconds(), Error: "client canceled before first event", ClaudeAudit: claudeAudit,
				})
				return false, true, nil
			}
			log.Warnf("proxy: stream broke before any output via %s (attempt %d, %s): %v — retrying on another credential",
				a.ID, attempts, time.Since(start).Round(time.Millisecond), res.err)
			a.MarkFailure("stream broke before first event: " + errString(res.err))
			s.emitLog(requestlog.Record{
				Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.NormalizeProvider(a.Provider),
				AuthID: a.ID, AuthLabel: a.Label, AuthKind: authKind, Model: model, Status: http.StatusBadGateway,
				Stream: stream, Path: path, Attempts: attempts, DurationMs: time.Since(start).Milliseconds(),
				Error: "stream broke before first event", AttemptOnly: true, ClaudeAudit: claudeAudit,
			})
			return true, false, &deferredResponse{
				status: http.StatusBadGateway, authID: a.ID, authLabel: a.Label, authKind: authKind, claudeAudit: claudeAudit,
			}
		}
		a.MarkSuccess()
		if !res.sawTerminal {
			terminalError = "stream truncated before terminal event"
			log.Warnf("proxy: SSE truncated mid-stream via %s (attempt %d, events=%d, bytes=%d, %s): %v",
				a.ID, attempts, res.events, res.bytes, time.Since(start).Round(time.Millisecond), res.err)
		}
	} else {
		writeResponseHeaders(c, resp)
		a.MarkSuccess()
		respBody, _ := io.ReadAll(resp.Body)
		if rewriteClientModel != "" {
			respBody = rewriteResponseModel(respBody, rewriteClientModel)
		}
		_, _ = c.Writer.Write(respBody)
		counts.Add(extractUsageFromJSON(respBody, &sub))
	}
	_ = resp.Body.Close()

	// Auth-side ledger: every request this credential served counts toward its
	// load, whether or not the upstream reported usage. Requests is restored on
	// a copy so the billing gate below keeps reading the honest signal.
	ledger := counts
	ledger.Requests = 1
	s.usage.Record(a.ID, a.Label, ledger)

	// A 200 that reported no usage at all is an upstream accounting failure, not
	// a free lunch. It cannot be billed (there is no observed quantity to price)
	// but it must be visible: log it with the stable marker so the admin panel
	// and any audit can find these instead of them looking like ordinary $0 rows.
	if resp.StatusCode < 400 && usage.MissingUsage(counts) {
		log.Warnf("proxy: %s returned %d without usage accounting (model=%s stream=%v) — billing $0; "+
			"upstream cannot account for what it served", a.ID, resp.StatusCode, model, stream)
	}

	// Compute the wallet-side cost: hand the official upstream price to the
	// SaaS adapter, which applies the per-(group × provider) multiplier and
	// returns the dollar amount actually deducted. The same value gets
	// written into the request log so the log row matches the wallet ledger.
	//
	// CostUSD carries the OFFICIAL upstream price and BilledUSD what the wallet
	// was actually debited — the same split CPA-Claude has always used. CostUSD
	// used to be overwritten with the billed figure here, which left the two
	// forks writing opposite meanings into the same JSONL column and made the
	// request-log UI's "official x multiplier == stored" check unverifiable.
	//
	// Rows predating this change have no billed_usd, so readers must fall back
	// to cost_usd for them; the frontend does exactly that.
	var costUSD, billedUSD float64
	var userID int64
	var multiplier float64
	var billingErr string
	if resp.StatusCode < 400 && counts.Requests > 0 && clientToken != "" {
		// Priced on the model we actually bought upstream (see billingModelFor);
		// every label below stays on the client-facing name.
		costUSD = s.pricing.Cost(auth.NormalizeProvider(a.Provider), billingModelFor(a, model), counts)
		billedUSD = costUSD
		if info, ok := saasInfoFrom(c); ok && s.saas != nil {
			billed, err := s.saas.Charge(chargeCtx(c), info, auth.NormalizeProvider(a.Provider), model, counts, costUSD)
			if err != nil {
				log.Warnf("saas: charge failed for token=%d user=%d: %v", info.TokenID, info.UserID, err)
				// Nobody was debited, so the row must not carry the official
				// price as revenue; the marker is what makes the drop findable.
				billedUSD = 0
				billingErr = billingDropped(err)
			} else {
				billedUSD = billed
				userID = info.UserID
				multiplier = s.saas.MultiplierFor(info, auth.NormalizeProvider(a.Provider))
			}
		}
	}
	// Advisor (server-side opus sub-call) is billed alongside the main
	// request: same auth absorbs the load, same client is charged, but the
	// requestlog gets a separate row per advisor model so by-model views
	// don't conflate sonnet-orchestrator cost with opus-advisor cost.
	advisorCost := s.recordSubUsage(c, a, authKind, clientToken, clientName, model, path, resp.StatusCode, sub)
	if resp.StatusCode < 400 && counts.Requests > 0 && clientToken != "" {
		// Single RecordClient call: weekly cost ledger should reflect the
		// total dollar cost of this /v1/messages call, advisor included.
		// Counts.Requests stays at 1 — advisor is a sub-call, not a request.
		var clientCounts usage.Counts
		clientCounts.Add(counts)
		for _, sc := range sub.Snapshot() {
			clientCounts.Add(sc)
		}
		s.usage.RecordClient(clientToken, clientName, clientCounts, billedUSD+advisorCost)
	}
	if claudeAudit != nil {
		log.Infof(
			"claude-identity-audit: account=%s class=%s mode=%s identity_mapped=%t",
			claudeAudit.AccountHash,
			claudeAudit.RequestClass,
			claudeAudit.IdentityMode,
			claudeAudit.AccountIdentityMapped,
		)
	}
	s.emitLog(requestlog.Record{
		Client:        clientName,
		ClientToken:   maskClientToken(clientToken),
		Provider:      auth.NormalizeProvider(a.Provider),
		AuthID:        a.ID,
		AuthLabel:     a.Label,
		AuthKind:      authKind,
		Model:         model,
		Input:         counts.InputTokens,
		Output:        counts.OutputTokens,
		CacheRead:     counts.CacheReadTokens,
		CacheCreate:   counts.CacheCreateTokens,
		CacheCreate1h: counts.CacheCreate1hTokens,
		CostUSD:       costUSD,
		BilledUSD:     billedUSD,
		Status:        resp.StatusCode,
		DurationMs:    time.Since(start).Milliseconds(),
		Stream:        stream,
		Path:          path,
		Attempts:      attempts,
		Error:         joinLogError(terminalError, billingErr),
		UserID:        userID,
		Multiplier:    multiplier,
		ClaudeAudit:   claudeAudit,
	})
	return false, true, nil
}

// billingModelFor returns the model name a request should be PRICED on, which
// is not always the name the client asked for.
//
// On an Anthropic OAuth credential, cc-core's DefaultClaudeOAuthModelMap folds
// retired generations onto the current model (claude-opus-4-7 → claude-opus-5,
// claude-sonnet-4-6 → claude-sonnet-5). What we actually buy from Anthropic is
// the resolved model, so that is what we cost. The client keeps seeing the name
// it asked for everywhere else — the response model is rewritten back
// (rewriteClientModel), the request log records the client-facing name, and the
// wallet/workspace ledger records it in ChargeMeta — so this changes the
// amount, never the label. For Opus the two are identical anyway (same price
// card); for Sonnet the resolved name is cheaper until the sonnet-5
// introductory rate lapses on 2026-08-31, and the customer gets that difference.
//
// API-key credentials are deliberately excluded. Their model_map is a relay
// vendor's naming convention, not a model substitution: it rewrites to names
// like "[0.1]a/claude-sonnet-4-6" that match no price card, so pricing them on
// the upstream name would silently drop every such request onto the provider
// default.
func billingModelFor(a *auth.Auth, clientModel string) string {
	if a == nil || a.Kind != auth.KindOAuth {
		return clientModel
	}
	if upstream, ok := a.ResolveUpstreamModel(clientModel); ok && upstream != "" {
		return upstream
	}
	return clientModel
}

// doForwardAnthropicAPIKey is the API-key passthrough for Anthropic-shaped
// upstreams (api.anthropic.com or third-party relays). Unlike the OAuth path,
// we inject no Claude Code mimicry headers and do not use uTLS: the request
// is forwarded essentially verbatim. The only request-side change allowed is
// the per-credential model rewrite (and the matching response-side rewrite)
// so model_map'd relay vendors keep working.
//
// Failure handling is driven by classifyUpstreamStatus, which separates
// faults the client caused from faults the upstream or the credential caused
// (see upstream_health.go):
//
//	faultNone       → MarkSuccess, response forwarded
//	faultCredential → MarkHardFailure, withheld + retried on another credential
//	faultUpstream   → MarkFailure,     withheld + retried on another credential
//	faultClient     → no health change, forwarded verbatim, never retried
//
// A transport error never reaches a status code and is classified by hand as
// the upstream-side class: the relay was unreachable, which says nothing about
// this request and is exactly what a differently-hosted relay survives.
//
// Only the upstream-side classes touch health, so one client sending
// malformed requests can no longer degrade a channel that is serving
// everyone else correctly.
//
// Both upstream-side classes feed cc-core's API-key circuit breaker: enough
// consecutive faults pause the channel for a self-expiring, exponentially
// growing interval, so traffic rotates onto another key instead of re-paying
// a doomed round-trip per request, and the channel probes itself back into
// rotation with no operator involvement. A definitive credential rejection
// pauses on the first strike. Neither ever *retires* the channel — only the
// explicit Disabled flag takes an API key offline for good.
//
// A <400 response is additionally checked against the Messages API wire
// format before it is committed or billed (validateAnthropicResponse) — a
// relay answering 200 with an HTML block page is a faultUpstream, not a
// zero-token success.
//
// The (retry, done, deferred) contract matches doForward: a retryable fault
// is withheld and returned in deferred so forwardWithFailover can switch
// credentials transparently, replaying the withheld response only if every
// credential is exhausted. There is no bootstrap-wait gate on this path, so
// it takes no isRetry flag.
func (s *Server) doForwardAnthropicAPIKey(c *gin.Context, a *auth.Auth, path string, body []byte, stream bool, model, clientToken, clientName string, start time.Time, attempts int) (retry bool, done bool, deferred *deferredResponse) {
	baseURL := s.cfg.AnthropicBaseURL
	if ab := strings.TrimRight(a.Snapshot().BaseURL, "/"); ab != "" {
		baseURL = ab
	}
	upURL := baseURL + path

	upstreamBody := body
	rewriteClientModel := ""
	if upstreamModel, ok := a.ResolveUpstreamModel(model); ok && upstreamModel != model && upstreamModel != "" {
		if rewritten, err := rewriteModelField(body, upstreamModel); err == nil {
			upstreamBody = rewritten
			rewriteClientModel = model
		} else {
			log.Warnf("proxy(apikey): model rewrite (%s -> %s) failed via %s: %v", model, upstreamModel, a.ID, err)
		}
	}

	ctx := c.Request.Context()
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(upstreamBody))
	if err != nil {
		writeAPIError(c, auth.NormalizeProvider(a.Provider), APIError{Status: http.StatusInternalServerError, Code: "request_preparation_failed", Message: "The request could not be prepared. Please try again."})
		return false, true, nil
	}
	copyForwardableHeaders(c.Request.Header, upReq.Header)
	stripIngressHeaders(upReq.Header)
	token, _ := a.Credentials()
	upReq.Header.Del("Authorization")
	upReq.Header.Set("x-api-key", token)
	if c.GetBool(claudePreparationAPIKeyFallbackKey) {
		stripAnthropicOAuthBeta(upReq.Header)
	}

	client := auth.ClientFor(a.ProxyURL, false)
	resp, err := client.Do(upReq)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			log.Infof("proxy(apikey): client canceled via %s: %v", a.ID, err)
			s.emitLog(requestlog.Record{
				Client: clientName, ClientToken: maskClientToken(clientToken),
				Provider: auth.NormalizeProvider(a.Provider), AuthID: a.ID, AuthLabel: a.Label, AuthKind: "apikey",
				Model: model, Stream: stream, Path: path, Status: 499,
				DurationMs: time.Since(start).Milliseconds(), Attempts: attempts, Error: "client canceled",
			})
			return false, true, nil
		}
		// A transport failure is the same class of event as a 5xx — the relay
		// is unreachable, not wrong about this request — and is exactly what a
		// different credential is most likely to survive, because relays sit
		// behind different hosts and proxies. So withhold it and roll back to
		// the failover loop instead of handing the client a bare 502. The
		// Claude OAuth path and both Codex paths already did this; this was the
		// last forward path that gave up on the first connection error.
		log.Warnf("proxy(apikey): upstream transport error via %s: %v — retrying on another credential", a.ID, err)
		a.MarkFailure(fmt.Sprintf("transport: %v", err))
		s.emitLog(requestlog.Record{
			Client: clientName, ClientToken: maskClientToken(clientToken),
			Provider: auth.NormalizeProvider(a.Provider), AuthID: a.ID, AuthLabel: a.Label, AuthKind: "apikey",
			Model: model, Stream: stream, Path: path, Status: http.StatusBadGateway,
			DurationMs: time.Since(start).Milliseconds(), Attempts: attempts,
			Error: "upstream transport error", AttemptOnly: true,
		})
		return true, false, &deferredResponse{
			status: http.StatusBadGateway, authID: a.ID, authLabel: a.Label, authKind: "apikey",
		}
	}

	// Decompress upstream gzip/br before reading. Some relays emit gzipped
	// 4xx error pages even when the request didn't advertise an
	// Accept-Encoding; without this the captured snippet is binary.
	ccstream.Decompress(resp)

	// Reactive thinking-signature recovery for API-key relays. Relays that pool
	// and rotate backend accounts per request reject the echoed `thinking`
	// signatures from prior turns ("Invalid signature in thinking block",
	// returned as 400 or relay-rewrapped 500). Sanitize + replay once on the
	// same key; on success, persist strip-thinking so future requests on this
	// credential sanitize proactively (no failing first attempt). Generic across
	// every API-key provider.
	if resp.StatusCode >= 400 && path == "/v1/messages" {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		recovered := false
		if thinkingsig.IsSignatureError(errBody) {
			sanitized := thinkingsig.SanitizeForSwitch(body)
			if !bytes.Equal(sanitized, body) {
				retryUpstream := sanitized
				if um, ok := a.ResolveUpstreamModel(model); ok && um != model && um != "" {
					if rw, e := rewriteModelField(retryUpstream, um); e == nil {
						retryUpstream = rw
					}
				}
				log.Warnf("proxy(apikey): %s returned %d signature-in-thinking — sanitizing and retrying once on same credential", a.ID, resp.StatusCode)
				if rreq, e := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(retryUpstream)); e == nil {
					copyForwardableHeaders(c.Request.Header, rreq.Header)
					stripIngressHeaders(rreq.Header)
					rreq.Header.Set("x-api-key", token)
					if rresp, de := client.Do(rreq); de == nil {
						ccstream.Decompress(rresp)
						if rresp.StatusCode < 400 {
							log.Infof("proxy(apikey): %s signature retry succeeded", a.ID)
							resp = rresp
							recovered = true
							flagStripThinking(a)
						} else {
							_ = rresp.Body.Close()
							log.Warnf("proxy(apikey): %s signature retry still %d — surfacing original error", a.ID, rresp.StatusCode)
						}
					} else {
						log.Warnf("proxy(apikey): %s signature retry transport error: %v", a.ID, de)
					}
				}
			}
		}
		if !recovered {
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
		}
	}

	// Response-contract check. A <400 status is not on its own evidence that
	// the exchange worked: a dead relay in front of the real API answers 200
	// with an HTML block page, which would otherwise be marked healthy,
	// streamed to the client as an empty response, and billed as zero tokens.
	// Validate the wire format first and demote a violation to faultUpstream
	// so it is withheld and retried like any other upstream failure.
	//
	// Buffer the body so the peek is non-consuming; Close still reaches the
	// original body, so the connection is released exactly as before.
	statusForFault := resp.StatusCode
	var bodyBuf *bufio.Reader
	if resp.StatusCode < 400 {
		bodyBuf = bufio.NewReaderSize(resp.Body, 64*1024)
		resp.Body = struct {
			io.Reader
			io.Closer
		}{bodyBuf, resp.Body}
		if v := validateAnthropicResponse(resp.Header, bodyBuf); v.Detail != "" {
			log.Warnf("proxy(apikey): %s returned %d but the body is not an Anthropic response (%s) — treating as an upstream failure",
				a.ID, resp.StatusCode, truncate([]byte(v.Detail), 300))
			statusForFault = http.StatusBadGateway
		}
	}

	// Credential health bookkeeping + retryability, computed before writing
	// anything so a retryable fault can be withheld and retried on another
	// credential while the pool still has a slot.
	fault := classifyUpstreamStatus(statusForFault)
	switch fault {
	case faultNone:
		a.MarkSuccess()
	case faultCredential:
		// Revoked, forbidden, or out of funds — definitive, so cc-core pauses
		// the channel on this single strike rather than re-presenting a dead
		// key on every subsequent request. Still never sticky for an API key:
		// the pause expires by itself.
		a.MarkHardFailure(fmt.Sprintf("upstream %d", statusForFault))
	case faultUpstream:
		// Throttling, gateway errors, or a contract violation. Not a verdict
		// on the key itself, so it takes several in a row before cc-core
		// pauses the channel — enough to ride out the ordinary weather of a
		// shared relay without pausing anything that still works.
		a.MarkFailure(fmt.Sprintf("upstream %d", statusForFault))
	case faultClient:
		// The caller's own request is at fault (400 malformed, 404 route not
		// implemented by this relay, 413 too large, …). Another credential
		// would return the identical error, so forward it through untouched
		// and leave credential health alone.
	}

	if fault.retryable() {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		log.Warnf("proxy(apikey): %s returned %d — retrying on another credential. body=%s", a.ID, resp.StatusCode, truncate(errBody, 500))
		// statusForFault, not resp.StatusCode: a contract violation arrives as
		// 200 but must be replayed to the client as the 502 it really is if
		// every credential ends up exhausted.
		return true, false, &deferredResponse{
			status: statusForFault,
			header: resp.Header.Clone(),
			body:   errBody,
			authID: a.ID,
		}
	}

	var counts usage.Counts
	var sub advisor.SubUsage
	var errSnippet string
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		errSnippet = truncate(errBody, 500)
		log.Warnf("proxy(apikey): %s returned %d — body=%s", a.ID, resp.StatusCode, errSnippet)
		copySafeRetryHeaders(c, resp.Header)
		writeAPIError(c, auth.NormalizeProvider(a.Provider), publicUpstreamError(resp.StatusCode, errBody))
	} else {
		writeResponseHeaders(c, resp)
		// counts.Requests stays zero until usage is actually observed — see the
		// OAuth path above for why hard-setting it here made the billing gate a
		// tautology. This branch carried the larger half of the 3,808 free rows
		// (3,104 of them, against 704 on OAuth).
		// Dispatch on the client's stream flag + the actual bytes, not the
		// upstream Content-Type alone: relays are known to stream the SSE
		// back as text/plain, which under a header-only check fell through to
		// the whole-body JSON parse and silently lost all usage (billing =
		// $0). Same fix the Codex path already carries. Peek is non-consuming.
		if stream && responseIsSSE(resp.Header, bodyBuf) {
			declareSSE(c)
			// commit=nil: the headers are already committed above, so this
			// relay cannot fail over. It does not need to — an upstream that
			// produces no bytes at all never reaches here, because an empty
			// body fails validateAnthropicResponse above and is withheld and
			// retried as a contract violation. By the time a stream starts,
			// bytes have reached the client and the exchange is committed. The
			// relay still adds keepalive + truncation detection so a broken
			// stream is logged, not silently swallowed.
			res := streamSSE(c, resp, &counts, &sub, rewriteClientModel, nil)
			if !res.sawTerminal && !isClientDisconnect(c.Request.Context(), res.err) {
				log.Warnf("proxy(apikey): SSE truncated mid-stream via %s (events=%d, bytes=%d, %s): %v",
					a.ID, res.events, res.bytes, time.Since(start).Round(time.Millisecond), res.err)
			}
		} else {
			respBody, _ := io.ReadAll(resp.Body)
			if rewriteClientModel != "" {
				respBody = rewriteResponseModel(respBody, rewriteClientModel)
			}
			_, _ = c.Writer.Write(respBody)
			counts.Add(extractUsageFromJSON(respBody, &sub))
		}
	}
	_ = resp.Body.Close()

	// CostUSD = official upstream price, BilledUSD = wallet debit. See the OAuth
	// path above for why the two are now distinct columns.
	var costUSD, billedUSD float64
	var userID int64
	var multiplier float64
	var billingErr string
	if resp.StatusCode < 400 {
		ledger := counts
		ledger.Requests = 1
		s.usage.Record(a.ID, a.Label, ledger)
		if usage.MissingUsage(counts) {
			log.Warnf("proxy(apikey): %s returned %d without usage accounting (model=%s stream=%v) — billing $0",
				a.ID, resp.StatusCode, model, stream)
		}
		if counts.Requests > 0 && clientToken != "" {
			costUSD = s.pricing.Cost(auth.NormalizeProvider(a.Provider), model, counts)
			billedUSD = costUSD
			if info, ok := saasInfoFrom(c); ok && s.saas != nil {
				billed, err := s.saas.Charge(chargeCtx(c), info, auth.NormalizeProvider(a.Provider), model, counts, costUSD)
				if err != nil {
					log.Warnf("saas: charge failed for token=%d user=%d: %v", info.TokenID, info.UserID, err)
					// Nobody was debited, so the row must not carry the
					// official price as revenue; the marker is what makes the
					// drop findable.
					billedUSD = 0
					billingErr = billingDropped(err)
				} else {
					billedUSD = billed
					userID = info.UserID
					multiplier = s.saas.MultiplierFor(info, auth.NormalizeProvider(a.Provider))
				}
			}
		}
		advisorCost := s.recordSubUsage(c, a, "apikey", clientToken, clientName, model, path, resp.StatusCode, sub)
		if counts.Requests > 0 && clientToken != "" {
			var clientCounts usage.Counts
			clientCounts.Add(counts)
			for _, sc := range sub.Snapshot() {
				clientCounts.Add(sc)
			}
			s.usage.RecordClient(clientToken, clientName, clientCounts, billedUSD+advisorCost)
		}
	}
	errField := ""
	if resp.StatusCode >= 400 {
		errField = fmt.Sprintf("upstream %d: %s", resp.StatusCode, truncate([]byte(errSnippet), 200))
	}
	s.emitLog(requestlog.Record{
		Client:        clientName,
		ClientToken:   maskClientToken(clientToken),
		Provider:      auth.NormalizeProvider(a.Provider),
		AuthID:        a.ID,
		AuthLabel:     a.Label,
		AuthKind:      "apikey",
		Model:         model,
		Input:         counts.InputTokens,
		Output:        counts.OutputTokens,
		CacheRead:     counts.CacheReadTokens,
		CacheCreate:   counts.CacheCreateTokens,
		CacheCreate1h: counts.CacheCreate1hTokens,
		CostUSD:       costUSD,
		BilledUSD:     billedUSD,
		Status:        resp.StatusCode,
		DurationMs:    time.Since(start).Milliseconds(),
		Stream:        stream,
		Path:          path,
		Attempts:      attempts,
		UserID:        userID,
		Multiplier:    multiplier,
		Error:         joinLogError(errField, billingErr),
	})
	return false, true, nil
}

func stripAnthropicOAuthBeta(h http.Header) {
	value := h.Get("Anthropic-Beta")
	if value == "" {
		return
	}
	parts := strings.Split(value, ",")
	kept := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(part, "oauth-") {
			continue
		}
		kept = append(kept, part)
	}
	if len(kept) == 0 {
		h.Del("Anthropic-Beta")
		return
	}
	h.Set("Anthropic-Beta", strings.Join(kept, ","))
}

// stripIngressHeaders removes headers that describe the *ingress path* into
// our server before forwarding upstream. Critical when the server sits
// behind Cloudflare Tunnel: cloudflared injects Cdn-Loop: cloudflare plus a
// pile of Cf-* headers, and api.anthropic.com / chatgpt.com are themselves
// behind CF — seeing those headers triggers CF's loop-prevention WAF and
// returns 403 HTML. Prefix match so future CF additions are covered.
func stripIngressHeaders(h http.Header) {
	// Relay identity is for us to act on, not to propagate: forwarding it would
	// hand an upstream vendor the shape of our client base, and would let an
	// untrusted caller's forged header reach a peer that does trust us.
	relay.Strip(h)
	for k := range h {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "cf-") || strings.HasPrefix(lower, "cdn-") ||
			strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "x-real-") {
			h.Del(k)
		}
	}
	for _, k := range []string{"Forwarded", "Via", "Cookie", "Referer", "Origin", "True-Client-Ip"} {
		h.Del(k)
	}
}

func copyForwardableHeaders(src, dst http.Header) {
	for k, vs := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		// Don't forward Host.
		if strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// writeResponseHeaders forwards the upstream response headers the client is
// allowed to see.
//
// The allowlist lives in cc-core/downstream. Copying verbatim used to hand the
// caller our pool's operational state: the twelve anthropic-ratelimit-unified-*
// headers (the serving account's tier, its 5h/7d utilisation, its overage status
// and exact reset timestamps), anthropic-organization-id,
// anthropic-workspace-id, the upstream request-id and cf-ray.
//
// Safe by capture, not by hope: a real third-party gateway returns none of them
// and Claude Code works against it unchanged (cc-core crack/thirdparty/SPEC.md
// §4). Called after the retry loop has read those headers for scheduling.
func writeResponseHeaders(c *gin.Context, resp *http.Response) {
	downstream.CopyResponseHeaders(c.Writer.Header(), resp.Header, time.Now())
	c.Writer.WriteHeader(resp.StatusCode)
}

// writeSSEResponseHeaders commits the headers for a stream we are about to
// relay as Server-Sent Events, declaring the media type ourselves.
//
// The allowlist forwards the upstream's Content-Type when there is one, but a
// relayed SSE must not depend on that. When no Content-Type reaches the client,
// net/http sniffs the first bytes and labels the stream `text/plain`, which
// leaves every downstream consumer guessing from the body. CPA-Claude guesses
// by peeking the first line: a stream that opens with an SSE comment (`: …`,
// which the ChatGPT backend emits while a request is queued) was read as a JSON
// body, found to carry no usage, and failed closed with a 502 — 44 of them on
// 2026-08-11, on requests this proxy had itself billed and logged as clean 200s.
//
// Declaring it is also just correct: SSE is text/event-stream, and no-cache is
// what keeps intermediaries from buffering the stream.
func writeSSEResponseHeaders(c *gin.Context, resp *http.Response) {
	downstream.CopyResponseHeaders(c.Writer.Header(), resp.Header, time.Now())
	declareSSE(c)
	c.Writer.WriteHeader(resp.StatusCode)
}

// declareSSE labels the pending response as Server-Sent Events. Safe to call
// after WriteHeader — gin only records the status there, so the header map
// stays mutable until the first body byte — which is what the API-key path
// needs: it commits headers before it has peeked enough bytes to know whether
// the upstream is streaming.
func declareSSE(c *gin.Context) {
	h := c.Writer.Header()
	if !strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream") {
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
	}
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-cache")
	}
}

// applyAnthropicHeaders is a thin adapter from this fork's *auth.Auth to
// cc-core/mimicry.ApplyClaudeCodeHeaders. The full header policy — pinned
// User-Agent / X-Stainless-* / Anthropic-Beta (OAuth vs API-key list) /
// X-Claude-Code-Session-Id / x-client-request-id / Accept-Encoding — lives in
// cc-core/mimicry so hypitoken and CPA-Claude stay byte-identical against the
// pinned Claude Code version target. Bumping the fingerprint is a cc-core +
// dependency-bump, not a per-fork edit.
func applyAnthropicHeaders(req *http.Request, a *auth.Auth, stream, isAnthropicBase bool, id mimicry.SimIdentity, body []byte) {
	token, kind := a.Credentials()
	mimicry.ApplyClaudeCodeHeaders(req, token, kindToMimicry(kind), stream, isAnthropicBase, id, body)
}

func applyAnthropicPreparedHeaders(req *http.Request, a *auth.Auth, stream, isAnthropicBase bool, prepared mimicry.BodyTransformResult) error {
	token, kind := a.Credentials()
	return mimicry.ApplyClaudeCodePreparedRequest(req, token, a.AccountKey(), kindToMimicry(kind), stream, isAnthropicBase, prepared)
}

func prepareClaudeOAuthBody(body []byte, model string, a *auth.Auth, id mimicry.SimIdentity) (mimicry.BodyTransformResult, error) {
	requestClass := mimicry.ClassifyClaudeCodeRequest(body)
	var policy mimicry.RequestPolicy
	switch requestClass {
	case mimicry.RequestClassGenuine:
		var err error
		policy, err = mimicry.NewClaudeCodeRequestPolicy(requestClass, mimicry.GenuineRequestRewrite)
		if err != nil {
			return mimicry.BodyTransformResult{}, err
		}
	case mimicry.RequestClassGeneric:
		policy = mimicry.NewGenericClaudeCodeSynthesizePolicy()
	default:
		return mimicry.BodyTransformResult{}, fmt.Errorf("unsupported Claude request class %s", requestClass)
	}
	return prepareClaudePreparedBody(body, model, a, id, policy)
}

func prepareClaudePreparedBody(body []byte, model string, a *auth.Auth, id mimicry.SimIdentity, policy mimicry.RequestPolicy) (mimicry.BodyTransformResult, error) {
	working := body
	// Any permitted body edits happen before the atomic transform. The rewrite
	// subsequently validates the prepared body, so these exact-byte edits cannot
	// leave a partially rewritten request behind.
	if upstreamModel, ok := a.ResolveUpstreamModel(model); ok && upstreamModel != model && upstreamModel != "" {
		rewritten, err := mimicry.RewriteModelFieldPreservingBytes(working, upstreamModel)
		if err != nil {
			return mimicry.BodyTransformResult{}, fmt.Errorf("model rewrite (%s -> %s): %w", model, upstreamModel, err)
		}
		working = rewritten
	}
	if normalized, changed := mimicry.NormalizeDateline(working); changed {
		working = normalized
	}
	return mimicry.PrepareClaudeCodeRequest(working, model, id, policy, kindToMimicry(a.Kind))
}

// prepareClaudeRewriteBody is retained for focused genuine-policy tests and
// downstream source compatibility. New request handling uses the class-neutral
// prepareClaudePreparedBody helper above.
func prepareClaudeRewriteBody(body []byte, model string, a *auth.Auth, id mimicry.SimIdentity, policy mimicry.RequestPolicy) (mimicry.BodyTransformResult, error) {
	return prepareClaudePreparedBody(body, model, a, id, policy)
}

func claudePreparationFailureReason(err error) string {
	if err == nil {
		return "unknown"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "account uuid"):
		return "missing_account_uuid"
	case strings.Contains(message, "anthropic-beta"):
		return "missing_anthropic_beta"
	case strings.Contains(message, "messages array"):
		return "invalid_messages"
	case strings.Contains(message, "json object"), strings.Contains(message, "request class"):
		return "invalid_json_body"
	case strings.Contains(message, "metadata"):
		return "metadata_rewrite_failed"
	case strings.Contains(message, "billing"):
		return "billing_rewrite_failed"
	case strings.Contains(message, "model rewrite"):
		return "model_rewrite_failed"
	default:
		return "request_preparation_failed"
	}
}

func kindToMimicry(k auth.Kind) string {
	if k == auth.KindAPIKey {
		return mimicry.KindAPIKey
	}
	return mimicry.KindOAuth
}

// streamSSE copies SSE events to the client as they arrive and parses
// message_delta events to accumulate usage. When rewriteClientModel is
// non-empty, each data: JSON has its top-level "model" and nested
// "message.model" fields rewritten to that value before being forwarded.
//
// Framing uses cc-core/stream.SSEScanner so the event/data parsing logic
// is shared with other forks; this function is the proxy-specific glue
// (model rewrite + usage accumulation + flusher dispatch).
//
// Resilience (mirrors the Codex relay): headers commit lazily via commit() on
// the first downstream byte, so a stream that breaks before any output can be
// retried by the caller on another credential; a synthetic Anthropic `ping`
// event is emitted after >=10s of downstream silence so intermediaries
// (Cloudflare Tunnel / the client idle timeout) don't cut a long stream while
// the model is mid-think or running a server-side advisor sub-call; and the
// terminal event (message_stop) is tracked so a truncated upstream is reported
// instead of looking like a clean end-of-stream.
func streamSSE(c *gin.Context, resp *http.Response, counts *usage.Counts, sub *advisor.SubUsage, rewriteClientModel string, commit func()) sseRelayResult {
	flusher, _ := c.Writer.(http.Flusher)
	sc := ccstream.NewSSEScanner(resp.Body, 64*1024)
	events := 0

	// next supplies framing + model-rewrite + usage to the shared relay; the
	// relay (cc-core/stream.Relay) owns keepalive + lazy commit + write locking.
	next := func() (out []byte, terminal bool, err error) {
		if !sc.Scan() {
			if e := sc.Err(); e != nil {
				return nil, false, e
			}
			return nil, false, io.EOF
		}
		line := sc.Line()
		outLine := line
		if payload := sc.Data(); payload != nil {
			if rewriteClientModel != "" && len(payload) > 0 && payload[0] == '{' {
				if rewritten := rewriteResponseModel(payload, rewriteClientModel); rewritten != nil {
					// Preserve the original line's trailing newline style.
					trim := bytes.TrimRight(line, "\r\n")
					tail := line[len(trim):]
					rebuilt := make([]byte, 0, len("data: ")+len(rewritten)+len(tail))
					rebuilt = append(rebuilt, []byte("data: ")...)
					rebuilt = append(rebuilt, rewritten...)
					rebuilt = append(rebuilt, tail...)
					outLine = rebuilt
				}
			}
			switch sc.Event() {
			case "message_start", "message_delta":
				mergeSSEUsage(counts, sub, payload)
				events++
			case "message_stop", "error":
				terminal = true
			}
			// Strip the upstream request id out of error frames. Gated on the
			// event name inside cc-core, so the thousands of delta frames in a
			// response cost one string compare and are never parsed.
			if scrubbed, changed := downstream.ScrubSSELine(sc.Event(), outLine); changed {
				outLine = scrubbed
			}
		}
		return outLine, terminal, nil
	}

	// A synthetic `ping` event is exactly what the real Anthropic API sends
	// during gaps, so Claude Code handles it natively.
	r := ccstream.Relay(c.Writer, func() {
		if flusher != nil {
			flusher.Flush()
		}
	}, ccstream.RelayOptions{
		Commit:           commit,
		KeepaliveIdle:    10 * time.Second,
		KeepalivePayload: []byte("event: ping\ndata: {\"type\": \"ping\"}\n\n"),
		Next:             next,
	})
	return sseRelayResult{sawTerminal: r.SawTerminal, wroteAny: r.WroteAny, events: events, bytes: r.Bytes, err: r.Err}
}

// sseRelayResult reports the outcome of an Anthropic SSE relay so the caller can
// choose between a transparent retry (nothing reached the client yet) and a
// logged give-up (bytes already committed downstream — uninterruptible).
type sseRelayResult struct {
	sawTerminal bool  // message_stop / error event relayed
	wroteAny    bool  // at least one byte was committed to the client
	events      int   // message_start/_delta events relayed (diagnostics)
	bytes       int64 // bytes written downstream (diagnostics)
	err         error // underlying read error when the stream broke early
}

// errString renders an error for a log/record field, tolerating nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type usageJSON struct {
	InputTokens              int64                    `json:"input_tokens"`
	OutputTokens             int64                    `json:"output_tokens"`
	CacheCreationInputTokens int64                    `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64                    `json:"cache_read_input_tokens"`
	Iterations               []advisor.IterationUsage `json:"iterations,omitempty"`

	// CacheCreation is Anthropic's per-TTL breakdown of the cache writes that
	// CacheCreationInputTokens totals. Present since the extended-TTL beta; a
	// response that omits it leaves the sub-counts at zero, which the pricing
	// layer reads as "don't distinguish" and bills exactly as before.
	//
	// It matters because mimicry rewrites every cache breakpoint to ttl:"1h"
	// (mimicry/body.go, ClaudeDefaultCacheTTL) and Anthropic prices a 1h write
	// at 2× input against a 5m write's 1.25×. Without this field there is no
	// way to tell after the fact which rate a request should have paid.
	CacheCreation *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation,omitempty"`
}

// observed reports whether this payload carried any billable quantity. Used to
// derive Counts.Requests so "we saw usage" stays distinguishable from "the
// request completed" — conflating them served 3,808 production requests free.
func (u usageJSON) observed() bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 ||
		u.CacheCreationInputTokens > 0 || u.CacheReadInputTokens > 0
}

func (u usageJSON) toCounts() usage.Counts {
	c := usage.Counts{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CacheCreateTokens: u.CacheCreationInputTokens,
		CacheReadTokens:   u.CacheReadInputTokens,
	}
	if u.CacheCreation != nil {
		c.CacheCreate1hTokens = u.CacheCreation.Ephemeral1h
	}
	if u.observed() {
		c.Requests = 1
	}
	return c
}

// recordSubUsage charges advisor (and any future server-side sub-model)
// counts to the same auth that handled the parent request, and emits one
// extra requestlog row per distinct sub-model so by-model aggregation in
// the admin panel separates orchestrator cost from advisor cost.
//
// Returns the total advisor USD cost so the caller can fold it into the
// per-client weekly ledger as a single sum (one /v1/messages call = one
// weekly Requests bump regardless of how many sub-models ran).
//
// No-op when the response is an error (status >= 400) or there are no
// advisor iterations. Auth-side load tracking only applies to successful
// sub-calls — a failed parent rarely has billable advisor activity, and
// double-counting would distort WeightedTotal-driven load balancing.
func (s *Server) recordSubUsage(c *gin.Context, a *auth.Auth, authKind, clientToken, clientName, _ string, path string, status int, sub advisor.SubUsage) float64 {
	if status >= 400 || sub.IsEmpty() {
		return 0
	}
	provider := auth.NormalizeProvider(a.Provider)
	var total float64
	info, hasSaaS := saasInfoFrom(c)
	for subModel, sc := range sub.Snapshot() {
		// Sub-calls bump the auth's daily/hourly bucket and WeightedTotal so
		// the credential bears the full opus load. Requests stays 0: the
		// parent already counted +1.
		s.usage.Record(a.ID, a.Label, sc)
		official := s.pricing.Cost(provider, subModel, sc)
		// Advisor cost goes through the same Charge funnel as the parent
		// request so the wallet ledger and the request log agree on what
		// the user actually paid for the sub-call. Same failure contract as
		// the parent, too: this used to swallow the error and stamp the row
		// with the user and multiplier anyway, which made a dropped advisor
		// charge look billed — and, because user_id was set, invisible to
		// reconcile-charges forever.
		billed := official
		var subUserID int64
		var subMultiplier float64
		var billingErr string
		if hasSaaS && s.saas != nil {
			b, err := s.saas.Charge(chargeCtxSlot(c, "advisor:"+subModel), info, provider, subModel, sc, official)
			if err != nil {
				log.Warnf("saas: advisor charge failed for token=%d user=%d model=%s: %v", info.TokenID, info.UserID, subModel, err)
				billed = 0
				billingErr = billingDropped(err)
			} else {
				billed = b
				subUserID = info.UserID
				subMultiplier = s.saas.MultiplierFor(info, provider)
			}
		}
		total += billed
		s.emitLog(requestlog.Record{
			Client:      clientName,
			ClientToken: maskClientToken(clientToken),
			Provider:    provider,
			AuthID:      a.ID,
			AuthLabel:   a.Label,
			AuthKind:    authKind,
			Model:       subModel,
			Input:       sc.InputTokens,
			Output:      sc.OutputTokens,
			CacheRead:   sc.CacheReadTokens,
			CacheCreate: sc.CacheCreateTokens,
			CostUSD:     official,
			BilledUSD:   billed,
			Status:      status,
			// DurationMs/Stream/Attempts intentionally zero: this row is a
			// sub-call summary, not an independent request — adding wall
			// time would double-count it in admin's "total time" stats.
			Path:       path + "#advisor:" + subModel,
			UserID:     subUserID,
			Multiplier: subMultiplier,
			Error:      billingErr,
		})
	}
	return total
}

// extractUsageFromJSON pulls the top-level "usage" from a non-streaming
// /v1/messages response. Advisor sub-billing iterations are folded into
// `sub` if non-nil.
func extractUsageFromJSON(body []byte, sub *advisor.SubUsage) usage.Counts {
	var wrap struct {
		Usage usageJSON `json:"usage"`
	}
	_ = json.Unmarshal(body, &wrap)
	if sub != nil {
		sub.ReplaceFrom(wrap.Usage.Iterations)
	}
	return wrap.Usage.toCounts()
}

// mergeSSEUsage overlays usage fields from a single Anthropic SSE data
// payload onto dst, using overwrite-if-positive semantics. This is NOT
// additive: Anthropic's stream sends the input/cache token baseline in
// message_start and the cumulative final usage (often repeating the same
// input/cache values plus the real output count) in message_delta, so
// summing the two events would double-count input and cache tokens.
//
// Shapes handled:
//
//	message_start:  {type: "message_start", message: {usage: {...}}}
//	message_delta:  {type: "message_delta", usage: {...}}
//
// Zero values from a later event don't clobber a prior non-zero value —
// matches the protocol where message_delta sometimes omits the input
// fields (e.g. emits input_tokens=0).
func mergeSSEUsage(dst *usage.Counts, sub *advisor.SubUsage, payload []byte) {
	if dst == nil {
		return
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil {
		return
	}
	var u usageJSON
	if raw, ok := probe["usage"]; ok {
		_ = json.Unmarshal(raw, &u)
	} else if raw, ok := probe["message"]; ok {
		var nested struct {
			Usage usageJSON `json:"usage"`
		}
		if err := json.Unmarshal(raw, &nested); err == nil {
			u = nested.Usage
		} else {
			return
		}
	} else {
		return
	}
	if u.InputTokens > 0 {
		dst.InputTokens = u.InputTokens
	}
	if u.OutputTokens > 0 {
		dst.OutputTokens = u.OutputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		dst.CacheCreateTokens = u.CacheCreationInputTokens
	}
	if u.CacheReadInputTokens > 0 {
		dst.CacheReadTokens = u.CacheReadInputTokens
	}
	if u.CacheCreation != nil && u.CacheCreation.Ephemeral1h > 0 {
		dst.CacheCreate1hTokens = u.CacheCreation.Ephemeral1h
	}
	// Requests marks "usage was observed at least once in this stream", so it
	// latches on and is never cleared by a later event that omits the fields.
	if u.observed() {
		dst.Requests = 1
	}
	if sub != nil && len(u.Iterations) > 0 {
		// message_delta.usage.iterations is cumulative — last non-empty
		// observation wins, never append.
		sub.ReplaceFrom(u.Iterations)
	}
}

func parseRetryAfter(h http.Header) time.Time {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return time.Time{}
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Now().Add(time.Duration(n) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return t
	}
	return time.Time{}
}

// parseUnifiedRatelimitRejected inspects Anthropic's `anthropic-ratelimit-
// unified-*` headers and classifies a quota rejection.
//
// Real responses carry a snapshot like:
//
//	anthropic-ratelimit-unified-status: rejected           ← top-level decision
//	anthropic-ratelimit-unified-5h-status: allowed         ← shared 5h window
//	anthropic-ratelimit-unified-7d-status: allowed         ← shared 7d window
//	anthropic-ratelimit-unified-5h-reset / -7d-reset / -reset
//	anthropic-ratelimit-unified-representative-claim: five_hour
//
// The shared 5h/7d buckets are account-wide — every model draws on them. There
// is NO per-model bucket header: a model-scoped window (e.g. fable's weekly
// allotment, ~50% of weekly per unified-fallback-percentage) surfaces only in
// the oauth/usage `limits[]` body, never in headers. So a scoped rejection is
// inferred from the request MODEL — when the top level is rejected but neither
// shared bucket is, and the failing request is for a model with its own scope
// (fable), the cooldown is scoped to that family instead of the whole account.
//
// reqModel is the client-requested model driving this request.
//
// Returns:
//   - ok=false: not rejected.
//   - scope != "": model-family-scoped cooldown (never banned) — caller must
//     use MarkModelRateLimited so the credential keeps serving other models.
//   - scope == "", banned=false: account-wide cooldown until resetAt.
//   - scope == "", banned=true: rejected with no usable FUTURE reset stamp —
//     the stealth-ban signature (a banned account stays "rejected" forever with
//     no recovery time). Caller escalates.
func parseUnifiedRatelimitRejected(h http.Header, reqModel string) (resetAt time.Time, scope string, banned bool, ok bool) {
	const statusPrefix = "rejected"
	isRejected := func(headerName string) bool {
		v := strings.ToLower(strings.TrimSpace(h.Get(headerName)))
		return v != "" && strings.HasPrefix(v, statusPrefix)
	}

	// Shared 5h/7d buckets — a rejection here is account-wide.
	sharedBuckets := []struct{ statusHdr, resetHdr string }{
		{"Anthropic-Ratelimit-Unified-5h-Status", "Anthropic-Ratelimit-Unified-5h-Reset"},
		{"Anthropic-Ratelimit-Unified-7d-Status", "Anthropic-Ratelimit-Unified-7d-Reset"},
	}
	sharedRejected := false
	var sharedReset time.Time
	for _, b := range sharedBuckets {
		if !isRejected(b.statusHdr) {
			continue
		}
		sharedRejected = true
		if t, parsed := parseUnixSecondsHeader(h.Get(b.resetHdr)); parsed && t.After(sharedReset) {
			sharedReset = t
		}
	}

	topRejected := isRejected("Anthropic-Ratelimit-Unified-Status")
	if !sharedRejected && !topRejected {
		return time.Time{}, "", false, false
	}

	now := time.Now()
	topReset, topOK := parseUnixSecondsHeader(h.Get("Anthropic-Ratelimit-Unified-Reset"))

	// Model-scoped rejection: top-level rejected, shared buckets fine, and the
	// request is for a model with its own quota scope. Cool down only that model
	// family — never ban the whole credential. Fall back to a modest re-probe
	// window if no reset stamp is present.
	if !sharedRejected {
		if s := auth.AnthropicModelScope(reqModel); s != "" {
			if topOK && topReset.After(now) {
				resetAt = clampReset(topReset)
			} else {
				resetAt = now.Add(time.Hour)
			}
			return resetAt, s, false, true
		}
	}

	// Account-wide. Prefer the top-level reset, else the latest shared bucket
	// reset — a 7d rejection isn't released by the (sooner) 5h reset.
	if topOK && topReset.After(now) {
		return clampReset(topReset), "", false, true
	}
	if !sharedReset.IsZero() && sharedReset.After(now) {
		return clampReset(sharedReset), "", false, true
	}
	// Rejected with no future reset — the stealth-ban signature.
	return time.Time{}, "", true, true
}

// parseUnixSecondsHeader parses an `epoch-seconds` integer header value into
// a time.Time. Tolerates whitespace; returns ok=false on empty / non-integer.
func parseUnixSecondsHeader(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// clampReset caps a parsed future reset stamp at 30 days as a defense
// against malformed payloads. Caller is responsible for ensuring t is
// already in the future (past stamps are a separate signal — see
// parseUnifiedRatelimitRejected).
func clampReset(t time.Time) time.Time {
	maxV := time.Now().Add(30 * 24 * time.Hour)
	if t.After(maxV) {
		return maxV
	}
	return t
}

// parseClaudeUsageLimitBody extracts the reset timestamp from a Claude
// subscription usage-limit 429. Anthropic encodes it as
// "Claude AI usage limit reached|<unix-seconds>" in the message field, e.g.
//
//	{"type":"error","error":{"type":"rate_limit_error",
//	  "message":"Claude AI usage limit reached|1714761600"}}
//
// ok=true means we found the marker AND parsed a sane future timestamp;
// caller should treat this as a regular quota cooldown (NOT a stealth ban),
// because the body is explicit about both the cause and the recovery time.
func parseClaudeUsageLimitBody(b []byte) (time.Time, bool) {
	if len(b) == 0 {
		return time.Time{}, false
	}
	const marker = "Claude AI usage limit reached"
	lower := bytes.ToLower(b)
	idx := bytes.Index(lower, []byte(strings.ToLower(marker)))
	if idx < 0 {
		return time.Time{}, false
	}
	// Walk past the marker in the original (non-lowercased) body; we want
	// the literal "|<digits>" tail. Tolerate optional whitespace.
	tail := b[idx+len(marker):]
	for len(tail) > 0 && (tail[0] == ' ' || tail[0] == '\t') {
		tail = tail[1:]
	}
	if len(tail) == 0 || tail[0] != '|' {
		// Marker present but no timestamp — still a usage-limit signal,
		// but we have nothing to set the cooldown to. Fall back to a
		// best-effort 1h cooldown so the credential doesn't loop.
		return time.Now().Add(1 * time.Hour), true
	}
	tail = tail[1:]
	end := 0
	for end < len(tail) && tail[end] >= '0' && tail[end] <= '9' {
		end++
	}
	if end == 0 {
		return time.Now().Add(1 * time.Hour), true
	}
	secs, err := strconv.ParseInt(string(tail[:end]), 10, 64)
	if err != nil {
		return time.Now().Add(1 * time.Hour), true
	}
	t := time.Unix(secs, 0)
	// Reject obviously bogus timestamps (already passed or > 30 days out)
	// — degrade to the 1h fallback so we don't park a credential forever
	// on a malformed payload.
	if t.Before(time.Now()) || t.After(time.Now().Add(30*24*time.Hour)) {
		return time.Now().Add(1 * time.Hour), true
	}
	return t, true
}

// isLongContextRejection reports whether a 429 body is the per-request
// "this prompt is too long for your subscription's context window" rejection
// rather than a credential-level quota/rate problem. These fire when a request
// exceeds the standard 200K context and would need usage-based billing
// ("extra usage"/credits) that subscription accounts don't have — so EVERY
// credential rejects the identical request. It must not cool down the
// credential or trigger a cross-pool retry (which would flag the whole pool
// unavailable for one oversized request). Anthropic has shipped the message
// under at least two wordings, hence the multi-marker match. Case-insensitive.
func isLongContextRejection(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	lower := bytes.ToLower(b)
	markers := [][]byte{
		[]byte("extra usage is required"),                     // older wording
		[]byte("usage credits are required for long context"), // current wording
		[]byte("long context request"),                        // defensive: copy tweaks
	}
	for _, m := range markers {
		if bytes.Contains(lower, m) {
			return true
		}
	}
	return false
}

// isAccountBanBody reports whether the upstream error body looks like a
// terminal account/organization ban from Anthropic. Match is case-insensitive
// and deliberately narrow to avoid firing on routine rate-limit / usage-limit
// copy (e.g. "your organization's usage limit").
func isAccountBanBody(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	lower := bytes.ToLower(b)
	markers := [][]byte{
		[]byte("organization has been disabled"),
		[]byte("account has been disabled"),
		[]byte("account is disabled"),
		[]byte("organization is disabled"),
		// Org-level OAuth revocation. Anthropic returns a 403
		// permission_error with this exact wording when the
		// subscription account has been blocked from using OAuth
		// (typically a stealth/soft ban). Recovery requires manual
		// intervention, not a cooldown — treat as terminal.
		[]byte("oauth authentication is currently not allowed"),
	}
	for _, m := range markers {
		if bytes.Contains(lower, m) {
			return true
		}
	}
	return false
}

// isDefinitiveAuthRejection reports whether a 401 response body indicates
// Anthropic has terminally rejected the credential (subscription revoked,
// account dead, Opus access stripped). Distinguished from transient 401s
// (which we'd want to cooldown-and-retry) by matching only the explicit
// authentication-error wording: `error.type == "authentication_error"` is
// reserved by Anthropic for credential-validity failures and never used
// for quota / permission / request-shape issues. Caller must also have
// confirmed status == 401 — at other statuses these markers can appear
// in benign contexts.
func isDefinitiveAuthRejection(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	lower := bytes.ToLower(b)
	markers := [][]byte{
		[]byte(`"type":"authentication_error"`),
		[]byte(`"type": "authentication_error"`),
		[]byte("invalid authentication credentials"),
	}
	for _, m := range markers {
		if bytes.Contains(lower, m) {
			return true
		}
	}
	return false
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

// rewriteModelField is the established best-effort model mapper used for
// generic callers that do not enter the genuine Claude Code prepared path.
func rewriteModelField(body []byte, upstream string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return body, nil
	}
	mb, err := json.Marshal(upstream)
	if err != nil {
		return nil, err
	}
	obj["model"] = mb
	return json.Marshal(obj)
}

// rewriteModelFieldPreservingBytes is restricted to rewrite. It changes
// only the top-level model value so the subsequent identity rewrite can verify
// and atomically install the resulting genuine Claude Code request.
// rewriteResponseModel substitutes the client-facing model name into the
// response JSON so the client never sees the relay vendor's prefixed name
// (e.g. "[0.16]稳定喵/claude-opus-4-6"). Handles both the non-streaming
// /v1/messages response (top-level "model") and SSE event payloads
// (message_start nests "message.model"). Returns the original bytes if
// parsing fails or no known model path is present.
func rewriteResponseModel(data []byte, clientModel string) []byte {
	if len(data) == 0 || clientModel == "" {
		return data
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return data
	}
	changed := false
	newModel, err := json.Marshal(clientModel)
	if err != nil {
		return data
	}
	if _, ok := obj["model"]; ok {
		obj["model"] = newModel
		changed = true
	}
	if raw, ok := obj["message"]; ok && len(raw) > 0 && raw[0] == '{' {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err == nil {
			if _, ok := inner["model"]; ok {
				inner["model"] = newModel
				if merged, err := json.Marshal(inner); err == nil {
					obj["message"] = merged
					changed = true
				}
			}
		}
	}
	if !changed {
		return data
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return data
	}
	return out
}

// unused — kept to avoid import churn if future error types are added.
var _ = fmt.Sprintf
var _ = context.Background
