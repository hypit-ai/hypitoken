# API-key failure and recovery policy

Three consecutive upstream failures automatically set `disabled: true` on an
API-key credential. This includes authentication/balance failures, throttling,
5xx, transport failures and invalid or incomplete responses. A completed,
accounted-for response resets the failure run. Client cancellation and ordinary
client parameter errors do not count as upstream failures.

An explicit `model_not_found`, `model_not_available` or `model_not_supported`
response skips only that credential/model pair for ten minutes, including when
returned as HTTP 400 or 404. It does not disable a key serving other models.
The next eligible credential is tried without fetching upstream model lists.
Manual clear-quota also clears these model-specific exclusions.

The disabled flag is persisted for file-backed credentials. Neither cooldown
expiry, a late in-flight success, nor last-resort scheduling can enable it.
Operators must explicitly enable the credential in the panel or use
`PATCH /admin/api/auths/:id` with `{"disabled":false}`. That action clears old
failure and quota state. Clear-failure and clear-quota alone do not enable it.
Config-defined keys have no backing credential file; their runtime disable
cannot survive restart, so use panel/file-backed keys for durable retirement.

The legacy `explicit_failures_only` field remains readable for compatibility,
but no longer exempts repeatedly failing channels from this policy. OAuth
credentials retain their own recovery policy.

For streaming OpenAI API-key calls, one upstream attempt has a 30-second budget
before usable output. Non-streaming calls and compaction keep their existing
request timeouts. Receiving real streaming output stops this timer and marks the request
committed for the overall failover watchdog. A content-free Responses opener,
pre-output error or truncated opener does not prevent switching credentials.
Once actual content has reached the client, the turn is never replayed on
another key. Native non-streaming invalid/missing-usage responses also retry;
non-streaming clients can consume relays that return Responses SSE.

If all candidates return an HTTP error, the final withheld upstream status and
safe retry headers are surfaced through the public error formatter. Transparent
retry attempts are logged as `attempt_only` rather than extra final requests.

## Per-channel model restrictions

API-key credentials may set `allowed_models` to a list of exact client-facing
model names. Empty or absent means unrestricted. The restriction is checked
before `model_map` rewrites names, before credential selection, and again during
rewrite resolution. Last-resort cooldown selection cannot bypass it. OAuth
credentials ignore the field, and the admin PATCH endpoint rejects attempts to
set it on OAuth credentials before applying any other edits.

The API-key create/edit dialogs expose an “Allowed models” field. Enter one
name per line or separate names with commas; clearing the field restores
unrestricted routing. Changes are persisted and apply without restarting.

`openai_api_key_only_models` in `config.yaml` can pin selected models to the
API-key pool. For those models, local allowlists are authoritative: no model
catalog request is made, and no enabled accepting key yields an immediate 503
`model_temporarily_unavailable` naming the requested model. Editing an allowlist
to admit that model restores eligibility immediately. Changing the YAML list
itself requires a restart.

For these configured models only, equally prioritized, equally healthy API
keys share traffic round-robin, independently per model. Operator order,
credential groups, disable flags and health penalties still apply. A healthy
lower-priority backup is preferred over a paused primary. Other model selection
and OAuth behavior keep their existing policies.
