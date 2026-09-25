// Package admin exposes a small REST API + an embedded SPA for managing
// OAuth credentials at runtime. Protected by config.AdminToken.
package admin

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	// used in Anthropic usage proxy below

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"

	"github.com/wjsoj/CPA-Claude/internal/config"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/clienttoken"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/quotaestimate"
	"github.com/wjsoj/cc-core/requestlog"
	"github.com/wjsoj/cc-core/usage"
)

//go:generate bash -c "cd web && bun install --frozen-lockfile && bun run build"

// The SPA under web/ is built with Vite; the bundled output in web/dist is
// what actually ships in the binary. The directory is committed with a
// .gitkeep so this embed works even on a clean checkout where `make web`
// hasn't run yet — the panel will 404 until the build step populates
// web/dist.
//
//go:embed all:web/dist
var webFS embed.FS

// SaaSDistFS exposes the embedded web/dist tree to the SaaS adapter so it can
// serve the new SPA at the root path without re-embedding the same files.
func SaaSDistFS() (fs.FS, error) { return fs.Sub(webFS, "web/dist") }

type Handler struct {
	cfg     *config.Config
	pool    *auth.Pool
	usage   *usage.Store
	pricing *pricing.Catalog
	tokens  *clienttoken.Store

	// Cache for the per-auth aggregates the summary renders, keyed by
	// window ("lifetime" and the rolling 24h). Both used to re-scan the log
	// archive on every refresh — the 24h one had no cache at all — so a
	// polling panel meant a continuous stream of full-directory passes.
	// A short TTL keeps the UI live; byAuthSF collapses concurrent misses
	// into one pass.
	lifetimeMu  sync.Mutex
	byAuthCache map[string]byAuthCacheEntry
	byAuthSF    singleflight.Group

	// Cache for /api/requests responses. The overview dashboard polls
	// every 10s and issues two filter shapes (14-day window + all-time);
	// without caching each poll re-scans every rotated log file. A short
	// TTL keeps the UI live while collapsing N concurrent pollers into
	// one disk pass. Invalidation is by TTL only — the log is append-only
	// so minor staleness is acceptable.
	reqCacheMu sync.Mutex
	reqCache   map[string]reqCacheEntry
	// reqSF collapses concurrent misses on the same filter; see cachedQuery.
	reqSF singleflight.Group

	// Serializes credential PATCH and upload handlers so concurrent admin
	// mutations cannot overwrite one another's persisted fields.
	authMutationMu sync.Mutex

	// hitCache memoises the rejection-anchored weekly allotment estimate the
	// credential list carries per Anthropic OAuth row. The number stops
	// changing ten minutes after the 429 that produced it, and the list is
	// polled; without this every poll would be one ledger query per
	// credential for a constant.
	hitCache quotaestimate.HitCache
	// quotaHistoryOnce / quotaHistory: the last few settled full-window
	// measurements per credential, persisted next to the state file so
	// "what did the last N full weeks cost" survives restarts. Opened
	// lazily on first use; an unreadable file degrades to in-memory with a
	// log line rather than taking the admin API down.
	quotaHistoryOnce sync.Once
	quotaHistory     *quotaestimate.History
}

type reqCacheEntry struct {
	at     time.Time
	result *requestlog.Result
}

// lifetimeCacheTTL is how long an aggregate is served without any refresh.
// lifetimeCacheMaxStale is how far past that we will still serve the cached
// copy *immediately* while refreshing in the background.
//
// The gap matters: on production these aggregates cost 0.7–1.7 s (the 24h
// window scans a 500 MB request index), and the TTL used to be exactly 15 s —
// the same as the credential panel's poll interval. Every single poll landed
// on an expired entry, so the panel paid the full aggregate every time and the
// operator saw a permanently "slow" page. With a stale window, only the
// background refresh pays it and the request itself is always a map lookup.
const lifetimeCacheTTL = 20 * time.Second
const lifetimeCacheMaxStale = 10 * time.Minute
const requestsCacheTTL = 15 * time.Second
const requestsCacheMax = 16

func New(cfg *config.Config, pool *auth.Pool, store *usage.Store, cat *pricing.Catalog, tokens *clienttoken.Store) *Handler {
	return &Handler{cfg: cfg, pool: pool, usage: store, pricing: cat, tokens: tokens}
}

type byAuthCacheEntry struct {
	at     time.Time
	result map[string]requestlog.Aggregate
}

func (h *Handler) lifetimeByAuth() map[string]requestlog.Aggregate {
	return h.cachedByAuth("lifetime", time.Time{}, time.Time{})
}

// cachedByAuth memoizes one AggregateByAuth window, stale-while-revalidate.
//
// Three cases:
//   - fresh (age < TTL)            → return it, do nothing
//   - stale but usable (< maxStale)→ return it NOW, refresh in the background
//   - missing or too stale         → compute synchronously
//
// The lock is released before the aggregate runs and reacquired to store the
// result, and concurrent misses collapse through singleflight. Holding the
// mutex across the call instead (which this did until the panel was profiled)
// meant a second caller waited out the first one's entire scan rather than
// being deduplicated: two requests arriving together turned one 17s aggregate
// into 17s + 18s.
//
// A stale entry is preferred to an error, so a transient failure keeps
// rendering the last known totals.
func (h *Handler) cachedByAuth(key string, from, to time.Time) map[string]requestlog.Aggregate {
	h.lifetimeMu.Lock()
	ent, ok := h.byAuthCache[key]
	h.lifetimeMu.Unlock()
	if ok {
		age := time.Since(ent.at)
		if age < lifetimeCacheTTL {
			return ent.result
		}
		if age < lifetimeCacheMaxStale {
			// Warm the entry for the next caller without making this one wait.
			// singleflight keeps a slow aggregate from stacking up refreshes
			// across a burst of polls.
			go func() { _, _, _ = h.byAuthSF.Do(key, func() (any, error) { return h.refreshByAuth(key, from, to) }) }()
			return ent.result
		}
	}

	v, err, _ := h.byAuthSF.Do(key, func() (any, error) {
		// Re-check under singleflight: whoever queued behind the winner must
		// not trigger a second pass.
		h.lifetimeMu.Lock()
		if ent, ok := h.byAuthCache[key]; ok && time.Since(ent.at) < lifetimeCacheTTL {
			h.lifetimeMu.Unlock()
			return ent.result, nil
		}
		h.lifetimeMu.Unlock()
		return h.refreshByAuth(key, from, to)
	})
	if err != nil {
		log.Warnf("admin: %s aggregate: %v", key, err)
		h.lifetimeMu.Lock()
		ent, ok := h.byAuthCache[key]
		h.lifetimeMu.Unlock()
		if ok {
			return ent.result
		}
		return map[string]requestlog.Aggregate{}
	}
	return v.(map[string]requestlog.Aggregate)
}

// refreshByAuth runs one aggregate and stores it. Always called through
// byAuthSF so only one pass per window is ever in flight.
func (h *Handler) refreshByAuth(key string, from, to time.Time) (map[string]requestlog.Aggregate, error) {
	m, err := requestlog.AggregateByAuth(h.cfg.LogDir, from, to)
	if err != nil {
		return nil, err
	}
	h.lifetimeMu.Lock()
	if h.byAuthCache == nil {
		h.byAuthCache = make(map[string]byAuthCacheEntry, 2)
	}
	h.byAuthCache[key] = byAuthCacheEntry{at: time.Now(), result: m}
	h.lifetimeMu.Unlock()
	return m, nil
}

// timePtr renders a zero time as JSON-omitted rather than "0001-01-01".
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func aggToCounts(a requestlog.Aggregate) usage.Counts {
	return usage.Counts{
		InputTokens:       a.InputTokens,
		OutputTokens:      a.OutputTokens,
		CacheCreateTokens: a.CacheCreateTokens,
		CacheReadTokens:   a.CacheReadTokens,
		Requests:          a.Count,
		Errors:            a.Errors,
	}
}

// Register attaches the legacy admin JSON API at /admin/api/*. The SPA
// shell that used to live at a separate /mgmt-console/ path has been removed — the SaaS
// frontend (mounted at /) is the only operator UI now.
//
// If cfg.AdminToken is empty the API is disabled.
func (h *Handler) Register(r *gin.Engine) {
	if strings.TrimSpace(h.cfg.AdminToken) == "" {
		log.Info("admin: disabled (admin_token not set)")
		return
	}
	log.Infof("admin: legacy JSON API enabled at /admin/api/")

	api := r.Group("/admin/api")
	api.Use(h.adminAuth())
	{
		api.GET("/summary", h.handleSummary)
		api.POST("/auths/upload", h.handleUpload)
		api.PATCH("/auths/:id", h.handlePatchAuth)
		api.DELETE("/auths/:id", h.handleDeleteAuth)
		api.POST("/auths/:id/refresh", h.handleRefresh)
		api.POST("/auths/:id/clear-quota", h.handleClearQuota)
		api.POST("/auths/:id/clear-failure", h.handleClearFailure)
		api.POST("/oauth/start", h.handleOAuthStart)
		api.POST("/oauth/finish", h.handleOAuthFinish)
		api.POST("/apikeys", h.handleCreateAPIKey)
		api.POST("/auths/:id/anthropic-usage", h.handleAnthropicUsage)
		api.POST("/auths/:id/codex-usage", h.handleCodexUsage)
		api.POST("/auths/:id/codex-subscription", h.handleCodexSubscription)
		api.POST("/auths/:id/reset-codex-credit", h.handleResetCodexCredit)
		api.GET("/requests", h.handleRequestsQuery)
		api.GET("/requests/clients", h.handleRequestsClients)
		api.GET("/requests/hourly", h.handleRequestsHourly)
		api.GET("/tokens", h.handleListTokens)
		api.POST("/tokens", h.handleCreateToken)
		api.GET("/orphan-tokens", h.handleListOrphanTokens)
		api.PATCH("/tokens/:token", h.handlePatchToken)
		api.DELETE("/tokens/:token", h.handleDeleteToken)
		api.POST("/tokens/:token/reset", h.handleResetToken)
		api.POST("/tokens/:token/inherit", h.handleInheritToken)
	}
}

