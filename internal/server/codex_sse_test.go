package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wjsoj/cc-core/usage"
)

func TestCodexTerminalEvent(t *testing.T) {
	terminal := []string{
		`{"type":"response.completed","response":{"usage":{}}}`,
		`{"type":"response.failed"}`,
		`{"type":"response.incomplete"}`,
		`{"type":"response.cancelled"}`,
		`{"type":"response.canceled"}`,
	}
	for _, p := range terminal {
		if !codexTerminalEvent([]byte(p)) {
			t.Errorf("expected terminal event for %s", p)
		}
	}
	nonTerminal := []string{
		`{"type":"response.output_item.done"}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.created"}`,
		`not json`,
		``,
	}
	for _, p := range nonTerminal {
		if codexTerminalEvent([]byte(p)) {
			t.Errorf("did not expect terminal event for %q", p)
		}
	}
}

func newCodexStreamCtx() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return c, w
}

// A stream that EOFs without a terminal event is reported as truncated, but the
// bytes already received are still passed through to the client verbatim.
func TestStreamSSECodexBackendTruncated(t *testing.T) {
	body := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
	c, w := newCodexStreamCtx()
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	var counts usage.Counts
	if res := streamSSECodexBackend(c, resp, &counts, nil); res.sawTerminal {
		t.Error("stream without a terminal event should report sawTerminal=false")
	}
	if !strings.Contains(w.Body.String(), "response.output_text.delta") {
		t.Errorf("partial bytes must still reach the client, got: %q", w.Body.String())
	}
}

// A stream ending with response.completed is reported complete and forwarded
// verbatim.
func TestStreamSSECodexBackendCompleted(t *testing.T) {
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"
	c, w := newCodexStreamCtx()
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	var counts usage.Counts
	if res := streamSSECodexBackend(c, resp, &counts, nil); !res.sawTerminal {
		t.Error("stream ending in response.completed should report sawTerminal=true")
	}
	if !strings.Contains(w.Body.String(), "response.completed") {
		t.Errorf("terminal event must reach the client, got: %q", w.Body.String())
	}
	if counts.OutputTokens != 5 || counts.InputTokens != 10 {
		t.Errorf("usage must be extracted from response.completed, got in=%d out=%d", counts.InputTokens, counts.OutputTokens)
	}
}

// TestClientCancelIsNotUpstreamTruncation pins the distinction that a
// production log audit showed was being lost: a stream ending without a
// terminal event has two very different causes, and only one of them is a
// fault.
//
// Codex CLI aborts the in-flight request when the user hits Ctrl-C / ESC.
// That cancels the request context and surfaces to the relay as a read error
// — byte-for-byte the same signal as an upstream that hung up. Before the
// context was consulted, every such cancellation was recorded as "stream
// truncated before terminal event": ~90% of all Codex errors in production
// were ordinary user behaviour wearing an upstream-incident label, which
// buried the ~0.05% of real h2 truncations underneath them.
func TestClientCancelIsNotUpstreamTruncation(t *testing.T) {
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name           string
		ctx            context.Context
		err            error
		wantDisconnect bool
	}{
		{
			name:           "client hung up mid-stream",
			ctx:            cancelledCtx,
			err:            context.Canceled,
			wantDisconnect: true,
		},
		{
			// The upstream really did go away: the context is still live, so
			// this stays an upstream truncation and keeps its warning.
			name:           "upstream h2 truncation",
			ctx:            context.Background(),
			err:            errors.New("stream error: stream ID 5; INTERNAL_ERROR; received from peer"),
			wantDisconnect: false,
		},
		{
			// A bare context.Canceled with a live context means the transport
			// aborted on its own — an upstream fault, not the client leaving.
			name:           "transport-level cancel with live context",
			ctx:            context.Background(),
			err:            context.Canceled,
			wantDisconnect: false,
		},
		{
			name:           "clean end of stream",
			ctx:            cancelledCtx,
			err:            nil,
			wantDisconnect: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClientDisconnect(tc.ctx, tc.err); got != tc.wantDisconnect {
				t.Fatalf("isClientDisconnect = %v, want %v", got, tc.wantDisconnect)
			}
		})
	}
}

