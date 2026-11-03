---
slug: codex-cli-terminal
title: Codex CLI setup
group: Clients
order: 21
intro: OpenAI's official Codex CLI connects to the unified gateway through /v1. The same key works for Claude Code too.
---

> This guide is for the terminal CLI, including WSL. Desktop users should follow [Codex Desktop App setup](/docs/codex-cli).

## Before you start

- Registered and created a key ([quick start](/docs/quick-start))
- Have balance or remaining trial credit ([how to top up](/docs/top-up))
- Know how to open a terminal ([never used one?](/docs/beginners))

## 1. About Codex CLI

Codex CLI is OpenAI's official terminal coding assistant — drive code, run commands, and read/write files in natural language from the shell.

| | Codex CLI | Claude Code |
| --- | --- | --- |
| Maker | OpenAI | Anthropic |
| Protocol | OpenAI | Anthropic |
| Node version | **v22+** | v18+ |
| Env vars | `OPENAI_API_KEY` + `OPENAI_BASE_URL` | `ANTHROPIC_AUTH_TOKEN` + `ANTHROPIC_BASE_URL` |

> The gateway speaks both protocols — **one account, one key** runs both Codex and Claude Code.

## 2. Endpoint

| Field | Value |
| --- | --- |
| Protocol | OpenAI Responses / Chat Completions API |
| Base URL | `https://api.novadiffusion.com/v1` (you **must** append `/v1`) |
| Env vars | `OPENAI_API_KEY` + `OPENAI_BASE_URL` |
| API key | Copy from the console's Tokens page, shared with Claude Code |

> Codex uses the OpenAI protocol, so the Base URL **must** end in `/v1` — unlike Claude Code. Forgetting it causes 404s.

## 3. Install Codex CLI

Codex CLI requires **Node.js v22+**.

<div data-tabs="install-codex">
<div data-tab="macOS">

```bash
# install Node 22 via Homebrew (or download the LTS from nodejs.org)
brew install node@22

# install Codex CLI
npm install -g @openai/codex
codex --version
```

</div>
<div data-tab="Windows">

OpenAI currently recommends running Codex CLI under WSL2 on Windows:

```powershell
# install WSL (reboots once)
wsl --install
```

After rebooting, open Ubuntu and install Node.js v22 + Codex CLI:

```bash
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt install -y nodejs
npm install -g @openai/codex
codex --version
```

> If you hit shell / PTY / permission issues in native PowerShell, switch to WSL2.

</div>
<div data-tab="Linux">

```bash
# install Node.js v22
sudo apt update
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt install -y nodejs

# install Codex CLI
npm install -g @openai/codex
codex --version
```

</div>
</div>

## 4. Point it at the gateway (recommended: config file)

Use Codex CLI's official persistent files: `~/.codex/config.toml` (model + Base URL) and `~/.codex/auth.json` (key).

<div data-tabs="config-codex">
<div data-tab="macOS">

```bash
mkdir -p ~/.codex
nano ~/.codex/config.toml
```

Add:

```toml
model_provider = "hypitoken"
model = "gpt-6-astra"
model_reasoning_effort = "high"     # high / medium / low / minimal
disable_response_storage = true     # third-party gateways cannot store responses

[model_providers.hypitoken]
name = "HypiToken"
base_url = "https://api.novadiffusion.com/v1"
wire_api = "responses"
requires_openai_auth = true
```

Then write `auth.json`:

```bash
cat > ~/.codex/auth.json <<'JSON'
{
  "OPENAI_API_KEY": "YOUR_TOKEN_HERE"
}
JSON
chmod 600 ~/.codex/auth.json
```

</div>
<div data-tab="Windows">

In WSL2 (recommended) the steps match macOS / Linux. For native PowerShell the config dir is `%USERPROFILE%\.codex`:

```powershell
New-Item -ItemType Directory -Force "$env:USERPROFILE\.codex"
notepad "$env:USERPROFILE\.codex\config.toml"
```

`config.toml`:

```toml
model_provider = "hypitoken"
model = "gpt-6-astra"
model_reasoning_effort = "high"     # high / medium / low / minimal
disable_response_storage = true     # third-party gateways cannot store responses

[model_providers.hypitoken]
name = "HypiToken"
base_url = "https://api.novadiffusion.com/v1"
wire_api = "responses"
requires_openai_auth = true
```

Then `auth.json`:

```powershell
@'
{
  "OPENAI_API_KEY": "YOUR_TOKEN_HERE"
}
'@ | Set-Content "$env:USERPROFILE\.codex\auth.json"
```

</div>
<div data-tab="Linux">

```bash
mkdir -p ~/.codex
nano ~/.codex/config.toml
```

Add:

```toml
model_provider = "hypitoken"
model = "gpt-6-astra"
model_reasoning_effort = "high"     # high / medium / low / minimal
disable_response_storage = true     # third-party gateways cannot store responses

[model_providers.hypitoken]
name = "HypiToken"
base_url = "https://api.novadiffusion.com/v1"
wire_api = "responses"
requires_openai_auth = true
```

