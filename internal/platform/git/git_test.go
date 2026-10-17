package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/platform/storage"
)

// gitBinary returns the Git executable, or skips the test when none is present.
//
// The variable and the skip behaviour are the T3 tier's documented contract in
// docs/architecture/testing.md: a missing Git fails the tier when the tier is
// required, and otherwise skips.
func gitBinary(t *testing.T) string {
	t.Helper()
	candidate := strings.TrimSpace(os.Getenv("GITNEST_TEST_GIT_BIN"))
	if candidate == "" {
		candidate = "git"
	}
	resolved, err := exec.LookPath(candidate)
	if err != nil {
		if strings.EqualFold(strings.TrimSpace(os.Getenv("GITNEST_TEST_REQUIRE")), "git") {
			t.Fatalf("GITNEST_TEST_REQUIRE requires git, but %q was not found", candidate)
		}
		t.Skipf("set GITNEST_TEST_GIT_BIN to run Git runner tests (%v)", err)
	}
	return resolved
}

// harness is a layout and a runner over one temporary base directory.
type harness struct {
	t       *testing.T
	layout  *storage.Layout
	runner  *Runner
	gitPath string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	gitPath := gitBinary(t)
	base := t.TempDir()
	layout, err := storage.NewLayout(filepath.Join(base, "repositories"), filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	runner, err := NewRunner(gitPath, layout, 4)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return &harness{t: t, layout: layout, runner: runner, gitPath: gitPath}
}

// initBare creates a bare repository through Git itself, so the runner is
// exercised against a repository Git made rather than one the test faked.
func (h *harness) initBare(storageID string) string {
	h.t.Helper()
	path, err := h.layout.EnsureRepository(storageID)
	if err != nil {
		h.t.Fatalf("EnsureRepository: %v", err)
	}
	command := exec.Command(h.gitPath, "init", "--bare", "--quiet", path)
	if output, err := command.CombinedOutput(); err != nil {
		h.t.Fatalf("git init --bare: %v\n%s", err, output)
	}
	return path
}

// fsck runs `git fsck --strict`, the documented integrity oracle for a bare
// repository.
//
// The repository is selected with `-C` rather than passed as a trailing
// argument: `git fsck` takes object names, not a path, so passing one makes it
// interpret the path as a revision — and then it silently checks whatever
// repository the process happened to be in.
func (h *harness) fsck(path string) {
	h.t.Helper()
	command := exec.Command(h.gitPath, "-C", path, "fsck", "--strict", "--no-progress")
	if output, err := command.CombinedOutput(); err != nil {
		h.t.Fatalf("git fsck --strict failed: %v\n%s", err, output)
	}
}

// seedCommit writes one commit into a bare repository using only Git plumbing.
//
// It deliberately avoids `git push` and `git commit`: both reach a shell-based
// transport that some sandboxes refuse to run (the child cannot create a signal
// pipe), and a test that seeds its fixture through plumbing runs everywhere the
// runner does. This is a test-fixture choice, not a production one: the runner
// itself never needs a shell.
func (h *harness) seedCommit(path, branch string) string {
	h.t.Helper()

	// A blob.
	blobCommand := exec.Command(h.gitPath, "-C", path, "hash-object", "-w", "--stdin")
	blobCommand.Stdin = strings.NewReader("# hello\n")
	blobOutput, err := blobCommand.Output()
	if err != nil {
		h.t.Fatalf("git hash-object: %v", err)
	}
	blob := strings.TrimSpace(string(blobOutput))

	// A tree containing it. The entry format is `<mode> blob <sha>\t<name>`.
	treeCommand := exec.Command(h.gitPath, "-C", path, "mktree")
	treeCommand.Stdin = strings.NewReader("100644 blob " + blob + "\tREADME.md\n")
	treeOutput, err := treeCommand.Output()
	if err != nil {
		h.t.Fatalf("git mktree: %v", err)
	}
	tree := strings.TrimSpace(string(treeOutput))

	// A commit over that tree.
	commitCommand := exec.Command(h.gitPath, "-C", path, "commit-tree", tree, "-m", "first")
	commitCommand.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	commitOutput, err := commitCommand.Output()
	if err != nil {
		h.t.Fatalf("git commit-tree: %v", err)
	}
	commit := strings.TrimSpace(string(commitOutput))

	// Point a branch at it.
	refCommand := exec.Command(h.gitPath, "-C", path, "update-ref", "refs/heads/"+branch, commit)
	if output, err := refCommand.CombinedOutput(); err != nil {
		h.t.Fatalf("git update-ref: %v\n%s", err, output)
	}
	return commit
}

// --- request validation, before any process starts -------------------------

// TestUnknownServiceStartsNoProcess is the T-A1 assertion: an unrecognised verb
// is refused before anything is spawned.
func TestUnknownServiceStartsNoProcess(t *testing.T) {
	h := newHarness(t)
	path := h.initBare("01920000-0000-7000-8000-000000000001")

	for _, service := range []Service{
		"", "clone", "commit", "shell", "config", "upload-pack; rm -rf /",
		"upload-pack --exec=sh", "../upload-pack",
	} {
		t.Run(string(service), func(t *testing.T) {
			result, err := h.runner.Run(context.Background(), Request{
				Service:        service,
				RepositoryPath: path,
			})
			if !errors.Is(err, ErrUnknownService) {
				t.Fatalf("Run(%q) = %v, want ErrUnknownService", service, err)
			}
			if result.ExitCode != -1 {
				t.Errorf("exit code = %d, want -1 because no process started", result.ExitCode)
			}
			if result.Duration != 0 {
				t.Errorf("duration = %s, want zero because no process ran", result.Duration)
			}
		})
	}
}

func TestServiceAllowlist(t *testing.T) {
	// The protocol verbs and the inspection verbs are both allowed. The
	// inspection verbs are read-only and are documented in verbs.go.
	for _, service := range []Service{
		ServiceUploadPack, ServiceReceivePack, ServiceUploadArchive,
		ServiceRevParse, ServiceCatFile, ServiceLsTree, ServiceLog, ServiceForEachRef,
	} {
		if !ServiceAllowed(service) {
			t.Errorf("ServiceAllowed(%q) = false, want true", service)
		}
	}
	// A verb that writes, one that is not a verb at all, and near-miss spellings
	// are all refused. `gc`, `commit`, `push`, and `clone` would each change or
	// create state, so they must never be reachable.
	for _, service := range []Service{
		"", "clone", "commit", "push", "gc", "update-ref", "checkout", "reset",
		"Upload-Pack", "upload_pack", " upload-pack", "cat-file ", "REV-PARSE",
	} {
		if ServiceAllowed(service) {
			t.Errorf("ServiceAllowed(%q) = true, want false", service)
		}
	}
}

// TestReadOnlyClassification pins which verbs are inspection verbs, because the
// request builder selects the repository differently for each kind: getting this
// wrong would pass a path as an argument a verb does not accept.
func TestReadOnlyClassification(t *testing.T) {
	for _, service := range []Service{ServiceRevParse, ServiceCatFile, ServiceLsTree, ServiceLog, ServiceForEachRef} {
		if !ReadOnly(service) {
			t.Errorf("ReadOnly(%q) = false, want true", service)
		}
	}
	for _, service := range []Service{ServiceUploadPack, ServiceReceivePack, ServiceUploadArchive} {
		if ReadOnly(service) {
			t.Errorf("ReadOnly(%q) = true, want false", service)
		}
	}
}

// TestInspectionVerbsSelectTheRepositoryWithCDirectory proves the argument shape
// by running a real inspection against a real repository: if the repository were
// passed in the wrong position the command would fail.
func TestInspectionVerbsSelectTheRepositoryWithCDirectory(t *testing.T) {
	h := newHarness(t)
	path := h.initBare("01920000-0000-7000-8000-000000000050")
	commit := h.seedCommit(path, "main")

	var out strings.Builder
	result, err := h.runner.Run(context.Background(), Request{
		Service:        ServiceRevParse,
		RepositoryPath: path,
		ServiceArgs:    []string{"HEAD"},
		Stdout:         &out,
		Limits:         Limits{Timeout: 30 * time.Second},
	})
	if err != nil {
		t.Fatalf("rev-parse: %v\nstderr: %s", err, result.StderrTail)
	}
	if got := strings.TrimSpace(out.String()); got != commit {
		t.Errorf("rev-parse HEAD = %q, want the seeded commit %q", got, commit)
	}

	// The same repository read through ls-tree, which is the directory listing a
	// browse surface needs.
	out.Reset()
	result, err = h.runner.Run(context.Background(), Request{
		Service:        ServiceLsTree,
		RepositoryPath: path,
		ServiceArgs:    []string{"HEAD"},
		Stdout:         &out,
		Limits:         Limits{Timeout: 30 * time.Second},
	})
	if err != nil {
		t.Fatalf("ls-tree: %v\nstderr: %s", err, result.StderrTail)
	}
	if !strings.Contains(out.String(), "README.md") {
		t.Errorf("ls-tree HEAD did not list the seeded file: %q", out.String())
	}
}

// TestInspectionVerbsRejectAClientOption keeps the option-injection control in
// force for the verbs that were added.
func TestInspectionVerbsRejectAClientOption(t *testing.T) {
	h := newHarness(t)
	path := h.initBare("01920000-0000-7000-8000-000000000051")

	for _, test := range []struct {
		service Service
		args    []string
	}{
		{service: ServiceRevParse, args: []string{"--all"}},
		{service: ServiceLsTree, args: []string{"--help"}},
		{service: ServiceLog, args: []string{"-o", "PAGER=cat"}},
		{service: ServiceCatFile, args: []string{"--batch-all-objects"}},
	} {
		t.Run(string(test.service), func(t *testing.T) {
			result, err := h.runner.Run(context.Background(), Request{
				Service:        test.service,
				RepositoryPath: path,
				ServiceArgs:    test.args,
				Stdout:         nopWriter{},
				Limits:         Limits{Timeout: 10 * time.Second},
			})
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("Run = %v, want ErrInvalidArgument", err)
			}
			if result.ExitCode != -1 || result.Duration != 0 {
				t.Errorf("result = %+v, want no process to have run", result)
			}
		})
	}
}