// Capacity shed arrives as an in-band `error` frame followed by
// `response.failed`, inside an otherwise-200 stream. Forwarding the error frame
// would (a) commit the response, foreclosing failover, and (b) reach the CLI as
// ApiError::ServerOverloaded, terminal for the session. Withhold it all instead
// so the caller can retry on another credential invisibly.
//
// Production shows this is worth the trouble: ~16% of Codex turns shed, and the
// rate is account-scoped (one account served 135 turns with zero sheds while
// another shed 27% of 97 in the same window), so another credential really can
// serve the turn.
func TestStreamSSECodexBackendShedsCapacityBeforeOutput(t *testing.T) {
	for _, code := range []string{"server_is_overloaded", "slow_down", "rate_limit_exceeded"} {
		t.Run(code, func(t *testing.T) {
			c, w := newCodexStreamCtx()
			body := "event: error\n" +
				`data: {"type":"error","error":{"code":"` + code + `","message":"nope"}}` + "\n\n" +
				"event: response.failed\n" +
				`data: {"type":"response.failed","response":{"error":{"code":"` + code + `"}}}` + "\n\n"
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

			committed := false
			var counts usage.Counts
			res := streamSSECodexBackend(c, resp, &counts, func() { committed = true })

			if committed {
				t.Error("commit() must not run — the response must stay uncommitted so failover works")
			}
			if res.wroteAny {
				t.Error("wroteAny must be false so the caller's pre-output failover fires")
			}
			if res.sawTerminal {
				t.Error("sawTerminal must be false — the withheld response.failed must not look like a clean end")
			}
			if res.shed == "" {
				t.Error("shed must carry the withheld frame for diagnostics")
			}
			if got := w.Body.String(); got != "" {
				t.Errorf("nothing may reach the client, got %q", got)
			}
		})
	}
}

// Once output has started there is no failover left, so the frame must be
// forwarded — but demoted out of the CLI's session-ending code set so it
// retries instead of giving up. The human-readable message must survive.
func TestStreamSSECodexBackendDemotesCapacityAfterOutput(t *testing.T) {
	c, w := newCodexStreamCtx()
	body := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n" +
		"event: error\n" +
		`data: {"type":"error","error":{"code":"server_is_overloaded","message":"we are full"}}` + "\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	var counts usage.Counts
	res := streamSSECodexBackend(c, resp, &counts, func() {})

	if !res.wroteAny {
		t.Fatal("wroteAny must be true — output already started")
	}
	if res.shed != "" {
		t.Error("must not report a pre-output shed once output has started")
	}
	if !res.demoted.shed || !res.demoted.capacity {
		t.Errorf("the post-output shed must be reported for the log; got %+v", res.demoted)
	}
	out := w.Body.String()
	if strings.Contains(out, "server_is_overloaded") {
		t.Errorf("the session-ending code must be demoted, got %q", out)
	}
	if !strings.Contains(out, "server_error") {
		t.Errorf("expected the demoted code in the output, got %q", out)
	}
	if !strings.Contains(out, "we are full") {
		t.Errorf("the upstream message must survive verbatim, got %q", out)
	}
}

// An error that is the request's own fault must pass through untouched, before
// output or after: retrying it on another credential would fail identically and
// the client needs the real reason.
func TestStreamSSECodexBackendLeavesFatalErrorsAlone(t *testing.T) {
	frame := `{"type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"blocked"}}`
	c, w := newCodexStreamCtx()
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("event: error\ndata: " + frame + "\n\n"))}

	var counts usage.Counts
	res := streamSSECodexBackend(c, resp, &counts, func() {})

	if res.shed != "" {
		t.Errorf("a fatal error is not a shed; got %q", res.shed)
	}
	if out := w.Body.String(); !strings.Contains(out, frame) {
		t.Errorf("frame must be forwarded verbatim:\n want %q\n  got %q", frame, out)
	}
}

