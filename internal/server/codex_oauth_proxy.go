package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/wjsoj/cc-core/apicompat"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/codexerr"
	"github.com/wjsoj/cc-core/codexws"
	"github.com/wjsoj/cc-core/downstream"
	"github.com/wjsoj/cc-core/mimicry"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/requestlog"
	"github.com/wjsoj/cc-core/servicetier"
	ccstream "github.com/wjsoj/cc-core/stream"
	"github.com/wjsoj/cc-core/usage"
)

// The Codex CLI fingerprint (codex-tui/0.135.0 identity over the legacy HTTP
// POST /codex/responses path) is now centralized in cc-core's mimicry package
// (mimicry.ApplyCodexCLIHeaders — no OpenAI-Beta on this path; real Codex sets
// it only on the WS handshake), shared with CPA-Claude so every relay stays
// in lockstep when the version target is bumped. See cc-core/mimicry/codex.go
// and cc-core/crack/codex/SPEC.md.

// The Codex backend request-shaping logic (path mapping + body sanitization)
// now lives in cc-core's mimicry package (mimicry.CodexOAuthPath /
// mimicry.SanitizeCodexRequestBody / mimicry.StripThinkingSuffix), shared with
// CPA-Claude. See cc-core/mimicry/codex_body.go.

// doForwardCodexOAuth forwards the client's /v1/responses request to the
// ChatGPT backend. Behavior matches the vendor Codex CLI: Bearer auth from
// the OAuth access_token, account_id from the cached ID-token claims, a
// session UUID that is STABLE for the conversation (see
// codexUpstreamSessionID — a fresh one per request is what a real client
// never does and what costs the upstream prompt cache), and the `codex-tui`
// User-Agent / Originator that the backend fingerprints on.
func (s *Server) doForwardCodexOAuth(c *gin.Context, a *auth.Auth, path string, body []byte, stream bool, model, clientToken, clientName, slotID string, start time.Time, attempts int) (retry, done bool) {
	// Validate before map-based sanitizers or model rewrites can collapse
	// duplicate keys and make the outbound tier ambiguous.
	validatedBody, _, validationErr := servicetier.NormalizeRequest(body)
	if validationErr != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": validationErr.Error()}})
		return false, true
	}
	body = validatedBody
	// /v1/chat/completions is bridged onto the backend's /codex/responses route
	// (codex_chat_bridge.go): the request is translated into a Responses body on
	// the way up and the Responses stream/object is rendered back as
	// chat.completion{,.chunk} on the way down. Without the bridge every
	// OpenAI-compatible client was unroutable to an OAuth subscription
	// credential and could only be served by a paid relay key.
	isChat := path == "/v1/chat/completions"
	if !isChat && path != "/v1/responses" && path != "/v1/responses/compact" {
		// Any other route genuinely has no backend equivalent. Ask the retry
		// loop to try a different credential; don't MarkFailure — this
		// credential isn't broken, just the wrong kind.
		log.Debugf("codex oauth: %s skipping %s (no ChatGPT backend equivalent)", a.ID, path)
		return true, false
	}

	// First touch of this (account, client) pair fires the auxiliary traffic a
	// real Codex client emits at startup — plugin store, MCP discovery, model
	// catalog. Inert unless codex_sidecar.enabled, and it never blocks: unlike
	// the Anthropic side there is no quota probe here whose result the first
	// business request needs to wait for.
	s.codexSidecar.Notify(a, clientToken)

	snap := a.Snapshot()
	baseURL := strings.TrimRight(s.cfg.ChatGPTBackendBaseURL, "/") + "/codex"
	// Per-credential base URL override is allowed for vendor-relay setups.
	if ab := strings.TrimRight(snap.BaseURL, "/"); ab != "" {
		baseURL = ab
	}
	// A bridged chat request is a Responses request from here on: same backend
	// route, same sanitizer, same fingerprint.
	upstreamPath := path
	if isChat {
		upstreamPath = "/v1/responses"
	}
	upURL := baseURL + mimicry.CodexOAuthPath(upstreamPath)

	sourceBody := body
	if isChat {
		converted, cerr := apicompat.ChatCompletionsToResponses(body)
		if cerr != nil {
			// A body we can't translate is a client-shape problem, not a
			// credential problem — but an API-key credential forwards
			// chat/completions verbatim and may well accept it, so roll back to
			// the loop instead of failing the request here.
			log.Infof("codex oauth: chat/completions bridge declined body via %s: %v — deferring to API-key path", a.ID, cerr)
			return true, false
		}
		sourceBody = converted
	}

	upstreamBody, _, err := mimicry.SanitizeCodexRequestBody(sourceBody, upstreamPath)
	if err != nil {
		log.Warnf("codex oauth: body sanitize failed via %s: %v", a.ID, err)
		upstreamBody = sourceBody
	}

	normalizedBody, outboundTier, tierErr := servicetier.NormalizeRequest(upstreamBody)
	if tierErr != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": tierErr.Error()}})
		return false, true
	}
	upstreamBody = normalizedBody

	ctx := c.Request.Context()
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(upstreamBody))
	if err != nil {
		writeAPIError(c, auth.ProviderOpenAI, APIError{Status: http.StatusInternalServerError, Code: "request_preparation_failed", Message: "The request could not be prepared. Please try again."})
		return false, true
	}
	copyForwardableHeaders(c.Request.Header, upReq.Header)
	stripIngressHeaders(upReq.Header)

	accessToken, _ := a.Credentials()
	accountID, _ := a.CodexIdentity()
	isCompactPath := path == "/v1/responses/compact"
	// Apply the Codex CLI fingerprint — codex-tui identity (Originator /
	// User-Agent / Version) over the HTTP POST /codex/responses{,/compact}
	// path. Centralized in cc-core (mimicry.ApplyCodexCLIHeaders) so every relay
	// stays in lockstep when the version target is bumped. See cc-core/crack/codex/SPEC.md.
	//
	// The routing hint is derived from upstreamBody — the bytes actually going
	// out, after sanitization — so the header can never name a different model
	// than the body does.
	routingModel, routingTier := mimicry.CodexModelAndTier(upstreamBody)
	// Hoisted because the WebSocket egress below needs the SAME value: it is
	// both the pool key and the frame identity's session id, so a conversation
	// keeps one upstream session — and therefore one prompt-cache namespace —
	// whichever transport ends up carrying a given turn.
	upstreamSessionID := s.codexUpstreamSessionID(a, clientToken, slotID, body)
	mimicry.ApplyCodexHeadersWithSession(upReq, mimicry.DefaultCodexProfile(), accessToken, accountID,
		isCompactPath, routingModel, routingTier, upstreamSessionID)

	// Shared pooled transport (per proxyURL). Reusing HTTP/2 connections is
	// critical here: chatgpt.com's CF edge rate-limits new TCP/TLS connections
	// from VPS/proxy IPs and RSTs the handshake when the per-IP new-connection
	// quota is hit — the classic alternating 200/503 symptom. A pooled h2 conn
	// carries many requests so we stay under the limit. ClientFor's transport
	// has HTTP/2 PING health checks (utls.go) so stale reused conns are
	// detected and re-dialed transparently.
	client := auth.ClientFor(snap.ProxyURL, s.cfg.UseUTLS)
	// Transient wire-level flaps (CF edge RST mid-handshake, h2 PROTOCOL_ERROR /
	// REFUSED_STREAM, `connection reset by peer`, stale pooled h2 conn) are
	// replayed with exponential backoff + jitter inside ClientFor's transport
	// (cc-core auth.retryRoundTripper) on this same credential — see
	// auth.IsTransientNetErr. By the time Do returns an error, that backoff loop
	// is already exhausted, so a transient error surviving to here means the
	// flap is persistent; we defer to the outer loop (another credential)
	// without MarkFailure rather than burning this one.
	// dispatchAt anchors this ATTEMPT's upstream clock. `start` is the whole
	// request's, so on a third attempt it already carries two credentials'
	// worth of failover; time-to-first-byte has to be measured against the
	// credential that actually served the turn or it says nothing about that
	// credential's egress.
	dispatchAt := time.Now()

	// WebSocket egress, when configured. The turn comes back dressed as an
	// *http.Response whose body is the upstream frames rendered as SSE, so
	// everything below this point — status handling, the streaming relays, the
	// non-streaming aggregator, billing, logging — is transport-agnostic.
	//
	// A failure here has not written a byte downstream, so falling back to the
	// HTTP request already prepared above costs the caller latency and nothing
	// else. dispatchAt is re-anchored so the time-to-first-byte we record
	// belongs to the transport that actually served the turn rather than
	// including a failed WebSocket dial.
	var resp *http.Response
	upstreamTransport := "http"
	// True when the turn went out on a socket the pool had already opened for
	// an earlier turn. It decides whether a pre-output transport break is worth
	// blaming on the credential; see the stale-socket branch below.
	wsReused := false
	if s.codexWSEgress.eligible(a, path, snap.BaseURL) {
		turn, werr := s.codexWSEgress.dial(ctx, a, upstreamBody, upstreamSessionID, routingModel, routingTier)
		switch {
		case werr == nil:
			resp = turn.resp
			upstreamTransport = "ws"
			wsReused = turn.reused
		case !s.cfg.CodexWS.Upstream.HTTPFallbackAllowed():
			// Diagnostic mode: surface the transport fault instead of masking
			// it behind a fallback that would make it invisible. Still a
			// credential-level rollback rather than a client-visible error —
			// another credential may well dial fine.
			log.Warnf("codex ws egress: %s dial failed and fallback is disabled: %v", a.ID, werr)
			return true, false
		default:
			s.codexWSEgress.noteFailure(a.ID, werr)
			dispatchAt = time.Now()
		}
	}
	if resp == nil {
		resp, err = client.Do(upReq)
	}
	if err != nil {
		if isClientDisconnect(ctx, err) {
			a.MarkClientCancel(err.Error())
			s.emitLog(requestlog.Record{
				Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.ProviderOpenAI,
				AuthID: a.ID, AuthLabel: a.Label, AuthKind: "oauth", Model: model,
				Stream: stream, Path: path, Status: 499, Attempts: attempts,
				DurationMs: time.Since(start).Milliseconds(),
				Error:      "client canceled",
			})
			return false, true
		}
		// Transient infra failure that survived the same-cred retry loop:
		// don't MarkFailure (would degrade the credential / show as unhealthy
		// in the admin panel), don't emit a request log row. Just ask the
		// outer loop to try another credential — and if that one is also the
		// only one, it'll come right back here for another round of retries.
		if isTransientNetErr(err) {
			log.Infof("codex oauth: transient net error survived same-cred retries via %s: %v (deferring to outer loop without MarkFailure)", a.ID, err)
			return true, false
		}
		a.MarkFailure(err.Error())
		log.Warnf("codex oauth: upstream error via %s: %v", a.ID, err)
		return true, false
	}

	// Capture rolling primary/secondary quota snapshot from upstream response
	// headers (the `x-codex-*` family). Done unconditionally since 4xx/429
	// responses also carry these — they're what tell us *why* we were blocked.
	a.CaptureCodexRateLimits(resp.Header)

	// Pre-read error bodies to inspect ChatGPT's usage-limit signals.
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden:
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resetAt := parseCodexResetAt(errBody)
		if resetAt.IsZero() {
			resetAt = parseRetryAfter(resp.Header)
		}
		// A usage_limit_reached body is the window filling: record it as the
		// measurement cc-core/quotaestimate anchors on ("what was this
		// account's last full window worth"). Per-request throttling
		// ({"detail":"Rate limit exceeded"}) and capacity bodies carry no
		// reset of their own and are not a measurement of anything.
		if resp.StatusCode == http.StatusTooManyRequests && !resetAt.IsZero() && isCodexUsageLimitBody(errBody) {
			a.MarkUsageLimitReached(resetAt)
		}
		log.Warnf("codex oauth: credential %s received %d: %s", a.ID, resp.StatusCode, truncate(errBody, 240))
		if resp.StatusCode == http.StatusUnauthorized {
			// A rejected bearer is handled on its own terms — strike counter,
			// forced refresh, cooldown only when neither helps — rather than
			// as a generic upstream error. See codex_auth_reject.go.
			s.rejectCodexBearer(a, accessToken, errBody)
			return true, false
		}
		s.pool.ReportUpstreamError(a, resp.StatusCode, resetAt)
		return true, false
	}
	// Capacity errors come back with 200+JSON on some edge deployments or
	// as 4xx; the body message is what we actually key on.
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if isCodexCapacityError(errBody) {
			resetAt := parseCodexResetAt(errBody)
			s.pool.ReportUpstreamError(a, http.StatusTooManyRequests, resetAt)
			return true, false
		}
		// Bridged chat clients routinely ask for relay-only model names
		// (gpt-5.6-terra-high, gpt-4o-mini, …) that the ChatGPT backend doesn't
		// host. Before the bridge those requests went straight to an API key;
		// now that OAuth is tried first, a model rejection must roll back to
		// the API-key pool rather than surface a 400 the client can't act on.
		// Scoped to the bridged route so native /v1/responses keeps relaying
		// its upstream 4xx verbatim. The credential is healthy — no MarkFailure.
		if isChat && codexModelUnsupported(resp.StatusCode, errBody) {
			log.Infof("codex oauth: %s does not host model %s (upstream %d) — rotating to an API-key credential", a.ID, model, resp.StatusCode)
			return true, false
		}
		copySafeRetryHeaders(c, resp.Header)
		writeAPIError(c, auth.ProviderOpenAI, publicUpstreamError(resp.StatusCode, errBody))
		s.emitLog(requestlog.Record{
			Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.ProviderOpenAI,
			AuthID: a.ID, AuthLabel: a.Label, AuthKind: "oauth", Model: model,
			Stream: stream, Path: path, Status: resp.StatusCode, Attempts: attempts,
			DurationMs: time.Since(start).Milliseconds(),
			Error:      fmt.Sprintf("upstream %d", resp.StatusCode),
		})
		return false, true
	}

	// The stall budget exists to convert a turn the backend parked into a
	// failover, and the failover is gone at the first byte the client receives.
	// Retire it there: past the commit it can only cut a slow turn into a
	// truncated one, which was the single largest source of truncated Codex
	// streams the day it shipped.
	//
	// Captured here rather than inside the relay because the observer below
	// wraps the body and hides the method — the first version of this fix read
	// the wrapper and silently did nothing.
	disarmStall := codexStallDisarmer(resp.Body)
	// Observe original bytes before protocol conversion or response scrubbing.
	tierObserver := servicetier.ObserveBody(resp.Body)
	resp.Body = tierObserver
	var priced pricing.CostResult
	var counts usage.Counts
	var streamErr string
	// firstOutputAt is when this credential's upstream started producing, set
	// by whichever relay served the turn. Zero on the paths that assemble a
	// whole body before answering, where there is no meaningful first byte to
	// separate out.
	var firstOutputAt time.Time
	// upstreamModel is reported by the streaming relay only. The aggregating
	// and compact paths assemble a whole body and never classify frames, so
	// they have no terminal event to read it off; an empty value there means
	// "not observed", not "matched".
	var upstreamModel string
	// Status recorded in the request log. Defaults to the upstream's, but a
	// mid-stream client hang-up overrides it to 499 — the response was 200 on
	// the wire, yet logging it as a success with an error attached hides it
	// from every "client canceled" view.
	logStatus := resp.StatusCode
	switch {
	case isCompactPath:
		// /codex/responses/compact returns a single JSON object — no SSE.
		// Read it once, extract usage, pass through verbatim. Matches sub2api's
		// handleNonStreamingResponsePassthrough behavior on this path.
		payload, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			log.Warnf("codex oauth: read compact body via %s: %v", a.ID, rerr)
			writeAPIError(c, auth.ProviderOpenAI, APIError{Status: http.StatusBadGateway, Code: "service_response_error", Message: "The model service returned an unreadable response. Please try again."})
			s.emitLog(requestlog.Record{
				Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.ProviderOpenAI,
				AuthID: a.ID, AuthLabel: a.Label, AuthKind: "oauth", Model: model,
				Stream: stream, Path: path, Status: 502, Attempts: attempts,
				DurationMs: time.Since(start).Milliseconds(),
				Error:      rerr.Error(),
			})
			return false, true
		}
		mergeCodexUsage(&counts, extractCodexBackendUsageFromJSON(payload))
		// Allowlist, not a hop-by-hop denylist. Forwarding everything else
		// handed the caller our pool's operational state: the x-codex-*
		// rate-limit headers (the serving account's window utilisation and
		// reset times), openai-organization, x-oai-request-id, set-cookie and
		// cf-ray — whose suffix is the Cloudflare datacentre our egress sits
		// in. The Claude path has used this allowlist since it was written;
		// only Codex was still copying verbatim.
		downstream.CopyResponseHeaders(c.Writer.Header(), resp.Header, time.Now())
		c.Writer.Header().Set("Content-Type", "application/json")
		c.Writer.WriteHeader(resp.StatusCode)
		_, _ = c.Writer.Write(payload)
	case stream:
		// Streaming client: passthrough SSE verbatim (with keepalive + terminal
		// tracking), or — on the bridged chat route — translated frame by frame
		// into chat.completion.chunk. A truncated upstream (no terminal event)
		// is surfaced in the request log instead of looking like a clean stream
		// end, on both paths.
		// Branching here rather than through a relay closure is deliberate:
		// capturing resp in a closure makes bodyclose lose track of the
		// unconditional Close after this switch and report a false leak.
		var sawTerminal, wroteAny bool
		var rerr error
		var res codexStreamResult
		// Both relays commit lazily: headers are written immediately before the
		// first byte, so a shed that arrives before any output leaves the
		// response uncommitted and the failover below is invisible to the
		// client. They report the same codexStreamResult, so everything
		// downstream is shared.
		if isChat {
			res = streamCodexAsChatCompletions(c, resp.Body, &counts, model, chatStreamWantsUsage(body), func() { disarmStall(); writeSSEResponseHeaders(c, resp) })
		} else {
			res = streamSSECodexBackend(c, resp, &counts, func() { disarmStall(); writeSSEResponseHeaders(c, resp) })
		}
		// A turn the backend parked and never scheduled is a capacity refusal
		// that happens to be shaped like silence: over the WebSocket it arrives
		// as keepalives instead of the error frame the HTTP transport sends, so
		// nothing above classifies it. Name it here, once, for both relays —
		// from this point it takes exactly the withhold-and-fail-over path a
		// shed frame would have, including the per-(credential, model)
		// demotion that steers the retry somewhere else.
		//
		// Safe only because the stream's budget is shorter than the withhold
		// cap: the preamble is still buffered, so wroteAny is false and the
		// failover stays invisible.
		if res.shed == "" && !res.wroteAny && errors.Is(res.err, codexws.ErrStalled) {
			res.shed = codexStalledShedLabel
		}
		sawTerminal, wroteAny, rerr = res.sawTerminal, res.wroteAny, res.err
		upstreamModel = res.upstreamModel
		firstOutputAt = res.firstOutputAt
		// A shed that landed after output started could not be withheld. Say so
		// either way: on the native route the CLI quietly retries, on the chat
		// route the frame is dropped in translation, and in both cases nothing
		// would otherwise record that upstream refused to serve — the turn
		// reaches the operator as one that finished with no usage, which reads
		// as a broken relay rather than a busy account.
		if res.demoted.shed {
			// What the client ends up seeing differs by route — the native
			// relay demotes the code so the CLI retries, while the chat bridge
			// cannot because the translation drops error frames outright — so
			// this says only what upstream did.
			log.Warnf("codex oauth: %s shed the turn after output started (capacity=%v)", a.ID, res.demoted.capacity)
			streamErr = shedTurnLabel(res.demoted.capacity)
			a.MarkModelShed(model, time.Now())
		}
		if !sawTerminal && !wroteAny {
			// Nothing reached the client yet, so this turn can still be rescued
			// on another credential without the caller ever knowing.
			_ = resp.Body.Close()
			if res.shed != "" {
				// Upstream shed this turn for capacity/quota inside an
				// otherwise-200 stream. Credential health is deliberately NOT
				// touched: production shows sheds are account-and-moment
				// scoped, and cooling the account would take all of its other
				// models offline over a condition that clears on its own.
				//
				// What IS recorded is a per-(credential, model) demotion, so
				// the scheduler sends this model somewhere else for the next
				// minute or so while everything else on the account keeps
				// scheduling normally. It orders candidates rather than
				// removing them, so a window where the model is at capacity
				// everywhere still routes instead of reporting an empty pool.
				a.MarkModelShed(model, time.Now())
				// A capacity refusal is not a verdict on the credential — it is
				// a verdict on how expensive THIS turn is to schedule, and the
				// credential that just refused it is the only one in the pool
				// holding this conversation's prompt cache. Rotating therefore
				// re-rolls the same dice with a strictly worse hand: the next
				// account has to prefill from scratch, which is the very thing
				// upstream just declined to do.
				//
				// Production says the shed is a property of the request, not of
				// the moment or the account. Over one 104-minute storm 70.4% of
				// turns succeeded on the FIRST credential while a minority were
				// refused again and again — nothing like the 36% a memoryless
				// per-attempt coin flip would give at the same 64% refusal rate
				// — and the per-minute spread of multi-attempt turns was only
				// 1.53x over-dispersed, so "bad moment" does not explain it
				// either. Turns that sailed through carried 10k tokens of
				// uncached prefill at 88% cache hit; turns that burned five or
				// more credentials carried 43k at 33%.
				//
				// So: ask the same credential again before giving up on it. The
				// forward loop bounds this (codexSameCredShedRetries) and then
				// rotates exactly as before.
				if codexCapacityShedCodes[res.shedCode] {
					c.Set(codexCapacityShedKey, true)
				}
				log.Warnf("codex oauth: %s shed the request before any output over %s (attempt %d, %s, code=%q): %s — retrying",
					a.ID, upstreamTransport, attempts, time.Since(start).Round(time.Millisecond), res.shedCode, res.shed)
				// Record the shed as a credential-attempt row. It is withheld
				// from the client by design, and until now it was withheld from
				// the archive too: 3315 of these in one 16-hour production
				// window left not a single row behind, so the only evidence
				// that a quarter of all turns were paying a ~21s upstream
				// stall was the journal. AttemptOnly keeps it out of every
				// user-visible aggregate — this is telemetry, not a request the
				// customer made or owes anything for.
				s.emitLog(requestlog.Record{
					Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.ProviderOpenAI,
					AuthID: a.ID, AuthLabel: a.Label, AuthKind: "oauth", Model: model,
					Stream: stream, Path: path, Status: resp.StatusCode, Attempts: attempts,
					DurationMs:  time.Since(dispatchAt).Milliseconds(),
					AttemptOnly: true,
					Error:       shedPreOutputLabel,
				})
				return true, false
			}
			if isClientDisconnect(ctx, rerr) {
				a.MarkClientCancel("client canceled before first event")
				s.emitLog(requestlog.Record{
					Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.ProviderOpenAI,
					AuthID: a.ID, AuthLabel: a.Label, AuthKind: "oauth", Model: model,
					Stream: stream, Path: path, Status: 499, Attempts: attempts,
					DurationMs: time.Since(start).Milliseconds(),
					Error:      "client canceled before first event",
				})
				return false, true
			}
			// A turn on a REUSED socket that produced no frame at all is the
			// signature cc-core's Lease.Reused exists to name: the backend
			// closed the connection while it sat idle in the pool, and the
			// pool's probe cannot see it because Ping only writes a control
			// frame — a half-closed socket accepts that write and hands back
			// the queued close on the first read. Production shows the shape
			// plainly: 131 of one hour's 513 pre-output breaks died in under a
			// second, far too fast for any real keepalive timeout.
			//
			// The credential is healthy, so rotating away from it is the wrong
			// move twice over — it burns one of the twelve failover rounds, and
			// it lands the turn on an account whose prompt cache has never seen
			// this conversation. Ask the loop to try this same credential once
			// more instead; the pool already dropped the dead entry on the
			// abnormal release, so the retry dials fresh.
			if upstreamTransport == "ws" && wsReused {
				c.Set(codexStaleSocketRetryKey, true)
				log.Warnf("codex oauth: stale pooled socket via %s (attempt %d, %s): %v — retrying on the same credential",
					a.ID, attempts, time.Since(start).Round(time.Millisecond), rerr)
				return true, false
			}
			log.Warnf("codex oauth: stream broke before any output via %s (attempt %d, %s): %v — retrying on another credential",
				a.ID, attempts, time.Since(start).Round(time.Millisecond), rerr)
			return true, false
		}
		if !sawTerminal {
			// A stream that ends without a terminal event is only an upstream
			// fault when the upstream is what went away. The far more common
			// cause is the client hanging up mid-turn — Codex CLI aborts the
			// request on Ctrl-C / ESC — which cancels c.Request.Context() and
			// surfaces here as a read error, indistinguishable from truncation
			// unless the context is consulted.
			//
			// Conflating the two made ordinary user behaviour look like an
			// upstream incident: in production this label accounted for ~90% of
			// all recorded Codex errors, drowning out the ~0.05% of genuine h2
			// truncations. Match the transport-error branch above (and the
			// Anthropic path) and name each for what it is.
			switch {
			case isClientDisconnect(ctx, rerr):
				streamErr = "client canceled"
				// 499 + MarkClientCancel match the pre-stream disconnect branch
				// above, so a mid-stream hang-up lands in the same bucket as one
				// that happened a second earlier instead of as a 200 carrying an
				// error string. MarkClientCancel is health-neutral by design —
				// the credential did nothing wrong.
				logStatus = 499
				a.MarkClientCancel("client canceled mid-stream")
				log.Infof("codex oauth: client canceled mid-stream via %s", a.ID)
			case res.fatalCode != "":
				// The stream ended because WE forwarded a fatal error frame —
				// upstream rejecting the request, not upstream dying mid-turn.
				// Calling that "truncated" put a client's own bad requests into
				// the same bucket as a broken backend and inflated the metric
				// the whole afternoon's work was being judged on: at the end of
				// it, every remaining "truncation" was one client sending
				// invalid_request_error six times out of six while the other 97
				// turns in the window had none.
				streamErr = "upstream rejected the request: " + res.fatalCode
				log.Warnf("codex oauth: %s upstream rejected the request via %s after %s (code=%s): %v",
					upstreamTransport, a.ID, time.Since(start).Round(time.Millisecond), res.fatalCode, rerr)
			default:
				streamErr = "stream truncated before terminal event"
				// The transport is named because it is the number this whole
				// egress change is judged on: a WebSocket carries protocol-level
				// ping/pong across the silences that truncate an idle HTTP
				// stream, so if it is working, this line stops saying "ws".
				log.Warnf("codex oauth: %s stream ended before terminal event (truncated upstream) via %s after %s (committed by %q%s%s): %v",
					upstreamTransport, a.ID, time.Since(start).Round(time.Millisecond), res.committedBy,
					codexCommittedLineSuffix(res.committedLine), codexFatalCodeSuffix(res.fatalCode), rerr)
			}
		}
	default:
		// Non-streaming client: aggregate SSE into a single response object
		// (mirrors CLIProxyAPI's CodexExecutor.Execute aggregation).
		payload, aggShed, aerr := aggregateCodexResponseStream(resp.Body, &counts)
		// Both branches below roll back to the forward loop instead of
		// answering. A non-streaming response is assembled first and sent
		// second, so at this point not one byte has reached the client and
		// another credential can serve the turn invisibly.
		//
		// This path used to answer 502 on both. In production that made the
		// non-streaming route 50x worse than the streaming one — 5.0% of
		// non-streaming turns failed against 0.1% of streaming — because every
		// shed landed on the client while the streaming relay was quietly
		// failing them over. All of them were capacity sheds: same model, ~2.3s,
		// the shape of a turn upstream refused rather than one it botched.
		if aggShed != "" {
			log.Warnf("codex oauth: %s shed the non-streaming request (attempt %d, %s): %s — retrying on another credential",
				a.ID, attempts, time.Since(start).Round(time.Millisecond), aggShed)
			_ = resp.Body.Close()
			return true, false
		}
		if aerr != nil {
			// A client that hung up mid-aggregation surfaces here as the same
			// read error an upstream fault would — but there is nobody left to
			// retry for. Rotating anyway burns a credential per attempt on a
			// request no one is listening to, and ends in a 502 delivered to a
			// closed connection. Seen in production immediately after this
			// branch learned to retry: one canceled turn spent 8 attempts.
			if isClientDisconnect(ctx, aerr) {
				a.MarkClientCancel("client canceled before aggregation completed")
				_ = resp.Body.Close()
				s.emitLog(requestlog.Record{
					Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.ProviderOpenAI,
					AuthID: a.ID, AuthLabel: a.Label, AuthKind: "oauth", Model: model,
					Stream: stream, Path: path, Status: 499, Attempts: attempts,
					DurationMs: time.Since(start).Milliseconds(),
					Error:      "client canceled",
				})
				return false, true
			}
			log.Warnf("codex oauth: aggregation via %s failed: %v — retrying on another credential", a.ID, aerr)
			_ = resp.Body.Close()
			s.emitLog(requestlog.Record{
				Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.ProviderOpenAI,
				AuthID: a.ID, AuthLabel: a.Label, AuthKind: "oauth", Model: model,
				Stream: stream, Path: path, Status: 502, Attempts: attempts,
				DurationMs: time.Since(start).Milliseconds(),
				Error:      aerr.Error(),
			})
			return true, false
		}
		if isChat {
			converted, cerr := apicompat.ResponsesToChatCompletion(payload, model, time.Now().Unix())
			if cerr != nil {
				log.Warnf("codex oauth: chat/completions render via %s failed: %v", a.ID, cerr)
				writeAPIError(c, auth.ProviderOpenAI, APIError{Status: http.StatusBadGateway, Code: "service_response_error", Message: "The model service returned an incomplete response. Please try again."})
				_ = resp.Body.Close()
				s.emitLog(requestlog.Record{
					Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.ProviderOpenAI,
					AuthID: a.ID, AuthLabel: a.Label, AuthKind: "oauth", Model: model,
					Stream: stream, Path: path, Status: 502, Attempts: attempts,
					DurationMs: time.Since(start).Milliseconds(),
					Error:      cerr.Error(),
				})
				return false, true
			}
			payload = converted
		}
		// Same allowlist as the non-streaming branch above. Content-Type is
		// overwritten right after: this branch aggregates an SSE stream into a
		// single JSON body, so the upstream's text/event-stream would be a lie.
		downstream.CopyResponseHeaders(c.Writer.Header(), resp.Header, time.Now())
		c.Writer.Header().Set("Content-Type", "application/json")
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write(payload)
	}
	_ = resp.Body.Close()

	s.usage.Record(a.ID, a.Label, counts)
	// CostUSD = official upstream price, BilledUSD = wallet debit.
	var costUSD, billedUSD float64
	var userID int64
	var multiplier float64
	var billingErr string
	if resp.StatusCode < 400 && counts.Requests > 0 && clientToken != "" {
		priced = s.pricing.CostWithOptions(auth.ProviderOpenAI, billingModelFor(a, model), counts, pricing.CostOptions{ServiceTier: outboundTier, ResponseServiceTier: tierObserver.Observed(), CodexOAuth: true})
		official := priced.CostUSD
		costUSD = official
		billedUSD = official
		// Same single-funnel as Anthropic: hand the official cost to the
		// SaaS adapter, get back the billed amount, log/charge with it.
		if info, ok := saasInfoFrom(c); ok && s.saas != nil {
			billed, err := s.saas.Charge(chargeCtx(c), info, auth.ProviderOpenAI, model, counts, official)
			if err != nil {
				log.Warnf("saas: charge failed for token=%d user=%d: %v", info.TokenID, info.UserID, err)
				// Nobody was debited, so the row must not carry the official
				// price as revenue; the marker is what makes the drop findable.
				billedUSD = 0
				billingErr = billingDropped(err)
			} else {
				billedUSD = billed
				userID = info.UserID
				multiplier = s.saas.MultiplierFor(info, auth.ProviderOpenAI)
			}
		}
		s.usage.RecordClient(clientToken, clientName, counts, billedUSD)
	}
	s.emitLog(requestlog.Record{
		RequestedServiceTier: outboundTier,
		UpstreamServiceTier:  tierObserver.Observed(),
		ServiceTier:          priced.Tier.Billing,
		Client:               clientName,
		ClientToken:          maskClientToken(clientToken),
		Provider:             auth.ProviderOpenAI,
		AuthID:               a.ID,
		AuthLabel:            a.Label,
		AuthKind:             "oauth",
		Model:                model,
		Input:                counts.InputTokens,
		Output:               counts.OutputTokens,
		ReasoningTokens:      counts.ReasoningTokens,
		UpstreamModel:        upstreamModel,
		CacheRead:            counts.CacheReadTokens,
		CostUSD:              costUSD,
		BilledUSD:            billedUSD,
		UserID:               userID,
		Multiplier:           multiplier,
		Status:               logStatus,
		DurationMs:           time.Since(start).Milliseconds(),
		TTFBMs:               upstreamTTFBMillis(dispatchAt, firstOutputAt),
		Stream:               stream,
		Path:                 path,
		Attempts:             attempts,
		Error:                joinLogError(streamErr, billingErr),
	})
	if resp.StatusCode < 400 {
		a.MarkSuccess()
		if streamErr == "" {
			// Served this model without being shed, so any capacity demotion
			// recorded earlier is stale — capacity comes back abruptly, and a
			// credential that has just proved it can serve the model should
			// compete on equal terms for the next request rather than sitting
			// out the rest of its window.
			a.NoteModelServed(model)
		}
	}
	return false, true
}

