package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/codeximage"
	"github.com/wjsoj/cc-core/downstream"
	"github.com/wjsoj/cc-core/mimicry"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/requestlog"
	ccstream "github.com/wjsoj/cc-core/stream"
	"github.com/wjsoj/cc-core/usage"
)

// OpenAI Images API (/v1/images/generations, /v1/images/edits) on a ChatGPT
// subscription credential, relayed to the backend's own Images route. Request
// shaping, the model list and the keepalive live in cc-core (codeximage,
// stream.JSONKeepalive); this is the pool/billing/logging glue around them.
// API-key credentials are not offered image requests (they roll the loop on).

func (s *Server) doForwardCodexImages(c *gin.Context, a *auth.Auth, path string, body []byte, model, clientToken, clientName, slotID string, start time.Time, attempts int) (retry, done bool) {
	if a.Kind != auth.KindOAuth {
		return true, false
	}
	logRow := func(status int, errText string) requestlog.Record {
		return requestlog.Record{
			Client: clientName, ClientToken: maskClientToken(clientToken), Provider: auth.ProviderOpenAI,
			AuthID: a.ID, AuthLabel: a.Label, AuthKind: "oauth", Model: model,
			Path: path, Status: status, Attempts: attempts,
			DurationMs: time.Since(start).Milliseconds(), Error: errText,
		}
	}
	reject := func(msg string) (bool, bool) {
		writeAPIError(c, auth.ProviderOpenAI, APIError{Status: http.StatusBadRequest, Code: "invalid_request_error", Message: msg})
		s.emitLog(logRow(http.StatusBadRequest, msg))
		return false, true
	}
	if !codeximage.IsModel(model) {
		return reject(fmt.Sprintf("Model %q is not an image model this endpoint serves. Use gpt-image-2.", model))
	}
	if _, stream := codeximage.Peek(body, c.GetHeader("Content-Type")); stream {
		return reject("Streaming image generation is not supported; send the request without `stream`.")
	}
	upBody, err := codeximage.UpstreamBody(body, c.GetHeader("Content-Type"), model)
	if err != nil {
		var bad codeximage.RequestError
		if errors.As(err, &bad) {
			return reject(bad.Message)
		}
		return reject("The request body could not be read.")
	}

	snap := a.Snapshot()
	baseURL := strings.TrimRight(s.cfg.ChatGPTBackendBaseURL, "/") + "/codex"
	if ab := strings.TrimRight(snap.BaseURL, "/"); ab != "" {
		baseURL = ab
	}
	upURL := baseURL + codeximage.BackendPath(path)

	ctx := c.Request.Context()
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(upBody))
	if err != nil {
		writeAPIError(c, auth.ProviderOpenAI, APIError{Status: http.StatusInternalServerError, Code: "request_preparation_failed", Message: "The request could not be prepared. Please try again."})
		return false, true
	}
	accessToken, _ := a.Credentials()
	accountID, _ := a.CodexIdentity()
	mimicry.ApplyCodexHeadersWithSession(upReq, mimicry.DefaultCodexProfile(), accessToken, accountID,
		false, "", "", s.codexUpstreamSessionID(a, clientToken, slotID, body))
	upReq.Header.Set("Accept", "application/json")

	// Past codeximage.CommitAfter the 200 goes out and a whitespace heartbeat
	// holds the connection open for the rest of the generation.
	ka := ccstream.NewJSONKeepalive(ctx, c.Writer)
	commitTimer := time.AfterFunc(codeximage.CommitAfter, func() {
		ka.Start(func() { markCommitted(c) })
	})
	defer commitTimer.Stop()
	// settle stops the heartbeat and reports whether the 200 already went out.
	settle := func() bool {
		commitTimer.Stop()
		return ka.Stop()
	}
	failCommitted := func(status int, reason string) (bool, bool) {
		ka.Fail("service_response_error", "The image service stopped before the image was finished. Please try again.")
		log.Warnf("codex images: %s failed after commit (%s): %s", a.ID, time.Since(start).Round(time.Millisecond), reason)
		s.emitLog(logRow(status, reason))
		return false, true
	}

	resp, err := auth.ClientFor(snap.ProxyURL, s.cfg.UseUTLS).Do(upReq)
	if err != nil {
		committed := settle()
		if isClientDisconnect(ctx, err) {
			a.MarkClientCancel("client canceled during image generation")
			s.emitLog(logRow(499, "client canceled"))
			return false, true
		}
		if committed {
			return failCommitted(http.StatusBadGateway, err.Error())
		}
		if !isTransientNetErr(err) {
			a.MarkFailure(err.Error())
		}
		log.Warnf("codex images: upstream error via %s: %v", a.ID, err)
		return true, false
	}
	defer func() { _ = resp.Body.Close() }()
	a.CaptureCodexRateLimits(resp.Header)
	respBody, rerr := io.ReadAll(io.LimitReader(resp.Body, codeximage.MaxResponseBytes))
	committed := settle()

	if resp.StatusCode >= 400 {
		switch resp.StatusCode {
		case http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden:
			resetAt := parseCodexResetAt(respBody)
			if resetAt.IsZero() {
				resetAt = parseRetryAfter(resp.Header)
			}
			if resp.StatusCode == http.StatusTooManyRequests && !resetAt.IsZero() && isCodexUsageLimitBody(respBody) {
				a.MarkUsageLimitReached(resetAt)
			}
			log.Warnf("codex images: credential %s received %d: %s", a.ID, resp.StatusCode, truncate(respBody, 240))
			if resp.StatusCode == http.StatusUnauthorized {
				s.rejectCodexBearer(a, accessToken, respBody)
			} else {
				s.pool.ReportUpstreamError(a, resp.StatusCode, resetAt)
			}
			if committed {
				return failCommitted(resp.StatusCode, fmt.Sprintf("upstream %d", resp.StatusCode))
			}
			return true, false
		}
		if isCodexCapacityError(respBody) || resp.StatusCode >= 500 || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			// Capacity, a backend fault, or an account whose backend lacks
			// the images route: another credential may well serve it.
			if isCodexCapacityError(respBody) {
				s.pool.ReportUpstreamError(a, http.StatusTooManyRequests, parseCodexResetAt(respBody))
			}
			log.Warnf("codex images: %s answered %d: %s", a.ID, resp.StatusCode, truncate(respBody, 240))
			if committed {
				return failCommitted(resp.StatusCode, fmt.Sprintf("upstream %d", resp.StatusCode))
			}
			return true, false
		}
		// A request the backend rejects (bad size, moderation, …) would fail
		// identically everywhere: relay it.
		if committed {
			return failCommitted(resp.StatusCode, fmt.Sprintf("upstream %d", resp.StatusCode))
		}
		copySafeRetryHeaders(c, resp.Header)
		writeAPIError(c, auth.ProviderOpenAI, publicUpstreamError(resp.StatusCode, respBody))
		s.emitLog(logRow(resp.StatusCode, fmt.Sprintf("upstream %d", resp.StatusCode)))
		return false, true
	}
	if rerr != nil || !json.Valid(respBody) {
		if isClientDisconnect(ctx, rerr) {
			a.MarkClientCancel("client canceled during image generation")
			s.emitLog(logRow(499, "client canceled"))
			return false, true
		}
		reason := "image response unreadable"
		if rerr != nil {
			reason = rerr.Error()
		}
		if committed {
			return failCommitted(http.StatusBadGateway, reason)
		}
		log.Warnf("codex images: %s returned an unreadable body: %s", a.ID, reason)
		return true, false
	}

	counts := usage.ParseImagesAPIUsage(respBody)
	counts.Requests = 1
	upstreamHeader := resp.Header
	ka.Deliver(respBody, func() { downstream.CopyResponseHeaders(c.Writer.Header(), upstreamHeader, time.Now()) })

	var costUSD, billedUSD, multiplier float64
	var userID int64
	var billingErr string
	if !counts.HasImageGen() {
		log.Warnf("codex images: %s returned an image without usage — not billed", a.ID)
		billingErr = usage.MissingUsageError
	} else if clientToken != "" {
		priced := s.pricing.CostWithOptions(auth.ProviderOpenAI, model, counts, pricing.CostOptions{CodexOAuth: true})
		costUSD, billedUSD = priced.CostUSD, priced.CostUSD
		if info, ok := saasInfoFrom(c); ok && s.saas != nil {
			billed, err := s.saas.Charge(chargeCtx(c), info, auth.ProviderOpenAI, model, counts, costUSD)
			if err != nil {
				log.Warnf("saas: image charge failed for token=%d user=%d: %v", info.TokenID, info.UserID, err)
				billedUSD = 0
				billingErr = billingDropped(err)
			} else {
				billedUSD = billed
				userID = info.UserID
				multiplier = s.saas.MultiplierFor(info, auth.ProviderOpenAI)
			}
		}
		s.usage.RecordClient(clientToken, clientName, counts, billedUSD)
	}
	s.usage.Record(a.ID, a.Label, counts)
	row := logRow(http.StatusOK, billingErr)
	row.Input = counts.ImageGenTextInputTokens + counts.ImageGenImageInputTokens
	row.Output = counts.ImageGenOutputTokens
	row.CostUSD, row.BilledUSD, row.UserID, row.Multiplier = costUSD, billedUSD, userID, multiplier
	s.emitLog(row)
	a.MarkSuccess()
	return false, true
}
