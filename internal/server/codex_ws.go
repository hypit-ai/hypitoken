package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	gorillaws "github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"

	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/codexerr"
	"github.com/wjsoj/cc-core/codexws"
	"github.com/wjsoj/cc-core/downstream"
	"github.com/wjsoj/cc-core/mimicry"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/requestlog"
	"github.com/wjsoj/cc-core/servicetier"
	"github.com/wjsoj/cc-core/usage"
)

// Codex WebSocket ingress. Real codex-tui 0.135.0 streams a turn over a
// WebSocket; a long-lived WS carries protocol-level ping/pong, so it survives
// the silent gaps (reasoning -> answer, tool thinking) that truncate the HTTP
// SSE path and surface to clients as "stream disconnected before completion".
//
// This is a passthrough relay: the client already speaks the Codex WS protocol,
// so frames are forwarded verbatim between client and upstream. We only parse
// out, best-effort: the model (for credential acquisition + billing),
// previous_response_id (for the cross-group safety boundary, see codex_session.go),
// the response id (to bind a conversation to its account), and usage (for
// billing, carried inside the terminal event). The whole path is opt-in
// (config.codex_ws.enabled) and unverified against a real ChatGPT token — see
// CLAUDE.md's Codex-OAuth caveat.

const (
	codexWSFirstFrameTimeout = 30 * time.Second
	codexWSUpstreamPingEvery = 20 * time.Second
	codexWSReadDeadline      = 15 * time.Minute
	codexWSWriteDeadline     = 2 * time.Minute
	// codexWSMaxAcquire bounds dial-time credential switches. Once the first
	// upstream frame is relayed to the client the credential is locked (no
	// silent switch is possible after bytes are committed to the client).
	codexWSMaxAcquire = 4
	// codexWSBillQueue is the per-session buffer of completed turns awaiting
	// asynchronous settlement. Deep enough that a slow SaaS write never
	// back-pressures the relay in practice; a full queue falls back to inline
	// billing rather than dropping a charge.
	codexWSBillQueue = 64
)

// codexWSTurnBill is one completed WS turn queued for asynchronous settlement.
type codexWSTurnBill struct {
	meta servicetier.Turn
	turn usage.Counts
	dur  time.Duration
	// seq numbers the turn within its session; it names the charge slot
	// ("turn:<seq>") so a retried settlement replays its own row and no other.
	seq int
}

var codexWSUpgrader = gorillaws.Upgrader{
	ReadBufferSize:    4096,
	WriteBufferSize:   4096,
	EnableCompression: true,
	// The bearer token already authenticated the request (clientAuth middleware
	// ran before this handler); the WS Origin header is not a security boundary
	// for a token-authenticated API, so accept any origin.
	CheckOrigin: func(*http.Request) bool { return true },
}

func isCodexWSUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// handleCodexResponsesWS upgrades a /v1/responses GET into a WebSocket and
// bridges it to the ChatGPT Codex backend over an upstream WebSocket dialed with
// the cc-core uTLS fingerprint.
func (s *Server) handleCodexResponsesWS(c *gin.Context) {
	if !isCodexWSUpgrade(c.Request) {
		writeAPIError(c, auth.ProviderOpenAI, APIError{Status: http.StatusUpgradeRequired, Code: "websocket_upgrade_required", Message: "This endpoint requires a WebSocket upgrade request."})
		return
	}
	const provider = auth.ProviderOpenAI
	start := time.Now()

	clientTokV, _ := c.Get("client_token")
	clientToken, _ := clientTokV.(string)
	if clientToken == "" {
		clientToken = c.ClientIP()
	}
	clientNameV, _ := c.Get("client_name")
	clientName, _ := clientNameV.(string)

	// Pre-flight gates — same single funnel as forward(): SaaS balance/caps,
	// then per-(provider|token) RPM and concurrency. These can still answer with
	// an HTTP status because the WS handshake has not happened yet.
	saasInfo, saasOK := saasInfoFrom(c)
	var clientGroup string
	if saasOK && s.saas != nil {
		clientGroup = s.saas.CredentialGroup(saasInfo)
		if pre := s.saas.PreCheck(c.Request.Context(), saasInfo); pre != nil {
			writeAPIError(c, auth.ProviderOpenAI, APIError{Status: pre.Status, Code: pre.Code, Message: pre.Message, Details: pre.Details})
			return
		}
	} else if entry, ok := s.tokens.Lookup(clientToken); ok {
		clientGroup = entry.Group
	}

	rpmKey := auth.NormalizeProvider(provider) + "|" + clientToken
	if limit := s.clientRPM(c, provider, clientToken); limit > 0 {
		if ok, retry := s.rpm.Allow(rpmKey, limit); !ok {
			c.Header("Retry-After", strconv.Itoa(retry))
			writeAPIError(c, provider, APIError{Status: http.StatusTooManyRequests, Code: "api_key_rate_limit_exceeded", Message: "This API key has exceeded its requests-per-minute limit. Retry after the indicated delay.", Details: map[string]any{"retry_after_seconds": retry}})
			return
		}
	}
	if maxConc := s.clientMaxConcurrent(c, provider, clientToken); maxConc > 0 {
		inflightKey := auth.NormalizeProvider(provider) + "|" + clientToken
		cur, releaseSlot := s.inflight.Begin(inflightKey)
		defer releaseSlot()
		if int(cur) > maxConc {
			c.Header("Retry-After", "5")
			writeAPIError(c, provider, APIError{Status: http.StatusTooManyRequests, Code: "api_key_concurrency_limit_exceeded", Message: "This API key has too many requests in progress. Wait for an active request to finish and try again.", Details: map[string]any{"max_concurrent": maxConc}})
			return
		}
	}

	slotID := clientSlotID(c)

	// Fair-share gate on pool slots. A WS session holds its slot for the whole
	// life of the socket (chatgpt.com keeps these open up to an hour), unlike an
	// HTTP request which holds one for seconds — so without this a couple of
	// heavy WS users sit on most of the provider's slot capacity and everyone
	// else gets "no credentials available" from a healthy fleet. Refuse only
	// slots this token does not already hold, so an established session is never
	// torn down. Checked before the upgrade, while an HTTP status can still be
	// returned.
	// A trusted relay is exempt: the cap counts slots to stop ONE user hoarding
	// the fleet, but a relay's slots belong to many users — capping it would
	// refuse the very fan-out the relay headers exist to enable. Its aggregate
	// pressure is still bounded by the RPM and concurrency limits on its token.
	if maxSess := s.cfg.ClientMaxSessions; maxSess > 0 && clientToken != "" && !relayIsTrusted(c) {
		if held, already := s.pool.SessionsHeld(provider, clientToken, slotID); !already && held >= maxSess {
			log.Warnf("codex ws: token %s at its session cap (%d held, max %d) — refusing a new session",
				maskClientToken(clientToken), held, maxSess)
			c.Header("Retry-After", "30")
			writeAPIError(c, provider, APIError{
				Status:  http.StatusTooManyRequests,
				Code:    "api_key_session_limit_exceeded",
				Message: "You already have too many concurrent Codex sessions open. Close an idle Codex window and retry — long-lived sessions hold an upstream slot for up to an hour.",
				Details: map[string]any{"held": held, "max_sessions": maxSess},
			})
			return
		}
	}

	// Upgrade the client connection. Past this point no HTTP status can be sent;
	// failures close the WS with a control frame.
	clientConn, err := codexWSUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Warnf("codex ws: client upgrade failed: %v", err)
		return
	}
	defer clientConn.Close()
	clientConn.SetReadLimit(s.cfg.CodexWS.ReadLimitBytes)

	// First client frame (response.create) — learn model + previous_response_id
	// before acquiring a credential.
	_ = clientConn.SetReadDeadline(time.Now().Add(codexWSFirstFrameTimeout))
	mt, firstFrame, err := clientConn.ReadMessage()
	if err != nil || mt != gorillaws.TextMessage {
		closeCodexWS(clientConn, gorillaws.CloseProtocolError, "expected initial JSON frame")
		return
	}
	_ = clientConn.SetReadDeadline(time.Time{})

	// The model is read here for credential acquisition, billing, AND the
	// handshake's x-codex-routing-hint, which is set from this same value just
	// below so the header can never name a different model than the frame.
	//
	// This comment used to say the handshake carries no routing hint, "both
	// captures agree" — the 0.153.4 captures disagree: every ordinary upgrade
	// carries it. It must not inherit the "unknown" placeholder below, which
	// exists solely so billing and log rows have something to group on.
	model := codexWSExtractModel(firstFrame)
	if model == "" {
		model = "unknown"
	}

	// Cross-group previous_response_id safety: if the chain belongs to this
	// group's sticky account, keep it; otherwise strip it so the upstream
	// rebuilds from full input (prevents replaying tenant A's chain on B).
	if prevID := codexPreviousResponseID(firstFrame); prevID != "" {
		if _, ok := s.codexRespAccount.Get(clientGroup, prevID); !ok {
			firstFrame = removeCodexPreviousResponseID(firstFrame)
			log.Infof("codex ws: stripped cross-group previous_response_id (group=%q)", clientGroup)
		}
	}

	betaValue := codexws.CodexOpenAIBetaWS
	if s.cfg.CodexWS.BetaVersion == "v1" {
		betaValue = codexws.CodexOpenAIBetaWSV1
	}
	wsURL := codexWSUpstreamURL(s.cfg.ChatGPTBackendBaseURL)

	// Acquire an OAuth credential, retrying dial-time failures on another one.
	tried := map[string]bool{}
	var up codexws.Conn
	var a *auth.Auth
	var ident mimicry.CodexFrameIdentity
	for i := 0; i < codexWSMaxAcquire; i++ {
		exclude := make([]string, 0, len(tried))
		for id := range tried {
			exclude = append(exclude, id)
		}
		cand := s.pool.Acquire(c.Request.Context(), provider, clientToken, clientGroup, model, slotID, exclude...)
		if cand == nil {
			break
		}
		tried[cand.ID] = true
		if cand.Kind != auth.KindOAuth {
			// API-key relays can't speak the ChatGPT WS backend.
			s.pool.Release(provider, clientToken, slotID)
			continue
		}
		snap := cand.Snapshot()
		accessToken, _ := cand.Credentials()
		accountID, _ := cand.CodexIdentity()
		// Resolve the upstream identity for THIS credential. The anchor is
		// server-side: the account key pins it to the credential (so a
		// credential switch correctly starts a new upstream session), and the
		// client token plus slot separate concurrent downstream conversations.
		// slotID is deliberately not used as the session id itself — it comes
		// from a downstream header (and from a trusted relay it is a
		// "client/session" pair, not even a UUID), while the session id becomes
		// our upstream prompt_cache_key, so a caller able to choose it could aim
		// at another tenant's cached prefix.
		candIdent := s.codexSessions.Identity(cand.AccountKey(), cand.AccountKey()+"|"+clientToken+"|"+slotID)
		// Identity carries the session/thread/installation ids, and the same
		// value goes to mimicry.RewriteCodexClientFrame for every frame on this
		// socket. Handing the handshake and the frames one object is what stops
		// them disagreeing — a genuine client always has them identical, so a
		// mismatch is a one-comparison tell. The routing hint is applied after
		// this call rather than through UpstreamHeaderOptions.Model, so it is
		// derived from the same firstFrame that goes upstream.
		header := codexws.BuildUpstreamHeadersWithOptions(codexws.UpstreamHeaderOptions{
			AccessToken: accessToken,
			AccountID:   accountID,
			Identity:    &candIdent,
			BetaValue:   betaValue,
		})
		if hint := mimicry.CodexRoutingHint(model, servicetier.Request(firstFrame)); hint != "" {
			header[mimicry.CodexRoutingHintHeader] = []string{hint}
		}
		conn, resp, derr := codexws.Dial(c.Request.Context(), codexws.DialConfig{
			URL:       wsURL,
			Header:    header,
			ProxyURL:  snap.ProxyURL,
			UseUTLS:   s.cfg.UseUTLS,
			ReadLimit: s.cfg.CodexWS.ReadLimitBytes,
		})
		// On a non-101 the body carries the upstream error; on success gorilla
		// hands back a NopCloser over leftover bytes (the live conn lives on
		// `conn`, not resp.Body), so closing here is safe either way. Headers
		// stay readable after the body is closed.
		var status int
		var retryAfter time.Time
		if resp != nil {
			status = resp.StatusCode
			retryAfter = parseRetryAfter(resp.Header)
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		}
		if derr != nil {
			// derr may embed an unparsed upstream response (e.g. an HTTP/2 SETTINGS
			// frame when ALPN mis-negotiates), which gorilla renders as a long
			// \x-escaped string. Cap it so a binary reply can't dump a screenful.
			log.Warnf("codex ws: upstream dial via %s failed (status=%d): %s", cand.ID, status, truncate([]byte(derr.Error()), 200))
			switch status {
			case http.StatusUnauthorized:
				// Same treatment as the HTTP path (codex_auth_reject.go): the
				// handshake body carries the backend's rejection of the bearer.
				s.rejectCodexBearer(cand, accessToken, []byte(derr.Error()))
				s.pool.Unstick(provider, clientToken, slotID)
			case http.StatusForbidden, http.StatusTooManyRequests:
				s.pool.ReportUpstreamError(cand, status, retryAfter)
				s.pool.Unstick(provider, clientToken, slotID)
			default:
				cand.MarkFailure(derr.Error())
			}
			s.pool.Release(provider, clientToken, slotID)
			continue
		}
		a = cand
		up = conn
		ident = candIdent
		break
	}
	if up == nil || a == nil {
		closeCodexWS(clientConn, gorillaws.CloseTryAgainLater, "no upstream credential available")
		s.emitLog(requestlog.Record{
			Client: clientName, ClientToken: maskClientToken(clientToken), Provider: provider, Model: model,
			Stream: true, Path: "/v1/responses", Status: 503, DurationMs: time.Since(start).Milliseconds(),
			Error: "ws: no upstream credential",
		})
		return
	}
	defer up.Close()
	defer s.pool.Release(provider, clientToken, slotID)

	// Relay the first frame upstream, then run the bidirectional pump. The
	// rebind runs AFTER the previous_response_id strip above, so the final bytes
	// are the byte-splicing rewriter's, not a map round-trip's.
	firstFrame, _, err = servicetier.NormalizeRequest(firstFrame)
	if err != nil {
		closeCodexWS(clientConn, gorillaws.ClosePolicyViolation, "invalid service tier")
		return
	}
	firstFrame = rebindCodexFrame(firstFrame, ident, a)
	tierTracker := &servicetier.TurnTracker{}
	if !tierTracker.Sent(firstFrame) {
		closeCodexWS(clientConn, gorillaws.ClosePolicyViolation, "invalid turn metadata")
		return
	}
	_ = up.SetWriteDeadline(time.Now().Add(codexWSWriteDeadline))
	if err := up.WriteMessage(codexws.TextMessage, firstFrame); err != nil {
		log.Warnf("codex ws: first upstream write via %s failed: %v", a.ID, err)
		closeCodexWS(clientConn, gorillaws.CloseInternalServerErr, "upstream write failed")
		return
	}

	var counts usage.Counts
	// Bill each turn as it completes, not once at the end: a WS session can run
	// for an hour and hundreds of turns, and deferring settlement to the close
	// makes the credential's cost lag its real upstream usage (the quota % ticks
	// up live while total cost sits still) and loses the whole session's billing
	// outright if the process restarts mid-stream.
	//
	// Settlement is asynchronous (matches sub2api): a single per-session goroutine
	// drains a buffered channel and runs the billing funnel (pricing + SaaS DB
	// write + request log) off the relay's hot path, so a slow charge never
	// stalls the next turn's forwarding. The channel is drained (not abandoned)
	// on close, so a normal disconnect loses nothing; only an outright process
	// crash can drop turns still queued — the same trade sub2api's worker pool
	// makes.
	billCh := make(chan codexWSTurnBill, codexWSBillQueue)
	var billWG sync.WaitGroup
	billWG.Add(1)
	go func() {
		defer billWG.Done()
		for tb := range billCh {
			s.billCodexWSTurn(c, a, codexBillingTurnModel(model, tb.meta), clientToken, clientName, tb.turn, tb.dur, tb.seq, pricing.CostOptions{ServiceTier: tb.meta.Requested, ResponseServiceTier: tb.meta.Observed})
		}
	}()
	turnSeq := 0 // only the pump goroutine calls billTurn, so no lock
	billTurn := func(turn usage.Counts, dur time.Duration) {
		turnSeq++
		tb := codexWSTurnBill{meta: tierTracker.LastCompleted(), turn: turn, dur: dur, seq: turnSeq}
		select {
		case billCh <- tb:
		default:
			s.billCodexWSTurn(c, a, codexBillingTurnModel(model, tb.meta), clientToken, clientName, tb.turn, tb.dur, tb.seq, pricing.CostOptions{ServiceTier: tb.meta.Requested, ResponseServiceTier: tb.meta.Observed})
		}
	}
	s.pumpCodexWS(c.Request.Context(), clientConn, up, a, clientGroup, ident, &counts, billTurn, tierTracker)
	close(billCh)
	billWG.Wait() // drain every queued turn before the request returns

	// Per-turn already settled cost + client billing + request log for each turn.
	// Fold the session's full token totals into the auth ledger once here (drives
	// load-balancing weight + the credential's token display); cost is zero to
	// avoid double-charging.
	if counts.InputTokens > 0 || counts.OutputTokens > 0 || counts.CacheReadTokens > 0 || counts.CacheCreateTokens > 0 {
		s.usage.Record(a.ID, a.Label, counts)
	}
	if counts.Requests > 0 {
		a.MarkSuccess()
	}
}

