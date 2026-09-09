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
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/wjsoj/cc-core/apicompat"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/codexerr"
	"github.com/wjsoj/cc-core/downstream"
	"github.com/wjsoj/cc-core/mimicry"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/requestlog"
	"github.com/wjsoj/cc-core/servicetier"
	ccstream "github.com/wjsoj/cc-core/stream"
	"github.com/wjsoj/cc-core/usage"
)

// Codex / OpenAI endpoint handlers. The request/retry/accounting machinery
// lives in forward() (proxy.go); this file supplies the provider-specific
// upstream call (doForwardCodex) plus the Codex-native route handlers.
//
// This file implements the API-key path — requests are forwarded to
// api.openai.com (or an overridden base URL) with the credential's bearer
// key swapped in. The OAuth path lives in doForwardCodexOAuth
// (codex_oauth_proxy.go) so the request-transformation complexity doesn't
// clutter the BYOK flow.

func (s *Server) handleCodexChatCompletions(c *gin.Context) {
	s.forward(c, auth.ProviderOpenAI, "/v1/chat/completions")
}

func (s *Server) handleCodexResponses(c *gin.Context) {
	s.forward(c, auth.ProviderOpenAI, "/v1/responses")
}

// handleCodexResponsesCompact forwards the Codex CLI's conversation-compaction
// request. Same /v1/responses body shape, different upstream path
// (/codex/responses/compact on the ChatGPT backend; /v1/responses/compact
// on API-key relays). Routed to the same forward() machinery — the path is
// translated at the upstream-call layer.
func (s *Server) handleCodexResponsesCompact(c *gin.Context) {
	s.forward(c, auth.ProviderOpenAI, "/v1/responses/compact")
}

// handleCodexModels returns the union of models exposed by the loaded
// OpenAI credentials: OAuth creds contribute their plan-tier catalog
// (see auth.CodexModelsForPlan) and API-key creds contribute the
// upstream's authoritative /v1/models listing. Returned shape matches
// OpenAI's: {"object":"list","data":[{id, object, owned_by}, ...]}.
func (s *Server) handleCodexModels(c *gin.Context) {
	// A Codex client asks the same URL but wants a different document. It is
	// identified by the client_version query parameter, which no OpenAI-API
	// client sends, and it wants the ChatGPT backend's manifest shape. Handing
	// it the OpenAI list below does not fail loudly — it cannot parse it, falls
	// back to the models compiled into its own build, and never sees anything
	// this gateway added after that build shipped.
	if clientVersion, ok := auth.CodexModelsRequest(c.Request.URL.Query()); ok {
		s.serveCodexModelsManifest(c, clientVersion)
		return
	}
	seen := map[string]bool{}
	var data []gin.H

	// OAuth: synthesize from plan_type claims so subscribers see exactly
	// the models their tier is entitled to (matches Codex CLI behavior).
	var apiKeyCred *auth.Auth
	for _, st := range s.pool.Status() {
		if auth.NormalizeProvider(st.Auth.Provider) != auth.ProviderOpenAI {
			continue
		}
		if st.Auth.Disabled {
			continue
		}
		live := s.pool.FindByID(st.Auth.ID)
		if live == nil {
			continue
		}
		if st.Auth.Kind == auth.KindOAuth {
			_, plan := live.CodexIdentity()
			for _, m := range auth.CodexModelsForPlan(plan) {
				if seen[m] {
					continue
				}
				seen[m] = true
				data = append(data, gin.H{"id": m, "object": "model", "owned_by": "openai"})
			}
			continue
		}
		if apiKeyCred == nil {
			apiKeyCred = live
		}
	}

	// API-key: transparent forward to upstream so BYOK users see whatever
	// their key is entitled to. Merge into `seen` so a model shared across
	// credentials isn't listed twice.
	if apiKeyCred != nil {
		if upstream, err := s.fetchCodexAPIKeyModels(c.Request.Context(), apiKeyCred); err == nil {
			for _, m := range upstream {
				if seen[m.id] {
					continue
				}
				seen[m.id] = true
				data = append(data, gin.H{"id": m.id, "object": "model", "owned_by": m.ownedBy})
			}
		} else {
			log.Warnf("codex: /v1/models upstream probe via %s failed: %v", apiKeyCred.ID, err)
		}
	}

	if data == nil {
		data = []gin.H{}
	}
	c.JSON(200, gin.H{"object": "list", "data": data})
}

// serveCodexModelsManifest answers a Codex client's model-picker refresh.
//
// The manifest is proxied from upstream with an OAuth credential rather than
// assembled here, so a model the ChatGPT backend adds tomorrow shows up without
// a release. That is sub2api's design; CLIProxyAPI instead ships a hand-edited
// JSON catalog, which is why adding one model there is a 260-line diff.
//
// The fallback matters for a deployment with no OAuth credential to borrow —
// an API-key-only pool still has to answer this route with the right SHAPE, or
// its users hit the same silent picker fallback.
func (s *Server) serveCodexModelsManifest(c *gin.Context, clientVersion string) {
	if creds := s.codexManifestCredentials(); len(creds) > 0 {
		// Fetch with OUR pinned client version, not the caller's, and cache
		// under one key for everyone.
		//
		// Upstream filters the catalog by the client_version in the query — it
		// returned 9 models for 0.153.4, 8 for 0.147.0 and 3 for 0.120.0 — so
		// forwarding the caller's version reintroduces the vendor's rollout
		// gate at the source, where no amount of not-filtering on our side can
		// undo it. gpt-6-astra vanished again for every client below 0.153.0
		// for exactly this reason, one deploy after it was fixed.
		//
		// What the caller asked for still shapes the ANSWER — see
		// FilterCodexManifest, which trims reasoning levels an older client
		// cannot render — but it no longer shapes the question.
		fetchVersion := mimicry.DefaultCodexProfile().ModelsClientVersion
		body, err := s.codexManifests.Get(fetchVersion, func() ([]byte, error) {
			// Try credentials in turn rather than trusting one. They do not
			// share an egress: each carries its own SOCKS5 proxy, and one of
			// those returning "general SOCKS server failure" for chatgpt.com
			// while every other credential serves traffic normally is exactly
			// what pinned this endpoint to the synthesized fallback in
			// production. The request path has always failed over; this had
			// not.
			var lastErr error
			for _, cred := range creds {
				body, err := auth.FetchCodexModelsManifest(c.Request.Context(), cred, fetchVersion, s.cfg.UseUTLS)
				if err == nil {
					return body, nil
				}
				lastErr = err
				log.Warnf("codex: models manifest via %s failed, trying the next credential: %v", cred.ID, err)
			}
			return nil, lastErr
		})
		if err != nil {
			// A stale body is still returned alongside the error, so this is a
			// log line and not a failure whenever anything was ever cached.
			log.Warnf("codex: models manifest refresh failed on all %d credentials: %v", len(creds), err)
		}
		if len(body) > 0 {
			c.Data(200, "application/json; charset=utf-8", auth.FilterCodexManifest(body, clientVersion))
			return
		}
	}

	// No OAuth credential, or upstream has never answered: synthesize from the
	// catalog the plain listing uses, so both routes agree about what exists.
	models := auth.CodexModelsForPlan("")
	c.Data(200, "application/json; charset=utf-8", auth.SynthesizeCodexModelsManifest(models, clientVersion))
}

