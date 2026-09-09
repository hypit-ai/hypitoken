package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wjsoj/cc-core/codexws"

	"github.com/wjsoj/CPA-Claude/internal/config"
)

// TestStallBudgetStaysInsideTheWithholdCap pins the ordering the whole fix
// depends on. A stalled turn is converted into a shed only because nothing has
// reached the client yet, and nothing has reached the client only while the
// preamble is still buffered. Let the stall budget drift past the withhold cap
// and the buffer flushes first, wroteAny goes true, the shed branch is skipped
// and the hang comes straight back — silently, because every other assertion
// here would still pass.
func TestStallBudgetStaysInsideTheWithholdCap(t *testing.T) {
	var u config.CodexWSUpstreamConfig
	u.Normalize()

	if got := u.StallTimeout(); got >= codexPreOutputWithholdCap {
		t.Fatalf("stall budget %s >= withhold cap %s: a parked turn would commit its keepalives before the stream gave up on it",
			got, codexPreOutputWithholdCap)
	}
}

// TestStallBudgetDefaultsAndOptOut guards the two config edges: an unset value
// must arrive as the 90s default rather than as "disabled", and a negative one
// must genuinely disable the budget so the previous behaviour is reproducible
// without a code change.
func TestStallBudgetDefaultsAndOptOut(t *testing.T) {
	var unset config.CodexWSUpstreamConfig
	unset.Normalize()
	if got, want := unset.StallTimeoutSeconds, 120; got != want {
		t.Fatalf("default stall budget = %ds, want %ds", got, want)
	}

	off := config.CodexWSUpstreamConfig{StallTimeoutSeconds: -1}
	off.Normalize()
	if got := off.StallTimeout(); got != 0 {
		t.Fatalf("negative stall budget = %s, want 0 (disabled)", got)
	}
}

// TestPreambleClassifierIsTheStallPredicate is the cross-check for the comment
// at the SSEStream construction site. The stream aborts a parked turn assuming
// the relay withheld everything so far; that holds only while both read the
// same frames as content-free.
func TestPreambleClassifierIsTheStallPredicate(t *testing.T) {
	contentFree := []string{
		// The WebSocket-only frames first: these are what the HTTP transport
		// sends as response headers, and misclassifying codex.rate_limits as
		// content is what closed the withhold window on every WebSocket turn.
		`{"type":"codex.rate_limits","plan_type":"plus","rate_limits":{"allowed":true}}`,
		`{"type":"codex.response.metadata","headers":{"x-models-etag":"W/\"abc\""}}`,
		`{"type":"responsesapi.websocket_timing","engine":"gpt56sol-codex-a-c321"}`,
		`{"type":"response.created"}`,
		`{"type":"response.in_progress"}`,
		`{"type":"keepalive","sequence_number":7}`,
		`{"type":"response.output_item.added"}`,
		`{"type":"response.content_part.added"}`,
		`{"type":"response.reasoning_summary_part.added"}`,
	}
	for _, frame := range contentFree {
		if !codexPreambleEvent([]byte(frame)) {
			t.Errorf("%s: not content-free to the relay, so the stream must not abort on it either", frame)
		}
	}

	content := []string{
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.completed"}`,
	}
	for _, frame := range content {
		if codexPreambleEvent([]byte(frame)) {
			t.Errorf("%s: treated as content-free — the relay would withhold real output", frame)
		}
	}
}

// parkingConn is the backend that cost production ten hours: it accepts the
// turn, heartbeats forever, and schedules nothing. Every keepalive resets the
// per-frame read deadline, so no ReadTimeout can end this — only the stall
// budget can.
type parkingConn struct {
	mu       sync.Mutex
	deadline time.Time
	beats    int
	closed   bool
}

func (c *parkingConn) WriteJSON(any) error            { return nil }
func (c *parkingConn) WriteMessage(int, []byte) error { return nil }
func (c *parkingConn) Ping(time.Time) error           { return nil }
func (c *parkingConn) SetWriteDeadline(time.Time) error {
	return nil
}
func (c *parkingConn) HandshakeResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusSwitchingProtocols}
}

func (c *parkingConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	return nil
}

func (c *parkingConn) ReadMessage() (int, []byte, error) {
	c.mu.Lock()
	deadline := c.deadline
	c.beats++
	c.mu.Unlock()

	const beat = 40 * time.Millisecond
	if !deadline.IsZero() && time.Until(deadline) < beat {
		time.Sleep(time.Until(deadline))
		return 0, nil, stallTimeoutErr{}
	}
	time.Sleep(beat)
	return codexws.TextMessage, []byte(`{"type":"keepalive","sequence_number":1}`), nil
}

