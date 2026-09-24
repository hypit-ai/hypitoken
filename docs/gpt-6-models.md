# GPT-6 Sol and Luna support

Both gateways use cc-core for OAuth model eligibility, Codex model-picker
metadata, Responses-Lite normalization, and prices. Updating UI labels or price
cards alone does not make a model routable: the old OAuth catalog caused the
gateway's `model_not_found` response for `gpt-6-sol`.

## Model behavior

Source: sibling CLIProxyAPI at `c404af96`,
`internal/registry/models/models.json` and `codex_client_models.json`.

- The plan registry exposes Luna on free/plus/pro/team and Sol on plus/pro/team.
  Unknown plans retain the existing Pro fallback. The client metadata includes
  broader plan lists; routing follows the explicit plan registry.
- Both models use Responses-Lite: no injected image-generation built-in and
  `parallel_tool_calls=false` on normalized OAuth requests.
- Model discovery uses at least client version `0.155.0`, with matching model
  fetch Version/User-Agent values. Generation fingerprints are unchanged.
- Codex metadata: 272,000 default / 872,000 maximum context; medium default
  reasoning. Sol supports low through ultra; Luna supports low through max.
  These are private Codex capabilities, distinct from the public API model page.
- HTTP Responses, Chat Completions translation and WebSocket preparation share
  the same cc-core normalization. API-key catalogs remain upstream-controlled.

## Fixed billing

Prices are USD per million tokens, before the account/workspace multiplier:

| Model | Input | Output | Cache read | Cache write |
| --- | ---: | ---: | ---: | ---: |
| gpt-6-sol | 2.00 | 10.00 | 0.20 | 2.50 |
| gpt-6-luna | 0.10 | 0.50 | 0.01 | 0.125 |

These are the published Standard short-context cards. By operator policy,
this deployment uses them at every context length; no long-context band is
applied. Upstream API costs can therefore exceed this base for long prompts.
The existing service-tier rules remain: API-key Fast/Priority is 2x, Flex is
0.5x subject to the observed response tier; OAuth subscription cost has no
service-tier surcharge. Apply the workspace multiplier once after base pricing,
then the existing ledger quantization. Model suffixes resolve to the same card.

Sources checked 2026-09-24:
- https://developers.openai.com/api/docs/models/gpt-6-sol
- https://developers.openai.com/api/docs/models/gpt-6-luna

## Build and release

Both sibling workspaces already replace cc-core with `../cc-core` for local
integration. Published builds must release these cc-core changes and bump both
forks together; the existing v0.8.139 price-only release lacks model eligibility
and Responses-Lite support. For a local hotfix build, retain the Go version of
the deployed service and use the sibling replacement. Do not build with Go 1.27:
the existing uTLS transport has a documented compatibility problem there.

Regression coverage includes plan eligibility, old/new client manifests,
Responses-Lite normalization, both streaming modes of Chat Completions via
OAuth, fixed pricing across 272K, service tiers, custom workspace multipliers,
ledger precision and retry idempotency.