// aggregateCodexResponseStream reads the backend SSE stream and returns
// the final response JSON object for a non-streaming client. Mirrors the
// aggregation in CLIProxyAPI's CodexExecutor.Execute: collects
// `response.output_item.done` items (keyed by output_index when present,
// falling back to arrival order), then on `response.completed` patches
// the response.output field if it arrived empty. Output shape matches
// OpenAI's /v1/responses non-streaming reply: the bare `response` object
// (id, object, output, usage, …) — not the SSE event envelope.
func aggregateCodexResponseStream(r io.Reader, counts *usage.Counts) (out []byte, shed string, err error) {
	reader := newLineReader(r)
	var byIndex []codexOutputSlot
	var fallback []json.RawMessage

	for {
		line, rerr := reader.readLine()
		if len(line) > 0 {
			trim := bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(trim, []byte("data:")) {
				payload := bytes.TrimSpace(trim[5:])
				if len(payload) > 0 && payload[0] == '{' {
					// A non-streaming turn is shed exactly like a streaming one
					// — an error frame inside an otherwise-200 stream — but this
					// path never looked for it, so the aggregation simply ran to
					// EOF and reported "stream closed before response.completed"
					// as a 502. Nothing has been written downstream yet here
					// (the response is assembled first, sent second), so a shed
					// is fully recoverable on another credential; report it and
					// let the caller fail over.
					if codexerr.Classify(payload) == codexerr.ClassRetryable {
						return nil, truncate(payload, 200), nil
					}
					var ev struct {
						Type        string          `json:"type"`
						Item        json.RawMessage `json:"item"`
						OutputIndex *int64          `json:"output_index"`
						Response    json.RawMessage `json:"response"`
					}
					if err := json.Unmarshal(payload, &ev); err == nil {
						switch ev.Type {
						case "response.output_item.done":
							if len(ev.Item) > 0 {
								if ev.OutputIndex != nil {
									byIndex = append(byIndex, codexOutputSlot{idx: *ev.OutputIndex, data: ev.Item})
								} else {
									fallback = append(fallback, ev.Item)
								}
							}
						case "response.completed":
							if len(ev.Response) == 0 {
								return nil, "", errors.New("response.completed missing response field")
							}
							mergeCodexUsage(counts, extractCodexBackendUsageFromJSON(payload))
							payload, perr := patchResponseOutput(ev.Response, byIndex, fallback)
							return payload, "", perr
						}
					}
				}
			}
		}
		if rerr != nil {
			return nil, "", fmt.Errorf("stream closed before response.completed: %w", rerr)
		}
	}
}

