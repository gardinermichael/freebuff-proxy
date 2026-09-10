# freebuff-proxy

Go wire gateway in front of FreeBuff, with OpenAI-compatible and Anthropic
endpoints plus an embedded Svelte dashboard.

## What it is

- Speaks OpenAI chat (`POST /v1/chat/completions`, `GET /v1/models`) and an
  Anthropic-compatible layer, then translates to the FreeBuff wire protocol.
- Runs in pooled, bridge, or hybrid mode (`EffectiveMode`):
  - **Pooled** — `AUTH_TOKENS` set + `BRIDGE_ENABLED=0`; pool only.
  - **Bridge** — `AUTH_TOKENS` empty; each request carries its own token.
  - **Hybrid** (default with `AUTH_TOKENS`) — `API_KEYS` credential uses the
    pool, any other credential relays upstream as a bridge token.
- Dashboard at `/admin` (Svelte SPA embedded in the binary).
- Freebucks metering follows the wire `prices` map: charged once per session-hour
  at session start, refunded on early `DELETE`, refilled on a Pacific-midnight
  cadence.

## Quickstart

```sh
cp .env.example .env   # then edit: AUTH_TOKENS, ADMIN_TOKEN, ...
go build ./backend/...
go run ./backend/cmd/freebuff-proxy
```

Then:

- `GET http://localhost:3457/healthz` → 200
- `GET http://localhost:3457/v1/models` → live model list
- `http://localhost:3457/admin` → dashboard

Defaults that matter (`.env.example`): `SAFE_MODE=true` (anti-ban preset),
`COST_MODE=free`, 30 req/min and 1500 req/day Pacific limits.

## Layout

- `backend/` — gateway source.
- `frontend/` — dashboard SPA source.
- `scripts/` — upstream sync / drift tooling.
- `docs/` — agent workflow notes.

## Contributing

Protected `main`: branch → PR → green CI → squash merge, Conventional Commits.
See `AGENTS.md` for the full operating guide. Never commit secrets.

## Langfuse tracing (optional)

The native Go OpenTelemetry exporter records one generation per inference
request across Chat Completions, Anthropic Messages, and Responses (streaming
and JSON). Child spans measure session acquisition and upstream attempts up to response
headers; the generation spans the complete response stream.
Responses include `X-Langfuse-Trace-Id` for correlation. Supply an optional
`X-Langfuse-Session-Id` request header to group calls into a conversation.
Requests rejected before the shared inference engine (such as invalid JSON or
API authentication failures) are not exported.

These settings are **process environment only**, read at startup. They do not
load from `.env`, appear in dashboard settings, or change on `/admin/reload`.
This keeps tracing credentials outside the dashboard's editable config store.

```sh
export LANGFUSE_ENABLED=true
export LANGFUSE_BASE_URL=https://us.cloud.langfuse.com
export LANGFUSE_PUBLIC_KEY=pk-lf-...
export LANGFUSE_SECRET_KEY=sk-lf-...
export LANGFUSE_TRACING_ENVIRONMENT=freebuff-proxy-local
./freebuff-proxy
```

Alternatively set `LANGFUSE_CREDENTIALS_FILE` to a private JSON file containing
`public_key`, `secret_key`, and `base_url`. Explicit environment values override
that file. No credential files are discovered automatically. HTTPS is required
except for local loopback collectors.

`LANGFUSE_CAPTURE_CONTENT=true` opts into sending prompt messages, tool schemas,
and response deltas to your Langfuse project. The default is false: only timing,
model, usage, and outcome metadata are exported. Request headers, tokens, and
internal upstream metadata are never included. Content capture is **not** a
secret/PII scrubber for text inside prompts or tool results; enable it only when
that content belongs in your Langfuse project. Input larger than 64 KiB is
omitted; output deltas are capped at 64 KiB, with truncation metadata. Provider
cache/reasoning usage is retained as metadata without adding it again to token
totals. Free-mode USD cost is explicitly zero; Freebucks session charges are
not converted to dollars.

Export runs in the background with a 256-span queue and bounded timeouts.
When the queue is full, telemetry is dropped instead of blocking chat. Shutdown
allows up to seven seconds to flush. Invalid tracing configuration logs a
warning and leaves the proxy available with tracing disabled. Export errors
are reported by OpenTelemetry; delivery is best-effort, not a durable audit log.
The integration uses OpenTelemetry Go 1.46.0 and Langfuse's v4 OTLP/HTTP endpoint.
