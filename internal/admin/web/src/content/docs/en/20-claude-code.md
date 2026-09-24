---
slug: claude-code
title: Claude Code setup
group: Clients
order: 21
intro: Point the official Anthropic Claude Code CLI at the gateway in 30 seconds. Advisor, extended thinking, sub-agents, and MCP keep their native behavior.
---

## Before you start

- Registered a HypiToken account ([how to register](/docs/register))
- Created an API key and **copied the full `sk-cpa-...` to your clipboard** ([how to create](/docs/create-key))
- Have balance or remaining trial credit ([how to top up](/docs/top-up))
- Know how to open a terminal ([never used one?](/docs/beginners))

## 1. Endpoint

| Field | Value |
| --- | --- |
| Protocol | Anthropic Messages API |
| Base URL | `https://api.novadiffusion.com` (do **not** append `/v1`) |
| Env vars | `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN` |
| API key | Copy from the console's Tokens page, format `sk-cpa-...` |

> Do **not** add `/v1`. The Anthropic protocol differs from OpenAI's; adding it causes 404s.

## 2. Install Claude Code

Pick your system with the **System** switch (top right); the commands below follow it.

<div data-tabs="install-cc">
<div data-tab="macOS">

```bash
# official native installer (recommended)
curl -fsSL https://claude.ai/install.sh | bash

# or Homebrew
brew install --cask claude-code

# verify
claude --version
```

</div>
<div data-tab="Windows">

Claude Code supports native Windows. Install [Git for Windows](https://git-scm.com/downloads/win) first, then install from PowerShell:

```powershell
# official native installer (recommended)
irm https://claude.ai/install.ps1 | iex

# or WinGet
winget install Anthropic.ClaudeCode

# verify
claude --version
```

> WSL2 is no longer required just to run Claude Code — native PowerShell is enough. Use WSL2 only if your own toolchain needs a Linux environment.

</div>
<div data-tab="Linux">

```bash
# official native installer (recommended)
curl -fsSL https://claude.ai/install.sh | bash

# verify
claude --version
```

</div>
</div>

## 3. Point it at the gateway (recommended: config file)

Put the gateway URL and key in Claude Code's official `settings.json`. Variables under `env` apply to every session.

<div data-tabs="config-cc">
<div data-tab="macOS">

```bash
mkdir -p ~/.claude
nano ~/.claude/settings.json
```

Add (replace `YOUR_TOKEN_HERE` with your real key):

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://api.novadiffusion.com",
    "ANTHROPIC_AUTH_TOKEN": "YOUR_TOKEN_HERE"
  }
}
```

</div>
<div data-tab="Windows">

```powershell
New-Item -ItemType Directory -Force "$env:USERPROFILE\.claude"
notepad "$env:USERPROFILE\.claude\settings.json"
```

Add:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://api.novadiffusion.com",
    "ANTHROPIC_AUTH_TOKEN": "YOUR_TOKEN_HERE"
  }
}
```

Open a **new** PowerShell session before running `claude`.

</div>
<div data-tab="Linux">

```bash
mkdir -p ~/.claude
nano ~/.claude/settings.json
```

Add:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://api.novadiffusion.com",
    "ANTHROPIC_AUTH_TOKEN": "YOUR_TOKEN_HERE"
  }
}
```

</div>
</div>

> Prefer `ANTHROPIC_AUTH_TOKEN` over `ANTHROPIC_API_KEY`: it sends the same `Authorization: Bearer <token>` the gateway expects, but skips Claude Code's validation path for official `sk-ant-` key formats — our keys start with `sk-cpa-`.

### Temporary debugging (env vars)

For a one-off test, set environment variables in the current shell:

<div data-tabs="env-cc">
<div data-tab="macOS">

```bash
export ANTHROPIC_BASE_URL="https://api.novadiffusion.com"
export ANTHROPIC_AUTH_TOKEN="YOUR_TOKEN_HERE"
```

</div>
<div data-tab="Windows">

```powershell
$env:ANTHROPIC_BASE_URL = "https://api.novadiffusion.com"
$env:ANTHROPIC_AUTH_TOKEN = "YOUR_TOKEN_HERE"
```

</div>
<div data-tab="Linux">

```bash
export ANTHROPIC_BASE_URL="https://api.novadiffusion.com"
export ANTHROPIC_AUTH_TOKEN="YOUR_TOKEN_HERE"
```

</div>
</div>

> Temporary variables only live in the current window. Use the `settings.json` method above for the long term.

## 4. Verify

Before running `claude`, check these three points.

**Check 1: is Claude Code installed?** Run `claude --version` — it should print a version. `command not found` → reopen the terminal; on Windows make sure "Add to PATH" was checked during install.

**Check 2: is the config in effect?** Confirm `~/.claude/settings.json` (Windows: `%USERPROFILE%\.claude\settings.json`) has `ANTHROPIC_BASE_URL` spelled correctly as `https://api.novadiffusion.com` (no `/v1`).