// patchResponseOutput replaces response.output with the collected
// output_item.done events when the completed event arrived with an empty
// or missing output array. Returns the (possibly unchanged) response JSON.
type codexOutputSlot struct {
	idx  int64
	data json.RawMessage
}

func patchResponseOutput(response json.RawMessage, byIndex []codexOutputSlot, fallback []json.RawMessage) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(response, &obj); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	// Only patch if the existing output is missing or empty.
	needsPatch := true
	if cur, ok := obj["output"]; ok {
		t := bytes.TrimSpace(cur)
		if len(t) > 2 && !bytes.Equal(t, []byte("[]")) && !bytes.Equal(t, []byte("null")) {
			needsPatch = false
		}
	}
	if needsPatch && (len(byIndex) > 0 || len(fallback) > 0) {
		sort.SliceStable(byIndex, func(i, j int) bool { return byIndex[i].idx < byIndex[j].idx })
		items := make([]json.RawMessage, 0, len(byIndex)+len(fallback))
		for _, s := range byIndex {
			items = append(items, s.data)
		}
		items = append(items, fallback...)
		patched, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		obj["output"] = patched
	}
	return json.Marshal(obj)
}

// codexTerminalEvent reports whether a Codex backend SSE data payload is a
// stream-terminating event. The client (codex-core) waits for one of these; if
// the upstream stream EOFs without it, the client raises
// "stream disconnected before completion".
func codexTerminalEvent(payload []byte) bool {
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return false
	}
	switch ev.Type {
	case "response.completed", "response.failed", "response.incomplete",
		"response.cancelled", "response.canceled":
		return true
	}
	return false
}

