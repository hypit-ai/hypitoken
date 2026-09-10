package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/codexws"

	"github.com/wjsoj/CPA-Claude/internal/config"
)

// egressConn is a scripted upstream WebSocket: it replays frames and records
// what was written to it.
type egressConn struct {
	mu     sync.Mutex
	frames []string
	writes [][]byte
	closed bool
	pings  int
}

func (c *egressConn) WriteJSON(any) error { return nil }
func (c *egressConn) WriteMessage(_ int, d []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, append([]byte(nil), d...))
	return nil
}

func (c *egressConn) ReadMessage() (int, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.frames) == 0 {
		return 0, nil, io.EOF
	}
	next := c.frames[0]
	c.frames = c.frames[1:]
	return codexws.TextMessage, []byte(next), nil
}

func (c *egressConn) Ping(time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pings++
	return nil
}
func (c *egressConn) SetReadDeadline(time.Time) error  { return nil }
func (c *egressConn) SetWriteDeadline(time.Time) error { return nil }
func (c *egressConn) HandshakeResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusSwitchingProtocols}
}
func (c *egressConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *egressConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *egressConn) rawFrame(t *testing.T) []byte {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.writes) == 0 {
		t.Fatal("nothing was written upstream")
	}
	return c.writes[0]
}

func (c *egressConn) sentFrame(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.writes) == 0 {
		t.Fatal("nothing was written upstream")
	}
	var out map[string]any
	if err := json.Unmarshal(c.writes[0], &out); err != nil {
		t.Fatalf("upstream frame is not JSON: %v (%s)", err, c.writes[0])
	}
	return out
}

// wsEgressServer builds a Server whose Codex OAuth path prefers the WebSocket
// and dials the supplied fake instead of a real upstream. backendBase is the
// HTTP fallback the egress rolls back to.
func wsEgressServer(t *testing.T, backendBase string, dial func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error), creds ...*auth.Auth) *Server {
	t.Helper()
	s := codexHTTPTestServer(backendBase, creds...)
	// Load() normalizes in production; do that one step by hand here.
	up := config.CodexWSUpstreamConfig{Mode: config.CodexWSUpstreamAuto}
	up.Normalize()
	s.cfg.CodexWS = config.CodexWSConfig{Upstream: up}
	e := newCodexWSEgress(s.cfg)
	e.dialFn = dial
	s.codexWSEgress = e
	t.Cleanup(e.Close)
	return s
}

func wsTurnBody() []byte {
	return []byte(`{"model":"gpt-5.6-sol","stream":true,"prompt_cache_key":"conv-1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
}

func runCodexTurn(t *testing.T, s *Server, cred *auth.Auth, body []byte) (*httptest.ResponseRecorder, bool, bool) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
	retry, done := s.doForwardCodexOAuth(c, cred, "/v1/responses", body, true, "gpt-5.6-sol", "tok", "tester", "slot-1", time.Now(), 1)
	return w, retry, done
}

func wsCred(id string) *auth.Auth {
	return &auth.Auth{
		ID: id, Kind: auth.KindOAuth, Provider: auth.ProviderOpenAI,
		AccessToken: "test-token", ExpiresAt: time.Now().Add(time.Hour),
	}
}

// The whole premise of the egress: an ordinary HTTP request is carried over a
// WebSocket and the client still receives an SSE stream, byte-shaped exactly as
// the HTTP upstream would have produced.
func TestCodexWSEgressServesHTTPClientOverWebSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &egressConn{frames: []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.completed","response":{"id":"resp_1"},"usage":{"input_tokens":11,"output_tokens":4}}`,
	}}
	cred := wsCred("ws-account")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)

	w, retry, done := runCodexTurn(t, s, cred, wsTurnBody())
	if retry || !done {
		t.Fatalf("turn did not complete: retry=%v done=%v", retry, done)
	}
	got := w.Body.String()
	for _, want := range []string{
		"event: response.created",
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		"event: response.completed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("client stream missing %q:\n%s", want, got)
		}
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want SSE", ct)
	}
}

