---
slug: ccswitch
title: CCSwitch credential manager
group: Tools
order: 23
intro: Manage HypiToken configuration for Codex Desktop App with CC Switch, with optional Claude Code setup.
---

## 1. Download

Official website: **[https://ccswitch.io/](https://ccswitch.io/)**. Download the installer for your system, install it and launch CC Switch.

This guide defaults to Codex Desktop App on native Windows. Claude Code is not required. CC Switch manages configuration; Codex Desktop App runs your tasks.

## 2. Import Codex configuration

1. Install Codex Desktop App and open CC Switch.
2. On the HypiToken [Tokens page](/app/tokens), click **Use token**. **Codex** is selected by default.
3. Click **Open in CC Switch**, confirm the import, then enable HypiToken in CC Switch's Codex configuration.
4. Fully quit Codex Desktop App, reopen it and start a new conversation.

| Setting | Value |
| --- | --- |
| Name | HypiToken |
| Base URL | `https://api.novadiffusion.com/v1` |
| API key | Your HypiToken token |
| Windows config folder | `%USERPROFILE%\.codex` |

See [Codex Desktop App setup](/docs/codex-cli) for details. Confirm routing in the HypiToken request log after switching.

## 3. Claude Code (optional)

Install it using the [Claude Code guide](/docs/claude-code), then select the Claude Code tab in Use token and import. Its base URL is `https://api.novadiffusion.com`, without `/v1`; keep it separate from Codex's URL.

## 4. Troubleshooting

- **Nothing opens:** launch CC Switch and retry, or paste the copied import link into it.
- **Configuration has no effect:** enable the correct provider, fully quit and reopen the desktop app.
- **Cannot find config:** enter `%USERPROFILE%\.codex` in File Explorer, or check your custom `CODEX_HOME` directory.
- **Work browser authentication prompt:** preserve ChatGPT sign-in and follow the [desktop guide](/docs/codex-cli).

Terminal and WSL users should use the separate [Codex CLI guide](/docs/codex-cli-terminal).
