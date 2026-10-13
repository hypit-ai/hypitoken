package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wjsoj/CPA-Claude/internal/config"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/clienttoken"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/usage"
)

// Codex API-key credentials reach their provider over the Responses protocol
// regardless of how the client came in. These relays resell Codex capacity,
// which is Responses-native; forwarding an inbound /v1/chat/completions
// verbatim reached an endpoint that either 404s or silently serves a different
// (non-Codex) upstream pool.
//
// The OAuth path already bridged chat/completions (codex_oauth_proxy.go) and is
// deliberately untouched by these tests beyond sharing the translator.

func codexAPIKeyTestServer(upstreamURL string, creds ...*auth.Auth) *Server {
	return &Server{
		cfg:     &config.Config{OpenAIBaseURL: upstreamURL, UseUTLS: false},
		pool:    auth.NewPool(creds, nil, 10*time.Minute, false, ""),
		usage:   usage.OpenInMemory(),
		pricing: pricing.NewCatalog(pricing.Config{}),
		tokens:  clienttoken.OpenInMemory(),
	}
}

func codexAPIKeyCred(id string) *auth.Auth {
	//nolint:gosec // G101: fixed test fixture, not a credential.
	return &auth.Auth{
		ID: id, Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI,
		Label: id, AccessToken: "sk-relay-" + id,
	}
}

func newCodexContext(t *testing.T, path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

var chatBody = []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"stream":true}`)

// A streaming chat/completions request must leave as a Responses request and
// come back rendered as chat.completion.chunk — the client never learns the
// protocol was swapped underneath it.
func TestCodexAPIKeyChatIsBridgedOntoResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var gotPath string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"))
	}))
	defer upstream.Close()

	cred := codexAPIKeyCred("relayA")
	s := codexAPIKeyTestServer(upstream.URL, cred)
	c, w := newCodexContext(t, "/v1/chat/completions", chatBody)

	retry, done := s.doForwardCodex(c, cred, "/v1/chat/completions", chatBody, true,
		"gpt-5.6-sol", "tok-abcdef123456", "client", "", time.Now(), 1)
	if retry || !done {
		t.Fatalf("bridged chat request should be served, got retry=%v done=%v", retry, done)
	}

	if gotPath != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses — API-key providers must be reached over Responses", gotPath)
	}
	var up map[string]any
	if err := json.Unmarshal(gotBody, &up); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if _, ok := up["input"]; !ok {
		t.Errorf("upstream body must carry Responses `input`, got keys %v", keysOf(up))
	}
	if _, ok := up["messages"]; ok {
		t.Error("upstream body must not carry Chat Completions `messages` after bridging")
	}
	if up["stream"] != true {
		t.Errorf("bridged body must carry the client's stream intent, got %v", up["stream"])
	}
	// stream_options is a Chat Completions field; Responses rejects it as an
	// unknown parameter, so the bridge must not inject it.
	if _, ok := up["stream_options"]; ok {
		t.Error("stream_options must never reach a Responses upstream")
	}

	out := w.Body.String()
	if !strings.Contains(out, "chat.completion.chunk") {
		t.Errorf("client must receive chat.completion.chunk frames, got %q", truncateForTest(out))
	}
	if strings.Contains(out, "response.output_text.delta") {
		t.Error("raw Responses events must not leak to a chat/completions client")
	}
}

// The usage reported on response.completed is Responses-shaped
// (input_tokens/output_tokens). Reading it with the Chat Completions extractor
// would silently bill $0 and cool a healthy credential, so the bridge must use
// the Codex extractor on both stream and non-stream paths.
func TestCodexAPIKeyBridgedStreamBillsObservedUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"))
	}))
	defer upstream.Close()

	cred := codexAPIKeyCred("relayB")
	s := codexAPIKeyTestServer(upstream.URL, cred)
	c, _ := newCodexContext(t, "/v1/chat/completions", chatBody)

	s.doForwardCodex(c, cred, "/v1/chat/completions", chatBody, true,
		"gpt-5.6-sol", "tok-abcdef123456", "client", "", time.Now(), 1)

	// A credential that served accounted-for usage must be healthy, not cooled.
	if _, _, _, consecutive := cred.HealthSnapshot(); consecutive != 0 {
		t.Fatalf("ConsecutiveFailures = %d, want 0 — usage was reported, the credential is fine", consecutive)
	}
}

// Non-streaming bridged request: the relay answers with a single Responses
// object and the client must get one chat.completion object back.
func TestCodexAPIKeyBridgedNonStreamRendersChatCompletion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed",` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],` +
			`"usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	cred := codexAPIKeyCred("relayC")
	s := codexAPIKeyTestServer(upstream.URL, cred)
	c, w := newCodexContext(t, "/v1/chat/completions", body)

	retry, done := s.doForwardCodex(c, cred, "/v1/chat/completions", body, false,
		"gpt-5.6-sol", "tok-abcdef123456", "client", "", time.Now(), 1)
	if retry || !done {
		t.Fatalf("bridged non-stream request should be served, got retry=%v done=%v", retry, done)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("client payload is not JSON: %v (%q)", err, truncateForTest(w.Body.String()))
	}
	if out["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", out["object"])
	}
	if out["model"] != "gpt-5.6-sol" {
		t.Errorf("model = %v, want the client-facing name gpt-5.6-sol", out["model"])
	}
}