Then write `auth.json`:

```bash
cat > ~/.codex/auth.json <<'JSON'
{
  "OPENAI_API_KEY": "YOUR_TOKEN_HERE"
}
JSON
chmod 600 ~/.codex/auth.json
```

</div>
</div>

### What the four required keys do

| Key | Why it matters |
| --- | --- |
| `model` | Default model. Without it Codex uses its own built-in default, which may not be in the gateway's available set. |
| `model_reasoning_effort` | Reasoning depth — see the table below. |
| `disable_response_storage` | Must be `true`. Third-party gateways don't offer OpenAI's response storage, and leaving it on makes requests fail. |
| `requires_openai_auth` | Must be `true`, otherwise Codex never sends the key from `auth.json` and you get a 401. |

`model_reasoning_effort` values:

| Value | When to use it |
| --- | --- |
| `ultra` | Maximum reasoning with automatic task delegation. `gpt-6-astra` and `gpt-5.6-sol` only. |
| `max` | Maximum reasoning depth, for the hardest problems. |
| `xhigh` | Extra reasoning depth above `high`. |
| `high` | Architecture work, cross-file refactors, hard bugs. Slower and pricier, highest success rate. |
| `medium` | The default for everyday coding and bug fixing. |
| `low` | Small edits, formatting, comments — when you want the answer fast. |
| `minimal` | Barely reasons at all; cheapest for mechanical transforms (translating, renaming, filling templates). |

> `xhigh`, `max` and `ultra` are only accepted by models that advertise them —
> `gpt-6-astra` supports `low` through `ultra`, while the older tiers stop at
> `high`. A model rejects an effort it does not support.

> Higher effort means more thinking tokens, which cost more. When in doubt start at `medium`.

### Temporary debugging (env vars)

<div data-tabs="env-codex">
<div data-tab="macOS">

```bash
export OPENAI_BASE_URL="https://api.novadiffusion.com/v1"
export OPENAI_API_KEY="YOUR_TOKEN_HERE"
```

</div>
<div data-tab="Windows">

In WSL2:

```bash
export OPENAI_BASE_URL="https://api.novadiffusion.com/v1"
export OPENAI_API_KEY="YOUR_TOKEN_HERE"
```

Native PowerShell:

```powershell
$env:OPENAI_BASE_URL = "https://api.novadiffusion.com/v1"
$env:OPENAI_API_KEY = "YOUR_TOKEN_HERE"
```

</div>
<div data-tab="Linux">

```bash
export OPENAI_BASE_URL="https://api.novadiffusion.com/v1"
export OPENAI_API_KEY="YOUR_TOKEN_HERE"
```

</div>
</div>

## 5. Verify and run

**Check 1: is Node v22+?** `node -v` should print `v22` or higher. Codex needs a recent Node; v18 / v20 won't run it. Use `nvm install 22 && nvm use 22` to switch.

**Check 2: is Codex installed?** `codex --version` should print a version.

**Check 3: launch Codex.** Open a new terminal:

```bash
codex
```

In the interactive UI, test with:

```text
write a Python script that lists every filename in the current directory
```

## 6. Available models

OpenAI-side model IDs on offer:

```
gpt-6-astra
gpt-5.6-sol  gpt-5.6-terra  gpt-5.6-luna
gpt-5.5  gpt-5.4  gpt-5.4-mini
```

`gpt-6-astra` is the flagship and the model to reach for on complex, demanding
work. `gpt-5.6-sol` remains the value pick — half astra's rate and still a
frontier model. `gpt-5.6-terra` balances capability against cost, and
`gpt-5.6-luna` targets cost-sensitive, high-volume workloads.

> `gpt-6-astra` needs Codex CLI **0.153.0 or newer**. Older clients are not
> offered it by the upstream plan and will fall back to their own default.

`GET /v1/models` returns the **live set** actually available (it moves with the upstream plan); the console's model list is authoritative.

```bash
codex --model gpt-6-astra "optimise this code"
```

> Model names may carry a suffix and still bill correctly, e.g. `gpt-6-astra(high)`.

## 7. Using it headlessly

`codex exec` is the non-interactive mode: it takes one task description, does the work, and exits without opening the TUI. That is what you want in scripts, CI, cron jobs, or a higher-level agent.

```bash
# one-shot task, exits when done
codex exec "update the install steps in README to Node 22"

# pick a model
codex exec --model gpt-6-astra "add JSDoc to every exported function"
```

Config still comes from `~/.codex/config.toml` + `~/.codex/auth.json`; in a CI box that has neither, just set `OPENAI_BASE_URL` and `OPENAI_API_KEY` as environment variables.

## 8. Troubleshooting

**❌ 401 Unauthorized / Invalid API Key** — check the key is complete (with `sk-cpa-` prefix), has no spaces/newlines, and that you have balance.

**❌ 404 Not Found / model does not exist** — the Base URL is missing `/v1`, or the model name is wrong (per console).

**❌ `codex: command not found`** — the npm global bin isn't on PATH:

