// Package webassets carries the built frontend into the binary.
//
// docs/architecture/frontend.md fixes why this exists: "The production instance
// serves pre-built assets from the Go process" and "Node.js is a build-time
// dependency only", so a deployed instance needs no Node runtime and no asset
// directory beside the binary. docs/architecture.md puts it the same way in the
// deployment baseline: "one binary, PostgreSQL, Git, and a writable data
// directory".
//
// The package lives here rather than under internal/ because an embed pattern
// cannot reach outside its own directory, and the build output is `web/dist`.
// The three lines of Go in this file are the whole of the coupling between the
// two toolchains.
package webassets

import (
	"embed"
	"io/fs"
)

// dist is the build output. `all:` includes files whose names begin with an
// underscore or a dot, which the default pattern skips; a hashed asset could
// legitimately be named either way and silently dropping one would produce a
// page that loads nothing.
//
//go:embed all:dist
var dist embed.FS

// FS returns the built assets rooted at the build output, so a caller addresses
// `index.html` and `assets/<name>-<hash>.js` rather than the directory above
// them.
//
// The directory always exists because the repository commits a placeholder in
// it: an embed pattern that matches nothing is a build failure, and `go build`
// has to work on a checkout where the frontend has not been built.
func FS() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}

// Built reports whether the frontend has been built.
//
// It is asked by the asset handler so that a checkout without a build answers
// with a message naming what to run, rather than serving an empty page that
// looks like a defect in the application.
func Built(fsys fs.FS) bool {
	_, err := fs.Stat(fsys, "index.html")
	return err == nil
}