// pumpCodexWS relays frames between the client and upstream WebSockets until
// either side closes. Usage and response-id binding are extracted from the
// upstream->client direction; the cross-group previous_response_id rewrite is
// applied on the client->upstream direction for follow-up turns. Both relay
// goroutines are joined before returning so counts is safe for billing.
func (s *Server) pumpCodexWS(ctx context.Context, client *gorillaws.Conn, up codexws.Conn, a *auth.Auth, group string, ident mimicry.CodexFrameIdentity, counts *usage.Counts, onTurn func(turn usage.Counts, dur time.Duration), tierTrackers ...*servicetier.TurnTracker) {
	var tierTracker *servicetier.TurnTracker
	if len(tierTrackers) > 0 {
		tierTracker = tierTrackers[0]
	}
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			_ = up.Close()
			_ = client.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// upstream -> client
	go func() {
		defer wg.Done()
		defer stop()
		// billed tracks the token totals already settled via onTurn, so each
		// terminal event bills only its own turn's delta. turnStart bounds the
		// per-turn duration reported to the request log.
		var billed usage.Counts
		// turn is the current turn's usage, overlaid overwrite-if-positive
		// (mergeCodexUsage) from whichever events carry it; counts, the
		// session total, only ever grows by whole turns. Summing every usage
		// object straight into counts — the old shape — was right only while
		// upstream reported usage once per turn, and nothing enforced that.
		var turn usage.Counts
		turnStart := time.Now()
		defer func() {
			// A turn cut off before its terminal event is not billed (no
			// terminal, no settlement), but any usage it did report still
			// belongs to the session total the close-time ledger fold reads.
			if turn.Requests > 0 {
				counts.Add(turn)
			}
		}()
		for {
			_ = up.SetReadDeadline(time.Now().Add(codexWSReadDeadline))
			mt, data, err := up.ReadMessage()
			if err != nil {
				return
			}
			// out is what the client sees; data stays the upstream original so
			// classification, usage extraction and terminal detection all read
			// what upstream actually said.
			out := data
			if mt == codexws.TextMessage && len(data) > 0 {
				tierTracker.Observe(data)
				if rid := codexResponseID(data); rid != "" {
					s.codexRespAccount.Bind(group, rid, a.ID)
				}
				mergeCodexUsage(&turn, extractCodexBackendUsageFromJSON(data))
				var shed, capacity bool
				if out, shed, capacity = codexerr.ClientFrame(data); shed {
					log.Warnf("codex ws: %s shed a turn (capacity=%t): %s",
						a.ID, capacity, truncate(data, 200))
				}
				if codexTerminalEvent(data) {
					tierTracker.Complete()
					// One request per turn, whether or not usage was observed
					// (a shed turn still ends). The delta below carries its
					// own Requests=1, so the session total and the per-turn
					// bill agree on what a turn is.
					turn.Requests = 1
					counts.Add(turn)
					turn = usage.Counts{}
					if onTurn != nil {
						onTurn(codexTurnDelta(*counts, billed), time.Since(turnStart))
						billed = *counts
						billed.Requests = 0 // Requests isn't part of the token delta
						turnStart = time.Now()
					}
				}

				// Withhold the pool's operational state, LAST — after usage
				// extraction, response-id binding and per-turn billing, all of
				// which read `data` (what upstream actually said) rather than
				// `out` (what the client gets). Placing it earlier would make a
				// dropped frame skip settlement; today none of the dropped types
				// is terminal, but that is not a property worth depending on.
				var keep bool
				if out, keep = downstream.ScrubCodexEvent(out); !keep {
					continue
				}
			}
			_ = client.SetWriteDeadline(time.Now().Add(codexWSWriteDeadline))
			// Echo the frame's own type. Only text frames go through the
			// scrub above, so relabelling a binary frame as text would forward
			// bytes the client cannot parse AND that were never inspected.
			if err := client.WriteMessage(mt, out); err != nil {
				return
			}
		}
	}()

	// client -> upstream
	go func() {
		defer wg.Done()
		defer stop()
		for {
			mt, data, err := client.ReadMessage()
			if err != nil {
				return
			}
			if mt == gorillaws.TextMessage {
				if prevID := codexPreviousResponseID(data); prevID != "" {
					if _, ok := s.codexRespAccount.Get(group, prevID); !ok {
						data = removeCodexPreviousResponseID(data)
					}
				}
				// Every turn after the first goes through here too. Skipping it
				// would leak the downstream client's identity from turn two
				// onward, which is the same disclosure as leaking it on turn one
				// — and would make session_id disagree with itself inside one
				// connection. Strip first, rewrite second, same as the first frame.
				if codexWSFrameIsResponseCreate(data) {
					var tierErr error
					data, _, tierErr = servicetier.NormalizeRequest(data)
					if tierErr != nil {
						closeCodexWS(client, gorillaws.ClosePolicyViolation, "invalid service tier")
						return
					}
				}
				data = rebindCodexFrame(data, ident, a)
				if !tierTracker.Sent(data) {
					closeCodexWS(client, gorillaws.ClosePolicyViolation, "too many pending turns")
					return
				}
			}
			_ = up.SetWriteDeadline(time.Now().Add(codexWSWriteDeadline))
			// Echo the frame's own type, for the mirror-image reason: only text
			// frames are rebound, so a binary frame relabelled as text would
			// reach upstream still carrying the downstream client's identity.
			if err := up.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}()

	// Upstream keepalive ping during quiet turns.
	go func() {
		t := time.NewTicker(codexWSUpstreamPingEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				stop()
				return
			case <-t.C:
				_ = up.Ping(time.Now().Add(5 * time.Second))
			}
		}
	}()

	wg.Wait()
}

