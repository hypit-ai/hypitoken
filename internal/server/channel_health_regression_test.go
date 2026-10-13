package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestCodexAPIKeyMissingMetadataDoesNotBlockChannel(t *testing.T) {
	delta := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
	terminal := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	for _, tc := range []struct {
		name, path, contentType, response string
		stream                            bool
	}{
		{"complete without usage", "/v1/responses", "text/event-stream", delta + terminal, true},
		{"truncated without usage", "/v1/responses", "text/event-stream", delta, true},
		{"truncated with usage", "/v1/responses", "text/event-stream", delta + "data: {\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}\n\n", true},
		{"bridged truncated", "/v1/chat/completions", "text/event-stream", delta, true},
		{"native JSON without usage", "/v1/responses", "application/json", `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer upstream.Close()
			a := codexAPIKeyCred("relay")
			s := codexAPIKeyTestServer(upstream.URL, a)
			body := []byte(`{"model":"gpt-6-astra","input":"hello","messages":[{"role":"user","content":"hello"}]}`)
			// Repeated observations must not accumulate into a channel cooldown.
			for i := 0; i < 4; i++ {
				c, w := newCodexContext(t, tc.path, body)
				retry, done := s.doForwardCodex(c, a, tc.path, body, tc.stream, "gpt-6-astra", "test", "test", "", time.Now(), 1)
				if retry || !done || w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello") {
					t.Fatalf("response not forwarded: retry=%v done=%v status=%d body=%s", retry, done, w.Code, w.Body.String())
				}
				if a.IsModelRateLimited(apiKeyModelScope("gpt-6-astra"), time.Now()) || a.HealthState().State != auth.HealthHealthy {
					t.Fatalf("metadata observation blocked channel: %+v", a.HealthState())
				}
			}
		})
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
