package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/wjsoj/CPA-Claude/internal/admin"
	"github.com/wjsoj/CPA-Claude/internal/config"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/clienttoken"
	"github.com/wjsoj/cc-core/codexsidecar"
	"github.com/wjsoj/cc-core/codexws"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/ratelimit"
	"github.com/wjsoj/cc-core/requestlog"
	ccsidecar "github.com/wjsoj/cc-core/sidecar"
	"github.com/wjsoj/cc-core/thinkingsig"
	"github.com/wjsoj/cc-core/usage"
)

// endpoint is one listening http.Server paired with its provider label. The
// admin panel lives on exactly one endpoint (the primary — see pickPrimary).
type endpoint struct {
	name     string // "claude" | "codex"
	provider string // canonical auth.Provider*
	http     *http.Server
	primary  bool // hosts the admin panel + status SPA
}

type Server struct {
	cfg     *config.Config
	pool    *auth.Pool
	usage   *usage.Store
	pricing *pricing.Catalog
	tokens  *clienttoken.Store
	// trustedRelays are client tokens belonging to a proxy we also run, whose
	// relay headers may be believed for routing. Resolved once from config.
	trustedRelays trustedRelaySet
	reqLog        *requestlog.Writer
	endpoints     []*endpoint
	// inflight is keyed by (provider | clientToken): Claude and Codex are
	// treated as independent budgets for the same user so a client running
	// Claude at cap doesn't block its Codex calls (and vice-versa). Matches
	// the per-provider stickiness already used by Pool.Acquire.
	inflight ratelimit.Concurrency
	// rpm enforces a sliding-window requests-per-minute cap. Keyed by
	// (provider | clientToken) — same scoping as inflight so Claude and
	// Codex traffic don't share one budget.
	rpm ratelimit.RPM
	// sidecar emulates the auxiliary traffic real Claude Code fires
	// alongside /v1/messages (Phase A: quota probe at session start).
	// Reduces the strongest stealth-detection signal — a healthy OAuth
	// account whose request stream contains zero quota probes is
	// trivially flagged as a third-party tool.
	sidecar *ccsidecar.Manager

	// codexSidecar emulates the auxiliary traffic a real Codex client emits.
	// Always constructed; it is inert unless cfg.CodexSidecar.Enabled.
	codexSidecar *codexsidecar.Manager

	// codexWSEgress forwards HTTP-ingress Codex requests over an upstream
	// WebSocket — the transport a current codex-tui actually uses — reusing one
	// socket per conversation. Always constructed; inert unless
	// cfg.CodexWS.Upstream selects a WebSocket mode. See codex_ws_egress.go.
	codexWSEgress *codexWSEgress

	// saas, when non-nil, layers multi-tenant user-token resolution + balance
	// billing on top of the legacy clienttoken.Store. nil-safe: when unset,
	// the proxy behaves exactly like the OSS build.
	saas SaaSAdapter

	// adminH is the legacy operator JSON API handler (now mounted at
	// /admin/api/*). Stored so the SaaS layer can borrow its requests-log
	// + Anthropic-usage handlers under /api/v2/admin/* with SaaS-side auth.
	adminH *admin.Handler

	// switchTracker detects when a conversation rotates mid-stream from
	// one upstream credential to another. On switch we sanitize the
	// prior account's signed `thinking` blocks before forwarding (they
	// would 400 with "signature in thinking" on the new account).
	switchTracker *thinkingsig.SwitchTracker

	// codexRefreshGate serialises the forced token refresh a ChatGPT 401
	// triggers (codex_auth_reject.go); codexRefresh lets tests stub the exchange.
	codexRefreshGate refreshGate
	codexRefresh     func(ctx context.Context, a *auth.Auth) error

	// codexManifests caches the upstream Codex model manifest per requesting
	// client version. Pickers refresh at client start, so a cache miss would
	// otherwise fan several concurrent clients out into as many upstream
	// fetches. A failed refresh serves the stale body rather than an empty
	// picker — see auth.CodexManifestCache.
	codexManifests *auth.CodexManifestCache

	// codexRespAccount binds a Codex response id to the credential that produced
	// it, namespaced by credential group. Backs the cross-group previous_response_id
	// safety boundary on the WS path. Always initialized (cheap; janitor goroutine).
	codexRespAccount *codexRespAccountStore

	// codexSessions maps a logical downstream conversation to the stable
	// upstream session id we present for it. That id is the handshake's
	// session-id AND the frame's prompt_cache_key, so minting a fresh one per
	// connection would drop every reconnect out of the upstream prompt cache.
	// Never keyed on anything a downstream client controls alone — see the
	// anchor built in handleCodexResponsesWS.
	codexSessions *codexws.SessionRegistry
}