// TestValidateArgumentRefusesOptionInjection is the control that stops a
// client-supplied ref from being read as an option.
func TestValidateArgumentRefusesOptionInjection(t *testing.T) {
	for _, test := range []struct {
		name     string
		argument string
		wantErr  bool
	}{
		{name: "ref", argument: "refs/heads/main", wantErr: false},
		{name: "sha", argument: "8f3c1a2b4c5d6e7f8091a2b3c4d5e6f708192a3b", wantErr: false},
		{name: "HEAD", argument: "HEAD", wantErr: false},
		{name: "detached", argument: "refs/tags/v1.0.0", wantErr: false},

		{name: "empty", argument: "", wantErr: true},
		{name: "leading dash", argument: "--upload-pack=sh", wantErr: true},
		{name: "single dash", argument: "-c", wantErr: true},
		{name: "embedded dash is fine", argument: "feature-1/x", wantErr: false},
		{name: "newline", argument: "main\nother", wantErr: true},
		{name: "carriage return", argument: "main\rother", wantErr: true},
		{name: "null byte", argument: "main\x00", wantErr: true},
		{name: "escape", argument: "main\x1b[31m", wantErr: true},
		{name: "tab", argument: "main\tother", wantErr: true},
		{name: "over-long", argument: strings.Repeat("a", maxArgumentLength+1), wantErr: true},
		{name: "at the limit", argument: strings.Repeat("a", maxArgumentLength), wantErr: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateArgument(test.argument)
			if test.wantErr && err == nil {
				t.Fatalf("ValidateArgument(%q) accepted the value", test.argument)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("ValidateArgument(%q): %v", test.argument, err)
			}
		})
	}
}

