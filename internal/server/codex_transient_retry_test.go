package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/codexws"
	"github.com/wjsoj/cc-core/usage"
)

func transientErrorEvent(code string, nested bool) string {
	if nested {
		return fmt.Sprintf(`data: {"type":"response.failed","response":{"status":"failed","error":{"code":%q,"message":"unexpected EOF"}}}`+"\n\n", code)
	}
	return fmt.Sprintf(`data: {"type":"error","error":{"code":%q,"message":"unexpected EOF"}}`+"\n\n", code)
}

// Exercise the real routing loop, not just the classification: a failed OAuth
// attempt must leave room for an API-key fallback to deliver the response.
func TestCodexTransientErrorRoutesToFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, code := range []string{"server_error", "stream_terminated"} {
		for _, nested := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/nested=%t/stream=%t", code, nested, stream), func(t *testing.T) {
					var oauthCalls, fallbackCalls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Header.Get("Authorization") == "Bearer test-token" {
							oauthCalls.Add(1)
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n"+transientErrorEvent(code, nested))
							return
						}
						fallbackCalls.Add(1)
						response := `{"id":"fallback_ok","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":2}}`
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":"+response+"}\n\n")
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, response)
						}
					}))
					defer upstream.Close()
					s := codexHTTPTestServer(upstream.URL, wsCred("transient-oauth"))
					s.cfg.OpenAIBaseURL = upstream.URL
					s.pool.AddAPIKey(codexAPIKeyCred("fallback"))
					body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","input":"hi","stream":%t}`, stream))
					c, w := newCodexContext(t, "/v1/responses", body)
					s.forwardWithFailover(c, auth.ProviderOpenAI, "/v1/responses", "gpt-5.6-sol", "test-client", "", "tester", "", body, stream, time.Now())
					if oauthCalls.Load() != 1 || fallbackCalls.Load() != 1 {
						t.Fatalf("routing calls: OAuth=%d fallback=%d; want one each", oauthCalls.Load(), fallbackCalls.Load())
					}
					if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fallback_ok") {
						t.Fatalf("fallback did not complete: %d %s", w.Code, w.Body.String())
					}
					if strings.Contains(w.Body.String(), "unexpected EOF") || strings.Contains(w.Body.String(), "response.failed") {
						t.Fatalf("failed attempt leaked downstream: %s", w.Body.String())
					}
				})
			}
		}
	}
}

func TestCodexTransientWSErrorAllowsRoutingRetry(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%t", nested), func(t *testing.T) {
			cred := wsCred("ws-transient")
			frame := strings.TrimSpace(strings.TrimPrefix(transientErrorEvent("server_error", nested), "data: "))
			conn := &egressConn{frames: []string{`{"type":"response.created"}`, frame}}
			s := wsEgressServer(t, "http://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
				return conn, conn.HandshakeResponse(), nil
			}, cred)
			w, retry, done := runCodexTurn(t, s, cred, wsTurnBody())
			if !retry || done || w.Body.Len() != 0 {
				t.Fatalf("WS failure must stay uncommitted: retry=%t done=%t body=%q", retry, done, w.Body.String())
			}
		})
	}
}

func TestCodexTransientErrorDoesNotReplayStartedResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"+transientErrorEvent("server_error", false))
	}))
	defer upstream.Close()
	s := codexHTTPTestServer(upstream.URL, wsCred("started-oauth"))
	s.cfg.OpenAIBaseURL = upstream.URL
	s.pool.AddAPIKey(codexAPIKeyCred("unused-fallback"))
	body := wsTurnBody()
	c, w := newCodexContext(t, "/v1/responses", body)
	s.forwardWithFailover(c, auth.ProviderOpenAI, "/v1/responses", "gpt-5.6-sol", "test-client", "", "tester", "", body, true, time.Now())
	if calls.Load() != 1 || strings.Count(w.Body.String(), "partial") != 1 || !strings.Contains(w.Body.String(), "server_error") {
		t.Fatalf("started response was replayed or error lost: calls=%d body=%q", calls.Load(), w.Body.String())
	}
}

func TestCodexTransientErrorExhaustionIsBounded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, transientErrorEvent("server_error", false))
	}))
	defer upstream.Close()
	s := codexHTTPTestServer(upstream.URL, wsCred("only-oauth"))
	body := wsTurnBody()
	c, w := newCodexContext(t, "/v1/responses", body)
	s.forwardWithFailover(c, auth.ProviderOpenAI, "/v1/responses", "gpt-5.6-sol", "test-client", "", "tester", "", body, true, time.Now())
	if calls.Load() != 1 || w.Code != http.StatusServiceUnavailable {
		t.Fatalf("exhausted route: calls=%d status=%d body=%q", calls.Load(), w.Code, w.Body.String())
	}
}

func TestCodexTransientErrorDoesNotWaitForUpstreamClose(t *testing.T) {
	for _, bridge := range []bool{false, true} {
		t.Run(fmt.Sprintf("chat=%t", bridge), func(t *testing.T) {
			c, w := newCodexStreamCtx()
			tail := &codexRetryTailReader{}
			reader := io.MultiReader(strings.NewReader(transientErrorEvent("server_error", false)), tail)
			var counts usage.Counts
			var result codexStreamResult
			if bridge {
				result = streamCodexAsChatCompletions(c, reader, &counts, "gpt-5.6-sol", false, func() {})
			} else {
				result = streamSSECodexBackend(c, &http.Response{Body: io.NopCloser(reader)}, &counts, func() {})
			}
			if result.shed == "" || result.wroteAny || result.sawTerminal || tail.read || w.Body.Len() != 0 {
				t.Fatalf("transient frame must immediately release routing: result=%+v readPastError=%t body=%q", result, tail.read, w.Body.String())
			}
		})
	}
}

func TestCodexTransientErrorDoesNotRetryCanceledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cred := wsCred("canceled-transient")
	conn := &cancelOnCodexErrorConn{
		egressConn: &egressConn{frames: []string{`{"type":"error","error":{"code":"server_error"}}`}},
		cancel:     cancel,
	}
	s := wsEgressServer(t, "http://unused.invalid", func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error) {
		return conn, conn.HandshakeResponse(), nil
	}, cred)
	body := wsTurnBody()
	c, _ := newCodexContext(t, "/v1/responses", body)
	c.Request = c.Request.WithContext(ctx)
	retry, done := s.doForwardCodexOAuth(c, cred, "/v1/responses", body, true, "gpt-5.6-sol", "test-client", "tester", "", time.Now(), 1)
	if retry || !done {
		t.Fatalf("canceled request must not consume another credential: retry=%t done=%t", retry, done)
	}
}

type cancelOnCodexErrorConn struct {
	*egressConn
	cancel context.CancelFunc
}

func (c *cancelOnCodexErrorConn) ReadMessage() (int, []byte, error) {
	mt, data, err := c.egressConn.ReadMessage()
	if strings.Contains(string(data), "server_error") {
		c.cancel()
	}
	return mt, data, err
}

type codexRetryTailReader struct{ read bool }

func (r *codexRetryTailReader) Read([]byte) (int, error) {
	r.read = true
	return 0, io.EOF
}
