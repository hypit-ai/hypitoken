package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	gorillaws "github.com/gorilla/websocket"

	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/clienttoken"
	"github.com/wjsoj/cc-core/codexws"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/usage"

	"github.com/wjsoj/CPA-Claude/internal/config"
)

// The identity tests next door all call the cc-core helpers (or rebindCodexFrame)
// directly. That proves the helpers work; it proves nothing about whether the
// relay ever calls them. These tests drive the real ingress —
// handleCodexResponsesWS -> pumpCodexWS -> a live upstream socket — so a future
// refactor that drops the rebind or the scrub from a pump direction fails here.

// codexWSTestServer builds the minimum *Server the WS ingress touches. saas and
// reqLog stay nil on purpose: both are nil-guarded, and the SaaS layer is not
// what is under test.
func codexWSTestServer(backendBase string, oauths ...*auth.Auth) *Server {
	return &Server{
		cfg: &config.Config{
			ChatGPTBackendBaseURL: backendBase,
			CodexWS:               config.CodexWSConfig{Enabled: true, BetaVersion: "v2", ReadLimitBytes: 1 << 20},
			UseUTLS:               false,
		},
		pool:             auth.NewPool(oauths, nil, 10*time.Minute, false, ""),
		usage:            usage.OpenInMemory(),
		pricing:          pricing.NewCatalog(pricing.Config{}),
		tokens:           clienttoken.OpenInMemory(),
		codexRespAccount: newCodexRespAccountStore(codexRespAccountTTL),
		codexSessions:    codexws.NewSessionRegistry(0),
	}
}

func codexWSTestOAuth(id string) *auth.Auth {
	//nolint:gosec // G101: a fixed string in a test fixture, not a credential.
	return &auth.Auth{
		ID: id, Kind: auth.KindOAuth, Provider: auth.ProviderOpenAI,
		Label: id, AccessToken: "oauth-token", ExpiresAt: time.Now().Add(time.Hour),
	}
}

// codexWSFront mounts the WS ingress on a real HTTP server so a client can
// upgrade against it, with the client token the auth middleware would have set.
func codexWSFront(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/v1/responses", func(c *gin.Context) {
		c.Set("client_token", "sk-downstream-user")
		c.Set("client_name", "tester")
		s.handleCodexResponsesWS(c)
	})
	front := httptest.NewServer(engine)
	t.Cleanup(front.Close)
	return front
}

func codexWSDialFront(t *testing.T, front *httptest.Server) *gorillaws.Conn {
	t.Helper()
	client, resp, err := gorillaws.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(front.URL, "http")+"/v1/responses",
		http.Header{"Session-Id": []string{"win-42"}})
	if resp != nil {
		// On a failed upgrade gorilla hands back the HTTP response; on success
		// it is a NopCloser over leftover bytes (the live conn lives on
		// `client`). Closing is correct either way, and bodyclose requires it.
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("client upgrade failed (status=%d): %v", status, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

var codexWSUpgraderForTest = gorillaws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func TestCodexWSSelectsAccountThatSupportsRequestedModel(t *testing.T) {
	saw := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := codexWSUpgraderForTest.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err == nil {
			saw <- r.Header.Get("Authorization")
		}
	}))
	defer upstream.Close()
	free, paid := codexWSTestOAuth("free"), codexWSTestOAuth("paid")
	free.PlanType, paid.PlanType = "free", "pro"
	free.AccessToken, paid.AccessToken = "synthetic-free", "synthetic-paid"
	s := codexWSTestServer(upstream.URL, free, paid)
	if got := s.pool.Acquire(context.Background(), auth.ProviderOpenAI, "sk-downstream-user", "", "gpt-6-luna", "win-42", paid.ID); got != free {
		t.Fatal("failed to establish the Free-account sticky session")
	}
	client := codexWSDialFront(t, codexWSFront(t, s))
	if err := client.WriteMessage(gorillaws.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-saw:
		if got != "Bearer synthetic-paid" {
			t.Fatal("paid model reached an ineligible account")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("eligible account never received the request")
	}
}

