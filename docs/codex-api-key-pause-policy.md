# Codex API-key pause policy

OpenAI API-key relay credentials may opt into `explicit_failures_only: true`
in their credential JSON or through `PATCH /auths/:id` on the admin API.
The admin summary exposes the current value. The default is false.

For opted-in relays, model-unavailable errors, generic 5xx, transport failures,
ambiguous 403 responses and missing usage do not accumulate circuit-breaker
strikes. Requests still fail or retry another credential normally. HTTP 401,
402 and 429, and recognized authentication, balance or rate-limit error bodies,
retain the existing pause behavior. Compressed HTTP errors are decoded before
classification. OAuth credentials retain their existing recovery policy.

Changing the setting persists it but does not clear an existing pause. After
checking the cause, an operator can use the separate clear-failure action.
Do not clear a genuine quota cooldown merely to enable this policy.

Retry-only HTTP and transport failures are marked `attempt_only` in request
logs, so final-request health statistics do not count transparent retries as
additional client requests. Historical rows are not rewritten.
