package adapter

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	legacyadmin "github.com/wjsoj/CPA-Claude/internal/admin"
	"github.com/wjsoj/CPA-Claude/internal/saas/admin"
	"github.com/wjsoj/CPA-Claude/internal/saas/analytics"
	"github.com/wjsoj/CPA-Claude/internal/saas/arena"
	saasauth "github.com/wjsoj/CPA-Claude/internal/saas/auth"
	"github.com/wjsoj/CPA-Claude/internal/saas/billing"
	"github.com/wjsoj/CPA-Claude/internal/saas/db"
	"github.com/wjsoj/CPA-Claude/internal/saas/growth"
	"github.com/wjsoj/CPA-Claude/internal/saas/profile"
	"github.com/wjsoj/CPA-Claude/internal/saas/referral"
	"github.com/wjsoj/CPA-Claude/internal/saas/support"
	"github.com/wjsoj/CPA-Claude/internal/saas/tokens"
	"github.com/wjsoj/CPA-Claude/internal/saas/usage"
	"github.com/wjsoj/CPA-Claude/internal/saas/workspace"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/requestlog"
)

// Mount attaches all /api/v2/* SaaS routes onto engine. Public-routes (auth
// + billing notify) sit outside RequireUser. credH may be nil — when set,
// /api/v2/admin/credentials/* is exposed. legacyH may be nil — when set, the
// /api/v2/admin/* group also exposes request-log queries + Anthropic OAuth
// quota probe (handlers reused from the legacy operator API).
//
// svcH may be nil — when set (saas.service_tokens is configured) the
// machine-to-machine /api/v2/svc/* group is mounted for the sibling HypiHub
// service. See service.go.
//
// ssoH may be nil — when set (saas.sso_return_origins is non-empty) the
// cross-origin single-sign-on minting route POST /api/v2/auth/sso/code is
// mounted on the authenticated group. See sso.go.
//
// referralsEnabled is the master switch for the invite / referral / marketing-
// attribution programme (saas.referrals_enabled, default false — suspended
// after the 2026-08-08 farming incident). When false the user-facing referral
// routes and the ?ref= attribution beacons are not mounted at all; the admin
// routes that read the historical channel / campaign / conversion data stay
// mounted either way, so the operator can still audit what was granted.
func Mount(engine *gin.Engine, store *db.DB, authH *saasauth.Handler, tokensH *tokens.Handler, billingH *billing.Handler, adminH *admin.Handler, credH *admin.CredHandler, iss *saasauth.Issuer, legacyH *legacyadmin.Handler, logDir string, catalog *pricing.Catalog, growthH *growth.Service, analyticsH *analytics.Service, arenaH *arena.Service, profileH *profile.Handler, referralH *referral.Service, workspaceH *workspace.Handler, supportH *support.Service, svcH *ServiceHandler, ssoH *SSOHandler, referralsEnabled bool) {
	v2 := engine.Group("/api/v2")

	// Service-to-service (/api/v2/svc/*) — the sibling HypiHub gateway sharing
	// these accounts and wallets. Mounted ONLY when saas.service_tokens is
	// non-empty: svcH is nil otherwise and the entire group ceases to exist,
	// which is the right default for every deployment that doesn't run HypiHub.
	// Registered before the user-facing groups purely so it sits outside them —
	// it authenticates by X-Service-Token, never by a user JWT.
	svcH.Mount(v2)

	// Public.
	v2.GET("/site", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	v2.GET("/exchange-rate", billingH.UserRateRouteShim())
	authG := v2.Group("/auth")
	authH.Routes(authG)
	billingH.PublicRoutes(v2)

	// Growth (marketing attribution) — public, unauthenticated ?ref= visit/dwell
	// tracking beacons. nil when the module is disabled; also skipped entirely
	// while the referral programme is suspended, so no attribution is recorded
	// and no signup can be credited to a channel. (Site-wide visitor analytics
	// below is a separate module and keeps running.)
	if growthH != nil && referralsEnabled {
		growthH.PublicRoutes(v2)
	}
	// Analytics (site-wide visitor behaviour) — public, unauthenticated
	// pageview/action/dwell beacons. nil when the module is disabled.
	if analyticsH != nil {
		analyticsH.PublicRoutes(v2)
	}

	// Support appeals — public by necessity. A disabled account cannot pass
	// RequireUser, so the one channel it has left has to live out here and
	// authenticate per-request with an emailed OTP.
	if supportH != nil {
		supportH.PublicRoutes(v2)
	}

	// Arena SSE office stream — registered on the public group because it does
	// its own JWT auth (EventSource can't send an Authorization header, so the
	// token rides the ?access_token= query parameter). The leaderboard itself
	// is a normal authed GET, registered below.
	if arenaH != nil {
		arenaH.PublicRoutes(v2)
	}

	// Authenticated.
	authed := v2.Group("")
	authed.Use(saasauth.RequireUser(iss, store))
	authed.GET("/me", func(c *gin.Context) {
		u := saasauth.CurrentUser(c)
		if u == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthenticated"})
			return
		}
		g, _ := store.GetGroup(c.Request.Context(), u.GroupID)
		user := gin.H{
			"id": u.ID, "email": u.Email, "role": u.Role,
			"balance_usd": u.BalanceUSD, "group_id": u.GroupID,
			"email_verified": u.EmailVerified, "created_at": u.CreatedAt.Unix(),
		}
		// Attach the public-arena profile (nickname + opt-in) so the dashboard
		// can greet the user by name without a second round-trip. Lazily created.
		if p, perr := store.GetOrCreateProfile(c.Request.Context(), u.ID); perr == nil {
			user["display_name"] = p.DisplayName
			user["name_is_default"] = p.NameIsDefault
			user["public_opt_in"] = p.PublicOptIn
		}
		// Workspaces the user belongs to (personal first, then any enterprise
		// spaces) with their role — drives the token billing-target picker and
		// conditional team-management nav.
		wsOut := []gin.H{}
		if list, werr := store.ListWorkspacesForUser(c.Request.Context(), u.ID); werr == nil {
			for _, w := range list {
				cm, xm := w.ClaudeMultiplier, w.CodexMultiplier
				if cm <= 0 {
					cm = defaultClaudeMultiplier
				}
				if xm <= 0 {
					xm = defaultCodexMultiplier
				}
				wsOut = append(wsOut, gin.H{
					"id": w.WorkspaceID, "name": w.Name, "type": w.Type, "role": w.Role,
					"claude_multiplier": cm, "codex_multiplier": xm,
				})
			}
		}
		user["workspaces"] = wsOut
		c.JSON(http.StatusOK, gin.H{"user": user, "group": g})
	})
	// Cross-origin SSO handoff (POST /auth/sso/code). Registered on the AUTHED
	// group even though its sibling path /auth/* is public: the code it returns
	// is a session, so only the holder of that session may ask for one. nil —
	// and therefore absent — unless saas.sso_return_origins lists an origin.
	ssoH.AuthedRoutes(authed)
	tokensH.Routes(authed.Group("/tokens"))
	billingH.UserRoutes(authed.Group("/billing"))
	// Spend analytics. Only needs the store, so it's built here rather than
	// lengthening Mount's already-long parameter list. The same handler serves the
	// team view further down, under RequireWorkspaceAdmin.
	usageH := usage.New(store)
	usageH.PersonalRoutes(authed.Group("/me/usage"))
	// Arena leaderboard + profile (nickname / public opt-in / IP greeting) —
	// all authed user routes.
	if arenaH != nil {
		arenaH.AuthedRoutes(authed)
	}
	if profileH != nil {
		profileH.Routes(authed)
	}
	// Referral (invite cards + peer gifting) — authed user routes. Unmounted
	// while the programme is suspended (farmed for signup credit on
	// 2026-08-08: 168 signups, ~$116), so the whole user-facing invite surface
	// disappears rather than 404-ing feature by feature.
	if referralH != nil && referralsEnabled {
		referralH.UserRoutes(authed)
	}

	// Available credential channels — the dropdown source for the per-token
	// "渠道" selector. Deduplicated by group name; each entry reports which
	// provider(s) back it and how many usable credentials it currently has.
	// "Usable" = not Disabled and not HardFailure — credentials in cooldown
	// (quota / rate-limit) still count, since they recover automatically.
	// The empty-string group (public pool) is exposed as "default".
	authed.GET("/channels", func(c *gin.Context) {
		if credH == nil || credH.Pool == nil {
			c.JSON(http.StatusOK, gin.H{"channels": []any{}})
			return
		}
		type chanInfo struct {
			Name      string   `json:"name"`
			Providers []string `json:"providers"`
			Count     int      `json:"count"`
		}
		acc := map[string]*chanInfo{}
		provSeen := map[string]map[string]bool{}
		for _, s := range credH.Pool.Status() {
			a := s.Auth
			if a.Disabled {
				continue
			}
			if live := credH.Pool.FindByID(a.ID); live != nil {
				if _, hardFail, _, _ := live.HealthSnapshot(); hardFail {
					continue
				}
			}
			name := a.Group
			if name == "" {
				name = "default"
			}
			ci, ok := acc[name]
			if !ok {
				ci = &chanInfo{Name: name}
				acc[name] = ci
				provSeen[name] = map[string]bool{}
			}
			ci.Count++
			prov := a.Provider
			if prov != "" && !provSeen[name][prov] {
				provSeen[name][prov] = true
				ci.Providers = append(ci.Providers, prov)
			}
		}
		out := make([]*chanInfo, 0, len(acc))
		for _, ci := range acc {
			sort.Strings(ci.Providers)
			out = append(out, ci)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Name == "default" {
				return false
			}
			if out[j].Name == "default" {
				return true
			}
			return out[i].Name < out[j].Name
		})
		c.JSON(http.StatusOK, gin.H{"channels": out})
	})

	// Per-user request log. Same shape as /admin/api/requests but filtered
	// to records emitted while the requester was the authenticated user —
	// powers the /app/logs page where customers reconcile every charge
	// against the catalog rate. Read-only, no mutation surface.
	authed.GET("/me/requests", func(c *gin.Context) {
		u := saasauth.CurrentUser(c)
		if u == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthenticated"})
			return
		}
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
		offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
		if limit <= 0 || limit > 200 {
			limit = 50
		}
		f := requestlog.Filter{
			Dir:    logDir,
			UserID: u.ID,
			Limit:  limit,
			Offset: offset,
		}
		// Optional secondary filters — useful for users wanting to drill
		// down to a single token or model.
		if mtok := c.Query("model"); mtok != "" {
			f.Model = mtok
		}
		if prov := c.Query("provider"); prov != "" {
			f.Provider = prov
		}
		if ct := c.Query("client_token"); ct != "" {
			f.ClientToken = ct
		}
		res, err := requestlog.Query(f)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// Attach the price card for every distinct (provider, model) that
		// appears so the frontend can render a per-row "how was this
		// charged" breakdown without a follow-up RPC. Keys are canonical
		// "<provider>/<model>" matching catalog.Lookup so the frontend can
		// resolve via the shared lookupPrice helper — keying by bare model
		// would silently fall back to the global default for OpenAI rows
		// (since the catalog stores them under "openai/...").
		prices := map[string]gin.H{}
		seen := map[string]struct{}{}
		add := func(provider, model string) {
			if model == "" {
				return
			}
			prov := provider
			if prov == "" {
				prov = auth.ProviderAnthropic // legacy records pre-dating the field
			}
			key := prov + "/" + model
			if _, ok := seen[key]; ok {
				return
			}
			seen[key] = struct{}{}
			p := catalog.Lookup(prov, model)
			prices[key] = gin.H{
				"input_per_1m":        p.InputPer1M,
				"output_per_1m":       p.OutputPer1M,
				"cache_read_per_1m":   p.CacheReadPer1M,
				"cache_create_per_1m": p.CacheCreatePer1M,
			}
		}
		for _, e := range res.Entries {
			add(e.Provider, e.Model)
		}
		c.JSON(http.StatusOK, gin.H{
			"summary":  res.Summary,
			"by_model": res.ByModel,
			"entries":  res.Entries,
			"scanned":  res.Scanned,
			"pricing":  prices,
		})
	})

	// Per-user console summary — account-level aggregates powering the
	// /app/console "个人" tab. Mirrors the platform KPI shape (requests /
	// tokens-in / tokens-out / total) but scoped to this user's own request
	// log via the UserID filter. `total` is all-time; `today` is the current
	// UTC day's bucket from ByDay (zero Aggregate when the user has no traffic
	// today). Same redaction posture as /me/requests — a user only ever sees
	// their own rows, never anyone else's or any fleet/credential detail.
	authed.GET("/me/console", func(c *gin.Context) {
		u := saasauth.CurrentUser(c)
		if u == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthenticated"})
			return
		}
		// Limit: 1 — we only need the aggregates (Summary/ByDay/ByModel), not
		// the entry page; the maps are computed over every matched record
		// regardless of Limit.
		res, err := requestlog.Query(requestlog.Filter{Dir: logDir, UserID: u.ID, Limit: 1})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		nowUTC := time.Now().UTC()
		today := nowUTC.Format("2006-01-02")
		// Actual spend comes from the wallet ledger (charge rows = official ×
		// pricing-group multiplier), NOT requestlog.CostUSD, which records the
		// *official* price before the discount — requestlog.BilledUSD is the
		// post-discount figure, and Record.BilledOrCost() reads it with a
		// fallback for rows written before the two were separate columns.
		// spent_total/spent_today are the figures the user truly paid, so the
		// dashboard "累计消费" and this tab agree.
		spentTotal, _ := store.SumChargeSince(c.Request.Context(), u.ID, time.Time{})
		// spent_today opens on the same billing day PreCheck enforces the daily
		// cap over (adapter.go). These two have to agree: if the cap counts a
		// UTC+8 day while the dashboard counts a UTC one, a user rejected for
		// exceeding their daily limit sees a "spent today" figure well under it
		// and has no way to make sense of the refusal. (today_key/by_day below
		// stay on UTC days — they index a requestlog aggregation keyed that
		// way, and they measure official cost rather than charged amount.)
		spentToday, _ := store.SumChargeSince(c.Request.Context(), u.ID, db.BillingDayStart(time.Now()))
		c.JSON(http.StatusOK, gin.H{
			"total":       res.Summary,
			"today":       res.ByDay[today],
			"today_key":   today,
			"by_model":    res.ByModel,
			"by_day":      res.ByDay,
			"balance_usd": u.BalanceUSD,
			"spent_total": spentTotal,
			"spent_today": spentToday,
		})
	})

	// Public price catalogue (read-only, used by the landing /pricing page).
	//
	// The page used to carry its own hardcoded copy of this table. That is a
	// second source of truth for the one number customers check before paying,
	// and it drifted: claude-opus-5 and claude-fable-5 were being billed at
	// $5/$25 and $10/$50 while appearing nowhere on the published price list,
	// and the gpt-5.6 tiers advertised no cache-write rate although the
	// catalogue charges one. Serving the catalogue itself means a model can be
	// priced but unlisted only if it is also absent from the billing path.
	//
	// Shape mirrors the operator panel's `pricing` block so both consume one
	// serialization. Unauthenticated by design — these are list prices, already
	// public on the vendors' own sites, and the page is reachable logged-out.
	// Nothing here is per-account: the multiplier that turns a list price into
	// what a given user pays comes from /groups.
	v2.GET("/pricing", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"default":           catalog.Default(),
			"provider_defaults": catalog.ProviderDefaults(),
			"models":            catalog.Models(),
		})
	})

	// Public groups (read-only, used on landing/pricing pages).
	v2.GET("/groups", func(c *gin.Context) {
		gs, err := store.ListGroups(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"groups": gs})
	})

	// Public health snapshot for /status. Two row kinds:
	//   - OAuth: aggregated to one row per (provider, model). Multiple OAuth
	//     credentials backing the same model are an implementation detail;
	//     the public page just needs "can the model be served?".
	//   - API key: one row per credential, like the operator panel. API keys
	//     are usually pinned to specific upstream gateways (tcdmx / fucheers
	//     / etc.) which fail independently, so we keep them split out.
	// Rows whose auth_id is no longer in the live pool are dropped — they
	// belong to deleted credentials.
	v2.GET("/health", func(c *gin.Context) {
		hs, err := store.ListModelHealth(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// Build live-pool maps from credH.Pool. When credH is nil (unwired),
		// fall through with no pool filtering — every row is treated as live.
		liveKind := map[string]auth.Kind{}
		havePool := credH != nil && credH.Pool != nil
		if havePool {
			for _, st := range credH.Pool.Status() {
				liveKind[st.Auth.ID] = st.Auth.Kind
			}
		}

		// Partition: OAuth rows go to aggregation groups, API-key rows go
		// straight to the output. Rows missing from the live pool (deleted
		// creds) are skipped.
		type groupKey struct{ provider, model string }
		oauthGroups := map[groupKey][]*db.ModelHealth{}
		apikeyRows := make([]*db.ModelHealth, 0)
		for _, rec := range hs {
			if havePool {
				k, ok := liveKind[rec.AuthID]
				if !ok {
					continue
				}
				if k == auth.KindOAuth {
					oauthGroups[groupKey{provider: rec.Provider, model: rec.Model}] = append(
						oauthGroups[groupKey{provider: rec.Provider, model: rec.Model}], rec,
					)
				} else {
					apikeyRows = append(apikeyRows, rec)
				}
			} else {
				// Best-effort fallback: treat unknown rows as their own row
				// (no aggregation), so something still renders.
				apikeyRows = append(apikeyRows, rec)
			}
		}

		// Stable display-name counter for API-key rows (same scheme as the
		// operator panel and the previous public endpoint).
		counters := map[string]int{}
		apikeyName := func(provider string) string {
			prov := "claude"
			if provider == auth.ProviderOpenAI {
				prov = "codex"
			}
			counters[prov]++
			return fmt.Sprintf("%s-api-%03d", prov, counters[prov])
		}

		// Stable order: Anthropic before OpenAI, OAuth aggregates before
		// API-key singletons within each provider, then by model name.
		oauthKeys := make([]groupKey, 0, len(oauthGroups))
		for k := range oauthGroups {
			oauthKeys = append(oauthKeys, k)
		}
		sort.Slice(oauthKeys, func(i, j int) bool {
			if oauthKeys[i].provider != oauthKeys[j].provider {
				return oauthKeys[i].provider < oauthKeys[j].provider
			}
			return oauthKeys[i].model < oauthKeys[j].model
		})
		sort.SliceStable(apikeyRows, func(i, j int) bool {
			if apikeyRows[i].Provider != apikeyRows[j].Provider {
				return apikeyRows[i].Provider < apikeyRows[j].Provider
			}
			if apikeyRows[i].Model != apikeyRows[j].Model {
				return apikeyRows[i].Model < apikeyRows[j].Model
			}
			return apikeyRows[i].AuthID < apikeyRows[j].AuthID
		})

		const bucketSec = 300 // 5-minute buckets for merged OAuth history

		out := make([]gin.H, 0, len(oauthGroups)+len(apikeyRows))
		nextID := 1

		// OAuth aggregates.
		for _, k := range oauthKeys {
			recs := oauthGroups[k]
			anyOK := false
			latSum, latN := 0, 0
			var newestChecked int64
			for _, r := range recs {
				if r.Status == "ok" {
					anyOK = true
					if r.LatencyMs > 0 {
						latSum += r.LatencyMs
						latN++
					}
				}
				if r.CheckedAt.Unix() > newestChecked {
					newestChecked = r.CheckedAt.Unix()
				}
			}
			status := "fail"
			if anyOK {
				status = "ok"
			}
			meanLat := 0
			if latN > 0 {
				meanLat = latSum / latN
			}

			type bucket struct {
				okCount, failCount int
				latSum, latN       int
				ts                 int64
			}
			buckets := map[int64]*bucket{}
			for _, r := range recs {
				hist, _ := store.ListModelHealthHistory(c.Request.Context(), r.AuthID, r.Model, 90)
				for _, h := range hist {
					ts := h.CheckedAt.Unix()
					bkey := ts / bucketSec
					b := buckets[bkey]
					if b == nil {
						b = &bucket{ts: ts}
						buckets[bkey] = b
					}
					if ts > b.ts {
						b.ts = ts
					}
					if h.Status == "ok" {
						b.okCount++
						if h.LatencyMs > 0 {
							b.latSum += h.LatencyMs
							b.latN++
						}
					} else {
						b.failCount++
					}
				}
			}
			bkeys := make([]int64, 0, len(buckets))
			for bk := range buckets {
				bkeys = append(bkeys, bk)
			}
			sort.Slice(bkeys, func(i, j int) bool { return bkeys[i] < bkeys[j] })
			if len(bkeys) > 90 {
				bkeys = bkeys[len(bkeys)-90:]
			}
			histSlice := make([]gin.H, 0, len(bkeys))
			for _, bk := range bkeys {
				b := buckets[bk]
				bSt := "fail"
				if b.okCount > 0 {
					bSt = "ok"
				}
				bLat := 0
				if b.latN > 0 {
					bLat = b.latSum / b.latN
				}
				histSlice = append(histSlice, gin.H{
					"status":     bSt,
					"latency_ms": bLat,
					"checked_at": b.ts,
				})
			}

			out = append(out, gin.H{
				"id":           nextID,
				"display_name": k.model,
				"provider":     k.provider,
				"model":        k.model,
				"kind":         "oauth",
				"status":       status,
				"latency_ms":   meanLat,
				"checked_at":   newestChecked,
				"history":      histSlice,
				"oauth_count":  len(recs),
			})
			nextID++
		}

		// API-key singletons.
		for _, rec := range apikeyRows {
			hist, _ := store.ListModelHealthHistory(c.Request.Context(), rec.AuthID, rec.Model, 90)
			histSlice := make([]gin.H, 0, len(hist))
			for _, r := range hist {
				histSlice = append(histSlice, gin.H{
					"status":     r.Status,
					"latency_ms": r.LatencyMs,
					"checked_at": r.CheckedAt.Unix(),
				})
			}
			out = append(out, gin.H{
				"id":           nextID,
				"display_name": apikeyName(rec.Provider),
				"provider":     rec.Provider,
				"model":        rec.Model,
				"kind":         "apikey",
				"status":       rec.Status,
				"latency_ms":   rec.LatencyMs,
				"checked_at":   rec.CheckedAt.Unix(),
				"history":      histSlice,
				// `error` intentionally omitted — operator-only detail.
			})
			nextID++
		}

		c.JSON(http.StatusOK, gin.H{"checks": out, "as_of": time.Now().Unix()})
	})

	// Public per-provider availability monitor. Aggregates ALL credentials of a
	// provider into a single line (no oauth/api or per-model split), exposing
	// two uptime strips: a fine-grained recent timeline (10-minute slots over
	// the last 24h) and a daily rollup (last 14 days). This is the status-page
	// data source; it answers "is Claude / Codex usable right now and how has
	// availability looked" without leaking per-credential detail.
	v2.GET("/health/monitor", func(c *gin.Context) {
		const (
			recentSlots = 144 // 24h / 10min
			recentSlotS = int64(600)
			dailyDays   = 30
			daySec      = int64(86400)
		)
		now := time.Now()
		nowU := now.Unix()
		recentStart := nowU - int64(recentSlots)*recentSlotS

		// Current status per provider from the live model_health rows.
		curr, _ := store.ListModelHealth(c.Request.Context())
		type pcount struct {
			ok, total int
			checkedAt int64
		}
		cur := map[string]*pcount{}
		for _, r := range curr {
			p := cur[r.Provider]
			if p == nil {
				p = &pcount{}
				cur[r.Provider] = p
			}
			p.total++
			if r.Status == "ok" {
				p.ok++
			}
			if r.CheckedAt.Unix() > p.checkedAt {
				p.checkedAt = r.CheckedAt.Unix()
			}
		}

		providers := []struct{ key, name string }{
			{auth.ProviderAnthropic, "Claude"},
			{auth.ProviderOpenAI, "Codex"},
		}
		out := make([]gin.H, 0, len(providers))
		for _, pv := range providers {
			cc := cur[pv.key]
			if cc == nil || cc.total == 0 {
				continue // no credentials for this provider — omit the card
			}
			// The pill answers one question: can a new user use the service right
			// now? Yes as long as AT LEAST ONE credential is healthy — the pool
			// routes around the dead ones. It is NOT a credential-health ratio:
			// many creds may sit dead/quota-exceeded while the service stays fully
			// usable, so we report operational whenever any credential can serve.
			operational := "down"
			if cc.ok > 0 {
				operational = "operational"
			}

			samples, _ := store.ProviderHealthSamples(c.Request.Context(), pv.key, time.Unix(recentStart, 0).Add(-time.Duration(dailyDays)*24*time.Hour))

			// Recent 10-minute slots (a slot is "up" if ANY credential was ok in it).
			recent := make([]gin.H, recentSlots)
			rOK := make([]int, recentSlots)
			rTot := make([]int, recentSlots)
			// Daily rollup measures service uptime, NOT credential health: a day's
			// {ok,total} count 10-min slots where AT LEAST ONE credential was ok vs.
			// slots with any sample. Counting raw per-credential samples instead
			// would peg every day red whenever most creds sit dead/quota-exceeded
			// (e.g. 5 healthy of 17 → ratio≈0.29) even while the service stayed
			// fully usable — matching the "any cred ok" semantics of recent slots.
			type slotAgg struct{ okCount, total int }
			daySlots := map[int64]map[int64]*slotAgg{} // dayMidnight -> slotStart -> agg
			for _, s := range samples {
				ts := s.CheckedAt.Unix()
				if ts >= recentStart && ts <= nowU {
					idx := int((ts - recentStart) / recentSlotS)
					if idx >= 0 && idx < recentSlots {
						rTot[idx]++
						if s.Status == "ok" {
							rOK[idx]++
						}
					}
				}
				d := (ts / daySec) * daySec
				slot := (ts / recentSlotS) * recentSlotS
				m := daySlots[d]
				if m == nil {
					m = map[int64]*slotAgg{}
					daySlots[d] = m
				}
				ag := m[slot]
				if ag == nil {
					ag = &slotAgg{}
					m[slot] = ag
				}
				ag.total++
				if s.Status == "ok" {
					ag.okCount++
				}
			}
			// Overlay real traffic onto the probe's verdict.
			//
			// The strip is built from health-probe samples, which answer "did a
			// synthetic request succeed". That stays green straight through a
			// capacity shed: the probe is one small request and the shed lands
			// on real turns under real load. Production had gpt-6-astra shedding
			// 30.7% of turns while this page reported the provider operational,
			// because the probe aims at gpt-5.6-sol.
			//
			// So each slot also carries what actually happened to customer
			// traffic in it. Slots with no traffic are left alone — the map is
			// sparse on purpose, so "nobody asked" stays distinct from "people
			// asked and it was fine".
			shedByStart := map[int64]requestlog.ShedBucket{}
			if logDir != "" {
				if st, err := requestlog.OpenStoreForRead(logDir); err == nil {
					buckets, berr := st.ShedBucketsSince(pv.key,
						time.Unix(recentStart, 0), time.Duration(recentSlotS)*time.Second)
					if berr == nil {
						for _, b := range buckets {
							shedByStart[b.Start.Unix()] = b
						}
					}
					st.Close()
				}
			}

			for i := 0; i < recentSlots; i++ {
				from := recentStart + int64(i)*recentSlotS
				slot := gin.H{"from": from, "ok": rOK[i], "total": rTot[i]}
				if b, ok := shedByStart[from]; ok && b.Requests > 0 {
					// Counts, not a rate: the threshold for "unstable" is a
					// presentation decision and belongs with the thing that
					// draws the strip, not with the thing that measures it.
					slot["reqs"] = b.Requests
					slot["shed"] = b.Shed
				}
				recent[i] = slot
			}
			todayMidnight := (nowU / daySec) * daySec
			daily := make([]gin.H, dailyDays)
			for i := 0; i < dailyDays; i++ {
				d := todayMidnight - int64(dailyDays-1-i)*daySec
				upSlots, totSlots := 0, 0
				for _, ag := range daySlots[d] {
					totSlots++
					if ag.okCount > 0 {
						upSlots++
					}
				}
				daily[i] = gin.H{"date": d, "ok": upSlots, "total": totSlots}
			}

			// healthy_creds/total_creds are deliberately omitted: the public
			// status page reports service usability, not credential-pool size.
			out = append(out, gin.H{
				"key":         pv.key,
				"name":        pv.name,
				"operational": operational,
				"checked_at":  cc.checkedAt,
				"recent":      recent,
				"daily":       daily,
			})
		}
		c.JSON(http.StatusOK, gin.H{"providers": out, "as_of": nowU})
	})

	// Fleet-wide wallet aggregate is exposed to any signed-in user — it
	// powers the "Saved by us" tile on the operator console which itself
	// is open to all users (per the SSO design). No PII, just sums.
	authed.GET("/admin/wallet-totals", adminH.WalletTotalsHandler())

	// Support desk for signed-in users.
	if supportH != nil {
		supportH.UserRoutes(authed)
		supportH.InvoiceRoutes(authed)
	}

	// Workspace (enterprise space) team management. Invite accept is open to any
	// signed-in user; the team-management subtree is gated per-workspace to that
	// space's own admins (a customer-facing role — NO fleet/credential access).
	if workspaceH != nil {
		workspaceH.AcceptRoutes(authed)
		teamG := authed.Group("/workspaces/:id")
		teamG.Use(saasauth.RequireWorkspaceAdmin(store))
		workspaceH.TeamRoutes(teamG)
		// Per-key spend for the whole space — same reports as the personal view,
		// scoped to the workspace the middleware just authorized.
		usageH.TeamRoutes(teamG.Group("/usage"))
	}

	// Admin (operator-only).
	adminG := authed.Group("/admin")
	adminG.Use(saasauth.RequireAdmin())
	adminH.Routes(adminG)
	adminH.WorkspaceRoutes(adminG)
	if credH != nil {
		credH.Routes(adminG)
	}
	// Admin/audit views over the historical channel + campaign + conversion
	// data. Deliberately mounted even while the programme is suspended — the
	// operator still has to audit what was already granted, and these are the
	// re-enable switches.
	if growthH != nil {
		growthH.AdminRoutes(adminG)
	}
	if referralH != nil {
		referralH.AdminRoutes(adminG)
	}
	if supportH != nil {
		supportH.AdminRoutes(adminG)
	}
	if analyticsH != nil {
		analyticsH.AdminRoutes(adminG)
	}
	if legacyH != nil {
		legacyH.RegisterSaaSBridge(adminG)
	}
}
