# Freebuff2API

[English](README.md) | [简体中文](README_zh.md)

Freebuff2API is an OpenAI-compatible proxy server for [Freebuff](https://freebuff.com). It translates standard OpenAI API requests into Freebuff's backend format, allowing you to use Freebuff's free models with any OpenAI-compatible client, SDK, or CLI tool.

## Features

- **OpenAI Compatible API** — Standard `/v1/chat/completions`, `/v1/models`, and `/v1/responses` (Responses API) endpoints.
- **Claude / Anthropic Compatible API** — Full `/v1/messages` translation including streaming (`message_start` → `content_block_delta` → `message_stop`), thinking blocks, tool use, images, and tiktoken-accurate `/v1/messages/count_tokens`.
- **Protocol Compatibility Layer** — Applies the Buffy system-prompt requirement, toolset signature (camouflaged tool names + `decide` bookkeeping tool), harness-phrase rewrites, forced upstream streaming with response reassembly, and the `cb_easp` stop sentinel the free tier expects.
- **Resilient Session Handling** — Free-session lifecycle with waiting-room polling, recovery from `session_superseded` / `session_model_mismatch` / `model_locked` / blank streams, 429-aware cooldowns parsed from `retryAfterMs`, and a per-account circuit breaker.
- **Stable Per-Account Identity** — Each token keeps a stable `client_id` (matching the official SDK format) instead of per-request randomness.
- **Per-Account Serialization** — Upstream chat calls on one account are serialized with a minimum gap, matching the free tier's single-request behavior.
- **Multi-Token Rotation** — Cycle through multiple auth tokens with automatic periodic run rotation and draining.
- **Live Model Catalog** — Parses the upstream freebuff model constants (`free-agents.ts`, `freebuff-models.ts`, `freebuff-model-ids.ts`) with a jsDelivr mirror fallback, 6-hour refresh, per-model reasoning-effort ladders, and a built-in snapshot fallback.
- **Health Endpoint** — `/healthz` reports per-token session state, queue position, quota tier and remaining rate limits, health classification, cooldowns, and model-registry status.
- **HTTP Proxy Support** — Route all outbound traffic through a configurable upstream proxy.
- **Token Onboarding CLI** — `cmd/freebuff-token` performs the device-code login and writes the token into `config.json`.

## Getting Auth Tokens

Freebuff2API requires one or more Freebuff **auth tokens**. There are two ways to obtain one:

### Method 1 — Web (Recommended)

Visit **[https://freebuff.llm.pm](https://freebuff.llm.pm)**, log in with your Freebuff account, and your auth token will be displayed directly on the page. Copy it as your **AUTH_TOKENS** — no local installation required.

### Method 2 — Freebuff CLI

Install the Freebuff CLI:

```bash
npm i -g freebuff
```

Run `freebuff` in your terminal — on first launch it will guide you through login.

After logging in, your token is saved to a local credentials file:

| OS | Credentials Path |
|---|---|
| Windows | `C:\Users\<username>\.config\manicode\credentials.json` |
| Linux / macOS | `~/.config/manicode/credentials.json` |

The file looks like:

```json
{
  "default": {
    "id": "user_10293847",
    "name": "Zhang San",
    "email": "zhangsan@example.com",
    "authToken": "fa82b5c1-e39d-4c7a-961f-d2b3c4e5f6a7",
    ...
  }
}
```

Only the `authToken` value is needed — copy it as your **AUTH_TOKENS**.

### Method 3 — Device Login CLI

Use the bundled CLI to log in interactively and store the token directly in your config:

```bash
go run ./cmd/freebuff-token --write-config
```

It prints a login URL, polls until you authorize, and appends the resulting token to `AUTH_TOKENS` in `config.json`.

> **Tip:** Log in with multiple accounts and configure all their tokens for higher throughput.

## Configuration

Configuration is managed via a JSON file and/or environment variables. The JSON keys and environment variable names are identical. By default the app looks for `config.json` in the working directory; use `-config` to specify another path.

```json
{
  "LISTEN_ADDR": ":8080",
  "UPSTREAM_BASE_URL": "https://www.codebuff.com",
  "AUTH_TOKENS": ["eyJhb..."],
  "ROTATION_INTERVAL": "6h",
  "REQUEST_TIMEOUT": "15m",
  "API_KEYS": [],
  "HTTP_PROXY": "",
  "MAX_REQUEST_BODY_MB": 32,
  "FORCE_UPSTREAM_STREAM": true,
  "TOOL_CAMOUFLAGE": true,
  "HARNESS_REWRITES": true,
  "BUFFY_GUARD": true,
  "UPSTREAM_MIN_GAP": "300ms"
}
```

### Reference

| Key / Env Var | Description |
|---|---|
| `LISTEN_ADDR` | Proxy listen address (default `:8080`) |
| `UPSTREAM_BASE_URL` | Freebuff backend URL (default `https://www.codebuff.com`) |
| `AUTH_TOKENS` | Freebuff auth tokens (JSON array or comma-separated env var) |
| `ROTATION_INTERVAL` | Run rotation interval (default `6h`) |
| `REQUEST_TIMEOUT` | Upstream request timeout (default `15m`) |
| `API_KEYS` | Client API keys for proxy auth (empty = open access) |
| `HTTP_PROXY` | HTTP proxy for outbound requests |
| `MAX_REQUEST_BODY_MB` | Client request body cap in MB (default 32) |
| `FORCE_UPSTREAM_STREAM` | Always stream upstream and reassemble for non-stream clients (default `true`) |
| `TOOL_CAMOUFLAGE` | Prefix tool names (`mcp__`) and append the `decide` signature tool on the wire (default `true`) |
| `HARNESS_REWRITES` | Rewrite coding-harness markers in system prompts (default `true`) |
| `BUFFY_GUARD` | Ensure the system prompt opens with the Buffy line the free tier requires (default `true`) |
| `UPSTREAM_MIN_GAP` | Minimum gap between upstream calls per account (default `300ms`) |

Environment variables override JSON values when both are set.

## Deployment

### Docker

Pre-built multi-arch images are available on GHCR:

```bash
docker run -d --name Freebuff2API \
  -p 8080:8080 \
  -e AUTH_TOKENS="token1,token2" \
  ghcr.io/quorinex/freebuff2api:latest
```

Build from source:

```bash
docker build -t Freebuff2API .
docker run -d -p 8080:8080 -e AUTH_TOKENS="token1,token2" Freebuff2API
```

### Build from Source

**Requirements:** Go 1.23+

```bash
git clone https://github.com/Quorinex/Freebuff2API.git
cd Freebuff2API
go build -o Freebuff2API .
./Freebuff2API -config config.json
```

## Links

- [linux.do](https://linux.do)

## Disclaimer

This project has no official affiliation with OpenAI, Codebuff, or Freebuff. All related trademarks and copyrights belong to their respective owners.

All contents within this repository are provided solely for communication, experimentation, and learning, and do not constitute production-ready services or professional advice. This project is provided on an "As-Is" basis, and users must use it at their own risk. The author assumes no liability for any direct or indirect damages resulting from the use, modification, or distribution of this project, nor provides any warranties of any kind, express or implied.

## License

MIT