// codexPreambleEvent reports whether a Codex SSE payload is one of the
// content-free events — the ones upstream opens with, plus the keepalive it
// emits while a turn waits for capacity. They carry no model output,
// so holding them back costs the client nothing — and it keeps the response
// uncommitted long enough for a capacity shed to be withheld and failed over
// instead of being forwarded as an error the user has to see.
// codexEventType reads a Codex event payload's declared type, or "" when the
// payload is not an object with one.
func codexEventType(payload []byte) string {
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return ""
	}
	return ev.Type
}

func codexPreambleEvent(payload []byte) bool {
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return false
	}
	// A frame that declares no type at all cannot be model output: every event
	// a client renders names itself. Over the WebSocket such a frame renders as
	// a bare `data:` line — codexws.appendSSEEvent omits the event line when
	// the type is empty — and emitting it committed the response before a
	// single delta had arrived.
	//
	// That is what `committed by ""` names in the truncation log, and it was
	// the most common way the withhold window closed: 141 of one afternoon's
	// 326 truncated streams were a turn committed by an untyped frame, parked
	// by the backend, and then cut at the stall budget with no failover left.
	if ev.Type == "" {
		return true
	}
	return codexContentFreeEvents[ev.Type]
}

// codexContentFreeEvents are the Responses events that carry no model-visible
// text. Everything here can be replayed by a retry, so holding it back costs
// the client nothing and keeps the response uncommitted for longer.
//
// The captured opening sequence (crack/codexapp0.147.0/rows/13-ws-server-events)
// runs:
//
//	response.created → response.in_progress → response.output_item.added
//	→ response.content_part.added → response.output_text.delta
//
// with the first four carrying no text and the delta being the first thing a
// user could actually see. Only the first two were listed here, so a capacity
// shed arriving during the two `.added` events — a window that is wide, because
// it spans the model's entire time-to-first-token — was treated as
// unrecoverable and forwarded to the client as "Our servers are currently
// overloaded", when it could have been withheld and retried on another
// credential.
//
// The rule is `.added`-style declarations plus the two openers: an `.added`
// event announces a container (an output item, a content part, a reasoning
// summary part) whose text always arrives afterwards in a `.delta`. It is an
// explicit list rather than a suffix match so that a future event type has to
// be read and classified rather than silently inheriting this behaviour.
//
// `keepalive` is the one that is not part of that opening sequence, and it is
// the one that mattered most in production. When the backend cannot schedule a
// turn it parks the request and emits `event: keepalive` /
// `data: {"type":"keepalive","sequence_number":N}` about every 30s, then sheds
// the turn a fraction of a second after one of those heartbeats. Three sampled
// sheds all had the identical shape:
//
//	response.created → response.in_progress → keepalive(31s) → error → response.failed
//
// The heartbeat carries no model output and no response id — it is replayable
// by definition — but it was not listed here, so it committed the response and
// the shed 0.4s behind it could only be demoted and handed to the client as
// "Our servers are currently overloaded". That is what the user sees as a
// reconnect. It accounted for essentially all of the unrecoverable sheds: of
// 510 shed turns in one six-hour window, 443 were never retried at all.
var codexContentFreeEvents = map[string]bool{
	"response.created":                      true,
	"response.in_progress":                  true,
	"response.output_item.added":            true,
	"response.content_part.added":           true,
	"response.reasoning_summary_part.added": true,
	"keepalive":                             true,

	// The three the WebSocket transport adds, which the HTTP transport sends as
	// response HEADERS and so never had to classify. cc-core's scrubber drops
	// the metadata and timing frames whole, so those never committed anything;
	// codex.rate_limits it rewrites and forwards, and forwarding is what
	// commits.
	//
	// That one frame closed the withhold window on every WebSocket turn before
	// response.created had even arrived. It is why the pre-output failover
	// fired zero times across a whole night of them, and why ~18% of turns then
	// ended as a visible truncation at the stall budget instead of moving to
	// another credential in silence — the same ~16% the HTTP path had always
	// rescued, because there the shed frame landed while the preamble was still
	// being held.
	//
	// None of the three carries model output. codex.rate_limits reaches the
	// client as a fixed throttled/not-throttled summary with the account's
	// quota numbers already stripped, so holding it back costs nothing and
	// replaying it after a failover costs nothing either.
	"codex.rate_limits":             true,
	"codex.response.metadata":       true,
	"responsesapi.websocket_timing": true,
}

