// Package saas implements the multi-tenant commercial layer on top of the
// existing CPA-Claude proxy: user accounts, per-user API tokens, USD wallet
// billed per request, CNY top-up via Alipay, and a fresh frontend.
//
// When the Config.Enabled flag is false the package is dormant — main.go
// skips wiring its routes and the proxy behaves exactly like the OSS build.
package saas

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config is the YAML shape of the `saas` block. Marshaled by config.Config.
type Config struct {
	Enabled bool `yaml:"enabled"`

	// DBPath is where the SQLite file lives. Relative paths resolve against
	// the dir of the loaded config file.
	DBPath string `yaml:"db_path"`

	// LocalSnapshotDays is how many daily on-disk snapshots to keep in
	// <dbdir>/backups. These are the local safety net; the off-host encrypted
	// backup (see the top-level `backup:` block) is the real one, so this does
	// not need a long tail. Default 14 — at ~190 MB per snapshot and growing,
	// the previous hardcoded 30 was heading for ~6 GB. Set 0 for the default.
	LocalSnapshotDays int `yaml:"local_snapshot_days,omitempty"`

	// JWTSecret is the HS256 signing key. If empty on first start, a 32-byte
	// random secret is generated and persisted to <DBPath>.jwt_secret (mode
	// 0600) so subsequent starts reuse it.
	JWTSecret     string        `yaml:"jwt_secret"`
	JWTTTL        time.Duration `yaml:"jwt_ttl"`
	JWTRefreshTTL time.Duration `yaml:"jwt_refresh_ttl"`

	// Site identity (used in emails and the frontend).
	SiteName string `yaml:"site_name"`
	SiteURL  string `yaml:"site_url"`

	// AdminEmail / AdminPassword bootstrap a default admin user on first
	// start when the users table is empty. After bootstrap they are
	// ignored — change role/password through the panel.
	AdminEmail    string `yaml:"admin_email"`
	AdminPassword string `yaml:"admin_password"`

	// FreeRegister: if false, registration is disabled (operator pre-creates
	// users). Default true.
	FreeRegister *bool `yaml:"free_register"`

	// SignupBonusUSD is the trial credit granted to every new user who did NOT
	// arrive through a marketing channel (?ref=). Channel signups get the
	// channel's own bonus instead (see internal/saas/growth).
	//
	// Default 0 — the welcome-credit programme is SUSPENDED. It was farmed:
	// the 2026-08-08 incident burned ~$116 across 168 throwaway signups. The
	// key is kept so an existing production config.yaml keeps parsing.
	//
	// It is subordinate to ReferralsEnabled: an amount set here is granted only
	// while referrals_enabled is true. That matters on rollout — a deployed
	// config may already pin signup_bonus_usd to a positive value, and this
	// suspension must take effect from the binary alone, with no config edit.
	SignupBonusUSD *float64 `yaml:"signup_bonus_usd"`

	// ReferralsEnabled is the master switch for the whole invite / referral /
	// marketing-channel-attribution programme: personal invite codes, the
	// two-sided invite bonus, milestone tiers, ?ref= channel attribution and
	// its public visit/dwell beacons.
	//
	// Default FALSE — suspended after the 2026-08-08 farming incident (168
	// signups, ~$116 granted through invite fission). Off means the user-facing
	// routes are not mounted and registration grants nothing and records no
	// conversion; the packages, tables, ledger history and the admin-side
	// historical/audit routes stay intact so what was already granted can be
	// audited. Set to true to re-enable.
	ReferralsEnabled *bool `yaml:"referrals_enabled"`

	// SignupFraud tunes the signup anti-abuse check that withholds the welcome
	// bonus from a device/network that already registered.
	SignupFraud SignupFraudConfig `yaml:"signup_fraud"`

	// Invoice configures the 开票 flow: where company-name lookups go and which
	// 对公 account customers transfer to. Both have working built-in defaults —
	// set these only to override.
	Invoice InvoiceConfig `yaml:"invoice"`

	// SMTP outbound mail. If Host is empty, mailer logs to stderr and
	// (in dev) prints verification codes inline.
	SMTP SMTPConfig `yaml:"smtp"`

	// Payment gateway selector. One of "alipay" (direct merchant), "zpay"
	// (易支付 aggregator), or "mock" (auto-confirms after 2s — dev only).
	// Empty defaults to whichever block is configured: zpay if zpay.pid
	// set, alipay if alipay.app_id set, else mock — but the mock fallback is
	// hard-blocked on a public/production site (see AllowMockPayment).
	PaymentProvider string `yaml:"payment_provider,omitempty"`

	// AllowMockPayment opts a public deployment into the mock gateway, which
	// auto-confirms every top-up after 2s without taking real money. It is
	// otherwise refused whenever SiteURL points at a non-local host, so a
	// misconfigured production server can never silently hand out free credit.
	// Leave false in production. Local dev (localhost SiteURL) needs no flag.
	AllowMockPayment bool `yaml:"allow_mock_payment,omitempty"`

	// MaxOverdraftUSD caps how far a user's USD wallet may be driven negative
	// by in-flight requests (a single huge request, or many concurrent ones,
	// can each pass the balance>0 PreCheck and then bill against a near-zero
	// balance). Charges are clamped so the wallet can never rest below
	// -MaxOverdraftUSD. nil → default 10.0; set to 0 to disable the floor
	// (unbounded negative — not recommended).
	MaxOverdraftUSD *float64 `yaml:"max_overdraft_usd"`

	// Alipay direct-merchant gateway. Requires real merchant onboarding.
	Alipay AlipayConfig `yaml:"alipay"`

	// Z-Pay 易支付 aggregator gateway — works for individual operators
	// without a business license.
	ZPay ZPayConfig `yaml:"zpay"`

	// Stripe gateway — runs *alongside* the QR gateway above (zpay/alipay),
	// not instead of it. When enabled it powers the embedded Payment Element
	// top-up surface (card / Alipay / WeChat Pay / crypto, all charged 1:1 in
	// USD). The QR gateway stays available as a fallback so a Stripe outage or
	// an Alipay-method restriction never takes down top-ups entirely.
	Stripe StripeConfig `yaml:"stripe"`

	// Exchange rate source. Empty = built-in fawazahmed0 currency-api.
	ExchangeRateURL   string  `yaml:"exchange_rate_url"`
	FallbackCNYPerUSD float64 `yaml:"fallback_cny_per_usd"`

	// HealthCheck cadence for API-key credentials.
	HealthCheckInterval time.Duration `yaml:"health_check_interval"`
	// CLI-only or probe-restricted upstreams use recent real requests instead of synthetic probes.
	HealthPassiveAuthIDs []string `yaml:"health_passive_auth_ids"`

	// ServiceTokens authenticate a SIBLING SERVICE — today HypiHub, the
	// image/video generation gateway — calling the machine-to-machine
	// /api/v2/svc/* routes to resolve a user token and move that user's
	// wallet. They are NOT user credentials and grant no user identity: a
	// holder can debit any workspace, so treat them exactly like a database
	// password.
	//
	// The /api/v2/svc group is mounted only while this list is non-empty; an
	// empty list (the default) means those routes do not exist at all, which
	// is the correct posture for every deployment that doesn't run HypiHub.
	//
	// These routes MUST NOT be internet-reachable. Bind the sibling to a
	// private interface or block /api/v2/svc/ at the reverse proxy — the
	// token is the only thing standing between the public internet and every
	// wallet in the fleet.
	//
	// Each entry may be a raw token, "sha256:<hex>" so the config need not
	// hold the secret in the clear, or "@/path/to/file" to read it from a file
	// (one token per line) — the same conventions as trusted_relay_tokens and
	// the payment-gateway keys.
	ServiceTokens []string `yaml:"service_tokens,omitempty"`

	// SSOReturnOrigins is the EXACT-match allowlist of origins a cross-site
	// single-sign-on handoff may be sent to (see docs/SPEC.md §12 in the
	// hypihub repo). Each entry is an origin — scheme://host[:port], no path,
	// e.g. "https://hub.novadiffusion.com".
	//
	// POST /api/v2/auth/sso/code takes a return_url from the browser, and the
	// code it mints is, for its 120-second life, that user's session. Sending
	// it to an attacker-chosen origin is a full account takeover, so the check
	// is exact string equality against a normalized origin and nothing else:
	// no prefix match, no suffix match, no wildcard subdomain. Every one of
	// those is an open redirect wearing an allowlist's clothes — a prefix
	// match on "https://hub.example.com" admits "https://hub.example.com.evil",
	// and a "*.example.com" wildcard admits any subdomain an attacker can get
	// a CNAME on.
	//
	// Empty (the default) means the SSO code endpoint is not mounted at all,
	// which is the right posture for every deployment without a sibling site.
	SSOReturnOrigins []string `yaml:"sso_return_origins,omitempty"`

	// TopupReturnOrigins is the EXACT-match allowlist of origins a wallet
	// top-up may send the buyer back to after a redirect-based payment
	// (Alipay/WeChat via Stripe). Same shape as SSOReturnOrigins: one origin
	// per entry, scheme://host[:port], no path.
	//
	// POST /api/v2/topup accepts an optional return_url so a top-up started
	// inside the sibling HypiHub product lands back on HypiHub instead of
	// here. HypiHub renders the Stripe Payment Element itself and never
	// touches money — it proxies the intent request and watches the order —
	// so the ONLY thing this key grants is "where may the browser be sent
	// back to", and the only defence against that being an attacker's site is
	// this list.
	//
	// DELIBERATELY A SEPARATE KEY from sso_return_origins rather than a reuse
	// of it. The two grant different things: an SSO code IS the user's session
	// for 120 seconds, while this is a redirect target on a money-in path.
	// Folding them together would mean that shipping this binary silently
	// widened an existing payment endpoint on every deployment that already
	// had SSO configured — the opposite of "no config, no new behaviour".
	// Operators who want both list the origin twice; that is the honest cost
	// of the two capabilities being independently revocable.
	//
	// Exact string equality on the normalized origin — no prefix match, no
	// suffix match, no wildcard subdomain, for exactly the reasons spelled out
	// on SSOReturnOrigins. Empty (the default) means a request carrying
	// return_url is REJECTED, so an unconfigured deployment behaves byte for
	// byte as it does today.
	TopupReturnOrigins []string `yaml:"topup_return_origins,omitempty"`
}

