package server

import (
	"testing"

	"github.com/wjsoj/cc-core/usage"
)

// A response.completed from a turn that called image_generation, shaped like
// crack/codexapp0.147.0/rows/13-ws-server-events with the counters filled in.
const imageTurnCompleted = `{"type":"response.completed","response":{"id":"resp_x","status":"completed",` +
	`"usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":1000},"output_tokens":80,"output_tokens_details":{"reasoning_tokens":20}},` +
	`"tool_usage":{"image_gen":{"input_tokens":60,"input_tokens_details":{"image_tokens":0,"text_tokens":60},"output_tokens":1056,"output_tokens_details":{"image_tokens":1056,"text_tokens":0},"total_tokens":1116},"web_search":{"num_requests":0}}}}`

func TestCodexExtractorsBillImageGenToolUsage(t *testing.T) {
	for name, extract := range map[string]func([]byte) usage.Counts{
		"backend": extractCodexBackendUsageFromJSON,
		"openai":  extractOpenAIUsageFromJSON,
	} {
		c := extract([]byte(imageTurnCompleted))
		if c.InputTokens != 200 || c.CacheReadTokens != 1000 || c.OutputTokens != 80 {
			t.Fatalf("%s: chat usage changed: %+v", name, c)
		}
		if c.ImageGenTextInputTokens != 60 || c.ImageGenImageInputTokens != 0 || c.ImageGenOutputTokens != 1056 || c.Requests != 1 {
			t.Fatalf("%s: image usage not extracted: %+v", name, c)
		}
	}
	if c := extractCodexBackendUsageFromJSON([]byte(`{"type":"response.output_text.delta","delta":"x"}`)); c != (usage.Counts{}) {
		t.Fatalf("non-terminal frame produced usage: %+v", c)
	}
}

func TestCodexWSTurnDeltaSettlesImagesPerTurn(t *testing.T) {
	var session, billed usage.Counts
	for i, imgOut := range []int64{1056, 0, 4160} {
		var turn usage.Counts
		mergeCodexUsage(&turn, usage.Counts{InputTokens: 100, OutputTokens: 10, Requests: 1, ImageGenOutputTokens: imgOut})
		turn.Requests = 1
		session.Add(turn)
		d := codexTurnDelta(session, billed)
		if d.ImageGenOutputTokens != imgOut {
			t.Fatalf("turn %d: billed %d image tokens, want %d", i, d.ImageGenOutputTokens, imgOut)
		}
		billed = session
		billed.Requests = 0
	}
}
