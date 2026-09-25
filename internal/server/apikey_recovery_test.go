package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wjsoj/cc-core/auth"
)

func TestAPIKeyAutoDisablePersistsAndCannotBeLastResort(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key.json")
	data := []byte(`{"type":"openai_api_key","provider":"openai","api_key":"test-only","label":"broken"}`)
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.ParseFile(p, data)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{pool: auth.NewPool(nil, []*auth.Auth{a}, time.Minute, false, "")}
	// Mixed upstream failures belong to the same consecutive failure run.
	for _, status := range []int{502, 429, 401} {
		s.reportCodexAPIKeyFault(a, status, time.Time{})
	}
	a.IsQuarantined(time.Now().Add(24 * time.Hour))
	a.ClearQuota()
	s.recordAPIKeySuccess(a) // late in-flight success must not revive it
	if !a.Snapshot().Disabled {
		t.Fatal("key revived without manual enable")
	}
	if got := s.pool.AcquireWithOptions(context.Background(), auth.ProviderOpenAI, "client", "", "gpt-6-sol", "s", auth.AcquireOptions{AllowAPIKeyFallback: true}); got != nil {
		t.Fatal("last-resort scheduler selected disabled API key")
	}
	persisted, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := auth.ParseFile(p, persisted)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Snapshot().Disabled {
		t.Fatal("restart lost disabled state")
	}
}

func TestAPIKeySuccessBreaksFailureRun(t *testing.T) {
	s := &Server{}
	a := codexAPIKeyCred("relay")
	for i := 0; i < 2; i++ {
		s.reportCodexAPIKeyFault(a, 502, time.Time{})
	}
	s.recordAPIKeySuccess(a)
	for i := 0; i < 2; i++ {
		s.reportCodexAPIKeyFault(a, 502, time.Time{})
	}
	if a.Snapshot().Disabled {
		t.Fatal("non-consecutive errors disabled a working key")
	}
	s.reportCodexAPIKeyFault(a, 502, time.Time{})
	if !a.Snapshot().Disabled {
		t.Fatal("third consecutive failure did not disable key")
	}
}

func TestAPIKeyAttemptBudgetStopsOnlyBeforeOutput(t *testing.T) {
	ctx, _, finish := apiKeyAttemptContext(context.Background(), 20*time.Millisecond)
	defer finish()
	select {
	case <-ctx.Done():
		if !strings.Contains(context.Cause(ctx).Error(), "produced no output") {
			t.Fatal(context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("silent upstream was not bounded")
	}
	ctx2, commit, finish2 := apiKeyAttemptContext(context.Background(), 20*time.Millisecond)
	defer finish2()
	commit()
	select {
	case <-ctx2.Done():
		t.Fatal("committed output was canceled by pre-output budget")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAPIKeyNativeResponsesFailoverBeforeOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, failure := range []struct {
		name, body string
		status     int
	}{
		{"http", `{"error":{"message":"unavailable"}}`, 503},
		{"shed", "event: response.created\ndata: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"error\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"try another\"}}\n\n", 200},
		{"preamble EOF", "data: {\"type\":\"response.created\"}\n\n", 200},
	} {
		t.Run(failure.name, func(t *testing.T) {
			var failedHits, healthyHits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if r.Header.Get("Authorization") == "Bearer sk-relay-bad" {
					failedHits.Add(1)
					w.WriteHeader(failure.status)
					_, _ = w.Write([]byte(failure.body))
					return
				}
				healthyHits.Add(1)
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"))
			}))
			defer upstream.Close()
			bad, good := codexAPIKeyCred("bad"), codexAPIKeyCred("good")
			s := codexAPIKeyTestServer(upstream.URL)
			s.pool = auth.NewPool(nil, []*auth.Auth{bad, good}, time.Minute, false, "")
			body := []byte(`{"model":"gpt-6-sol","input":"OK","stream":true}`)
			c, w := newCodexContext(t, "/v1/responses", body)
			s.forwardWithFailover(c, auth.ProviderOpenAI, "/v1/responses", "gpt-6-sol", "client", "", "client", "s", body, true, time.Now())
			if failedHits.Load() != 1 || healthyHits.Load() != 1 {
				t.Fatalf("attempts bad=%d good=%d", failedHits.Load(), healthyHits.Load())
			}
			if !strings.Contains(w.Body.String(), "response.completed") || !strings.Contains(w.Body.String(), "OK") || strings.Contains(w.Body.String(), "server_is_overloaded") {
				t.Fatalf("failed to replace broken stream with healthy answer: %s", w.Body.String())
			}
		})
	}
}