func (c *parkingConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

type stallTimeoutErr struct{}

func (stallTimeoutErr) Error() string   { return "i/o timeout" }
func (stallTimeoutErr) Timeout() bool   { return true }
func (stallTimeoutErr) Temporary() bool { return true }

// TestParkedTurnFailsOverInsteadOfHanging is the end-to-end regression. The
// production shape was: WebSocket dial succeeds, backend heartbeats, request
// sits ~600s on one credential, client gives up, attempts stays 1 and not one
// shed row is written. The turn must instead come back as a retryable
// pre-output shed with nothing committed to the client.
func TestParkedTurnFailsOverInsteadOfHanging(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &parkingConn{}
	cred := wsCred("parked-account")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)
	// One second rather than the 90s default: the behaviour under test is the
	// budget expiring, not how long it is.
	s.codexWSEgress.cfg.StallTimeoutSeconds = 1

	start := time.Now()
	w, retry, done := runCodexTurn(t, s, cred, wsTurnBody())
	elapsed := time.Since(start)

	if !retry || done {
		t.Fatalf("parked turn: retry=%v done=%v, want retry=true done=false — it has to move to another credential", retry, done)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("parked turn took %s to give up: the stall budget did not bound it", elapsed)
	}
	if body := w.Body.String(); body != "" {
		t.Fatalf("parked turn committed %q to the client; a withheld shed must leave the response untouched", body)
	}
	if conn.beats < 2 {
		t.Fatalf("only %d reads: the test never reached the parked state it is about", conn.beats)
	}
	if !conn.closed {
		t.Fatal("the parked socket was kept: it still has an unscheduled turn on it, and the next turn would read this one's leftovers")
	}
}

// TestParkedNonStreamingTurnFailsOver is the production shape the streaming
// test missed. Half the overnight hangs were `stream=false` on
// /v1/chat/completions: no first byte, 601s, one attempt. That path does not
// use the relay at all — it aggregates the whole SSE body first — so the stall
// budget has to reach it through the read error rather than through the
// relay's withhold latch.
func TestParkedNonStreamingTurnFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &parkingConn{}
	cred := wsCred("parked-nonstream")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)
	s.codexWSEgress.cfg.StallTimeoutSeconds = 1

	// /v1/responses, not chat/completions: the chat route is deliberately kept
	// off the WebSocket egress (see eligible), so routing this through it would
	// never dial the socket and the stall budget would never be consulted.
	body := []byte(`{"model":"gpt-5.5","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))

	start := time.Now()
	retry, done := s.doForwardCodexOAuth(c, cred, "/v1/responses", body, false, "gpt-5.5", "tok", "tester", "slot-2", time.Now(), 1)
	elapsed := time.Since(start)

	if !retry || done {
		t.Fatalf("parked non-streaming turn: retry=%v done=%v, want retry=true done=false", retry, done)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("parked non-streaming turn took %s: the stall budget did not bound it", elapsed)
	}
	// Without this the test passes for the wrong reason: a turn that never
	// reached the WebSocket also comes back retry=true, instantly, and would
	// report the budget working when it was never consulted.
	if conn.beats == 0 {
		t.Fatal("the turn never reached the parked socket, so nothing here was a test of the stall budget")
	}
	if elapsed < time.Second {
		t.Fatalf("gave up after %s, before the 1s budget could expire — something other than the stall budget ended this turn", elapsed)
	}
}

// rateLimitThenParkConn opens the way every real WebSocket turn does — with the
// frames the HTTP transport sends as response headers — and then parks.
type rateLimitThenParkConn struct {
	mu       sync.Mutex
	deadline time.Time
	n        int
}

func (c *rateLimitThenParkConn) WriteJSON(any) error              { return nil }
func (c *rateLimitThenParkConn) WriteMessage(int, []byte) error   { return nil }
func (c *rateLimitThenParkConn) Ping(time.Time) error             { return nil }
func (c *rateLimitThenParkConn) SetWriteDeadline(time.Time) error { return nil }
func (c *rateLimitThenParkConn) Close() error                     { return nil }
func (c *rateLimitThenParkConn) HandshakeResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusSwitchingProtocols}
}

func (c *rateLimitThenParkConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	return nil
}

func (c *rateLimitThenParkConn) ReadMessage() (int, []byte, error) {
	c.mu.Lock()
	deadline := c.deadline
	c.n++
	n := c.n
	c.mu.Unlock()

	const beat = 40 * time.Millisecond
	if !deadline.IsZero() && time.Until(deadline) < beat {
		time.Sleep(time.Until(deadline))
		return 0, nil, stallTimeoutErr{}
	}
	time.Sleep(beat)
	switch n {
	case 1:
		return codexws.TextMessage, []byte(`{"type":"codex.rate_limits","plan_type":"plus","rate_limits":{"allowed":true,"limit_reached":false}}`), nil
	case 2:
		return codexws.TextMessage, []byte(`{"type":"codex.response.metadata","headers":{"x-models-etag":"W/\"abc\""}}`), nil
	default:
		return codexws.TextMessage, []byte(`{"type":"keepalive","sequence_number":1}`), nil
	}
}

// TestRateLimitsFrameDoesNotForecloseFailover is the regression for the reason
// the pre-output withhold fired zero times over the WebSocket. codex.rate_limits
// is the transport's spelling of a response header, but cc-core rewrites and
// forwards it rather than dropping it, and forwarding is what commits the
// response. It arrived before response.created on every turn, so by the time
// the backend parked, failover had already been foreclosed and the turn could
// only end as a visible truncation — the same turns the HTTP path had always
// rescued in silence.
func TestRateLimitsFrameDoesNotForecloseFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &rateLimitThenParkConn{}
	cred := wsCred("ratelimit-then-park")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)
	s.codexWSEgress.cfg.StallTimeoutSeconds = 1

	w, retry, done := runCodexTurn(t, s, cred, wsTurnBody())

	if !retry || done {
		t.Fatalf("retry=%v done=%v, want retry=true done=false — a turn that produced nothing but headers must still be failed over", retry, done)
	}
	if body := w.Body.String(); body != "" {
		t.Fatalf("committed %q to the client before parking; the failover is no longer invisible", body)
	}
}

// metadataThenParkConn opens with the one frame cc-core drops whole, then
// parks. Both of that frame's lines are removed by the scrubber, leaving its
// blank terminator with nothing in front of it.
type metadataThenParkConn struct {
	mu       sync.Mutex
	deadline time.Time
	n        int
}

func (c *metadataThenParkConn) WriteJSON(any) error              { return nil }
func (c *metadataThenParkConn) WriteMessage(int, []byte) error   { return nil }
func (c *metadataThenParkConn) Ping(time.Time) error             { return nil }
func (c *metadataThenParkConn) SetWriteDeadline(time.Time) error { return nil }
func (c *metadataThenParkConn) Close() error                     { return nil }
func (c *metadataThenParkConn) HandshakeResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusSwitchingProtocols}
}

func (c *metadataThenParkConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	return nil
}

func (c *metadataThenParkConn) ReadMessage() (int, []byte, error) {
	c.mu.Lock()
	deadline := c.deadline
	c.n++
	n := c.n
	c.mu.Unlock()

	const beat = 40 * time.Millisecond
	if !deadline.IsZero() && time.Until(deadline) < beat {
		time.Sleep(time.Until(deadline))
		return 0, nil, stallTimeoutErr{}
	}
	time.Sleep(beat)
	if n == 1 {
		return codexws.TextMessage, []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"gAAAAA","x-models-etag":"W/\"abc\""}}`), nil
	}
	return codexws.TextMessage, []byte(`{"type":"keepalive","sequence_number":1}`), nil
}