// codexPreOutputWithholdCap bounds how long the pre-output window may stay
// withheld. Buffering the opening events costs the client nothing except
// silence, and silence is exactly what upstream's keepalive was there to
// prevent — so holding those heartbeats back trades a visible reconnect for a
// quiet socket, and that trade has to be finite.
//
// Four minutes sits under the five-minute idle deadline Codex clients use on a
// Responses stream (CLIProxyAPI pins its socket read deadline there), while
// still covering the whole observed shed distribution: the slowest shed in a
// 24h sample of 1861 landed well inside it. Past the cap the withhold is
// abandoned, the buffer is flushed in upstream's original order, and the turn
// behaves exactly as it did before any of this existed.
const codexPreOutputWithholdCap = 4 * time.Minute

// codexStalledShedLabel names a turn the backend accepted, heartbeated, and
// never scheduled. It reads as a shed in the logs and the attempt archive
// because that is what it is — the WebSocket transport's spelling of the error
// frame the HTTP transport sends.
const codexStalledShedLabel = "upstream parked the turn: no output within the stall budget"

// codexShedPreviewBytes is how much of a shed frame reaches the journal. The
// Responses failure envelope spends its first ~200 bytes on id/object/status
// bookkeeping, so the old cap cut the line exactly before `error` — the only
// field that says WHY upstream refused. During a shed storm that is the
// difference between "2273 sheds an hour" and knowing what to do about them.
const codexShedPreviewBytes = 600

