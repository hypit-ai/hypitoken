package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wjsoj/cc-core/auth"
)

func apiKeyTestCred(id string) *auth.Auth {
	return &auth.Auth{
		ID:          id,
		Kind:        auth.KindAPIKey,
		Provider:    "anthropic",
		Label:       id,
		AccessToken: "sk-ant-" + id,
	}
}

func TestStripAnthropicOAuthBetaForPreparationFallback(t *testing.T) {
	h := http.Header{"Anthropic-Beta": []string{"claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14"}}
	stripAnthropicOAuthBeta(h)
	got := h.Get("Anthropic-Beta")
	if strings.Contains(got, "oauth-") || !strings.Contains(got, "claude-code-20250219") || !strings.Contains(got, "interleaved-thinking-2025-05-14") {
		t.Fatalf("fallback beta sanitization = %q", got)
	}
}

// TestAPIKeyContractViolationIsWithheldAndRetried is the regression test for
// the silent-failure incident: a relay that has gone dead answers 200 with an
// HTML block page. Before the response-contract check the proxy marked the
// credential healthy, streamed the HTML through as a "successful" response,
// and billed zero tokens — the failure was only discoverable afterwards by
// noticing the credential had logged nothing but zero-token rows.
//
// The exchange must now be classified as an upstream fault: withheld from the
// client, retried on another credential, and surfaced as a 502 (not the
// upstream's misleading 200) if every credential is exhausted.
func TestAPIKeyContractViolationIsWithheldAndRetried(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>Access restricted</body></html>"))
	}))
	defer upstream.Close()

	cred := apiKeyTestCred("relayA")
	s := newDoForwardTestServer(t, upstream.URL, cred)
	c, w := newMessagesContext(t, haikuBody)

	retry, done, deferred := s.doForwardAnthropicAPIKey(c, cred, "/v1/messages", haikuBody, false,
		"claude-haiku-4-5-20251001", "tok-abcdef123456", "client", time.Now(), 1)

	if !retry || done {
		t.Fatalf("a 200-with-HTML must be retried on another credential; got retry=%v done=%v", retry, done)
	}
	if deferred == nil {
		t.Fatal("the violating response must be withheld as a deferred response")
	}
	if deferred.status != http.StatusBadGateway {
		t.Fatalf("deferred.status = %d, want 502 — the client must never be told the upstream's misleading 200", deferred.status)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("the HTML page must never reach the client; got %q", w.Body.String())
	}
	// The credential must be recorded as having failed, not as healthy.
	if _, _, _, consecutive := cred.HealthSnapshot(); consecutive != 1 {
		t.Fatalf("ConsecutiveFailures = %d, want 1 — the fault must count toward credential health", consecutive)
	}
}

// TestAPIKeyRepeatedFaultsPauseTheChannel is the end-to-end wiring proof for
// the circuit breaker: a relay that keeps answering 200-with-HTML must
// eventually be taken out of rotation, so traffic rotates onto another key
// instead of re-paying a doomed upstream round-trip on every single request.
// Three consecutive faults disable the channel until explicit manual recovery.
func TestAPIKeyRepeatedFaultsPauseTheChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>Access restricted</html>"))
	}))
	defer upstream.Close()

	cred := apiKeyTestCred("relayDead")
	s := newDoForwardTestServer(t, upstream.URL, cred)

	for i := 0; i < 3; i++ {
		c, _ := newMessagesContext(t, haikuBody)
		s.doForwardAnthropicAPIKey(c, cred, "/v1/messages", haikuBody, false,
			"claude-haiku-4-5-20251001", "tok-abcdef123456", "client", time.Now(), 1)
	}

	until, strikes := cred.QuarantineSnapshot()
	if until.IsZero() {
		t.Fatal("a relay that keeps returning HTML must be paused, not retried forever")
	}
	if strikes != 1 {
		t.Fatalf("QuarantineStrikes = %d, want 1 on the first pause", strikes)
	}
	if cred.IsHealthy() {
		t.Fatal("a paused channel must read unhealthy so the pool routes around it")
	}
	if cred.IsHardFailed() {
		t.Fatal("API-key retirement must use Disabled, not the OAuth hard-failure flag")
	}
	cred.IsQuarantined(until.Add(time.Second))
	if !cred.Snapshot().Disabled || cred.IsHealthy() {
		t.Fatal("cooldown expiry must not restore an automatically disabled channel")
	}
}

// TestAPIKeyClientFaultIsNotRetried proves the other half of the split: a
// request the *client* got wrong must be forwarded verbatim, never retried
// across the fleet, and must leave credential health untouched. Retrying a
// 400 on every key burns one upstream round-trip per credential to arrive at
// the identical error, and counting it toward health lets one broken caller
// degrade a channel that is serving everyone else correctly.
func TestAPIKeyClientFaultIsNotRetried(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))
	}))
	defer upstream.Close()

	cred := apiKeyTestCred("relayB")
	s := newDoForwardTestServer(t, upstream.URL, cred)
	c, w := newMessagesContext(t, haikuBody)

	retry, done, deferred := s.doForwardAnthropicAPIKey(c, cred, "/v1/messages", haikuBody, false,
		"claude-haiku-4-5-20251001", "tok-abcdef123456", "client", time.Now(), 1)

	if retry || !done {
		t.Fatalf("a client-side 400 must terminate the request; got retry=%v done=%v", retry, done)
	}
	if deferred != nil {
		t.Fatal("a client-side fault must not be withheld — the client needs to see its own error")
	}
	if w.Body.Len() == 0 {
		t.Fatal("the client error must be forwarded to the client")
	}
	if _, _, _, consecutive := cred.HealthSnapshot(); consecutive != 0 {
		t.Fatalf("ConsecutiveFailures = %d, want 0 — the client's own bad request must not dent credential health", consecutive)
	}
}

// TestAPIKeyUpstreamFaultCountsTowardHealth checks that a genuine upstream
// error does the opposite of the case above: it counts, and it retries.
func TestAPIKeyUpstreamFaultCountsTowardHealth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer upstream.Close()

	cred := apiKeyTestCred("relayC")
	s := newDoForwardTestServer(t, upstream.URL, cred)
	c, _ := newMessagesContext(t, haikuBody)

	retry, _, deferred := s.doForwardAnthropicAPIKey(c, cred, "/v1/messages", haikuBody, false,
		"claude-haiku-4-5-20251001", "tok-abcdef123456", "client", time.Now(), 1)

	if !retry || deferred == nil || deferred.status != http.StatusTooManyRequests {
		t.Fatalf("a 429 must be withheld and retried; got retry=%v deferred=%+v", retry, deferred)
	}
	if _, _, _, consecutive := cred.HealthSnapshot(); consecutive != 1 {
		t.Fatalf("ConsecutiveFailures = %d, want 1 — throttling is an upstream fault and must be visible to operators", consecutive)
	}
	// A single throttle counts once but does not reach the disable threshold.
	if _, hardFailed, _, _ := cred.HealthSnapshot(); hardFailed {
		t.Fatal("an API-key credential must never be auto-hard-failed")
	}
}
