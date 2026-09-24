package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/usage"
)

// Drive the real ingress and OAuth transport with no API-key fallback. This
// catches a route gate that would otherwise make the translator unreachable.
func TestCodexAgentChatOAuthEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, model := range []string{"gpt-5.6-sol", "gpt-6-sol", "gpt-6-luna"} {
		for _, streaming := range []bool{false, true} {
			t.Run(model+"/"+fmt.Sprint(streaming), func(t *testing.T) {
				requests := make(chan []byte, 1)
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					requests <- b
					if r.URL.Path != "/codex/responses" || r.Header.Get("Authorization") != "Bearer test-token" {
						t.Errorf("bad upstream request: %s", r.URL.Path)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"run","arguments":"{\"command\":\"pwd\"}"}}`+"\n\n")
					_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_agent","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":2}}}`+"\n\n")
				}))
				defer backend.Close()
				s := codexHTTPTestServer(backend.URL, wsCred("agent-oauth"))
				body := []byte(fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"run pwd"}],"tools":[{"type":"function","function":{"name":"run","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]}`, model, streaming))
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
				c.Set("client_token", "agent-test")
				s.forward(c, auth.ProviderOpenAI, "/v1/chat/completions")
				if rec.Code != 200 {
					t.Fatalf("request failed: %d %s", rec.Code, rec.Body.String())
				}
				out := rec.Body.String()
				for _, want := range []string{`"name":"run"`, `"call_1"`, `"finish_reason":"tool_calls"`, `pwd`} {
					if !strings.Contains(out, want) {
						t.Errorf("missing %s: %s", want, out)
					}
				}
				select {
				case b := <-requests:
					var req map[string]any
					if err := json.Unmarshal(b, &req); err != nil {
						t.Fatal(err)
					}
					if req["stream"] != true || req["store"] != false || req["parallel_tool_calls"] != false || req["messages"] != nil {
						t.Errorf("incorrect Codex request: %s", b)
					}
				default:
					t.Fatal("OAuth backend was never called")
				}
			})
		}
	}

}

func TestCodexAgentIncompleteNonStreaming(t *testing.T) {
	var counts usage.Counts
	payload, _, err := aggregateCodexResponseStream(strings.NewReader("data: "+`{"type":"response.incomplete","response":{"id":"resp_partial","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}],"usage":{"input_tokens":10,"output_tokens":2}}}`+"\n\n"), &counts)
	if err != nil || !strings.Contains(string(payload), `"status":"incomplete"`) || counts.OutputTokens != 2 {
		t.Fatalf("partial result lost: %s %+v %v", payload, counts, err)
	}
}

func TestCodexAgentStreamFailureIsError(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := "data: " + `{"type":"response.output_text.delta","delta":"partial"}` + "\n\ndata: " + `{"type":"response.failed","response":{"error":{"code":"invalid_request_error","message":"bad tool"}}}` + "\n\n"
	var counts usage.Counts
	result := streamCodexAsChatCompletions(c, strings.NewReader(body), &counts, "m", false, func() {})
	out := rec.Body.String()
	if result.failure == nil || !strings.Contains(out, `"error":`) || strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("false successful completion: %+v %s", result, out)
	}
}

func TestCodexAgentInvalidRequestDoesNotReachUpstream(t *testing.T) {
	s := codexHTTPTestServer("http://invalid.invalid", wsCred("invalid"))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("null"))
	retry, done := s.doForwardCodexOAuth(c, wsCred("invalid"), "/v1/responses", []byte("null"), false, "m", "tok", "test", "", time.Now(), 1)
	if retry || !done || rec.Code != 400 {
		t.Fatalf("local failure retried: %t %t %d", retry, done, rec.Code)
	}
}

func TestCodexAgentResponsesStreamRestoresTerminalOutput(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := "data: " + `{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"c","name":"run","arguments":"{}"}}` + "\n\ndata: " + `{"type":"response.completed","response":{"id":"r","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
	var counts usage.Counts
	result := streamSSECodexBackend(c, resp, &counts, func() {})
	if !result.sawTerminal {
		t.Fatalf("missing terminal: %+v", result)
	}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			Response struct {
				Output []json.RawMessage `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) == nil && ev.Type == "response.completed" {
			if len(ev.Response.Output) != 1 || !bytes.Contains(ev.Response.Output[0], []byte(`"call_id":"c"`)) {
				t.Fatalf("terminal lost tool: %s", line)
			}
			return
		}
	}
	t.Fatal("no completed event delivered")
}
