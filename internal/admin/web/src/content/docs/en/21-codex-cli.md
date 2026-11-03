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

## 1. Recommended: CC Switch Routing mode

Use **Routing mode** for this desktop setup. Importing the provider does **not** start routing.

1. Install the current stable CC Switch from [its website](https://ccswitch.io/). As checked on November 3, 2026, the latest release is [v4.0.25](https://github.com/farion1231/cc-switch/releases/tag/v4.0.25).
2. On [Tokens](/app/tokens), click **Use token**, confirm the primary model, and import HypiToken into CC Switch.
3. If ChatGPT sign-in is missing, switch to **OpenAI Official**, fully restart Codex and sign in, then quit Codex again.
4. Open **Codex → Routing** in CC Switch, click **Start routing**, and choose **Route here** on HypiToken. Confirm the mode is **active**: opening a tab alone does not enable it.
5. Reopen Codex Desktop. Try the existing conversation; if model/history compatibility errors occur, keep that history and start a new conversation with a supported model.
6. Keep CC Switch running. For subsequent provider changes, stay in Routing rather than repeatedly changing connection modes.

See the [official switching guide](https://github.com/farion1231/cc-switch/blob/main/docs/user-manual/en/2-providers/2.2-switch.md) and [maintainer's authentication advice](https://github.com/farion1231/cc-switch/issues/8431).

Check the imported values:

| Setting | Value |
| --- | --- |
| Client | Codex |
| Provider | HypiToken |
| Provider endpoint in CC Switch | `https://api.novadiffusion.com/v1` |
| Active Codex connection mode | Routing |
| API key | Your HypiToken token |
| API | Responses |

If nothing opens, launch CC Switch and retry, or copy the import link into CC Switch. Download the app from [https://ccswitch.io/](https://ccswitch.io/).

## 2. Native Windows configuration

Enter this path in File Explorer's address bar:

```text
%USERPROFILE%\.codex
```

The user configuration is `config.toml` in that folder. Back up existing settings before editing and merge changes without duplicate keys or tables. If you set `CODEX_HOME`, edit `config.toml` in that directory instead.

In Routing mode, a loopback address in Codex config is expected. Keep the real gateway endpoint and token in the CC Switch provider; do not replace the loopback URL manually. CC Switch manages `config.toml` and credentials. Verify the selected primary model, enable the provider, check that the endpoint ends in `/v1`, and restart the desktop app. Do not combine the import with a separate environment-variable configuration or overwrite an existing `auth.json`; preserve official sign-in if you need Work features.

The macOS config directory is `~/.codex`. Desktop users can use CC Switch; manual terminal configuration is covered in the separate [Codex CLI guide](/docs/codex-cli-terminal).

## 3. Existing conversations after switching

- **Work browser authentication error:** Direct switching may stash ChatGPT sign-in. Restore it and use Routing as above. v4.0.16 fixed losing sign-in across CC Switch restarts. Direct mode also has a “preserve official login” setting, but it is not our default setup. [Issue #8431](https://github.com/farion1231/cc-switch/issues/8431)
- **Model provider not found:** older sessions reference a removed provider definition. New versions normalize provider IDs and preserve definitions, but opening history does not guarantee continued requests through a different provider. For official/third-party transitions, the maintainer suggests “Unify Codex session history” and selecting a compatible model. Incompatible encrypted reasoning still requires a new conversation; preserve the old history. [Issue #7910](https://github.com/farion1231/cc-switch/issues/7910)
- **Aggregation to Direct produces `ccs-` or compaction errors:** old model IDs or third-party history can be incompatible with the official backend. The maintainer says Direct traffic cannot be repaired by CC Switch; start a new conversation rather than rewriting history automatically. [Issue #8655](https://github.com/farion1231/cc-switch/issues/8655)

Routing reduces configuration and login changes; it does not guarantee that arbitrary conversations work across incompatible models or providers.

### Official quota exhaustion

Some desktop versions block requests before they reach the gateway when the signed-in official account runs out of quota. This does not mean HypiToken credit is exhausted. v4.0.25 adds optional **Settings → App Configuration → Codex enhancements → Official quota protection**. It is off by default and only protects official requests through this local router. It cannot undo an existing lock or control usage on other devices. The maintainer suggests temporary third-party Direct mode without preserved official sign-in, a full restart and a new conversation when already locked; CLI is another option. Return to Routing after official quota recovers. The maintainer has not fully validated the actual-exhaustion workflow. [Issue #7962](https://github.com/farion1231/cc-switch/issues/7962)

## 4. Verify the route

During normal use, match CC Switch routing logs with HypiToken request time, model and token. Import or sign-in success alone does not confirm the inference route. Keep CC Switch running; connection failures to loopback usually need a running local router. Check the provider token for 401 and endpoint/model for 404.

## 5. Terminal users

Use the separate [Codex CLI guide](/docs/codex-cli-terminal) for CLI, WSL and automation. Desktop users do not need WSL.