// LegacyAdmin returns the legacy admin handler so main.go can wire its
// requests-log + anthropic-usage endpoints under the SaaS admin group.
func (s *Server) LegacyAdmin() *admin.Handler { return s.adminH }

// SetSaaS attaches the SaaS multi-tenant adapter. Must be called before Start.
func (s *Server) SetSaaS(a SaaSAdapter) { s.saas = a }

// New constructs the multi-endpoint server. At least one endpoint must be
// enabled — otherwise the returned Server has no listeners and Start will
// refuse to run. Admin panel + public /status page are mounted on the
// "primary" endpoint: Claude when enabled, otherwise Codex.
func New(cfg *config.Config, pool *auth.Pool, store *usage.Store, reqLog *requestlog.Writer, tokens *clienttoken.Store) *Server {
	gin.SetMode(gin.ReleaseMode)
	cat := pricing.NewCatalog(cfg.Pricing)
	s := &Server{cfg: cfg, pool: pool, usage: store, pricing: cat, tokens: tokens, reqLog: reqLog}
	if s.trustedRelays = newTrustedRelaySet(cfg.TrustedRelayTokens); len(s.trustedRelays) > 0 {
		log.Infof("relay: trusting %d client token(s) to declare their downstream callers", len(s.trustedRelays))
	}
	s.codexRespAccount = newCodexRespAccountStore(codexRespAccountTTL)
	// 30 min: model catalogs move on the order of weeks, so this is already
	// far tighter than the thing it tracks, and a shorter TTL only adds
	// upstream fetches without adding freshness.
	s.codexManifests = &auth.CodexManifestCache{TTL: 30 * time.Minute}
	s.codexSessions = codexws.NewSessionRegistry(0)
	s.sidecar = ccsidecar.New(ccsidecar.Config{
		Enabled: true,
		UseUTLS: cfg.UseUTLS,
		BaseURL: cfg.AnthropicBaseURL,
	})
	// The Codex counterpart is a separate manager, not a mode of the Anthropic
	// one: the two clients' auxiliary traffic has no endpoint, no body and no
	// identity in common, and generalising over them would produce a shape
	// belonging to neither.
	s.codexSidecar = codexsidecar.New(codexsidecar.Config{
		Enabled: cfg.CodexSidecar.Enabled,
		UseUTLS: cfg.UseUTLS,
	})
	s.codexWSEgress = newCodexWSEgress(cfg)
	s.switchTracker = thinkingsig.NewSwitchTracker()

	primary := pickPrimary(cfg)
	adminH := admin.New(cfg, pool, store, cat, tokens)
	s.adminH = adminH

	if cfg.Endpoints.Claude.IsEnabled() {
		eng := s.buildClaudeEngine(adminH, primary == "claude")
		s.endpoints = append(s.endpoints, &endpoint{
			name:     "claude",
			provider: auth.ProviderAnthropic,
			primary:  primary == "claude",
			http: &http.Server{
				Addr:              fmt.Sprintf("%s:%d", cfg.Endpoints.Claude.Host, cfg.Endpoints.Claude.Port),
				Handler:           eng,
				ReadHeaderTimeout: 10 * time.Second,
			},
		})
	}
	if cfg.Endpoints.Codex.IsEnabled() {
		eng := s.buildCodexEngine(adminH, primary == "codex")
		s.endpoints = append(s.endpoints, &endpoint{
			name:     "codex",
			provider: auth.ProviderOpenAI,
			primary:  primary == "codex",
			http: &http.Server{
				Addr:              fmt.Sprintf("%s:%d", cfg.Endpoints.Codex.Host, cfg.Endpoints.Codex.Port),
				Handler:           eng,
				ReadHeaderTimeout: 10 * time.Second,
			},
		})
	}
	return s
}