// The real shape of a shed turn: upstream opens with response.created (and
// often response.in_progress), then refuses. Before the preamble buffer those
// openers committed the response, so the withhold could never fire — in
// production it triggered zero times while 52 sheds in the same window were
// forced onto the demote path. Buffering them keeps the response uncommitted
// long enough to fail over, and the client never sees the error at all.
func TestStreamSSECodexBackendShedsAfterPreambleOnly(t *testing.T) {
	c, w := newCodexStreamCtx()
	body := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.in_progress\n" +
		`data: {"type":"response.in_progress","response":{"id":"resp_1"}}` + "\n\n" +
		"event: error\n" +
		`data: {"type":"error","error":{"code":"server_is_overloaded","message":"we are full"}}` + "\n\n" +
		"event: response.failed\n" +
		`data: {"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}` + "\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	committed := false
	var counts usage.Counts
	res := streamSSECodexBackend(c, resp, &counts, func() { committed = true })

	if committed {
		t.Error("commit() must not run — a turn that only produced openers is still fully retryable")
	}
	if res.wroteAny {
		t.Error("wroteAny must be false so the caller's failover fires")
	}
	if res.shed == "" {
		t.Error("the withheld shed must be reported so the caller retries rather than gives up")
	}
	if got := w.Body.String(); got != "" {
		t.Errorf("the client must see nothing at all, got %q", got)
	}
}

// The buffer must not swallow anything on a healthy turn: the openers are
// released, in upstream's original order, as soon as real content arrives.
func TestStreamSSECodexBackendReleasesPreambleOnContent(t *testing.T) {
	c, w := newCodexStreamCtx()
	body := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}` + "\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	var counts usage.Counts
	res := streamSSECodexBackend(c, resp, &counts, func() {})

	if !res.sawTerminal {
		t.Error("a completed stream must still report sawTerminal")
	}
	out := w.Body.String()
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(out, want) {
			t.Errorf("%s must reach the client, got %q", want, out)
		}
	}
	if i, j := strings.Index(out, "response.created"), strings.Index(out, "response.output_text.delta"); i > j {
		t.Error("the buffered opener must be released before the content that flushed it")
	}
	if counts.InputTokens != 10 || counts.OutputTokens != 5 {
		t.Errorf("usage must survive the buffering: in=%d out=%d", counts.InputTokens, counts.OutputTokens)
	}
}

// A stream that ends with nothing but content-free frames buffered must leave
// the response UNCOMMITTED, so the forward loop can fetch a real answer from
// another credential.
//
// This reverses an earlier rule ("the buffered opener must not be swallowed on
// EOF"). Releasing it looked like the conservative choice and was the opposite:
// the opener committed the response, which foreclosed failover, and what
// reached the client was a `response.created` followed by a dead stream. In
// production that WAS the complaint — one 25-minute window put 101 of 373
// gpt-5.6-sol turns in this state, upstream closing the socket with
// `close 1000 (normal)` and no terminal event, while eight healthy accounts sat
// in the pool and were never asked.
//
// Nothing is lost by dropping it: a turn that genuinely finished emits
// response.completed, which is not content-free, so a complete response always
// has real content to flush the buffer ahead of.
func TestStreamSSECodexBackendWithholdsAPreambleOnlyTruncation(t *testing.T) {
	c, w := newCodexStreamCtx()
	body := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	var counts usage.Counts
	res := streamSSECodexBackend(c, resp, &counts, func() {})

	if res.shed != "" {
		t.Errorf("a plain truncation is not a shed; got %q", res.shed)
	}
	if res.wroteAny || w.Body.Len() > 0 {
		t.Errorf("a preamble-only truncation committed the response, foreclosing failover: %q", w.Body.String())
	}
}

// A non-streaming turn is shed the same way a streaming one is: an error frame
// inside an otherwise-200 stream. This path never looked for it, so aggregation
// just ran to EOF and reported "stream closed before response.completed" as a
// 502 the client had to see.
//
// Production made the gap plain: after the streaming relay learned to fail sheds
// over, the non-streaming route was 50x worse — 5.0% of non-streaming turns
// returned 5xx against 0.1% of streaming ones, and every one of those was a
// capacity shed (same model, ~2.3s, the shape of a refusal rather than a botched
// response).
func TestAggregateCodexResponseStreamReportsShed(t *testing.T) {
	for _, code := range []string{"server_is_overloaded", "slow_down", "rate_limit_exceeded"} {
		t.Run(code, func(t *testing.T) {
			body := "event: response.created\n" +
				`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
				"event: error\n" +
				`data: {"type":"error","error":{"code":"` + code + `","message":"nope"}}` + "\n\n"
			var counts usage.Counts
			out, shed, err := aggregateCodexResponseStream(strings.NewReader(body), &counts)

			if shed == "" {
				t.Error("a shed must be reported so the caller fails over instead of answering 502")
			}
			if err != nil {
				t.Errorf("a shed is not an aggregation error; got %v", err)
			}
			if out != nil {
				t.Errorf("no payload may be produced from a shed turn; got %q", out)
			}
		})
	}
}

