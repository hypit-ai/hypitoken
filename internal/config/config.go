package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wjsoj/CPA-Claude/internal/saas"
	"github.com/wjsoj/CPA-Claude/internal/shop"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/pricing"
	"gopkg.in/yaml.v3"
)

type APIKey struct {
	Key      string `yaml:"key"`
	Provider string `yaml:"provider,omitempty"` // "anthropic" | "openai"; empty = anthropic (legacy)
	ProxyURL string `yaml:"proxy_url,omitempty"`
	Label    string `yaml:"label,omitempty"`
	BaseURL  string `yaml:"base_url,omitempty"`
	Group    string `yaml:"group,omitempty"`
	// ModelMap routes/rewrites client-facing model names to upstream model
	// names. See auth.Auth.ModelMap. Non-empty map turns this key into a
	// model-restricted credential. Empty = wildcard.
	ModelMap map[string]string `yaml:"model_map,omitempty"`
}

// EndpointConfig selects the listening host/port for one provider-scoped
// HTTP endpoint. An endpoint is considered live when Port > 0 and !Disabled
// — this lets users toggle Claude or Codex off without having to remove the
// whole section.
type EndpointConfig struct {
	Host     string `yaml:"host,omitempty"`
	Port     int    `yaml:"port,omitempty"`
	Disabled bool   `yaml:"disabled,omitempty"`
}

// IsEnabled reports whether this endpoint should be bound on startup.
func (e EndpointConfig) IsEnabled() bool { return !e.Disabled && e.Port > 0 }

// EndpointsConfig groups the per-provider endpoint configs. Claude and
// Codex share the same upstream credential pool, client token store,
// usage store, and request log — they differ only in the routes they
// expose and the credential subset they route to. Shop is an independent
// storefront (发卡网) that doesn't touch any of the proxy state.
type EndpointsConfig struct {
	Claude EndpointConfig `yaml:"claude"`
	Codex  EndpointConfig `yaml:"codex"`
	Shop   EndpointConfig `yaml:"shop"`
}

