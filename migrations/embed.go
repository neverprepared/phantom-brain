// Package migrations embeds the SoR schema migrations as an io/fs for
// golang-migrate (iofs source). The SQL files live alongside this file at
// the repo-root migrations/ dir; go:embed can only reach files in or below
// its own directory, so the embed must be declared from a package here.
package migrations

import "embed"

//go:embed *.up.sql *.down.sql
var FS embed.FS