// codexManifestCredentials lists the OpenAI OAuth credentials worth borrowing
// for a manifest fetch, healthy ones first.
//
// It returns a LIST rather than a pick, because the two ways this went wrong in
// production were both "the one we chose cannot do it":
//
//   - the first credential in pool order had a refresh token upstream had
//     invalidated, so every picker refresh re-attempted a doomed refresh;
//   - the first HEALTHY credential's SOCKS5 proxy answered "general SOCKS
//     server failure" for chatgpt.com while ten other credentials served
//     traffic normally, pinning the endpoint to the synthesized fallback.
//
// Credentials do not share an egress, so a failure is a property of the
// credential, not of the destination. Hard-failed ones are skipped outright;
// unhealthy-but-not-dead ones go last, since a long-shot fetch still beats
// having none.
//
// Capped, because this runs on a request path and the point is to survive one
// or two bad egresses, not to sweep the whole pool.
func (s *Server) codexManifestCredentials() []*auth.Auth {
	const maxCandidates = 4
	var healthy, degraded []*auth.Auth
	for _, st := range s.pool.Status() {
		if auth.NormalizeProvider(st.Auth.Provider) != auth.ProviderOpenAI {
			continue
		}
		if st.Auth.Disabled || st.Auth.Kind != auth.KindOAuth {
			continue
		}
		live := s.pool.FindByID(st.Auth.ID)
		if live == nil || live.IsHardFailed() {
			continue
		}
		if live.IsHealthy() {
			healthy = append(healthy, live)
		} else {
			degraded = append(degraded, live)
		}
	}
	// A fresh slice rather than appending onto healthy: the two are separate
	// buckets and reusing one's backing array to hold both is how a caller
	// later reading healthy gets degraded entries it never asked for.
	out := make([]*auth.Auth, 0, len(healthy)+len(degraded))
	out = append(out, healthy...)
	out = append(out, degraded...)
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}

type codexUpstreamModel struct{ id, ownedBy string }

func (s *Server) fetchCodexAPIKeyModels(ctx context.Context, a *auth.Auth) ([]codexUpstreamModel, error) {
	snap := a.Snapshot()
	baseURL := strings.TrimRight(snap.BaseURL, "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(s.cfg.OpenAIBaseURL, "/")
	}
	// Shared join rule (see mimicry.JoinCodexAPIKeyUpstreamURL): a bare-origin
	// relay BaseURL keeps /v1 (new-api/one-api serve /v1/models); a BaseURL
	// that already carries a path is authoritative.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mimicry.JoinCodexAPIKeyUpstreamURL(baseURL, "/v1/models"), nil)
	if err != nil {
		return nil, err
	}
	access, _ := a.Credentials()
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	client := auth.ClientFor(snap.ProxyURL, s.cfg.UseUTLS)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(truncate(body, 200))
	}
	var wrap struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, err
	}
	out := make([]codexUpstreamModel, 0, len(wrap.Data))
	for _, m := range wrap.Data {
		out = append(out, codexUpstreamModel{id: m.ID, ownedBy: m.OwnedBy})
	}
	return out, nil
}