// A genuinely truncated stream is still an error, not a shed — the two must stay
// distinguishable so the log says which happened.
func TestAggregateCodexResponseStreamTruncationIsNotShed(t *testing.T) {
	body := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n"
	var counts usage.Counts
	_, shed, err := aggregateCodexResponseStream(strings.NewReader(body), &counts)
	if shed != "" {
		t.Errorf("a truncation is not a shed; got %q", shed)
	}
	if err == nil {
		t.Error("a stream ending without response.completed must report an error")
	}
}

// The happy path must be untouched by the shed check.
func TestAggregateCodexResponseStreamStillAggregates(t *testing.T) {
	body := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r","output":[],"usage":{"input_tokens":7,"output_tokens":3}}}` + "\n\n"
	var counts usage.Counts
	out, shed, err := aggregateCodexResponseStream(strings.NewReader(body), &counts)
	if err != nil || shed != "" {
		t.Fatalf("clean stream must aggregate: shed=%q err=%v", shed, err)
	}
	if len(out) == 0 {
		t.Error("a completed stream must produce a payload")
	}
	if counts.InputTokens != 7 || counts.OutputTokens != 3 {
		t.Errorf("usage must be collected: in=%d out=%d", counts.InputTokens, counts.OutputTokens)
	}
}

// TestStreamSSECodexBackendTimesFirstOutput pins what firstOutputAt is measuring.
//
// The obvious reading — "when the response arrived" — is wrong on this backend
// and would report a number that is always near zero: upstream opens every turn
// with response.created immediately and only then goes away to think, which is
// exactly the interval worth measuring. The stamp has to land on the first
// CONTENT-bearing event, after the preamble.
func TestStreamSSECodexBackendTimesFirstOutput(t *testing.T) {
	c, _ := newCodexStreamCtx()
	body := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	before := time.Now()
	var counts usage.Counts
	res := streamSSECodexBackend(c, resp, &counts, func() {})
	after := time.Now()

	if res.firstOutputAt.IsZero() {
		t.Fatal("firstOutputAt must be stamped once real output is relayed")
	}
	if res.firstOutputAt.Before(before) || res.firstOutputAt.After(after) {
		t.Errorf("firstOutputAt = %v, outside the relay window [%v, %v]", res.firstOutputAt, before, after)
	}
}

// A withheld shed produced no output at all, so there is no first byte to
// report. Stamping one anyway would put the shed's own latency into the TTFB
// average of the turns that succeeded.
func TestStreamSSECodexBackendLeavesFirstOutputUnsetOnShed(t *testing.T) {
	c, _ := newCodexStreamCtx()
	body := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: error\n" +
		`data: {"type":"error","error":{"code":"server_is_overloaded","message":"nope"}}` + "\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	var counts usage.Counts
	res := streamSSECodexBackend(c, resp, &counts, func() {})
	if res.shed == "" {
		t.Fatal("precondition: the frame should have been withheld as a shed")
	}
	if !res.firstOutputAt.IsZero() {
		t.Errorf("firstOutputAt = %v on a turn that produced nothing; want zero", res.firstOutputAt)
	}
}

func TestUpstreamTTFBMillis(t *testing.T) {
	base := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	if got := upstreamTTFBMillis(base, base.Add(10500*time.Millisecond)); got != 10500 {
		t.Errorf("TTFB = %d, want 10500", got)
	}
	// An unmeasured turn must report "unknown", not "instant": rounding it to
	// zero would make every shed and every broken stream look like the fastest
	// requests in the archive.
	if got := upstreamTTFBMillis(base, time.Time{}); got != 0 {
		t.Errorf("unset firstOutputAt = %d, want 0", got)
	}
	if got := upstreamTTFBMillis(time.Time{}, base); got != 0 {
		t.Errorf("unset dispatchAt = %d, want 0", got)
	}
	// Timestamps from two different attempts would produce a negative gap;
	// clamp rather than write a nonsense duration into the archive.
	if got := upstreamTTFBMillis(base, base.Add(-time.Second)); got != 0 {
		t.Errorf("negative gap = %d, want 0", got)
	}
}

