package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wjsoj/cc-core/auth"
)

// End-to-end through the handler: a pool of resellers must produce a counted
// 200, not the 404 the reseller would have returned.
func TestCountTokensAnsweredLocallyEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
	relay := &auth.Auth{ID: "relay", Kind: auth.KindAPIKey, Provider: auth.ProviderAnthropic,
		AccessToken: "sk-x", BaseURL: "https://www.duckcoding.ai"}
	s := codexGuardServer(relay)

	body := `{"model":"claude-sonnet-5","system":"You are a coding agent.","messages":[{"role":"user","content":[{"type":"text","text":"refactor the parser please"}]}],"tools":[{"name":"bash","description":"run a shell command"}]}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	s.handleCountTokens(c)

	if w.Code != 200 {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}
	var got struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, w.Body.String())
	}
	if got.InputTokens < 10 || got.InputTokens > 300 {
		t.Errorf("input_tokens = %d, not a plausible count for this body", got.InputTokens)
	}
	t.Logf("count_tokens → %d", got.InputTokens)
}

func TestInputTokensAnsweredLocallyEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := codexGuardServer(wsCred("codex-oauth"))
	body := `{"model":"gpt-5.6-sol","instructions":"You are a coding agent.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"refactor the parser please"}]}]}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses/input_tokens", strings.NewReader(body))
	s.handleCodexInputTokens(c)

	if w.Code != 200 {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}
	var got struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got.InputTokens < 5 {
		t.Errorf("input_tokens = %d, too low", got.InputTokens)
	}
	t.Logf("input_tokens → %d", got.InputTokens)
}

// A body with no model is rejected the way the vendors reject it, in the shape
// the caller's API uses.
func TestTokenCountRejectsBodyWithoutModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	relay := &auth.Auth{ID: "relay", Kind: auth.KindAPIKey, Provider: auth.ProviderAnthropic,
		AccessToken: "sk-x", BaseURL: "https://www.duckcoding.ai"}
	s := codexGuardServer(relay)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{"messages":[]}`))
	s.handleCountTokens(c)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 for a body with no model", w.Code)
	}
}
