package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wjsoj/cc-core/usage"
)

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

func TestAggregateSignalsImageGenerationOnce(t *testing.T) {
	body := sse(
		`{"type":"response.created","response":{"id":"r"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"image_generation_call","id":"ig"}}`,
		`{"type":"response.image_generation_call.in_progress","output_index":0}`,
		`{"type":"response.image_generation_call.generating","output_index":0}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"image_generation_call","id":"ig","result":"AAAA"}}`,
		`{"type":"response.completed","response":{"id":"r","output":[],"usage":{"input_tokens":10,"output_tokens":5},"tool_usage":{"image_gen":{"input_tokens":20,"output_tokens":1056}}}}`,
	)
	calls := 0
	var counts usage.Counts
	out, shed, err := aggregateCodexResponseStream(strings.NewReader(body), &counts, func() { calls++ })
	if err != nil || shed != "" {
		t.Fatalf("err=%v shed=%q", err, shed)
	}
	if calls != 1 {
		t.Fatalf("onGenerating called %d times, want 1", calls)
	}
	if !bytes.Contains(out, []byte(`"result":"AAAA"`)) {
		t.Fatalf("image result lost: %s", out)
	}
	if counts.ImageGenOutputTokens != 1056 || counts.ImageGenTextInputTokens != 20 {
		t.Fatalf("image usage not billed: %+v", counts)
	}

	calls = 0
	if _, _, err := aggregateCodexResponseStream(strings.NewReader(sse(`{"type":"response.completed","response":{"id":"r","output":[]}}`)), &usage.Counts{}, func() { calls++ }); err != nil || calls != 0 {
		t.Fatalf("text turn triggered keepalive: calls=%d err=%v", calls, err)
	}
}