type Config struct {
	LogLevel string `yaml:"log_level"`

	// Endpoints configures per-provider HTTP listeners. See the struct doc
	// on EndpointsConfig. Admin panel + public status page are served on
	// whichever endpoint is designated primary (claude if enabled, else
	// codex, else startup fails).
	Endpoints EndpointsConfig `yaml:"endpoints"`

	// Directory containing OAuth credential JSON files.
	AuthDir string `yaml:"auth_dir"`

	// Persistence file for usage statistics and session state.
	StateFile string `yaml:"state_file"`

	// Off-host disaster-recovery backup to an S3-compatible bucket. Disabled
	// by default. See BackupConfig.
	Backup BackupConfig `yaml:"backup,omitempty"`

	// Minutes of inactivity after which a client session releases its OAuth slot.
	ActiveWindowMinutes int `yaml:"active_window_minutes"`

	// Token required to access the management panel and APIs.
	// Empty = panel disabled. Send as X-Admin-Token header (or Authorization: Bearer).
	AdminToken string `yaml:"admin_token,omitempty"`

	// API-key fallback pool. No concurrency limit.
	APIKeys []APIKey `yaml:"api_keys"`

	// Default upstream proxy URL used when an OAuth file has none specified.
	DefaultProxyURL string `yaml:"default_proxy_url,omitempty"`

	// Anthropic API base URL (override for testing).
	AnthropicBaseURL string `yaml:"anthropic_base_url,omitempty"`

	// OpenAI API base URL (override for testing; used for BYOK Codex API-key
	// routing). Defaults to https://api.openai.com/v1.
	//
	// The full upstream URL is `BaseURL + endpoint` where endpoint is
	// /responses, /chat/completions, /models, etc — without a /v1/ prefix.
	// If a third-party gateway (e.g. tcdmx) serves at the root without /v1/,
	// configure the credential with BaseURL=https://gateway.example and
	// it'll receive /responses directly. This makes BaseURL authoritative
	// for whether /v1/ ends up in the upstream URL.
	OpenAIBaseURL string `yaml:"openai_base_url,omitempty"`

	// Codex OAuth-authenticated requests hit the ChatGPT backend, not the
	// public OpenAI API. This base URL is here so installations behind
	// vendor relays can override it; normally unchanged.
	ChatGPTBackendBaseURL string `yaml:"chatgpt_backend_base_url,omitempty"`

	// If true, OAuth/API-key refresh+request uses utls Chrome fingerprint.
	UseUTLS bool `yaml:"use_utls"`

	// CodexWS controls the Codex /v1/responses WebSocket ingress (and the
	// WebSocket upstream to chatgpt.com). Disabled by default — the proxy keeps
	// serving Codex over the proven HTTP POST + SSE path until WS is enabled
	// per-deployment. See CodexWSConfig.
	CodexWS CodexWSConfig `yaml:"codex_ws"`

	// CodexSidecar controls emulation of the auxiliary traffic a real Codex
	// client emits alongside its turns — the plugin store, MCP discovery, the
	// model catalog, user settings, analytics and OTLP metrics.
	//
	// Disabled by default, and the default is the conservative side of a real
	// trade-off rather than a placeholder. A relayed account that emits only
	// business requests looks unlike a desktop install; an account that emits
	// auxiliary traffic we got subtly wrong looks unlike anything at all. The
	// emulator is built so the second failure cannot happen quietly — analytics
	// events cannot be constructed without ids from a turn that actually
	// occurred, and per-account attributes vary — but it is still new, so it is
	// opt-in per deployment.
	CodexSidecar CodexSidecarConfig `yaml:"codex_sidecar"`

	// Directory for per-request JSONL logs (one file per day:
	// requests-YYYY-MM-DD.jsonl). Empty = disabled.
	LogDir string `yaml:"log_dir,omitempty"`

	// Default maximum concurrent in-flight requests per client token.
	// 0 = unlimited. Per-token overrides take precedence.
	ClientMaxConcurrent int `yaml:"client_max_concurrent"`

	// Maximum pool slots one client token may hold at once for a single
	// provider. 0 = unlimited.
	//
	// A fair-share cap, distinct from ClientMaxConcurrent. A pool slot is what a
	// credential's max_concurrent rations, and slots are held for very unequal
	// durations: an HTTP request holds one for seconds, but a codex-tui
	// WebSocket session holds one for as long as the socket is open — chatgpt.com
	// keeps those alive up to an hour. Without this a couple of WS users can sit
	// on most of a provider's slot capacity and everyone else gets "no
	// credentials available" from a healthy fleet. Only NEW slots are refused; a
	// session already holding one keeps working.
	ClientMaxSessions int `yaml:"client_max_sessions"`

	// TrustedRelayTokens are client tokens belonging to a proxy we also run —
	// today CPA-Claude, which can route here through a single API key. Traffic
	// on such a token carries cc-core/relay headers naming the DOWNSTREAM
	// caller; for a token listed here (and ONLY for one listed here) those
	// headers are believed and become the scheduler slot, so the relay's users
	// spread across credentials instead of all pinning to one.
	//
	// This is routing only: RPM, concurrency, quota and billing stay keyed on
	// the relay's own token, because the relay is one paying customer however
	// many users sit behind it — and because a limit keyed on a self-asserted
	// header is a limit anyone can evade by inventing a new value. The one
	// exception is client_max_sessions, which counts slots and would otherwise
	// refuse the very fan-out this enables; a trusted relay is exempt from it.
	//
	// Entries may be a raw token or "sha256:<hex>" so a config file need not
	// hold the secret in the clear. Empty (the default) trusts nobody, and the
	// headers are stripped from every request.
	TrustedRelayTokens []string `yaml:"trusted_relay_tokens,omitempty"`

	// Default sliding-window requests-per-minute cap per client token.
	// 0 = unlimited. Per-token overrides take precedence.
	ClientRPM int `yaml:"client_rpm"`

	// Days to retain rotated request logs. 0 = disable GC (keep forever).
	LogRetentionDays int `yaml:"log_retention_days,omitempty"`

	// Opt out of the SQLite index over the request log (requests.db inside
	// log_dir). The index is derived state that makes every admin and
	// per-user aggregate cheap; disabling it falls back to re-scanning the
	// JSONL on each query, which at ~1M records costs tens of seconds.
	// Here as an escape hatch, not a tuning knob.
	LogIndexDisabled bool `yaml:"log_index_disabled,omitempty"`

	// Stop writing the daily-rotated requests-*.jsonl files and keep request
	// history only in the index. Halves the disk the log costs and turns a
	// client-token rename from a rewrite of every archived file into one
	// UPDATE.
	//
	// The trade is real: while the archive exists the index can be deleted and
	// rebuilt from it, and a failed insert is retried from the file on the next
	// pass. With the archive off neither is true — a failed insert is a lost
	// record, and requests.db is the only copy on the box. The daily off-host
	// backup does carry it (buildManifest snapshots it, and refuses to ship an
	// archive without it while this is on), so the exposure is bounded by the
	// backup interval rather than open-ended. Requires the index (mutually
	// exclusive with log_index_disabled), and `hypitoken export-requests` is
	// the way back out to a .jsonl file.
	LogJSONLDisabled bool `yaml:"log_jsonl_disabled,omitempty"`

	// Pricing overrides (optional). Built-in defaults cover claude-haiku-4-5,
	// claude-opus-4-6, and claude-sonnet-4-6.
	Pricing pricing.Config `yaml:"pricing"`

	// TokenGroups declares the named credential-group registry that client
	// tokens can attach to. A token's Groups slice is a priority-ordered
	// list of these names; AcquireMulti walks them in order.
	//
	// Each entry binds a group to an upstream channel + a billing discount.
	// Models is an optional whitelist (empty = accept everything that the
	// upstream supports).
	TokenGroups []TokenGroup `yaml:"token_groups,omitempty"`

	// SaaS multi-tenant layer (commercial mode). Disabled by default; the
	// proxy behaves exactly like the OSS build when SaaS.Enabled is false.
	SaaS saas.Config `yaml:"saas"`

	// Shop is the standalone 发卡网 storefront — independent of SaaS. When
	// Shop.Enabled is false and endpoints.shop is disabled, no shop code
	// runs and no extra listener binds.
	Shop shop.Config `yaml:"shop"`
}