// doForwardCodex performs one upstream attempt against an OpenAI-style
// provider credential. Contract matches doForward (proxy.go):
//
//	retry=true  → caller should exclude this credential and retry
//	done=true   → response was delivered (success or non-retryable error)
//
// Only API-key credentials are handled here; OAuth credentials are
// delegated to doForwardCodexOAuth (codex_oauth_proxy.go), a full
// implementation that forwards to the ChatGPT Codex backend.
func (s *Server) doForwardCodex(c *gin.Context, a *auth.Auth, path string, body []byte, stream bool, model, clientToken, clientName, slotID string, start time.Time, attempts int) (retry, done bool) {
	// Validate before map-based sanitizers or model rewrites can collapse
	// duplicate keys and make the outbound tier ambiguous.
	validatedBody, _, validationErr := servicetier.NormalizeRequest(body)
	if validationErr != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": validationErr.Error()}})
		return false, true
	}
	body = validatedBody
	if a.Kind == auth.KindOAuth {
		return s.doForwardCodexOAuth(c, a, path, body, stream, model, clientToken, clientName, slotID, start, attempts)
	}

	// API-key passthrough. We do not inject any Codex-CLI mimicry, do not
	// use uTLS, or apply the OAuth compact whitelist. Service-tier aliases
	// are normalized for forwarding/billing, stream usage is requested, and
	// per-credential model rewrites preserve relay vendor compatibility.
	//
	// Health tracking shares classifyUpstreamStatus with the Anthropic
	// API-key path (upstream_health.go). Retryable faults — 429, 5xx,
	// transport errors, and a rejected key — are NOT relayed: we roll back to
	// the forward loop (retry=true) so it excludes this credential and tries
	// the next, and report the fault so cc-core's breaker pauses the relay and
	// stops it being picked. This is what keeps one dead relay (e.g. a
	// reseller returning a 502 page) from surfacing 502s to every client when
	// healthy Codex credentials are available. Client-side faults (400, 404,
	// 422 …) are forwarded verbatim and leave health alone. See
	// reportCodexAPIKeyFault.
	snap := a.Snapshot()
	baseURL := strings.TrimRight(snap.BaseURL, "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(s.cfg.OpenAIBaseURL, "/")
	}
	// Codex API-key providers are reached over the Responses protocol by
	// default: an inbound /v1/chat/completions is translated into a Responses
	// body and sent to the provider's /v1/responses, then rendered back as
	// chat.completion{,.chunk} on the way down. These relays resell Codex
	// capacity, which is Responses-native — several host /v1/responses only, so
	// forwarding chat/completions verbatim reached an endpoint that either
	// 404s or falls back to a different (non-Codex) upstream pool.
	//
	// The bridge is best-effort: a body apicompat declines to translate falls
	// back to verbatim chat/completions forwarding, which is what a plain
	// OpenAI-compatible gateway expects. Same translator as the OAuth path
	// (codex_oauth_proxy.go), so both credential kinds render identically.
	isChat := path == "/v1/chat/completions"
	bridged := false
	sourceBody := body
	upstreamPath := path
	if isChat {
		if converted, cerr := apicompat.ChatCompletionsToResponses(body); cerr == nil {
			sourceBody = converted
			upstreamPath = "/v1/responses"
			bridged = true
		} else {
			log.Infof("codex proxy(apikey): chat/completions bridge declined body via %s: %v — forwarding verbatim", a.ID, cerr)
		}
	}
	// Join BaseURL with the (possibly bridged) endpoint via the shared rule
	// (mimicry.JoinCodexAPIKeyUpstreamURL):
	//   BaseURL=https://api.openai.com/v1 + /v1/responses → .../v1/responses ✓
	//   BaseURL=https://relay.example     + /v1/responses → .../v1/responses ✓ (bare origin keeps /v1)
	//   BaseURL=https://gateway.io/codex  + /v1/responses → .../codex/responses ✓
	// A bare-origin relay (new-api/one-api) serves under /v1, so we no longer
	// strip it into a /responses request that hit the gateway HTML homepage and
	// surfaced as "stream disconnected before completion".
	upURL := mimicry.JoinCodexAPIKeyUpstreamURL(baseURL, upstreamPath)

	upstreamBody := sourceBody
	rewriteClientModel := ""
	// stream_options.include_usage is a Chat Completions field; Responses
	// reports usage on response.completed unconditionally. Injecting it into a
	// bridged body would get rejected as an unknown parameter, so it stays on
	// the verbatim path only. A bridged body instead carries the client's
	// stream intent explicitly, since the translator does not set it.
	if bridged {
		if rewritten, err := setJSONBool(upstreamBody, "stream", stream); err == nil {
			upstreamBody = rewritten
		} else {
			log.Warnf("codex proxy(apikey): stream flag injection failed on bridged body via %s: %v", a.ID, err)
		}
	} else if stream {
		if rewritten, err := usage.EnsureOpenAIStreamUsage(upstreamBody); err == nil {
			upstreamBody = rewritten
		} else {
			log.Warnf("codex proxy(apikey): stream usage injection skipped for non-JSON body via %s: %v", a.ID, err)
		}
	}
	if upstreamModel, ok := a.ResolveUpstreamModel(model); ok && upstreamModel != model && upstreamModel != "" {
		if rewritten, err := rewriteModelField(upstreamBody, upstreamModel); err == nil {
			upstreamBody = rewritten
			rewriteClientModel = model
		} else {
			log.Warnf("codex proxy(apikey): model rewrite (%s -> %s) failed via %s: %v", model, upstreamModel, a.ID, err)
		}
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
	upReq.Header.Set("Authorization", "Bearer "+accessToken)

	client := auth.ClientFor(snap.ProxyURL, false)
	// Per-attempt upstream clock; see the same anchor in doForwardCodexOAuth.
	dispatchAt := time.Now()
	resp, err := client.Do(upReq)
	if err != nil {
		if isClientDisconnect(ctx, err) {
			log.Infof("codex proxy(apikey): client canceled via %s: %v", a.ID, err)
			s.emitLog(requestlog.Record{
				Client: clientName, ClientToken: maskClientToken(clientToken),
				Provider: auth.ProviderOpenAI, AuthID: a.ID, AuthLabel: a.Label, AuthKind: "apikey",
				Model: model, Stream: stream, Path: path, Status: 499,
				DurationMs: time.Since(start).Milliseconds(), Attempts: attempts, Error: "client canceled",
			})
			return false, true
		}
		// Transport/gateway failure — treat like a retryable 5xx: cool the
		// credential down and roll back to the loop to try the next one
		// instead of handing the client a bare 502.
		log.Warnf("codex proxy(apikey): upstream transport error via %s: %v — rotating to next credential", a.ID, err)
		s.reportCodexAPIKeyFault(a, http.StatusBadGateway, time.Time{})
		s.emitLog(requestlog.Record{
			Client: clientName, ClientToken: maskClientToken(clientToken),
			Provider: auth.ProviderOpenAI, AuthID: a.ID, AuthLabel: a.Label, AuthKind: "apikey",
			Model: model, Stream: stream, Path: path, Status: 502,
			DurationMs: time.Since(start).Milliseconds(), Attempts: attempts, Error: err.Error(),
		})
		return true, false
	}

	// Retryable fault (throttle / overload / gateway down / this key rejected):
	// don't relay it. Read+discard the body, report the fault so the breaker
	// can pause the relay, and roll back to the loop to try the next
	// credential. Nothing has been written to the client yet, so the retry is
	// transparent.
	//
	// classifyUpstreamStatus is shared with the Anthropic API-key path, so the
	// two providers no longer disagree about which statuses are the client's
	// fault. It also widens the retry set here: 401/402/403 used to be
	// forwarded to the caller, but on a relay another key may well work, and a
	// rejected key needs to leave rotation rather than be re-presented on every
	// request.
	if classifyUpstreamStatus(resp.StatusCode).retryable() {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		snippet := truncate(errBody, 500)
		log.Warnf("codex proxy(apikey): %s returned %d — rotating to next credential. body=%s", a.ID, resp.StatusCode, snippet)
		s.reportCodexAPIKeyFault(a, resp.StatusCode, parseRetryAfter(resp.Header))
		s.emitLog(requestlog.Record{
			Client: clientName, ClientToken: maskClientToken(clientToken),
			Provider: auth.ProviderOpenAI, AuthID: a.ID, AuthLabel: a.Label, AuthKind: "apikey",
			Model: model, Stream: stream, Path: path, Status: resp.StatusCode,
			DurationMs: time.Since(start).Milliseconds(), Attempts: attempts,
			Error: fmt.Sprintf("upstream %d: %s", resp.StatusCode, truncate(errBody, 200)),
		})
		return true, false
	}

	// Observe original bytes before protocol conversion or response scrubbing.
	tierObserver := servicetier.ObserveBody(resp.Body)
	resp.Body = tierObserver
	var priced pricing.CostResult
	var counts usage.Counts
	var errSnippet string
	// firstOutputAt is when this credential's upstream produced its first
	// frame, set by whichever relay served the turn.
	var firstOutputAt time.Time
	// streamTruncated records an upstream that stopped without a terminal
	// event, so the request log names it instead of showing a clean 200 for a
	// stream the client saw break.
	var streamTruncated bool
	// shedLabel records an upstream that refused the turn mid-stream, using the
	// same wording as the OAuth path so both aggregate together.
	var shedLabel string
	outcome := usage.StreamComplete
	// logStatus defaults to the upstream's. A mid-stream client hang-up
	// overrides it to 499 so a cancellation stops masquerading as a clean 200
	// in every "success rate" view.
	logStatus := resp.StatusCode
	switch {
	case resp.StatusCode >= 400:
		errBody, _ := io.ReadAll(resp.Body)
		errSnippet = truncate(errBody, 500)
		log.Warnf("codex proxy(apikey): %s returned %d — body=%s", a.ID, resp.StatusCode, errSnippet)
		copySafeRetryHeaders(c, resp.Header)
		writeAPIError(c, auth.ProviderOpenAI, publicUpstreamError(resp.StatusCode, errBody))
	default:
		// Decide SSE-vs-JSON from the client's stream flag + the actual bytes,
		// NOT the upstream Content-Type alone: relays (e.g. New-API gateways)
		// stream the /v1/responses SSE back as `text/plain`, which used to fall
		// through to the whole-body JSON parse and silently lose usage (billing
		// = $0). Mirrors the OAuth path and sub2api, which both dispatch on the
		// requested stream rather than the response header.
		br := bufio.NewReaderSize(resp.Body, 64*1024)
		switch {
		case stream && responseIsSSE(resp.Header, br):
			var clientGone bool
			if bridged {
				// Bridged stream: the upstream speaks Responses SSE and the
				// client asked for chat.completion.chunk. Usage is read off the
				// raw upstream events inside the translator, so billing matches
				// the native path byte for byte. `model` is the client-facing
				// name — the rendered frames must echo what was requested, not
				// the relay's model_map'd name, which is why rewriteClientModel
				// has no role here.
				//
				// Commits lazily, so a shed arriving before any output leaves
				// the response uncommitted and can be retried elsewhere.
				res := streamCodexAsChatCompletions(c, br, &counts, model, chatStreamWantsUsage(body), func() { writeSSEResponseHeaders(c, resp) })
				firstOutputAt = res.firstOutputAt
				if res.shed != "" {
					// Not a byte has reached the client, so this turn is still
					// fully recoverable on another credential. Without this the
					// translation would have rendered the shed as an empty but
					// successful-looking completion — see
					// streamCodexAsChatCompletions.
					_ = resp.Body.Close()
					log.Warnf("codex proxy(apikey): %s shed the bridged stream before any output: %s — rotating to next credential", a.ID, res.shed)
					s.reportCodexAPIKeyFault(a, http.StatusServiceUnavailable, time.Time{})
					return true, false
				}
				if res.demoted.shed {
					log.Warnf("codex proxy(apikey): %s shed the bridged stream after output started (capacity=%v)", a.ID, res.demoted.capacity)
					shedLabel = shedTurnLabel(res.demoted.capacity)
				}
				clientGone = !res.sawTerminal && isClientDisconnect(ctx, res.err)
				if !res.sawTerminal && !clientGone {
					log.Warnf("codex proxy(apikey): bridged SSE ended before terminal event via %s: %v", a.ID, res.err)
					streamTruncated = true
				}
			} else {
				writeSSEResponseHeaders(c, resp)
				sse := streamSSEOpenAI(c, br, &counts, rewriteClientModel)
				firstOutputAt = sse.firstOutputAt
				clientGone = sse.clientGone
				// An in-band capacity/quota frame is why a turn can end with
				// output but no usage. Naming it separates "the relay shed this
				// turn" from "the relay is broken", which the bare
				// missing-usage warning below could never distinguish.
				if sse.shed {
					log.Warnf("codex proxy(apikey): %s shed the turn mid-stream (capacity=%v); the client sees a retryable error, not a session-ending one",
						a.ID, sse.capacity)
					shedLabel = shedTurnLabel(sse.capacity)
				}
				// A stream that stops without a terminal event is what the
				// client reports as "stream disconnected before completion".
				// This path had no way to see it until now.
				if !sse.sawTerminal && !clientGone {
					log.Warnf("codex proxy(apikey): %s SSE ended before terminal event (truncated upstream) — client will report a disconnect", a.ID)
					streamTruncated = true
				}
			}
			outcome = usage.ClassifyStreamOutcome(counts, clientGone)
			switch outcome {
			case usage.StreamClientCanceled:
				// The user walked away. Bill whatever usage arrived before they
				// did (often nothing) and leave the credential untouched — this
				// path used to charge a full-prompt estimate AND cool a healthy
				// key on every Ctrl-C.
				logStatus = 499
				a.MarkClientCancel(usage.ClientCanceledError)
				log.Infof("codex proxy(apikey): client canceled mid-stream via %s (observed in=%d out=%d)",
					a.ID, counts.InputTokens, counts.OutputTokens)
			case usage.StreamUpstreamNoUsage:
				// The relay served a stream it cannot account for. Bill nothing
				// — there is no honest number to put on the invoice — and cool
				// the credential so traffic rotates to one that reports usage.
				log.Warnf("codex proxy(apikey): %s streamed success without usage; billing $0 and cooling credential", a.ID)
				s.reportCodexAPIKeyFault(a, http.StatusBadGateway, time.Time{})
			}
		case bridged:
			// Non-streaming bridged request. The relay may still answer with an
			// SSE stream (several ignore `stream:false` on /v1/responses), so
			// dispatch on the bytes exactly like the streaming branch does and
			// aggregate when needed — the alternative is handing the whole-body
			// JSON parse an SSE payload and failing a good turn closed.
			//
			// Every failure below rolls back to the forward loop (retry=true)
			// rather than answering the client. Not one byte has been written
			// yet, so trying the next credential is invisible to the caller —
			// whereas a 502 here hands over a hard error while healthy
			// credentials sit unused. This is the same reasoning the retryable
			// status branch above applies before the response is committed.
			var payload []byte
			if responseIsSSE(resp.Header, br) {
				agg, aggShed, aerr := aggregateCodexResponseStream(br, &counts)
				if aggShed != "" {
					// Same reasoning as the streaming branch: nothing has been
					// written downstream, so a shed is recoverable on another
					// credential rather than something the client must see.
					_ = resp.Body.Close()
					log.Warnf("codex proxy(apikey): %s shed the bridged non-stream request: %s — rotating to next credential", a.ID, aggShed)
					s.reportCodexAPIKeyFault(a, http.StatusServiceUnavailable, time.Time{})
					return true, false
				}
				if aerr != nil {
					_ = resp.Body.Close()
					// A client that hung up mid-aggregation reaches us as the
					// same read error an upstream fault would, but there is
					// nobody left to retry for — and the credential is
					// blameless, so its health must not be touched either.
					if isClientDisconnect(ctx, aerr) {
						a.MarkClientCancel(usage.ClientCanceledError)
						log.Infof("codex proxy(apikey): client canceled during bridged aggregation via %s", a.ID)
						s.emitLog(requestlog.Record{
							Client: clientName, ClientToken: maskClientToken(clientToken),
							Provider: auth.ProviderOpenAI, AuthID: a.ID, AuthLabel: a.Label, AuthKind: "apikey",
							Model: model, Stream: stream, Path: path, Status: 499,
							DurationMs: time.Since(start).Milliseconds(), Attempts: attempts, Error: "client canceled",
						})
						return false, true
					}
					log.Warnf("codex proxy(apikey): bridged aggregation via %s failed: %v — rotating to next credential", a.ID, aerr)
					s.reportCodexAPIKeyFault(a, http.StatusBadGateway, time.Time{})
					s.emitLog(requestlog.Record{
						Client: clientName, ClientToken: maskClientToken(clientToken),
						Provider: auth.ProviderOpenAI, AuthID: a.ID, AuthLabel: a.Label, AuthKind: "apikey",
						Model: model, Stream: stream, Path: path, Status: http.StatusBadGateway,
						DurationMs: time.Since(start).Milliseconds(), Attempts: attempts, Error: aerr.Error(),
					})
					return true, false
				}
				payload = agg
			} else {
				payload, _ = io.ReadAll(br)
				// Responses reports usage as input_tokens/output_tokens, not
				// the Chat Completions prompt_/completion_ pair, so this needs
				// the Codex extractor rather than extractOpenAIUsageFromJSON.
				mergeCodexUsage(&counts, extractCodexBackendUsageFromJSON(payload))
			}
			if usage.MissingUsage(counts) {
				_ = resp.Body.Close()
				log.Warnf("codex proxy(apikey): %s returned success without usage on bridged non-stream response — rotating to next credential", a.ID)
				s.reportCodexAPIKeyFault(a, http.StatusBadGateway, time.Time{})
				s.emitLog(requestlog.Record{
					Client: clientName, ClientToken: maskClientToken(clientToken),
					Provider: auth.ProviderOpenAI, AuthID: a.ID, AuthLabel: a.Label, AuthKind: "apikey",
					Model: model, Stream: stream, Path: path, Status: http.StatusBadGateway,
					DurationMs: time.Since(start).Milliseconds(), Attempts: attempts, Error: usage.MissingUsageError,
				})
				return true, false
			}
			converted, cerr := apicompat.ResponsesToChatCompletion(payload, model, time.Now().Unix())
			if cerr != nil {
				_ = resp.Body.Close()
				log.Warnf("codex proxy(apikey): chat/completions render via %s failed: %v — rotating to next credential", a.ID, cerr)
				s.emitLog(requestlog.Record{
					Client: clientName, ClientToken: maskClientToken(clientToken),
					Provider: auth.ProviderOpenAI, AuthID: a.ID, AuthLabel: a.Label, AuthKind: "apikey",
					Model: model, Stream: stream, Path: path, Status: http.StatusBadGateway,
					DurationMs: time.Since(start).Milliseconds(), Attempts: attempts, Error: cerr.Error(),
				})
				return true, false
			}
			// Content-Type is stamped after the allowlist copy: this branch may
			// have aggregated an SSE stream, so the upstream's
			// text/event-stream would misdescribe the single JSON object.
			downstream.CopyResponseHeaders(c.Writer.Header(), resp.Header, time.Now())
			c.Writer.Header().Set("Content-Type", "application/json")
			c.Writer.WriteHeader(http.StatusOK)
			_, _ = c.Writer.Write(converted)
		default:
			respBody, _ := io.ReadAll(br)
			if rewriteClientModel != "" {
				respBody = rewriteResponseModel(respBody, rewriteClientModel)
			}
			parsed := extractOpenAIUsageFromJSON(respBody)
			if usage.MissingUsage(parsed) {
				_ = resp.Body.Close()
				log.Warnf("codex proxy(apikey): %s returned success without usage on non-stream response; failing closed", a.ID)
				s.reportCodexAPIKeyFault(a, http.StatusBadGateway, time.Time{})
				writeAPIError(c, auth.ProviderOpenAI, APIError{Status: http.StatusBadGateway, Code: "service_response_error", Message: "The model service returned an incomplete response. Please try again."})
				s.emitLog(requestlog.Record{
					Client: clientName, ClientToken: maskClientToken(clientToken),
					Provider: auth.ProviderOpenAI, AuthID: a.ID, AuthLabel: a.Label, AuthKind: "apikey",
					Model: model, Stream: stream, Path: path, Status: http.StatusBadGateway,
					DurationMs: time.Since(start).Milliseconds(), Attempts: attempts, Error: usage.MissingUsageError,
				})
				return false, true
			}
			writeResponseHeaders(c, resp)
			_, _ = c.Writer.Write(respBody)
			mergeCodexUsage(&counts, parsed)
		}
	}
	_ = resp.Body.Close()

	// Only success is recorded here. Every retryable fault — including
	// 401/402/403, which used to be marked at this point — returns above,
	// having already been reported to the breaker by reportCodexAPIKeyFault.
	// What reaches here with a >=400 status is a client-side fault (400, 404,
	// 422 …), which by design leaves credential health untouched.
	//
	// A client cancellation is a success for the credential: it served bytes
	// until the caller stopped listening. Only StreamUpstreamNoUsage withholds
	// MarkSuccess, and it has already reported the fault above.
	if resp.StatusCode < 400 && !outcome.CredentialFault() {
		a.MarkSuccess()
	}

	// CostUSD = official upstream price, BilledUSD = wallet debit.
	var costUSD, billedUSD float64
	var userID int64
	var multiplier float64
	var billingErr string
	if resp.StatusCode < 400 {
		s.usage.Record(a.ID, a.Label, counts)
		// outcome.Billable gates on OBSERVED usage — never on an estimate. A
		// stream the relay failed to account for costs the customer $0; the
		// credential's breaker, not the customer's wallet, absorbs it.
		if outcome.Billable(counts) && clientToken != "" {
			priced = s.pricing.CostWithOptions(auth.ProviderOpenAI, billingModelFor(a, model), counts, pricing.CostOptions{ServiceTier: outboundTier, ResponseServiceTier: tierObserver.Observed(), CodexOAuth: false})
			costUSD = priced.CostUSD
			billedUSD = costUSD
			// Codex apikey path used to skip the SaaS Charge funnel entirely,
			// so wallets weren't deducted for Codex traffic. Route it through
			// the same path Anthropic uses now.
			if info, ok := saasInfoFrom(c); ok && s.saas != nil {
				billed, err := s.saas.Charge(chargeCtx(c), info, auth.ProviderOpenAI, model, counts, costUSD)
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
					multiplier = s.saas.MultiplierFor(info, auth.ProviderOpenAI)
				}
			}
			s.usage.RecordClient(clientToken, clientName, counts, billedUSD)
		}
	}
	errField := outcome.LogError()
	// Same labels the OAuth path uses, so the two aggregate together in the
	// request log instead of hiding half of these under a clean 200. A shed
	// outranks a truncation because it is the cause: a turn upstream refused
	// ends without a terminal event, so labelling it "truncated" would report
	// the symptom and lose why it happened.
	if errField == "" && shedLabel != "" {
		errField = shedLabel
	}
	if errField == "" && streamTruncated {
		errField = "stream truncated before terminal event"
	}
	if resp.StatusCode >= 400 {
		errField = fmt.Sprintf("upstream %d: %s", resp.StatusCode, truncate([]byte(errSnippet), 200))
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
		AuthKind:             "apikey",
		Model:                model,
		Input:                counts.InputTokens,
		Output:               counts.OutputTokens,
		CacheRead:            counts.CacheReadTokens,
		CacheCreate:          counts.CacheCreateTokens,
		UserID:               userID,
		Multiplier:           multiplier,
		CostUSD:              costUSD,
		BilledUSD:            billedUSD,
		Status:               logStatus,
		DurationMs:           time.Since(start).Milliseconds(),
		TTFBMs:               upstreamTTFBMillis(dispatchAt, firstOutputAt),
		Stream:               stream,
		Path:                 path,
		Attempts:             attempts,
		Error:                joinLogError(errField, billingErr),
	})
	return false, true
}