**Check 3: launch Claude Code.** Open a new terminal in any project directory:

```bash
claude
```

In the interactive UI, test with:

```text
write me a hello world Python script
```

A streaming reply means you're connected 🎉.

## 5. Run it

```bash
# interactive session
claude

# one-shot prompt
claude "summarise this diff"

# advisor mode (Claude Code ≥ 2.1)
claude --advisor "review my architecture"
```

### Using it headlessly

`claude -p` (`--print`) is the non-interactive mode: it prints the result to stdout and exits without opening the TUI — what you want in scripts, CI, cron jobs, or a higher-level agent.

```bash
# one-shot task, result written to a file
claude -p "summarise the changes in git diff HEAD~1 as markdown" > summary.md

# read from a pipe, pick a model, emit JSON for programmatic parsing
git diff | claude -p --model claude-sonnet-4-6 --output-format json "review this diff"
```

`ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` are read from `settings.json` or the environment as usual — in CI just set the env vars. Note that `-p` is still Claude Code itself making the request, so it is not affected by the client filter described in section 7.

## 6. Supported features

| Feature | Status |
| --- | --- |
| Advisor mode | ✓ |
| Extended thinking | ✓ |
| Sub-agents | ✓ |
| MCP tool use | ✓ |
| Prompt caching | ✓ |
| Streaming responses | ✓ |

### Available models

Anthropic model IDs that currently have pricing (the console is authoritative):

```
claude-haiku-4-5-20251001   claude-haiku-4-5
claude-sonnet-4-6   claude-sonnet-5
claude-opus-4-6   claude-opus-4-7   claude-opus-4-8   claude-opus-5
claude-opus-5-5
claude-fable-5
```

Opus 5.5 (`claude-opus-5-5`) standard prices per million tokens: input $4, output $20, cache read $0.20, 5-minute cache write $5, and 1-hour cache write $8. The full 1M context uses these rates, before your group multiplier. [Official API pricing](https://platform.claude.com/docs/en/about-claude/pricing).

Rule of thumb: `claude-haiku-4-5` is fastest and cheapest, `claude-sonnet-4-6` / `claude-sonnet-5` are the balanced recommendation, `claude-opus-*` are the most capable.

> The Claude side has no model allowlist — any model name is forwarded upstream, and names outside the list above bill at the default rate. Suffixed names are recognised too, e.g. `claude-opus-5-5[1m]`.
> There is **no** `/v1/models` route on the Claude side (requesting it 404s); check the console for what's available.

## 7. Troubleshooting

**❌ 401 Unauthorized** — bad key. Check it's copied in full (with the `sk-cpa-` prefix), has no stray spaces, and is still active in the console.

**❌ 403 `client_not_allowed`** — the message reads "This API endpoint only accepts supported interactive clients." `/v1/messages` inspects the User-Agent **before** authentication, so a match returns 403 regardless of your key or balance. It is a **blocklist, not an allowlist** — only these UA fragments are refused (case-insensitive substring match), plus an **empty User-Agent**:

```
python-requests/  python-httpx/  python-urllib  urllib3/  aiohttp/  scrapy/
anthropic/python  anthropic/js  openai/python  openai/nodejs  openai-python/  litellm
curl/  wget/  go-http-client/  okhttp/  java/  apache-httpclient/
postmanruntime/  insomnia/  httpie/  apifox/  restsharp/
```

Real interactive clients — Claude Code CLI, Claude Code IDE / Web, Claude Desktop, Cursor — are not on the list and pass through fine.

**For scripting or any programmatic access, use the Codex-side endpoints** `/v1/chat/completions` or `/v1/responses` (`https://api.novadiffusion.com/v1`). They have no such filter, so curl and the official openai SDK work directly — see [Codex CLI setup](/docs/codex-cli). By the same token, don't try to smoke-test `/v1/messages` with curl: it will always 403.

**❌ 404 Not Found** — wrong Base URL. The correct form is `https://api.novadiffusion.com` (no trailing `/` and no `/v1`). The Claude side also has no `/v1/models` route, so requesting that 404s.

**❌ 503 Service Unavailable** — the upstream pool temporarily has no usable credential. The body names the actual reason and the `Retry-After` header gives a suggested wait (capped at 300 seconds). Wait and retry, or check the [status page](/status).

**❌ 402 Payment Required** — out of balance. [Top up](/docs/top-up) in the console.

**❌ Requests time out / are slow** — wait and retry, or check the [status page](/status). Could also be your local network (VPN, firewall).

**❌ `claude: command not found`** — the npm global bin isn't on PATH. macOS / Linux:

```bash
echo 'export PATH="$(npm config get prefix)/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

Windows: reopen PowerShell as administrator, or reinstall with "Add to PATH" checked.

## 8. How routing works

The gateway keeps a **sticky session** per client token: your Claude Code session lands on the same upstream credential within a 10-minute activity window, preserving cache hits and conversation continuity. If that credential becomes unavailable, the pool switches to the next healthy one and retries.
