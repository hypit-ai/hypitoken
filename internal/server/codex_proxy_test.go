package server

import (
	"testing"
	"time"

	"github.com/wjsoj/cc-core/auth"
)

// The Codex API-key path now shares classifyUpstreamStatus with the Anthropic
// one (see upstream_health.go, which owns the exhaustive status table). Pin
// only the Codex-specific consequence here: which statuses roll back to the
// forward loop for another credential, and which are handed to the caller.
//
// 401/402/403 are retryable on this path even though they are the credential's
// own answer — on a relay, another key may well work, and the rejected key
// needs to leave rotation rather than be re-presented on the next request.
func TestCodexRetryableStatuses(t *testing.T) {
	for _, s := range []int{408, 429, 500, 502, 503, 504, 529, 599, 401, 402, 403} {
		if !classifyUpstreamStatus(s).retryable() {
			t.Errorf("status %d should be retryable (rotate to next credential)", s)
		}
	}
	for _, s := range []int{200, 201, 400, 404, 409, 422} {
		if classifyUpstreamStatus(s).retryable() {
			t.Errorf("status %d should be forwarded to the client, not retried", s)
		}
	}
}

// Regression for the dead-relay 502 incident: a Codex API-key relay returning
// 5xx must eventually stop being selected.
//
// This used to be enforced by having reportCodexAPIKeyFault apply a fixed 45s
// *quota* cooldown, because MarkFailure alone could not downgrade an API key
// and the quota flag was the only lever IsHealthy honoured. cc-core's circuit
// breaker replaced that workaround, so the assertion moves with it: the relay
// is paused after a run of failures (not on the first, so a one-off 502 no
// longer sidelines a working key), the pause reports as a quarantine rather
// than as a misleading "quota exceeded". Three failures also persist Disabled.
func TestCodexAPIKey5xxRunPausesRelay(t *testing.T) {
	s := &Server{}
	a := &auth.Auth{ID: "relay", Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI}

	if !a.IsHealthy() {
		t.Fatal("a fresh API-key credential should be healthy")
	}

	s.reportCodexAPIKeyFault(a, 502, time.Time{})
	if !a.IsHealthy() {
		t.Fatal("a single 502 must not sideline a relay that is otherwise working")
	}

	s.reportCodexAPIKeyFault(a, 502, time.Time{})
	s.reportCodexAPIKeyFault(a, 502, time.Time{})

	if a.IsHealthy() {
		t.Fatal("a run of 5xx must pause the relay so Acquire skips it")
	}
	until, strikes := a.QuarantineSnapshot()
	if until.IsZero() || strikes == 0 {
		t.Fatalf("expected a quarantine pause; got (%v, %d)", until, strikes)
	}
	if info := a.Snapshot(); !info.QuotaResetAt.IsZero() {
		t.Fatal("an upstream 5xx must no longer be reported to operators as a quota cooldown")
	}
	if a.IsQuarantined(until.Add(time.Second)) {
		t.Fatal("the pause must expire on its own")
	}
	if !a.Snapshot().Disabled || a.IsHealthy() {
		t.Fatal("three failures must leave the relay disabled after its cooldown expires")
	}
}

// A rejected key is definitive, so it leaves rotation on the first strike
// instead of being re-presented on every following request.
func TestCodexAPIKeyRejectionPausesImmediately(t *testing.T) {
	s := &Server{}
	a := &auth.Auth{ID: "relay", Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI}

	s.reportCodexAPIKeyFault(a, 401, time.Time{})

	if !a.IsQuarantined(time.Now()) {
		t.Fatal("a 401 must pause the channel on the first strike")
	}
	if a.IsHardFailed() {
		t.Fatal("even a rejection must stay self-healing for an API key")
	}
}