// reportCodexAPIKeyFault records an upstream failure on a Codex API-key relay
// so the pool stops selecting it while it is broken.
//
// This used to hand a 5xx to MarkFailure *plus* a fixed 45s MarkQuotaExceeded.
// The quota call was a workaround, not an intent: MarkFailure alone could not
// take an API key out of rotation (IsHealthy skipped the consecutive-failure
// heuristic for KindAPIKey), and the quota cooldown was the only lever
// IsHealthy honoured — at the cost of reporting an upstream 5xx to operators
// as "quota exceeded", and of a flat interval that both over-reacted to a
// one-off 502 and under-reacted to a permanently dead relay by re-probing it
// every 45s forever.
//
// cc-core's API-key circuit breaker removes that constraint, so the classes
// now map onto shared machinery:
//
//	429            → the pool's throttling path (Retry-After aware, growing
//	                 backoff) — kept, because a rate limit is the one case
//	                 where the upstream tells us how long to wait
//	401/402/403    → MarkHardFailure: definitive, pauses on the first strike
//	5xx/transport/ → MarkFailure: pauses after a few in a row, then backs off
//	contract        exponentially and probes itself back in
//
// None of these retire the channel; every pause expires on its own.
func (s *Server) reportCodexAPIKeyFault(a *auth.Auth, status int, resetAt time.Time) {
	if status == http.StatusTooManyRequests {
		s.pool.ReportUpstreamError(a, status, resetAt)
		return
	}
	if classifyUpstreamStatus(status) == faultCredential {
		a.MarkHardFailure(fmt.Sprintf("upstream %d", status))
		return
	}
	a.MarkFailure(fmt.Sprintf("upstream %d", status))
}

