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

// Anything unparseable or of an unknown type commits, which is the safe
// default: holding back an event we do not understand risks swallowing real
// output.
func TestCodexPreambleFailsClosed(t *testing.T) {
	for _, ev := range []string{`not json`, `{"type":"response.some_future_event"}`, ``} {
		if codexPreambleEvent([]byte(ev)) {
			t.Errorf("%q must not be withheld — unknown events have to commit", ev)
		}
	}
}

// An event that declares no type at all is the exception to failing closed, and
// it earned the exception in production. Over the WebSocket a typeless frame
// renders as a bare `data:` line (codexws.appendSSEEvent writes no event line
// without a type), which fell straight through to the emit path and committed
// the response before response.created had arrived. The truncation log named
// the culprit `committed by ""`, and it accounted for 141 of 326 truncated
// streams in a seven-hour window: each one a turn the backend then parked, cut
// at the stall budget, with the failover already foreclosed.
//
// Withholding is safe because every event a client renders names itself, and
// the buffer is released in upstream's original order the moment any typed
// event arrives — or at codexPreOutputWithholdCap, or at end of stream.
func TestCodexPreambleWithholdsATypelessFrame(t *testing.T) {
	for _, ev := range []string{`{}`, `{"type":""}`, `{"sequence_number":3}`} {
		if !codexPreambleEvent([]byte(ev)) {
			t.Errorf("%s declares no type, so it cannot be model output — committing on it forecloses the failover a parked turn needs", ev)
		}
	}
}

// The keepalive is not part of the opening sequence — it is what upstream sends
// while a turn sits waiting for capacity, roughly every 30s — and it is where
// the pre-output failover was actually being lost. Every sampled production
// shed had the shape
//
//	response.created → response.in_progress → keepalive(31s) → error → response.failed
//
// so the heartbeat committed the stream a fraction of a second before the shed
// it was announcing, leaving the shed with nowhere to go but the client. The
// frame carries no text and no response id, so a retry reproduces the stream
// exactly.
func TestCodexPreambleWithholdsTheCapacityKeepalive(t *testing.T) {
	if !codexPreambleEvent([]byte(`{"type":"keepalive","sequence_number":2}`)) {
		t.Error("a keepalive carries no model text; committing on it forecloses the failover the shed needs")
	}
}
