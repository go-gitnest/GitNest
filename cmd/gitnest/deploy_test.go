package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The deployment artifacts are checked here rather than reviewed by eye, because the
// failure they are prone to is drift: a unit that names a command the binary does not
// have, an environment example that names a variable the configuration does not read,
// or a documented install step that installs a file that is not there.
//
// Every one of those is a machine-checkable claim, and an operator who follows a wrong
// one gets a service that does not start. That is exactly what happened while this
// workstream was being written: units were drafted for `worker` and `ssh` run modes
// that do not exist in this release, and shipping them would have failed on a machine
// whose operator trusted the documentation.

// repoRoot returns the repository root from the package's directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	return root
}

// TestEveryUnitNamesACommandThatExists is the check that would have caught the phantom
// units. A unit whose `ExecStart` names an unknown subcommand fails at startup, and
// `Type=notify` makes it worse: systemd waits for a readiness notification that a
// process which never started cannot send, so the failure is a timeout rather than a
// clear "unknown command".
func TestEveryUnitNamesACommandThatExists(t *testing.T) {
	root := repoRoot(t)
	units, err := filepath.Glob(filepath.Join(root, "deploy", "*.service"))
	if err != nil {
		t.Fatalf("list the units: %v", err)
	}
	if len(units) == 0 {
		t.Fatal("no unit files found; the deployment example is missing")
	}

	known := map[string]bool{}
	for _, command := range commands() {
		known[command.name] = true
	}

	execPattern := regexp.MustCompile(`(?m)^ExecStart=\s*(\S+)\s+(\S+)`)
	for _, unit := range units {
		name := filepath.Base(unit)
		content, err := os.ReadFile(unit)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		match := execPattern.FindStringSubmatch(string(content))
		if match == nil {
			t.Errorf("%s has no ExecStart with a subcommand", name)
			continue
		}
		if got, want := filepath.Base(match[1]), "gitnest"; got != want {
			t.Errorf("%s starts %q, want the gitnest binary", name, got)
		}
		if !known[match[2]] {
			t.Errorf("%s runs `gitnest %s`, which this binary does not implement; "+
				"a unit for a run mode that does not exist fails at startup", name, match[2])
		}
	}
}

// TestUnitsRequireAnExistingEnvironmentFile keeps a unit from pointing at a path the
// install steps never create.
func TestUnitsRequireAnExistingEnvironmentFile(t *testing.T) {
	root := repoRoot(t)
	units, err := filepath.Glob(filepath.Join(root, "deploy", "*.service"))
	if err != nil {
		t.Fatalf("list the units: %v", err)
	}

	environmentPattern := regexp.MustCompile(`(?m)^EnvironmentFile=(\S+)`)
	for _, unit := range units {
		name := filepath.Base(unit)
		content, err := os.ReadFile(unit)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		match := environmentPattern.FindStringSubmatch(string(content))
		if match == nil {
			t.Errorf("%s has no EnvironmentFile, so it would run with no configuration", name)
			continue
		}
		if match[1] != "/etc/gitnest/gitnest.env" {
			t.Errorf("%s reads %q, want the documented /etc/gitnest/gitnest.env", name, match[1])
		}
	}

	// The install documentation must create the file's directory and the file itself
	// from the example, or the unit cannot start.
	readme, err := os.ReadFile(filepath.Join(root, "deploy", "README.md"))
	if err != nil {
		t.Fatalf("read the deployment README: %v", err)
	}
	for _, step := range []string{
		"/etc/gitnest",
		"gitnest.env.example",
		"/etc/gitnest/gitnest.env",
	} {
		if !strings.Contains(string(readme), step) {
			t.Errorf("the deployment README does not mention %q, so the unit's environment file is never installed", step)
		}
	}
}