// shedTurnLabel renders the request-log tag for a turn upstream refused. Both
// credential kinds and all four relay paths route through it, so the wording
// can never drift between them — the tag is what makes sheds countable across
// the whole deployment.
func shedTurnLabel(capacity bool) string {
	if capacity {
		return "upstream shed the turn (capacity)"
	}
	return "upstream shed the turn (quota/rate)"
}

// shedPreOutputLabel tags the attempt row for a shed that arrived before any
// output and was therefore withheld from the client entirely.
//
// It is deliberately distinct from shedTurnLabel's two values: those name a
// turn the caller SAW go wrong, this one names a turn that was rescued on
// another credential and cost the user only latency. Sharing a label would
// make the two indistinguishable in the archive, and they answer different
// questions — one is a failure rate, the other is a retry tax.
const shedPreOutputLabel = "upstream shed the turn before any output"

// setJSONBool sets a top-level boolean field on a JSON object body, preserving
// every other field's raw bytes. Used to stamp the client's `stream` intent
// onto a bridged Responses body — apicompat's translator deliberately leaves
// the field unset, because the Codex backend path forces it in the sanitizer
// while a generic relay honors whatever the caller asks for.
func setJSONBool(body []byte, field string, v bool) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return body, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	obj[field] = b
	return json.Marshal(obj)
}