// OriginAllowlist is a parsed, normalized set of origins compared by exact
// equality.
//
// Parsed ONCE at startup, not per request: a return_url check that re-parses
// operator config on the hot path is a check that can start failing open
// halfway through the process's life. Malformed entries are a boot-time fatal
// (see NewOriginAllowlist) rather than a silently-dropped line, because an
// allowlist that quietly lost its only entry looks exactly like a working one
// until somebody notices the feature is off — or, worse, until the code that
// consumes it treats "empty" as "allow".
type OriginAllowlist struct {
	origins []string
}

// NewOriginAllowlist normalizes each entry to lowercase scheme://host[:port].
//
// Returns (nil, nil) for an empty list: that is "the feature is off", and the
// caller must not mount the route. Any malformed entry is an error the caller
// is expected to treat as fatal.
//
// Rejected deliberately: anything carrying a path, query, fragment, or
// userinfo. Those are not origins, and accepting them would invite the belief
// that the path portion is being checked — it never is, so "https://x.com/safe"
// in the config would silently authorize "https://x.com/anything".
func NewOriginAllowlist(entries []string) (*OriginAllowlist, error) {
	return NewNamedOriginAllowlist("saas.sso_return_origins", entries)
}

// NewNamedOriginAllowlist is NewOriginAllowlist with the config key spelled
// out, so a second allowlist (saas.topup_return_origins) can fail at boot with
// a message naming the line the operator actually has to fix. Same rules, same
// normalization, same (nil, nil) meaning "the feature is off".
func NewNamedOriginAllowlist(field string, entries []string) (*OriginAllowlist, error) {
	a := &OriginAllowlist{}
	for _, raw := range entries {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// Checked before parsing so a wildcard always earns the wildcard
		// message rather than whatever generic complaint the parser reaches
		// first — the operator who wrote "*.example.com" needs to be told why
		// it is refused, not merely that it is.
		if strings.Contains(raw, "*") {
			return nil, fmt.Errorf("%s: %q: wildcards are not supported — "+
				"a wildcard origin is an open redirect; list each origin in full", field, raw)
		}
		origin, err := NormalizeOrigin(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %w", field, raw, err)
		}
		if !slices.Contains(a.origins, origin) {
			a.origins = append(a.origins, origin)
		}
	}
	if len(a.origins) == 0 {
		return nil, nil
	}
	return a, nil
}