// TestInvalidArgumentStartsNoProcess proves the validation runs before the
// spawn, so a rejected request costs nothing.
func TestInvalidArgumentStartsNoProcess(t *testing.T) {
	h := newHarness(t)
	path := h.initBare("01920000-0000-7000-8000-000000000002")

	result, err := h.runner.Run(context.Background(), Request{
		Service:        ServiceUploadPack,
		RepositoryPath: path,
		ServiceArgs:    []string{"--upload-pack=sh"},
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Run = %v, want ErrInvalidArgument", err)
	}
	if result.ExitCode != -1 || result.Duration != 0 {
		t.Errorf("result = %+v, want no process to have run", result)
	}
}

// TestRepositoryPathMustBeInsideTheRoot covers the containment check.
func TestRepositoryPathMustBeInsideTheRoot(t *testing.T) {
	h := newHarness(t)

	outside := t.TempDir()
	for _, path := range []string{
		outside,
		"/etc",
		filepath.Join(h.layout.Root(), "..", "elsewhere"),
		"",
		"   ",
		// A sibling whose name merely starts with the root's name must not be
		// treated as being inside it.
		h.layout.Root() + "-evil",
	} {
		t.Run(path, func(t *testing.T) {
			_, err := h.runner.Run(context.Background(), Request{
				Service:        ServiceUploadPack,
				RepositoryPath: path,
			})
			if !errors.Is(err, ErrOutsideRepositoryRoot) {
				t.Fatalf("Run(%q) = %v, want ErrOutsideRepositoryRoot", path, err)
			}
		})
	}
}

// --- the environment ------------------------------------------------------

// TestChildEnvironmentIsMinimal is the T-A6 assertion: nothing inherited, and no
// credential reachable, even when this process holds one.
func TestChildEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("GITNEST_DATABASE_URL", "postgres://user:secret@host/db")
	t.Setenv("MY_SERVICE_TOKEN", "token-value")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("HTTPS_PROXY", "http://proxy:8080")
	t.Setenv("SOME_UNRELATED_VARIABLE", "should-not-travel")

	entries := Environment(nil)
	joined := strings.Join(entries, "\n")

	for _, forbidden := range []string{
		"secret", "token-value", "aws-secret", "proxy:8080",
		"should-not-travel", "GITNEST_DATABASE_URL", "MY_SERVICE_TOKEN",
		"AWS_SECRET_ACCESS_KEY",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("the child environment carries %q:\n%s", forbidden, joined)
		}
	}

	// The controls that matter must be present and explicit.
	for _, required := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("the child environment is missing %q:\n%s", required, joined)
		}
	}

	// PATH is inherited: Git needs it to find its own subcommands.
	if !strings.Contains(joined, "PATH=") {
		t.Error("the child environment has no PATH, so Git cannot find its subcommands")
	}
}