// codexStaleSocketRetryKey marks an attempt that died on a pooled WebSocket the
// backend had already closed. The forward loop reads it to keep the credential
// eligible for one more round instead of excluding it.
const codexStaleSocketRetryKey = "codex_stale_socket_retry"

// codexCommitPreview keeps both ends of the committing write. The head says
// what was buffered, the TAIL says what released it — and the tail is the half
// that names the bug, which a plain head-truncation hides.
func codexCommitPreview(emit []byte) string {
	const half = 200
	if len(emit) <= 2*half {
		return string(emit)
	}
	return string(emit[:half]) + "…<cut>…" + string(emit[len(emit)-half:])
}

// codexCommittedLineSuffix names the bytes that committed a response when the
// committing frame declared no event type, so an empty `committed by ""` can be
// told apart from the other things that produce the same empty string.
func codexCommittedLineSuffix(line string) string {
	if line == "" {
		return ""
	}
	return " via=" + strconv.Quote(line)
}

// codexCapacityShedKey marks an attempt withheld because upstream refused the
// turn for capacity. The forward loop reads it to retry the SAME credential a
// bounded number of times before rotating; see the shed branch for why.
const codexCapacityShedKey = "codex_capacity_shed"

// codexCapacityShedCodes are the refusals that say "not right now" about the
// turn rather than anything about the account. Quota and rate codes are
// deliberately absent: those ARE about the account, and rotating is the right
// answer for them.
var codexCapacityShedCodes = map[string]bool{
	"server_is_overloaded": true,
	"slow_down":            true,
}

// codexStreamResult reports the outcome of a Codex backend SSE relay so the
// caller can choose between a transparent retry (nothing reached the client
// yet) and a logged give-up (bytes already committed downstream — from that
// point the exchange is uninterruptible).
type codexStreamResult struct {
	sawTerminal bool  // a response.{completed,failed,...} event was relayed
	wroteAny    bool  // at least one byte was committed to the client
	err         error // underlying read error when the stream broke early
	// shed: non-empty when a capacity/quota frame arrived BEFORE any output and
	// was withheld, leaving failover clean.
	shed string
	// demoted: a shed that arrived after output had started, so it could only
	// be demoted on the way out rather than withheld.
	demoted shedSignal
	// committedLine is the first ~120 bytes actually written downstream, kept
	// only when the committing frame declared no event type. `committedBy` alone
	// has now sent three separate investigations down the wrong path: it reports
	// an empty string for a typeless frame, an orphaned event terminator and an
	// unparsed data line alike, and each of those is a different bug with a
	// different fix. The bytes say which.
	committedLine string
	// committedBy is the event type of the frame that first reached the client
	// and so closed the withhold window. It exists because "which frame
	// committed the response" decided, twice in one morning, whether a stalled
	// turn could be rescued or only truncated — and both times it had to be
	// inferred from a timestamp column, once wrongly. The transport keeps
	// adding frames the HTTP path never had; this names them as they appear.
	committedBy string
	// fatalCode is the vendor error code of an error frame the classifier
	// called fatal and therefore forwarded — which is what commits the
	// response. Recorded so a code that is really a transient account fault can
	// be told apart from a genuine rejection without another day of guessing.
	fatalCode string
	// shedCode is the vendor error code of the withheld shed frame. It decides
	// whether the retry rotates credentials or stays put: a capacity refusal is
	// about how expensive this turn is to schedule, and the credential that
	// just refused it is the only one holding its prompt cache.
	shedCode string
	// upstreamModel is what the terminal event said the provider actually ran.
	// It is not always what was asked for: a provider under load can serve
	// something lighter and say so only here.
	upstreamModel string
	// firstOutputAt is when upstream produced its first content-bearing event —
	// not the response headers, and not the response.created/in_progress
	// preamble it opens with, which arrive immediately and say nothing about
	// when the model started working. Zero when the turn produced no output at
	// all (a withheld shed, a broken stream).
	firstOutputAt time.Time
}

