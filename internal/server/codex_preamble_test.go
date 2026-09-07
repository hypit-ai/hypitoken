package server

import "testing"

// The window between response.created and the first output_text.delta spans the
// model's entire time-to-first-token, which on a loaded backend is where a
// capacity shed lands. Every event in it is a container declaration that a
// retry reproduces, so treating any of them as committed output throws away a
// recoverable turn and shows the user "Our servers are currently overloaded".
//
// Ordering is ground truth from crack/codexapp0.147.0/rows/13-ws-server-events.
func TestCodexPreambleCoversTheWholePreTextWindow(t *testing.T) {
	preText := []string{
		`{"type":"response.created"}`,
		`{"type":"response.in_progress"}`,
		`{"type":"response.output_item.added","item":{"id":"msg_1","type":"message"}}`,
		`{"type":"response.content_part.added","part":{"type":"output_text","text":""}}`,
	}
	for _, ev := range preText {
		if !codexPreambleEvent([]byte(ev)) {
			t.Errorf("%s carries no model text and must not commit the stream", ev)
		}
	}
}

// The converse matters just as much: once real text is out, a retry would
// duplicate it downstream, so these must commit.
func TestCodexPreambleExcludesAnythingUserVisible(t *testing.T) {
	visible := []string{
		`{"type":"response.output_text.delta","delta":"He"}`,
		`{"type":"response.output_text.done"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
		`{"type":"response.custom_tool_call_input.delta","delta":"{"}`,
		`{"type":"response.output_item.done"}`,
		`{"type":"response.completed"}`,
		`{"type":"response.failed"}`,
	}
	for _, ev := range visible {
		if codexPreambleEvent([]byte(ev)) {
			t.Errorf("%s would be withheld, but a retry after it duplicates output", ev)
		}
	}
}

// Anything unparseable or unknown commits, which is the safe default: holding
// back an event we do not understand risks swallowing real output.
func TestCodexPreambleFailsClosed(t *testing.T) {
	for _, ev := range []string{`not json`, `{}`, `{"type":"response.some_future_event"}`, ``} {
		if codexPreambleEvent([]byte(ev)) {
			t.Errorf("%q must not be withheld — unknown events have to commit", ev)
		}
	}
}