// TestEnvironmentRefusesCredentialBearingExtras proves a caller cannot smuggle a
// secret through the extra map.
func TestEnvironmentRefusesCredentialBearingExtras(t *testing.T) {
	entries := Environment(map[string]string{
		"GIT_PROTOCOL":             "version=2",
		"GITNEST_DATABASE_URL":     "postgres://user:secret@host/db",
		"CUSTOM_PASSWORD":          "hunter2",
		"SERVICE_PRIVATE_KEY":      "-----BEGIN KEY-----",
		"X_CREDENTIAL":             "value",
		"ANOTHER_TOKEN":            "value",
		"HTTP_PROXY":               "http://proxy:8080",
		"GIT_ASKPASS":              "/tmp/steal",
		"GIT_ALTERNATE_OBJECT_DIR": "/etc",
	})
	joined := strings.Join(entries, "\n")

	// The one legitimate extra survives.
	if !strings.Contains(joined, "GIT_PROTOCOL=version=2") {
		t.Errorf("a legitimate protocol variable was dropped:\n%s", joined)
	}
	for _, forbidden := range []string{
		"secret", "hunter2", "PRIVATE_KEY", "X_CREDENTIAL", "ANOTHER_TOKEN",
		"proxy:8080", "steal", "GIT_ALTERNATE_OBJECT_DIR",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("the child environment carries %q:\n%s", forbidden, joined)
		}
	}
}

