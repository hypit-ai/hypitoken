package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	mimicry.ApplyCodexHeadersWithSession(upReq, mimicry.DefaultCodexProfile(), accessToken, accountID,
		isCompactPath, routingModel, routingTier, s.codexUpstreamSessionID(a, clientToken, slotID, body))

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
	resp, err := client.Do(upReq)
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

	// Observe original bytes before protocol conversion or response scrubbing.
	tierObserver := servicetier.ObserveBody(resp.Body)
	resp.Body = tierObserver
	var priced pricing.CostResult
	var counts usage.Counts
	var streamErr string
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
			res = streamCodexAsChatCompletions(c, resp.Body, &counts, model, chatStreamWantsUsage(body), func() { writeSSEResponseHeaders(c, resp) })
		} else {
			res = streamSSECodexBackend(c, resp, &counts, func() { writeSSEResponseHeaders(c, resp) })
		}
		sawTerminal, wroteAny, rerr = res.sawTerminal, res.wroteAny, res.err
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
				log.Warnf("codex oauth: %s shed the request before any output (attempt %d, %s): %s — retrying on another credential",
					a.ID, attempts, time.Since(start).Round(time.Millisecond), res.shed)
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
			if isClientDisconnect(ctx, rerr) {
				streamErr = "client canceled"
				// 499 + MarkClientCancel match the pre-stream disconnect branch
				// above, so a mid-stream hang-up lands in the same bucket as one
				// that happened a second earlier instead of as a 200 carrying an
				// error string. MarkClientCancel is health-neutral by design —
				// the credential did nothing wrong.
				logStatus = 499
				a.MarkClientCancel("client canceled mid-stream")
				log.Infof("codex oauth: client canceled mid-stream via %s", a.ID)
			} else {
				streamErr = "stream truncated before terminal event"
				log.Warnf("codex oauth: SSE stream ended before terminal event (truncated upstream) via %s: %v", a.ID, rerr)
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
		CacheRead:            counts.CacheReadTokens,
		CostUSD:              costUSD,
		BilledUSD:            billedUSD,
		UserID:               userID,
		Multiplier:           multiplier,
		Status:               logStatus,
		DurationMs:           time.Since(start).Milliseconds(),
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
// content-free events upstream always opens with. They carry no model output,
// so holding them back costs the client nothing — and it keeps the response
// uncommitted long enough for a capacity shed to be withheld and failed over
// instead of being forwarded as an error the user has to see.
func codexPreambleEvent(payload []byte) bool {
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return false
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
var codexContentFreeEvents = map[string]bool{
	"response.created":                      true,
	"response.in_progress":                  true,
	"response.output_item.added":            true,
	"response.content_part.added":           true,
	"response.reasoning_summary_part.added": true,
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
func streamSSECodexBackend(c *gin.Context, resp *http.Response, counts *usage.Counts, commit func()) codexStreamResult {
	flusher, _ := c.Writer.(http.Flusher)
	reader := newLineReader(resp.Body)
	out := codexStreamResult{}

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
	// preamble buffers the content-free opening events (response.created,
	// response.in_progress) until the stream reveals what it is.
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
	// anything else, and a shed — the case this exists for — lands at a median
	// of 3.3s.
	var preamble []byte

	next := func() (emit []byte, terminal bool, err error) {
		for {
			line, rerr := reader.readLine()
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
						mergeCodexUsage(counts, extractCodexBackendUsageFromJSON(payload))

						if codexerr.Classify(payload) == codexerr.ClassRetryable {
							if !sentAny {
								// Failover is still possible — withhold this
								// frame and everything after it, including the
								// held event line, any buffered preamble and
								// the response.failed that follows, so nothing
								// commits the response.
								shedding = true
								out.shed = truncate(payload, 200)
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

						if codexTerminalEvent(payload) && !shedding {
							terminal = true
						}

						// Buffer a content-free opener instead of emitting it,
						// so it does not count as output and foreclose failover.
						// Anything else falls through and flushes the buffer.
						if !sentAny && !shedding && codexPreambleEvent(payload) {
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

			// The blank line that closes an SSE event belongs to the event
			// before it, so while an opener is buffered its terminator has to be
			// buffered too. Left alone it matches none of the cases above, falls
			// straight through to the emit switch as a line of its own, and
			// there it both flushes the buffer early and marks the stream as
			// having produced output — which is exactly what forecloses the
			// failover this buffer exists to preserve.
			if !sentAny && !shedding && len(preamble) > 0 && len(line) > 0 && len(bytes.TrimSpace(line)) == 0 {
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
			case rerr != nil && len(held) > 0 && !shedding:
				// Stream ended with an unresolved event line — release it so
				// nothing is silently dropped.
				emit, held = held, nil
			}

			// Flush the buffered opener ahead of whatever released it, so the
			// client still receives the stream in upstream's original order. On
			// a clean EOF with nothing but a preamble, release it too rather
			// than swallowing the whole response.
			if len(preamble) > 0 && !shedding && (len(emit) > 0 || rerr != nil) {
				emit = append(append(make([]byte, 0, len(preamble)+len(emit)), preamble...), emit...)
				preamble = nil
			}

			if len(emit) > 0 {
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
