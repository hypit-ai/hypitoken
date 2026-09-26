package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/usage"
)

func TestAPIKeyFaultsBackOffWithoutPersistingDisable(t *testing.T) {
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
	oldStart := time.Now()
	for i := 0; i < 3; i++ {
		s.reportCodexAPIKeyFault(a, 502, time.Time{})
	}
	first, strikes := a.QuarantineSnapshot()
	if strikes != 1 || time.Until(first) < 7*time.Second {
		t.Fatal("missing initial backoff")
	}
	s.recordAPIKeySuccess(a, oldStart)
	if a.HealthState().State != auth.HealthCooling {
		t.Fatal("late success cleared the pause")
	}
	if release, _ := s.beginAPIKeyAttempt(a); release != nil {
		t.Fatal("paused channel admitted")
	}
	persisted, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := auth.ParseFile(p, persisted)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Snapshot().Disabled || a.Snapshot().Disabled {
		t.Fatal("transient failure persisted a manual disable")
	}
	a.IsQuarantined(first.Add(time.Second))
	release, _ := s.beginAPIKeyAttempt(a)
	if release == nil {
		t.Fatal("pause expiry did not permit a probe")
	}
	if extra, _ := s.beginAPIKeyAttempt(a); extra != nil {
		t.Fatal("concurrent recovery probe admitted")
	}
	s.reportCodexAPIKeyFault(a, 502, time.Time{})
	release()
	second, strikes := a.QuarantineSnapshot()
	if strikes != 2 || time.Until(second) < 23*time.Second {
		t.Fatal("failed probe did not increase backoff")
	}
	a.IsQuarantined(second.Add(time.Second))
	release, _ = s.beginAPIKeyAttempt(a)
	if release == nil {
		t.Fatal("second probe not admitted")
	}
	s.recordAPIKeySuccess(a, time.Now())
	release()
	if a.HealthState().State != auth.HealthHealthy {
		t.Fatal("successful probe did not recover")
	}
	for i := 0; i < 3; i++ {
		s.reportCodexAPIKeyFault(a, 502, time.Time{})
	}
	_, strikes = a.QuarantineSnapshot()
	if strikes != 1 {
		t.Fatal("recovery did not reset backoff")
	}
}

func TestAPIKeyManualDisableSurvivesSuccessAndExpiry(t *testing.T) {
	s := &Server{}
	a := codexAPIKeyCred("manual")
	for i := 0; i < 3; i++ {
		s.reportCodexAPIKeyFault(a, 502, time.Time{})
	}
	a.SetDisabled(true)
	a.IsQuarantined(time.Now().Add(24 * time.Hour))
	s.recordAPIKeySuccess(a, time.Now())
	if release, _ := s.beginAPIKeyAttempt(a); release != nil || !a.Snapshot().Disabled {
		t.Fatal("manual disable was overridden")
	}
}

func TestAPIKeySuccessBreaksFailureRun(t *testing.T) {
	s := &Server{}
	a := codexAPIKeyCred("relay")
	for i := 0; i < 2; i++ {
		s.reportCodexAPIKeyFault(a, 502, time.Time{})
	}
	s.recordAPIKeySuccess(a, time.Now())
	for i := 0; i < 2; i++ {
		s.reportCodexAPIKeyFault(a, 502, time.Time{})
	}
	if a.IsQuarantined(time.Now()) {
		t.Fatal("non-consecutive failures paused a working key")
	}
	s.reportCodexAPIKeyFault(a, 502, time.Time{})
	if !a.IsQuarantined(time.Now()) || a.Snapshot().Disabled {
		t.Fatal("third failure must pause, not disable")
	}
}

func TestAPIKey429HonorsRetryAfterWithoutDoubleCounting(t *testing.T) {
	a := codexAPIKeyCred("limited")
	s := &Server{pool: auth.NewPool(nil, []*auth.Auth{a}, time.Minute, false, "")}
	until := time.Now().Add(17 * time.Second)
	s.reportCodexAPIKeyFault(a, 429, until)
	report := a.HealthState()
	if report.Consecutive429s != 1 || report.ConsecutiveFailures != 0 || report.QuarantineStrikes != 0 {
		t.Fatalf("429 counted as generic failure: %+v", report)
	}
	if release, wait := s.beginAPIKeyAttempt(a); release != nil || wait < 16*time.Second {
		t.Fatal("Retry-After bypassed")
	}
}

