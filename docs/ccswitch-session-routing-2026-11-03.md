# CC Switch session continuity investigation — 2026-11-03

Current stable release verified via GitHub API: v4.0.25, published 2026-11-02T15:08:52Z.

- #8431 / #8604: maintainers attribute Work/resume failures to the desktop client needing ChatGPT sign-in, which Direct mode can stash. They recommend Routing with sign-in preserved; Direct also has a preserve-login setting. v4.0.16 fixed losing/restoring sign-in across CC Switch restarts. https://github.com/farion1231/cc-switch/issues/8431
- #8269: v4 normalizes third-party provider IDs to custom; the CLI still isolates history by provider. https://github.com/farion1231/cc-switch/issues/8269
- #7910: dormant provider definitions restore access to history, not necessarily the ability to send. The maintainer additionally suggests Unify Codex session history and a supported target model for official/third-party continuation; incompatible encrypted reasoning can still require a new conversation. https://github.com/farion1231/cc-switch/issues/7910#issuecomment-6217520483
- #8655: aggregation-to-Direct can leave frozen ccs model identifiers/compaction metadata or unsupported reasoning structures. Maintainer says CC Switch cannot rewrite requests that bypass it and will not automatically rewrite rollout history; a new conversation is recommended. https://github.com/farion1231/cc-switch/issues/8655#issuecomment-6312953888
- #7962: official quota exhaustion may block desktop requests before local routing. v4.0.25 adds opt-in quota protection for official traffic through this router. It cannot undo exhaustion or cover other devices. The maintainer explicitly has not verified the entire actual-exhaustion workflow, and retains temporary Direct/new-session guidance for an existing lock. https://github.com/farion1231/cc-switch/issues/7962

Product decision: recommend Routing as the standard desktop onboarding path with current stable CC Switch, retained official sign-in, explicit Start routing/Route here steps and running local app. Do not claim every Direct switch breaks history, that Routing fixes all compatibility problems, or that closed issues imply universal resolution. No server restriction is added against legitimate Direct clients. Import URL protocol remains byte-compatible with New API; no invented routing flags.

Implementation: desktop token hint and both Chinese/English Desktop and CC Switch guides updated. Release frontend-20261103.2 changes static frontend only. No inference probes, credential changes or backend deployment.