// A native /v1/responses request must be untouched by the bridge: same path,
// same body. This is the overwhelming majority of real Codex CLI traffic.
func TestCodexAPIKeyResponsesPathIsNotRewritten(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	var gotPath string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"))
	}))
	defer upstream.Close()

	cred := codexAPIKeyCred("relayD")
	s := codexAPIKeyTestServer(upstream.URL, cred)
	c, w := newCodexContext(t, "/v1/responses", body)

	s.doForwardCodex(c, cred, "/v1/responses", body, true,
		"gpt-5.6-sol", "tok-abcdef123456", "client", "", time.Now(), 1)

	if gotPath != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses", gotPath)
	}
	var up map[string]any
	if err := json.Unmarshal(gotBody, &up); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if _, ok := up["input"]; !ok {
		t.Error("a native Responses body must pass through with its input intact")
	}
	// Native passthrough must stay verbatim SSE, not chat-rendered.
	if strings.Contains(w.Body.String(), "chat.completion") {
		t.Error("a native /v1/responses client must not be handed chat.completion frames")
	}
}

// A body the translator cannot represent (no messages) must fall back to
// verbatim chat/completions forwarding rather than failing the request: a plain
// OpenAI-compatible gateway behind an API key still serves it.
func TestCodexAPIKeyUntranslatableChatFallsBackVerbatim(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.6-sol","messages":[]}`)
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"chat.completion","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	cred := codexAPIKeyCred("relayE")
	s := codexAPIKeyTestServer(upstream.URL, cred)
	c, _ := newCodexContext(t, "/v1/chat/completions", body)

	s.doForwardCodex(c, cred, "/v1/chat/completions", body, false,
		"gpt-5.6-sol", "tok-abcdef123456", "client", "", time.Now(), 1)

	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want the verbatim /v1/chat/completions fallback", gotPath)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func truncateForTest(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// A valid answer without accounting metadata must still reach the client.
func TestCodexAPIKeyBridgedNonStreamWithoutUsageIsForwarded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// A completed Responses object carrying no usage at all.
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed",` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`))
	}))
	defer upstream.Close()

	cred := codexAPIKeyCred("relayF")
	s := codexAPIKeyTestServer(upstream.URL, cred)
	c, w := newCodexContext(t, "/v1/chat/completions", body)

	retry, done := s.doForwardCodex(c, cred, "/v1/chat/completions", body, false,
		"gpt-5.6-sol", "tok-abcdef123456", "client", "", time.Now(), 1)

	if retry || !done {
		t.Fatalf("answer without usage must be forwarded; got retry=%v done=%v", retry, done)
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("answer did not reach the client; got %q", truncateForTest(w.Body.String()))
	}
	if _, _, _, consecutive := cred.HealthSnapshot(); consecutive != 0 {
		t.Fatalf("ConsecutiveFailures = %d, want 0", consecutive)
	}
}

// A client that hangs up mid-aggregation surfaces as the same read error an
// upstream fault would, but there is nobody left to retry for. Rotating anyway
// burns one credential per attempt on a request no one is listening to and ends
// in a 502 written to a closed connection.
//
// Observed in production the minute the aggregation branch learned to retry:
// one canceled turn spent 8 attempts before giving up, another 9.
func TestCodexAPIKeyBridgedAggregationClientCancelIsNotRetried(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}]}`)

	// Upstream opens an SSE stream and never finishes it; the client's context
	// is canceled underneath, which is what a real hang-up looks like here.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
	}))
	defer upstream.Close()

	cred := codexAPIKeyCred("relayG")
	s := codexAPIKeyTestServer(upstream.URL, cred)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()

	retry, done := s.doForwardCodex(c, cred, "/v1/chat/completions", body, false,
		"gpt-5.6-sol", "tok-abcdef123456", "client", "", time.Now(), 1)

	if retry || !done {
		t.Fatalf("a client hang-up must end the exchange, not rotate credentials; got retry=%v done=%v", retry, done)
	}
	// The credential did nothing wrong, so its health must be untouched.
	if _, _, _, consecutive := cred.HealthSnapshot(); consecutive != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 — the client left, the relay is blameless", consecutive)
	}
}
