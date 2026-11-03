---
slug: codex-cli
title: Codex Desktop App setup
group: Clients
order: 20
intro: Connect Codex Desktop App on native Windows with your HypiToken token. CC Switch import is the recommended setup path.
---

## Before you start

- Install Codex Desktop App. If you already have it, continue without installing Node.js or Codex CLI.
- Create a token on the HypiToken [Tokens page](/app/tokens) and check your available credit.
- Install and launch CC Switch from its [official website](https://ccswitch.io/).

## 1. Import with CC Switch

1. Click **Use token** on the Tokens page. **Codex** opens by default.
2. Confirm or edit the **Primary model**, then click **Open in CC Switch** and confirm the HypiToken import.
3. In CC Switch's **Codex** configuration, select and enable the imported HypiToken provider.
4. Fully quit Codex Desktop App, including any tray process, and reopen it.
5. Open a project, create a new conversation, and choose a model supported by the console, such as `gpt-6-astra`.

Check the imported values:

| Setting | Value |
| --- | --- |
| Client | Codex |
| Provider | HypiToken |
| Base URL | `https://api.novadiffusion.com/v1` |
| API key | Your HypiToken token |
| API | Responses |

If nothing opens, launch CC Switch and retry, or copy the import link into CC Switch. Download the app from [https://ccswitch.io/](https://ccswitch.io/).

## 2. Native Windows configuration

Enter this path in File Explorer's address bar:

```text
%USERPROFILE%\.codex
```

The user configuration is `config.toml` in that folder. Back up existing settings before editing and merge changes without duplicate keys or tables. If you set `CODEX_HOME`, edit `config.toml` in that directory instead.

CC Switch generates and manages `config.toml` and the corresponding credentials. Verify the selected primary model, enable the provider, check that the endpoint ends in `/v1`, and restart the desktop app. Do not combine the import with a separate environment-variable configuration or overwrite an existing `auth.json`; preserve official sign-in if you need Work features.

The macOS config directory is `~/.codex`. Desktop users can use CC Switch; manual terminal configuration is covered in the separate [Codex CLI guide](/docs/codex-cli-terminal).

## 3. Verify the connection

Start a normal task in the desktop app and check its time, model and token against the HypiToken request log. A successful import alone does not confirm that the desktop app switched providers.

- **Still using the previous provider:** enable HypiToken, fully quit and reopen Codex, then start a new conversation.
- **401:** check the token and whether the app can read its credential.
- **404:** make sure the base URL ends in `/v1`.
- **Unavailable model:** check the current console model list and spelling.
- **Work browser authentication prompt:** use Codex mode for coding. Work features may require separate ChatGPT sign-in; a gateway token does not replace that sign-in.

## 4. Prefer a terminal?

The separate [Codex CLI guide](/docs/codex-cli-terminal) covers terminal installation, configuration, WSL and automation. Desktop App users can skip it.
