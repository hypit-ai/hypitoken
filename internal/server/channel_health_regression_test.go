package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wjsoj/cc-core/auth"
)

func TestClaudeChannelRejectionsAreScoped(t *testing.T) {
	for _, tc := range []struct {
		name, body                  string
		status                      int
		modelPause, credentialFault bool
	}{
		{"model unavailable", `{"error":{"code":"model_not_found","message":"no channel"}}`, 503, true, false},
		{"CLI restriction", `{"error":{"message":"Request blocked: this endpoint only accepts requests from the official Claude Code CLI."}}`, 403, false, false},
		{"balance failure", `{"error":{"message":"用户额度不足"}}`, 403, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			a := apiKeyTestCred("scoped")
			s := newDoForwardTestServer(t, upstream.URL, a)
			c, w := newMessagesContext(t, haikuBody)
			retry, done, d := s.doForwardAnthropicAPIKey(c, a, "/v1/messages", haikuBody, false, "claude-haiku-4-5-20251001", "test", "test", time.Now(), 1)
			if !retry || done || d == nil || w.Body.Len() != 0 {
				t.Fatal("rejection must remain eligible for failover")
			}
			if got := a.IsModelRateLimited(apiKeyModelScope("claude-haiku-4-5-20251001"), time.Now()); got != tc.modelPause {
				t.Fatalf("model pause=%v", got)
			}
			if a.IsModelRateLimited(apiKeyModelScope("claude-opus-5"), time.Now()) {
				t.Fatal("unrelated model paused")
			}
			if got := a.HealthState().State != auth.HealthHealthy; got != tc.credentialFault {
				t.Fatalf("credential health=%+v", a.HealthState())
			}
		})
	}
}

func TestMissingUsagePausesOnlyAffectedModelDespiteConcurrentSuccess(t *testing.T) {
	a := codexAPIKeyCred("relay")
	s := &Server{}
	s.recordAPIKeyStreamFailure(a, "gpt-6-astra", "missing usage")
	s.recordAPIKeySuccess(a, time.Now())
	if !a.IsModelRateLimited(apiKeyModelScope("gpt-6-astra"), time.Now()) {
		t.Fatal("unrelated success erased model cooldown")
	}
	if a.IsModelRateLimited(apiKeyModelScope("gpt-6-sol"), time.Now()) {
		t.Fatal("unrelated model cooled")
	}
	if a.IsModelRateLimited(apiKeyModelScope("gpt-6-astra"), time.Now().Add(time.Minute)) {
		t.Fatal("model cooldown did not expire")
	}
}

func TestAPIKeyEmptyCompletedStreamRemainsRetryable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"))
	}))
	defer upstream.Close()
	a := codexAPIKeyCred("empty-completed")
	s := codexAPIKeyTestServer(upstream.URL, a)
	body := []byte(`{"model":"gpt-6-sol","input":"hello","stream":true}`)
	c, w := newCodexContext(t, "/v1/responses", body)
	retry, done := s.doForwardCodex(c, a, "/v1/responses", body, true, "gpt-6-sol", "test", "test", "", time.Now(), 1)
	if !retry || done || w.Body.Len() != 0 {
		t.Fatalf("empty completed stream committed: retry=%v done=%v body=%s", retry, done, w.Body.String())
	}
}