// AttachExtraEndpoint registers an additional HTTP listener that's
// independent of the proxy state. Used by the SaaS-free `shop` package
// (and any future side-services) to mount on a dedicated port without
// touching the credential pool / SaaS adapter wiring. Must be called
// before Start().
func (s *Server) AttachExtraEndpoint(name, addr string, handler http.Handler) {
	s.endpoints = append(s.endpoints, &endpoint{
		name: name,
		http: &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second},
	})
}

// pickPrimary returns the name of the endpoint that will host the admin
// panel + public status page. Claude wins when both are up — matches the
// user's existing workflow and URLs.
func pickPrimary(cfg *config.Config) string {
	switch {
	case cfg.Endpoints.Claude.IsEnabled():
		return "claude"
	case cfg.Endpoints.Codex.IsEnabled():
		return "codex"
	default:
		return ""
	}
}

// Endpoints returns a read-only snapshot of the configured live endpoints,
// for diagnostics / admin display. Safe after Start().
func (s *Server) Endpoints() []EndpointInfo {
	out := make([]EndpointInfo, 0, len(s.endpoints))
	for _, ep := range s.endpoints {
		out = append(out, EndpointInfo{
			Name: ep.name, Provider: ep.provider, Addr: ep.http.Addr, Primary: ep.primary,
		})
	}
	return out
}

// GinEngines returns every per-endpoint gin engine. Used by the SaaS layer
// to mount /api/v2/* routes on the same listeners as the proxy.
func (s *Server) GinEngines() []*gin.Engine {
	out := make([]*gin.Engine, 0, len(s.endpoints))
	for _, ep := range s.endpoints {
		if eng, ok := ep.http.Handler.(*gin.Engine); ok {
			out = append(out, eng)
		}
	}
	return out
}

// EndpointInfo is the diagnostic shape exported by Server.Endpoints.
type EndpointInfo struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Addr     string `json:"addr"`
	Primary  bool   `json:"primary"`
}