// streamSSECodexBackend is the Codex backend SSE passthrough. The format
// differs from OpenAI's API-key response: events carry JSON payloads
// structured as `response.completed` / `response.output_item.done` etc.
// Usage arrives inside the `response.completed` event as
// `response.usage.{input_tokens, output_tokens, input_tokens_details.cached_tokens}`.
//
// Beyond verbatim passthrough it:
//
//   - emits an SSE keepalive comment during silent gaps so intermediaries
//     (Caddy/Cloudflare/the client's own idle timeout) don't cut the long-lived
//     stream while the model is mid-think;
//   - tracks whether the terminal event arrived, so a truncated upstream is
//     logged instead of being passed off as a clean end-of-stream (the root
//     cause of the "stream disconnected before completion" reports);
//   - withholds a pre-output capacity/quota shed so the caller can fail over to
//     another credential invisibly.
//
// That last one is the important one. Upstream sheds load as an error frame
// inside an otherwise-200 stream, and in production ~16% of Codex turns come
// back that way — but the rate is wildly uneven across accounts (one account
// served 135 turns with zero sheds in the same window another shed 27% of 97).
// So a shed is an account-and-moment property, not the model-wide one
// cc-core/codexerr's doc assumes, and moving the turn to another credential
// genuinely rescues it. Demotion — which only downgrades the CLI's verdict from
// session-ending to retryable — remains the fallback for a shed that lands
// after output has already started.
//
// gin's ResponseWriter is not goroutine-safe, so the keepalive goroutine and the
// read loop share one mutex around every Write/Flush.
// codexStallDisarmer retires the WebSocket stall budget once a relay has
// committed the response.
//
// The budget exists to convert a turn the backend parked into a failover, and
// the failover is gone the moment the first byte reaches the client. Left armed
// it can only cut a slow turn into a truncated one — the single largest source
// of truncated Codex streams the day the budget shipped. The HTTP transport has
// no such budget and returns a no-op.
// codexErrorFrameCode reads the vendor error code out of an error frame, in
// both the shapes the Codex backend uses. It exists for the log line below:
// an error frame the classifier calls fatal is forwarded verbatim, which
// commits the response and forecloses failover, so when the socket then dies
// the turn reaches the client as a truncation. Sixty of those landed in one
// seven-hour window under a single label, with nothing recorded to say which
// code produced them or whether it should have been retryable.
func codexErrorFrameCode(payload []byte) string {
	var f struct {
		Error *struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
		Response *struct {
			Error *struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &f) != nil {
		return ""
	}
	e := f.Error
	if e == nil && f.Response != nil {
		e = f.Response.Error
	}
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return e.Code
	}
	return e.Type
}

// codexFatalCodeSuffix annotates the truncation log with the vendor code that
// committed the response, when one did. Empty for every other commit so the
// line keeps its shape.
func codexFatalCodeSuffix(code string) string {
	if code == "" {
		return ""
	}
	return " code=" + code
}

func codexStallDisarmer(r any) func() {
	if d, ok := r.(interface{ DisarmStall() }); ok {
		return d.DisarmStall
	}
	return func() {}
}

func streamSSECodexBackend(c *gin.Context, resp *http.Response, counts *usage.Counts, commit func()) codexStreamResult {
	flusher, _ := c.Writer.(http.Flusher)
	reader := newLineReader(resp.Body)
	out := codexStreamResult{}
	// Start of the withhold window — see codexPreOutputWithholdCap.
	withholdStart := time.Now()

	// shedding latches once a capacity/quota error frame is seen before any
	// output has reached the client. From that point the rest of the stream is
	// withheld — including the response.failed that follows — so Relay ends with
	// SawTerminal=false and WroteAny=false and the caller's pre-output failover
	// fires. Without the latch the error frame itself counts as the first output
	// and permanently forecloses failover.
	shedding := false
	sentAny := false // whether we've handed Relay any bytes yet
	// An SSE event is "event: X\ndata: {…}\n\n", and the verdict lives in the
	// data line — but the event line arrives first. Releasing it immediately
	// would commit the response before we know whether the frame is one we mean
	// to withhold, so an event line is held until its data line is classified
	// and then emitted together with it.
	var held []byte
	// lastPayloadType is the type of the most recently classified data line,
	// carried so the commit below can name the frame that closed the window.
	lastPayloadType := ""
	// preamble buffers the content-free events (response.created,
	// response.in_progress, and the keepalives upstream sends while a turn
	// waits for capacity) until the stream reveals what it is.
	//
	// Without this the withhold above never fires in practice: upstream always
	// opens with response.created, forwarding it commits the response, and the
	// shed frame that arrives a second later is then stuck on the demote path.
	// Production bore that out — after shipping the withhold, it triggered zero
	// times while 52 sheds in the same window took the demote branch.
	//
	// The buffer is released the moment any other event arrives, so it holds
	// only for the gap between response.created and the first real event. The
	// cost is that Relay's keepalive does not start until the first byte, so
	// that gap runs unprotected; it is bounded by upstream sending literally
	// anything else, and by codexPreOutputWithholdCap.
	var preamble []byte

	next := func() (emit []byte, terminal bool, err error) {
		for {
			line, rerr := reader.readLine()
			// A parked turn ends as a read error, not as a frame, so it has to
			// latch the withhold here rather than in the classifier below.
			// Without the latch the buffered preamble is released on the way
			// out — the "release it rather than swallow the response" rule two
			// screens down, which is right for an EOF and exactly wrong here.
			// Those bytes are nothing but keepalives, and flushing them commits
			// the response and forecloses the failover this whole path exists
			// to keep open.
			if rerr != nil && !sentAny && !shedding && errors.Is(rerr, codexws.ErrStalled) {
				shedding = true
				out.shed = codexStalledShedLabel
				held = nil
				preamble = nil
			}
			if len(line) > 0 {
				trim := bytes.TrimRight(line, "\r\n")
				switch {
				case bytes.HasPrefix(trim, []byte("event:")):
					if !shedding {
						held = append(held[:0], line...)
					}
					line = nil

				case bytes.HasPrefix(trim, []byte("data:")):
					payload := bytes.TrimSpace(trim[5:])
					if len(payload) > 0 && payload[0] == '{' {
						lastPayloadType = codexEventType(payload)
						mergeCodexUsage(counts, extractCodexBackendUsageFromJSON(payload))
						if m := extractCodexUpstreamModel(payload); m != "" {
							out.upstreamModel = m
						}

						if codexerr.Classify(payload) == codexerr.ClassRetryable {
							if !sentAny {
								// Failover is still possible — withhold this
								// frame and everything after it, including the
								// held event line, any buffered preamble and
								// the response.failed that follows, so nothing
								// commits the response.
								shedding = true
								out.shed = truncate(payload, codexShedPreviewBytes)
								out.shedCode = codexErrorFrameCode(payload)
								held = nil
								preamble = nil
								line = nil
							} else if demoted, ok := codexerr.DemoteCapacityCode(payload); ok {
								// Output already started, so the client must be
								// told something. Demote the two session-ending
								// capacity codes to one the CLI retries; the
								// message is left untouched.
								out.demoted.shed = true
								out.demoted.capacity = true
								tail := line[len(trim):]
								rebuilt := make([]byte, 0, len("data: ")+len(demoted)+len(tail))
								rebuilt = append(rebuilt, []byte("data: ")...)
								rebuilt = append(rebuilt, demoted...)
								rebuilt = append(rebuilt, tail...)
								line = rebuilt
							} else {
								// Quota/rate after output started: forwarded
								// untouched (the CLI handles those
								// non-terminally and reads its retry delay off
								// the original code), but still worth naming.
								out.demoted.shed = true
							}
						}
						// ClassFatal frames are forwarded verbatim: retrying
						// them elsewhere would fail identically, and the client
						// needs the real reason.
						//
						// Name the code when one of them is what commits the
						// response, so a fatal that is really a transient
						// account fault — the pattern behind the 1006 closures
						// that follow these frames — can be told apart from a
						// genuine rejection instead of being counted as one.
						if !sentAny && codexerr.Classify(payload) == codexerr.ClassFatal {
							out.fatalCode = codexErrorFrameCode(payload)
						}

						if codexTerminalEvent(payload) && !shedding {
							terminal = true
						}

						// Buffer a content-free opener instead of emitting it,
						// so it does not count as output and foreclose failover.
						// Anything else falls through and flushes the buffer.
						if !sentAny && !shedding && codexPreambleEvent(payload) &&
							time.Since(withholdStart) < codexPreOutputWithholdCap {
							if scrubbed, keep := downstream.ScrubCodexSSELine(line); keep {
								preamble = append(preamble, held...)
								preamble = append(preamble, scrubbed...)
							}
							held = nil
							line = nil
							continue
						}

						// Withhold the pool's state, LAST — usage extraction,
						// error classification and terminal detection above all
						// read `payload` (what upstream said). This is the SSE
						// twin of the WS frame scrub in codex_ws.go.
						//
						// A dropped data line takes its held `event:` line with
						// it: emitting an event with no data is malformed SSE.
						if scrubbed, keep := downstream.ScrubCodexSSELine(line); !keep {
							line = nil
							held = nil
						} else {
							line = scrubbed
						}
					}
				}
			}

			if shedding {
				line = nil
				held = nil
				terminal = false
			}

			// The blank line that closes an SSE event belongs to the event before it,
			// and while nothing has committed yet that event was either buffered into
			// the preamble or dropped outright by the scrubber. Either way its
			// terminator has to follow it rather than slip out alone: left to fall
			// through to the emit switch it becomes the first byte written, which both
			// flushes the buffer early and marks the stream as having produced output —
			// exactly what forecloses the failover this buffer exists to preserve.
			//
			// This used to also require a non-empty preamble, which held for a buffered
			// frame and failed for a dropped one: cc-core drops codex.response.metadata
			// whole, so both of its lines vanished and the orphaned blank line committed
			// the response on its own. It was invisible while codex.rate_limits was
			// still committing first, and took over the moment that was fixed — the
			// truncation log named the culprit as an empty event type, which is what an
			// orphaned terminator looks like.
			if !sentAny && !shedding && len(line) > 0 && len(bytes.TrimSpace(line)) == 0 {
				preamble = append(preamble, line...)
				continue
			}

			// Emit the held event line together with the line that resolved it.
			switch {
			case len(line) > 0 && len(held) > 0:
				emit = append(append(make([]byte, 0, len(held)+len(line)), held...), line...)
				held = nil
			case len(line) > 0:
				emit = line
			case rerr != nil && len(held) > 0 && !shedding && sentAny:
				// Stream ended with an unresolved event line. `held` only ever
				// carries an `event:` line waiting for its `data:`, so this is
				// half an SSE event — malformed on its own, and worth passing on
				// only to a client already mid-stream, where dropping bytes
				// silently is the worse of two bad options.
				//
				// Before anything has been committed it is not passed on at
				// all. Releasing it there was the same mistake the preamble
				// flush made one door over: it committed the response, which
				// foreclosed the failover, and what the client got was a bare
				// `event:` line and a dead stream. 37 of 37 turns truncated in
				// the fifteen minutes after that first fix landed came through
				// HERE — all zero-output, across three credentials and three
				// clients, every one logged as `committed by ""` because no
				// data payload had ever been seen to name.
				emit, held = held, nil
			case rerr != nil && len(held) > 0 && !shedding:
				held = nil
			}

			// Flush the buffered opener ahead of whatever released it, so the
			// client still receives the stream in upstream's original order.
			//
			// Only ever ahead of real content. A stream that ends with NOTHING
			// but content-free frames buffered has produced nothing a caller
			// can use, and releasing the buffer on the way out was the worst of
			// both worlds: the preamble committed the response, which foreclosed
			// the failover that would have fetched a real answer, and what the
			// client got was an opener followed by a dead stream. That is the
			// shape of "the connection keeps dropping" — 101 of one 25-minute
			// window's 373 gpt-5.6-sol turns, every one of them upstream closing
			// the socket with `close 1000 (normal)` and no terminal event, while
			// the pool still held eight healthy accounts that were never asked.
			//
			// Dropping it instead costs nothing: a turn that really finished
			// emits response.completed, which is not content-free, so a complete
			// response always has something in `emit` here.
			if len(preamble) > 0 && !shedding && len(emit) > 0 {
				emit = append(append(make([]byte, 0, len(preamble)+len(emit)), preamble...), emit...)
				preamble = nil
			}

			if len(emit) > 0 {
				if !sentAny {
					out.firstOutputAt = time.Now()
					out.committedBy = lastPayloadType
					if lastPayloadType == "" {
						out.committedLine = codexCommitPreview(emit)
					}
				}
				sentAny = true
			}
			if len(emit) > 0 || rerr != nil {
				return emit, terminal, rerr
			}
			// Nothing to emit yet (a held event line) — keep reading.
		}
	}

	r := ccstream.Relay(c.Writer, func() {
		if flusher != nil {
			flusher.Flush()
		}
	}, ccstream.RelayOptions{
		Commit:           commit,
		KeepaliveIdle:    10 * time.Second,
		KeepalivePayload: []byte(":\n\n"),
		Next:             next,
	})
	out.sawTerminal = r.SawTerminal
	out.wroteAny = r.WroteAny
	out.err = r.Err
	return out
}

