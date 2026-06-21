# Multi-stage build for pbrainctl. Produces a slim distroless image
# carrying just the binary + libc (needed by mattn/go-sqlite3 + the
# embedded sqlite-vec extension).
#
# Build:
#   docker build -t pbrainctl:dev -f docker/Dockerfile .
#
# Run the daemon (mount config + data):
#   docker run --rm -p 9998:9998 \
#     -v $HOME/.config/phantom-brain-server:/config:ro \
#     -v /var/lib/phantom-brain:/data \
#     -e PHANTOM_BRAIN_CONFIG_DIR=/config \
#     -e PHANTOM_BRAIN_DATA_DIR=/data \
#     pbrainctl:dev serve
#
# v5.0 spec §4 deploy contract. The agent (pbrainctl mcp) is NOT
# meant to run from this image — it ships on developer hosts via the
# go install / GitHub release path.

# ---- builder ---------------------------------------------------------
FROM golang:1.26-alpine AS builder
RUN apk add --no-cache build-base git
WORKDIR /src

# Cache go.mod + go.sum separately so layer reuse survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=1 is required: mattn/go-sqlite3 wraps the sqlite C
# library, and internal/vec extracts a sqlite-vec loadable extension
# at runtime. -ldflags="-w -s" strips debug symbols to shave ~10MB.
ENV CGO_ENABLED=1
RUN go build -tags=sqlite_fts5 -ldflags="-w -s" \
    -o /out/pbrainctl ./cmd/pbrainctl

# ---- runtime ---------------------------------------------------------
# distroless/cc-debian12 carries libc + libgcc, which mattn needs.
# Switching to distroless/static (~10 MB image) is a Phase 5 option
# once internal/vec is reworked to use modernc.org/sqlite.
FROM gcr.io/distroless/cc-debian12:nonroot

COPY --from=builder /out/pbrainctl /usr/local/bin/pbrainctl

ENV PHANTOM_BRAIN_CONFIG_DIR=/config \
    PHANTOM_BRAIN_DATA_DIR=/data

EXPOSE 9998
VOLUME ["/config", "/data"]

ENTRYPOINT ["/usr/local/bin/pbrainctl"]
CMD ["serve"]
