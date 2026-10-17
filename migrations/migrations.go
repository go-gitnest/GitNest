// Package migrations carries the ordered SQL migrations shipped with a GitNest
// build.
//
// The files are embedded so a binary can migrate the database it was built for
// without needing the source tree. The conventions they follow, the ledger they
// are recorded in, and the marker for a migration that cannot run inside a
// transaction are documented in README.md in this directory.
package migrations

import "embed"

// Files holds every migration shipped with this build. The runner validates and
// sorts the entries, so a new migration is picked up by adding its file here.
//
//go:embed *.up.sql
var Files embed.FS
