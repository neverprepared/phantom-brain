# Multi-stage build for pbrainctl. Ubuntu both stages so the libc
# matches across builder + runtime — the previous Alpine builder +
# distroless/cc runtime produced a musl-linked binary that couldn't
# find its glibc dynamic linker at runtime, surfacing as the
# misleading "exec: no such file or directory" from the kernel.
#
# Build:
#   docker build -t pbrainctl:dev -f docker/Dockerfile .
#
# Run the daemon (mount config + data):
#   docker run --rm -p 9998:9998 \
#     -v $HOME/.config/phantom-brain-server:/config:ro \
#     -v $HOME/.local/share/phantom-brain-server:/data \
#     -e PHANTOM_BRAIN_CONFIG_DIR=/config \
#     -e PHANTOM_BRAIN_DATA_DIR=/data \
#     pbrainctl:dev serve
#
# The agent (pbrainctl mcp) is NOT meant to run from this image —
# it ships on developer hosts via `brew install neverprepared/tap/
# pbrainctl` or the GitHub release tarball.

# ---- builder ---------------------------------------------------------
FROM ubuntu:24.04 AS builder

ARG GO_VERSION=1.26.2
ARG TARGETARCH

# Go toolchain + cgo prereqs. ca-certificates so `go mod download`
# trusts the proxy; tar/curl for the Go tarball; build-essential
# brings gcc + libc-dev which mattn/go-sqlite3 needs.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl tar build-essential git \
    && rm -rf /var/lib/apt/lists/*

# Pull the official Go tarball matching the host arch.
# golang.org publishes linux-amd64 + linux-arm64 (TARGETARCH from
# Docker buildkit gives us amd64 / arm64 / etc.).
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
        | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:$PATH

WORKDIR /src

# Layer cache: go.mod + go.sum separate from the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=1: mattn/go-sqlite3 wraps the sqlite C library +
# internal/vec extracts a sqlite-vec loadable extension at runtime.
# sqlite_fts5 tag: internal/index requires FTS5 (panics without).
# ldflags: strip debug symbols to shave ~10MB; version stamps mirror
# what the Makefile injects so `pbrainctl version` is accurate.
ENV CGO_ENABLED=1
RUN go build -tags=sqlite_fts5 -ldflags="-w -s" \
    -o /out/pbrainctl ./cmd/pbrainctl

# ---- runtime ---------------------------------------------------------
# Ubuntu minimal — same libc lineage as the builder so the binary's
# dynamic linker resolves cleanly. Image weighs ~80MB, vs ~25MB for
# distroless; trade-off for debuggability (you can `docker exec -it
# pbrainctl bash` and poke around).
FROM ubuntu:24.04

# The runtime image bundles the `claude` CLI so the SynthWorker can
# actually run gate + distill instead of falling back to raw-body.
# We install Node via NodeSource (apt's `nodejs` is too old for the
# @anthropic-ai/claude-code package) plus the CLI itself globally.
# Adds ~250 MB to the image — acceptable for the operator UX win.
#
# Auth: pass CLAUDE_CODE_OAUTH_TOKEN to the daemon via docker compose.
# Generate the token locally with `claude setup-token`. Falls back to
# raw-body if the env var is absent (gate.go's ClaudeCLIAvailable
# probe runs `which claude` at startup and gates further calls).
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg poppler-utils \
    && curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @anthropic-ai/claude-code \
    && apt-get purge -y --auto-remove gnupg \
    && rm -rf /var/lib/apt/lists/* /root/.npm \
    && groupadd -r pbrain && useradd -r -g pbrain -d /data -s /sbin/nologin pbrain \
    && claude --version \
    && pdftotext -v 2>&1 | head -1

COPY --from=builder /out/pbrainctl /usr/local/bin/pbrainctl

ENV PHANTOM_BRAIN_CONFIG_DIR=/config \
    PHANTOM_BRAIN_DATA_DIR=/data

EXPOSE 9998
VOLUME ["/config", "/data"]

# Drop privileges by default. The data + config dirs need to be
# writable by uid 999 (the pbrain user); compose bind-mounts inherit
# from the host so adjust host-side permissions if needed.
# The claude CLI writes state to $HOME/.claude — pbrain's home is
# /data, which is bind-mounted, so cache + sessions survive
# container restarts.
USER pbrain

ENTRYPOINT ["/usr/local/bin/pbrainctl"]
CMD ["serve"]