// (1) The client->upstream direction really rebinds, and the frame's identity
// matches the one advertised on the handshake.
//
// The equality is the whole point of the change: a genuine codex-tui always has
// handshake session-id == client_metadata.session_id, so asserting only
// "non-empty" would pass on exactly the mismatch that gives us away.
func TestCodexWSPumpRebindsClientFrameToHandshakeIdentity(t *testing.T) {
	type peerSaw struct {
		handshakeSession string
		frame            string
	}
	saw := make(chan peerSaw, 4)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hs := r.Header.Get("session-id")
		conn, err := codexWSUpgraderForTest.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Two frames: the first is relayed by the handler, the second by
		// pumpCodexWS's client->upstream goroutine. Both must be rebound — a
		// turn-two leak is the same disclosure as a turn-one leak, and would
		// additionally make session_id disagree with itself inside one socket.
		for {
			_, f, err := conn.ReadMessage()
			if err != nil {
				return
			}
			saw <- peerSaw{handshakeSession: hs, frame: string(f)}
		}
	}))
	defer upstream.Close()

	s := codexWSTestServer(upstream.URL, codexWSTestOAuth("codex-a.json"))
	client := codexWSDialFront(t, codexWSFront(t, s))

	// A downstream client that supplies its OWN ids. None of them may survive.
	const clientSession = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	firstFrame := `{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"session_id":"` +
		clientSession + `","thread_id":"` + clientSession + `","x-codex-window-id":"` + clientSession + `:3"},"input":[]}`
	if err := client.WriteMessage(gorillaws.TextMessage, []byte(firstFrame)); err != nil {
		t.Fatalf("write first frame: %v", err)
	}

	// Turn two goes through pumpCodexWS's client->upstream goroutine, not the
	// handler.
	secondFrame := `{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"session_id":"` +
		clientSession + `","thread_id":"` + clientSession + `"},"input":[]}`
	if err := client.WriteMessage(gorillaws.TextMessage, []byte(secondFrame)); err != nil {
		t.Fatalf("write second frame: %v", err)
	}

	sessionIDs := make([]string, 0, 2)
	for i, label := range []string{"first frame (handler)", "second frame (pump)"} {
		var got peerSaw
		select {
		case got = <-saw:
		case <-time.After(15 * time.Second):
			t.Fatalf("upstream never received the %s — it was not relayed", label)
		}
		if !strings.Contains(got.frame, `"client_metadata"`) {
			t.Fatalf("%s: upstream saw %q, want a client_metadata", label, got.frame)
		}
		var relayed struct {
			ClientMetadata map[string]string `json:"client_metadata"`
		}
		if err := json.Unmarshal([]byte(got.frame), &relayed); err != nil {
			t.Fatalf("%s: relayed frame is not valid JSON: %v", label, err)
		}
		if got.handshakeSession == "" {
			t.Fatal("upstream saw no session-id on the handshake")
		}
		if relayed.ClientMetadata["session_id"] != got.handshakeSession {
			t.Errorf("%s: frame session_id %q disagrees with handshake session-id %q — a real client always has them equal",
				label, relayed.ClientMetadata["session_id"], got.handshakeSession)
		}
		if i == 0 {
			if want := got.handshakeSession + ":0"; relayed.ClientMetadata["x-codex-window-id"] != want {
				t.Errorf("%s: frame window id %q, want %q", label, relayed.ClientMetadata["x-codex-window-id"], want)
			}
		}
		// The client's own ids present N of our users as N installations on one
		// ChatGPT account, and contradict the handshake. They must be gone from
		// the WHOLE frame, not just from the keys we happened to inspect.
		if strings.Contains(got.frame, clientSession) {
			t.Errorf("%s: the downstream client's own session id survived into the upstream frame: %s", label, got.frame)
		}
		sessionIDs = append(sessionIDs, relayed.ClientMetadata["session_id"])
	}
	if sessionIDs[0] != sessionIDs[1] {
		t.Errorf("session id changed mid-socket: %q then %q — one connection is one upstream session",
			sessionIDs[0], sessionIDs[1])
	}
}

