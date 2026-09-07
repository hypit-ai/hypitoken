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