// CodexSidecarConfig gates the Codex auxiliary-traffic emulator.
type CodexSidecarConfig struct {
	// Enabled turns the emulator on. Off by default.
	Enabled bool `yaml:"enabled"`
}

// CodexWSConfig configures the Codex WebSocket transport. WebSocket carries
// protocol-level ping/pong, so it survives the multi-second silent gaps that
// truncate the legacy HTTP SSE path and surface to clients as "stream
// disconnected before completion". Real codex-tui 0.135.0 uses this transport.
type CodexWSConfig struct {
	// Enabled turns on the WS upgrade route on /v1/responses. Default false:
	// ship dark, enable per-deployment after smoke-testing against a real
	// ChatGPT token. The HTTP POST path is unaffected either way.
	Enabled bool `yaml:"enabled"`

	// ForceHTTP, when true, accepts a client WS upgrade but bridges it over the
	// proven HTTP upstream path instead of dialing an upstream WS. Emergency
	// degrade valve if the WS upstream misbehaves. Default false.
	ForceHTTP bool `yaml:"force_http,omitempty"`

	// BetaVersion selects the responses_websockets beta marker sent upstream:
	// "v2" (default, 2026-02-06) or "v1" (2026-02-04).
	BetaVersion string `yaml:"beta_version,omitempty"`

	// ReadLimitBytes caps a single inbound WS message. 0 => 16 MiB.
	ReadLimitBytes int64 `yaml:"read_limit_bytes,omitempty"`

	// Upstream configures the EGRESS transport for ordinary HTTP requests —
	// independent of Enabled above, which is about the WS ingress route.
	Upstream CodexWSUpstreamConfig `yaml:"upstream,omitempty"`
}