// The frame is the part a genuine client would be compared against: it must be
// a response.create, and it must carry the identity we put on the handshake
// rather than nothing at all.
func TestCodexWSEgressSendsIdentityBoundResponseCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &egressConn{frames: []string{
		`{"type":"response.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	}}
	cred := wsCred("ws-frame-account")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)

	runCodexTurn(t, s, cred, wsTurnBody())

	frame := conn.sentFrame(t)
	if frame["type"] != "response.create" {
		t.Fatalf("frame type = %v, want response.create", frame["type"])
	}
	if frame["model"] != "gpt-5.6-sol" {
		t.Fatalf("frame lost the model: %v", frame["model"])
	}
	if frame["client_metadata"] == nil {
		t.Fatal("frame carries no client_metadata; no genuine client sends one without it")
	}
	// The client's own cache key must not become our upstream cache namespace.
	if frame["prompt_cache_key"] == "conv-1" {
		t.Fatal("client-chosen prompt_cache_key reached upstream")
	}
	// The backend streams a turn's events regardless; real frames say so.
	if frame["stream"] != true {
		t.Fatalf("frame stream = %v, want true", frame["stream"])
	}

	// Top-level key ORDER, not just contents. The sanitizer upstream of this
	// round-trips the body through a map, so what arrives is alphabetical; the
	// frame builder is the one place that can put it back into the captured
	// order, and a helper that re-sorts (setJSONBool did) silently undoes it.
	// json.Unmarshal into a map cannot see order, so read the raw bytes.
	raw := conn.rawFrame(t)
	wantOrder := []string{"type", "model", "input", "stream", "prompt_cache_key", "client_metadata"}
	at := -1
	for _, k := range wantOrder {
		i := bytes.Index(raw, []byte(`"`+k+`":`))
		if i < 0 {
			t.Fatalf("frame is missing %q: %s", k, raw)
		}
		if i < at {
			t.Fatalf("key %q is out of captured order in: %s", k, raw)
		}
		at = i
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`{"type":"response.create"`)) {
		t.Fatalf("type is not the first key: %s", raw)
	}

	// request_kind must say what the frame actually is. A prewarm is a
	// generate:false cache-priming frame that returns no output; ours generates.
	md := map[string]any{}
	cm, _ := frame["client_metadata"].(map[string]any)
	tm, _ := cm["x-codex-turn-metadata"].(string)
	if err := json.Unmarshal([]byte(tm), &md); err != nil {
		t.Fatalf("embedded turn metadata invalid: %v", err)
	}
	if md["request_kind"] != "turn" {
		t.Fatalf("request_kind = %v, want turn", md["request_kind"])
	}
	if md["workspace_kind"] != "projectless" {
		t.Fatalf("workspace_kind = %v, want projectless", md["workspace_kind"])
	}
}

// The safety property that makes this enableable: a WebSocket that fails before
// any byte is committed must be invisible to the caller. The same turn is
// re-run over HTTP, and the credential is pinned to HTTP afterwards so the next
// request does not pay the same failed dial.
func TestCodexWSEgressFallsBackToHTTPBeforeAnyOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}\n\n")
	}))
	defer upstream.Close()

	cred := wsCred("ws-fallback-account")
	dials := 0
	s := wsEgressServer(t, upstream.URL, func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		dials++
		return nil, nil, errors.New("websocket: bad handshake")
	}, cred)

	w, retry, done := runCodexTurn(t, s, cred, wsTurnBody())
	if retry || !done {
		t.Fatalf("fallback turn did not complete: retry=%v done=%v", retry, done)
	}
	if !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("client did not get the HTTP-served stream:\n%s", w.Body.String())
	}
	if dials != 1 {
		t.Fatalf("dial attempts = %d, want 1", dials)
	}
	if !s.codexWSEgress.inCooldown(cred.ID) {
		t.Fatal("a failed dial did not pin the credential to HTTP")
	}

	// Second turn must not re-dial while the cooldown holds.
	runCodexTurn(t, s, cred, wsTurnBody())
	if dials != 1 {
		t.Fatalf("dial attempts after cooldown = %d, want 1", dials)
	}
}

// A capacity shed arriving before output is what the pre-output withhold exists
// for. Carrying the turn over a WebSocket must not cost that: the client sees
// nothing and the caller is told to try another credential.
func TestCodexWSEgressPreOutputShedStillFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &egressConn{frames: []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"error","error":{"code":"server_is_overloaded","message":"Selected model is at capacity."}}`,
		`{"type":"response.failed","response":{"status":"failed"}}`,
	}}
	cred := wsCred("ws-shed-account")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)

	w, retry, done := runCodexTurn(t, s, cred, wsTurnBody())
	if !retry || done {
		t.Fatalf("pre-output shed did not roll back: retry=%v done=%v", retry, done)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("shed leaked bytes to the client: %q", w.Body.String())
	}
	// The socket IS kept. A shed ends with response.failed, which is a terminal
	// event: the turn is over upstream and nothing of it is left queued, so the
	// connection is reusable even though the turn was refused. The two questions
	// are separate — "was this turn served?" is answered to the caller by
	// retry=true, while "is this socket clean?" is what decides pooling.
	if conn.isClosed() {
		t.Fatal("a socket drained through response.failed was discarded")
	}
}

// A socket whose turn broke early may still have that turn's frames queued.
// Pooling it would deliver them to the next turn as if they belonged to it.
func TestCodexWSEgressDiscardsSocketAfterTruncatedTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &egressConn{frames: []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_text.delta","delta":"partial"}`,
		// then EOF, with no terminal event
	}}
	cred := wsCred("ws-truncated-account")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)

	runCodexTurn(t, s, cred, wsTurnBody())
	if !conn.isClosed() {
		t.Fatal("truncated turn's socket was returned to the pool")
	}
}

func TestCodexWSEgressEligibility(t *testing.T) {
	on := config.CodexWSUpstreamConfig{Mode: config.CodexWSUpstreamAuto}
	on.Normalize()
	cfg := &config.Config{ChatGPTBackendBaseURL: "https://chatgpt.com/backend-api",
		CodexWS: config.CodexWSConfig{Upstream: on}}
	e := newCodexWSEgress(cfg)
	defer e.Close()

	oauth := wsCred("cred-1")
	apikey := &auth.Auth{ID: "cred-2", Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI}

	if !e.eligible(oauth, "/v1/responses", "") {
		t.Fatal("a plain OAuth /v1/responses request should be eligible")
	}
	if e.eligible(apikey, "/v1/responses", "") {
		t.Fatal("API-key credentials do not reach chatgpt.com and must not be eligible")
	}
	if e.eligible(oauth, "/v1/responses/compact", "") {
		t.Fatal("compact has no WebSocket equivalent and must not be eligible")
	}
	if e.eligible(oauth, "/v1/responses", "https://relay.example/codex") {
		t.Fatal("a per-credential relay base URL must not be assumed to terminate WebSockets")
	}
	if e.eligible(nil, "/v1/responses", "") {
		t.Fatal("nil credential must not be eligible")
	}

	// The canary allowlist is what gives the rollout a concurrent control
	// group: listed credentials go over the WebSocket while the rest keep
	// carrying turns over HTTP through the same minutes.
	only := config.CodexWSUpstreamConfig{Mode: config.CodexWSUpstreamAuto, AuthIDs: []string{"cred-1"}}
	only.Normalize()
	scoped := newCodexWSEgress(&config.Config{
		ChatGPTBackendBaseURL: "https://chatgpt.com/backend-api",
		CodexWS:               config.CodexWSConfig{Upstream: only},
	})
	defer scoped.Close()
	if !scoped.eligible(oauth, "/v1/responses", "") {
		t.Fatal("the allowlisted credential was excluded")
	}
	if scoped.eligible(wsCred("cred-9"), "/v1/responses", "") {
		t.Fatal("a credential outside the allowlist was admitted; there would be no control group")
	}

	// The allowlist matches auth.Auth.ID, which is the credential FILENAME.
	// An operator writing the short label gets an entry that admits nothing —
	// which reads exactly like "the WebSocket egress had no effect" — so the
	// id must be matched exactly and the mismatch has to be announced.
	if scoped.eligible(wsCred("cred-1@gmail.com-pro.json"), "/v1/responses", "") {
		t.Fatal("allowlist matched on a prefix; a canary must admit exactly what was named")
	}

	// Disabled by default: a deployment that never sets the mode keeps the
	// behaviour every release before this one had.
	unset := config.CodexWSUpstreamConfig{}
	unset.Normalize()
	off := &config.Config{ChatGPTBackendBaseURL: "https://chatgpt.com/backend-api",
		CodexWS: config.CodexWSConfig{Upstream: unset}}
	offEgress := newCodexWSEgress(off)
	defer offEgress.Close()
	if offEgress.eligible(oauth, "/v1/responses", "") {
		t.Fatal("WebSocket egress is on without being configured")
	}
}

// A typo in the mode must degrade to the proven transport, not to an
// unintended one and not to a failed startup.
func TestCodexWSUpstreamModeFallsBackToSSE(t *testing.T) {
	for _, mode := range []string{"", "sse", "websocket", "WS ", "nonsense"} {
		got := config.CodexWSUpstreamConfig{Mode: mode}
		got.Normalize()
		switch strings.ToLower(strings.TrimSpace(mode)) {
		case "ws":
			if got.Mode != config.CodexWSUpstreamWS || got.HTTPFallbackAllowed() {
				t.Errorf("mode %q => %q (fallback=%v)", mode, got.Mode, got.HTTPFallbackAllowed())
			}
		default:
			if got.Mode != config.CodexWSUpstreamSSE || got.WSEgressEnabled() {
				t.Errorf("mode %q => %q (ws=%v), want sse/off", mode, got.Mode, got.WSEgressEnabled())
			}
		}
		if got.PoolIdle() <= 0 || got.PoolMaxAge() <= 0 || got.ReadTimeout() <= 0 || got.FallbackCooldown() <= 0 {
			t.Errorf("mode %q left a zero duration: %+v", mode, got)
		}
	}
}

// The 101's headers reach the caller as if they were an HTTP response's, so the
// four that describe the upgrade itself have to go — while the x-codex-* quota
// snapshot, which is the reason to keep any of them, has to stay.
func TestHandshakeResponseHeadersDropsUpgradeNoise(t *testing.T) {
	in := &http.Response{Header: http.Header{
		"Upgrade":                      []string{"websocket"},
		"Connection":                   []string{"Upgrade"},
		"Sec-Websocket-Accept":         []string{"abc="},
		"X-Codex-Primary-Used-Percent": []string{"41.5"},
		"X-Request-Id":                 []string{"req_1"},
	}}
	out := handshakeResponseHeaders(in)
	for _, gone := range []string{"Upgrade", "Connection", "Sec-Websocket-Accept"} {
		if len(out[gone]) != 0 {
			t.Errorf("%s survived", gone)
		}
	}
	if out.Get("X-Codex-Primary-Used-Percent") != "41.5" {
		t.Error("quota header was dropped")
	}
	if out.Get("X-Request-Id") != "req_1" {
		t.Error("request id was dropped")
	}
	if handshakeResponseHeaders(nil) == nil {
		t.Error("nil handshake should yield an empty header, not nil")
	}
}

// The claim that the WebSocket is a pure transport swap rests on the branches
// below sharing the response pipeline. These two are the ones with their own
// readers — the non-streaming aggregator and the chat-completions bridge — so
// they are where a transport that only worked for plain SSE would show it.
func TestCodexWSEgressServesNonStreamingClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &egressConn{frames: []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]},"usage":{"input_tokens":9,"output_tokens":2}}`,
	}}
	cred := wsCred("ws-nonstream-account")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)

	body := []byte(`{"model":"gpt-5.6-sol","stream":false,"prompt_cache_key":"conv-ns","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
	retry, done := s.doForwardCodexOAuth(c, cred, "/v1/responses", body, false, "gpt-5.6-sol", "tok", "tester", "slot-ns", time.Now(), 1)

	if retry || !done {
		t.Fatalf("non-streaming turn did not complete: retry=%v done=%v", retry, done)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON for a non-streaming client", ct)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("aggregated body is not JSON: %v (%s)", err, w.Body.String())
	}
	if out["id"] != "resp_1" {
		t.Fatalf("aggregate lost the response: %v", out)
	}
	// The frame still says stream:true — the backend has no other mode — and the
	// aggregation downstream is what makes that invisible to the caller.
	if conn.sentFrame(t)["stream"] != true {
		t.Fatal("non-streaming request went upstream with stream:false")
	}
}

// TestChatCompletionsNeverDialsTheWebSocket replaces a test that asserted the
// opposite: that the chat bridge served a turn over the WebSocket egress. It
// did, until production showed what that costs.
//
// apicompat translates a chat body into the Responses shape and the result is
// thinner than anything the vendor client sends — no prompt_cache_key, no
// reasoning, no tool_choice, no parallel_tool_calls, empty instructions. The
// HTTP backend serves it; the WebSocket backend accepts it and never schedules
// it. The path ran at 93-97% every day until the canary was widened to every
// credential at 22:41 on 2026-09-08 and at 5% the hour after, on two
// deployments, while /v1/responses was unaffected.
//
// The bridge's own translation and frame rendering are covered by
// codex_chat_bridge_test.go over the HTTP path. What this pins is that a chat
// request never reaches the dialer at all.
func TestChatCompletionsNeverDialsTheWebSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dialed := false
	cred := wsCred("ws-chat-account")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		dialed = true
		return nil, nil, errors.New("chat must never dial the WebSocket")
	}, cred)

	body := []byte(`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
	s.doForwardCodexOAuth(c, cred, "/v1/chat/completions", body, true, "gpt-5.6-sol", "tok", "tester", "slot-chat", time.Now(), 1)

	if dialed {
		t.Fatal("a chat/completions turn dialed the WebSocket — the translated body parks there and produces nothing")
	}
}

// TestChatCompletionsStaysOnHTTP is the regression for the night the WebSocket
// canary was widened to every credential.
//
// chat/completions is not a Codex turn — apicompat translates it into the
// Responses shape and what comes out has no prompt_cache_key, no reasoning, no
// tool_choice, no parallel_tool_calls and an empty instructions string. The
// HTTP backend serves that; the WebSocket backend accepts it and parks it. The
// path ran at 93-97% every day until 22:41 on 2026-09-08 and at 5% the hour
// after, on two independent deployments, while /v1/responses was unaffected on
// the same credentials over the same transport.
func TestChatCompletionsStaysOnHTTP(t *testing.T) {
	cred := wsCred("eligibility")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return nil, nil, errors.New("dial must not be reached")
	}, cred)

	if s.codexWSEgress.eligible(cred, "/v1/chat/completions", "") {
		t.Error("chat/completions was routed over the WebSocket — the translated body parks there and the turn produces nothing")
	}
	if s.codexWSEgress.eligible(cred, "/v1/responses/compact", "") {
		t.Error("compact has no WebSocket equivalent and must stay on HTTP")
	}
	if !s.codexWSEgress.eligible(cred, "/v1/responses", "") {
		t.Error("/v1/responses is the path this transport exists for and must remain eligible")
	}
}

// runCodexTurnCtx is runCodexTurn with the gin context handed back, so a test
// can read the flags an attempt leaves behind for the forward loop.
func runCodexTurnCtx(t *testing.T, s *Server, cred *auth.Auth, body []byte) (*gin.Context, bool, bool) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
	retry, done := s.doForwardCodexOAuth(c, cred, "/v1/responses", body, true, "gpt-5.6-sol", "tok", "tester", "slot-1", time.Now(), 1)
	return c, retry, done
}

// A pooled socket the backend closed while it sat idle must not cost the
// credential its place in the failover order.
//
// The pool's liveness probe cannot catch this one: Ping only writes a control
// frame, and a half-closed socket accepts that write, so the death is only
// discovered on the turn's first read. In production this was 513 pre-output
// breaks an hour, a quarter of them dying in under a second — every one of them
// rotating a healthy account out of the running and landing the conversation on
// an account with a cold prompt cache.
func TestStaledPooledSocketRetriesTheSameCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// One clean turn, then nothing: the socket goes back to the pool and the
	// next turn's first read finds it gone.
	conn := &egressConn{frames: []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.completed","response":{"id":"resp_1"},"usage":{"input_tokens":11,"output_tokens":4}}`,
	}}
	dials := 0
	cred := wsCred("ws-stale-account")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		dials++
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)

	if _, retry, done := runCodexTurnCtx(t, s, cred, wsTurnBody()); retry || !done {
		t.Fatalf("first turn did not complete: retry=%v done=%v", retry, done)
	}
	c, retry, done := runCodexTurnCtx(t, s, cred, wsTurnBody())
	if dials != 1 {
		t.Fatalf("second turn did not reuse the pooled socket (dials=%d)", dials)
	}
	if !retry || done {
		t.Fatalf("stale socket did not roll the attempt back: retry=%v done=%v", retry, done)
	}
	if !c.GetBool(codexStaleSocketRetryKey) {
		t.Fatal("stale pooled socket excluded a healthy credential instead of asking for one more round on it")
	}
}

// The counterpart: a FRESH socket that breaks before any output is a real
// upstream fault on that credential, and must still rotate away from it.
// Without this the flag would turn every transport break into a retry loop on
// one account.
func TestFreshSocketBreakStillRotatesCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &egressConn{} // no frames at all: EOF on the first read
	cred := wsCred("ws-fresh-break-account")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)

	c, retry, done := runCodexTurnCtx(t, s, cred, wsTurnBody())
	if !retry || done {
		t.Fatalf("pre-output break did not roll back: retry=%v done=%v", retry, done)
	}
	if c.GetBool(codexStaleSocketRetryKey) {
		t.Fatal("a fresh socket's break was excused as a stale pooled one")
	}
}
