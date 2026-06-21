//go:build !sqlite_fts5

// This file is compiled ONLY when the sqlite_fts5 build tag is missing.
// It panics at init time with a clear message so developers running
// `go test ./...` or `go build` directly don't get the cryptic
// "no such module: fts5" error at first FTS5 query.
//
// See the project Makefile (target `test`/`build`) for the canonical
// invocation, or export `GOFLAGS=-tags=sqlite_fts5` to make plain go
// commands work.

package index

func init() {
	panic("internal/index requires the sqlite_fts5 build tag — " +
		"use `make build`/`make test`, or set GOFLAGS=-tags=sqlite_fts5")
}
