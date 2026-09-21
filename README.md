# HypiToken

> A multi-tenant LLM API gateway for **Claude** and **Codex** — one binary, real USD wallet, Alipay top-up, status-page health.

[![CI](https://github.com/hypit-ai/hypitoken/actions/workflows/ci.yml/badge.svg)](https://github.com/hypit-ai/hypitoken/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/hypit-ai/hypitoken?display_name=tag)](https://github.com/hypit-ai/hypitoken/releases)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

HypiToken is a self-hostable LLM gateway that fans out client requests across many upstream credentials (Anthropic OAuth + API keys, OpenAI / ChatGPT OAuth + API keys), bills users in real USD via an Alipay-funded wallet, and ships a complete operator console at the same URL — all in a single static Go binary.

It started life as a reverse proxy for personal credential pooling. The commercial SaaS layer adds per-user accounts, payments, pricing groups, and an operator panel — turning one binary into a self-serve API resale platform.

---

## Why

Anthropic and OpenAI subscription credentials are dramatically cheaper per token than pay-as-you-go API access — but a single account can't serve a team. HypiToken lets one operator pool credentials and resell access at a fair, transparent rate, with real-USD wallets billed against an RMB peg the operator controls.

```text
bill_usd = official_cost × (peg_rmb / live_cny) × group_multiplier
```

A `$0.10` Claude Sonnet call at the default tier (peg `¥2`, live rate `¥7.20`) bills **`$0.0278`** from the user's wallet — about a quarter of API list price.

---

## Features

### Core proxy

- **Two endpoints, one fleet** — Claude on `:8317` (`/v1/messages`), Codex on `:8318` (`/v1/chat/completions`, `/v1/responses`). Each can be enabled independently; per-provider concurrency budgets never share buckets.
- **SDK and CLI compatibility** — native Anthropic forwarding plus OpenAI Responses and Chat Completions. Codex OAuth bridges Chat Completions through the shared cc-core Responses adapter, including streaming tool calls.
- **Sticky sessions** — one client token always lands on the same upstream credential within an active window, preserving prompt cache hits and conversation continuity.
- **Smart credential rotation** — automatic retries across credentials on 429 / 5xx / quota exceeded; sticky-session break on auth errors; configurable cooldowns; daily reset job for transient API-key failures.
- **Per-credential proxies** — each OAuth file can specify its own SOCKS/HTTP proxy; uTLS Chrome fingerprint optional.
- **Request log** — append-only JSONL, daily rotated, with retention.

### Multi-tenant SaaS layer

- **Email + password auth** with SMTP-based 6-digit verification, password reset, JWT bearer for `/api/v2/*`. Open-registration toggleable per deployment.
- **USD wallet** billed on every successful response. Top up via Alipay at the live USD/CNY rate (refreshed hourly from a free public API); mock gateway in dev auto-confirms after 2s.
- **Per-user API tokens** (`sk-cpa-…`) with daily / monthly USD caps, concurrency limit, and RPM cap.
- **Pricing groups** — each group sets its own RMB↔USD peg per provider plus a small multiplier. Defaults: Codex `¥0.5 = $1`, Claude `¥2 = $1`.
- **Status page** — big-banner uptime UI, per-model probes against API-key credentials every 30 minutes.
- **Operator panel** — users, pricing groups, upstream credentials, payments, manual reconciliation.
- **Documentation site** — fumadocs-style three-column docs at `/docs`, sourced from plain Markdown files in `internal/admin/web/src/content/docs/`.

### Production hardening

> [!IMPORTANT]
> Wallet integrity is enforced at every layer. Every notification — async or operator-triggered — funnels through one validator that checks signature, `app_id`, `total_amount`, `trade_status`, order TTL, and pending state. Late or replayed notifications are silently rejected.

- Append-only `wallet_tx` ledger — balance is `SUM(amount)` over a user's transactions; immune to schema rewrites.
- Per-user create-rate limit (`10/hr`) + max-pending cap (`5`) on `/topup`. Per-email + per-IP rate limits on `/auth/send-code`.
- Background sweeper marks pending Alipay orders older than 30 min as expired.
- `alipay.trade.query` reconciliation endpoint to repair lost notifications without touching SQL.
- SQLite WAL + automatic pre-migration backups (`saas.db.backup-vN-<ts>`); refuses to start an older binary against a newer DB.

---

## Quick start

### Build

```bash
git clone https://github.com/hypit-ai/hypitoken.git
cd hypitoken
make build
# => bin/hypitoken
```

Requires Go ≥ 1.25 and [Bun](https://bun.sh) for the frontend.

### Install (binary)

```bash
curl -fsSL https://raw.githubusercontent.com/hypit-ai/hypitoken/main/install.sh | bash
```

The installer downloads the latest release for your platform, puts the binary in `/usr/local/bin`, and writes a systemd unit if available.

### Configure

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml
./bin/hypitoken -config config.yaml
```

Out of the box:

- Claude proxy on `:8317` (primary)
- Codex proxy on `:8318` (disabled — opt in)
- SaaS mode **off** — behaves exactly like the underlying OSS proxy

Enable the commercial layer:

```yaml
saas:
  enabled: true
  db_path: ./saas.db
  admin_email: you@example.com
  admin_password: change-me-now-change-me-now
  smtp:
    host: smtp.example.com
    port: 587
    username: postmaster
    password: ${SMTP_PASS}
    from: support@example.com   # use a real, replyable mailbox — Gmail downranks/filters "no-reply" senders, which buries verification codes
    use_tls: true
  alipay:
    app_id: ""        # empty = mock gateway (dev auto-confirms in 2s)
    private_key: "@/etc/cpa/alipay_private.pem"
    alipay_public_key: "@/etc/cpa/alipay_public.pem"
    is_production: true
    notify_url: https://your.host/api/v2/billing/notify
```

On first start, the admin user is bootstrapped with the configured email + password. After that, change credentials from the panel.

> [!TIP]
> Read [`config.example.yaml`](config.example.yaml) in full — every field has an inline comment explaining the default.

---

## Architecture

```
                  ┌─────────────────────────────────────┐
                  │           HypiToken binary          │
                  │                                     │
   /v1/messages   │   ┌──────────────┐                  │
   /v1/responses  │   │  Proxy core  │ ─────► OAuth + API key pool
   ─────────────► │   │  (server)    │        ↳ sticky sessions, retries
                  │   └──────┬───────┘        ↳ usage ledger
                  │          │                ↳ uTLS / per-cred proxies
                  │          ▼
   /api/v2/*      │   ┌──────────────┐
   /              │   │   SaaS       │
   ─────────────► │   │   adapter    │ ─────► users, tokens, wallet,
                  │   │              │        pricing groups, orders,
                  │   │              │        model health
                  │   └──────────────┘                  │
                  │          │                          │
                  │          ▼                          │
                  │   ┌──────────────┐                  │
                  │   │  SQLite WAL  │  saas.db         │
                  │   │  + backups   │                  │
                  │   └──────────────┘                  │
                  └─────────────────────────────────────┘
                              │
                              ▼
                Alipay  ◀ ──  ────  ──▶  Anthropic / OpenAI
                         signed                upstream
                         notify
```

| Component | Where it lives | Notes |
| --- | --- | --- |
| Proxy core | `internal/server/`, `internal/auth/`, `internal/usage/`, `internal/pricing/` | Provider-agnostic credential pool; sticky sessions; usage ledger; pricing catalog. |
| SaaS adapter | `internal/saas/adapter/` | Bridges `server.SaaSAdapter`; routes `/api/v2/*`; mounts SPA shell. |
| Auth | `internal/saas/auth/` | bcrypt + JWT(HS256); SMTP-based email codes; `(email, IP)` rate limits. |
| Billing | `internal/saas/billing/` | Wallet, pricing math, exchange-rate fetcher, Alipay gateway, expiry sweeper. |
| Operator panel | `internal/saas/admin/` | Users, pricing groups, credentials, payments, reconciliation. |
| Frontend | `internal/admin/web/` | Vite + React + Tailwind v4 + shadcn/ui. Built into `dist/` and embedded. |
| Docs source | `internal/admin/web/src/content/docs/*.md` | Plain Markdown; rendered with react-markdown + rehype-highlight. |

---

## Client setup

Once a user has a token from `/app/tokens`, point any compatible client at the proxy. Full guides live at `/docs`.

### Claude Code

```bash
export ANTHROPIC_BASE_URL="https://your.host"
export ANTHROPIC_API_KEY="sk-cpa-•••"
claude
```

### Codex CLI

```bash
export OPENAI_BASE_URL="https://your.host:8318/v1"
export OPENAI_API_KEY="sk-cpa-•••"
codex
```

### OpenAI SDK / LiteLLM

```python
client = OpenAI(api_key="sk-cpa-•••", base_url="https://your.host:8318/v1")
```

---

## Deploying to production

> [!WARNING]
> The mock Alipay gateway auto-credits orders 2 seconds after creation. **Never deploy with `alipay.app_id` empty** to a public host.

1. **Apply for an Alipay merchant account** at [open.alipay.com](https://open.alipay.com). Generate an RSA2 keypair, upload the public key in the merchant console, and download the Alipay platform public key.
2. **Configure SMTP** for email verification — SendGrid, Mailgun, Resend, Aliyun Direct Mail all work. Without SMTP, verification codes only print to the server log (`LogMailer`).
3. **Front with HTTPS** — Alipay's async notify requires a public HTTPS callback at `notify_url`. Use Cloudflare Tunnel, nginx + certbot, or Caddy.
4. **Whitelist Alipay IPs** if you firewall the notify endpoint. The list is in the merchant console under "开发设置".
5. **Test with sandbox first** — `alipay.is_production: false` plus the sandbox app credentials before going live.

The release pipeline emits cross-platform binaries via [GoReleaser](https://goreleaser.com) on every `v*` tag.

---

## Operator endpoints

Once logged in as an admin (`role=admin`):

| Path | Action |
| --- | --- |
| `/app/admin/users` | List, search, role / group / disabled toggles, manual balance adjust |
| `/app/admin/groups` | Pricing-group CRUD (peg, multiplier, credential-group filter) |
| `/app/admin/credentials` | Add API keys, list pool credentials, remove |
| `/app/admin/payments` | All Alipay orders, status, manual reconcile |

---

## Configuration reference

The full annotated config lives in [`config.example.yaml`](config.example.yaml). Highlights:

| Block | Purpose |
| --- | --- |
| `endpoints.claude` / `endpoints.codex` | Per-provider HTTP listeners. |
| `auth_dir`, `api_keys` | Upstream credential sources (OAuth files + inline API keys). |
| `pricing` | Per-1M-token rates per (provider, model). |
| `saas` | Multi-tenant layer — DB path, JWT, SMTP, Alipay, exchange rate, health-check cadence. |
| `admin_token` | Legacy operator JSON API auth (mounted at `/admin/api/*`). |

---

## Development

```bash
# backend only — works without a frontend build
go build ./...
go test ./...

# frontend dev server with hot reload (proxies /api → :8317)
make web-dev

# full release build
make build
```

Add new docs by dropping a Markdown file into `internal/admin/web/src/content/docs/`:

```markdown
---
slug: my-page
title: My new page
group: Reference
order: 70
intro: Optional one-liner shown under the title.
---

## Heading

Body in regular Markdown. Code blocks get syntax highlighting automatically.
```

`bun run build` then rebuild the Go binary; the new doc shows up in the sidebar.

---

## Database migrations

> [!NOTE]
> Wallet and order ledgers are append-only. Even when the schema changes substantially, historical balance can always be reconstructed from `SUM(wallet_tx.amount_usd)`.

Each schema delta is a new entry appended to `migrations` in `internal/saas/db/migrations.go`. The migrator:

1. Refuses to start when the binary is older than the persisted schema version.
2. Backs up the SQLite file to `saas.db.backup-vN-<timestamp>` (with `-wal` / `-shm` siblings) before applying any new migration.
3. Runs each migration in its own transaction; failure rolls back atomically.
4. Logs `applying migration vN…` and `migrated vX → vY`.

To add a new column:

```go
var migrations = []string{
    `... v1 ...`, // never modify
    `ALTER TABLE users ADD COLUMN timezone TEXT NOT NULL DEFAULT 'UTC';
     UPDATE users SET timezone = 'Asia/Shanghai';`, // backfill
}
```

---

## Credits

HypiToken is built on top of [CPA-Claude](https://github.com/wjsoj/CPA-Claude), itself a derivative of [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (MIT). The Anthropic OAuth refresh, Codex OAuth + JWT parsing, and uTLS Chrome transport were lifted from the upstream — huge thanks to its authors.

UI built with [shadcn/ui](https://ui.shadcn.com), [Tailwind v4](https://tailwindcss.com), and the [Bricolage Grotesque](https://fonts.google.com/specimen/Bricolage+Grotesque) + [JetBrains Mono](https://fonts.google.com/specimen/JetBrains+Mono) typeface pairing.

Released under the [MIT License](LICENSE).
