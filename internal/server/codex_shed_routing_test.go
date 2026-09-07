package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wjsoj/cc-core/auth"
)

// A capacity shed has to leave a per-(credential, model) mark, or the scheduler
// sends the very next request for that model back to the account upstream just
// refused it on. cc-core turns the mark into a sort demotion; this pins the
// wiring on the relay side, which is where it silently goes missing.
//
// The sibling-model assertion is the important half. Sheds are model-scoped —
// production had gpt-6-astra shedding 26% of turns on accounts serving
// gpt-5.6-sol at 0.7% in the same window — so a mark that leaked across models
// would take a healthy account off its whole catalogue over one busy model.
func TestCodexShedMarksOnlyTheShedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "gpt-6-astra"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// The real shape: a 200 whose stream opens normally and then refuses.
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Selected model is at capacity. Please try a different model.\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n")
	}))
	defer upstream.Close()

	cred := &auth.Auth{
		ID: "shed-account", Kind: auth.KindOAuth, Provider: auth.ProviderOpenAI,
		AccessToken: "test-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	s := codexHTTPTestServer(upstream.URL, cred)
	s.cfg.OpenAIBaseURL = upstream.URL

	body := []byte(fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"hello"}],"stream":true}`, model))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))

	retry, done := s.doForwardCodexOAuth(c, cred, "/v1/responses", body, true, model, "shed-test-token", "tester", "", time.Now(), 1)
	if !retry || done {
		t.Fatalf("a shed before any output must ask for another credential; retry=%t done=%t", retry, done)
	}

	now := time.Now()
	if got := cred.ModelShedPenalty(model, now); got == 0 {
		t.Fatalf("%s carries no shed penalty after being shed", model)
	}
	if got := cred.ModelShedPenalty("gpt-5.6-sol", now); got != 0 {
		t.Fatalf("sibling model penalty = %d, want 0 — a shed must not leak across models", got)
	}
}

// The converse: a turn that completes clears whatever penalty an earlier shed
// left, so a credential is not held at the back of the queue after upstream
// capacity has come back.
func TestCodexCleanTurnClearsShedPenalty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "gpt-6-astra"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n")
	}))
	defer upstream.Close()

	cred := &auth.Auth{
		ID: "recovered-account", Kind: auth.KindOAuth, Provider: auth.ProviderOpenAI,
		AccessToken: "test-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	cred.MarkModelShed(model, time.Now())
	s := codexHTTPTestServer(upstream.URL, cred)
	s.cfg.OpenAIBaseURL = upstream.URL

	body := []byte(fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"hello"}],"stream":true}`, model))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))

	if retry, done := s.doForwardCodexOAuth(c, cred, "/v1/responses", body, true, model, "clear-test-token", "tester", "", time.Now(), 1); retry || !done {
		t.Fatalf("clean turn: retry=%t done=%t status=%d", retry, done, w.Code)
	}
	if got := cred.ModelShedPenalty(model, time.Now()); got != 0 {
		t.Fatalf("penalty after a served turn = %d want 0", got)
	}
}

// The shed that production actually produces does not arrive on a silent
// stream: when the backend cannot schedule a turn it parks it and heartbeats
// every ~30s, then sheds a fraction of a second after one of those beats. The
// heartbeat carries no output, but it used to commit the response, so the shed
// behind it could only be demoted and shown to the user as a reconnect. Three
// sampled sheds all had this exact shape and 443 of 510 shed turns in a
// six-hour window were never retried because of it.
func TestCodexKeepaliveDoesNotForecloseFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "gpt-6-astra"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n")
		_, _ = io.WriteString(w, "event: keepalive\ndata: {\"type\":\"keepalive\",\"sequence_number\":2}\n\n")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Selected model is at capacity. Please try a different model.\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n")
	}))
	defer upstream.Close()

	cred := &auth.Auth{
		ID: "keepalive-shed", Kind: auth.KindOAuth, Provider: auth.ProviderOpenAI,
		AccessToken: "test-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	s := codexHTTPTestServer(upstream.URL, cred)
	s.cfg.OpenAIBaseURL = upstream.URL

	body := []byte(fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"hello"}],"stream":true}`, model))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))

	retry, done := s.doForwardCodexOAuth(c, cred, "/v1/responses", body, true, model, "ka-token", "tester", "", time.Now(), 1)
	if !retry || done {
		t.Fatalf("a shed behind a keepalive is still pre-output; retry=%t done=%t", retry, done)
	}
	if got := w.Body.String(); got != "" {
		t.Fatalf("nothing may reach the client before failover, got %q", got)
	}
	if got := cred.ModelShedPenalty(model, time.Now()); got == 0 {
		t.Fatal("the shed must still be recorded against this credential")
	}
}

// A keepalive on a stream that goes on to serve the turn is replayed in
// upstream's original order rather than dropped, so the client sees the
// sequence it would have seen without the proxy.
func TestCodexKeepaliveReplayedWhenTurnSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "gpt-6-astra"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: keepalive\ndata: {\"type\":\"keepalive\",\"sequence_number\":1}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n")
	}))
	defer upstream.Close()

	cred := &auth.Auth{
		ID: "keepalive-ok", Kind: auth.KindOAuth, Provider: auth.ProviderOpenAI,
		AccessToken: "test-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	s := codexHTTPTestServer(upstream.URL, cred)
	s.cfg.OpenAIBaseURL = upstream.URL

	body := []byte(fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"hello"}],"stream":true}`, model))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))

	if retry, done := s.doForwardCodexOAuth(c, cred, "/v1/responses", body, true, model, "ka-ok-token", "tester", "", time.Now(), 1); retry || !done {
		t.Fatalf("clean turn: retry=%t done=%t", retry, done)
	}
	got := w.Body.String()
	created := strings.Index(got, "response.created")
	ka := strings.Index(got, "\"keepalive\"")
	delta := strings.Index(got, "output_text.delta")
	if created < 0 || ka < 0 || delta < 0 {
		t.Fatalf("client lost part of the stream: %q", got)
	}
	if created >= ka || ka >= delta {
		t.Fatalf("events replayed out of upstream order: created=%d keepalive=%d delta=%d", created, ka, delta)
	}
}
