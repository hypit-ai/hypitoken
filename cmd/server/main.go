package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/wjsoj/CPA-Claude/internal/admin"
	"github.com/wjsoj/CPA-Claude/internal/config"
	"github.com/wjsoj/CPA-Claude/internal/logging"
	"github.com/wjsoj/CPA-Claude/internal/saas"
	saasadapter "github.com/wjsoj/CPA-Claude/internal/saas/adapter"
	saasadmin "github.com/wjsoj/CPA-Claude/internal/saas/admin"
	"github.com/wjsoj/CPA-Claude/internal/saas/analytics"
	"github.com/wjsoj/CPA-Claude/internal/saas/arena"
	saasauth "github.com/wjsoj/CPA-Claude/internal/saas/auth"
	"github.com/wjsoj/CPA-Claude/internal/saas/billing"
	saasdb "github.com/wjsoj/CPA-Claude/internal/saas/db"
	"github.com/wjsoj/CPA-Claude/internal/saas/growth"
	"github.com/wjsoj/CPA-Claude/internal/saas/health"
	"github.com/wjsoj/CPA-Claude/internal/saas/mail"
	saasprofile "github.com/wjsoj/CPA-Claude/internal/saas/profile"
	"github.com/wjsoj/CPA-Claude/internal/saas/referral"
	"github.com/wjsoj/CPA-Claude/internal/saas/support"
	"github.com/wjsoj/CPA-Claude/internal/saas/tokens"
	saasworkspace "github.com/wjsoj/CPA-Claude/internal/saas/workspace"
	"github.com/wjsoj/CPA-Claude/internal/server"
	"github.com/wjsoj/CPA-Claude/internal/shop"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/clienttoken"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/requestlog"
	"github.com/wjsoj/cc-core/usage"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	// Subcommand dispatch (before flag.Parse so the default server path is
	// untouched). os.Args[1] is never a flag in normal use — --config/--version
	// start with "-" — so this is unambiguous and existing invocations
	// (including the systemd ExecStart=... --config ...) keep working.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backup":
			runBackupCmd(os.Args[2:])
			return
		case "restore":
			runRestoreCmd(os.Args[2:])
			return
		case "export-requests":
			runExportRequestsCmd(os.Args[2:])
			return
		case "reconcile-charges":
			runReconcileChargesCmd(os.Args[2:])
			return
		}
	}

	configPath := flag.String("config", "config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("cpa-claude %s (commit=%s built=%s)\n", version, commit, buildDate)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}
	logging.Setup(cfg.LogLevel)

	log.Infof("loading credentials from %s", cfg.AuthDir)
	oauths, apikeysFromDir, err := auth.LoadAuthDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auth dir: %v", err)
	}
	log.Infof("loaded %d OAuth credential(s)", len(oauths))
	for _, a := range oauths {
		log.Infof("  - %s (label=%s proxy=%q max_concurrent=%d)", a.ID, a.Label, a.ProxyURL, a.MaxConcurrent)
	}
	log.Infof("loaded %d API-key credential(s) from auth_dir", len(apikeysFromDir))
	for _, a := range apikeysFromDir {
		log.Infof("  - %s (label=%s proxy=%q)", a.ID, a.Label, a.ProxyURL)
	}

	apikeys := apikeysFromDir
	for i, k := range cfg.APIKeys {
		if strings.TrimSpace(k.Key) == "" {
			continue
		}
		label := k.Label
		if label == "" {
			label = fmt.Sprintf("apikey-%d", i+1)
		}
		proxy := k.ProxyURL
		if proxy == "" {
			proxy = cfg.DefaultProxyURL
		}
		apikeys = append(apikeys, &auth.Auth{
			ID:          "apikey:" + label,
			Kind:        auth.KindAPIKey,
			Provider:    auth.NormalizeProvider(k.Provider),
			Label:       label,
			AccessToken: k.Key,
			ProxyURL:    proxy,
			BaseURL:     k.BaseURL,
			Group:       auth.NormalizeGroup(k.Group),
			ModelMap:    k.ModelMap,
		})
	}
	log.Infof("loaded %d API key(s)", len(apikeys))

	if len(oauths) == 0 && len(apikeys) == 0 {
		if strings.TrimSpace(cfg.AdminToken) == "" {
			log.Fatalf("no upstream credentials configured and admin panel is disabled — add credentials to auth_dir or set admin_token to bootstrap from the panel")
		}
		log.Warn("no upstream credentials loaded; waiting for admin panel uploads")
	}

	store, err := usage.Open(cfg.StateFile)
	if err != nil {
		log.Fatalf("open state file: %v", err)
	}

	var reqLog *requestlog.Writer
	var logIndex *requestlog.Store
	if cfg.LogDir != "" {
		// The index opens FIRST because the writer may depend on it: with the
		// JSONL archive off it is the only place a record can go, and
		// OpenWithOptions refuses to start without it.
		//
		// Without it every aggregate re-parses the whole archive. That is
		// worse here than on the operator panel: each customer opening their
		// own usage page triggers a full-archive scan of its own, and the
		// health checker scans per credential.
		//
		// While the archive is on the index is derived state, so a failure is
		// logged and ignored — the query paths fall back to scanning on their
		// own.
		if !cfg.LogIndexDisabled {
			if st, err := requestlog.OpenStore(cfg.LogDir); err != nil {
				log.Warnf("request log: index unavailable (%v); queries fall back to scanning", err)
			} else {
				logIndex = st
			}
		} else {
			log.Info("request log: index disabled by config (log_index_disabled)")
		}

		reqLog, err = requestlog.OpenWithOptions(cfg.LogDir, requestlog.Options{
			RetentionDays: cfg.LogRetentionDays,
			JSONLArchive:  !cfg.LogJSONLDisabled,
		})
		if err != nil {
			log.Fatalf("open request log: %v", err)
		}
		if cfg.LogJSONLDisabled {
			log.Infof("request log: index-only at %s (retain %d days, no .jsonl archive)", cfg.LogDir, cfg.LogRetentionDays)
		} else {
			log.Infof("request log: writing to %s (retain %d days)", cfg.LogDir, cfg.LogRetentionDays)
		}
	} else {
		log.Info("request log: disabled (set log_dir in config to enable)")
	}

	pool := auth.NewPool(oauths, apikeys,
		time.Duration(cfg.ActiveWindowMinutes)*time.Minute,
		cfg.UseUTLS, cfg.DefaultProxyURL)
	pool.SetUsageLoadFunc(func(authID string) int64 {
		return store.Sum5h(authID).WeightedTotal()
	})

	refresherCtx, refresherCancel := context.WithCancel(context.Background())
	go pool.RunRefresher(refresherCtx, time.Minute, 10*time.Minute)
	go pool.RunDailyAnthropicAPIKeyReset(refresherCtx)

	tokensPath := filepath.Join(filepath.Dir(cfg.StateFile), "tokens.json")
	clientTokens, err := clienttoken.Open(tokensPath)
	if err != nil {
		log.Fatalf("open client token store: %v", err)
	}
	log.Infof("client tokens: %d loaded from %s", len(clientTokens.List()), tokensPath)

	s := server.New(cfg, pool, store, reqLog, clientTokens)

	// SaaS layer (multi-tenant). Wires SQLite-backed user/token resolution +
	// wallet billing on top of the proxy. Disabled by default — when off,
	// nothing changes from the OSS build.
	var saasDB *saasdb.DB
	var saasShutdown []func()
	if cfg.SaaS.Enabled {
		log.Infof("SaaS: opening DB at %s", cfg.SaaS.DBPath)
		if err := cfg.SaaS.EnsureJWTSecret(); err != nil {
			log.Fatalf("saas jwt secret: %v", err)
		}
		saasDB, err = saasdb.Open(cfg.SaaS.DBPath)
		if err != nil {
			log.Fatalf("open saas db: %v", err)
		}
		bootstrapAdmin(saasDB, cfg.SaaS)
		bootstrapPricingFromCatalog(cfg.SaaS, saasDB)

		// Daily VACUUM INTO snapshot. Atomic / online — request handlers
		// don't see a pause, and each snapshot is a clean self-contained
		// .db file (no WAL/SHM siblings) which makes off-host shipping a
		// single-file copy.
		//
		// Retention is saas.local_snapshot_days (default 14). It used to be a
		// hardcoded 30: at ~190 MB per snapshot and growing that was ~6 GB of
		// disk for a tier of backup the off-host archive already covers.
		go saasDB.RunDailyBackups(refresherCtx, cfg.SaaS.LocalSnapshotDays)

		mailer := mail.New(mail.SMTPConfig{
			Host: cfg.SaaS.SMTP.Host, Port: cfg.SaaS.SMTP.Port,
			Username: cfg.SaaS.SMTP.Username, Password: cfg.SaaS.SMTP.Password,
			From: cfg.SaaS.SMTP.From, UseTLS: cfg.SaaS.SMTP.UseTLS,
		}, cfg.SaaS.SiteName)

		// Hourly PRAGMA quick_check, plus escalation of any corruption error
		// that surfaces from live traffic. On 2026-08-18 saas.db developed
		// page corruption and nothing raised it: the server kept billing
		// against a broken b-tree for ~20 minutes, dropping every charge,
		// until a human noticed by failing to log in. Detection was the gap,
		// not the corruption itself.
		go saasDB.RunIntegrityChecks(refresherCtx, saasdb.DefaultIntegrityInterval,
			integrityAlerter(mailer, cfg.SaaS.SiteName, cfg.SaaS.AdminEmail))

		// Sweep spent / expired cross-origin SSO handoff codes every 10 minutes,
		// collecting anything that expired more than an hour ago. Each row is a
		// bearer credential with a 120-second life, and saas.db is snapshotted
		// daily and shipped off-host — so dead rows are cleared rather than left
		// to accumulate in every backup forever. Unconditional: the table exists
		// from migration v23 whether or not the feature is configured, and a
		// no-op DELETE costs nothing.
		go saasDB.RunSSOCodePrune(refresherCtx, 10*time.Minute)
		issuer := saasauth.NewIssuer(cfg.SaaS.JWTSecret, cfg.SaaS.JWTTTL)

		freeReg := true
		if cfg.SaaS.FreeRegister != nil {
			freeReg = *cfg.SaaS.FreeRegister
		}
		authH := saasauth.NewHandler(saasDB, issuer, mailer, cfg.SaaS.SiteName, freeReg)
		// referralsOn is the master switch for the whole invite / referral /
		// channel-attribution programme, the welcome credit included. Default
		// false (config.ApplyDefaults) — suspended after the 2026-08-08 farming
		// incident (168 throwaway signups, ~$116 drained via invite fission).
		//
		// It gates the welcome credit deliberately: signup_bonus_usd is an OLD
		// key that a deployed config.yaml may already pin to a positive value,
		// so keying the grant off that alone would keep paying until somebody
		// hand-edited prod config. referrals_enabled cannot exist in any config
		// written before this build, so it is false everywhere on rollout and
		// shipping the binary is by itself enough to stop the grant. Turning the
		// programme back on is then one deliberate edit.
		referralsOn := cfg.SaaS.ReferralsEnabled != nil && *cfg.SaaS.ReferralsEnabled
		if referralsOn && cfg.SaaS.SignupBonusUSD != nil {
			authH.TrialBonusUSD = *cfg.SaaS.SignupBonusUSD
		}
		tokensH := tokens.New(saasDB)
		rate := billing.NewRate(cfg.SaaS.ExchangeRateURL, cfg.SaaS.FallbackCNYPerUSD)
		go rate.RunRefresher(refresherCtx, time.Hour)

		gateway, gwName, err := selectPaymentGateway(cfg.SaaS)
		if err != nil {
			log.Fatalf("payment gateway: %v", err)
		}
		log.Infof("payment gateway: %s", gwName)
		billingH := billing.NewHandler(saasDB, rate, gateway, cfg.SaaS.SiteName)
		billingH.SiteURL = cfg.SaaS.SiteURL
		// Exact-origin allowlist for the optional cross-site top-up return_url
		// (a top-up started in the sibling HypiHub must land back on HypiHub).
		// Parsed ONCE here so a malformed origin is a boot-time fatal rather
		// than a silently-shortened allowlist. nil when the key is unset, and
		// then every return_url is rejected — today's behaviour exactly.
		topupOrigins, err := billing.TopupReturnOriginsFrom(cfg.SaaS.TopupReturnOrigins)
		if err != nil {
			log.Fatalf("saas: %v", err)
		}
		billingH.TopupReturnOrigins = topupOrigins
		if topupOrigins.Len() > 0 {
			log.Infof("saas: cross-site top-up return_url enabled for %v", topupOrigins.Origins())
		}
		// Stripe runs alongside the QR gateway (not instead of it) so the
		// embedded Payment Element — card / Alipay / WeChat / crypto, charged
		// 1:1 in USD — and the legacy QR rail coexist.
		if stripeGW, err := buildStripeGateway(cfg.SaaS); err != nil {
			log.Fatalf("stripe gateway: %v", err)
		} else if stripeGW != nil {
			billingH.Stripe = stripeGW
			log.Infof("stripe gateway: enabled (currency=%s, webhook=%v)", stripeGW.Currency(), stripeGW.HasWebhookSecret())
		}
		// Background sweeper: marks pending alipay orders older than the
		// TTL as expired so late notifications can't credit them.
		go billingH.RunExpirySweeper(refresherCtx)
		// Admin "Sync from Alipay" hook — calls alipay.trade.query through
		// the same applyNotification funnel as the async notify path.
		saasadmin.Reconciler = func(_ interface{ Done() <-chan struct{} }, out string) (string, error) {
			c, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			return billingH.ReconcileOrder(c, out)
		}
		adminH := saasadmin.New(saasDB)
		credH := saasadmin.NewCred(pool, cfg.AuthDir, cfg.UseUTLS)

		// Growth (marketing-channel attribution). Self-contained module: owns
		// its own tables, credits the signup bonus through the audited wallet
		// ledger (saasDB satisfies growth.Wallet), and registers the conversion
		// hook on the auth handler so ?ref= signups are attributed.
		growthSvc := growth.New(saasDB.DB, saasDB)
		// Signup anti-abuse: withhold the welcome bonus from a device/network
		// that already registered (fingerprint match or shared-subnet burst).
		fraudEnabled := true
		if cfg.SaaS.SignupFraud.Enabled != nil {
			fraudEnabled = *cfg.SaaS.SignupFraud.Enabled
		}
		requireFP := true
		if cfg.SaaS.SignupFraud.RequireFingerprint != nil {
			requireFP = *cfg.SaaS.SignupFraud.RequireFingerprint
		}
		growthSvc.ConfigureFraud(growth.FraudConfig{
			Enabled:              fraudEnabled,
			SubnetThreshold:      cfg.SaaS.SignupFraud.IPSubnetThreshold,
			Window:               time.Duration(cfg.SaaS.SignupFraud.WindowHours) * time.Hour,
			RequireFingerprint:   requireFP,
			EmailDomainThreshold: cfg.SaaS.SignupFraud.EmailDomainThreshold,
			DisposableDomains:    cfg.SaaS.SignupFraud.DisposableDomains,
		})
		// authH.Referral is set below to the referral service (which composes
		// growthSvc: personal invite codes first, then admin marketing channels)
		// — but only while the referral programme is enabled. growthSvc itself
		// is always constructed: the admin Growth tab reads its historical
		// channel/conversion data through the same instance.

		// Analytics (site-wide visitor behaviour). Captures EVERY landing-page
		// visitor's first action / dwell / flow / source, surfaced in the
		// admin Growth tab.
		//
		// It lives in its OWN database file, not saas.db. This is the highest
		// write rate and the lowest value data in the product — pageviews and
		// dwell pings — and on 2026-08-18 it shared a WAL with the wallet when
		// that file corrupted. Separating them means a damaged tracking table
		// can no longer stop billing, and the ledger's WAL is no longer
		// churning on traffic nobody would pay to recover. Falls back to the
		// shared handle if the dedicated file can't be opened: losing metrics
		// is not a reason to fail startup.
		analyticsPath := filepath.Join(filepath.Dir(cfg.SaaS.DBPath), analytics.DefaultFileName)
		analyticsSvc, err := analytics.Open(analyticsPath)
		if err != nil {
			log.Warnf("analytics: cannot open %s (%v) — falling back to the shared saas.db", analyticsPath, err)
			analyticsSvc = analytics.New(saasDB.DB)
		} else {
			log.Infof("analytics: tracking database at %s", analyticsPath)
			// One-time move of history out of saas.db. No-op once populated;
			// the source tables stay put until a later release drops them.
			importCtx, importCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := analyticsSvc.ImportFrom(importCtx, saasDB.DB); err != nil {
				log.Warnf("analytics: importing history from saas.db failed: %v", err)
			}
			importCancel()
			saasShutdown = append(saasShutdown, func() { _ = analyticsSvc.Close() })
		}

		// Arena (public leaderboard + real-time "Agent office"). Self-contained:
		// owns user_profiles (migration v10) via the DB layer, fans billing
		// pulses out over SSE. Injected into the adapter so Charge can publish.
		arenaSvc := arena.New(saasDB, issuer)
		profileH := saasprofile.New(saasDB)

		// Referral (viral growth: invite cards + peer gifting + campaigns).
		// Self-contained module owning migration-v11 tables; credits through the
		// audited wallet ledger and composes growthSvc for signup anti-abuse +
		// admin-channel fallback. It becomes the auth handler's referral granter
		// (personal invite codes first, growth channels second) and gift
		// auto-claimer; the adapter calls it to release deferred inviter rewards.
		//
		// The programme is SUSPENDED by default (referrals_enabled, default
		// false): it was farmed on 2026-08-08 — 168 throwaway signups drained
		// ~$116 through invite fission. When off we still construct the service,
		// but nothing on the granting path is wired to it: no invite/channel
		// bonus, no milestone tier, no conversion row, and the user-facing
		// routes are not mounted (see saasadapter.Mount). The admin/audit routes
		// and every table stay live so the operator can review what was granted.
		referralSvc := referral.New(saasDB, mailer, growthSvc, cfg.SaaS.SiteName, cfg.SaaS.SiteURL)
		// The sweeper runs regardless: it refunds EXPIRED gift escrow back to the
		// sender's wallet. It grants nothing new, and killing it would strand
		// real user money in gift_cards rows forever.
		referralSvc.StartSweeper(refresherCtx)
		if referralsOn {
			authH.Referral = referralSvc
		}
		// The admin campaign/channel editors stay reachable while suspended so
		// past grants remain auditable — but they would silently accept edits
		// that do nothing. Tell the panel, so it can say so.
		referralSvc.Suspended = !referralsOn
		growthSvc.SetSuspended(!referralsOn)
		// Gift auto-claim stays wired: gifts are user-funded transfers, not
		// signup credit, so already-sent gifts must still be able to land.
		authH.GiftClaimer = referralSvc
		workspaceH := saasworkspace.New(saasDB, mailer, cfg.SaaS.SiteURL, cfg.SaaS.SiteName)

		// Support desk. Owns tickets from signed-in users AND the appeal channel
		// for accounts the anti-abuse rules got wrong — which must work without a
		// session (a disabled account cannot authenticate) and without email (the
		// mail provider's quota is itself a casualty of the kind of attack that
		// triggers mass disabling).
		supportSvc := support.New(saasDB, cfg.SaaS.SiteName, cfg.SaaS.SiteURL)
		supportSvc.ConfigureInvoicing(cfg.SaaS.Invoice.TitleSuggestURL, support.PaymentInfo{
			AccountNo:   cfg.SaaS.Invoice.AccountNo,
			AccountName: cfg.SaaS.Invoice.AccountName,
			BankBranch:  cfg.SaaS.Invoice.BankBranch,
			BankCode:    cfg.SaaS.Invoice.BankCode,
		})

		// Catalog used for pricing computation (same instance as the proxy's
		// internal billing — the SaaS layer only adds the multiplier on top).
		catalog := pricing.NewCatalog(cfg.Pricing)
		adapter := saasadapter.NewAdapter(saasDB, catalog, rate)
		if cfg.SaaS.MaxOverdraftUSD != nil {
			adapter.MaxOverdraftUSD = *cfg.SaaS.MaxOverdraftUSD
		}
		// A wallet refusing debits is the same condition the integrity checker
		// pages on — the database is not taking writes — so it goes to the same
		// inbox through the same template rather than a second alert channel.
		if alert := integrityAlerter(mailer, cfg.SaaS.SiteName, cfg.SaaS.AdminEmail); alert != nil {
			adapter.AlertDrop = func(detail string) {
				alert(saasdb.IntegrityAlert{Source: "billing", Detail: detail, At: time.Now()})
			}
		}
		adapter.Arena = arenaSvc
		// Deferred inviter payout (reward_on=first_spend). Left nil while the
		// programme is suspended, otherwise conversions recorded before the
		// shutdown would keep paying out on the invitee's first spend.
		if referralsOn {
			adapter.Referral = referralSvc
		}
		s.SetSaaS(adapter)

		// Mount /api/v2/* on every gin engine the server hosts. Both Claude
		// and Codex listeners get the same SaaS routes — operators can hit
		// either port for management.
		var spaFS fs.FS
		if f, ferr := admin.SaaSDistFS(); ferr == nil {
			spaFS = f
		}
		legacyAdmin := s.LegacyAdmin()
		// SSO bridge: a logged-in SaaS user can hit /admin/api/*
		// with their JWT instead of the legacy operator token. GETs are
		// allowed for any authenticated user (so the Overview / charts /
		// Pricing tabs work for everyone signed in); mutations require the
		// admin role *as recorded in saas.db* — the role claim inside the
		// token is never consulted. See saas/auth.LegacyAdminSSO.
		admin.SSOAuth = saasauth.LegacyAdminSSO(issuer, saasDB)
		// Machine-to-machine routes for the sibling HypiHub gateway, which shares
		// these user accounts and wallets over HTTP instead of opening saas.db.
		// nil when saas.service_tokens is unset — then /api/v2/svc/* is never
		// mounted at all. A malformed entry is fatal: silently running with the
		// integration off would look identical to a firewall problem on the
		// sibling's side.
		svcTokens, err := saasadapter.NewServiceTokenSet(cfg.SaaS.ServiceTokens)
		if err != nil {
			log.Fatalf("saas: %v", err)
		}
		// The issuer is the same one /auth/login uses: a session adopted across
		// origins must be indistinguishable from one obtained by logging in here.
		svcH := saasadapter.NewServiceHandler(adapter, svcTokens).WithIssuer(issuer)

		// Cross-origin SSO handoff (docs/SPEC.md §12 in the hypihub repo).
		// Parsed ONCE here so a malformed origin is a boot-time fatal rather
		// than a line silently dropped from the allowlist — a shortened
		// allowlist looks exactly like a working one right up until it either
		// turns the feature off or, in a sloppier implementation, fails open.
		// nil when saas.sso_return_origins is empty, and then the minting route
		// does not exist at all.
		ssoOrigins, err := saas.NewOriginAllowlist(cfg.SaaS.SSOReturnOrigins)
		if err != nil {
			log.Fatalf("saas: %v", err)
		}
		ssoH := saasadapter.NewSSOHandler(saasDB, ssoOrigins)

		for _, h := range s.GinEngines() {
			saasadapter.Mount(h, saasDB, authH, tokensH, billingH, adminH, credH, issuer, legacyAdmin, cfg.LogDir, catalog, growthSvc, analyticsSvc, arenaSvc, profileH, referralSvc, workspaceH, supportSvc, svcH, ssoH, referralsOn)
			if spaFS != nil {
				saasadapter.MountSPA(h, spaFS)
			}
		}

		// Model health background checker.
		hc := health.New(saasDB, pool, cfg.SaaS.HealthCheckInterval, cfg.LogDir)
		go hc.Run(refresherCtx)
		saasadmin.HealthRefresher = hc.Refresh

		saasShutdown = append(saasShutdown, func() { _ = saasDB.Close() })
		log.Info("SaaS: enabled")
	}

	// Shop (发卡网) standalone storefront. Independent of SaaS — wires its
	// own SQLite + Z-Pay gateway + SMTP mailer onto its own gin engine,
	// attached to the server's endpoint list so Start()/Shutdown() see it.
	if cfg.Shop.Enabled && cfg.Endpoints.Shop.IsEnabled() {
		// Shop collects via Stripe hosted Checkout (card + Alipay). Secret +
		// webhook values support the @file convention to keep them out of the
		// committed config.
		shopGw, err := buildShopStripeGateway(cfg.Shop)
		if err != nil {
			log.Fatalf("shop stripe: %v", err)
		}
		shopDB, err := shop.Open(cfg.Shop.DBPath)
		if err != nil {
			log.Fatalf("shop db: %v", err)
		}
		shopMailer := shop.NewMailer(cfg.Shop.SMTP, cfg.Shop.SiteName)
		shopInst, err := shop.New(cfg.Shop, shopDB, shopGw, shopMailer, cfg.AdminToken)
		if err != nil {
			log.Fatalf("shop init: %v", err)
		}
		shopEng := gin.New()
		shopEng.Use(gin.Recovery())
		shopEng.Use(func(c *gin.Context) {
			start := time.Now()
			c.Next()
			log.Infof("[shop] %s %s %d %s", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(start))
		})
		shopInst.RegisterRoutes(shopEng)
		addr := fmt.Sprintf("%s:%d", cfg.Endpoints.Shop.Host, cfg.Endpoints.Shop.Port)
		s.AttachExtraEndpoint("shop", addr, shopEng)
		// Delivery emails outlive the request that spawned them and end in a
		// write (email_sent). Join them before closing the DB, or a buyer who
		// was mailed during shutdown stays flagged undelivered.
		saasShutdown = append(saasShutdown, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := shopInst.WaitDeliveries(ctx); err != nil {
				log.Warnf("shop: delivery emails still in flight at shutdown: %v", err)
			}
			_ = shopDB.Close()
		})
		log.Infof("shop: enabled at %s (site=%q stripe currency=%s webhook=%t)", addr, cfg.Shop.SiteName, shopGw.Currency(), shopGw.HasWebhookSecret())
	}

	for _, ep := range s.Endpoints() {
		primary := ""
		if ep.Primary {
			primary = " (primary — admin panel mounted here)"
		}
		log.Infof("endpoint %s [%s] → %s%s", ep.Name, ep.Provider, ep.Addr, primary)
	}

	// Graceful shutdown.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		log.Info("shutting down...")
		refresherCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		store.Close()
		// Writer first, then the index — requestlog.Shutdown enforces the only
		// order that cannot lose the final batch. This used to be the other way
		// round, which is unrecoverable under log_jsonl_disabled: the store
		// deregisters on Close, so the writer's last flush resolves no
		// destination and there is no file behind it.
		requestlog.Shutdown(reqLog, logIndex)
		for _, fn := range saasShutdown {
			fn()
		}
	}()

	if err := s.Start(); err != nil {
		log.Infof("server stopped: %v", err)
	}
	<-shutdownDone
}