// SSOAuth is an optional hook that lets the SaaS layer turn a SaaS JWT
// into a legacy /admin/api/* authorization. main.go wires it up after the
// SaaS issuer is constructed; nil = SSO disabled (legacy token only).
//
// Returns (allowed, isAdmin, err):
//   - allowed=true means the caller is a logged-in SaaS user. The legacy
//     admin gate accepts the request.
//   - isAdmin reflects the user's SaaS role *as stored*, not as claimed by
//     the token. For non-admins, mutation endpoints (POST/PATCH/DELETE) are
//     still blocked so a regular user can read fleet stats but can't, say,
//     delete an upstream credential.
//   - err != nil means the hook could not reach the store and therefore does
//     not know the caller's role. This gate must fail closed on it (503) and
//     must never treat "unknown" as "not an admin but allowed" — see
//     saas/auth.LegacyAdminSSO for why the answer cannot come from the token.
//
// The hook is consulted only when the bearer token doesn't match the
// configured AdminToken — so legacy clients with the operator password
// keep working unchanged.
var SSOAuth func(c *gin.Context) (allowed bool, isAdmin bool, err error)

// adminAuth verifies the X-Admin-Token header (or Bearer token) against
// ctxKeyPrivileged marks a request as coming from an operator (legacy admin
// token or SaaS admin role) rather than an ordinary signed-in user. Read-only
// handlers use it to decide whether to redact fleet-internal detail.
const ctxKeyPrivileged = "admin_privileged"

// config.AdminToken using constant-time compare. If neither matches and
// SSOAuth is registered, it falls back to SaaS JWT verification — that
// path lets a logged-in SaaS user reach the legacy console without ever
// learning the operator password.
func (h *Handler) adminAuth() gin.HandlerFunc {
	want := []byte(strings.TrimSpace(h.cfg.AdminToken))
	return func(c *gin.Context) {
		got := strings.TrimSpace(c.GetHeader("X-Admin-Token"))
		if got == "" {
			v := strings.TrimSpace(c.GetHeader("Authorization"))
			if strings.HasPrefix(strings.ToLower(v), "bearer ") {
				got = strings.TrimSpace(v[len("bearer "):])
			}
		}
		// Path 1: legacy operator token. Constant-time compare so a token
		// that happens to look like a SaaS JWT can't time-leak the secret.
		if got != "" && subtle.ConstantTimeCompare([]byte(got), want) == 1 {
			c.Set(ctxKeyPrivileged, true)
			c.Next()
			return
		}
		// Path 2: SSO via SaaS JWT. Read-only endpoints are open to any
		// authenticated user (including non-admin SaaS users) — that's
		// the whole point: Overview/charts visible to anyone signed in.
		// Mutations require role=admin. Non-admin SSO callers are flagged
		// non-privileged so read-only handlers can redact fleet-internal
		// detail (credential counts, real token labels) — see handleSummary
		// and handleRequestsQuery.
		if SSOAuth != nil {
			allowed, isAdmin, err := SSOAuth(c)
			switch {
			case err != nil:
				// The hook could not establish the caller's role. Refuse
				// rather than guess: a real operator retrying one request is
				// cheap, admitting an unverified one is not.
				log.Warnf("admin: SSO authorization unavailable: %v", err)
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "authorization backend unavailable"})
				return
			case allowed:
				if isAdmin || c.Request.Method == http.MethodGet {
					c.Set(ctxKeyPrivileged, isAdmin)
					c.Next()
					return
				}
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "operator role required"})
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid admin token"})
	}
}

// ---- responses ----

type authRow struct {
	ExplicitFailuresOnly bool       `json:"explicit_failures_only"`
	ID                   string     `json:"id"`
	Kind                 string     `json:"kind"`
	Provider             string     `json:"provider"` // "anthropic" | "openai"
	PlanType             string     `json:"plan_type,omitempty"`
	Label                string     `json:"label"`
	Email                string     `json:"email,omitempty"`
	ProxyURL             string     `json:"proxy_url"`
	BaseURL              string     `json:"base_url,omitempty"`
	Group                string     `json:"group,omitempty"`
	MaxConcurrent        int        `json:"max_concurrent"`
	ActiveClients        int        `json:"active_clients"`
	ClientTokens         []string   `json:"client_tokens"`
	Disabled             bool       `json:"disabled"`
	QuotaExceeded        bool       `json:"quota_exceeded"`
	QuotaResetAt         *time.Time `json:"quota_reset_at,omitempty"`
	// Quarantine is the API-key circuit breaker: a channel that failed
	// repeatedly is paused for a self-expiring, exponentially growing
	// interval so traffic rotates onto a working key. Surfaced so the panel
	// can distinguish "paused, will retry at X" from a plain failure — a
	// silently paused channel looks identical to a healthy idle one.
	// Zero/absent = circuit closed. API-key credentials only.
	QuarantinedUntil  *time.Time `json:"quarantined_until,omitempty"`
	QuarantineStrikes int        `json:"quarantine_strikes,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	LastFailure       string     `json:"last_failure,omitempty"`
	FileBacked        bool       `json:"file_backed"`
	Healthy           bool       `json:"healthy"`
	HardFailure       bool       `json:"hard_failure"`
	FailureReason     string     `json:"failure_reason,omitempty"`
	// RefreshSuspended is true when the background OAuth refresher
	// (cc-core/auth Pool.RefreshExpiring) will deliberately skip this
	// credential — i.e. it is disabled or hard-failed. The frontend uses
	// this to explain why an OAuth token can show "expired Xd ago"
	// without ever being refreshed: refresh is intentionally frozen
	// pending operator action (clear-failure / re-enable).
	RefreshSuspended       bool   `json:"refresh_suspended,omitempty"`
	RefreshSuspendedReason string `json:"refresh_suspended_reason,omitempty"`
	// Most recent client-initiated cancellation. Informational only —
	// doesn't affect Healthy or trigger any cooldown. Used by the panel to
	// show a low-tone "client canceled" hint distinct from upstream
	// failures.
	LastClientCancel   *time.Time        `json:"last_client_cancel,omitempty"`
	ClientCancelReason string            `json:"client_cancel_reason,omitempty"`
	ModelMap           map[string]string `json:"model_map,omitempty"`
	AllowedModels      []string          `json:"allowed_models,omitempty"`
	Usage              *usageSummary     `json:"usage,omitempty"`
	// WeeklyAllotment is what the credential's 7-day window turned out to be
	// worth the last time it filled: the request-log spend between the
	// window's start (reset − 168h) and the usage-limit 429, at catalogue
	// price. Anthropic OAuth only, and only once a weekly rejection has been
	// seen since this process started — the live probe (anthropic-usage)
	// covers the no-rejection case. See cc-core/quotaestimate.
	WeeklyAllotment *quotaestimate.Estimate `json:"weekly_allotment,omitempty"`
	// WeeklyAllotmentHistory is the last few settled full-window
	// measurements (newest first), persisted across restarts — the record an
	// operator compares to decide whether an account's allotment shrank.
	WeeklyAllotmentHistory []quotaestimate.Measurement `json:"weekly_allotment_history,omitempty"`
	// QuotaUsageLimit qualifies QuotaExceeded: true when the window actually
	// filled (usage-limit rejection with the upstream's reset), false when it
	// is the pool's own throttle pause after a generic 429/401/403. Both park
	// the credential; only the former is "quota exhausted" to a human.
	QuotaUsageLimit bool `json:"quota_usage_limit"`
	// CodexRateLimits holds the latest x-codex-* response headers from
	// chatgpt.com — primary/secondary window used-percent, resets etc.
	// Empty for non-OAuth / non-Codex credentials or until first call.
	CodexRateLimits   map[string]string `json:"codex_rate_limits,omitempty"`
	CodexRateLimitsAt *time.Time        `json:"codex_rate_limits_at,omitempty"`
	// CodexUsage is the latest wham/usage snapshot (active probe of the
	// chatgpt.com web portal). Stays nil for non-Codex creds and for Codex
	// creds that have never been probed.
	CodexUsage   *auth.CodexUsageInfo `json:"codex_usage,omitempty"`
	CodexUsageAt *time.Time           `json:"codex_usage_at,omitempty"`
	// CodexSubscription is the billing view (plan, term, renewal, delinquency)
	// from the last FetchCodexSubscription. Carried on the row — not only in
	// the probe response — so a credential that is about to lapse for billing
	// reasons is visible on page load rather than only after someone clicks.
	CodexSubscription *codexSubscriptionView `json:"codex_subscription,omitempty"`
}

// codexSubscriptionView is the raw upstream billing view plus the answers
// cc-core derives from it.
//
// The derived fields are computed in Go rather than in the SPA on purpose:
// cc-core exposes PurchasedAt/ExpiresAt/Plan/IsFree/AtRisk precisely so every
// consumer answers these questions identically, and two of them are not
// obvious. "Is it free?" has two independent sources (a gratis flag and a
// 100%-off promo), and "is it at risk?" has to pick between grace-period end
// and term end. Re-deriving either in TypeScript is how the panel and the
// server start disagreeing about whether an account is paid.
type codexSubscriptionView struct {
	Info *auth.CodexSubscriptionInfo `json:"info,omitempty"`

	Plan        string     `json:"plan,omitempty"`
	PurchasedAt *time.Time `json:"purchased_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`

	Free       bool   `json:"free"`
	FreeReason string `json:"free_reason,omitempty"`

	AtRisk       bool       `json:"at_risk"`
	RiskReason   string     `json:"risk_reason,omitempty"`
	RiskDeadline *time.Time `json:"risk_deadline,omitempty"`

	// FetchedAt is when the probe last succeeded, so the panel can say how
	// stale the answer is instead of implying it is live.
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
}

