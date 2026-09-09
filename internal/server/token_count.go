package server

import (
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/tokencount"
)

// Answering the two token-counting endpoints.
//
// Anthropic publishes POST /v1/messages/count_tokens and OpenAI publishes
// POST /v1/responses/input_tokens. Both are cheap, unbilled, and called
// constantly — Claude Code uses count_tokens to decide when to compact a
// conversation — and both return the same field, {"input_tokens": N}.
//
// Forwarding is the right answer only when the credential actually reaches the
// vendor, because the vendor counts things a tokenizer here cannot see: role
// and message framing, tool schemas, images, files. Every resale relay in this
// pool returns 404 for both routes, which is why count_tokens has never once
// succeeded here: 1634 requests from 17 customer tokens in fourteen days, zero
// answers. So the vendor-reachable case forwards and everything else is
// estimated locally (cc-core/tokencount).
//
// Two deliberate consequences of answering locally:
//
//   - It costs nothing and bills nothing, so it does not touch the pool, and a
//     credential is neither acquired nor charged for it.
//   - It does not consume the caller's RPM or concurrency budget. That budget
//     exists to ration upstream capacity, and a local count uses none. It also
//     ends a feedback loop seen in production: a client hammering a route that
//     always failed burned its own rate budget on the failures and then could
//     not use the models that did work.

func (s *Server) handleCountTokens(c *gin.Context) {
	if s.reachesVendorForTokenCount(auth.ProviderAnthropic) {
		s.forward(c, auth.ProviderAnthropic, "/v1/messages/count_tokens")
		return
	}
	s.answerTokenCountLocally(c, auth.ProviderAnthropic)
}

// handleCodexInputTokens serves OpenAI's POST /v1/responses/input_tokens.
//
// The route did not exist here at all, so a Codex client asking for a count got
// gin's own 404 with no JSON body.
func (s *Server) handleCodexInputTokens(c *gin.Context) {
	if s.reachesVendorForTokenCount(auth.ProviderOpenAI) {
		s.forward(c, auth.ProviderOpenAI, "/v1/responses/input_tokens")
		return
	}
	s.answerTokenCountLocally(c, auth.ProviderOpenAI)
}

// reachesVendorForTokenCount reports whether any enabled credential for this
// provider talks to the vendor's own API, where these routes exist.
//
// Deliberately narrow. A credential with a base-URL override points at a
// reseller, and no reseller in production implements either route; forwarding
// to one produces the 404 this whole file exists to stop. The Codex OAuth
// backend is excluded for a different reason: chatgpt.com/backend-api/codex is
// not api.openai.com and has never been observed serving input_tokens, so
// claiming it does would trade a working estimate for an unverified round trip.
func (s *Server) reachesVendorForTokenCount(provider string) bool {
	want := auth.NormalizeProvider(provider)
	for _, st := range s.pool.Status() {
		if auth.NormalizeProvider(st.Auth.Provider) != want || st.Auth.Disabled {
			continue
		}
		live := s.pool.FindByID(st.Auth.ID)
		if live == nil {
			continue
		}
		base := strings.ToLower(strings.TrimRight(live.Snapshot().BaseURL, "/"))
		switch want {
		case auth.ProviderAnthropic:
			// An OAuth credential with no override is api.anthropic.com, which
			// implements count_tokens (cc-core already carries the beta header
			// that route needs).
			if st.Auth.Kind == auth.KindOAuth && base == "" {
				return true
			}
			if base == "https://api.anthropic.com" {
				return true
			}
		default:
			if st.Auth.Kind == auth.KindAPIKey && (base == "" || base == "https://api.openai.com") {
				return true
			}
		}
	}
	return false
}

func (s *Server) answerTokenCountLocally(c *gin.Context, provider string) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeAPIError(c, provider, APIError{Status: http.StatusBadRequest, Code: "invalid_request_body", Message: "The request body could not be read. Send valid JSON and try again."})
		return
	}

	var n int
	if auth.NormalizeProvider(provider) == auth.ProviderAnthropic {
		n, err = tokencount.EstimateAnthropicInputTokens(body)
	} else {
		n, err = tokencount.EstimateResponsesInputTokens(body)
	}
	if err != nil {
		writeAPIError(c, provider, APIError{Status: http.StatusBadRequest, Code: "invalid_request_error", Message: "The request body could not be counted. Send a valid request with a model and try again."})
		return
	}

	log.Debugf("token count: answered %s locally with %d input tokens (no credential reaches the vendor route)", c.Request.URL.Path, n)
	c.JSON(http.StatusOK, gin.H{"input_tokens": n})
}