// bootstrapAdmin creates the configured admin user on first run if the users
// table is empty. Subsequent starts ignore admin_email/password.
func bootstrapAdmin(db *saasdb.DB, cfg saas.Config) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := db.CountUsers(ctx)
	if err != nil {
		log.Warnf("saas: count users: %v", err)
		return
	}
	if n > 0 {
		return
	}
	email := strings.TrimSpace(cfg.AdminEmail)
	pw := strings.TrimSpace(cfg.AdminPassword)
	if email == "" || pw == "" {
		log.Warn("saas: no admin user — set saas.admin_email + saas.admin_password to auto-bootstrap one on first start, or register via /api/v2/auth/register")
		return
	}
	hash, err := saasauth.HashPassword(pw)
	if err != nil {
		log.Warnf("saas: bootstrap admin hash: %v", err)
		return
	}
	def, err := db.DefaultGroup(ctx)
	if err != nil {
		log.Warnf("saas: default group: %v", err)
		return
	}
	u, err := db.CreateUser(ctx, email, hash, saasdb.RoleAdmin, def.ID, true)
	if err != nil {
		log.Warnf("saas: bootstrap admin: %v", err)
		return
	}
	log.Infof("saas: bootstrapped admin user id=%d email=%s", u.ID, u.Email)
}