// newCodexSubscriptionView folds the cc-core helpers into one payload. Returns
// nil for a credential that has never been probed, so the field stays absent
// rather than rendering as an empty billing panel.
func newCodexSubscriptionView(info *auth.CodexSubscriptionInfo, at time.Time) *codexSubscriptionView {
	if info == nil {
		return nil
	}
	v := &codexSubscriptionView{Info: info, Plan: info.Plan()}
	if t := info.PurchasedAt(); !t.IsZero() {
		v.PurchasedAt = &t
	}
	if t := info.ExpiresAt(); !t.IsZero() {
		v.ExpiresAt = &t
	}
	v.Free, v.FreeReason = info.IsFree()
	var deadline time.Time
	v.AtRisk, v.RiskReason, deadline = info.AtRisk()
	if !deadline.IsZero() {
		v.RiskDeadline = &deadline
	}
	if !at.IsZero() {
		v.FetchedAt = &at
	}
	return v
}

type usageSummary struct {
	Total  usage.Counts `json:"total"`
	Sum24h usage.Counts `json:"sum_24h"`
	// Sum5h is the in-memory rolling 5h window — used by the UI to show
	// "recent burn" for Codex OAuth creds (ChatGPT backend doesn't expose
	// a proactive remaining-quota API, so this is the best signal we have
	// before a 429 actually fires).
	Sum5h    usage.Counts `json:"sum_5h"`
	LastUsed *time.Time   `json:"last_used,omitempty"`
	Daily    []credDay    `json:"daily"` // last 14 days, oldest first; filled only by handleBridgeCredUsage
	// TotalCostUSD is the lifetime USD spend routed through this credential,
	// summed from the request log. Includes advisor sub-call rows (priced
	// under their own model), so a credential's bar reflects true cost.
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// credDay is one day of the per-credential 14-day series served by
// handleBridgeCredUsage. Sourced from the request log rather than the
// in-memory usage store so each day carries a real USD cost.
type credDay struct {
	Day               string  `json:"day"` // "YYYY-MM-DD" in requestlog.BucketLocation()
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	CacheCreateTokens int64   `json:"cache_create_tokens"`
	Requests          int64   `json:"requests"`
	Errors            int64   `json:"errors"`
	CostUSD           float64 `json:"cost_usd"`
}

// buildAuthRows produces the rich credential rows used by both the legacy
// /summary endpoint and the SaaS /api/v2/admin/credentials bridge. Factored
// out so the SaaS panel can render the same data (usage / quota / model_map
// / client_tokens / codex rate-limits) without re-shipping all of /summary.
//
// The 14-day daily series is never filled here: nothing on either panel's
// list view reads it, yet it was ~70% of the response body (77 KB for 35
// credentials, re-sent on every 15s poll). The credential detail dialog
// fetches it per-credential instead — see handleBridgeCredUsage.
func (h *Handler) buildAuthRows() []authRow {
	usageMap := h.usage.Snapshot()
	lifetime := h.lifetimeByAuth()
	// Truncated to the minute so the cache key is stable across a polling
	// panel's requests; a 24h window does not care about second-level drift.
	last24h := h.cachedByAuth("24h", time.Now().Add(-24*time.Hour).Truncate(time.Minute), time.Time{})
	rows := make([]authRow, 0, 16)
	for _, st := range h.pool.Status() {
		kind := "oauth"
		if st.Auth.Kind == auth.KindAPIKey {
			kind = "apikey"
		}
		var u *usageSummary
		// Show a usage row for every auth that has either in-memory daily
		// history or any log-recorded activity, so lifetime totals keep
		// rendering even after a state rebuild wipes the in-memory store.
		v, hasMem := usageMap[st.Auth.ID]
		lifeAgg, hasLife := lifetime[st.Auth.ID]
		last24Agg := last24h[st.Auth.ID]
		if hasMem || hasLife {
			var lastPtr *time.Time
			if hasMem && !v.LastUsed.IsZero() {
				lu := v.LastUsed
				lastPtr = &lu
			}
			u = &usageSummary{
				Total:        aggToCounts(lifeAgg),
				Sum24h:       aggToCounts(last24Agg),
				Sum5h:        h.usage.Sum5h(st.Auth.ID),
				LastUsed:     lastPtr,
				TotalCostUSD: lifeAgg.CostUSD,
			}
		}
		live := h.pool.FindByID(st.Auth.ID)
		var healthy, hardFail bool
		var failReason string
		var cancelAt *time.Time
		var cancelReason string
		if live != nil {
			healthy, hardFail, failReason, _ = live.HealthSnapshot()
			if at, reason := live.ClientCancelSnapshot(); !at.IsZero() {
				t := at
				cancelAt = &t
				cancelReason = reason
			}
		}
		provider := auth.NormalizeProvider(st.Auth.Provider)
		var planType string
		if live != nil {
			_, planType = live.CodexIdentity()
		}
		// refresh_suspended mirrors the gate in cc-core/auth Pool.RefreshExpiring:
		// background refresh skips disabled and hard-failed credentials. We
		// surface the gate here so the admin UI can explain why an OAuth
		// shows "expired Xd ago" yet never gets refreshed — operator action
		// (re-enable or clear-failure) is required first.
		var refreshSuspended bool
		var refreshSuspendedReason string
		if st.Auth.Kind == auth.KindOAuth {
			if st.Auth.Disabled {
				refreshSuspended = true
				refreshSuspendedReason = "credential disabled"
			} else if hardFail {
				refreshSuspended = true
				if failReason != "" {
					refreshSuspendedReason = "hard failure: " + failReason
				} else {
					refreshSuspendedReason = "hard failure"
				}
			}
		}
		rows = append(rows, authRow{
			ID:                     st.Auth.ID,
			Kind:                   kind,
			Provider:               provider,
			PlanType:               planType,
			Label:                  st.Auth.Label,
			Email:                  st.Auth.Email,
			ProxyURL:               st.Auth.ProxyURL,
			BaseURL:                st.Auth.BaseURL,
			Group:                  st.Auth.Group,
			MaxConcurrent:          st.Auth.MaxConcurrent,
			ActiveClients:          st.ActiveClients,
			ClientTokens:           h.resolveClientTokenLabels(st.ClientTokens),
			Disabled:               st.Auth.Disabled,
			QuotaExceeded:          !st.Auth.QuotaExceededAt.IsZero(),
			QuotaResetAt:           timePtr(st.Auth.QuotaResetAt),
			QuarantinedUntil:       timePtr(st.Auth.QuarantineUntil),
			QuarantineStrikes:      st.Auth.QuarantineStrikes,
			ExplicitFailuresOnly:   st.Auth.ExplicitFailuresOnly,
			ExpiresAt:              timePtr(st.Auth.ExpiresAt),
			FileBacked:             strings.TrimSpace(st.Auth.FilePath) != "",
			Healthy:                healthy,
			HardFailure:            hardFail,
			FailureReason:          failReason,
			RefreshSuspended:       refreshSuspended,
			RefreshSuspendedReason: refreshSuspendedReason,
			LastClientCancel:       cancelAt,
			ClientCancelReason:     cancelReason,
			ModelMap:               st.Auth.ModelMap,
			AllowedModels:          st.Auth.AllowedModels,
			Usage:                  u,
			WeeklyAllotment:        h.weeklyAllotment(st.Auth, provider, kind),
			WeeklyAllotmentHistory: h.quotaHist().For(st.Auth.ID),
			QuotaUsageLimit:        st.Auth.QuotaUsageLimit,
			// st.Auth is already a full auth.Snapshot() taken by Pool.Status(),
			// Codex fields included. This used to call live.Snapshot() four
			// more times per row — four extra credential-lock acquisitions and
			// two map deep-copies each, to read fields we were holding.
			CodexRateLimits:   st.Auth.CodexRateLimits,
			CodexRateLimitsAt: timePtr(st.Auth.CodexRateLimitsAt),
			CodexUsage:        st.Auth.CodexUsage,
			CodexUsageAt:      timePtr(st.Auth.CodexUsageAt),
			CodexSubscription: newCodexSubscriptionView(st.Auth.CodexSubscription, st.Auth.CodexSubscriptionAt),
		})
	}
	return rows
}

func (h *Handler) handleSummary(c *gin.Context) {
	rows := h.buildAuthRows()
	// Clients (per-access-token spending).
	clientSnap := h.usage.SnapshotClients()
	currentWeek := h.usage.CurrentWeekKey()
	clientRows := make([]clientRow, 0)
	seen := make(map[string]bool)
	addRow := func(token, label, group string, weeklyLimit float64, rpm int, fromConfig, managed bool) {
		seen[token] = true
		pc, hasData := clientSnap[token]
		weekly := 0.0
		var weeks []usage.WeekEntry
		var total usage.ClientCost
		var last *time.Time
		if hasData {
			weeks = pc.WeeklyOrdered(8)
			if w, ok := pc.Weekly[currentWeek]; ok {
				weekly = w.CostUSD
			}
			total = pc.Total
			if !pc.LastUsed.IsZero() {
				lu := pc.LastUsed
				last = &lu
			}
		}
		row := clientRow{
			Token:       maskToken(token),
			Label:       label,
			WeeklyUSD:   weekly,
			WeeklyLimit: weeklyLimit,
			Blocked:     weeklyLimit > 0 && weekly >= weeklyLimit,
			FromConfig:  fromConfig,
			Managed:     managed,
			Group:       group,
			RPM:         rpm,
			Total:       total,
			Weekly:      weeks,
			LastUsed:    last,
		}
		if managed || fromConfig {
			row.FullToken = token
		}
		clientRows = append(clientRows, row)
	}
	// Rows for every configured or runtime-added access token.
	for _, t := range h.tokens.List() {
		addRow(t.Token, t.Name, t.Group, t.WeeklyUSD, t.RPM, false, true)
	}
	// Rows for every client we've actually seen that isn't already listed
	// (e.g. open-mode requests keyed by IP).
	for tok, pc := range clientSnap {
		if !seen[tok] {
			addRow(tok, pc.Label, "", 0, 0, false, false)
		}
	}

	// Pricing view (editable in config.yaml, read-only here).
	priceView := make(map[string]pricing.ModelPrice)
	for k, v := range h.pricing.Models() {
		priceView[k] = v
	}

	// Fleet-wide usage aggregate + a single service-health verb. Computed
	// for every caller so the user-facing console can show usage KPIs and a
	// health badge without ever touching the per-credential rows.
	var totals usageSummary
	health := "down"
	for _, r := range rows {
		if r.Healthy {
			health = "operational"
		}
		if r.Usage != nil {
			totals.Total.Add(r.Usage.Total)
			totals.Sum24h.Add(r.Usage.Sum24h)
		}
	}

	out := gin.H{
		"active_window_minutes": h.cfg.ActiveWindowMinutes,
		"current_week":          currentWeek,
		"service_health":        health,
		"pricing": gin.H{
			"default":           h.pricing.Default(),
			"provider_defaults": h.pricing.ProviderDefaults(),
			"models":            priceView,
		},
	}
	// Operators get the full fleet detail; ordinary signed-in users do not —
	// the per-credential rows (their count, oauth/api split), the real client
	// token labels, and the fleet usage totals are all fleet-internal. The
	// totals in particular are the platform's 24h request and token volume:
	// a single number that tells any customer how big we are.
	if c.GetBool(ctxKeyPrivileged) {
		out["auth_dir"] = h.cfg.AuthDir
		out["default_proxy_url"] = h.cfg.DefaultProxyURL
		out["auths"] = rows
		out["clients"] = clientRows
		out["usage_totals"] = gin.H{"total": totals.Total, "sum_24h": totals.Sum24h}
	} else {
		out["auths"] = []authRow{}
		out["clients"] = []clientRow{}
	}
	c.JSON(http.StatusOK, out)
}

type clientRow struct {
	// Masked token (e.g. "sk-tes…aaaa") for display.
	Token string `json:"token"`
	// Full token; only set for rows that correspond to a registered client
	// token (not for the synthetic IP-keyed rows in open mode). The panel
	// needs this to build PATCH/DELETE URLs — admin auth covers exposure.
	FullToken   string  `json:"full_token,omitempty"`
	Label       string  `json:"label,omitempty"`
	WeeklyUSD   float64 `json:"weekly_usd"`
	WeeklyLimit float64 `json:"weekly_limit"`
	Blocked     bool    `json:"blocked"`
	FromConfig  bool    `json:"from_config,omitempty"`
	Managed     bool    `json:"managed,omitempty"` // true = panel can edit/delete
	Group       string  `json:"group,omitempty"`
	// RPM is the per-token requests-per-minute override. 0 = use global default.
	RPM      int               `json:"rpm,omitempty"`
	Total    usage.ClientCost  `json:"total"`
	Weekly   []usage.WeekEntry `json:"weekly,omitempty"`
	LastUsed *time.Time        `json:"last_used,omitempty"`
}

func maskToken(t string) string {
	if len(t) <= 10 {
		return "***"
	}
	return t[:6] + "…" + t[len(t)-4:]
}

// authKindString returns the wire-format tag ("oauth" / "apikey") used in the
// request log and admin API. Matches the string literal the proxy writes at
// request time so display-remapped entries stay schema-compatible.
func authKindString(k auth.Kind) string {
	if k == auth.KindAPIKey {
		return "apikey"
	}
	return "oauth"
}

// resolveClientTokenLabels turns raw client tokens into display strings for
// the admin panel. Registered tokens are shown by human name; unknown ones
// (open-mode IPs, stale entries) fall back to a masked form. Duplicates are
// collapsed with an "×N" suffix so N concurrent requests from the same client
// render as one tooltip entry.
func (h *Handler) resolveClientTokenLabels(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}
	order := make([]string, 0, len(tokens))
	counts := make(map[string]int, len(tokens))
	for _, t := range tokens {
		label := ""
		if h.tokens != nil {
			if entry, ok := h.tokens.Lookup(t); ok && strings.TrimSpace(entry.Name) != "" {
				label = entry.Name
			}
		}
		if label == "" {
			label = maskToken(t)
		}
		if _, ok := counts[label]; !ok {
			order = append(order, label)
		}
		counts[label]++
	}
	out := make([]string, 0, len(order))
	for _, label := range order {
		if counts[label] > 1 {
			out = append(out, fmt.Sprintf("%s ×%d", label, counts[label]))
		} else {
			out = append(out, label)
		}
	}
	return out
}