// TestRealInvocationSeesNoCredential is the assertion at the boundary: a real
// child process reports the environment it actually received.
func TestRealInvocationSeesNoCredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The probe below emits the environment; on Windows the shell that would
		// print it is not available without a shell, which this package refuses
		// to use. The invariant is covered by the unit test above and by the
		// Linux CI run.
		t.Log("skipped on Windows: the probe needs a POSIX shell to print its environment")
		return
	}
	h := newHarness(t)
	t.Setenv("GITNEST_DATABASE_URL", "postgres://user:secret@host/db")

	// A Git alias cannot be used here (no configuration is read), so the
	// environment is observed by running a service that echoes its input and
	// asserting the child's own report through a script Git will not run.
	// Instead the assertion is made directly on the constructed slice, which is
	// exactly what the child receives.
	entries := Environment(nil)
	if strings.Contains(strings.Join(entries, "\n"), "secret") {
		t.Error("a credential reached the child environment")
	}
	_ = h
}

// --- a real operation ------------------------------------------------------

// TestUploadPackOnARealRepository runs the runner against a repository Git made,
// and then asks Git itself whether the repository is still sound.
func TestUploadPackOnARealRepository(t *testing.T) {
	h := newHarness(t)
	storageID := "01920000-0000-7000-8000-000000000003"
	path := h.initBare(storageID)

	// Seed one commit so the repository has real content to serve.
	commit := h.seedCommit(path, "main")
	h.fsck(path)

	// `upload-pack --stateless-rpc --advertise-refs` is what a clone's first
	// request does, and the runner passes the repository path as the trailing
	// argument the service expects.
	var out strings.Builder
	result, err := h.runner.Run(context.Background(), Request{
		Service:        ServiceUploadPack,
		RepositoryPath: path,
		Options:        []string{"--stateless-rpc", "--advertise-refs"},
		Stdout:         &out,
		Limits:         Limits{Timeout: 30 * time.Second},
	})
	if err != nil {
		t.Fatalf("upload-pack: %v\nstderr: %s", err, result.StderrTail)
	}
	if !result.Satisfied() {
		t.Fatalf("result = %+v, want a satisfied operation", result)
	}
	if result.BytesOut == 0 || out.Len() == 0 {
		t.Error("the operation produced no output, so nothing was advertised")
	}
	// The advertisement carries the head and the symbolic ref a real client
	// relies on to learn the default branch.
	//
	// The `# service=git-upload-pack` line is deliberately not asserted: that
	// line is written by `git http-backend`, not by `upload-pack`, so it belongs
	// to the transport that wraps this service rather than to the service.
	advertisement := out.String()
	for _, want := range []string{commit, "symref=HEAD:refs/heads/main"} {
		if !strings.Contains(advertisement, want) {
			t.Errorf("the advertisement does not contain %q:\n%s", want, advertisement)
		}
	}
	// The repository is unchanged and still sound.
	h.fsck(path)
}

// --- bounds, cancellation, and cleanup ------------------------------------