// (2) The upstream->client direction really scrubs, and the scrub runs AFTER
// per-turn settlement — a frame the scrub rewrites or drops must not change what
// was billed.
func TestCodexWSPumpScrubsUpstreamFramesWithoutLosingBilling(t *testing.T) {
	ready := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := codexWSUpgraderForTest.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		close(ready)
		for _, frame := range []string{
			// In-band quota state: plan, utilisation, exact reset. Rewritten,
			// not dropped — the client still learns it is throttled.
			`{"type":"codex.rate_limits","plan_type":"pro","rate_limits":{"allowed":false,"limit_reached":true,"primary":{"used_percent":97.5,"reset_after_seconds":900,"reset_at":1789000000}},"credits":{"balance":3}}`,
			// Pure upstream telemetry: dropped whole.
			`{"type":"responsesapi.websocket_timing","timing_metrics":{"ttft_ms":120,"engine":"gpt56sol-codex-a-c321"}}`,
			// Terminal event: carries the turn's billing AND two leaks.
			`{"type":"response.completed","response":{"id":"resp_1","prompt_cache_key":"our-upstream-session","safety_identifier":"user-abc123","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":400},"output_tokens":250}}}`,
		} {
			if err := conn.WriteMessage(gorillaws.TextMessage, []byte(frame)); err != nil {
				return
			}
		}
		_ = conn.WriteMessage(gorillaws.CloseMessage,
			gorillaws.FormatCloseMessage(gorillaws.CloseNormalClosure, "done"))
	}))
	defer upstream.Close()

	s := codexWSTestServer(upstream.URL, codexWSTestOAuth("codex-a.json"))
	client := codexWSDialFront(t, codexWSFront(t, s))
	if err := client.WriteMessage(gorillaws.TextMessage,
		[]byte(`{"type":"response.create","model":"gpt-5.6-sol","input":[]}`)); err != nil {
		t.Fatalf("write first frame: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("upstream never received the first frame")
	}

	// Read everything the client is given until the socket closes.
	var received []string
	_ = client.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		_, data, err := client.ReadMessage()
		if err != nil {
			break
		}
		received = append(received, string(data))
	}

	joined := strings.Join(received, "\n")
	if strings.Contains(joined, "responsesapi.websocket_timing") {
		t.Errorf("pure upstream telemetry reached the client: %s", joined)
	}
	if len(received) != 2 {
		t.Fatalf("client saw %d frames, want 2 (rate_limits rewritten + response.completed): %q", len(received), received)
	}

	// The rate-limit frame is REWRITTEN, not dropped: the client must still be
	// told it is throttled, just not whose quota or when the window rolls over.
	rl := received[0]
	if !strings.Contains(rl, `"codex.rate_limits"`) {
		t.Fatalf("first client frame = %q, want the rewritten codex.rate_limits", rl)
	}
	for _, leak := range []string{"plan_type", "used_percent", "reset_at", "credits"} {
		if strings.Contains(rl, leak) {
			t.Errorf("%q leaked to the client in %s", leak, rl)
		}
	}
	if !strings.Contains(rl, `"limit_reached":true`) {
		t.Errorf("the client must still learn it is throttled: %s", rl)
	}

	// The terminal event survives whole, minus the two identity leaks, with its
	// usage numbers intact.
	done := []byte(received[1])
	if !codexTerminalEvent(done) {
		t.Fatalf("second client frame = %q, want the response.completed", done)
	}
	for _, leak := range []string{"prompt_cache_key", "safety_identifier", "our-upstream-session", "user-abc123"} {
		if strings.Contains(string(done), leak) {
			t.Errorf("%q leaked to the client in %s", leak, done)
		}
	}
	if got := extractCodexBackendUsageFromJSON(done); got.InputTokens != 600 || got.CacheReadTokens != 400 || got.OutputTokens != 250 {
		t.Errorf("usage in the client's copy = %+v, want the upstream numbers intact", got)
	}

	// Settlement is asynchronous but drained before the handler returns, and the
	// handler cannot return before the socket the client is reading closed. Poll
	// briefly rather than sleeping a fixed amount.
	var billed usage.ClientCost
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if pc, ok := s.usage.SnapshotClients()["sk-downstream-user"]; ok && pc.Total.Requests > 0 {
			billed = pc.Total
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if billed.Requests != 1 {
		t.Fatalf("client billed %d requests, want exactly 1 — the scrub must run AFTER per-turn settlement, not instead of it", billed.Requests)
	}
	if billed.Tokens.InputTokens != 600 || billed.Tokens.CacheReadTokens != 400 || billed.Tokens.OutputTokens != 250 {
		t.Errorf("billed tokens %+v, want the upstream's own numbers", billed.Tokens)
	}
	if billed.CostUSD <= 0 {
		t.Errorf("billed cost = %v, want a positive charge for a real turn", billed.CostUSD)
	}
}

// (3) SessionRegistry is nil-safe on every method, so a *Server built without
// codexSessions would not panic on the WS path — it would silently mint a fresh
// session id per frame, dropping every turn out of the upstream prompt cache and
// making the frames disagree with the handshake. A nil check here is far cheaper
// than finding that in production.
func TestNewServerInitializesCodexSessions(t *testing.T) {
	// No endpoints enabled: New() then builds no listeners, so this stays a
	// cheap constructor test rather than a server boot.
	cfg := &config.Config{}
	s := New(cfg, auth.NewPool(nil, nil, time.Minute, false, ""), usage.OpenInMemory(), nil, clienttoken.OpenInMemory())
	if s.codexSessions == nil {
		t.Fatal("New() left codexSessions nil — every WS frame would get a fresh session id, silently")
	}
	if s.codexRespAccount == nil {
		t.Fatal("New() left codexRespAccount nil — the cross-group previous_response_id boundary would be off")
	}
	// Two calls with the same anchor must agree, or the session id is not stable.
	a := s.codexSessions.Identity("acct", "acct|tok|slot")
	b := s.codexSessions.Identity("acct", "acct|tok|slot")
	if a.SessionID == "" || a.SessionID != b.SessionID {
		t.Errorf("session id not stable: %q then %q", a.SessionID, b.SessionID)
	}
}