// NormalizeOrigin parses one origin string into canonical lowercase
// scheme://host[:port] form. The default port for the scheme is dropped so
// "https://x.com:443" and "https://x.com" compare equal.
func NormalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("not a valid URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New(`scheme must be "http" or "https"`)
	}
	if u.User != nil {
		return "", errors.New("must not contain userinfo")
	}
	if u.Opaque != "" {
		return "", errors.New("must be scheme://host[:port]")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("missing host")
	}
	// Only a bare origin, or an origin with the empty/"/" path a browser sends
	// as document.origin. Anything more specific is not an origin.
	if p := strings.TrimSuffix(u.EscapedPath(), "/"); p != "" {
		return "", errors.New("must not contain a path — list the origin only")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("must not contain a query or fragment")
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		// net/url already rejects a non-numeric port, but be explicit: this
		// value is about to be compared by string equality.
		if _, err := strconv.Atoi(port); err != nil {
			return "", errors.New("invalid port")
		}
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host, nil
}

// Allows reports whether rawURL's origin is on the list, and returns the
// trimmed URL the caller should hand back to the browser.
//
// EXACT equality on the normalized origin. The path of rawURL is not checked
// and not restricted — the origin is the whole security boundary here, since
// any page on an allowed origin is already trusted with the user's session
// there. What matters is that no unlisted origin can ever be reached, which is
// why this is string equality and not any form of matching.
func (a *OriginAllowlist) Allows(rawURL string) (string, bool) {
	if a == nil || len(a.origins) == 0 {
		return "", false
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil || u.Opaque != "" {
		return "", false
	}
	origin, err := NormalizeOrigin(u.Scheme + "://" + u.Host)
	if err != nil {
		return "", false
	}
	if !slices.Contains(a.origins, origin) {
		return "", false
	}
	return rawURL, true
}

// Origins returns the normalized entries, for logging and for the SPA's own
// pre-flight check. Copy, so a caller cannot mutate the allowlist.
func (a *OriginAllowlist) Origins() []string {
	if a == nil {
		return nil
	}
	return slices.Clone(a.origins)
}

// Len reports how many origins are configured. Nil-safe.
func (a *OriginAllowlist) Len() int {
	if a == nil {
		return 0
	}
	return len(a.origins)
}

type SMTPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	From     string `yaml:"from"`
	UseTLS   bool   `yaml:"use_tls"`
}