type patchAuthBody struct {
	ExplicitFailuresOnly *bool              `json:"explicit_failures_only"`
	Disabled             *bool              `json:"disabled"`
	MaxConcurrent        *int               `json:"max_concurrent"`
	ProxyURL             *string            `json:"proxy_url"`
	BaseURL              *string            `json:"base_url"`
	APIKey               *string            `json:"api_key"`
	Label                *string            `json:"label"`
	Group                *string            `json:"group"`
	ModelMap             *map[string]string `json:"model_map"`
	AllowedModels        *[]string          `json:"allowed_models"`
}

func (h *Handler) handlePatchAuth(c *gin.Context) {
	h.authMutationMu.Lock()
	defer h.authMutationMu.Unlock()

	id := c.Param("id")
	a := h.pool.FindByID(id)
	if a == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	// API keys declared in config.yaml have no backing file — they can't be
	// edited at runtime. File-backed keys (in auth_dir) are mutable like OAuth.
	if a.Kind == auth.KindAPIKey && a.FilePath == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "config.yaml-defined API keys are read-only; edit the YAML and restart, or add the key via the panel instead"})
		return
	}
	var body patchAuthBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.AllowedModels != nil && a.Kind != auth.KindAPIKey {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "allowed_models is API-key-only"})
		return
	}
	if body.APIKey != nil {
		if a.Kind != auth.KindAPIKey {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "api_key can only be changed for API-key credentials"})
			return
		}
		if strings.TrimSpace(*body.APIKey) == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "api_key must not be empty"})
			return
		}
	}
	if body.ProxyURL != nil {
		if err := auth.ValidateProxyURL(strings.TrimSpace(*body.ProxyURL)); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid proxy_url: " + err.Error()})
			return
		}
	}
	if body.ExplicitFailuresOnly != nil {
		if a.Kind != auth.KindAPIKey {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "explicit_failures_only is API-key-only"})
			return
		}
		a.SetExplicitFailuresOnly(*body.ExplicitFailuresOnly)
	}
	if body.Disabled != nil {
		if !*body.Disabled && a.Kind == auth.KindAPIKey {
			// Explicit enable starts a fresh trial after automatic retirement.
			a.ClearFailure()
			a.ClearQuota()
		}
		a.SetDisabled(*body.Disabled)
	}
	if body.MaxConcurrent != nil {
		a.SetMaxConcurrent(*body.MaxConcurrent)
	}
	if body.ProxyURL != nil {
		a.SetProxyURL(strings.TrimSpace(*body.ProxyURL))
	}
	if body.BaseURL != nil {
		a.SetBaseURL(strings.TrimRight(strings.TrimSpace(*body.BaseURL), "/"))
	}
	if body.Label != nil {
		label := strings.TrimSpace(*body.Label)
		if label != "" {
			a.Label = label
		}
	}
	if body.Group != nil {
		a.SetGroup(*body.Group)
	}
	if body.AllowedModels != nil {
		a.SetAllowedModels(*body.AllowedModels)
	}
	if body.ModelMap != nil {
		// Rewrite table for both kinds. API-key relays remap vendor model names;
		// Claude OAuth uses it for the default opus-4-6/4-7 → 4-8 upgrade (and any
		// operator overrides). Applied at routing time via ResolveUpstreamModel.
		a.SetModelMap(*body.ModelMap)
	}
	if err := a.Persist(); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "persist failed: " + err.Error()})
		return
	}
	if body.APIKey != nil {
		key := strings.TrimSpace(*body.APIKey)
		data, err := os.ReadFile(a.FilePath)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "read credential failed: " + err.Error()})
			return
		}
		var raw map[string]any
		if err = json.Unmarshal(data, &raw); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "parse credential failed: " + err.Error()})
			return
		}
		raw["api_key"] = key
		delete(raw, "key")
		delete(raw, "access_token")
		data, err = json.MarshalIndent(raw, "", "  ")
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "encode credential failed: " + err.Error()})
			return
		}
		replacement, err := auth.ParseFile(a.FilePath, data)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		tmp := a.FilePath + ".tmp"
		if err = os.WriteFile(tmp, data, 0600); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "write credential failed: " + err.Error()})
			return
		}
		if err = os.Rename(tmp, a.FilePath); err != nil {
			_ = os.Remove(tmp)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "replace credential failed: " + err.Error()})
			return
		}
		h.pool.AddAPIKey(replacement)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) handleDeleteAuth(c *gin.Context) {
	id := c.Param("id")
	a := h.pool.RemoveAuth(id)
	if a == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warnf("admin: failed to remove %s: %v", a.FilePath, err)
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) handleRefresh(c *gin.Context) {
	id := c.Param("id")
	a := h.pool.FindByID(id)
	if a == nil || a.Kind != auth.KindOAuth {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "oauth not found"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	// EnsureFresh with a huge leeway forces refresh regardless of current expiry.
	if err := a.EnsureFresh(ctx, 365*24*time.Hour, h.pool.UseUTLS()); err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "expires_at": a.Snapshot().ExpiresAt})
}