func TestAPIKeyNativeNonStreamFailureIsRetried(t *testing.T) {
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"message":"no answer"}`)) }))
	defer u.Close()
	a := codexAPIKeyCred("bad")
	s := codexAPIKeyTestServer(u.URL, a)
	body := []byte(`{"model":"gpt-6-sol","input":"OK","stream":false}`)
	c, w := newCodexContext(t, "/v1/responses", body)
	retry, done := s.doForwardCodex(c, a, "/v1/responses", body, false, "gpt-6-sol", "client", "client", "s", time.Now(), 1)
	if !retry || done || w.Body.Len() != 0 {
		t.Fatalf("retry=%v done=%v body=%s", retry, done, w.Body.String())
	}
}

func TestAPIKeyModelRejectionSkipsOnlyThatModel(t *testing.T) {
	for _, status := range []int{400, 404, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var badHits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer sk-relay-limited" {
					badHits.Add(1)
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":{"code":"model_not_found","message":"No available channel for model"}}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":1}}`))
			}))
			defer upstream.Close()
			limited, good := codexAPIKeyCred("limited"), codexAPIKeyCred("good")
			s := codexAPIKeyTestServer(upstream.URL)
			s.pool = auth.NewPool(nil, []*auth.Auth{limited, good}, time.Minute, false, "")
			body := []byte(`{"model":"gpt-6-sol","input":"OK","stream":false}`)
			for i := 0; i < 2; i++ {
				c, w := newCodexContext(t, "/v1/responses", body)
				s.forwardWithFailover(c, auth.ProviderOpenAI, "/v1/responses", "gpt-6-sol", "client", "", "client", "s", body, false, time.Now())
				if w.Code != 200 || !strings.Contains(w.Body.String(), "completed") {
					t.Fatalf("response %d %s", w.Code, w.Body.String())
				}
			}
			if badHits.Load() != 1 {
				t.Fatalf("repeated known unsupported model: %d attempts", badHits.Load())
			}
			if limited.Snapshot().Disabled || !limited.IsHealthy() {
				t.Fatal("model rejection must not disable the entire key")
			}
			if limited.IsModelRateLimited(apiKeyModelScope("gpt-6-astra"), time.Now()) {
				t.Fatal("unrelated model was excluded")
			}
			limited.ClearQuota()
			if limited.IsModelRateLimited(apiKeyModelScope("gpt-6-sol"), time.Now()) {
				t.Fatal("manual quota clear did not restore model eligibility")
			}
		})
	}
}

func TestAPIKeyExhaustionPreservesRetryAfter(t *testing.T) {
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
	}))
	defer u.Close()
	a := codexAPIKeyCred("limited")
	s := codexAPIKeyTestServer(u.URL)
	s.pool = auth.NewPool(nil, []*auth.Auth{a}, time.Minute, false, "")
	body := []byte(`{"model":"gpt-6-sol","input":"OK"}`)
	c, w := newCodexContext(t, "/v1/responses", body)
	s.forwardWithFailover(c, auth.ProviderOpenAI, "/v1/responses", "gpt-6-sol", "client", "", "client", "s", body, false, time.Now())
	if w.Code != 429 || w.Header().Get("Retry-After") != "17" {
		t.Fatalf("status=%d retry-after=%s", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestAPIKeyNativeResponsesNeverReplaysCommittedOutput(t *testing.T) {
	var hits atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n"))
	}))
	defer u.Close()
	a, b := codexAPIKeyCred("first"), codexAPIKeyCred("second")
	s := codexAPIKeyTestServer(u.URL)
	s.pool = auth.NewPool(nil, []*auth.Auth{a, b}, time.Minute, false, "")
	body := []byte(`{"model":"gpt-6-sol","input":"OK","stream":true}`)
	c, w := newCodexContext(t, "/v1/responses", body)
	s.forwardWithFailover(c, auth.ProviderOpenAI, "/v1/responses", "gpt-6-sol", "client", "", "client", "s", body, true, time.Now())
	if hits.Load() != 1 || !strings.Contains(w.Body.String(), "partial answer") {
		t.Fatalf("committed answer replayed: hits=%d body=%s", hits.Load(), w.Body.String())
	}
}

func TestAPIKeyNativeNonStreamAggregatesUpstreamSSE(t *testing.T) {
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"))
	}))
	defer u.Close()
	a := codexAPIKeyCred("relay")
	s := codexAPIKeyTestServer(u.URL, a)
	body := []byte(`{"model":"gpt-6-sol","input":"OK","stream":false}`)
	c, w := newCodexContext(t, "/v1/responses", body)
	retry, done := s.doForwardCodex(c, a, "/v1/responses", body, false, "gpt-6-sol", "client", "client", "s", time.Now(), 1)
	if retry || !done || w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), "OK") || strings.Contains(w.Body.String(), "data:") {
		t.Fatalf("retry=%v done=%v status=%d body=%s", retry, done, w.Code, w.Body.String())
	}
}
