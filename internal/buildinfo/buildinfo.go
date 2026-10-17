// Package buildinfo reports the version identity of a running GitNest binary.
//
// The version, commit, and build time are set at build time with
// -ldflags "-X <path>.Version=... -X <path>.Commit=... -X <path>.BuildTime=...".
// A binary built without them reports the development defaults rather than
// pretending to be a release.
package buildinfo

import (
	"fmt"
	"runtime"
)

var (
	// Version is the release version, or "dev" for a build without one.
	Version = "dev"
	// Commit is the source revision the binary was built from.
	Commit = "unknown"
	// BuildTime is the build timestamp, normally taken from SOURCE_DATE_EPOCH.
	BuildTime = "unknown"
)

// Info describes the running binary.
type Info struct {
	Version   string
	Commit    string
	BuildTime string
	GoVersion string
	Platform  string
}

// Current returns the identity of the running binary.
func Current() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String renders the identity as one line, for `gitnest version` and the
// startup log line.
func (i Info) String() string {
	return fmt.Sprintf("gitnest %s (commit %s, built %s, %s, %s)",
		i.Version, i.Commit, i.BuildTime, i.GoVersion, i.Platform)
}
