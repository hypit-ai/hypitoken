package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wjsoj/cc-core/auth"
)

const apiKeyPreOutputTimeout = 30 * time.Second
const codexDeferredResponseContextKey = "codex_deferred_response"

func apiKeyModelScope(model string) string { return "apikey-model:" + model }

// Explicit model availability failures apply to this model, not to the key's
// ability to serve other models. Do not infer this from an arbitrary 400/503.
func apiKeyModelUnavailable(body []byte) bool {
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	switch payload.Error.Code {
	case "model_not_found", "model_not_available", "model_not_supported":
		return true
	}
	return false
}

// Upstream faults use cc-core's timed circuit breaker, never the operator's
// persistent Disabled switch. Failed recovery attempts retain their strikes
// and increase the pause; only a verified response resets the backoff.
func (s *Server) recordAPIKeyFailure(a *auth.Auth, status int, resetAt time.Time, reason string) {
	s.apiKeyHealthMu.Lock()
	defer s.apiKeyHealthMu.Unlock()
	if a.Snapshot().Disabled {
		return
	}
	defer func() {
		if !a.HealthState().Serving {
			if s.apiKeyRecovery == nil {
				s.apiKeyRecovery = make(map[*auth.Auth]bool)
			}
			s.apiKeyRecovery[a] = true
		}
	}()
	if status == http.StatusTooManyRequests && s.pool != nil {
		s.pool.ReportUpstreamError(a, status, resetAt)
		return // 429 has its own counter and Retry-After; do not count it twice.
	}
	if classifyUpstreamStatus(status) == faultCredential {
		a.MarkHardFailure(reason)
	} else {
		a.MarkFailure(reason)
	}
}

func (s *Server) recordAPIKeySuccess(a *auth.Auth, started time.Time) {
	s.apiKeyHealthMu.Lock()
	defer s.apiKeyHealthMu.Unlock()
	report := a.HealthState()
	// A request already in flight when the circuit opened is not a recovery
	// probe. Nor can any response override a manual disable.
	if report.State == auth.HealthDisabled || ((report.QuarantineStrikes > 0 || s.apiKeyRecovery[a]) && !started.After(report.LastFailure)) {
		return
	}
	a.MarkSuccess()
	delete(s.apiKeyRecovery, a)
}

// The shared pool may return a paused key as a last resort. Enforce its pause
// here even when it is the only channel, and allow only one half-open request
// at a time. The caller releases the lease on every completion, including
// cancellation and client/model errors, which are neutral health signals.
func (s *Server) beginAPIKeyAttempt(a *auth.Auth) (release func(), retryAfter time.Duration) {
	s.apiKeyHealthMu.Lock()
	defer s.apiKeyHealthMu.Unlock()
	report := a.HealthState()
	if !report.Serving {
		return nil, report.RetryAfter
	}
	if s.apiKeyProbes[a] {
		return nil, time.Second
	}
	if report.State != auth.HealthHalfOpen && !s.apiKeyRecovery[a] {
		return func() {}, 0
	}
	if s.apiKeyProbes == nil {
		s.apiKeyProbes = make(map[*auth.Auth]bool)
	}
	s.apiKeyProbes[a] = true
	return sync.OnceFunc(func() {
		s.apiKeyHealthMu.Lock()
		delete(s.apiKeyProbes, a)
		s.apiKeyHealthMu.Unlock()
	}), 0
}

// Offer a due recovery probe before ordinary traffic so a lower-priority key
// does not stay unverified forever while another healthy channel serves all
// requests. Selection still goes through the pool's provider/group/model gates.
func (s *Server) acquireWithAPIKeyRecovery(ctx context.Context, provider, clientToken, group, model, session string, opts auth.AcquireOptions) *auth.Auth {
	s.apiKeyHealthMu.Lock()
	due := make(map[string]bool)
	for a := range s.apiKeyRecovery {
		r := a.HealthState()
		if r.State == auth.HealthDisabled || r.State == auth.HealthHealthy || s.pool.FindByID(a.ID) != a {
			delete(s.apiKeyRecovery, a)
			continue
		}
		if r.Serving && !s.apiKeyProbes[a] && auth.NormalizeProvider(a.Provider) == provider && a.AcceptsModel(model) {
			due[a.ID] = true
		}
	}
	s.apiKeyHealthMu.Unlock()
	if len(due) > 0 {
		probeOpts := opts
		probeOpts.APIKeyOnly = true
		probeOpts.ExcludeIDs = append([]string(nil), opts.ExcludeIDs...)
		for id := range s.pool.LabelIndex() {
			if !due[id] {
				probeOpts.ExcludeIDs = append(probeOpts.ExcludeIDs, id)
			}
		}
		if a := s.pool.AcquireWithOptions(ctx, provider, clientToken, group, model, session, probeOpts); a != nil {
			return a
		}
	}
	return s.pool.AcquireWithOptions(ctx, provider, clientToken, group, model, session, opts)
}

// Bound one silent attempt, not an answer that is already streaming. The
// atomic state arbitrates commit vs timeout, so a racing timer cannot cancel
// a stream whose first content has already been committed.
func apiKeyAttemptContext(parent context.Context, budget time.Duration) (context.Context, func(), func()) {
	ctx, cancel := context.WithCancelCause(parent)
	var state atomic.Int32
	timer := time.AfterFunc(budget, func() {
		if state.CompareAndSwap(0, 1) {
			cancel(fmt.Errorf("API-key upstream produced no output within %s", budget))
		}
	})
	commit := func() {
		if state.CompareAndSwap(0, 2) {
			timer.Stop()
		}
	}
	return ctx, commit, func() { timer.Stop(); cancel(context.Canceled) }
}