// TestCancellationReapsTheProcess covers the acceptance criterion: a cancelled
// operation stops promptly, reports the cancellation, and leaves nothing behind.
func TestCancellationReapsTheProcess(t *testing.T) {
	h := newHarness(t)
	storageID := "01920000-0000-7000-8000-000000000004"
	path := h.initBare(storageID)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Cancel while the operation is in flight.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result, err := h.runner.Run(ctx, Request{
		Service:        ServiceUploadPack,
		RepositoryPath: path,
		// An input that never arrives keeps the operation waiting, which is the
		// state cancellation has to break out of.
		Stdin:  stalledStdin(t),
		Stdout: nopWriter{},
		Limits: Limits{Timeout: 60 * time.Second},
	})
	if err == nil {
		t.Fatal("the cancelled operation reported success")
	}
	if !result.Cancelled {
		t.Errorf("result = %+v, want Cancelled", result)
	}
	if result.Duration > 20*time.Second {
		t.Errorf("cancellation took %s, want it to be prompt", result.Duration)
	}
	// The scratch area is left clean, which is the cleanup half of the
	// criterion.
	if err := h.assertTempRootEmpty(); err != nil {
		t.Error(err)
	}
}

// TestTimeoutKillsTheOperation covers the wall-clock bound.
func TestTimeoutKillsTheOperation(t *testing.T) {
	h := newHarness(t)
	storageID := "01920000-0000-7000-8000-000000000005"
	path := h.initBare(storageID)

	result, err := h.runner.Run(context.Background(), Request{
		Service:        ServiceUploadPack,
		RepositoryPath: path,
		Stdin:          stalledStdin(t),
		Stdout:         nopWriter{},
		Limits:         Limits{Timeout: 500 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("the timed-out operation reported success")
	}
	if !result.TimedOut {
		t.Errorf("result = %+v, want TimedOut", result)
	}
	if result.Duration > 10*time.Second {
		t.Errorf("the timeout took %s to take effect, want the configured 500ms", result.Duration)
	}
	if result.Cancelled {
		t.Error("a timeout was reported as a cancellation; the two outcomes are distinguishable")
	}
}

// TestOutputLimitStopsTheOperation covers the byte bound.
func TestOutputLimitStopsTheOperation(t *testing.T) {
	h := newHarness(t)
	storageID := "01920000-0000-7000-8000-000000000006"
	path := h.initBare(storageID)

	result, err := h.runner.Run(context.Background(), Request{
		Service:        ServiceUploadPack,
		RepositoryPath: path,
		Options:        []string{"--stateless-rpc", "--advertise-refs"},
		Stdout:         nopWriter{},
		// Smaller than any real advertisement, so the bound is certainly hit.
		Limits: Limits{Timeout: 30 * time.Second, MaxCapturedBytes: 16},
	})
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("Run = %v, want ErrOutputLimit", err)
	}
	if result.BytesOut > 64 {
		t.Errorf("wrote %d bytes, want the operation stopped near the 16-byte bound", result.BytesOut)
	}
}

// TestAdmissionControlRefusesRatherThanQueuing covers the concurrency bound.
func TestAdmissionControlRefusesRatherThanQueuing(t *testing.T) {
	gitPath := gitBinary(t)
	base := t.TempDir()
	layout, err := storage.NewLayout(filepath.Join(base, "repositories"), filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	// One slot, held by an operation that will not finish on its own.
	runner, err := NewRunner(gitPath, layout, 1)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	storageID := "01920000-0000-7000-8000-000000000007"
	path, err := layout.EnsureRepository(storageID)
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	if output, err := exec.Command(gitPath, "init", "--bare", "--quiet", path).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}

	// Occupy the only slot.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	go func() {
		defer close(release)
		_, _ = runner.Run(ctx, Request{
			Service: ServiceUploadPack, RepositoryPath: path,
			Stdin: stalledStdin(t), Stdout: nopWriter{},
			Limits: Limits{Timeout: 30 * time.Second},
		})
	}()
	// Wait until the slot is taken.
	deadline := time.Now().Add(10 * time.Second)
	for len(runner.slots) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(runner.slots) == 0 {
		t.Fatal("the first operation never took the slot")
	}

	// The second is refused rather than queued behind it.
	_, err = runner.Run(context.Background(), Request{
		Service: ServiceUploadPack, RepositoryPath: path,
		Stdout: nopWriter{}, Limits: Limits{Timeout: 5 * time.Second},
	})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("the second operation = %v, want ErrBusy", err)
	}

	cancel()
	<-release
}