// billCodexWS runs the same billing funnel as the HTTP Codex path: official cost
// -> SaaS Charge (group×provider multiplier) -> usage ledger -> request log. A
// multi-turn WS connection accumulates one Requests increment per terminal
// event, so counts already reflects every billed turn.
// codexTurnDelta returns the tokens consumed since the last settled turn —
// cur (running session total) minus billed (total already settled) — tagged as
// one request. Keeping this a pure function makes the "each turn bills only its
// own delta, never the running total" invariant directly testable.
func codexTurnDelta(cur, billed usage.Counts) usage.Counts {
	return usage.Counts{
		InputTokens:       cur.InputTokens - billed.InputTokens,
		OutputTokens:      cur.OutputTokens - billed.OutputTokens,
		CacheCreateTokens: cur.CacheCreateTokens - billed.CacheCreateTokens,
		CacheReadTokens:   cur.CacheReadTokens - billed.CacheReadTokens,
		Requests:          1,
	}
}

// billCodexWSTurn settles one completed WS turn through the same funnel as the
// HTTP Codex path. turn carries just this turn's tokens with Requests==1. The
// auth's own token ledger is NOT touched here — it is folded in once for the
// whole session when the socket closes, so per-turn settlement never
// double-counts it. One request-log row is emitted per turn, so the admin panel
// shows each turn's real cost as it happens rather than one hour-long row.
func (s *Server) billCodexWSTurn(c *gin.Context, a *auth.Auth, model, clientToken, clientName string, turn usage.Counts, dur time.Duration, seq int, options ...pricing.CostOptions) {
	billingOptions := pricing.CostOptions{}
	if len(options) > 0 {
		billingOptions = options[0]
	}
	billingOptions.CodexOAuth = a.Kind == auth.KindOAuth
	var priced pricing.CostResult
	// CostUSD = official upstream price, BilledUSD = wallet debit.
	var costUSD, billedUSD float64
	var userID int64
	var multiplier float64
	var billingErr string
	if turn.Requests > 0 && clientToken != "" {
		priced = s.pricing.CostWithOptions(auth.ProviderOpenAI, billingModelFor(a, model), turn, billingOptions)
		official := priced.CostUSD
		costUSD = official
		billedUSD = official
		if info, ok := saasInfoFrom(c); ok && s.saas != nil {
			billed, err := s.saas.Charge(chargeCtxSlot(c, fmt.Sprintf("turn:%d", seq)), info, auth.ProviderOpenAI, model, turn, official)
			if err != nil {
				log.Warnf("saas: ws charge failed for token=%d user=%d turn=%d: %v", info.TokenID, info.UserID, seq, err)
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
		s.usage.RecordClient(clientToken, clientName, turn, billedUSD)
	}
	s.emitLog(requestlog.Record{
		RequestedServiceTier: billingOptions.ServiceTier,
		UpstreamServiceTier:  billingOptions.ResponseServiceTier,
		ServiceTier:          priced.Tier.Billing,
		Client:               clientName,
		ClientToken:          maskClientToken(clientToken),
		Provider:             auth.ProviderOpenAI,
		AuthID:               a.ID,
		AuthLabel:            a.Label,
		AuthKind:             "oauth",
		Model:                model,
		Input:                turn.InputTokens,
		Output:               turn.OutputTokens,
		CacheRead:            turn.CacheReadTokens,
		// CacheCreate was missing from this row while Cost and Charge above
		// both counted it — the ledger and the log would have disagreed the
		// day upstream first reported a cache write on a WS turn.
		CacheCreate: turn.CacheCreateTokens,
		CostUSD:     costUSD,
		BilledUSD:   billedUSD,
		UserID:      userID,
		Multiplier:  multiplier,
		Status:      200,
		DurationMs:  dur.Milliseconds(),
		Stream:      true,
		Path:        "/v1/responses",
		Error:       billingErr,
	})
}

func closeCodexWS(conn *gorillaws.Conn, code int, reason string) {
	_ = conn.WriteControl(gorillaws.CloseMessage,
		gorillaws.FormatCloseMessage(code, reason),
		time.Now().Add(2*time.Second))
}

// codexWSUpstreamURL turns the configured ChatGPT backend base (https://...
// /backend-api) into the Codex responses WebSocket URL (wss://.../codex/responses).
func codexWSUpstreamURL(base string) string {
	u := strings.TrimRight(base, "/") + "/codex/responses"
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	default:
		return u
	}
}

// codexWSExtractModel best-effort reads the model from the first client frame,
// checking the top level and a nested "response" envelope.
func codexWSExtractModel(frame []byte) string {
	var probe struct {
		Model    string `json:"model"`
		Response struct {
			Model string `json:"model"`
		} `json:"response"`
	}
	if json.Unmarshal(frame, &probe) != nil {
		return ""
	}
	if probe.Model != "" {
		return probe.Model
	}
	return probe.Response.Model
}

// rebindCodexFrame maps a downstream client's response.create frame into the
// identity we advertised on this socket's handshake.
//
// Without it the frame keeps the DOWNSTREAM client's own client_metadata —
// installation id, session/thread/turn/window ids — so N users of one pooled
// credential present to OpenAI as N installations on a single ChatGPT account,
// and the frame contradicts the handshake headers, which a genuine client
// always matches.
//
// A rewrite failure is a LOCAL judgement, never a credential fault: it must not
// MarkFailure, must not trigger failover, and must not drop the turn. Forwarding
// the original frame is strictly better than killing a working session — the
// worst case is that this one frame keeps the client's ids, which is exactly the
// status quo this function improves on.
func rebindCodexFrame(frame []byte, ident mimicry.CodexFrameIdentity, a *auth.Auth) []byte {
	out, err := mimicry.RewriteCodexClientFrame(frame, ident)
	if err != nil {
		id := "?"
		if a != nil {
			id = a.ID
		}
		log.Warnf("codex ws: client frame rebind via %s failed, forwarding as-is: %v", id, err)
		return frame
	}
	return out
}

// codexResponseID extracts response.id from a Codex backend event payload.
func codexResponseID(payload []byte) string {
	var ev struct {
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return ""
	}
	return ev.Response.ID
}

func codexBillingTurnModel(fallback string, turn servicetier.Turn) string {
	if turn.Model != "" {
		return turn.Model
	}
	return fallback
}

func codexWSFrameIsResponseCreate(frame []byte) bool {
	var event struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(frame, &event) == nil && event.Type == "response.create"
}