// Start launches every configured endpoint in its own goroutine and blocks
// until all of them have stopped. Returns the first non-http.ErrServerClosed
// error (or nil on clean shutdown).
func (s *Server) Start() error {
	if len(s.endpoints) == 0 {
		return errors.New("no endpoints enabled — set endpoints.claude.port or endpoints.codex.port in config.yaml")
	}
	errCh := make(chan error, len(s.endpoints))
	for _, ep := range s.endpoints {
		ep := ep
		go func() {
			tag := ep.name
			if ep.primary {
				tag += "*"
			}
			log.Infof("cpa-claude[%s] listening on %s", tag, ep.http.Addr)
			err := ep.http.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errCh <- err
		}()
	}
	var firstErr error
	for i := 0; i < len(s.endpoints); i++ {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Shutdown gracefully stops every endpoint in parallel.
func (s *Server) Shutdown(ctx context.Context) error {
	s.sidecar.Stop()
	s.codexSidecar.Stop()
	s.codexWSEgress.Close()
	var wg sync.WaitGroup
	errs := make([]error, len(s.endpoints))
	for i, ep := range s.endpoints {
		wg.Add(1)
		go func(i int, ep *endpoint) {
			defer wg.Done()
			errs[i] = ep.http.Shutdown(ctx)
		}(i, ep)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// buildClaudeEngine returns the gin.Engine serving Anthropic-native routes
// (/v1/messages, /v1/messages/count_tokens). When primary is true it also
// mounts the admin SPA + public status page.
func (s *Server) buildClaudeEngine(adminH *admin.Handler, primary bool) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery(), loggingMiddleware("claude"), corsMiddleware())
	engine.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok", "endpoint": "claude"}) })

	v1 := engine.Group("/v1")
	// requireClaudeCodeClient runs BEFORE clientAuth so we don't even
	// hash-compare an attacker's bearer token if their headers don't look
	// like CC. Cheaper to reject and log fewer DB lookups.
	v1.Use(requireClaudeCodeClient(), s.clientAuth())
	{
		v1.POST("/messages", s.handleMessages)
		v1.POST("/messages/count_tokens", s.handleCountTokens)
	}

	// Legacy JSON status (behind client auth). The bare /status path is the
	// public status SPA, served only on the primary endpoint below.
	engine.GET("/status.json", s.clientAuth(), s.handleStatus)

	if primary {
		// Legacy /status SPA is mounted only when SaaS is off — when SaaS
		// runs, /status is the new React-based statuspage served via the
		// SPA fallback in saas/adapter.MountSPA.
		// Legacy /status SPA is only mounted when SaaS is off — when SaaS
		// is enabled, /status is the new React-based statuspage served
		// via the SPA fallback in saas/adapter.MountSPA.
		if !s.cfg.SaaS.Enabled {
			adminH.RegisterStatus(engine)
		}
		adminH.Register(engine)
	}
	return engine
}

// buildCodexEngine is the sibling engine for the OpenAI/Codex endpoint.
// The route handlers are stubs in this phase — they land in Phase 3. For
// now the engine exists so the port binds and /healthz responds, which is
// enough to verify the multi-endpoint machinery end-to-end.
func (s *Server) buildCodexEngine(adminH *admin.Handler, primary bool) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery(), loggingMiddleware("codex"), corsMiddleware())
	engine.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok", "endpoint": "codex"}) })

	v1 := engine.Group("/v1")
	v1.Use(s.clientAuth())
	{
		// Stubs — implemented in Phase 3 (see codex_proxy.go).
		v1.POST("/chat/completions", s.handleCodexChatCompletions)
		v1.POST("/responses", s.handleCodexResponses)
		v1.POST("/responses/compact", s.handleCodexResponsesCompact)
		v1.GET("/models", s.handleCodexModels)
		// WebSocket ingress for /v1/responses (real codex-tui transport). Opt-in;
		// a GET with Upgrade: websocket. The POST path above is unaffected.
		if s.cfg.CodexWS.Enabled {
			v1.GET("/responses", s.handleCodexResponsesWS)
		}
	}

	// chatgpt_base_url mode. A Codex client configured with
	// `chatgpt_base_url = "https://gateway"` refreshes its picker from this
	// path rather than from /v1/models, and until it existed the SPA
	// catch-all answered it with a 200 carrying the admin panel's HTML —
	// which a Codex client can neither parse nor complain about. It must stay
	// registered ahead of any NoRoute handler.
	backend := engine.Group("/backend-api/codex")
	backend.Use(s.clientAuth())
	{
		backend.GET("/models", s.handleCodexModels)
	}

	if primary {
		// Legacy /status SPA is only mounted when SaaS is off — when SaaS
		// is enabled, /status is the new React-based statuspage served
		// via the SPA fallback in saas/adapter.MountSPA.
		if !s.cfg.SaaS.Enabled {
			adminH.RegisterStatus(engine)
		}
		adminH.Register(engine)
	}
	return engine
}

func loggingMiddleware(tag string) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Infof("[%s] %s %s %d %s", tag, c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(start))
	}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// clientAuth matches the incoming Authorization: Bearer or x-api-key against
