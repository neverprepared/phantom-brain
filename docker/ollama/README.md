# Ollama embedding fleet

Per-host deploy unit for the Ollama instances that the OpenSearch-native
design (epic #92) uses for embeddings. The main stack's HAProxy
(`docker/docker-compose.yml` → `ollama-lb`) load-balances across all of
them via one VIP.

## Where to run what

| Host type | How to run Ollama | Why |
|---|---|---|
| **Linux + NVIDIA GPU** | this compose (Docker) | GPU passthrough = near-native perf + Docker ops |
| **Apple Silicon (macOS)** | **native** (`brew install ollama`, `ollama serve`) | Docker can't reach Metal → CPU-only |
| **CPU-only** | this compose (Docker) | no GPU to lose |

Mixed fleets are fine — list every instance (dockerized + native) as a
backend in the main stack's `haproxy/haproxy.cfg`.

## Linux GPU host

Prereq: [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html).

```sh
docker compose -f docker/ollama/docker-compose.yml up -d
docker compose -f docker/ollama/docker-compose.yml logs -f ollama-pull   # waits for model pull
```

Knobs (env or `.env`): `OLLAMA_VERSION`, `OLLAMA_KEEP_ALIVE` (default `-1`,
never unload), `OLLAMA_NUM_PARALLEL` (default `4`), `OLLAMA_PULL_MODEL`
(default `nomic-embed-text`).

## macOS (native)

```sh
brew install ollama
brew services start ollama          # or: ollama serve
ollama pull nomic-embed-text
# make it reachable from HAProxy on another host:
#   launchctl setenv OLLAMA_HOST 0.0.0.0   (then restart the service)
```

## Register the host with HAProxy

Add a line to `docker/haproxy/haproxy.cfg` for each instance, then reload
the `ollama-lb` service:

```
server ollama2 10.0.0.12:11434 check inter 5s fall 3 rise 2 maxconn 64
```

Watch health/balance at the stats UI: `http://<main-host>:8404/`.