// CodexWSUpstreamConfig selects and tunes the WebSocket egress path: forwarding
// an HTTP-ingress Codex request over an upstream WebSocket instead of the
// legacy HTTP POST /codex/responses.
//
// The two directions are deliberately separate settings. CodexWSConfig.Enabled
// opens a WS route for clients that already speak the protocol; this block
// changes what we do upstream for the HTTP clients that are the overwhelming
// majority of the traffic. Turning one on has never implied the other.
type CodexWSUpstreamConfig struct {
	// Mode is one of:
	//
	//	"sse"  — always use the HTTP POST path. The default, and the behaviour
	//	         every release before this one had.
	//	"auto" — try the WebSocket first and fall back to HTTP within the same
	//	         attempt when it fails before any byte reaches the client. A
	//	         failure is invisible to the caller, costing only latency.
	//	"ws"   — try the WebSocket and do NOT fall back. Diagnostic only: it
	//	         makes a transport fault visible instead of masking it.
	//
	// Anything unrecognised is treated as "sse", so a typo degrades to the
	// proven path rather than to an unintended one.
	Mode string `yaml:"mode,omitempty"`

	// PoolIdleSeconds closes a pooled upstream socket left unused this long.
	// 0 => 300 (5 min). Reuse is the whole point of the pool: a socket dialed
	// per request would cost one TLS handshake per turn against an edge that
	// rate-limits new connections, which is strictly worse than the pooled h2
	// transport the HTTP path already uses.
	PoolIdleSeconds int `yaml:"pool_idle_seconds,omitempty"`

	// PoolMaxAgeSeconds retires a socket this long after it was dialed.
	// 0 => 3300 (55 min), just inside where the backend retires its own.
	PoolMaxAgeSeconds int `yaml:"pool_max_age_seconds,omitempty"`

	// PoolMaxEntries caps pooled sockets across all accounts. 0 => 512.
	// Reaching the cap serves the turn on an unpooled socket rather than
	// failing it.
	PoolMaxEntries int `yaml:"pool_max_entries,omitempty"`

	// ReadTimeoutSeconds bounds the wait for each upstream frame. 0 => 180.
	//
	// This is not a turn budget. The backend parks a queued turn and heartbeats
	// with `keepalive` frames roughly every 30s, so a short value here cuts off
	// turns that were only waiting for capacity — exactly the turns the
	// WebSocket transport exists to hold on to.
	ReadTimeoutSeconds int `yaml:"read_timeout_seconds,omitempty"`

	// FallbackCooldownSeconds is how long one conversation stays pinned to the
	// HTTP path after its WebSocket attempt failed. 0 => 600 (10 min).
	//
	// Without a cooldown a backend that refuses WebSockets for an account makes
	// every one of its requests pay a failed dial before falling back. With it,
	// the first failure pays and the rest go straight to HTTP.
	FallbackCooldownSeconds int `yaml:"fallback_cooldown_seconds,omitempty"`
}

// Codex WebSocket egress modes.
const (
	CodexWSUpstreamSSE  = "sse"
	CodexWSUpstreamAuto = "auto"
	CodexWSUpstreamWS   = "ws"
)

// Normalize fills the zero values and forces an unrecognised mode to "sse".
//
// Defaulting an unknown mode to the proven path rather than rejecting the
// config is deliberate: a typo in this block must not take a deployment down,
// and must not silently opt it into the new transport either.
func (u *CodexWSUpstreamConfig) Normalize() {
	switch strings.ToLower(strings.TrimSpace(u.Mode)) {
	case CodexWSUpstreamAuto:
		u.Mode = CodexWSUpstreamAuto
	case CodexWSUpstreamWS:
		u.Mode = CodexWSUpstreamWS
	default:
		u.Mode = CodexWSUpstreamSSE
	}
	if u.PoolIdleSeconds <= 0 {
		u.PoolIdleSeconds = 300
	}
	if u.PoolMaxAgeSeconds <= 0 {
		u.PoolMaxAgeSeconds = 3300
	}
	if u.PoolMaxEntries <= 0 {
		u.PoolMaxEntries = 512
	}
	if u.ReadTimeoutSeconds <= 0 {
		u.ReadTimeoutSeconds = 180
	}
	if u.FallbackCooldownSeconds <= 0 {
		u.FallbackCooldownSeconds = 600
	}
}

// WSEgressEnabled reports whether an HTTP-ingress Codex request may be
// forwarded over a WebSocket.
func (u CodexWSUpstreamConfig) WSEgressEnabled() bool {
	return u.Mode == CodexWSUpstreamAuto || u.Mode == CodexWSUpstreamWS
}