// InvoiceConfig overrides the 开票 defaults. Empty fields keep the built-in
// value, so a partial block is safe.
type InvoiceConfig struct {
	// TitleSuggestURL is the company-name suggest endpoint proxied for the
	// 抬头 picker. Defaults to 天眼查's public one.
	TitleSuggestURL string `yaml:"title_suggest_url,omitempty"`
	// The 对公转账 destination shown after an invoice request is filed.
	AccountNo   string `yaml:"account_no,omitempty"`
	AccountName string `yaml:"account_name,omitempty"`
	BankBranch  string `yaml:"bank_branch,omitempty"`
	BankCode    string `yaml:"bank_code,omitempty"`
}

// SignupFraudConfig tunes the welcome-bonus anti-abuse check (internal/saas/
// growth/fraud.go). A new signup is flagged — and its bonus withheld — when its
// browser fingerprint matches a prior user, or when its /24 (IPv6 /48) subnet
// produced at least IPSubnetThreshold distinct signups within WindowHours.
// Enabled defaults to true; a zero threshold/window falls back to 3 / 720h.
type SignupFraudConfig struct {
	Enabled           *bool `yaml:"enabled"`             // default true
	IPSubnetThreshold int   `yaml:"ip_subnet_threshold"` // default 3
	WindowHours       int   `yaml:"window_hours"`        // default 720 (30 days)
	// RequireFingerprint withholds the bonus from a signup carrying no browser
	// fingerprint at all — the shape of a scripted registration that never
	// loaded the page. Default true. Set false only if a legitimate integration
	// registers users server-side.
	RequireFingerprint *bool `yaml:"require_fingerprint"`
	// EmailDomainThreshold flags a burst of signups sharing one non-mainstream
	// email domain within the window. Default 3.
	EmailDomainThreshold int `yaml:"email_domain_threshold"`
	// DisposableDomains extends the built-in throwaway-mailbox blocklist. Exact
	// domains, or a leading "." for a suffix match (".example.tld").
	DisposableDomains []string `yaml:"disposable_domains"`
}

// ZPayConfig configures the Z-Pay aggregator gateway. Key is sensitive
// — prefer the @path/to/file form so the secret stays out of the YAML
// committed to git.
type ZPayConfig struct {
	BaseURL   string `yaml:"base_url,omitempty"` // default https://zpayz.cn
	PID       string `yaml:"pid"`                // 商户ID
	Key       string `yaml:"key"`                // 商户密钥, or @/path/to/file
	NotifyURL string `yaml:"notify_url"`         // public webhook
	ReturnURL string `yaml:"return_url,omitempty"`
}