func TestAPIKeyPausedChannelCannotBeUsedAsLastResort(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(500) }))
	defer upstream.Close()
	a := codexAPIKeyCred("paused")
	s := codexAPIKeyTestServer(upstream.URL)
	s.pool = auth.NewPool(nil, []*auth.Auth{a}, time.Minute, false, "")
	for i := 0; i < 3; i++ {
		s.reportCodexAPIKeyFault(a, 502, time.Time{})
	}
	body := []byte(`{"model":"gpt-6-sol","input":"OK","stream":false}`)
	c, w := newCodexContext(t, "/v1/responses", body)
	s.forwardWithFailover(c, auth.ProviderOpenAI, "/v1/responses", "gpt-6-sol", "client", "", "client", "s", body, false, time.Now())
	if hits.Load() != 0 || w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("cooldown bypass: hits=%d status=%d retry=%s", hits.Load(), w.Code, w.Header().Get("Retry-After"))
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

func TestAPIKeyKeepaliveDoesNotCancelSilentAttemptBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.queued\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx, contentStarted, finish := apiKeyAttemptContext(parent, 3*time.Second)
	defer finish()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	c, w := newCodexContext(t, "/v1/responses", []byte(`{"stream":true}`))
	var counts usage.Counts
	var commits atomic.Int32
	result := streamSSECodexBackendWithContentStart(c, resp, &counts, func() { commits.Add(1) }, contentStarted)
	if commits.Load() != 1 || !strings.Contains(w.Body.String(), ":\n\n") {
		t.Fatalf("expected header-only keepalive: commits=%d body=%q", commits.Load(), w.Body.String())
	}
	if result.wroteAny {
		t.Fatal("keepalive was counted as real output")
	}
	if cause := context.Cause(ctx); cause == nil || !strings.Contains(cause.Error(), "produced no output") {
		t.Fatalf("keepalive canceled the silent attempt budget: %v", cause)
	}
}

func TestAPIKeyLatePreambleDoesNotDisarmTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		ctx, contentStarted, finish := apiKeyAttemptContext(parent, apiKeyPreOutputTimeout)
		defer finish()
		reader, writer := io.Pipe()
		defer reader.Close()
		go func() {
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\"}\n\n")
			time.Sleep(56 * time.Second)
			_, _ = io.WriteString(writer, "data: {\"type\":\"keepalive\"}\n\n")
			<-ctx.Done()
			_ = writer.CloseWithError(context.Cause(ctx))
		}()
		c, w := newCodexContext(t, "/v1/responses", nil)
		var counts usage.Counts
		start := time.Now()
		res := streamSSECodexBackendWithContentStart(c, &http.Response{Body: reader}, &counts, nil, contentStarted)
		if elapsed := time.Since(start); elapsed != 60*time.Second {
			t.Fatalf("silent attempt ended after %s, want 60s", elapsed)
		}
		if res.wroteAny || res.sawTerminal || strings.Contains(w.Body.String(), "data:") {
			t.Fatalf("late preamble foreclosed failover: %+v body=%q", res, w.Body.String())
		}
		if cause := context.Cause(ctx); cause == nil || !strings.Contains(cause.Error(), "produced no output") {
			t.Fatalf("silent attempt lost its timeout: %v", cause)
		}
	})
}

func TestAPIKeyOutputAfterThirtySecondsCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, contentStarted, finish := apiKeyAttemptContext(context.Background(), apiKeyPreOutputTimeout)
		defer finish()
		reader, writer := io.Pipe()
		defer reader.Close()
		go func() {
			defer writer.Close()
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\"}\n\n")
			time.Sleep(45 * time.Second)
			if ctx.Err() != nil {
				_ = writer.CloseWithError(context.Cause(ctx))
				return
			}
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n")
			time.Sleep(20 * time.Second)
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		}()
		c, w := newCodexContext(t, "/v1/responses", nil)
		var counts usage.Counts
		res := streamSSECodexBackendWithContentStart(c, &http.Response{Body: reader}, &counts, nil, contentStarted)
		if ctx.Err() != nil || res.err != nil || !res.sawTerminal || !res.wroteAny || !strings.Contains(w.Body.String(), "OK") || counts.OutputTokens != 1 {
			t.Fatalf("late output did not complete: context=%v result=%+v counts=%+v", context.Cause(ctx), res, counts)
		}
	})
}