// TestTheEnvironmentExampleOnlyNamesKnownVariables is the drift check that matters
// most: an example naming a variable the configuration does not read would be silently
// ignored, and the operator would believe a setting had been applied.
//
// The expected set comes from the configuration's own environment constants, so a
// variable added there and forgotten here fails, and a variable invented here fails.
func TestTheEnvironmentExampleOnlyNamesKnownVariables(t *testing.T) {
	root := repoRoot(t)

	// The configuration's declared names, read from its source rather than duplicated:
	// duplicating them here would let the two drift, which is the thing being tested.
	source, err := os.ReadFile(filepath.Join(root, "internal", "config", "config.go"))
	if err != nil {
		t.Fatalf("read the configuration: %v", err)
	}
	declared := map[string]bool{}
	for _, match := range regexp.MustCompile(`"GITNEST_[A-Z0-9_]+"`).FindAllString(string(source), -1) {
		declared[strings.Trim(match, `"`)] = true
	}
	if len(declared) == 0 {
		t.Fatal("no environment constants were found in the configuration")
	}

	example, err := os.ReadFile(filepath.Join(root, "deploy", "gitnest.env.example"))
	if err != nil {
		t.Fatalf("read the environment example: %v", err)
	}

	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(example)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		name, _, found := strings.Cut(line, "=")
		if !found {
			t.Errorf("the example has a line that is neither a comment nor an assignment: %q", line)
			continue
		}
		name = strings.TrimSpace(name)
		if !strings.HasPrefix(name, "GITNEST_") {
			t.Errorf("the example sets %q, which is not a GitNest variable", name)
		}
		if !declared[name] {
			t.Errorf("the example sets %q, which the configuration does not read; "+
				"an operator would believe the setting had been applied", name)
		}
		seen[name] = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read the example: %v", err)
	}

	// Every declared variable is documented in the example, so an operator reading it
	// sees the full set rather than a subset they cannot distinguish from the whole.
	for name := range declared {
		if !seen[name] {
			t.Errorf("the configuration reads %q and the example does not document it", name)
		}
	}
}

// TestTheEnvironmentExampleCarriesNoRealSecret is the rule from packaging.md section
// 10: "the examples contain no secret values".
//
// A placeholder is fine and a plausible-looking credential is not, because the second
// one gets copied into a deployment.
func TestTheEnvironmentExampleCarriesNoRealSecret(t *testing.T) {
	root := repoRoot(t)
	example, err := os.ReadFile(filepath.Join(root, "deploy", "gitnest.env.example"))
	if err != nil {
		t.Fatalf("read the environment example: %v", err)
	}
	content := string(example)

	// The DSN must carry an obvious placeholder rather than something that looks
	// usable, and must not carry a password that could be mistaken for a real one.
	if !strings.Contains(content, "CHANGE_ME") {
		t.Error("the example's database URL carries no placeholder password")
	}
	for _, forbidden := range []string{
		"gitnest:gitnest@", // the development default, which is not a deployment value
		"password=",
		"PRIVATE KEY",
	} {
		if strings.Contains(content, forbidden) {
			t.Errorf("the example contains %q, which looks like a real credential", forbidden)
		}
	}
}

// TestTheContainerImageBuildsWithoutNodeInTheRuntime is the property the release smoke
// test checks: Node is a build-time dependency and the deployed image does not contain
// it.
//
// The assertions are on the Dockerfile's structure, which is what can be checked
// without a container runtime — and there is none in this environment.
func TestTheContainerImageBuildsWithoutNodeInTheRuntime(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("read the Dockerfile: %v", err)
	}
	content := string(raw)

	// The runtime stage must not be a Node image, and must not install Node.
	runtimeStart := strings.LastIndex(content, "\nFROM ")
	if runtimeStart < 0 {
		t.Fatal("the Dockerfile has no stages")
	}
	runtime := content[runtimeStart:]
	if strings.Contains(runtime, "node") {
		t.Error("the runtime stage is a Node image, so Node ships to production")
	}
	if strings.Contains(runtime, "apt-get install") && strings.Contains(runtime, "nodejs") {
		t.Error("the runtime stage installs Node, which is a build-time dependency only")
	}
	// A Git executable has to be there: the server invokes it.
	if !strings.Contains(runtime, "git") {
		t.Error("the runtime stage does not install Git, which the server invokes")
	}

	// The entrypoint is the binary in exec form, so a signal reaches the process and
	// the bounded shutdown runs. A shell form would swallow SIGTERM.
	if !strings.Contains(content, `ENTRYPOINT ["/usr/local/bin/gitnest"]`) {
		t.Error("the entrypoint is not the binary in exec form, so SIGTERM would not reach it")
	}
	if !strings.Contains(content, `CMD ["serve"]`) {
		t.Error("the image's default command is not `serve`")
	}
	// The data directory is declared as a volume, so an operator who forgets to mount
	// one sees the data in the container rather than discovering it later.
	if !strings.Contains(content, "VOLUME [\"/var/lib/gitnest\"]") {
		t.Error("the image does not declare its data directory as a volume")
	}
	// The process runs as a fixed non-root account, which packaging.md section 7
	// requires at UID/GID 10001.
	if !strings.Contains(content, "USER 10001:10001") {
		t.Error("the image does not run as the documented fixed non-root account")
	}
}