// HTTPFallbackAllowed reports whether a failed WebSocket attempt may retry the
// same turn over HTTP. False only in the diagnostic "ws" mode.
func (u CodexWSUpstreamConfig) HTTPFallbackAllowed() bool {
	return u.Mode != CodexWSUpstreamWS
}

// PoolIdle / PoolMaxAge / ReadTimeout / FallbackCooldown are the normalized
// durations. Callers should use these rather than the raw second counts, which
// are zero until Load has normalized them.
func (u CodexWSUpstreamConfig) PoolIdle() time.Duration {
	return time.Duration(u.PoolIdleSeconds) * time.Second
}

func (u CodexWSUpstreamConfig) PoolMaxAge() time.Duration {
	return time.Duration(u.PoolMaxAgeSeconds) * time.Second
}

func (u CodexWSUpstreamConfig) ReadTimeout() time.Duration {
	return time.Duration(u.ReadTimeoutSeconds) * time.Second
}

func (u CodexWSUpstreamConfig) FallbackCooldown() time.Duration {
	return time.Duration(u.FallbackCooldownSeconds) * time.Second
}

// TokenGroup defines one named credential group + its upstream channel +
// per-group billing discount. Declared at config.token_groups[]; consumed
// by the dispatch layer (which upstream to forward to) and the billing
// layer (how to multiply the recorded cost).
type TokenGroup struct {
	// Name is the group identifier as it appears in clienttoken.Token.Groups.
	// Required; canonicalized via auth.NormalizeGroup at load time.
	Name string `yaml:"name"`

	// Upstream selects the channel. Only "anthropic" (the OAuth/API-key path
	// to api.anthropic.com) exists today; the field is retained so future
	// channels can be added without a config migration. Defaults to "anthropic".
	Upstream string `yaml:"upstream,omitempty"`

	// Discount is a multiplier applied to the official Anthropic price
	// before debiting the user's wallet. 1.0 = no discount, 0.05 = 1/20.
	// Must be > 0. Defaults to 1.0.
	Discount float64 `yaml:"discount,omitempty"`

	// Models is an optional whitelist (Anthropic-side model names). Empty
	// = accept any model the upstream supports.
	Models []string `yaml:"models,omitempty"`
}

// Upstream constants for TokenGroup.Upstream.
const (
	UpstreamAnthropic = "anthropic"
)

// AcceptsModel reports whether g's whitelist allows model (empty list = wildcard).
func (g *TokenGroup) AcceptsModel(model string) bool {
	if len(g.Models) == 0 {
		return true
	}
	for _, m := range g.Models {
		if m == model {
			return true
		}
	}
	return false
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(cfg, path)
	cfg.DefaultProxyURL = strings.TrimSpace(cfg.DefaultProxyURL)
	if err := auth.ValidateProxyURL(cfg.DefaultProxyURL); err != nil {
		return nil, fmt.Errorf("default_proxy_url: %w", err)
	}
	for i := range cfg.APIKeys {
		cfg.APIKeys[i].ProxyURL = strings.TrimSpace(cfg.APIKeys[i].ProxyURL)
		if err := auth.ValidateProxyURL(cfg.APIKeys[i].ProxyURL); err != nil {
			return nil, fmt.Errorf("api_keys[%d].proxy_url: %w", i, err)
		}
	}
	// Turning off both the archive and the index would leave the request-log
	// writer with nowhere to put a record. Refuse the combination rather than
	// start up and silently discard request history.
	if cfg.LogJSONLDisabled && cfg.LogIndexDisabled {
		return nil, fmt.Errorf("config: log_jsonl_disabled requires the index; unset log_index_disabled")
	}
	return cfg, nil
}