// TestDroppedFrameTerminatorDoesNotCommit is the regression for the frame that
// turned out to be committing the response after codex.rate_limits stopped:
// none. cc-core drops codex.response.metadata whole, so its event and data
// lines both vanish — and the blank line that terminated them fell through and
// became the first byte written, closing the withhold window on its own.
//
// Production named it as `committed by ""`, an empty event type, which is
// exactly what an orphaned terminator looks like and is not something any
// amount of reading the frame list would have suggested.
func TestDroppedFrameTerminatorDoesNotCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &metadataThenParkConn{}
	cred := wsCred("metadata-then-park")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)
	s.codexWSEgress.cfg.StallTimeoutSeconds = 1

	w, retry, done := runCodexTurn(t, s, cred, wsTurnBody())

	if !retry || done {
		t.Fatalf("retry=%v done=%v, want retry=true done=false — a dropped frame's terminator must not commit the response", retry, done)
	}
	if body := w.Body.String(); body != "" {
		t.Fatalf("committed %q to the client — the orphaned blank line escaped", body)
	}
}

// typelessThenParkConn opens with a JSON frame that declares no `type` at all,
// then parks. Over the WebSocket such a frame renders as a bare `data:` line —
// codexws.appendSSEEvent writes no event line without a type — which is the
// shape that fell through the relay's classifier and committed the response.
type typelessThenParkConn struct {
	mu       sync.Mutex
	deadline time.Time
	n        int
}

func (c *typelessThenParkConn) WriteJSON(any) error              { return nil }
func (c *typelessThenParkConn) WriteMessage(int, []byte) error   { return nil }
func (c *typelessThenParkConn) Ping(time.Time) error             { return nil }
func (c *typelessThenParkConn) SetWriteDeadline(time.Time) error { return nil }
func (c *typelessThenParkConn) Close() error                     { return nil }
func (c *typelessThenParkConn) HandshakeResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusSwitchingProtocols}
}

