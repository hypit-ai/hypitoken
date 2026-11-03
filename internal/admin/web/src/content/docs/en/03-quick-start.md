---
slug: quick-start
title: Quick start
group: Getting started
order: 3
intro: The shortest path from zero, each step linking to a detailed page. On your first run with HypiToken, walk through it in order.
---

## One address, two protocols

| Tool | Base URL |
| --- | --- |
| Claude Code | `https://api.novadiffusion.com` |
| Codex CLI / Cursor | `https://api.novadiffusion.com/v1` |

The same API key works across these clients. The gateway routes `/v1/chat/*`, `/v1/responses*`, and `/v1/models` to the OpenAI endpoint; everything else goes to the Anthropic endpoint.

## Four steps

### 1. Register · 1 min

Sign up with email at [api.novadiffusion.com/register](https://api.novadiffusion.com/register). → [details](/docs/register)

### 2. Create a key · 30 sec

After logging in, go to **Tokens**, click "Create", then click "Copy" to grab the key (format `sk-cpa-...`). → [details](/docs/create-key)

### 3. Top up · 1 min

New accounts get trial credit to try things out. Add balance under **Billing** before heavy use. → [details](/docs/top-up)

### 4. Connect a client · 2–3 min

Pick the client you want and follow its guide to fill in the Base URL and key:

- ⚡ [**Codex Desktop App (recommended)**](/docs/codex-cli) — native Windows desktop setup
- 🤖 [**Claude Code**](/docs/claude-code) — Anthropic's official CLI
- 🎯 [**Cursor**](/docs/cursor) — GUI editor, just fill a few settings fields

> **Want to switch providers on the fly?** [**CCSwitch**](/docs/ccswitch) is a local credential / config manager that manages the provider config for Claude Code and Codex and switches in one click — it isn't a gateway client itself; Claude Code / Codex still make the actual requests.

> **One key everywhere.** The same API key works in all these clients at once — no need to create a separate key per tool. If you want per-project usage stats, create a dedicated key per project.