// extractCodexBackendUsageFromJSON reads usage from the ChatGPT Codex
// backend's response/event JSON, covering both shapes:
//
//	{"response":{"usage":{...}}}        ← streaming "response.completed"
//	{"usage":{...}}                     ← non-stream compact wrapper
//
// Cached input tokens are split out into Counts.CacheReadTokens so they're
// billed at the discounted rate.
func extractCodexBackendUsageFromJSON(body []byte) usage.Counts {
	if len(body) == 0 {
		return usage.Counts{}
	}
	var wrap struct {
		Response struct {
			Usage *openaiUsage `json:"usage"`
		} `json:"response"`
		Usage *openaiUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return usage.Counts{}
	}
	u := wrap.Response.Usage
	if u == nil {
		u = wrap.Usage
	}
	if u == nil {
		return usage.Counts{}
	}
	return u.toCounts()
}

// isCodexCapacityError detects the upstream's "model is at capacity"
// rejection so the picker cools down this credential without giving up on
// the request. Strings come from CLIProxyAPI's codex_executor.go.
func isCodexCapacityError(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("selected model is at capacity")) ||
		bytes.Contains(lower, []byte("model is at capacity"))
}

// parseCodexResetAt extracts the reset timestamp from a usage_limit_reached
// error body. Supports both epoch-seconds and relative-seconds encodings:
//
//	{"error":{"type":"usage_limit_reached","resets_at":1716000000}}
//	{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}
func parseCodexResetAt(body []byte) time.Time {
	if len(body) == 0 {
		return time.Time{}
	}
	var wrap struct {
		Error struct {
			Type            string  `json:"type"`
			ResetsAt        int64   `json:"resets_at"`
			ResetsInSeconds float64 `json:"resets_in_seconds"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return time.Time{}
	}
	if wrap.Error.ResetsAt > 0 {
		return time.Unix(wrap.Error.ResetsAt, 0)
	}
	if wrap.Error.ResetsInSeconds > 0 {
		return time.Now().Add(time.Duration(wrap.Error.ResetsInSeconds) * time.Second)
	}
	return time.Time{}
}

// isCodexUsageLimitBody reports whether a 429 body is ChatGPT's
// usage_limit_reached — the account's window filled — as opposed to the
// per-request "Rate limit exceeded" throttle or a capacity rejection.
func isCodexUsageLimitBody(body []byte) bool {
	var wrap struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return false
	}
	return wrap.Error.Type == "usage_limit_reached"
}

// lineReader is a tiny buffered reader that preserves the original
// trailing newline so the passthrough writes the exact bytes the upstream
// sent (SSE is whitespace-sensitive).
type lineReader struct {
	err error // deferred until all bytes from the same Read are framed
	buf []byte
	pos int
	src io.Reader
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{src: r, buf: make([]byte, 0, 8192)} }

func (lr *lineReader) readLine() ([]byte, error) {
	for {
		if idx := bytes.IndexByte(lr.buf[lr.pos:], '\n'); idx >= 0 {
			line := lr.buf[lr.pos : lr.pos+idx+1]
			lr.pos += idx + 1
			if lr.pos >= len(lr.buf) {
				lr.buf = lr.buf[:0]
				lr.pos = 0
			}
			return line, nil
		}
		if lr.err != nil {
			rest := lr.buf[lr.pos:]
			lr.pos = len(lr.buf)
			return rest, lr.err
		}
		// Shift remaining unread bytes to the start before the next read
		// so we don't grow the buffer unbounded on a slow stream.
		if lr.pos > 0 {
			copy(lr.buf, lr.buf[lr.pos:])
			lr.buf = lr.buf[:len(lr.buf)-lr.pos]
			lr.pos = 0
		}
		chunk := make([]byte, 4096)
		n, err := lr.src.Read(chunk)
		if n > 0 {
			lr.buf = append(lr.buf, chunk[:n]...)
			// io.Reader may return final data and EOF together. Frame every
			// buffered line before exposing that error to the SSE consumer.
			lr.err = err
			continue
		}
		if err != nil {
			// Flush any tail bytes without a terminator on EOF.
			if lr.pos < len(lr.buf) {
				rest := lr.buf[lr.pos:]
				lr.pos = len(lr.buf)
				return rest, err
			}
			return nil, err
		}
	}
}
