package buildinfo

import (
	"strings"
	"testing"
)

func TestCurrentReportsBuildIdentity(t *testing.T) {
	info := Current()
	for name, value := range map[string]string{
		"Version":   info.Version,
		"Commit":    info.Commit,
		"BuildTime": info.BuildTime,
		"GoVersion": info.GoVersion,
		"Platform":  info.Platform,
	} {
		if value == "" {
			t.Errorf("%s is empty; a build identity field must always have a value", name)
		}
	}
	if !strings.HasPrefix(info.GoVersion, "go") {
		t.Errorf("GoVersion = %q, want a runtime version such as go1.26.0", info.GoVersion)
	}
	if !strings.Contains(info.Platform, "/") {
		t.Errorf("Platform = %q, want an os/arch pair", info.Platform)
	}
}

func TestStringIncludesVersionAndCommit(t *testing.T) {
	info := Info{Version: "1.2.3", Commit: "abc1234", BuildTime: "2026-01-01T00:00:00Z", GoVersion: "go1.26.0", Platform: "linux/amd64"}
	line := info.String()
	for _, want := range []string{"1.2.3", "abc1234", "linux/amd64"} {
		if !strings.Contains(line, want) {
			t.Errorf("String() = %q, want it to contain %q", line, want)
		}
	}
}