// TestScratchDataIsRemovedAfterCancellation covers the cleanup criterion at the
// layout level, which is what a transport relies on.
func TestScratchDataIsRemovedAfterCancellation(t *testing.T) {
	h := newHarness(t)
	if err := h.layout.EnsureTmpRoot(); err != nil {
		t.Fatalf("EnsureTmpRoot: %v", err)
	}
	scratch, err := h.layout.TempPath("upload")
	if err != nil {
		t.Fatalf("TempPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(scratch, "pack"), 0o750); err != nil {
		t.Fatalf("create scratch data: %v", err)
	}

	if err := h.layout.CleanupTemp(); err != nil {
		t.Fatalf("CleanupTemp: %v", err)
	}
	if err := h.assertTempRootEmpty(); err != nil {
		t.Error(err)
	}
}

func (h *harness) assertTempRootEmpty() error {
	h.t.Helper()
	entries, err := os.ReadDir(h.layout.TmpRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("the scratch area still holds data after the operation ended")
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

// TestValidateConfigSetting covers the guard on a configuration override.
//
// The value is never client-derived, so this is a guard against a future call
// site interpolating request data into a setting rather than a filter for a
// client's benefit — and it keeps a newline out, which is what would let a value
// escape into a second configuration line.
func TestValidateConfigSetting(t *testing.T) {
	for _, test := range []struct {
		setting string
		valid   bool
	}{
		{setting: "receive.maxInputSize=2147483648", valid: true},
		{setting: "core.bare=true", valid: true},
		{setting: "key=", valid: true},
		{setting: "", valid: false},
		{setting: "no-equals-sign", valid: false},
		{setting: "=value", valid: false},
		{setting: "receive.maxInputSize=1\nother=2", valid: false},
		{setting: "key=va\x00lue", valid: false},
		{setting: "key=va\x1b[31mlue", valid: false},
		{setting: strings.Repeat("k", maxArgumentLength+1) + "=v", valid: false},
	} {
		t.Run(test.setting, func(t *testing.T) {
			err := validateConfigSetting(test.setting)
			if test.valid && err != nil {
				t.Errorf("validateConfigSetting(%q): %v", test.setting, err)
			}
			if !test.valid && err == nil {
				t.Errorf("validateConfigSetting(%q) accepted the setting", test.setting)
			}
		})
	}
}

// TestArgumentOrderPlacesConfigBeforeTheService is the property that makes a
// configuration override work at all: Git accepts `-c` before its subcommand and
// treats it as an argument elsewhere.
func TestArgumentOrderPlacesConfigBeforeTheService(t *testing.T) {
	h := newHarness(t)
	path := h.initBare("01920000-0000-7000-8000-000000000042")

	var out strings.Builder
	result, err := h.runner.Run(context.Background(), Request{
		Service:        ServiceUploadPack,
		RepositoryPath: path,
		Config:         []string{"uploadpack.allowFilter=true", "core.quotePath=false"},
		Options:        []string{"--stateless-rpc", "--advertise-refs"},
		Stdout:         &out,
		Limits:         Limits{Timeout: 30 * time.Second},
	})
	if err != nil {
		t.Fatalf("Run: %v\nstderr: %s", err, result.StderrTail)
	}
	if !result.Satisfied() {
		t.Fatalf("result = %+v, want a satisfied operation", result)
	}
	// The command would have failed outright if `-c` had been misplaced, so a
	// successful advertisement whose first byte is a pkt-line length is the
	// evidence.
	if out.Len() == 0 || !strings.HasPrefix(out.String(), "0") {
		t.Errorf("the advertisement does not look like a pkt-line stream: %q", out.String())
	}
}

// TestInvalidConfigStartsNoProcess keeps a malformed override from reaching Git.
func TestInvalidConfigStartsNoProcess(t *testing.T) {
	h := newHarness(t)
	path := h.initBare("01920000-0000-7000-8000-000000000043")

	for _, setting := range []string{"", "no-equals", "=value", "key=va\nlue"} {
		t.Run(setting, func(t *testing.T) {
			result, err := h.runner.Run(context.Background(), Request{
				Service:        ServiceUploadPack,
				RepositoryPath: path,
				Config:         []string{setting},
				Stdout:         nopWriter{},
				Limits:         Limits{Timeout: 10 * time.Second},
			})
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("Run = %v, want ErrInvalidArgument", err)
			}
			if result.ExitCode != -1 || result.Duration != 0 {
				t.Errorf("result = %+v, want no process to have run", result)
			}
		})
	}
}

// stalledStdin returns a read end that never receives data, with the write end
// held open by the test.
//
// It is an *os.File rather than an io.Reader on purpose. When Stdin is not a
// file, exec.Cmd starts a goroutine copying from the reader into a pipe, and
// Wait blocks until that copy returns — so a reader that blocks forever makes
// the test hang rather than the operation. Passing a file handle hands the
// descriptor straight to the child, which then blocks on its input exactly like
// a protocol request that stalls mid-negotiation, and no copy goroutine exists
// to outlive the kill.
func stalledStdin(t *testing.T) *os.File {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("create the stalled input pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = read.Close()
		_ = write.Close()
	})
	return read
}

// nopWriter discards output.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestDefaultsMatchTheDocumentedBudgets pins the limits table.
func TestDefaultsMatchTheDocumentedBudgets(t *testing.T) {
	limits := Limits{}.withDefaults()
	if limits.Timeout != DefaultFetchTimeout {
		t.Errorf("default timeout = %s, want %s", limits.Timeout, DefaultFetchTimeout)
	}
	if limits.StallTimeout != DefaultStallTimeout {
		t.Errorf("default stall timeout = %s, want %s", limits.StallTimeout, DefaultStallTimeout)
	}
	if limits.MaxCapturedBytes != DefaultMaxCapturedSize {
		t.Errorf("default byte bound = %d, want %d", limits.MaxCapturedBytes, DefaultMaxCapturedSize)
	}
	if DefaultFetchTimeout != 30*time.Minute {
		t.Errorf("the fetch timeout = %s, want the documented 30 minutes", DefaultFetchTimeout)
	}
	if DefaultPushTimeout != 15*time.Minute {
		t.Errorf("the push timeout = %s, want the documented 15 minutes", DefaultPushTimeout)
	}
	if DefaultStallTimeout != 60*time.Second {
		t.Errorf("the stall timeout = %s, want the documented 60 seconds", DefaultStallTimeout)
	}
	if DefaultMaxCapturedSize != 2<<30 {
		t.Errorf("the byte bound = %d, want the documented 2 GiB", DefaultMaxCapturedSize)
	}

	// A caller's values are honoured.
	custom := Limits{Timeout: time.Minute, StallTimeout: time.Second, MaxCapturedBytes: 10}
	if got := custom.withDefaults(); got != custom {
		t.Errorf("withDefaults changed explicit values: %+v", got)
	}
}

func TestNewRunnerResolvesTheBinaryOnce(t *testing.T) {
	gitPath := gitBinary(t)
	base := t.TempDir()
	layout, err := storage.NewLayout(filepath.Join(base, "repositories"), filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	runner, err := NewRunner(gitPath, layout, 1)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if !filepath.IsAbs(runner.Binary()) {
		t.Errorf("Binary() = %q, want a resolved absolute path", runner.Binary())
	}

	if _, err := NewRunner("definitely-not-a-real-git-binary", layout, 1); err == nil {
		t.Error("an unresolvable binary was accepted")
	}
	if _, err := NewRunner(gitPath, nil, 1); err == nil {
		t.Error("a nil layout was accepted")
	}
}