// StripeConfig configures the Stripe Payment Element top-up surface. Secret
// fields accept the @path/to/file form so they stay out of the YAML committed
// to git. Stripe charges the wallet top-up amount 1:1 in Currency (USD by
// default) — no exchange-rate coupling.
type StripeConfig struct {
	Enabled bool `yaml:"enabled"`

	// SecretKey is the sk_(test|live)_… key — backend only. @path supported.
	SecretKey string `yaml:"secret_key"`
	// PublishableKey is the pk_(test|live)_… key, handed to the browser to
	// mount the Payment Element. Public by design, but kept in config so test
	// and live keys move together.
	PublishableKey string `yaml:"publishable_key"`
	// WebhookSecret is the whsec_… signing secret for the
	// /billing/stripe/webhook endpoint. @path supported. Optional: the poll
	// path (orderStatus retrieves the PaymentIntent live) credits orders even
	// without a delivered webhook, so this can be omitted in local dev.
	WebhookSecret string `yaml:"webhook_secret"`

	// Currency is the ISO presentment currency. Default "usd" — credits the
	// USD wallet 1:1. Lowercase.
	Currency string `yaml:"currency,omitempty"`

	// PaymentMethodConfiguration optionally pins a specific
	// pmc_… payment-method configuration (the set of enabled rails: card,
	// alipay, wechat_pay, crypto). Empty = use the account's default
	// dashboard configuration via automatic_payment_methods.
	PaymentMethodConfiguration string `yaml:"payment_method_configuration,omitempty"`

	// PaymentMethodTypes pins the exact rails offered at checkout, e.g.
	// ["alipay", "card"]. Empty = dynamic payment methods, where Stripe.js
	// picks which of the account's enabled rails to render per buyer from IP
	// and browser signals — that filter is what silently drops the Alipay tab
	// for a buyer whose network doesn't geolocate to China, and there is no
	// API parameter to override it. Listing the rails explicitly turns the
	// filter off: every buyer gets exactly these, in this order (which is also
	// the Payment Element's tab order, so the first entry is preselected).
	// Mutually exclusive with PaymentMethodConfiguration — this wins.
	PaymentMethodTypes []string `yaml:"payment_method_types,omitempty"`

	// ReturnURL is where redirect-based methods (Alipay/WeChat/crypto) send
	// the browser back after authorization. Empty = SiteURL + "/app/billing".
	ReturnURL string `yaml:"return_url,omitempty"`
}

type AlipayConfig struct {
	AppID           string `yaml:"app_id"`
	PrivateKey      string `yaml:"private_key"`       // PEM (multiline) or @path/to/file
	AlipayPublicKey string `yaml:"alipay_public_key"` // PEM or @path
	IsProduction    bool   `yaml:"is_production"`
	NotifyURL       string `yaml:"notify_url"` // public webhook URL
	ReturnURL       string `yaml:"return_url"` // browser redirect after pay
}

// ApplyDefaults fills in zero-value fields with sensible defaults. configDir
// is the directory containing config.yaml; it is used to resolve relative
// paths.
func (c *Config) ApplyDefaults(configDir string) {
	if c.DBPath == "" {
		c.DBPath = filepath.Join(configDir, "saas.db")
	} else if !filepath.IsAbs(c.DBPath) {
		c.DBPath = filepath.Join(configDir, c.DBPath)
	}
	if c.LocalSnapshotDays <= 0 {
		c.LocalSnapshotDays = 14
	}
	if c.JWTTTL == 0 {
		c.JWTTTL = 24 * time.Hour
	}
	if c.JWTRefreshTTL == 0 {
		c.JWTRefreshTTL = 14 * 24 * time.Hour
	}
	if c.SiteName == "" {
		c.SiteName = "HypiToken"
	}
	if c.FreeRegister == nil {
		t := true
		c.FreeRegister = &t
	}
	if c.SignupBonusUSD == nil {
		// Suspended programme: no welcome credit unless an operator opts back in.
		v := 0.0
		c.SignupBonusUSD = &v
	}
	if c.ReferralsEnabled == nil {
		f := false
		c.ReferralsEnabled = &f
	}
	if c.FallbackCNYPerUSD <= 0 {
		c.FallbackCNYPerUSD = 7.2
	}
	if strings.TrimSpace(c.Stripe.Currency) == "" {
		c.Stripe.Currency = "usd"
	} else {
		c.Stripe.Currency = strings.ToLower(strings.TrimSpace(c.Stripe.Currency))
	}
	if c.HealthCheckInterval == 0 {
		c.HealthCheckInterval = 10 * time.Minute
	}
	if c.SMTP.Port == 0 {
		c.SMTP.Port = 587
	}
}

// EnsureJWTSecret loads or generates the HS256 signing key. Generated keys
// are persisted next to the SQLite DB so restarts don't invalidate sessions.
func (c *Config) EnsureJWTSecret() error {
	if strings.TrimSpace(c.JWTSecret) != "" {
		return nil
	}
	keyPath := c.DBPath + ".jwt_secret"
	data, err := os.ReadFile(keyPath)
	if err == nil {
		c.JWTSecret = strings.TrimSpace(string(data))
		if c.JWTSecret != "" {
			return nil
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	c.JWTSecret = hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(keyPath, []byte(c.JWTSecret), 0o600)
}