func (h *Handler) handleClearQuota(c *gin.Context) {
	id := c.Param("id")
	a := h.pool.FindByID(id)
	if a == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	a.ClearQuota()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) handleClearFailure(c *gin.Context) {
	id := c.Param("id")
	a := h.pool.FindByID(id)
	if a == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	a.ClearFailure()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

type uploadBody struct {
	// Provider scopes the upload to the tab the operator was on when they
	// opened the modal. When the parsed file omits a `provider` field (or
	// a recognizable `type`), this fills it in. When both are present and
	// disagree, the request is rejected — we don't want a Claude OAuth
	// silently persisted into the Codex tab just because the file didn't
	// declare itself.
	Provider      string          `json:"provider"`
	Filename      string          `json:"filename"`
	Content       json.RawMessage `json:"content"`
	Label         string          `json:"label"`
	MaxConcurrent int             `json:"max_concurrent"`
	ProxyURL      string          `json:"proxy_url"`
	Disabled      bool            `json:"disabled"`
	Group         string          `json:"group"`
}

func (h *Handler) handleUpload(c *gin.Context) {
	h.authMutationMu.Lock()
	defer h.authMutationMu.Unlock()

	var body uploadBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(body.Content) == 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing content"})
		return
	}
	// Merge user-supplied metadata into the raw JSON so parseFile sees it.
	var merged map[string]any
	if err := json.Unmarshal(body.Content, &merged); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid JSON: " + err.Error()})
		return
	}
	if merged == nil {
		merged = make(map[string]any)
	}
	// Reconcile the operator's tab-scoped provider choice with whatever the
	// uploaded JSON declares. Four cases:
	//   1. body.Provider empty, merged has no provider  → fall through, let
	//      parseFile infer from `type` (defaults to anthropic).
	//   2. body.Provider empty, merged has provider     → respect the file.
	//   3. body.Provider set, merged has no provider    → stamp it in.
	//   4. both set but mismatch                        → reject loudly.
	wantProv := auth.NormalizeProvider(body.Provider)
	if body.Provider != "" {
		if existing, _ := merged["provider"].(string); existing != "" {
			if auth.NormalizeProvider(existing) != wantProv {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error": fmt.Sprintf("uploaded file declares provider=%q but the %s tab was active — open the matching tab and try again", existing, wantProv),
				})
				return
			}
		} else {
			merged["provider"] = wantProv
		}
	}
	if body.Label != "" {
		merged["label"] = body.Label
	}
	if body.MaxConcurrent > 0 {
		merged["max_concurrent"] = body.MaxConcurrent
	}
	if strings.TrimSpace(body.ProxyURL) != "" {
		merged["proxy_url"] = strings.TrimSpace(body.ProxyURL)
	}
	if body.Disabled {
		merged["disabled"] = true
	}
	if g := auth.NormalizeGroup(body.Group); g != "" {
		merged["group"] = g
	}
	// The per-account mode experiment is retired. Ignore stale flags from older
	// uploaded files; genuine Claude Code OAuth traffic always uses Rewrite.
	delete(merged, "claude_identity_mode")

	// Derive target filename. Prefix with provider so the auths/ directory
	// is self-documenting when inspected directly on disk.
	finalProv, _ := merged["provider"].(string)
	finalProv = auth.NormalizeProvider(finalProv)
	prefix := "claude"
	if finalProv == auth.ProviderOpenAI {
		prefix = "codex"
	}
	name := sanitizeFilename(body.Filename)
	if name == "" {
		email, _ := merged["email"].(string)
		if email != "" {
			name = prefix + "-" + sanitizeFilename(email) + ".json"
		} else {
			name = fmt.Sprintf("%s-%d.json", prefix, time.Now().Unix())
		}
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0700); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	full := filepath.Join(h.cfg.AuthDir, name)

	finalBytes, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	oldBytes, oldErr := os.ReadFile(full)
	existed := oldErr == nil
	a, err := auth.InstallCredentialFile(full, finalBytes)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, auth.ErrCredentialFileAccountMismatch) {
			status = http.StatusConflict
		}
		c.AbortWithStatusJSON(status, gin.H{"error": "install credential: " + err.Error()})
		return
	}
	if err := h.pool.AddOAuth(a); err != nil {
		if existed {
			if _, restoreErr := auth.InstallCredentialFile(full, oldBytes); restoreErr != nil {
				log.Errorf("admin: rollback credential upload %s: %v", a.ID, restoreErr)
			}
		} else if removeErr := os.Remove(full); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			log.Errorf("admin: remove rejected credential upload %s: %v", a.ID, removeErr)
		}
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "id": a.ID})
}

type oauthStartBody struct {
	Provider string `json:"provider"` // "anthropic" | "openai"; empty = anthropic (back-compat)
	ProxyURL string `json:"proxy_url"`
	Label    string `json:"label"`
}

func (h *Handler) handleOAuthStart(c *gin.Context) {
	var body oauthStartBody
	_ = c.ShouldBindJSON(&body)
	provider := auth.NormalizeProvider(body.Provider)
	sess, authURL, err := auth.StartLogin(provider, body.ProxyURL, body.Label)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"session_id":   sess.ID,
		"provider":     provider,
		"auth_url":     authURL,
		"redirect_uri": auth.RedirectURIFor(provider),
	})
}

type oauthFinishBody struct {
	SessionID     string `json:"session_id"`
	Callback      string `json:"callback"` // full URL, or "code#state", or raw code
	Code          string `json:"code"`
	State         string `json:"state"`
	MaxConcurrent int    `json:"max_concurrent"`
	Group         string `json:"group"`
}

func (h *Handler) handleOAuthFinish(c *gin.Context) {
	var body oauthFinishBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(body.SessionID) == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing session_id"})
		return
	}
	code := strings.TrimSpace(body.Code)
	state := strings.TrimSpace(body.State)
	if code == "" && strings.TrimSpace(body.Callback) != "" {
		parsedCode, parsedState, err := auth.ParseCallback(body.Callback)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		code = parsedCode
		if state == "" {
			state = parsedState
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	a, err := auth.FinishLogin(ctx, body.SessionID, code, state, h.cfg.AuthDir, body.MaxConcurrent, h.cfg.UseUTLS, body.Group)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if err := h.pool.AddOAuth(a); err != nil {
		if errors.Is(err, auth.ErrDuplicateClaudeAccountUUID) && a.FilePath != "" {
			if removeErr := os.Remove(a.FilePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				log.Errorf("admin: remove rejected duplicate OAuth file %s: %v", a.ID, removeErr)
			}
		}
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "id": a.ID, "email": a.Email})
}

// ---- API key CRUD ----

type createAPIKeyBody struct {
	Provider      string            `json:"provider"` // "anthropic" | "openai"; empty = anthropic
	APIKey        string            `json:"api_key"`
	Label         string            `json:"label"`
	ProxyURL      string            `json:"proxy_url"`
	BaseURL       string            `json:"base_url"`
	Filename      string            `json:"filename"`
	Group         string            `json:"group"`
	ModelMap      map[string]string `json:"model_map"`
	AllowedModels []string          `json:"allowed_models"`
}

func (h *Handler) handleCreateAPIKey(c *gin.Context) {
	var body createAPIKeyBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing api_key"})
		return
	}
	label := strings.TrimSpace(body.Label)
	name := sanitizeFilename(body.Filename)
	if name == "" {
		if label != "" {
			name = sanitizeFilename("apikey-"+label) + ".json"
		} else {
			name = fmt.Sprintf("apikey-%d.json", time.Now().Unix())
		}
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0700); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	full := filepath.Join(h.cfg.AuthDir, name)
	provider := auth.NormalizeProvider(body.Provider)
	typ := "apikey"
	if provider == auth.ProviderOpenAI {
		typ = "openai_api_key"
	}
	raw := map[string]any{
		"type":     typ,
		"provider": provider,
		"api_key":  key,
	}
	if label != "" {
		raw["label"] = label
	}
	if strings.TrimSpace(body.ProxyURL) != "" {
		raw["proxy_url"] = strings.TrimSpace(body.ProxyURL)
	}
	if strings.TrimSpace(body.BaseURL) != "" {
		raw["base_url"] = strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	}
	if g := auth.NormalizeGroup(body.Group); g != "" {
		raw["group"] = g
	}
	if models := auth.NormalizeAllowedModels(body.AllowedModels); len(models) > 0 {
		raw["allowed_models"] = models
	}
	if len(body.ModelMap) > 0 {
		mm := make(map[string]any, len(body.ModelMap))
		for k, v := range body.ModelMap {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			mm[k] = strings.TrimSpace(v)
		}
		if len(mm) > 0 {
			raw["model_map"] = mm
		}
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	a, err := auth.ParseFile(full, data)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := os.WriteFile(full, data, 0600); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.pool.AddAPIKey(a)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "id": a.ID})
}

// ---- Anthropic upstream usage proxy ----

func (h *Handler) handleAnthropicUsage(c *gin.Context) {
	id := c.Param("id")
	a := h.pool.FindByID(id)
	if a == nil || a.Kind != auth.KindOAuth {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "oauth credential not found"})
		return
	}
	// Endpoint only speaks Anthropic's OAuth usage API — reject Codex
	// credentials up front rather than 502ing after a pointless token
	// refresh and an unknown-host probe.
	if auth.NormalizeProvider(a.Provider) != auth.ProviderAnthropic {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "anthropic-usage endpoint is Anthropic-only"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	// Ensure the access token is fresh before hitting the upstream endpoints.
	if err := a.EnsureFresh(ctx, 5*time.Minute, h.pool.UseUTLS()); err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "token refresh: " + err.Error()})
		return
	}
	token, _ := a.Credentials()
	client := auth.ClientFor(a.Snapshot().ProxyURL, h.pool.UseUTLS())

	fetch := func(url string) (int, map[string]any, string) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, nil, err.Error()
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err.Error()
		}
		defer resp.Body.Close()
		buf, _ := io.ReadAll(resp.Body)
		var obj map[string]any
		_ = json.Unmarshal(buf, &obj)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			msg := strings.TrimSpace(string(buf))
			if len(msg) > 300 {
				msg = msg[:300] + "...(truncated)"
			}
			return resp.StatusCode, obj, msg
		}
		return resp.StatusCode, obj, ""
	}

	usageStatus, usageBody, usageErr := fetch("https://api.anthropic.com/api/oauth/usage")
	profileStatus, profileBody, profileErr := fetch("https://api.anthropic.com/api/oauth/profile")

	// Allotment estimates: what each reported window is worth in our own
	// ledger's units, from the window's start (resets_at − length) to now
	// divided by utilization — or, when this process saw the window fill,
	// the spend up to that 429 as a measured 100%. A failed probe still
	// yields the rejection-anchored estimate when there is one. Never an
	// error: the probe result is returned regardless.
	var estimates []quotaestimate.Estimate
	if usageStatus >= 200 && usageStatus < 300 {
		estimates = quotaestimate.ForCredential(usageBody, a.Snapshot().LastQuotaHit,
			quotaestimate.RequestLogSpend(h.cfg.LogDir, a.ID), time.Now())
	} else {
		estimates = quotaestimate.ForCredential(nil, a.Snapshot().LastQuotaHit,
			quotaestimate.RequestLogSpend(h.cfg.LogDir, a.ID), time.Now())
	}

	c.JSON(http.StatusOK, gin.H{
		"usage": gin.H{
			"status": usageStatus,
			"body":   usageBody,
			"error":  usageErr,
		},
		"profile": gin.H{
			"status": profileStatus,
			"body":   profileBody,
			"error":  profileErr,
		},
		"allotment_estimates": estimates,
	})
}

