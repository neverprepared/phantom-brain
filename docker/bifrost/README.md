# Bifrost AI gateway

Standalone satellite stack for [Bifrost](https://docs.getbifrost.ai) — an
OpenAI-compatible gateway that fronts 1000+ models (OpenAI, Anthropic,
Bedrock, …) behind one API with load balancing, fallbacks, and guardrails.
Same per-unit deploy pattern as `docker/ollama/`; not wired into the main
phantom-brain stack.

**Ollama-first.** The shipped `config.example.json` registers a local
Ollama provider only — launch with zero cloud keys and route to models
already running on your machine. Add cloud providers later in the UI or
`config.json`.

## Quick start (Ollama)

Prereq: Ollama reachable on the host. On macOS/Windows run it **natively**
(`brew install ollama && ollama serve`) — Docker can't reach Metal — or use
the `docker/ollama/` stack. Pull a model: `ollama pull llama3.2`.

```sh
cd docker/bifrost
mkdir -p data
cp config.example.json data/config.json    # Ollama-first, no keys required
docker compose up -d
docker compose logs -f bifrost
open http://localhost:8080                  # web UI + config + request logs
```

The container reaches the host's Ollama via `host.docker.internal:11434`
(set in `config.json` → `network_config.base_url`, with the matching
`extra_hosts` entry in the compose file). If Ollama listens elsewhere,
edit that `base_url`.

Smoke test the OpenAI-compatible endpoint (use a model you've pulled):

```sh
curl http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"ollama/llama3.2","messages":[{"role":"user","content":"ping"}]}'
```

## Adding cloud providers later

```sh
cp .env.example .env        # set OPENAI_API_KEY / ANTHROPIC_API_KEY
# add the matching provider block to data/config.json (or use the UI),
# referencing the key as "env.OPENAI_API_KEY"
docker compose up -d        # re-reads env
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
