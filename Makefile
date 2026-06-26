# pbrainctl build / test
#
# REQUIRES the sqlite_fts5 build tag because internal/index uses FTS5
# (BM25 ranked full-text search) alongside sqlite-vec. mattn/go-sqlite3
# omits FTS5 from its default build to keep the binary small; the tag
# adds -DSQLITE_ENABLE_FTS5 to its cgo CFLAGS.
#
# All targets pre-set the tag so plain `make` invocations always
# produce a binary with both vec0 and FTS5 wired in. Developers running
# `go test ./...` or `go build` directly need to remember the tag (or
# export GOFLAGS=-tags=sqlite_fts5).

TAGS := sqlite_fts5
PKGS := ./...

VERSION  := v5.0.0-dev
COMMIT   := $(shell git rev-parse --short HEAD)
DATE     := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -X github.com/neverprepared/phantom-brain/internal/version.Version=$(VERSION) \
            -X github.com/neverprepared/phantom-brain/internal/version.Commit=$(COMMIT) \
            -X github.com/neverprepared/phantom-brain/internal/version.BuildDate=$(DATE)

.PHONY: build test test-race vet tidy clean fmt all sqlc

all: vet test build

# Regenerate the type-safe Postgres data-access layer from the migrations +
# query files into internal/pgstore/pgdb. The generated code is checked in;
# this is only needed after editing migrations or queries. NOT part of `all`.
sqlc:
	sqlc generate

build:
	go build -tags=$(TAGS) -ldflags="$(LDFLAGS)" -o pbrainctl ./cmd/pbrainctl

test:
	go test -tags=$(TAGS) -count=1 -timeout=90s $(PKGS)

test-race:
	go test -tags=$(TAGS) -race -count=1 -timeout=180s $(PKGS)

vet:
	go vet -tags=$(TAGS) $(PKGS)

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

clean:
	rm -f pbrainctl