// The other door into the same fault the preamble flush opened: a stream that
// dies holding an unresolved `event:` line must not release it before anything
// has been committed.
//
// `held` only ever carries an `event:` line waiting for its `data:`, so what
// gets released is half an SSE event — malformed, empty of content, and enough
// to commit the response and foreclose the failover. In the fifteen minutes
// after the preamble fix shipped, 37 of 37 truncated production turns came
// through here: all zero-output, three credentials, three clients, every one
// logged as `committed by ""` because no data payload had been seen to name.
func TestOrphanedEventLineDoesNotCommitTheResponse(t *testing.T) {
	c, w := newCodexStreamCtx()
	// An event line whose data never arrives, then EOF.
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("event: response.output_text.delta\n"))}

	var counts usage.Counts
	res := streamSSECodexBackend(c, resp, &counts, func() {})

	if res.wroteAny || w.Body.Len() > 0 {
		t.Fatalf("an orphaned event line committed the response: %q", w.Body.String())
	}
	if res.sawTerminal {
		t.Error("no terminal event arrived; the turn must read as truncated")
	}
}

// Once real content is on the wire the failover is gone anyway, so a trailing
// unresolved event line is still passed on rather than silently dropped.
func TestOrphanedEventLineIsStillReleasedMidStream(t *testing.T) {
	c, w := newCodexStreamCtx()
	body := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		"event: response.output_text.delta\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	var counts usage.Counts
	res := streamSSECodexBackend(c, resp, &counts, func() {})

	if !res.wroteAny {
		t.Fatal("real content must still commit the response")
	}
	if got := strings.Count(w.Body.String(), "event: response.output_text.delta"); got != 2 {
		t.Fatalf("trailing event line was dropped from a committed stream: %q", w.Body.String())
	}
}

// A stream that ends because a fatal error frame was forwarded is upstream
// REJECTING the request, not upstream dying mid-turn. Recording both as
// "truncated" put clients' own bad requests in the same bucket as a broken
// backend — and at the end of an afternoon spent chasing truncation, every
// remaining one was a single client sending invalid_request_error six times
// out of six while the other 97 turns in the window had none.
func TestFatalErrorFrameIsNotRecordedAsATruncation(t *testing.T) {
	c, w := newCodexStreamCtx()
	body := "event: error\n" +
		`data: {"type":"error","error":{"code":"invalid_request_error","message":"bad"}}` + "\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	var counts usage.Counts
	res := streamSSECodexBackend(c, resp, &counts, func() {})

	if res.fatalCode != "invalid_request_error" {
		t.Fatalf("fatal code not recorded: %q", res.fatalCode)
	}
	if res.shed != "" {
		t.Errorf("a client-side rejection is not a capacity shed: %q", res.shed)
	}
	if !strings.Contains(w.Body.String(), "invalid_request_error") {
		t.Errorf("the client must see the real reason, got %q", w.Body.String())
	}
}

// An error frame the classifier calls fatal ends the turn. Forwarding it and
// then returning to the read loop left the stream OPEN with nothing further
// ever coming, until ReadTimeout expired — ten minutes on the production
// setting.
//
// Measured end to end against production: the frame
// ("X-OpenAI-Internal-Codex-Responses-Lite requires reasoning.context to be
// all_turns", status 400) arrived at 2s and the connection was still open when
// curl gave up at 60s. Users reported it as "ten minutes and no result"; the
// committed stall budget only ever capped it at four.
func TestFatalErrorFrameEndsTheStream(t *testing.T) {
	c, w := newCodexStreamCtx()
	// The error, then a reader that would block forever if anyone kept reading.
	body := io.MultiReader(
		strings.NewReader("event: error\n"+
			`data: {"type":"error","error":{"code":"unsupported_value","message":"bad"},"status":400}`+"\n\n"),
		blockingReader{},
	)
	resp := &http.Response{Body: io.NopCloser(body)}

	done := make(chan codexStreamResult, 1)
	var counts usage.Counts
	go func() { done <- streamSSECodexBackend(c, resp, &counts, func() {}) }()

	select {
	case res := <-done:
		if res.fatalCode != "unsupported_value" {
			t.Errorf("fatal code = %q, want unsupported_value", res.fatalCode)
		}
		if !strings.Contains(w.Body.String(), "unsupported_value") {
			t.Errorf("the client must still receive the real reason, got %q", w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the relay kept reading after a fatal error frame — this is the ten-minute hang")
	}
}

// blockingReader never returns, standing in for an upstream that sends nothing
// more and never closes.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }
