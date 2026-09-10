package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/clienttoken"
	"github.com/wjsoj/cc-core/codexws"
	"github.com/wjsoj/cc-core/mimicry"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/usage"

	"github.com/wjsoj/CPA-Claude/internal/config"
)

// The HTTP Codex path used to mint a fresh `session-id` per REQUEST. That is
// the header the backend places a conversation in its prompt cache by, so every
// turn of a live conversation looked like a brand-new one and re-uploaded a
// context the account already had cached — production cache hit rate fell from
// ~87% to ~45% once a header-name fix made the backend start reading it.
//
// Asserting on codexUpstreamSessionID alone would not catch the regression that
// actually happened: the helper existed on the WS path the whole time and the
// HTTP path simply never called it. So these tests drive the real forward and
// read the header off the wire.

// codexHTTPSessionSpy is a stand-in ChatGPT backend that records the session-id
// of every request it serves and answers with the shortest valid Responses SSE.
type codexHTTPSessionSpy struct {
	mu       sync.Mutex
	sessions []string
	legacy   bool
}

func (spy *codexHTTPSessionSpy) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http canonicalizes an inbound "session-id" to "Session-Id", so
		// Get is the right reader here. The misspelled "Session_id" canonicalizes
		// to itself, so it is asserted absent separately rather than being
		// silently folded in.
		spy.mu.Lock()
		spy.sessions = append(spy.sessions, r.Header.Get(mimicry.CodexSessionIDHeader))
		if _, bad := r.Header["Session_id"]; bad {
			spy.legacy = true
		}
		spy.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: " +
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed",` +
			`"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (spy *codexHTTPSessionSpy) seen(t *testing.T) []string {
	t.Helper()
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.legacy {
		t.Error(`backend saw "Session_id" (underscore) — no genuine Codex client sends that name`)
	}
	return append([]string(nil), spy.sessions...)
}

func codexHTTPTestServer(backendBase string, oauths ...*auth.Auth) *Server {
	return &Server{
		cfg: &config.Config{
			ChatGPTBackendBaseURL: backendBase,
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

// forwardCodexTurn runs one turn through the real OAuth forward and returns
// nothing: what the turn did is read off the spy.
func forwardCodexTurn(t *testing.T, s *Server, cred *auth.Auth, cacheKey, clientToken, slotID string) {
	t.Helper()
	turn := map[string]any{
		"model":  "gpt-5.6-sol",
		"stream": true,
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "hello"}},
		}},
	}
	if cacheKey != "" {
		turn["prompt_cache_key"] = cacheKey
	} else {
		// A response-chain continuation names no conversation of its own:
		// conversationAnchor withholds the content fallback there because such a
		// turn carries only the delta. This is the case the slot fallback exists
		// for, and the one where getting it wrong is worst — the chain id is only
		// valid on the account that minted it.
		turn["previous_response_id"] = "resp_chain"
	}
	body, err := json.Marshal(turn)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Set("client_token", clientToken)
	c.Set("client_name", "tester")
	s.doForwardCodexOAuth(c, cred, "/v1/responses", body, true, "gpt-5.6-sol", clientToken, "tester", slotID, time.Now(), 1)
}

// Every turn of one conversation must present the SAME session id upstream.
func TestCodexHTTPSessionIDStableAcrossTurns(t *testing.T) {
	spy := &codexHTTPSessionSpy{}
	backend := spy.start(t)
	cred := codexWSTestOAuth("codex-acct-1")
	s := codexHTTPTestServer(backend.URL, cred)

	for range 3 {
		forwardCodexTurn(t, s, cred, "conv-alpha", "sk-tenant-one", "slot-a")
	}

	seen := spy.seen(t)
	if len(seen) != 3 {
		t.Fatalf("backend saw %d turns, want 3 (%v)", len(seen), seen)
	}
	if seen[0] == "" {
		t.Fatal("no session-id reached the backend")
	}
	for i, got := range seen {
		if got != seen[0] {
			t.Fatalf("turn %d presented session-id %q, want %q — a per-request id "+
				"drops every turn out of the upstream prompt cache", i, got, seen[0])
		}
	}
	if got := seen[0][14]; got != '7' {
		t.Errorf("session-id %q has UUID version nibble %q, want '7'", seen[0], got)
	}
}

// Two unrelated conversations behind ONE client token must not share a session
// id: it is the upstream cache namespace, so merging them interleaves unrelated
// prefixes in one cache. A sessionless relay is exactly this shape.
func TestCodexHTTPSessionIDSeparatesConversations(t *testing.T) {
	spy := &codexHTTPSessionSpy{}
	backend := spy.start(t)
	cred := codexWSTestOAuth("codex-acct-1")
	s := codexHTTPTestServer(backend.URL, cred)

	// Same token, same slot — only the conversation differs, which is what a
	// relay's two users look like once they land in the same fan-out bucket.
	forwardCodexTurn(t, s, cred, "conv-alpha", "sk-tenant-one", "slot-a")
	forwardCodexTurn(t, s, cred, "conv-beta", "sk-tenant-one", "slot-a")

	seen := spy.seen(t)
	if len(seen) != 2 {
		t.Fatalf("backend saw %d turns, want 2", len(seen))
	}
	if seen[0] == seen[1] {
		t.Errorf("two conversations shared session-id %q", seen[0])
	}
}

// The id is scoped to the credential: after a failover the new account has none
// of this conversation cached, and replaying one account's session id on
// another is a shape no genuine client produces.
func TestCodexHTTPSessionIDIsPerCredential(t *testing.T) {
	spy := &codexHTTPSessionSpy{}
	backend := spy.start(t)
	credA := codexWSTestOAuth("codex-acct-1")
	credB := codexWSTestOAuth("codex-acct-2")
	s := codexHTTPTestServer(backend.URL, credA, credB)

	forwardCodexTurn(t, s, credA, "conv-alpha", "sk-tenant-one", "slot-a")
	forwardCodexTurn(t, s, credB, "conv-alpha", "sk-tenant-one", "slot-a")

	seen := spy.seen(t)
	if len(seen) != 2 {
		t.Fatalf("backend saw %d turns, want 2", len(seen))
	}
	if seen[0] == seen[1] {
		t.Errorf("two credentials shared session-id %q", seen[0])
	}
}

// A tenant must not be able to steer its way onto another tenant's session id
// (and so its cached prefix) by choosing prompt_cache_key.
func TestCodexHTTPSessionIDSeparatesTenants(t *testing.T) {
	spy := &codexHTTPSessionSpy{}
	backend := spy.start(t)
	cred := codexWSTestOAuth("codex-acct-1")
	s := codexHTTPTestServer(backend.URL, cred)

	forwardCodexTurn(t, s, cred, "conv-alpha", "sk-tenant-one", "slot-a")
	forwardCodexTurn(t, s, cred, "conv-alpha", "sk-tenant-two", "slot-a")

	seen := spy.seen(t)
	if len(seen) != 2 {
		t.Fatalf("backend saw %d turns, want 2", len(seen))
	}
	if seen[0] == seen[1] {
		t.Errorf("two tenants shared session-id %q — a caller that picks its own "+
			"prompt_cache_key could aim at another tenant's cached prefix", seen[0])
	}
}

// metadata.user_id names the END USER in the OpenAI dialect, not the
// conversation. Anchoring the upstream session on it merged every concurrent
// thread of one user into a single upstream session, so the backend routed
// several unrelated 200k-token conversations together and they evicted each
// other from the account's prompt cache.
func TestCodexHTTPSessionIDIgnoresOpenAIUserID(t *testing.T) {
	spy := &codexHTTPSessionSpy{}
	backend := spy.start(t)
	cred := codexWSTestOAuth("codex-acct-1")
	s := codexHTTPTestServer(backend.URL, cred)

	// Two distinct conversations of ONE end user: same metadata.user_id, no
	// prompt_cache_key, different opening messages.
	for _, opening := range []string{"refactor the parser", "write release notes"} {
		body, err := json.Marshal(map[string]any{
			"model":    "gpt-5.6-sol",
			"stream":   true,
			"metadata": map[string]any{"user_id": "end-user-42"},
			"input": []any{map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": opening}},
			}},
		})
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		gin.SetMode(gin.TestMode)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
		c.Set("client_token", "sk-tenant-one")
		c.Set("client_name", "tester")
		s.doForwardCodexOAuth(c, cred, "/v1/responses", body, true, "gpt-5.6-sol",
			"sk-tenant-one", "tester", "slot-a", time.Now(), 1)
	}

	seen := spy.seen(t)
	if len(seen) != 2 {
		t.Fatalf("backend saw %d turns, want 2", len(seen))
	}
	if seen[0] == seen[1] {
		t.Errorf("two conversations of one end user shared session-id %q — "+
			"metadata.user_id is the user, not the conversation", seen[0])
	}
}

// A body that names no conversation falls back to the scheduler slot, which is
// still stable per turn.
func TestCodexHTTPSessionIDFallsBackToSlot(t *testing.T) {
	spy := &codexHTTPSessionSpy{}
	backend := spy.start(t)
	cred := codexWSTestOAuth("codex-acct-1")
	s := codexHTTPTestServer(backend.URL, cred)

	forwardCodexTurn(t, s, cred, "", "sk-tenant-one", "slot-a")
	forwardCodexTurn(t, s, cred, "", "sk-tenant-one", "slot-a")
	forwardCodexTurn(t, s, cred, "", "sk-tenant-one", "slot-b")

	seen := spy.seen(t)
	if len(seen) != 3 {
		t.Fatalf("backend saw %d turns, want 3", len(seen))
	}
	if seen[0] != seen[1] {
		t.Errorf("same slot produced %q then %q", seen[0], seen[1])
	}
	if seen[1] == seen[2] {
		t.Errorf("different slots shared session-id %q", seen[2])
	}
}

// The client's own headers must not ride along to chatgpt.com.
//
// Every other forward path copies them, because it is proxying to something
// that expects a proxy. This one is pretending to be a desktop application
// talking to its vendor, and every capture of that application shows six to
// eight headers on the wire. Copying meant whatever the downstream client sent
// arrived on top of the ten we carefully match — one chatty SDK was enough to
// make this deployment's requests a shape nothing else produces.
func TestClientHeadersDoNotReachChatGPT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n" +
			`data: {"type":"response.completed","usage":{"input_tokens":1,"output_tokens":1}}` + "\n\n"))
	}))
	t.Cleanup(up.Close)

	cred := &auth.Auth{
		ID: "codex-hdr.json", Kind: auth.KindOAuth, Provider: auth.ProviderOpenAI,
		AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour),
	}
	s := codexHTTPTestServer(up.URL, cred)

	body := []byte(`{"model":"gpt-5.6-sol","stream":true,"prompt_cache_key":"conv",` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	// The shape a real third-party caller produces: an SDK's telemetry block,
	// an org header, browser hints, and something entirely of its own.
	for k, v := range map[string]string{
		"X-Stainless-Lang":    "js",
		"X-Stainless-Runtime": "node",
		"Openai-Organization": "org-abc",
		"Accept-Language":     "zh-CN,zh;q=0.9",
		"Sec-Ch-Ua":           `"Chromium";v="131"`,
		"X-Totally-Custom":    "1",
		"Openai-Beta":         "assistants=v2",
	} {
		c.Request.Header.Set(k, v)
	}
	c.Set("client_token", "tok-1")
	c.Set("client_name", "tester")

	s.doForwardCodexOAuth(c, cred, "/v1/responses", body, true, "gpt-5.6-sol", "tok-1", "tester", "slot", time.Now(), 1)

	if got == nil {
		t.Fatal("upstream never received the request")
	}
	for _, leaked := range []string{
		"X-Stainless-Lang", "X-Stainless-Runtime", "Openai-Organization",
		"Accept-Language", "Sec-Ch-Ua", "X-Totally-Custom", "Openai-Beta",
	} {
		if v := got.Get(leaked); v != "" {
			t.Errorf("client header %s=%q reached chatgpt.com", leaked, v)
		}
	}
	// And the identity we DO owe upstream is still there — this test must fail
	// if the fix were "send nothing".
	for _, required := range []string{"Authorization", "User-Agent", "Originator", "Version"} {
		if got.Get(required) == "" {
			t.Errorf("%s went missing from the upstream request", required)
		}
	}
}