// responseIsSSE reports whether a <400 response should be parsed as an SSE
// stream. It trusts the Content-Type when it advertises `text/event-stream`,
// but also peeks the buffered body for a `data:`/`event:` line — some relays
// stream the Codex /v1/responses SSE back as `text/plain` (no event-stream
// header), and a header-only check would lose their usage. Peek does not
// consume, so the same reader is safe to hand to streamSSEOpenAI afterward.
func responseIsSSE(h http.Header, br *bufio.Reader) bool {
	if strings.Contains(h.Get("Content-Type"), "text/event-stream") {
		return true
	}
	return looksLikeSSE(br)
}

// looksLikeSSE peeks the first chunk of a buffered reader and reports whether
// it begins with an SSE line, tolerating leading blank lines. Non-consuming.
//
// Every SSE line counts, not just `data:` / `event:`. A comment (`: …`) is the
// one that actually shows up first: an upstream emits comment keepalives while
// a turn is queued, and when it declares no Content-Type this peek is the whole
// decision. Accepting only field lines sent those turns through the whole-body
// JSON parse, which found no usage and failed them closed with a 502 —
// reproduced against a stub upstream that opens with `: queued`.
func looksLikeSSE(br *bufio.Reader) bool {
	peek, _ := br.Peek(512)
	for len(peek) > 0 {
		nl := bytes.IndexByte(peek, '\n')
		var line []byte
		if nl < 0 {
			line = peek
			peek = nil
		} else {
			line = peek[:nl]
			peek = peek[nl+1:]
		}
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			continue // skip leading blank lines
		}
		return bytes.HasPrefix(line, []byte("data:")) ||
			bytes.HasPrefix(line, []byte("event:")) ||
			bytes.HasPrefix(line, []byte("id:")) ||
			bytes.HasPrefix(line, []byte("retry:")) ||
			bytes.HasPrefix(line, []byte(":"))
	}
	return false
}

// shedSignal records an in-band shed observed while relaying a Codex SSE
// stream: upstream accepted the request, answered 200, then refused the turn
// with an error frame instead of content.
//
// Both relays report it — OAuth and API-key alike — because the demotion that
// keeps the client's session alive is invisible by design: the CLI recovers, so
// nothing downstream ever says how often upstream is shedding. Without this the
// only trace was a turn that finished with no usage, which reads as "the relay
// is broken" rather than "the model was at capacity".
type shedSignal struct {
	// shed: an error frame classified as retryable (capacity, quota, or rate).
	shed bool
	// capacity: that shed was one of the two session-terminating capacity
	// codes, and was demoted on the way out so the CLI retries instead of
	// ending the session.
	capacity bool
}