func TestAPIKeyCompletionDoesNotWaitForUpstreamClose(t *testing.T) {
	c, w := newCodexContext(t, "/v1/responses", nil)
	body := io.MultiReader(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"), blockingReader{})
	resp := &http.Response{Body: io.NopCloser(body)}
	var counts usage.Counts
	done := make(chan codexStreamResult, 1)
	go func() { done <- streamSSECodexBackendWithContentStart(c, resp, &counts, func() {}, func() {}) }()
	select {
	case res := <-done:
		if !res.sawTerminal || !res.wroteAny || res.err != nil {
			t.Fatalf("incomplete result: %+v", res)
		}
		if !strings.HasSuffix(w.Body.String(), "\n\n") {
			t.Fatalf("terminal SSE frame missing separator: %q", w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("completed answer waited for upstream EOF")
	}
}

func TestAPIKeyFailoverTriesEveryEligibleChannel(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer sk-relay-last" {
			w.WriteHeader(502)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	var keys []*auth.Auth
	for i := 0; i < 13; i++ {
		keys = append(keys, codexAPIKeyCred(fmt.Sprint(i)))
	}
	keys = append(keys, codexAPIKeyCred("last"))
	s := codexAPIKeyTestServer(upstream.URL)
	s.pool = auth.NewPool(nil, keys, time.Minute, false, "")
	body := []byte(`{"model":"gpt-6-sol","input":"OK","stream":false}`)
	c, w := newCodexContext(t, "/v1/responses", body)
	s.forwardWithFailover(c, auth.ProviderOpenAI, "/v1/responses", "gpt-6-sol", "client", "", "client", "s", body, false, time.Now())
	if hits.Load() != 14 || w.Code != 200 || !strings.Contains(w.Body.String(), "OK") {
		t.Fatalf("failover stopped early: hits=%d status=%d body=%s", hits.Load(), w.Code, w.Body.String())
	}
}

func TestAPIKeyRecoveryGetsProbeWithoutBypassingRouting(t *testing.T) {
	good, bad := codexAPIKeyCred("good"), codexAPIKeyCred("recovering")
	s := &Server{pool: auth.NewPool(nil, []*auth.Auth{good, bad}, time.Minute, false, "")}
	for i := 0; i < 3; i++ {
		s.reportCodexAPIKeyFault(bad, 502, time.Time{})
	}
	until, _ := bad.QuarantineSnapshot()
	opts := auth.AcquireOptions{AllowAPIKeyFallback: true, APIKeyOnly: true}
	acquire := func(model string) *auth.Auth {
		return s.acquireWithAPIKeyRecovery(context.Background(), auth.ProviderOpenAI, "client", "", model, "session", opts)
	}
	if acquire("gpt-6-sol") != good {
		t.Fatal("cooling key displaced healthy channel")
	}
	bad.IsQuarantined(until.Add(time.Second))
	if acquire("gpt-6-sol") != bad {
		t.Fatal("healthy channel starved recovery probe")
	}
	release, _ := s.beginAPIKeyAttempt(bad)
	if release == nil {
		t.Fatal("probe not admitted")
	}
	if acquire("gpt-6-sol") != good {
		t.Fatal("concurrent request did not prefer healthy channel")
	}
	release()
	bad.SetAllowedModels([]string{"gpt-6-astra"})
	if acquire("gpt-6-sol") != good {
		t.Fatal("probe bypassed model allowlist")
	}
	bad.SetDisabled(true)
	if acquire("gpt-6-astra") != good {
		t.Fatal("probe bypassed manual disable")
	}
}

func TestAPIKeyHalfOpenProbeAdmissionIsAtomic(t *testing.T) {
	s := &Server{}
	a := codexAPIKeyCred("recovering")
	for i := 0; i < 3; i++ {
		s.reportCodexAPIKeyFault(a, 502, time.Time{})
	}
	until, _ := a.QuarantineSnapshot()
	a.IsQuarantined(until.Add(time.Second))
	var wg sync.WaitGroup
	releases := make(chan func(), 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, _ := s.beginAPIKeyAttempt(a); release != nil {
				releases <- release
			}
		}()
	}
	wg.Wait()
	close(releases)
	if len(releases) != 1 {
		t.Fatalf("admitted %d simultaneous probes", len(releases))
	}
	for release := range releases {
		release()
		release()
	}
	if release, _ := s.beginAPIKeyAttempt(a); release == nil {
		t.Fatal("neutral cancellation left the probe stuck")
	} else {
		release()
	}
}