```bash
echo 'export PATH="$(npm config get prefix)/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

**❌ Node.js version too low** — Codex needs v22+. Upgrade with `nvm install 22 && nvm use 22`.

**❌ 503 Service Unavailable** — the upstream pool temporarily has no usable credential. The body names the actual reason and the `Retry-After` header gives a suggested wait (capped at 300 seconds). Wait and retry, or check the [status page](/status).

**❌ 402 Payment Required** — out of balance. [Top up](/docs/top-up) in the console.

Quick reference:

| Code | What it actually means |
| --- | --- |
| `401` | Key missing / mistyped / has stray whitespace; or `requires_openai_auth` isn't `true` |
| `402` | Out of balance |
| `404` | `base_url` is missing `/v1`, or the model name doesn't exist |
| `503` | Upstream pool temporarily has no credential; `Retry-After` is at most 300 seconds |

### Work browser authentication errors (desktop app)

For `Work browser access requires an authentication method`, start here. Public reports use the similar wording `Work browser access requires an authenticated account`; we have not confirmed that these are the same error on the customer's device.

**Likely cause:** Work browser initialization can require a ChatGPT identity even when a gateway key provides model access. Submission may fail before inference. See the [upstream BYOK / Work report](https://github.com/openai/codex/issues/54841). The message alone does not establish insufficient credit, an invalid key, or HTTP 429.

#### Coding only: switch to Codex mode

Select **Codex** instead of **Work / ChatGPT Work**, then try a new chat. If unavailable, quit the app and back up the configuration used by that client before editing it.

| OS / runtime | Default configuration file | How to open it |
| --- | --- | --- |
| macOS | `/Users/your-name/.codex/config.toml`, or `~/.codex/config.toml` | In Finder, press `Command + Shift + G`, enter `~/.codex`, then open `config.toml` |
| Linux | Usually `/home/your-name/.codex/config.toml`, or `~/.codex/config.toml` | Run `nano ~/.codex/config.toml` in a terminal |
| Native Windows desktop app / PowerShell | Usually `C:\Users\your-name\.codex\config.toml` | Enter `%USERPROFILE%\.codex` in File Explorer, or run `notepad "$env:USERPROFILE\.codex\config.toml"` in PowerShell |
| Codex CLI inside WSL | `~/.codex/config.toml` inside WSL, usually `/home/your-wsl-name/.codex/config.toml` | Run `nano ~/.codex/config.toml` inside the relevant WSL distribution |

`~` means the home directory of the user running Codex. Native Windows and WSL use separate configurations by default. For a Windows desktop error, check the Windows file first; editing the WSL file usually does not affect the Windows app. The WSL row distinguishes environments; CLI itself does not use desktop Work mode.

If Codex has a custom `CODEX_HOME`, use **`config.toml` inside that directory** instead. Check the current terminal with `printenv CODEX_HOME` on macOS / Linux / WSL or `$env:CODEX_HOME` in PowerShell. A desktop app may inherit a different environment, so use its actual setting. Do not overwrite other configuration merely because the default file is absent.

Edit `conversationDetailMode` in the existing `[desktop]` table; add the table only if it is absent:

```toml
[desktop]
conversationDetailMode = "STEPS_COMMANDS"
```

Do not duplicate the table or replace the whole file. Keep your HypiToken configuration and credentials. Restart and test a new chat. This reported workaround leaves Work mode; it does not grant Work access and may vary by version. **Codex CLI** is an alternative. Do not delete `auth.json` or top up to fix this prompt.

#### Need Work: retain your ChatGPT sign-in

Sign in to your own ChatGPT account and check feature availability. [Official authentication documentation](https://learn.chatgpt.com/docs/auth) distinguishes API-key access from account-dependent features.

With CC Switch, sign in through **OpenAI Official**, quit the desktop app, select **routing mode** with HypiToken, and reopen. For direct switching, check **Keep official login for direct switches**. See the [maintainer's instructions](https://github.com/farion1231/cc-switch/issues/8431).

[CC Switch v4.0.16](https://github.com/farion1231/cc-switch/releases/tag/v4.0.16) fixes lost logins after routing restarts. Upgrade affected older versions; this does not remove Work's identity requirement.

**Verify routing:** send a short test and compare CC Switch logs with HypiToken request records to confirm provider, model, and billing destination. Successful sign-in alone does not prove gateway routing. Investigate subsequent HTTP errors separately.

If unresolved, share app/OS versions, mode, ChatGPT sign-in status, CC Switch version/routing mode, error time, and redacted logs. Never share keys, login tokens, or complete `auth.json` files. Sources checked 2026-10-21; not reproduced on the customer's device.

## 9. Direct API

The endpoint supports both `/v1/chat/completions` and OpenAI's `/v1/responses`. Send the OpenAI request shape and get the OpenAI response shape — existing tooling works unchanged.

> The Codex side has **no User-Agent restriction** — curl, the official openai SDK, LiteLLM and friends can all call it directly. Use these two endpoints for scripting and automation rather than the Claude-side `/v1/messages`, which does filter clients (see [Claude Code setup](/docs/claude-code)).