// weeklyAllotment returns the rejection-anchored weekly estimate for an
// OAuth credential list row (Anthropic usage-limit 429 or ChatGPT
// usage_limit_reached), nil for everything else. The
// estimate is a constant once the rejection is ten minutes old, so it is
// served from hitCache; the live utilization-based estimate is deliberately
// NOT computed here — it needs an upstream probe per credential, which the
// list must never do on a poll.
func (h *Handler) weeklyAllotment(info auth.AuthInfo, provider, kind string) *quotaestimate.Estimate {
	if kind != "oauth" || info.LastQuotaHit.At.IsZero() {
		return nil
	}
	_ = provider // both OAuth providers record usage-limit hits the same way
	now := time.Now()
	est := h.hitCache.Weekly(info.ID, info.LastQuotaHit,
		quotaestimate.RequestLogSpend(h.cfg.LogDir, info.ID), now)
	// Once the ledger has settled (in-flight requests at the time of the 429
	// have finished logging), the measurement is final: keep it.
	if est != nil && est.QuotaHitAt != nil && now.Sub(*est.QuotaHitAt) >= 10*time.Minute {
		if _, err := h.quotaHist().Record(info.ID, *est, now); err != nil {
			log.Warnf("quota history: record %s: %v", info.ID, err)
		}
	}
	return est
}

// quotaHist returns the persisted measurement history, opening it on first
// use at <state_file dir>/quota_history.json.
func (h *Handler) quotaHist() *quotaestimate.History {
	h.quotaHistoryOnce.Do(func() {
		path := filepath.Join(filepath.Dir(h.cfg.StateFile), "quota_history.json")
		hist, err := quotaestimate.OpenHistory(path, 0)
		if err != nil {
			log.Warnf("quota history: open %s: %v (keeping measurements in memory only)", path, err)
			hist = &quotaestimate.History{}
		}
		h.quotaHistory = hist
	})
	return h.quotaHistory
}

// handleCodexUsage actively probes chatgpt.com/backend-api/wham/usage for an
// OpenAI OAuth credential. The same data is mirrored into a.CodexRateLimits
// so the legacy "Rolling 5h / weekly" panel stays in sync; the full payload
// (plan_type / credits / spend_control) is returned to the caller for the
// admin dialog. See cc-core/auth/codex_usage.go for the response shape.
func (h *Handler) handleCodexUsage(c *gin.Context) {
	id := c.Param("id")
	a := h.pool.FindByID(id)
	if a == nil || a.Kind != auth.KindOAuth {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "oauth credential not found"})
		return
	}
	if auth.NormalizeProvider(a.Provider) != auth.ProviderOpenAI {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "codex-usage endpoint is OpenAI-only"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	info, err := a.FetchCodexUsage(ctx, h.pool.UseUTLS())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	// Allotment estimates for the reported windows — see handleAnthropicUsage;
	c.JSON(http.StatusOK, gin.H{
		"usage": info,
		"allotment_estimates": quotaestimate.ForCodexCredential(info, a.Snapshot().LastQuotaHit,
			quotaestimate.RequestLogSpend(h.cfg.LogDir, a.ID), time.Now()),
	})
}

// handleCodexSubscription probes the chatgpt.com billing endpoints
// (/backend-api/subscriptions + accounts/check) for an OpenAI OAuth credential
// and returns the plan/term/renewal view.
//
// This answers a different question from codex-usage. wham/usage says how much
// quota is left right now; this says what was bought, when the term started,
// and whether it renews — and in particular whether the account is delinquent.
// Delinquency is invisible to every other signal we have: the account keeps
// serving traffic normally until its grace period ends, then stops dead. This
// probe is the only advance warning.
//
// Like codex-usage it never touches credential health on failure (see the note
// on FetchCodexSubscription): a flaky billing endpoint says nothing about
// whether /responses works, and a delinquent account is still serving.
func (h *Handler) handleCodexSubscription(c *gin.Context) {
	id := c.Param("id")
	a := h.pool.FindByID(id)
	if a == nil || a.Kind != auth.KindOAuth {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "oauth credential not found"})
		return
	}
	if auth.NormalizeProvider(a.Provider) != auth.ProviderOpenAI {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "codex-subscription endpoint is OpenAI-only"})
		return
	}
	// Two upstream round trips, so a little more headroom than the single-call
	// usage probe.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	info, err := a.FetchCodexSubscription(ctx, h.pool.UseUTLS())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"subscription": newCodexSubscriptionView(info, a.Snapshot().CodexSubscriptionAt),
	})
}

// handleResetCodexCredit consumes one Codex rate-limit reset credit (the "quota
// reset card") for an OpenAI OAuth credential, immediately resetting the
// account's rolling rate-limit window(s) instead of waiting for the natural
// reset. It burns one of the account's finite reset credits.
//
// After a successful redeem we re-probe wham/usage so the response carries the
// refreshed available_count and reset windows, letting the UI update in one
// round trip. A usage-refresh failure is non-fatal — the reset already
// succeeded, so we still return 200 with the reset result and omit usage.
func (h *Handler) handleResetCodexCredit(c *gin.Context) {
	id := c.Param("id")
	a := h.pool.FindByID(id)
	if a == nil || a.Kind != auth.KindOAuth {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "oauth credential not found"})
		return
	}
	if auth.NormalizeProvider(a.Provider) != auth.ProviderOpenAI {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "reset-codex-credit endpoint is OpenAI-only"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	result, err := a.ResetCodexCredit(ctx, h.pool.UseUTLS())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	resp := gin.H{"reset": result}
	if usage, uerr := a.FetchCodexUsage(ctx, h.pool.UseUTLS()); uerr == nil {
		resp["usage"] = usage
	}
	c.JSON(http.StatusOK, resp)
}

// ---- request log query ----

func (h *Handler) handleRequestsHourly(c *gin.Context) {
	if h.cfg.LogDir == "" {
		c.JSON(http.StatusOK, gin.H{"buckets": []requestlog.HourBucket{}})
		return
	}
	hours := 24
	if v := strings.TrimSpace(c.Query("hours")); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &hours)
		if hours < 1 {
			hours = 1
		}
		if hours > 168 {
			hours = 168
		}
	}
	buckets, err := requestlog.AggregateHourly(h.cfg.LogDir, hours)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// The 24h pulse is on the customer-visible console. Non-operators get the
	// curve's shape, never the hourly volume behind it.
	if !c.GetBool(ctxKeyPrivileged) {
		buckets = redactPublicHourly(buckets)
	}
	c.JSON(http.StatusOK, gin.H{"buckets": buckets})
}

func (h *Handler) handleRequestsClients(c *gin.Context) {
	// Return current token names (no log scan). Orphan names that exist only
	// in historical logs are not offered as filter options; the user can
	// still type the old name into the URL if needed.
	seen := make(map[string]struct{})
	if h.tokens != nil {
		for _, t := range h.tokens.List() {
			n := strings.TrimSpace(t.Name)
			if n == "" {
				continue
			}
			seen[n] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	c.JSON(http.StatusOK, gin.H{"clients": out})
}

func (h *Handler) handleRequestsQuery(c *gin.Context) {
	if h.cfg.LogDir == "" {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "log_dir not configured"})
		return
	}
	f := requestlog.Filter{
		Dir:   h.cfg.LogDir,
		Model: strings.TrimSpace(c.Query("model")),
	}
	// Resolve the `client` query param (a current token name) to the masked
	// token so the filter matches across renames. Fall back to string match
	// on Record.Client for orphan names that no longer resolve.
	if name := strings.TrimSpace(c.Query("client")); name != "" {
		var matched string
		if h.tokens != nil {
			for _, t := range h.tokens.List() {
				if strings.EqualFold(strings.TrimSpace(t.Name), name) {
					matched = maskToken(t.Token)
					break
				}
			}
		}
		if matched != "" {
			f.ClientToken = matched
		} else {
			f.Client = name
		}
	}
	applyDateBounds(&f, c.Query("from"), c.Query("to"))
	f.Dims = parseDims(c.Query("dims"))
	if v := c.Query("limit"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &f.Limit)
	}
	if v := c.Query("offset"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &f.Offset)
	}
	if v := c.Query("status"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &f.Status)
	}
	res, err := h.cachedQuery(f)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	switch {
	case !c.GetBool(ctxKeyPrivileged):
		// Ordinary signed-in users. Anonymizing identities is not enough here:
		// the aggregates themselves are the fleet's daily request volume and
		// revenue, which are not customer-facing facts. Ship shapes only —
		// see public_redact.go.
		redactPublicResult(res)
	case c.Query("anon") == "1":
		// anon=1 forces the anonymized projection even for an operator. The
		// /console overview passes it so client identities (real token labels
		// or masked sk-… tokens) NEVER reach the browser there — the console
		// mirrors the public-status/cpa-claude pseudonym scheme. Aggregates
		// stay real: this branch is the operator looking at their own fleet.
		res.Entries = nil
		res.ByClient = anonymizeByClient(res.ByClient)
	default:
		// Operator on the /admin management surface: real labels, real rows.
		h.remapDisplayNames(res.Entries)
		res.ByClient = h.remapByClient(res.ByClient)
	}
	c.JSON(http.StatusOK, res)
}

