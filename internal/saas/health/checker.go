// Package health verifies supported models and derives CLI-only channel health from real traffic.
package health

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wjsoj/CPA-Claude/internal/saas/db"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/mimicry"
	"github.com/wjsoj/cc-core/requestlog"
	ccstream "github.com/wjsoj/cc-core/stream"
)

const anthropicProbeModel = "claude-haiku-4-5-20251001"
const codexProbeModel = "gpt-5.6-sol"

type Checker struct {
	// PassiveAuthIDs identifies API-key channels that require passive health checks.
	PassiveAuthIDs []string
	DB             *db.DB
	Pool           *auth.Pool
	Interval       time.Duration
	// LogDir is the request-log directory; when set, OAuth health rows are
	// hydrated with the DurationMs of the most recent successful request
	// for the credential. Empty = latency reported as 0 for OAuth.
	LogDir  string
	mu      sync.Mutex
	running bool
}

func New(store *db.DB, pool *auth.Pool, interval time.Duration, logDir string) *Checker {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	return &Checker{DB: store, Pool: pool, Interval: interval, LogDir: logDir}
}

// Run starts the periodic checker. Cancel ctx to stop.
func (c *Checker) Run(ctx context.Context) {
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	c.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.RunOnce(ctx)
		}
	}
}

// RunOnce probes every credential once. Safe to call concurrently.
func (c *Checker) RunOnce(ctx context.Context) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	c.running = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.running = false; c.mu.Unlock() }()

	// Evict health rows for credentials that no longer exist in the live pool.
	// Without this, deleted credentials keep their last status forever and
	// /status keeps probing/showing them as ghost rows. Built once per cycle
	// — the pool's Status() snapshot is the source of truth.
	statuses := c.Pool.Status()
	liveIDs := make([]string, 0, len(statuses))
	for _, st := range statuses {
		liveIDs = append(liveIDs, st.Auth.ID)
	}
	if err := c.DB.PruneModelHealthExcept(ctx, liveIDs); err != nil {
		log.Warnf("health: prune stale auth rows: %v", err)
	}

	for _, st := range statuses {
		a := c.Pool.FindByID(st.Auth.ID)
		if a == nil {
			continue
		}
		if a.Snapshot().Disabled {
			// Keep historical evidence, remove only the stale current verdict.
			if _, err := c.DB.ExecContext(ctx, "DELETE FROM model_health WHERE auth_id=?", a.ID); err != nil {
				log.Warnf("health: remove disabled current row: %v", err)
			}
			continue
		}
		provider := auth.NormalizeProvider(a.Provider)
		models := probeModels(a, provider)
		if len(models) == 0 {
			continue
		}
		if err := c.DB.PruneModelHealthOtherModels(ctx, a.ID, models); err != nil {
			log.Warnf("health: prune stale rows for %s: %v", a.ID, err)
		}
		model := models[0]
		// OAuth credentials must not be probed — every synthetic probe is a
		// strong third-party-detection signal upstream. Derive status from
		// the pool's recorded health (populated by real proxy traffic) and
		// borrow latency from the most recent successful request log entry.
		if a.Kind == auth.KindOAuth {
			// For Codex OAuth, opportunistically refresh the wham/usage
			// snapshot before reading from the pool. Unlike the /responses
			// probe — which is a third-party-detection signal we must
			// avoid — wham/usage is the official portal endpoint used by
			// chatgpt.com itself, so polling it is safe and gives the
			// admin a live view even when no traffic is flowing.
			// Errors are swallowed: this is a best-effort background
			// refresh, and a transient chatgpt.com failure must not
			// taint the (separately maintained) /responses health view.
			if provider == auth.ProviderOpenAI {
				probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				if _, err := a.FetchCodexUsage(probeCtx, c.Pool.UseUTLS()); err != nil {
					log.Debugf("health: codex wham/usage probe %s: %v", a.ID, err)
				}
				cancel()
			}
			c.recordOAuthFromPool(ctx, a, provider, model)
			continue
		}
		for _, model := range models {
			if ctx.Err() != nil {
				return
			}
			if c.isPassive(a.ID) {
				c.recordPassiveTraffic(ctx, a, provider, model)
			} else {
				c.checkOne(ctx, a, provider, model)
			}
		}
	}
}

