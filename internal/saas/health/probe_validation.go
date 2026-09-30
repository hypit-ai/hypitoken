package health

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wjsoj/CPA-Claude/internal/saas/db"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/requestlog"
)

// Each configured model is independent. Do not infer Opus availability from Haiku.
func probeModels(a *auth.Auth, provider string) []string {
	snap := a.Snapshot()
	if snap.Disabled {
		return nil
	}
	if a.Kind == auth.KindAPIKey && len(snap.AllowedModels) > 0 {
		return slices.Clone(snap.AllowedModels)
	}
	switch provider {
	case auth.ProviderAnthropic:
		return []string{anthropicProbeModel}
	case auth.ProviderOpenAI:
		return []string{codexProbeModel}
	default:
		return nil
	}
}

func (c *Checker) isPassive(id string) bool { return slices.Contains(c.PassiveAuthIDs, id) }

// CLI-only relays must be judged by actual client requests, not synthetic CLI headers.
func (c *Checker) recordPassiveTraffic(ctx context.Context, a *auth.Auth, provider, model string) {
	rec := db.ModelHealth{AuthID: a.ID, Provider: provider, Model: model, Status: "unknown", Error: "no recent completed request for this model"}
	if c.LogDir != "" {
		res, err := requestlog.Query(requestlog.Filter{Dir: c.LogDir, AuthID: a.ID, Model: model, From: time.Now().Add(-30 * time.Minute), Limit: 50, Dims: requestlog.DimSummary})
		if err != nil {
			log.Warnf("health: passive query %s: %v", a.ID, err)
			return
		}
		for _, r := range res.Entries {
			if r.Status == 499 || strings.Contains(r.Error, "client cancel") || strings.Contains(r.Path, "#advisor:") || r.Error == "upstream requires official Claude Code CLI" {
				continue
			}
			rec.Status, rec.Error = "fail", r.Error
			if r.Status >= 200 && r.Status < 300 && r.Error == "" {
				rec.Status, rec.Error = "ok", ""
			}
			if rec.Status == "fail" && rec.Error == "" {
				rec.Error = fmt.Sprintf("HTTP %d", r.Status)
			}
			rec.LatencyMs = int(r.DurationMs)
			break
		}
	}
	if err := c.DB.UpsertModelHealth(ctx, rec); err != nil {
		log.Warnf("health: passive upsert: %v", err)
	}
	// No traffic is unknown, never a synthetic failure in the uptime history.
	if rec.Status != "unknown" {
		if err := c.DB.AppendModelHealthHistory(ctx, rec); err != nil {
			log.Warnf("health: passive history: %v", err)
		}
	}
	log.Infof("health passive %s/%s %s → %s (%dms) %s", a.ID, model, provider, rec.Status, rec.LatencyMs, rec.Error)
}

type probeEnvelope struct {
	Type       string                     `json:"type"`
	Status     string                     `json:"status"`
	StopReason *string                    `json:"stop_reason"`
	Error      json.RawMessage            `json:"error"`
	Usage      map[string]json.RawMessage `json:"usage"`
	Response   *probeEnvelope             `json:"response"`
	Message    *probeEnvelope             `json:"message"`
}

func validateProbeResponse(body io.Reader, provider string) error {
	const limit = 2 << 20
	br := bufio.NewReader(io.LimitReader(body, limit))
	// Detect JSON without trusting Content-Type; relays sometimes mislabel SSE.
	for {
		b, err := br.Peek(1)
		if err != nil {
			return fmt.Errorf("empty probe response: %w", err)
		}
		if !strings.ContainsRune(" \r\n\t", rune(b[0])) {
			break
		}
		_, _ = br.ReadByte()
	}
	var input, output bool
	observe := func(u map[string]json.RawMessage) {
		for _, key := range []string{"input_tokens", "prompt_tokens"} {
			if validTokenCount(u[key]) {
				input = true
			}
		}
		for _, key := range []string{"output_tokens", "completion_tokens"} {
			if validTokenCount(u[key]) {
				output = true
			}
		}
	}
	check := func(e probeEnvelope) error {
		if len(e.Error) > 0 && string(e.Error) != "null" {
			return fmt.Errorf("upstream error in probe response")
		}
		return nil
	}
	if first, _ := br.Peek(1); first[0] == '{' {
		var e probeEnvelope
		if err := json.NewDecoder(br).Decode(&e); err != nil {
			return fmt.Errorf("invalid probe JSON: %w", err)
		}
		if err := check(e); err != nil {
			return err
		}
		observe(e.Usage)
		complete := e.Status == "completed"
		if provider == auth.ProviderAnthropic {
			complete = e.Type == "message" && e.StopReason != nil && *e.StopReason != ""
		}
		if !complete || !input || !output {
			return fmt.Errorf("probe response missing completion or usage")
		}
		return nil
	}
	scanner := bufio.NewScanner(br)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data []string
	consume := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "[DONE]" {
			return false, fmt.Errorf("probe ended without a protocol completion event")
		}
		var e probeEnvelope
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			return false, fmt.Errorf("invalid probe event: %w", err)
		}
		if err := check(e); err != nil {
			return false, err
		}
		if e.Type == "error" || e.Type == "response.failed" || e.Type == "response.incomplete" {
			return false, fmt.Errorf("probe terminal event: %s", e.Type)
		}
		observe(e.Usage)
		if e.Message != nil {
			observe(e.Message.Usage)
		}
		if e.Response != nil {
			if err := check(*e.Response); err != nil {
				return false, err
			}
			observe(e.Response.Usage)
		}
		done := (provider == auth.ProviderOpenAI && e.Type == "response.completed") || (provider == auth.ProviderAnthropic && e.Type == "message_stop")
		if done && (!input || !output) {
			return false, fmt.Errorf("completed probe missing usage")
		}
		return done, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			done, err := consume()
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("probe stream read: %w", err)
	}
	if done, err := consume(); err != nil {
		return err
	} else if done {
		return nil
	}
	return fmt.Errorf("probe stream ended before completion")
}

func validTokenCount(raw json.RawMessage) bool {
	var n int64
	return len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &n) == nil && n >= 0
}