// cachedQuery returns a fresh shallow copy of the cached Result for the
// given filter. Aggregate maps (ByClient/ByModel/ByDay) and Summary are
// shared with the cache — callers must replace them, not mutate in place.
// Entries are cloned into a fresh slice so downstream remapDisplayNames
// can mutate without data-racing concurrent readers.
func (h *Handler) cachedQuery(f requestlog.Filter) (*requestlog.Result, error) {
	key := reqCacheKey(f)
	h.reqCacheMu.Lock()
	if ent, ok := h.reqCache[key]; ok && time.Since(ent.at) < requestsCacheTTL {
		clone := cloneResult(ent.result)
		h.reqCacheMu.Unlock()
		return clone, nil
	}
	h.reqCacheMu.Unlock()

	// Collapse concurrent misses, the way cachedByAuth already does. Two
	// operators opening the panel a second apart — or one panel whose
	// auto-refresh fires while the previous load is still running — otherwise
	// each pay the full query, and the cost of an expensive window is
	// multiplied by however many of them arrive inside one TTL.
	v, err, _ := h.reqSF.Do(key, func() (any, error) {
		res, err := requestlog.Query(f)
		if err != nil {
			return nil, err
		}
		h.storeReqCache(key, res)
		return res, nil
	})
	if err != nil {
		return nil, err
	}
	res, _ := v.(*requestlog.Result)
	return cloneResult(res), nil
}

// storeReqCache publishes a fresh Result under key, evicting coarsely when the
// map has grown past its bound.
func (h *Handler) storeReqCache(key string, res *requestlog.Result) {
	h.reqCacheMu.Lock()
	if h.reqCache == nil || len(h.reqCache) >= requestsCacheMax {
		// Coarse eviction: when the cache grows unbounded (e.g., varied
		// user filters from the Requests tab), drop everything. The hot
		// Overview polls refill the two common keys within 10s.
		h.reqCache = make(map[string]reqCacheEntry, 4)
	}
	h.reqCache[key] = reqCacheEntry{at: time.Now(), result: res}
	h.reqCacheMu.Unlock()
}

func reqCacheKey(f requestlog.Filter) string {
	// Dir is constant per process; skip it to keep keys short.
	// FromDay/ToDay must be part of the key. They are an ALTERNATIVE to
	// From/To, not an addition: when they are set the timestamps are still
	// zero here (requestlog resolves them later), so a key built from the
	// timestamps alone would be byte-identical for every day-bounded window
	// and serve one range's aggregates for another.
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%d|%d|%d|%d",
		f.From.UTC().Format(time.RFC3339),
		f.To.UTC().Format(time.RFC3339),
		f.FromDay, f.ToDay,
		f.Client, f.ClientToken, f.Model,
		f.Status, f.Limit, f.Offset, int(f.Dims),
	)
}

func cloneResult(r *requestlog.Result) *requestlog.Result {
	if r == nil {
		return nil
	}
	out := *r
	if r.Entries != nil {
		out.Entries = append([]requestlog.Record(nil), r.Entries...)
	}
	return &out
}

// remapByClient rewrites ByClient map keys from masked ClientToken to the
// current display name. Unknown masks (deleted tokens) fall back to the
// mask itself so they remain visible as orphan rows. Merges buckets if two
// masks ever map to the same name (shouldn't happen in practice).
func (h *Handler) remapByClient(in map[string]requestlog.Aggregate) map[string]requestlog.Aggregate {
	if len(in) == 0 {
		return in
	}
	nameByMasked := make(map[string]string)
	if h.tokens != nil {
		for _, t := range h.tokens.List() {
			n := strings.TrimSpace(t.Name)
			if n == "" {
				continue
			}
			nameByMasked[maskToken(t.Token)] = n
		}
	}
	out := make(map[string]requestlog.Aggregate, len(in))
	for k, v := range in {
		display := k
		if cur, ok := nameByMasked[k]; ok {
			display = cur
		}
		if existing, ok := out[display]; ok {
			existing.Count += v.Count
			existing.InputTokens += v.InputTokens
			existing.OutputTokens += v.OutputTokens
			existing.CacheReadTokens += v.CacheReadTokens
			existing.CacheCreateTokens += v.CacheCreateTokens
			existing.CostUSD += v.CostUSD
			existing.Errors += v.Errors
			existing.TotalDurationMs += v.TotalDurationMs
			out[display] = existing
		} else {
			out[display] = v
		}
	}
	return out
}

// remapDisplayNames rewrites snapshot display fields on log entries to their
// current values. The log is append-only and stores a point-in-time snapshot
// of the client name and auth label; when either is renamed, the UI should
// reflect the new name even for historical rows. Audit correlation stays
// keyed by stable IDs (ClientToken masked form, AuthID), which the log also
// carries untouched. If an ID no longer resolves (token / auth deleted), the
// snapshot is left in place as the last known display value.
func (h *Handler) remapDisplayNames(entries []requestlog.Record) {
	if len(entries) == 0 {
		return
	}
	nameByMasked := make(map[string]string)
	if h.tokens != nil {
		for _, t := range h.tokens.List() {
			n := strings.TrimSpace(t.Name)
			if n == "" {
				continue
			}
			nameByMasked[maskToken(t.Token)] = n
		}
	}
	var labelIdx map[string]auth.AuthLabelInfo
	if h.pool != nil {
		labelIdx = h.pool.LabelIndex()
	}
	for i := range entries {
		if cur, ok := nameByMasked[entries[i].ClientToken]; ok {
			entries[i].Client = cur
		}
		if entries[i].AuthID != "" && labelIdx != nil {
			if cur, ok := labelIdx[entries[i].AuthID]; ok {
				entries[i].AuthLabel = cur.Label
				entries[i].AuthKind = authKindString(cur.Kind)
			}
		}
	}
}

// applyDateBounds sets a Filter's window from the `from`/`to` strings the
// panel sends. Mirrors CPA-Claude's helper of the same name, which has had
// this since the earlier index work; hypitoken never got the port.
//
// Bare "YYYY-MM-DD" — which is all a date picker can produce — is passed
// through as day *labels* rather than converted to instants here. That is what
// lets requestlog answer from the pre-summed cube: the two forms describe the
// same window, but only the labelled one can be matched against the cube's day
// grain. Measured on this deployment's archive (1.09M rows, the panel's own
// 14-day window), the timestamp form materialises 481,989 rows and takes
// 3.2-4.0s for the four aggregates while the cube answers them in 34ms — and
// the endpoint demonstrated both at once, returning in 479ms with no window
// (a zero window IS cube-eligible) and 8.9s with one attached.
//
// Anything else (an RFC3339 instant, i.e. a sub-day window) still becomes a
// timestamp bound and takes the row-by-row path, which is the only path that
// can express it.
func applyDateBounds(f *requestlog.Filter, from, to string) {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	// Both bounds must take the same form. requestlog drops the labels when
	// timestamps are also set, so a mixed pair would silently widen the window
	// to whichever half survived; falling through to timestamps for both keeps
	// the window the caller asked for.
	if (isDayLabel(from) && isDayLabel(to)) || (isDayLabel(from) && to == "") || (from == "" && isDayLabel(to)) {
		f.FromDay, f.ToDay = from, to
		return
	}
	if from != "" {
		if t, err := parseDateBound(from, false); err == nil {
			f.From = t
		}
	}
	if to != "" {
		if t, err := parseDateBound(to, true); err == nil {
			f.To = t
		}
	}
}

// parseDims turns a comma-separated `dims` param into the aggregate set the
// caller actually reads. Each dimension is its own GROUP BY, so a panel that
// renders top-clients and top-models pays for a by-day rollup it never looks
// at — measurably, since cc-core reports 76.6ms for all four against 21.9ms
// for one on a 90-day window.
//
// An absent or unrecognised value means "all of them", which is what every
// existing caller gets today: the param can only ever narrow the response, so
// a client that does not know about it is unaffected.
func parseDims(s string) requestlog.Dims {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0 // zero means all, per requestlog.Filter.Dims
	}
	var d requestlog.Dims
	for _, part := range strings.Split(s, ",") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "summary":
			d |= requestlog.DimSummary
		case "by_client", "client":
			d |= requestlog.DimByClient
		case "by_model", "model":
			d |= requestlog.DimByModel
		case "by_day", "day":
			d |= requestlog.DimByDay
		}
	}
	return d
}

// isDayLabel reports whether s is exactly a "YYYY-MM-DD" calendar date.
func isDayLabel(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil && len(s) == len("2006-01-02")
}

// parseDateBound accepts "YYYY-MM-DD" (start-of-day) or full RFC3339.
// endOfDay=true shifts bare dates to 23:59:59 so `to=2026-04-14` covers
// the whole day.
func parseDateBound(s string, endOfDay bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, err
	}
	if endOfDay {
		return t.Add(24*time.Hour - time.Second), nil
	}
	return t, nil
}

func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "..", "")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

// ---- Client access token CRUD ----

// tokenView is the shape returned to the panel. The token itself is masked
// unless the caller asks for the full value (`?full=1`), in which case
// every row is returned verbatim — used by the "copy to clipboard" button
// in the Add-token modal right after creation.
type tokenView struct {
	Token         string     `json:"token"`
	Masked        string     `json:"masked"`
	Name          string     `json:"name"`
	WeeklyUSD     float64    `json:"weekly_usd"`
	MaxConcurrent int        `json:"max_concurrent,omitempty"`
	RPM           int        `json:"rpm,omitempty"`
	Group         string     `json:"group,omitempty"`
	Groups        []string   `json:"groups,omitempty"` // priority-ordered fallthrough channel list
	CreatedAt     *time.Time `json:"created_at,omitempty"`
	// Live usage for the current ISO week, convenient for the panel row.
	WeeklyUsedUSD float64 `json:"weekly_used_usd"`
}

func (h *Handler) handleListTokens(c *gin.Context) {
	full := c.Query("full") == "1"
	rows := h.tokens.List()
	out := make([]tokenView, 0, len(rows))
	for _, t := range rows {
		v := tokenView{
			Masked:        maskToken(t.Token),
			Name:          t.Name,
			WeeklyUSD:     t.WeeklyUSD,
			MaxConcurrent: t.MaxConcurrent,
			RPM:           t.RPM,
			Group:         t.Group,
			Groups:        append([]string(nil), t.Groups...),
			WeeklyUsedUSD: h.usage.WeeklyCostUSD(t.Token),
		}
		if !t.CreatedAt.IsZero() {
			ct := t.CreatedAt
			v.CreatedAt = &ct
		}
		if full {
			v.Token = t.Token
		}
		out = append(out, v)
	}
	c.JSON(http.StatusOK, gin.H{"tokens": out})
}