func (c *typelessThenParkConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	return nil
}

func (c *typelessThenParkConn) ReadMessage() (int, []byte, error) {
	c.mu.Lock()
	deadline := c.deadline
	c.n++
	n := c.n
	c.mu.Unlock()

	const beat = 40 * time.Millisecond
	if !deadline.IsZero() && time.Until(deadline) < beat {
		time.Sleep(time.Until(deadline))
		return 0, nil, stallTimeoutErr{}
	}
	time.Sleep(beat)
	if n == 1 {
		return codexws.TextMessage, []byte(`{"sequence_number":0,"obfuscation":"aGVsbG8"}`), nil
	}
	return codexws.TextMessage, []byte(`{"type":"keepalive","sequence_number":1}`), nil
}

// TestTypelessFrameDoesNotCommit is the regression for the largest single
// source of truncated Codex streams the day the stall budget shipped: 141 of
// 326, all logged as `committed by ""`.
//
// A frame with no type cannot be model output, but the relay only recognised
// content-free events by name, so an unnamed one went straight to the client —
// committing the response before response.created had even arrived. The
// backend then parked the turn, the budget fired, and the failover it exists to
// enable had already been foreclosed by that first stray frame. The client saw
// a stream that stopped after two minutes.
func TestTypelessFrameDoesNotCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &typelessThenParkConn{}
	cred := wsCred("typeless-then-park")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)
	s.codexWSEgress.cfg.StallTimeoutSeconds = 1

	w, retry, done := runCodexTurn(t, s, cred, wsTurnBody())

	if !retry || done {
		t.Fatalf("retry=%v done=%v, want retry=true done=false — a frame with no type must not commit the response", retry, done)
	}
	if body := w.Body.String(); body != "" {
		t.Fatalf("committed %q to the client — the typeless frame escaped the withhold", body)
	}
}

// deltaThenLongParkConn produces one real content delta and then goes quiet for
// longer than the read timeout. It is the post-commit case: the client already
// has bytes, so there is no failover left to protect.
type deltaThenLongParkConn struct {
	mu       sync.Mutex
	deadline time.Time
	n        int
}

func (c *deltaThenLongParkConn) WriteJSON(any) error              { return nil }
func (c *deltaThenLongParkConn) WriteMessage(int, []byte) error   { return nil }
func (c *deltaThenLongParkConn) Ping(time.Time) error             { return nil }
func (c *deltaThenLongParkConn) SetWriteDeadline(time.Time) error { return nil }
func (c *deltaThenLongParkConn) Close() error                     { return nil }
func (c *deltaThenLongParkConn) HandshakeResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusSwitchingProtocols}
}

func (c *deltaThenLongParkConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	return nil
}

func (c *deltaThenLongParkConn) ReadMessage() (int, []byte, error) {
	c.mu.Lock()
	deadline := c.deadline
	c.n++
	n := c.n
	c.mu.Unlock()

	if n == 1 {
		time.Sleep(20 * time.Millisecond)
		return codexws.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hi"}`), nil
	}
	// Quieter than either budget from here on, so whichever deadline is armed
	// is the one that ends the turn — which is exactly what the test measures.
	if !deadline.IsZero() {
		time.Sleep(time.Until(deadline))
	}
	return 0, nil, stallTimeoutErr{}
}

// TestCommittedTurnIsNotCutByTheStallBudget is the other half of the same
// production day. The budget converts a parked turn into a failover, and the
// failover is gone at the first committed byte — so past that point firing can
// only turn a slow turn into a truncated one, which is strictly worse for the
// client than waiting.
//
// The two deadlines are deliberately far apart so the elapsed time names which
// one fired: the stall budget at 1s, the read timeout at 3s. An armed budget
// ends the turn at ~1s; a retired one lets the read timeout have it at ~3s.
func TestCommittedTurnIsNotCutByTheStallBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &deltaThenLongParkConn{}
	cred := wsCred("delta-then-long-park")
	s := wsEgressServer(t, "https://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}, nil
	}, cred)
	s.codexWSEgress.cfg.StallTimeoutSeconds = 1
	s.codexWSEgress.cfg.ReadTimeoutSeconds = 3

	began := time.Now()
	w, _, _ := runCodexTurn(t, s, cred, wsTurnBody())
	elapsed := time.Since(began)

	if body := w.Body.String(); !strings.Contains(body, "response.output_text.delta") {
		t.Fatalf("the delta never reached the client: %q", body)
	}
	if elapsed < 2*time.Second {
		t.Fatalf("the turn ended after %s — the stall budget was still armed past the commit, so a slow turn was cut into a truncated one for a failover that no longer existed", elapsed.Round(time.Millisecond))
	}
}
