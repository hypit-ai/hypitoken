package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/wjsoj/cc-core/auth"
)

const apiKeyDisableThreshold = 3
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

// API keys are operator-managed: three consecutive upstream faults disable
// the channel until an operator enables it. Persisting Disabled also prevents
// cc-core's last-resort scheduler and process restarts from reviving bad keys.
func (s *Server) recordAPIKeyFailure(a *auth.Auth, status int, resetAt time.Time, reason string) {
	s.apiKeyHealthMu.Lock()
	defer s.apiKeyHealthMu.Unlock()
	if a.Snapshot().Disabled {
		return
	}
	if status == http.StatusTooManyRequests && s.pool != nil {
		s.pool.ReportUpstreamError(a, status, resetAt)
	}
	if classifyUpstreamStatus(status) == faultCredential {
		a.MarkHardFailure(reason)
	} else {
		a.MarkFailure(reason)
	}
	_, _, _, consecutive := a.HealthSnapshot()
	if a.Kind != auth.KindAPIKey || consecutive < apiKeyDisableThreshold {
		return
	}
	a.SetDisabled(true)
	if err := a.Persist(); err != nil {
		log.Errorf("auth: could not persist disabled API key %s: %v", a.ID, err)
	}
	log.Warnf("auth: api-key %s disabled after %d consecutive upstream failures; manual enable required (%s)", a.ID, consecutive, reason)
}

func (s *Server) recordAPIKeySuccess(a *auth.Auth) {
	s.apiKeyHealthMu.Lock()
	defer s.apiKeyHealthMu.Unlock()
	// A late completion must not reset the evidence for an already-disabled key.
	if !a.Snapshot().Disabled {
		a.MarkSuccess()
	}
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