// bootstrapPricingFromCatalog seeds nothing — the migration creates the
// default group. This hook exists so future expansions (per-provider rate
// imports, etc.) have a place to land. Intentionally a no-op for now.
func bootstrapPricingFromCatalog(_ saas.Config, _ *saasdb.DB) {}

// selectPaymentGateway picks a billing.Gateway from the SaaS config. The
// `payment_provider` field is authoritative; if empty we infer from
// which credential block is populated. Returns the gateway plus a short
// name for logging.
func selectPaymentGateway(s saas.Config) (billing.Gateway, string, error) {
	provider := strings.ToLower(strings.TrimSpace(s.PaymentProvider))
	if provider == "" {
		switch {
		case strings.TrimSpace(s.ZPay.PID) != "":
			provider = "zpay"
		case strings.TrimSpace(s.Alipay.AppID) != "":
			provider = "alipay"
		default:
			provider = "mock"
		}
	}
	// Hard guard: the mock gateway auto-confirms every order after 2s without
	// taking real money. Refuse it on a public/production deployment unless the
	// operator has explicitly opted in. This catches the common footgun of
	// shipping a server with no payment block configured.
	if provider == "mock" && !mockPaymentAllowed(s) {
		return nil, "", fmt.Errorf("refusing mock payment gateway on a public site (SiteURL=%q): configure a real gateway (payment_provider: zpay|alipay + its credentials), or set saas.allow_mock_payment: true to override for testing", strings.TrimSpace(s.SiteURL))
	}
	switch provider {
	case "zpay":
		key, err := loadKeyFile(s.ZPay.Key)
		if err != nil {
			return nil, "", fmt.Errorf("zpay key: %w", err)
		}
		gw, err := billing.NewZPayGateway(billing.ZPayParams{
			BaseURL:   s.ZPay.BaseURL,
			PID:       s.ZPay.PID,
			Key:       key,
			NotifyURL: s.ZPay.NotifyURL,
			ReturnURL: s.ZPay.ReturnURL,
		})
		if err != nil {
			return nil, "", err
		}
		return gw, "zpay (易支付 aggregator, pid=" + s.ZPay.PID + ")", nil
	case "alipay":
		gw, err := billing.NewAlipayGateway(billing.AlipayParams{
			AppID:           s.Alipay.AppID,
			PrivateKey:      s.Alipay.PrivateKey,
			AlipayPublicKey: s.Alipay.AlipayPublicKey,
			IsProduction:    s.Alipay.IsProduction,
			NotifyURL:       s.Alipay.NotifyURL,
		})
		if err != nil {
			return nil, "", err
		}
		return gw, "alipay (direct merchant)", nil
	case "mock":
		return &billing.MockGateway{}, "mock (auto-confirms after 2s — DO NOT use in production)", nil
	}
	return nil, "", fmt.Errorf("unknown payment_provider %q (want zpay|alipay|mock)", provider)
}

