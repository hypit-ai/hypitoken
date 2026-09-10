package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wjsoj/cc-core/auth"
)

// APIError is the gateway's provider-neutral error description. writeAPIError
// renders it in the native schema expected by the requested API, while keeping
// stable, documented codes and never exposing credential or upstream details.
type APIError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func writeAPIError(c *gin.Context, provider string, e APIError) {
	if e.Status == 0 {
		e.Status = http.StatusInternalServerError
	}
	if e.Code == "" {
		e.Code = "internal_error"
	}
	if e.Message == "" {
		e.Message = "The request could not be completed. Please try again."
	}
	// A response that has already started cannot be given a status.
	//
	// The pre-output keepalive makes that a routine case rather than a bug:
	// bytes go out to keep the connection visibly alive while the forward loop
	// is still hunting for a credential, so by the time every credential is
	// exhausted the 200 and the SSE content-type are long since on the wire.
	// Calling c.JSON here would log "headers were already written" and send the
	// caller nothing at all. Say it in band instead, in the shape the upstream
	// itself uses for a mid-stream failure — which is the shape the client
	// already knows how to read.
	if c.Writer.Written() {
		writeSSEErrorFrame(c, provider, e)
		return
	}

	c.Header("Cache-Control", "no-store")
	c.Header("X-Error-Code", e.Code)

	if auth.NormalizeProvider(provider) == auth.ProviderAnthropic {
		body := gin.H{
			"type":  "error",
			"error": gin.H{"type": e.Code, "message": e.Message},
		}
		if len(e.Details) > 0 {
			body["details"] = e.Details
		}
		c.AbortWithStatusJSON(e.Status, body)
		return
	}

	errBody := gin.H{
		"message": e.Message,
		"type":    errorType(e.Status),
		"code":    e.Code,
		"param":   nil,
	}
	if len(e.Details) > 0 {
		errBody["details"] = e.Details
	}
	c.AbortWithStatusJSON(e.Status, gin.H{"error": errBody})
}

func requestProvider(c *gin.Context) string {
	if strings.Contains(c.Request.URL.Path, "messages") {
		return auth.ProviderAnthropic
	}
	return auth.ProviderOpenAI
}

func errorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// publicUpstreamError converts a provider/relay response into a deliberately
// small customer-facing taxonomy. The original body remains available only in
// operator logs; it is never replayed because it may contain vendor names,
// URLs, account identifiers, balances, or credential diagnostics.
func publicUpstreamError(status int, body []byte) APIError {
	text := strings.ToLower(extractErrorMessage(body))
	switch {
	case status == http.StatusRequestEntityTooLarge || strings.Contains(text, "context length") ||
		strings.Contains(text, "context_length") || strings.Contains(text, "too many tokens") ||
		strings.Contains(text, "prompt is too long"):
		return APIError{Status: statusOr(status, http.StatusBadRequest), Code: "context_length_exceeded", Message: "The request is too large for this model. Reduce the conversation or input size and try again."}
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return APIError{Status: status, Code: "invalid_request", Message: "The request is invalid or contains unsupported parameters. Check the model and request fields, then try again."}
	case status == http.StatusNotFound:
		return APIError{Status: http.StatusNotFound, Code: "model_or_endpoint_not_found", Message: "The requested model or endpoint is not available."}
	case status == http.StatusTooManyRequests:
		return APIError{Status: http.StatusTooManyRequests, Code: "service_rate_limited", Message: "The service is temporarily rate limited. Please retry after a short delay."}
	case status == http.StatusUnauthorized || status == http.StatusPaymentRequired || status == http.StatusForbidden:
		return APIError{Status: http.StatusServiceUnavailable, Code: "service_temporarily_unavailable", Message: "The requested model is temporarily unavailable. Please try again shortly."}
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return APIError{Status: http.StatusGatewayTimeout, Code: "service_timeout", Message: "The model service took too long to respond. Please try again."}
	case status >= 500:
		return APIError{Status: http.StatusServiceUnavailable, Code: "service_temporarily_unavailable", Message: "The model service is temporarily unavailable. Please try again shortly."}
	default:
		return APIError{Status: http.StatusBadGateway, Code: "service_error", Message: "The model service could not complete the request. Please try again."}
	}
}

func extractErrorMessage(body []byte) string {
	var v struct {
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	if json.Unmarshal(body, &v) == nil {
		if v.Message != "" {
			return v.Message
		}
		switch e := v.Error.(type) {
		case string:
			return e
		case map[string]any:
			if m, _ := e["message"].(string); m != "" {
				return m
			}
		}
	}
	return string(body)
}

func statusOr(status, fallback int) int {
	if status >= 400 && status < 500 {
		return status
	}
	return fallback
}

// writeSSEErrorFrame reports an error on a stream that has already begun.
//
// The frame mirrors what each vendor sends for a mid-stream failure, so a
// client that can read the upstream's own errors can read this one: Anthropic's
// `event: error` with a typed body, and the Responses API's `type: "error"`
// event. Both are followed by nothing — the stream simply ends — because there
// is no terminal event to fabricate and inventing one would tell the client the
// turn completed.
func writeSSEErrorFrame(c *gin.Context, provider string, e APIError) {
	var payload []byte
	if auth.NormalizeProvider(provider) == auth.ProviderAnthropic {
		payload, _ = json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": e.Code, "message": e.Message},
		})
		_, _ = c.Writer.WriteString("event: error\n")
	} else {
		payload, _ = json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "server_error", "code": e.Code, "message": e.Message},
		})
		_, _ = c.Writer.WriteString("event: error\n")
	}
	_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}
