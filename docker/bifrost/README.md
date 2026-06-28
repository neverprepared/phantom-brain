# Bifrost AI gateway

Standalone satellite stack for [Bifrost](https://docs.getbifrost.ai) — an
OpenAI-compatible gateway that fronts 1000+ models (OpenAI, Anthropic,
Bedrock, …) behind one API with load balancing, fallbacks, and guardrails.
Same per-unit deploy pattern as `docker/ollama/`; not wired into the main
phantom-brain stack.

## Quick start

```sh
cd docker/bifrost
mkdir -p data
cp config.example.json data/config.json   # optional — or configure via the UI
cp .env.example .env                       # set provider API keys
docker compose up -d
docker compose logs -f bifrost
open http://localhost:8080                  # web UI + config + request logs
```

Smoke test the OpenAI-compatible endpoint:

```sh
curl http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}'
```

## How config works

The mounted `./data` dir **is** Bifrost's app-dir (`/app/data`):

| File | What |
|---|---|
| `config.json` | Optional file-backed config (providers, keys, routing). |
| `config.db`   | SQLite store for edits made through the UI/API. |
| `logs.db`     | Request log store. |

Precedence: Bifrost reconciles `config.json` against the store by a
content hash per entity. Edit an entity in `config.json` and the file
wins; entities added only via the UI are preserved. Zero-config also
works — start with no `config.json` and add providers in the UI.

Keys are referenced from `config.json` as `env.OPENAI_API_KEY`; supply
the actual values via `.env` (passed through in `docker-compose.yml`).

## Knobs (`.env`)

| Var | Default | Purpose |
|---|---|---|
| `BIFROST_VERSION` | `latest` | Image tag — pin to a release for reproducibility. |
| `APP_PORT` | `8080` | Host port (container always listens on 8080). |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `LOG_STYLE` | `json` | `pretty` \| `json`. |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | — | Resolved by `env.*` refs in `config.json`. |

> `APP_HOST` is forced to `0.0.0.0` in the compose file — Bifrost defaults
> to `localhost`, which inside a container is unreachable from the host.

## Using it from phantom-brain

Bifrost speaks the OpenAI API, so any OpenAI-compatible client can target
`http://<host>:8080/v1`. The phantom-brain daemon's synth backend is
Ollama or the `claude` CLI today (see the root `CLAUDE.md` `[synth]`
block), not an OpenAI base-URL — so this stack stands alone unless/until
an OpenAI-compatible synth backend is added.