type createTokenBody struct {
	Token         string   `json:"token"`
	Name          string   `json:"name"`
	WeeklyUSD     float64  `json:"weekly_usd"`
	MaxConcurrent int      `json:"max_concurrent,omitempty"`
	RPM           int      `json:"rpm,omitempty"`
	Group         string   `json:"group,omitempty"`
	Groups        []string `json:"groups,omitempty"` // priority-ordered fallthrough list
	Generate      bool     `json:"generate"`         // if true and Token == "", mint a fresh sk-...
}

func (h *Handler) handleCreateToken(c *gin.Context) {
	var body createTokenBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tok := strings.TrimSpace(body.Token)
	if tok == "" {
		if !body.Generate {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "token required (or set generate:true)"})
			return
		}
		v, err := clienttoken.Generate()
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "generate: " + err.Error()})
			return
		}
		tok = v
	}
	entry := clienttoken.Token{
		Token:         tok,
		Name:          body.Name,
		WeeklyUSD:     body.WeeklyUSD,
		MaxConcurrent: body.MaxConcurrent,
		RPM:           body.RPM,
		Group:         body.Group,
		Groups:        body.Groups,
	}
	if err := h.tokens.Add(entry); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"token":      tok, // return the full value once so the panel can show it
		"name":       body.Name,
		"weekly_usd": body.WeeklyUSD,
	})
}

type patchTokenBody struct {
	Name          *string   `json:"name"`
	WeeklyUSD     *float64  `json:"weekly_usd"`
	MaxConcurrent *int      `json:"max_concurrent"`
	RPM           *int      `json:"rpm"`
	Group         *string   `json:"group"`
	Groups        *[]string `json:"groups"` // priority-ordered fallthrough list; nil = unchanged
}

func (h *Handler) handlePatchToken(c *gin.Context) {
	tok := c.Param("token")
	var body patchTokenBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.tokens.Update(tok, body.Name, body.WeeklyUSD, body.MaxConcurrent, body.RPM, body.Group, body.Groups, nil); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) handleDeleteToken(c *gin.Context) {
	tok := c.Param("token")
	if err := h.tokens.Delete(tok); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// orphanToken is a client token that appears in recorded usage but is
// not in the clienttoken store (deleted or never registered). Exposed to
// admins so the Edit dialog can offer a "inherit usage from …" merge.
type orphanToken struct {
	Token    string           `json:"token"`
	Masked   string           `json:"masked"`
	Label    string           `json:"label,omitempty"`
	Total    usage.ClientCost `json:"total"`
	LastUsed *time.Time       `json:"last_used,omitempty"`
}

func (h *Handler) handleListOrphanTokens(c *gin.Context) {
	registered := make(map[string]bool)
	for _, t := range h.tokens.List() {
		registered[t.Token] = true
	}
	out := make([]orphanToken, 0)
	for tok, pc := range h.usage.SnapshotClients() {
		if registered[tok] {
			continue
		}
		row := orphanToken{
			Token:  tok,
			Masked: maskToken(tok),
			Label:  pc.Label,
			Total:  pc.Total,
		}
		if !pc.LastUsed.IsZero() {
			lu := pc.LastUsed
			row.LastUsed = &lu
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"orphans": out})
}

func (h *Handler) handleResetToken(c *gin.Context) {
	tok := c.Param("token")
	newTok, err := clienttoken.Generate()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "generate: " + err.Error()})
		return
	}
	if err := h.tokens.Reset(tok, newTok); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Rename usage after the token store commits; if this fails the old
	// usage history stays under the old key (surfaces as orphan), but the
	// new token already works — non-fatal.
	if err := h.usage.RenameClient(tok, newTok); err != nil {
		log.Warnf("admin: reset usage rename %s→%s: %v", maskToken(tok), maskToken(newTok), err)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "token": newTok})
}

type inheritTokenBody struct {
	From string `json:"from"`
}

func (h *Handler) handleInheritToken(c *gin.Context) {
	dst := c.Param("token")
	var body inheritTokenBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	src := strings.TrimSpace(body.From)
	if src == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "from required"})
		return
	}
	if _, ok := h.tokens.Lookup(dst); !ok {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "destination token not registered"})
		return
	}
	// Refuse merging from a still-registered token: it's either a mistake
	// or the caller should delete the source explicitly first.
	if _, ok := h.tokens.Lookup(src); ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "source token is still registered; delete it first or pick an orphan"})
		return
	}
	if err := h.usage.MergeClient(src, dst); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// RegisterSaaSBridge mounts the legacy admin handlers that the SaaS UI needs
// (request-log queries + Anthropic OAuth quota probe) on the supplied group.
// Caller is responsible for attaching the right auth middleware — this method
// does NOT install the legacy admin_token gate. The expected use is binding
// it under /api/v2/admin/* after the SaaS RequireAdmin middleware.
func (h *Handler) RegisterSaaSBridge(g *gin.RouterGroup) {
	// The caller binds this group behind the SaaS RequireAdmin middleware, so
	// every request that reaches these handlers is a verified operator. Mark it
	// privileged here — the legacy adminAuth() gate (the only other place that
	// sets this) is never in the chain on the SaaS path, so without this the
	// read-only handlers redact operator-only detail: handleRequestsQuery would
	// null out res.Entries and the Requests tab renders empty. The /console
	// overview still anonymizes via its explicit anon=1 query param, which
	// handleRequestsQuery honors regardless of this flag.
	g.Use(func(c *gin.Context) {
		c.Set(ctxKeyPrivileged, true)
		c.Next()
	})
	g.GET("/requests", h.handleRequestsQuery)
	g.GET("/requests/clients", h.handleRequestsClients)
	g.GET("/requests/hourly", h.handleRequestsHourly)
	g.POST("/credentials/:id/anthropic-usage", h.handleAnthropicUsage)
	g.POST("/credentials/:id/codex-usage", h.handleCodexUsage)
	g.POST("/credentials/:id/codex-subscription", h.handleCodexSubscription)
	g.POST("/credentials/:id/reset-codex-credit", h.handleResetCodexCredit)
	// Rich credential read + mutations. Mirrors the legacy /summary fields so
	// the SaaS panel can render the same usage / quota / model_map / sparkline
	// data that the operator panel does. The saas/admin CredHandler still owns
	// create / oauth-start / oauth-finish / delete — those are not just
	// pass-throughs (they write JSON files into AuthDir).
	g.GET("/credentials", h.handleBridgeListCreds)
	g.GET("/credentials/:id/usage", h.handleBridgeCredUsage)
	// Upload a raw credential JSON (export from another instance, etc.). The
	// legacy handler writes the file into AuthDir and adds it to the shared
	// pool — the same pool the SaaS panel reads — so it bridges cleanly.
	g.POST("/credentials/upload", h.handleUpload)
	g.PATCH("/credentials/:id", h.handlePatchAuth)
	g.POST("/credentials/:id/refresh", h.handleRefresh)
	g.POST("/credentials/:id/clear-quota", h.handleClearQuota)
	g.POST("/credentials/:id/clear-failure", h.handleClearFailure)
}

// handleBridgeListCreds emits the rich credential rows for the SaaS panel
// (/api/v2/admin/credentials). Same shape as /summary's "auths" but without
// the clients/pricing fan-out — those have their own endpoints elsewhere.
func (h *Handler) handleBridgeListCreds(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"credentials": h.buildAuthRows()})
}

// credDailySeries builds the last-14-days spend series for one credential
// from the request log. Always exactly 14 consecutive days (missing days
// zero-filled) so the chart's bars align. Day labels live in
// requestlog.BucketLocation()'s zone — the same zone ByDay keys use — and
// the FromDay/ToDay form keeps the query cube-eligible on the SQLite index.
func (h *Handler) credDailySeries(id string) []credDay {
	today := time.Now().In(requestlog.BucketLocation())
	first := today.AddDate(0, 0, -13)
	res, err := requestlog.Query(requestlog.Filter{
		Dir:     h.cfg.LogDir,
		AuthID:  id,
		FromDay: first.Format("2006-01-02"),
		ToDay:   today.Format("2006-01-02"),
		Limit:   1, // aggregates only; Entries are unread
	})
	byDay := map[string]requestlog.Aggregate{}
	if err != nil {
		// A broken log query should degrade to an empty chart, not a 500 —
		// the rest of the usage block renders from the in-memory store.
		log.WithError(err).WithField("auth_id", id).Warn("admin: daily spend query failed")
	} else {
		byDay = res.ByDay
	}
	days := make([]credDay, 0, 14)
	for i := 0; i < 14; i++ {
		label := first.AddDate(0, 0, i).Format("2006-01-02")
		agg := byDay[label]
		days = append(days, credDay{
			Day:               label,
			InputTokens:       agg.InputTokens,
			OutputTokens:      agg.OutputTokens,
			CacheReadTokens:   agg.CacheReadTokens,
			CacheCreateTokens: agg.CacheCreateTokens,
			Requests:          agg.Count,
			Errors:            agg.Errors,
			CostUSD:           agg.CostUSD,
		})
	}
	return days
}

// handleBridgeCredUsage serves one credential's usage block including the
// 14-day daily series. Split out of the list response because only the
// detail dialog reads the series, and shipping it for every credential on
// every poll tripled the list payload.
func (h *Handler) handleBridgeCredUsage(c *gin.Context) {
	id := c.Param("id")
	for _, row := range h.buildAuthRows() {
		if row.ID == id {
			if row.Usage == nil {
				c.JSON(http.StatusOK, gin.H{"usage": nil})
				return
			}
			row.Usage.Daily = h.credDailySeries(id)
			c.JSON(http.StatusOK, gin.H{"usage": row.Usage})
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
}