// TestTheHealthcheckUsesTheBinary covers the reason the health command exists: the
// image carries no `curl`, because packaging.md section 7 excludes anything beyond the
// binary, Git, its runtime dependencies, and CA certificates.
func TestTheHealthcheckUsesTheBinary(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("read the Dockerfile: %v", err)
	}
	content := string(raw)

	if !strings.Contains(content, "HEALTHCHECK") {
		t.Fatal("the image has no health check, which packaging.md section 7 requires")
	}
	if !strings.Contains(content, `"/usr/local/bin/gitnest", "health"`) {
		t.Error("the health check does not use the binary's own probe")
	}
	for _, forbidden := range []string{"curl ", "wget "} {
		if strings.Contains(content, forbidden) {
			t.Errorf("the health check uses %q, which would add a package the image deliberately excludes", forbidden)
		}
	}
}

// TestTheDocumentedInstallStepsReferenceFilesThatExist keeps the README's commands
// honest: an `install` of a file that is not in `deploy/` fails on the operator's
// machine.
func TestTheDocumentedInstallStepsReferenceFilesThatExist(t *testing.T) {
	root := repoRoot(t)
	readme, err := os.ReadFile(filepath.Join(root, "deploy", "README.md"))
	if err != nil {
		t.Fatalf("read the deployment README: %v", err)
	}

	// Every `deploy/<name>` the README names must be present.
	pattern := regexp.MustCompile(`deploy/([A-Za-z0-9._-]+)`)
	seen := map[string]bool{}
	for _, match := range pattern.FindAllStringSubmatch(string(readme), -1) {
		if seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		if _, err := os.Stat(filepath.Join(root, "deploy", match[1])); err != nil {
			t.Errorf("the deployment README installs deploy/%s, which does not exist", match[1])
		}
	}
	if len(seen) == 0 {
		t.Error("the deployment README references no files under deploy/")
	}

	// And every file in `deploy/` is described by the README, so an operator reading it
	// learns what each one is.
	entries, err := os.ReadDir(filepath.Join(root, "deploy"))
	if err != nil {
		t.Fatalf("list deploy/: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "README.md" {
			continue
		}
		if !strings.Contains(string(readme), entry.Name()) {
			t.Errorf("deploy/%s exists and the README does not mention it", entry.Name())
		}
	}
}

// TestConfigurationHasNoConfigurationFile keeps the documented claim true: "No
// configuration file is required to start" (packaging.md section 7).
//
// It is asserted by the configuration loader's own surface: every value it reads comes
// from the environment, so a file-based source would appear here as a reader that takes
// a path.
func TestConfigurationHasNoConfigurationFile(t *testing.T) {
	root := repoRoot(t)
	source, err := os.ReadFile(filepath.Join(root, "internal", "config", "config.go"))
	if err != nil {
		t.Fatalf("read the configuration: %v", err)
	}
	// A YAML, TOML, or JSON decode in the loader would be a configuration file.
	for _, forbidden := range []string{"yaml.Unmarshal", "toml.Unmarshal", "json.Unmarshal", "os.ReadFile"} {
		if strings.Contains(string(source), forbidden) {
			t.Errorf("the configuration loader uses %q, so a configuration file may be required", forbidden)
		}
	}
}