func applyDefaults(c *Config, path string) {
	if c.Endpoints.Claude.Port == 0 {
		c.Endpoints.Claude.Port = 8317
	}
	if c.Endpoints.Claude.Host == "" {
		c.Endpoints.Claude.Host = "0.0.0.0"
	}
	if c.Endpoints.Codex.Port == 0 {
		// Codex endpoint defaults to configured-but-disabled so merely
		// upgrading the server binary doesn't flip on an empty listener.
		c.Endpoints.Codex.Port = 8318
		c.Endpoints.Codex.Disabled = true
	}
	if c.Endpoints.Codex.Host == "" {
		c.Endpoints.Codex.Host = "0.0.0.0"
	}
	if c.Endpoints.Shop.Port == 0 {
		// Shop defaults to configured-but-disabled so existing deployments
		// don't bind an extra listener on upgrade. Enable explicitly via
		// endpoints.shop.disabled=false + shop.enabled=true.
		c.Endpoints.Shop.Port = 8319
		c.Endpoints.Shop.Disabled = true
	}
	if c.Endpoints.Shop.Host == "" {
		c.Endpoints.Shop.Host = "0.0.0.0"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.ActiveWindowMinutes == 0 {
		c.ActiveWindowMinutes = 5
	}
	if c.ClientMaxConcurrent == 0 {
		c.ClientMaxConcurrent = 15
	}
	if c.ClientRPM == 0 {
		c.ClientRPM = 60
	}
	if c.AnthropicBaseURL == "" {
		c.AnthropicBaseURL = "https://api.anthropic.com"
	}
	if c.OpenAIBaseURL == "" {
		c.OpenAIBaseURL = "https://api.openai.com/v1"
	}
	if c.ChatGPTBackendBaseURL == "" {
		c.ChatGPTBackendBaseURL = "https://chatgpt.com/backend-api"
	}
	if c.CodexWS.BetaVersion == "" {
		c.CodexWS.BetaVersion = "v2"
	}
	if c.CodexWS.ReadLimitBytes == 0 {
		c.CodexWS.ReadLimitBytes = 16 << 20
	}
	c.CodexWS.Upstream.Normalize()
	dir := filepath.Dir(path)
	if c.AuthDir == "" {
		c.AuthDir = filepath.Join(dir, "auths")
	} else if !filepath.IsAbs(c.AuthDir) {
		c.AuthDir = filepath.Join(dir, c.AuthDir)
	}
	if c.StateFile == "" {
		c.StateFile = filepath.Join(dir, "state.json")
	} else if !filepath.IsAbs(c.StateFile) {
		c.StateFile = filepath.Join(dir, c.StateFile)
	}
	if c.LogDir != "" && !filepath.IsAbs(c.LogDir) {
		c.LogDir = filepath.Join(dir, c.LogDir)
	}
	if c.LogRetentionDays == 0 {
		c.LogRetentionDays = 90
	}
	c.SaaS.ApplyDefaults(filepath.Dir(path))
	c.Shop.ApplyDefaults(filepath.Dir(path))
	if c.Shop.DBPath != "" && !filepath.IsAbs(c.Shop.DBPath) {
		c.Shop.DBPath = filepath.Join(filepath.Dir(path), c.Shop.DBPath)
	}

	c.Backup.applyDefaults()
	c.applyTokenGroupDefaults()
}

// applyTokenGroupDefaults seeds the built-in claude-official group (no
// discount, anthropic upstream) if the config omits it entirely.
func (c *Config) applyTokenGroupDefaults() {
	have := make(map[string]bool, len(c.TokenGroups))
	for i := range c.TokenGroups {
		g := &c.TokenGroups[i]
		g.Name = strings.TrimSpace(strings.ToLower(g.Name))
		if g.Upstream == "" {
			g.Upstream = UpstreamAnthropic
		}
		if g.Discount <= 0 {
			g.Discount = 1.0
		}
		if g.Name != "" {
			have[g.Name] = true
		}
	}
	if !have["claude-official"] {
		c.TokenGroups = append(c.TokenGroups, TokenGroup{
			Name:     "claude-official",
			Upstream: UpstreamAnthropic,
			Discount: 1.0,
		})
	}
}

// FindTokenGroup looks up a TokenGroup by (canonicalized) name. Returns nil
// when not found. Callers shouldn't mutate the returned pointer.
func (c *Config) FindTokenGroup(name string) *TokenGroup {
	name = strings.TrimSpace(strings.ToLower(name))
	for i := range c.TokenGroups {
		if c.TokenGroups[i].Name == name {
			return &c.TokenGroups[i]
		}
	}
	return nil
}