// the SaaS user-token DB first (when enabled), then the legacy clienttoken
// store. It FAILS CLOSED: a request that matches neither is always rejected.
// There is deliberately no "open mode" — the legacy "empty token store ⇒ open
// proxy" convenience was removed because, once SaaS moved client tokens into
// its own DB, the empty legacy store turned the proxy into an unbilled open
// relay (any bearer token served for free against the operator's upstream
// credentials).
func (s *Server) clientAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		tok := extractClientToken(c.Request)
		// SaaS path: per-user token in DB.
		if s.saas != nil && tok != "" {
			if info, ok := s.saas.Lookup(tok); ok {
				if info.Disabled {
					writeAPIError(c, requestProvider(c), APIError{Status: http.StatusForbidden, Code: "account_disabled", Message: "This API key or account is disabled. Contact support if you believe this is a mistake."})
					return
				}
				c.Set("client_token", tok)
				c.Set("client_name", info.Name)
				c.Set("saas_info", info)
				c.Set(relayTrustedKey, s.trustedRelays.Has(tok))
				c.Next()
				return
			}
		}
		// Legacy clienttoken store (cc-core). When populated, the token must be
		// a configured operator token.
		if !s.tokens.Empty() {
			if tok == "" {
				writeAPIError(c, requestProvider(c), APIError{Status: http.StatusUnauthorized, Code: "missing_api_key", Message: "No API key was provided. Add a Bearer token or API key header and try again."})
				return
			}
			entry, ok := s.tokens.Lookup(tok)
			if !ok {
				writeAPIError(c, requestProvider(c), APIError{Status: http.StatusUnauthorized, Code: "invalid_api_key", Message: "The provided API key is invalid."})
				return
			}
			c.Set("client_token", tok)
			c.Set("client_name", entry.Name)
			// Either opt-in designates a relay: the operator's config list, or
			// the token's own trusted_relay flag in the legacy store.
			c.Set(relayTrustedKey, entry.TrustedRelay || s.trustedRelays.Has(tok))
			c.Next()
			return
		}
		// Fail closed: matched neither a SaaS token nor a configured legacy token.
		if tok == "" {
			writeAPIError(c, requestProvider(c), APIError{Status: http.StatusUnauthorized, Code: "missing_api_key", Message: "No API key was provided. Add a Bearer token or API key header and try again."})
			return
		}
		writeAPIError(c, requestProvider(c), APIError{Status: http.StatusUnauthorized, Code: "invalid_api_key", Message: "The provided API key is invalid."})
	}
}

// saasInfoFrom returns the SaaS token info attached by clientAuth, or
// (zero, false) if this request is on a legacy (non-SaaS) token.
func saasInfoFrom(c *gin.Context) (SaaSTokenInfo, bool) {
	v, ok := c.Get("saas_info")
	if !ok {
		return SaaSTokenInfo{}, false
	}
	info, ok := v.(SaaSTokenInfo)
	return info, ok
}

func extractClientToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" {
		parts := strings.SplitN(v, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return strings.TrimSpace(parts[1])
		}
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	return ""
}