// mockPaymentAllowed reports whether the mock (auto-confirm) gateway may run.
// It is permitted only when explicitly opted in, or when SiteURL points at a
// local host (dev). An empty SiteURL counts as non-local: a real deployment
// sets SiteURL, so "unset" should fail closed rather than silently mock.
func mockPaymentAllowed(s saas.Config) bool {
	return s.AllowMockPayment || isLocalSiteURL(s.SiteURL)
}

// isLocalSiteURL reports whether the configured site origin is a loopback /
// development host (localhost, 127.0.0.0/8, ::1, 0.0.0.0, or a *.local /
// *.localhost name). Empty or unparsable URLs are treated as non-local.
func isLocalSiteURL(siteURL string) bool {
	siteURL = strings.TrimSpace(siteURL)
	if siteURL == "" {
		return false
	}
	if !strings.Contains(siteURL, "://") {
		siteURL = "http://" + siteURL
	}
	u, err := url.Parse(siteURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch {
	case host == "localhost", host == "0.0.0.0", host == "::1":
		return true
	case strings.HasSuffix(host, ".local"), strings.HasSuffix(host, ".localhost"):
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// buildStripeGateway constructs the optional Stripe gateway. Returns
// (nil, nil) when Stripe isn't configured (no enabled flag and no secret key),
// so the caller can treat "absent" and "error" distinctly. Resolves the @path
// indirection on the secret key + webhook secret.
func buildStripeGateway(s saas.Config) (*billing.StripeGateway, error) {
	sc := s.Stripe
	if !sc.Enabled && strings.TrimSpace(sc.SecretKey) == "" {
		return nil, nil // not configured
	}
	secret, err := loadKeyFile(sc.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("secret_key: %w", err)
	}
	whsec, err := loadKeyFile(sc.WebhookSecret)
	if err != nil {
		return nil, fmt.Errorf("webhook_secret: %w", err)
	}
	returnURL := strings.TrimSpace(sc.ReturnURL)
	if returnURL == "" && strings.TrimSpace(s.SiteURL) != "" {
		returnURL = strings.TrimRight(s.SiteURL, "/") + "/app/billing"
	}
	return billing.NewStripeGateway(billing.StripeParams{
		SecretKey:                  secret,
		PublishableKey:             sc.PublishableKey,
		WebhookSecret:              whsec,
		Currency:                   sc.Currency,
		PaymentMethodConfiguration: sc.PaymentMethodConfiguration,
		PaymentMethodTypes:         sc.PaymentMethodTypes,
		ReturnURL:                  returnURL,
	})
}

// buildShopStripeGateway constructs the shop's Stripe hosted-Checkout gateway.
// Resolves the @path indirection on the secret + webhook secret. The shop may
// reuse the SaaS account's secret_key with its own webhook endpoint secret.
func buildShopStripeGateway(c shop.Config) (*billing.StripeGateway, error) {
	sc := c.Stripe
	secret, err := loadKeyFile(sc.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("secret_key: %w", err)
	}
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("secret_key is required (shop now collects via Stripe)")
	}
	whsec, err := loadKeyFile(sc.WebhookSecret)
	if err != nil {
		return nil, fmt.Errorf("webhook_secret: %w", err)
	}
	return billing.NewStripeGateway(billing.StripeParams{
		SecretKey:     secret,
		WebhookSecret: whsec,
		Currency:      sc.Currency,
	})
}

// loadKeyFile resolves either an inline secret or an "@/path" reference.
// Mirrors billing.loadKey but exposed at the main package layer so we
// can keep the file-loaded value out of the YAML committed to git.
func loadKeyFile(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "@") {
		data, err := os.ReadFile(s[1:])
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	return s, nil
}

// integrityAlerter returns the handler that notifies the operator when the
// SaaS database reports corruption. A nil-safe no-op when no admin address is
// configured — the error-level log line still lands either way.
//
// Mail is the right channel here despite being slow: the failure is rare,
// needs a human, and the alternative (only a log line) is exactly what let the
// 2026-08-18 incident run unnoticed among thousands of ordinary request lines.
func integrityAlerter(mailer mail.Mailer, siteName, adminEmail string) func(saasdb.IntegrityAlert) {
	to := strings.TrimSpace(adminEmail)
	if to == "" || mailer == nil {
		return nil
	}
	return func(a saasdb.IntegrityAlert) {
		var subject, html, text string
		if a.Resolved {
			subject, html, text = mail.DatabaseRecoveredEmail(siteName, a.Detail, a.At.Format(time.RFC3339))
		} else {
			subject, html, text = mail.DatabaseAlertEmail(siteName, a.Source, a.Detail, a.At.Format(time.RFC3339))
		}
		if err := mailer.Send(to, subject, html, text); err != nil {
			log.Warnf("saas-db: could not mail integrity alert to %s: %v", to, err)
		}
	}
}
