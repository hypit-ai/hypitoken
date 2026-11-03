---
slug: intro
title: Overview
group: Getting started
order: 1
intro: HypiToken is a unified API gateway that speaks both the OpenAI and Anthropic protocols. One key connects GPT, Claude and more across Codex Desktop App / CLI, Claude Code, and Cursor.
---

## What it is

HypiToken is an **OpenAI / Anthropic compatible** API relay. Point your client's Base URL at this gateway, drop in a key issued by the console, and you reach the major models directly — **no proxy or VPN required**.

Behind the scenes the gateway schedules your requests across multiple upstream subscription credentials (sticky sessions + automatic failover). The experience matches the official API, but it's steadier and usually cheaper.

## Supported clients

| Client | Type | Protocol | Base URL |
| --- | --- | --- | --- |
| Codex Desktop App / CLI | Desktop / CLI | OpenAI | `https://api.novadiffusion.com/v1` |
| Claude Code | CLI | Anthropic | `https://api.novadiffusion.com` |
| Cursor | GUI | OpenAI | `https://api.novadiffusion.com/v1` |

> **About the trailing `/v1`**
>
> The Anthropic protocol (Claude Code) does **not** take `/v1`; the OpenAI protocol (Codex Desktop App / CLI, Cursor) **must** include `/v1`. This is the single most common cause of 404s.

> **CCSwitch is not a gateway client.** It's a local credential / config manager that switches the provider config for Codex and Claude Code in one click — the actual requests are still made by Codex / Claude Code. See [CCSwitch credential manager](/docs/ccswitch).

## Which clients are not supported

The gateway only accepts **interactive AI coding clients**. Raw SDK and scripting callers are rejected at ingress with `403 client not allowed`:

- ❌ Blocked: OpenAI / Anthropic official SDKs, LiteLLM, `curl`, `python-requests`, Postman, scraping scripts
- ✅ Allowed: Codex Desktop App / CLI, Claude Code, Cursor, Claude Desktop and other normal interactive clients

In other words you **cannot** "just test the endpoint with `curl`" — it gets rejected. Connect with one of the official clients below.

## The overall flow

1. **Register** — sign up with email at [api.novadiffusion.com](https://api.novadiffusion.com/register).
2. **Create a key** — in the console's **Tokens** page, create and copy a key (format `sk-cpa-...`).
3. **Top up** — new accounts get trial credit; add balance under **Billing** before heavy use.
4. **Connect a client** — follow the matching client guide and fill in the Base URL and API key.

> New to APIs or the command line? Start with [First-timers](/docs/beginners) — 3 minutes on the vocabulary and how to open a terminal. Ready to dive in? See [Quick start](/docs/quick-start).
