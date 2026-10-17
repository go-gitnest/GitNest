package config

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestComposeRecipeMatchesTheDefaultConfiguration keeps the local development
// database and the compiled default in step.
//
// The check reads the file instead of starting Docker, so it runs everywhere: a
// contributor without a Docker daemon still learns that the two drifted apart,
// and CI catches the change that caused it.
func TestComposeRecipeMatchesTheDefaultConfiguration(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "compose.yaml"))
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	recipe := string(raw)

	defaults := mustLoad(t, nil)
	parsed, err := url.Parse(defaults.DatabaseURL)
	if err != nil {
		t.Fatalf("parse the default database URL: %v", err)
	}
	if parsed.User == nil {
		t.Fatal("the default database URL carries no user")
	}
	password, ok := parsed.User.Password()
	if !ok {
		t.Fatal("the default database URL carries no password")
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}

	for name, want := range map[string]string{
		"POSTGRES_USER":     parsed.User.Username(),
		"POSTGRES_PASSWORD": password,
		"POSTGRES_DB":       strings.Trim(parsed.Path, "/"),
	} {
		if got := composeValue(t, recipe, name); got != want {
			t.Errorf("compose.yaml sets %s to %q, but the default database URL uses %q", name, got, want)
		}
	}

	if !strings.Contains(recipe, `"`+port+`:`+port+`"`) {
		t.Errorf("compose.yaml does not publish port %s, which the default database URL expects", port)
	}
	if !regexp.MustCompile(`(?m)^\s+healthcheck:`).MatchString(recipe) {
		t.Error("compose.yaml has no healthcheck, so `docker compose ps` cannot report the database ready")
	}
	if !regexp.MustCompile(`(?m)^volumes:\s*$`).MatchString(recipe) {
		t.Error("compose.yaml declares no named volume, so `docker compose down -v` has nothing to delete")
	}
}

// composeValue returns the value of the first `NAME: value` line in the recipe.
func composeValue(t *testing.T, recipe, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `:\s*(\S+)\s*$`)
	match := pattern.FindStringSubmatch(recipe)
	if match == nil {
		t.Fatalf("compose.yaml does not set %s", name)
	}
	return match[1]
}
