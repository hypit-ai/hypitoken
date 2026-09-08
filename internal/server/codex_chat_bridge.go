package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wjsoj/cc-core/apicompat"
	"github.com/wjsoj/cc-core/codexerr"
	ccstream "github.com/wjsoj/cc-core/stream"
	"github.com/wjsoj/cc-core/usage"
)

// codex_chat_bridge.go is the transport half of the /v1/chat/completions bridge
// for Codex OAuth credentials. The protocol translation itself lives in
// cc-core's apicompat package — CPA-Claude has the same gap and cc-core already
// owns Codex request shaping (mimicry.SanitizeCodexRequestBody), so the mapping
// belongs there and only the gin/SSE plumbing stays here.

// streamCodexAsChatCompletions relays the backend's Responses SSE stream to the
// client as a chat.completion.chunk stream. Keepalive, write serialization,
// lazy commit and terminal tracking come from the same cc-core relay the native
// /v1/responses passthrough uses, so a bridged stream behaves identically under
// a slow upstream or an intermediary idle timeout.
//
// Takes an io.Reader rather than the *http.Response so both bridge callers can
// use it: the OAuth path hands over resp.Body directly, while the API-key path
// has already wrapped the body in a bufio.Reader to sniff SSE-vs-JSON
// (responseIsSSE peeks) and must pass that reader in — reading resp.Body afresh
// would drop whatever the sniff already buffered.
//
// commit is invoked once, immediately before the first byte reaches the client.
// Committing lazily is what lets a pre-output shed be withheld and failed over,
// exactly as on the native path; pass a closure that writes the SSE headers.
//
// A shed needs handling here more than anywhere else, because the translation
// erases it. apicompat.Translate has no case for {"type":"error"} — the frame
// produces no chat frames at all — and the response.failed that follows renders
// as an ordinary finish frame with finish_reason "stop". Left alone, a shed
// therefore reaches the client as a well-formed, successful-looking, EMPTY
// completion: the model appears to have returned nothing and stopped normally,
// with no error, no retry signal, and (since the relay does see a terminal
// event) no failover and no record anywhere. That is strictly worse than the
// native path, where the CLI at least sees the code and backs off.
func streamCodexAsChatCompletions(c *gin.Context, upstream io.Reader, counts *usage.Counts, model string, includeUsage bool, commit func()) codexStreamResult {
	flusher, _ := c.Writer.(http.Flusher)
	reader := newLineReader(upstream)
	st := apicompat.NewStreamState(model, includeUsage, time.Now().Unix())
	var out codexStreamResult

	// shedding latches when a capacity/quota frame arrives before any output.
	// From there the rest of the stream is suppressed — including the
	// response.failed that follows, which would otherwise translate into a
	// terminal finish frame and make the turn look cleanly complete — so the
	// relay ends with SawTerminal=false and WroteAny=false and the caller's
	// pre-output failover fires.
	shedding := false
	sentAny := false // whether we've handed Relay any bytes yet

	// One upstream event can expand into several chat frames (role + tool-call
	// header + delta) and most expand into none, so pending holds the overflow
	// and Next keeps its one-frame-per-call contract.
	var pending [][]byte
	next := func() (frame []byte, terminal bool, err error) {
		for {
			if len(pending) > 0 {
				frame, pending = pending[0], pending[1:]
				if !sentAny {
					out.firstOutputAt = time.Now()
				}
				sentAny = true
				return frame, apicompat.IsDoneFrame(frame), nil
			}
			line, rerr := reader.readLine()
			if len(line) > 0 {
				trim := bytes.TrimRight(line, "\r\n")
				if bytes.HasPrefix(trim, []byte("data:")) {
					payload := bytes.TrimSpace(trim[5:])
					if len(payload) > 0 && payload[0] == '{' {
						// Usage and classification are read off the raw upstream
						// event, not the translated frames, so billing stays
						// identical to the native /v1/responses path.
						counts.Add(extractCodexBackendUsageFromJSON(payload))

						if !shedding && codexerr.Classify(payload) == codexerr.ClassRetryable {
							// pending is provably empty here: the loop only
							// reaches readLine once it has drained it, so a
							// pre-output shed has nothing to unwind.
							if !sentAny {
								shedding = true
								out.shed = truncate(payload, 200)
							} else {
								// Output has already started, so there is no
								// failover left. Demotion — the fallback the
								// native paths use — buys nothing here either:
								// the code never reaches the client in the first
								// place, since Translate drops error frames. All
								// that can be done is say it happened.
								out.demoted.shed = true
								_, isCapacity := codexerr.DemoteCapacityCode(payload)
								out.demoted.capacity = isCapacity
							}
						}
						if !shedding {
							frames, _ := st.Translate(payload)
							pending = append(pending, frames...)
						}
					}
				}
			}
			if rerr != nil {
				if len(pending) > 0 && !shedding {
					continue
				}
				return nil, false, rerr
			}
		}
	}

	r := ccstream.Relay(c.Writer, func() {
		if flusher != nil {
			flusher.Flush()
		}
	}, ccstream.RelayOptions{
		Commit:           commit,
		KeepaliveIdle:    10 * time.Second,
		KeepalivePayload: []byte(":\n\n"),
		Next:             next,
	})
	out.sawTerminal = r.SawTerminal
	out.wroteAny = r.WroteAny
	out.err = r.Err
	return out
}

// chatStreamWantsUsage reports whether the client asked for the trailing
// usage-bearing chunk (stream_options.include_usage). Emitting it
// unconditionally breaks clients that assume every chunk carries a choice.
func chatStreamWantsUsage(body []byte) bool {
	var raw struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	return json.Unmarshal(body, &raw) == nil && raw.StreamOptions.IncludeUsage
}

// codexModelUnsupported reports whether an upstream 4xx is the backend saying
// it doesn't host this model, as opposed to a genuine client error.
//
// This is what keeps the bridge from turning a working request into a hard
// failure: relay-only model names (gpt-5.6-terra-high, gpt-4o-mini, …) now
// reach an OAuth credential first, and a model rejection must roll back to the
// API-key pool that does host them instead of surfacing a 400 the client can do
// nothing about.
func codexModelUnsupported(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		return false
	}
	lower := bytes.ToLower(body)
	for _, needle := range []string{
		"model_not_found", "does not exist", "unsupported model",
		"unknown model", "invalid model", "not supported", "no available channel",
	} {
		if bytes.Contains(lower, []byte(needle)) {
			return true
		}
	}
	return false
}