// sseRelayOutcome is what one API-key SSE relay observed, beyond the bytes it
// forwarded. Each field drives a different decision, so they are reported
// separately rather than collapsed into a status.
type sseRelayOutcome struct {
	shedSignal
	// clientGone: the CLIENT is what ended the stream.
	clientGone bool
	// sawTerminal: a stream-terminating event arrived. Without one the client
	// raises "stream disconnected before completion", so its absence is a real
	// upstream fault and must be visible in the log rather than passed off as a
	// clean end-of-stream.
	sawTerminal bool
	// firstOutputAt is when the relay produced its first frame. The API-key path
	// is the only unproxied Codex egress in the fleet, which makes it the
	// control the OAuth path's time-to-first-byte is read against; measuring
	// one and not the other leaves the comparison stuck in ad-hoc analysis.
	firstOutputAt time.Time
}

// streamSSEOpenAI is the OpenAI SSE passthrough. The wire format is `data:
// <json>\n\n` with a terminal `data: [DONE]`; Codex relays answering
// /v1/responses instead terminate with a `response.completed`-family event.
// Usage arrives in the final chunk when stream_options.include_usage is on (we
// always ensure that); parsing it here keeps billing correct for streaming
// clients.
//
// This path used to be a bare read/write loop while the OAuth path
// (streamSSECodexBackend) had three protections it lacked. Every one of them
// matters more here, because API-key relays now carry the bulk of Codex
// traffic:
//
//   - Keepalive. A model that thinks for a minute writes nothing, and the
//     intermediaries in between (Caddy, Cloudflare, the client's own idle
//     timeout) cut a silent connection. The client reports that as "stream
//     disconnected before completion".
//   - Terminal tracking. Without it a truncated upstream is indistinguishable
//     from a clean end, so the fault is invisible in the request log — which is
//     why these disconnects looked like they came from nowhere.
//   - Capacity demotion. Upstream sheds load as an in-band error frame inside a
//     200 stream. Forwarded verbatim, `server_is_overloaded` reaches the CLI as
//     ApiError::ServerOverloaded, which is TERMINAL for the session; the same
//     failure under nearly any other code lands in the CLI's Retryable arm and
//     is merely backed off. cc-core's codexerr.ClientFrame rewrites just that
//     code and leaves the human-readable message intact.
//
// Billing and health are read from the ORIGINAL payload, before demotion —
// after ClientFrame the code no longer says why the request failed.
//
// clientGone is load-bearing for billing: an upstream that never reported usage
// is a relay fault (bill nothing, cool the credential), while a client hang-up
// is the user's own choice (bill the partial usage, leave health alone).
// Conflating the two is what made every Ctrl-C both overcharge and trip the
// breaker. The read error alone cannot tell them apart — an upstream RST and a
// client disconnect surface identically at the reader — so the signal comes
// from the request context, exactly as the Anthropic and Codex-OAuth paths do.
func streamSSEOpenAI(c *gin.Context, reader *bufio.Reader, counts *usage.Counts, rewriteClientModel string) sseRelayOutcome {
	flusher, _ := c.Writer.(http.Flusher)
	// c.Request is nil in unit tests that drive the relay directly; treat that
	// as "no cancellation signal" rather than panicking on the hot path.
	var ctx context.Context
	if c.Request != nil {
		ctx = c.Request.Context()
	}

	var out sseRelayOutcome
	next := func() (emit []byte, terminal bool, err error) {
		line, rerr := reader.ReadBytes('\n')
		if len(line) == 0 {
			return nil, false, rerr
		}
		trim := bytes.TrimRight(line, "\r\n")
		outLine := line
		if bytes.HasPrefix(trim, []byte("data:")) {
			payload := bytes.TrimSpace(trim[5:])
			switch {
			case bytes.Equal(payload, []byte("[DONE]")):
				terminal = true
			case len(payload) > 0 && payload[0] == '{':
				// Accounting first, on the untouched payload.
				mergeCodexUsage(counts, extractOpenAIUsageFromJSON(payload))
				if codexTerminalEvent(payload) {
					terminal = true
				}
				if codexerr.Classify(payload) == codexerr.ClassRetryable {
					out.shed = true
				}
				rewritten := payload
				if rewriteClientModel != "" {
					if r := rewriteResponseModel(rewritten, rewriteClientModel); r != nil {
						rewritten = r
					}
				}
				// Demote only the two session-ending capacity codes. Quota and
				// rate codes are left alone: the CLI handles them
				// non-terminally and parses its retry delay off the original.
				if frame, shed, capacity := codexerr.ClientFrame(rewritten); shed {
					rewritten = frame
					if capacity {
						out.capacity = true
					}
				}
				if !bytes.Equal(rewritten, payload) {
					tail := line[len(trim):]
					rebuilt := make([]byte, 0, len("data: ")+len(rewritten)+len(tail))
					rebuilt = append(rebuilt, []byte("data: ")...)
					rebuilt = append(rebuilt, rewritten...)
					rebuilt = append(rebuilt, tail...)
					outLine = rebuilt
				}
			}
		}
		if len(outLine) > 0 && out.firstOutputAt.IsZero() {
			out.firstOutputAt = time.Now()
		}
		return outLine, terminal, rerr
	}

	// commit=nil: this path commits headers eagerly before the relay starts
	// (writeSSEResponseHeaders), matching the OAuth passthrough.
	r := ccstream.Relay(c.Writer, func() {
		if flusher != nil {
			flusher.Flush()
		}
	}, ccstream.RelayOptions{
		KeepaliveIdle:    10 * time.Second,
		KeepalivePayload: []byte(":\n\n"),
		Next:             next,
	})
	out.sawTerminal = r.SawTerminal
	// Relay drains the upstream body past the terminal event, and Codex CLI
	// closes its socket the moment it has read `response.completed`. The
	// request context is therefore routinely canceled AFTER the turn is
	// complete, and that cancellation surfaces here as a read error on the
	// (context-bound) upstream body. It is not a hang-up: the user got the
	// whole answer. Counting it as one turned 76% of a day's "client canceled"
	// rows (294 of 388, all carrying usage and billed) into a fake 7–15% error
	// rate on the panel. Only a disconnect BEFORE the terminal event is a
	// cancellation.
	out.clientGone = !r.SawTerminal && isClientDisconnect(ctx, r.Err)
	return out
}