func (s *Server) handleStatus(c *gin.Context) {
	type row struct {
		ID            string    `json:"id"`
		Kind          string    `json:"kind"`
		Provider      string    `json:"provider,omitempty"`
		Label         string    `json:"label"`
		Email         string    `json:"email,omitempty"`
		ProxyURL      string    `json:"proxy_url,omitempty"`
		MaxConcurrent int       `json:"max_concurrent"`
		ActiveClients int       `json:"active_clients"`
		ClientTokens  []string  `json:"client_tokens,omitempty"`
		Disabled      bool      `json:"disabled,omitempty"`
		QuotaExceeded bool      `json:"quota_exceeded,omitempty"`
		QuotaResetAt  time.Time `json:"quota_reset_at,omitempty"`
		ExpiresAt     time.Time `json:"expires_at,omitempty"`
	}
	poolStatus := s.pool.Status()
	rows := make([]row, 0, len(poolStatus))
	for _, st := range poolStatus {
		kind := "oauth"
		if st.Auth.Kind == auth.KindAPIKey {
			kind = "apikey"
		}
		masked := make([]string, 0, len(st.ClientTokens))
		for _, t := range st.ClientTokens {
			masked = append(masked, auth.MaskToken(t))
		}
		rows = append(rows, row{
			ID:            st.Auth.ID,
			Kind:          kind,
			Provider:      auth.NormalizeProvider(st.Auth.Provider),
			Label:         st.Auth.Label,
			Email:         st.Auth.Email,
			ProxyURL:      st.Auth.ProxyURL,
			MaxConcurrent: st.Auth.MaxConcurrent,
			ActiveClients: st.ActiveClients,
			ClientTokens:  masked,
			Disabled:      st.Auth.Disabled,
			QuotaExceeded: !st.Auth.QuotaExceededAt.IsZero(),
			QuotaResetAt:  st.Auth.QuotaResetAt,
			ExpiresAt:     st.Auth.ExpiresAt,
		})
	}
	c.JSON(200, gin.H{
		"active_window_minutes": int(s.pool.ActiveWindow() / time.Minute),
		"auths":                 rows,
		"usage":                 s.usage.Snapshot(),
		"endpoints":             s.Endpoints(),
	})
}

// codexLimitMultiplier scales the per-client RPM and concurrency caps for the
// Codex (OpenAI) provider relative to Claude. Codex traffic is cheaper and
// chattier (tool loops, /v1/responses polling), so it gets a higher ceiling.
// Applied on top of whatever effective cap is resolved (global default,
// per-token override, or SaaS plan), so Codex is always Nx the equivalent
// Claude limit regardless of source. 0/unlimited caps stay unlimited.
const codexLimitMultiplier = 5

// providerLimitScale returns the multiplier applied to RPM/concurrency caps for
// the given provider. Claude is the baseline (1x); Codex is codexLimitMultiplier.
func providerLimitScale(provider string) int {
	if auth.NormalizeProvider(provider) == auth.ProviderOpenAI {
		return codexLimitMultiplier
	}
	return 1
}

// clientMaxConcurrent returns the effective max concurrent requests for this
// client. SaaS-token override wins, then per-token override, then the global
// default. The resolved cap is scaled per provider (Codex gets a higher
// ceiling than Claude); a 0/unlimited cap is left untouched.
func (s *Server) clientMaxConcurrent(c *gin.Context, provider, clientToken string) int {
	base := s.clientMaxConcurrentBase(c, clientToken)
	if base <= 0 {
		return base
	}
	return base * providerLimitScale(provider)
}

func (s *Server) clientMaxConcurrentBase(c *gin.Context, clientToken string) int {
	if info, ok := saasInfoFrom(c); ok && info.MaxConcurrent > 0 {
		return info.MaxConcurrent
	}
	if entry, ok := s.tokens.Lookup(clientToken); ok && entry.MaxConcurrent > 0 {
		return entry.MaxConcurrent
	}
	return s.cfg.ClientMaxConcurrent
}

// clientRPM returns the effective requests-per-minute cap for this client. The
// resolved cap is scaled per provider (Codex gets a higher ceiling than
// Claude); a 0/unlimited cap is left untouched.
func (s *Server) clientRPM(c *gin.Context, provider, clientToken string) int {
	base := s.clientRPMBase(c, clientToken)
	if base <= 0 {
		return base
	}
	return base * providerLimitScale(provider)
}

func (s *Server) clientRPMBase(c *gin.Context, clientToken string) int {
	if info, ok := saasInfoFrom(c); ok && info.RPM > 0 {
		return info.RPM
	}
	if rpm, ok := s.tokens.RPM(clientToken); ok && rpm > 0 {
		return rpm
	}
	return s.cfg.ClientRPM
}