// recordOAuthFromPool writes a ModelHealth row for an OAuth credential
// without making any upstream request. Status comes from the pool's
// HealthSnapshot (set by real proxy traffic via MarkSuccess/MarkFailure);
// latency comes from the DurationMs of the most recent successful entry
// in the request log for this credential, or 0 if none is available.
func (c *Checker) recordOAuthFromPool(ctx context.Context, a *auth.Auth, provider, model string) {
	healthy, hardFail, reason, _ := a.HealthSnapshot()
	snap := a.Snapshot()
	st := "ok"
	errMsg := ""
	switch {
	case snap.Disabled:
		st = "fail"
		errMsg = "disabled"
	case !snap.QuotaExceededAt.IsZero():
		st = "fail"
		errMsg = "quota exceeded"
	case hardFail:
		st = "fail"
		if reason != "" {
			errMsg = reason
		} else {
			errMsg = "hard failure"
		}
	case !healthy:
		st = "fail"
		if reason != "" {
			errMsg = reason
		} else {
			errMsg = "unhealthy"
		}
	}
	latency := c.recentSuccessLatencyMs(a.ID)
	rec := db.ModelHealth{
		AuthID:    a.ID,
		Provider:  provider,
		Model:     model,
		Status:    st,
		LatencyMs: latency,
		Error:     errMsg,
	}
	if err := c.DB.UpsertModelHealth(ctx, rec); err != nil {
		log.Warnf("health: upsert oauth %s/%s: %v", a.ID, model, err)
	}
	if err := c.DB.AppendModelHealthHistory(ctx, rec); err != nil {
		log.Warnf("health: history oauth %s/%s: %v", a.ID, model, err)
	}
	log.Infof("health passive %s/%s %s → %s (%dms) %s", a.ID, model, provider, st, latency, errMsg)
}

// recentSuccessLatencyMs returns the DurationMs of the most recent
// successful (HTTP < 400) request for this credential in the last 24h.
// Returns 0 when no log dir is configured or no successful entry exists.
func (c *Checker) recentSuccessLatencyMs(authID string) int {
	if c.LogDir == "" || authID == "" {
		return 0
	}
	res, err := requestlog.Query(requestlog.Filter{
		Dir:    c.LogDir,
		AuthID: authID,
		From:   time.Now().Add(-24 * time.Hour),
		Limit:  50,
	})
	if err != nil {
		return 0
	}
	for _, r := range res.Entries {
		if r.Status < 400 && r.Error == "" && r.DurationMs > 0 {
			return int(r.DurationMs)
		}
	}
	return 0
}

// Refresh kicks off RunOnce in the background.
func (c *Checker) Refresh() { go c.RunOnce(context.Background()) }

func (c *Checker) checkOne(ctx context.Context, a *auth.Auth, provider, model string) {
	t := time.Now()
	st, errMsg := c.probe(ctx, a, provider, model)
	latency := int(time.Since(t).Milliseconds())
	rec := db.ModelHealth{
		AuthID:    a.ID,
		Provider:  provider,
		Model:     model,
		Status:    st,
		LatencyMs: latency,
		Error:     errMsg,
	}
	if err := c.DB.UpsertModelHealth(ctx, rec); err != nil {
		log.Warnf("health: upsert %s/%s: %v", a.ID, model, err)
	}
	if err := c.DB.AppendModelHealthHistory(ctx, rec); err != nil {
		log.Warnf("health: history %s/%s: %v", a.ID, model, err)
	}
	log.Infof("health probe %s/%s %s → %s (%dms) %s", a.ID, model, provider, st, latency, errMsg)
}

// probe validates the whole small response, including termination and observed usage.
func (c *Checker) probe(ctx context.Context, a *auth.Auth, provider, model string) (string, string) {
	snap := a.Snapshot()
	cli := auth.ClientFor(snap.ProxyURL, false)
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	upstreamModel, ok := a.ResolveUpstreamModel(model)
	if !ok {
		return "fail", "model is not enabled on this channel"
	}
	switch provider {
	case auth.ProviderAnthropic:
		return c.probeAnthropic(ctx, cli, a, upstreamModel)
	case auth.ProviderOpenAI:
		return c.probeOpenAI(ctx, cli, a, upstreamModel)
	default:
		return "fail", "unknown provider"
	}
}

func (c *Checker) probeAnthropic(ctx context.Context, cli *http.Client, a *auth.Auth, model string) (string, string) {
	base := strings.TrimRight(a.Snapshot().BaseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	body := map[string]any{"model": model, "max_tokens": 32, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "Reply with exactly OK."}}}
	return sendProbe(ctx, cli, a, auth.ProviderAnthropic, base+"/v1/messages", body)
}

func (c *Checker) probeOpenAI(ctx context.Context, cli *http.Client, a *auth.Auth, model string) (string, string) {
	base := strings.TrimRight(a.Snapshot().BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	body := map[string]any{
		"model": model, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply with exactly OK."}}}},
		"max_output_tokens": 256, "reasoning": map[string]any{"effort": "low"}, "stream": true, "store": false,
	}
	// Use exactly the production URL rule. Bare origins still need /v1.
	return sendProbe(ctx, cli, a, auth.ProviderOpenAI, mimicry.JoinCodexAPIKeyUpstreamURL(base, "/v1/responses"), body)
}

func sendProbe(ctx context.Context, cli *http.Client, a *auth.Auth, provider, url string, body map[string]any) (string, string) {
	payload, err := json.Marshal(body)
	if err != nil {
		return "fail", err.Error()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "fail", err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "HypiToken-Health/1.0")
	token, _ := a.Credentials()
	if provider == auth.ProviderAnthropic {
		req.Header.Set("x-api-key", token)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "fail", err.Error()
	}
	defer resp.Body.Close()
	ccstream.Decompress(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "fail", fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := validateProbeResponse(resp.Body, provider); err != nil {
		return "fail", err.Error()
	}
	return "ok", ""
}