// extractOpenAIUsageFromJSON pulls a usage.Counts from an OpenAI-shaped
// response chunk. Handles both the /v1/chat/completions shape:
//
//	{"usage":{"prompt_tokens":N,"completion_tokens":M,
//	  "prompt_tokens_details":{"cached_tokens":K}}}
//
// and the /v1/responses shape (nested under "response.usage" when wrapped
// in an event envelope, or top-level):
//
//	{"response":{"usage":{"input_tokens":N,"output_tokens":M,
//	  "input_tokens_details":{"cached_tokens":K}}}}
//
// Returns a zero Counts when no usage is present — the caller Adds them so
// absent usage is idempotent. Requests counter is incremented only when
// non-zero token counts actually landed (mirrors Anthropic extractor).
func extractOpenAIUsageFromJSON(body []byte) usage.Counts {
	if len(body) == 0 {
		return usage.Counts{}
	}
	var wrap struct {
		Usage    *openaiUsage `json:"usage"`
		Response struct {
			Usage *openaiUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return usage.Counts{}
	}
	u := wrap.Usage
	if u == nil {
		u = wrap.Response.Usage
	}
	if u == nil {
		return usage.Counts{}
	}
	return u.toCounts()
}

type openaiUsage struct {
	// chat/completions names
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// /v1/responses names
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// mergeCodexUsage overlays one usage report onto dst with overwrite-if-positive
// semantics — the contract mergeSSEUsage already gives the Anthropic stream,
// and one the Codex paths used to lack.
//
// They summed instead (counts.Add on every frame that carried a usage object).
// That is only correct while the upstream reports usage exactly once per
// response, which today it does: on response.completed for /v1/responses, on
// the final chunk for chat/completions. Nothing guaranteed it. The moment an
// intermediate event carries a usage snapshot — a running total, which is what
// every other cumulative stream format does — Add bills the prompt twice, and
// no assertion anywhere would notice. Each OpenAI usage object is the total for
// its response, so the last one seen is the answer; taking it, rather than the
// sum, is what makes the second copy harmless.
//
// Requests latches: it marks "usage was observed", and a later event that omits
// a field must not clear it or add to it.
func mergeCodexUsage(dst *usage.Counts, u usage.Counts) {
	if dst == nil {
		return
	}
	if u.InputTokens > 0 {
		dst.InputTokens = u.InputTokens
	}
	if u.OutputTokens > 0 {
		dst.OutputTokens = u.OutputTokens
	}
	if u.CacheReadTokens > 0 {
		dst.CacheReadTokens = u.CacheReadTokens
	}
	if u.CacheCreateTokens > 0 {
		dst.CacheCreateTokens = u.CacheCreateTokens
	}
	if u.CacheCreate1hTokens > 0 {
		dst.CacheCreate1hTokens = u.CacheCreate1hTokens
	}
	if u.ReasoningTokens > 0 {
		dst.ReasoningTokens = u.ReasoningTokens
	}
	if u.Requests > 0 && dst.Requests == 0 {
		dst.Requests = 1
	}
}

func (u openaiUsage) toCounts() usage.Counts {
	input := u.PromptTokens
	if input == 0 {
		input = u.InputTokens
	}
	output := u.CompletionTokens
	if output == 0 {
		output = u.OutputTokens
	}
	cached := u.PromptTokensDetails.CachedTokens
	if cached == 0 {
		cached = u.InputTokensDetails.CachedTokens
	}
	// Follow OpenAI billing: cached prompt tokens are billed at a discount,
	// so we split prompt_tokens into (input - cached) + cached.
	nonCached := input - cached
	if nonCached < 0 {
		nonCached = 0
	}
	// No request is counted unless we actually observed usage data — this
	// keeps partial-stream chunks from over-incrementing the request
	// counter.
	if input == 0 && output == 0 && cached == 0 {
		return usage.Counts{}
	}
	reasoning := u.CompletionTokensDetails.ReasoningTokens
	if reasoning == 0 {
		reasoning = u.OutputTokensDetails.ReasoningTokens
	}
	return usage.Counts{
		InputTokens:  nonCached,
		OutputTokens: output,
		// A subset of OutputTokens, not an addition to it — both spellings of
		// the details block were already parsed and then dropped on the floor.
		// It is the only field that separates a turn the provider downgraded
		// from one it merely served slowly.
		ReasoningTokens: reasoning,
		CacheReadTokens: cached,
		Requests:        1,
	}
}

// extractCodexUpstreamModel reads the model the upstream says it served off a
// terminal event. It is deliberately separate from the usage extractor rather
// than folded into it: usage is merged from every frame that carries any,
// while this answers a different question — whether the provider ran what was
// asked for — and only the terminal event is authoritative about that.
//
// Empty when the payload names no model, which is every non-terminal frame.
func extractCodexUpstreamModel(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var wrap struct {
		Response struct {
			Model string `json:"model"`
		} `json:"response"`
		Model string `json:"model"`
	}
	if json.Unmarshal(payload, &wrap) != nil {
		return ""
	}
	if m := strings.TrimSpace(wrap.Response.Model); m != "" {
		return m
	}
	return strings.TrimSpace(wrap.Model)
}

// small helper duplicating what proxy.go expresses inline — kept separate
// so codex_proxy stays self-contained for future edits.
// isClientDisconnect reports whether err from an upstream request came from
// the *client* going away, not the upstream / proxy dropping the socket.
// Use `ctx` (the client's request context) as the discriminator: if our own
// context is canceled, the client is gone; otherwise the error happened on
// the wire between us and the upstream and should be retried on another
// credential, not masked as "client canceled".
//
// We still accept context.Canceled / DeadlineExceeded *when the ctx has a
// matching error* — http.Client.Do sometimes wraps proxy-side resets in
// context.Canceled after an internal timeout, and those we want to retry.
func isClientDisconnect(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	// Fall-through: a raw context.Canceled with no ctx cancel means the
	// transport itself aborted — treat as upstream failure, not client cancel.
	return false
}

// isTransientNetErr reports whether err looks like a transient wire-level
// failure worth a short retry on the same credential. Targets the CF
// new-connection rate-limit symptom on chatgpt.com (RST mid-TLS), h2 stream
// rejections (PROTOCOL_ERROR / REFUSED_STREAM), and similar proxy/h2 flaps.
// Distinct from isClientDisconnect (client went away) and from HTTP-status
// errors (handled by the pool's ReportUpstreamError path).
//
// Delegates to the canonical classifier in cc-core (auth.IsTransientNetErr) so
// the transport's backoff-retry layer and this caller-side "defer to another
// credential without MarkFailure" decision stay in lockstep.
func isTransientNetErr(err error) bool {
	return auth.IsTransientNetErr(err)
}

// upstreamTTFBMillis renders the gap between handing a request to the transport
// and the upstream's first content-bearing event, for requestlog.Record.TTFBMs.
//
// Zero means "not measured" rather than "instant", so a relay that never
// reported a first output must not be rounded down into looking like the
// fastest turn in the archive: an unset firstOutputAt returns 0, and so does a
// negative gap, which can only come from a caller pairing timestamps from two
// different attempts.
func upstreamTTFBMillis(dispatchAt, firstOutputAt time.Time) int64 {
	if dispatchAt.IsZero() || firstOutputAt.IsZero() {
		return 0
	}
	ms := firstOutputAt.Sub(dispatchAt).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}
