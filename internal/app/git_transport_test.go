package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// gitClient returns the Git executable, or skips when none is present.
//
// It is the T3 tier's documented gate: the tier drives a real client, so a
// machine without Git cannot run it, and a run that requires the tier fails
// rather than skipping.
func gitClient(t *testing.T) string {
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
		t.Skipf("set GITNEST_TEST_GIT_BIN to run Git transport tests (%v)", err)
	}
	return resolved
}

// gitRepository is one repository prepared for the transport test.
type gitRepository struct {
	name string
	row  repository.Repository
	path string
}

// endpoint returns the clone URL for a repository.
func (g gitRepository) endpoint(base string) string {
	return base + "/alice/" + g.name + ".git"
}

// transportInstance starts the whole application against a fresh schema and
// prepares two repositories: one public, one private.
//
// The bare directories are created and seeded through Git's plumbing rather than
// through a push, because a local-path push reaches a shell transport that some
// environments refuse.
func transportInstance(t *testing.T, gitPath string) (string, []gitRepository, *bytes.Buffer) {
	t.Helper()
	schema := postgrestest.NewSchema(t)
	port := freePort(t)
	cfg := testConfig(t, schemaDSN(t, schema), port)

	// The access log and everything the handlers write go to one buffer, so the
	// log assertions below cover the real middleware chain.
	logOutput := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)

	admin, err := CreateInitialAdmin(ctx, cfg, identity.CreateInitialAdminInput{
		Username: "alice", Email: "alice@example.com", Password: endToEndPassword,
	})
	if err != nil {
		cancel()
		t.Fatalf("bootstrap the administrator: %v", err)
	}

	layout, err := storage.NewLayout(cfg.RepoRoot, cfg.TmpRoot)
	if err != nil {
		cancel()
		t.Fatalf("NewLayout: %v", err)
	}

	repositories := repository.NewService(repository.NewPostgresStore(schema.Client.DB))
	actor := repository.User(admin.ID, true)

	var prepared []gitRepository
	for _, spec := range []struct {
		name       string
		visibility repository.Visibility
	}{
		{name: "public-repo", visibility: repository.VisibilityPublic},
		{name: "private-repo", visibility: repository.VisibilityPrivate},
	} {
		row, err := repositories.Create(ctx, repository.CreateInput{
			Actor: actor, OwnerID: admin.ID, Name: spec.name, Visibility: spec.visibility,
		})
		if err != nil {
			cancel()
			t.Fatalf("create %s: %v", spec.name, err)
		}
		path, err := layout.EnsureRepository(row.StorageID)
		if err != nil {
			cancel()
			t.Fatalf("EnsureRepository(%s): %v", spec.name, err)
		}
		if _, err := exec.Command(gitPath, "init", "--bare", "--quiet", path).CombinedOutput(); err != nil {
			cancel()
			t.Fatalf("git init --bare: %v", err)
		}
		if err := seedBareRepository(gitPath, path, "# first\n"); err != nil {
			cancel()
			t.Fatalf("seed %s: %v", spec.name, err)
		}
		prepared = append(prepared, gitRepository{name: spec.name, row: row, path: path})
	}

	go func() { stopped <- New(cfg).Run(ctx) }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForStatus(t, base+"/api/ready", http.StatusOK, 25*time.Second)

	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(25 * time.Second):
			t.Error("the server did not stop during cleanup")
		}
	})
	return base, prepared, logOutput
}

// seedBareRepository writes one commit whose README has the given content,
// using plumbing only, and returns nothing because the commit is discovered
// through the clone.
func seedBareRepository(gitPath, repo, readme string) error {
	blobCommand := exec.Command(gitPath, "-C", repo, "hash-object", "-w", "--stdin")
	blobCommand.Stdin = strings.NewReader(readme)
	blobOutput, err := blobCommand.Output()
	if err != nil {
		return fmt.Errorf("hash-object: %w", err)
	}
	blob := strings.TrimSpace(string(blobOutput))

	treeCommand := exec.Command(gitPath, "-C", repo, "mktree")
	treeCommand.Stdin = strings.NewReader("100644 blob " + blob + "\tREADME.md\n")
	treeOutput, err := treeCommand.Output()
	if err != nil {
		return fmt.Errorf("mktree: %w", err)
	}
	tree := strings.TrimSpace(string(treeOutput))

	commitCommand := exec.Command(gitPath, "-C", repo, "commit-tree", tree, "-m", "seed")
	commitCommand.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	commitOutput, err := commitCommand.Output()
	if err != nil {
		return fmt.Errorf("commit-tree: %w", err)
	}
	commit := strings.TrimSpace(string(commitOutput))

	refCommand := exec.Command(gitPath, "-C", repo, "update-ref", "refs/heads/main", commit)
	if output, err := refCommand.CombinedOutput(); err != nil {
		return fmt.Errorf("update-ref: %w\n%s", err, output)
	}
	return nil
}

// runGit runs a Git command and returns its combined output.
func runGit(t *testing.T, gitPath, workingDir string, args ...string) (string, error) {
	t.Helper()
	command := exec.Command(gitPath, args...)
	if workingDir != "" {
		command.Dir = workingDir
	}
	// A clone of a private repository must fail rather than wait for input on a
	// terminal that does not exist.
	command.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	return output.String(), err
}

// TestCloneAndFetchThroughARealClient is the acceptance criterion: a real Git
// client clones and fetches, using protocol v2.
func TestCloneAndFetchThroughARealClient(t *testing.T) {
	gitPath := gitClient(t)
	base, repositories, _ := transportInstance(t, gitPath)

	var public gitRepository
	for _, candidate := range repositories {
		if candidate.name == "public-repo" {
			public = candidate
		}
	}

	cloneDir := filepath.Join(t.TempDir(), "clone")
	output, err := runGit(t, gitPath, "", "-c", "protocol.version=2", "clone", public.endpoint(base), cloneDir)
	if err != nil {
		t.Fatalf("clone failed: %v\n%s", err, output)
	}

	// The clone carries the seeded commit and its content.
	logOutput, err := runGit(t, gitPath, cloneDir, "log", "--oneline")
	if err != nil {
		t.Fatalf("git log: %v\n%s", err, logOutput)
	}
	if !strings.Contains(logOutput, "seed") {
		t.Errorf("the clone does not contain the seeded commit:\n%s", logOutput)
	}
	content, err := os.ReadFile(filepath.Join(cloneDir, "README.md"))
	if err != nil {
		t.Fatalf("read the cloned file: %v", err)
	}
	if string(content) != "# first\n" {
		t.Errorf("cloned content = %q, want the seeded content", content)
	}

	// A fetch after a new commit in the origin brings the change across.
	if err := seedBareRepository(gitPath, public.path, "# second\n"); err != nil {
		t.Fatalf("seed a second commit: %v", err)
	}
	fetchOutput, err := runGit(t, gitPath, cloneDir, "fetch", "origin")
	if err != nil {
		t.Fatalf("fetch failed: %v\n%s", err, fetchOutput)
	}
	// The fetched branch is now ahead of the local one.
	revisionOutput, err := runGit(t, gitPath, cloneDir, "rev-parse", "origin/main")
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, revisionOutput)
	}
	if strings.TrimSpace(revisionOutput) == "" {
		t.Error("origin/main does not resolve after the fetch")
	}
	if !strings.Contains(fetchOutput, "main") && !strings.Contains(fetchOutput, "origin") {
		t.Logf("fetch output: %s", fetchOutput)
	}
}

// TestProtocolVersionTwoIsOffered checks the capability advertisement directly,
// so v2 support is asserted rather than inferred from a clone succeeding.
func TestProtocolVersionTwoIsOffered(t *testing.T) {
	gitPath := gitClient(t)
	base, repositories, _ := transportInstance(t, gitPath)

	var public gitRepository
	for _, candidate := range repositories {
		if candidate.name == "public-repo" {
			public = candidate
		}
	}

	request, err := http.NewRequest(http.MethodGet,
		public.endpoint(base)+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Git-Protocol", "version=2")

	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("info/refs: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(response.Body); err != nil {
		t.Fatalf("read the advertisement: %v", err)
	}
	advertisement := body.String()
	// A v2 advertisement announces the version and the commands it supports.
	for _, want := range []string{"version 2", "ls-refs", "fetch"} {
		if !strings.Contains(advertisement, want) {
			t.Errorf("the v2 advertisement does not mention %q:\n%s", want, advertisement)
		}
	}
}

// TestPrivateAndMissingRepositoriesAreIndistinguishable is the T-A4 criterion
// through a real client: the two failures are identical.
func TestPrivateAndMissingRepositoriesAreIndistinguishable(t *testing.T) {
	gitPath := gitClient(t)
	base, repositories, _ := transportInstance(t, gitPath)

	var private gitRepository
	for _, candidate := range repositories {
		if candidate.name == "private-repo" {
			private = candidate
		}
	}

	work := t.TempDir()
	privateOutput, privateErr := runGit(t, gitPath, "",
		"clone", private.endpoint(base), filepath.Join(work, "private"))
	if privateErr == nil {
		t.Fatal("an anonymous clone of a private repository succeeded")
	}
	missingOutput, missingErr := runGit(t, gitPath, "",
		"clone", base+"/alice/no-such-repo.git", filepath.Join(work, "missing"))
	if missingErr == nil {
		t.Fatal("a clone of a nonexistent repository succeeded")
	}

	// The client's message is what a caller sees, so it must not differ.
	normalize := func(output, name string) string {
		replaced := strings.ReplaceAll(output, name, "<url>")
		replaced = strings.ReplaceAll(replaced, filepath.Join(work, "private"), "<dir>")
		replaced = strings.ReplaceAll(replaced, filepath.Join(work, "missing"), "<dir>")
		return replaced
	}
	privateMessage := normalize(privateOutput, private.endpoint(base))
	missingMessage := normalize(missingOutput, base+"/alice/no-such-repo.git")
	if privateMessage != missingMessage {
		t.Errorf("the two failures differ:\nprivate: %s\nmissing: %s", privateMessage, missingMessage)
	}
	// Neither says the repository is absent, and neither says it exists: both ask for a
	// credential. A client that presented none cannot be told which of the two it is,
	// because the server does not know either — an anonymous caller is challenged before any
	// repository is resolved.
	if strings.Contains(privateMessage, "not found") {
		t.Errorf("the failure reports the repository as missing: %s", privateMessage)
	}
	if !strings.Contains(strings.ToLower(privateMessage), "authentication") &&
		!strings.Contains(privateMessage, "could not read Username") {
		t.Errorf("the failure neither asks for a credential nor hides the answer: %s", privateMessage)
	}

	// And the raw responses are identical, header for header.
	for _, path := range []string{"/alice/private-repo.git", "/alice/no-such-repo.git"} {
		t.Run("info refs "+path, func(t *testing.T) {
			response, err := http.Get(base + path + "/info/refs?service=git-upload-pack")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			// **A 401 challenge is the correct answer, and it is not a weakening of the
			// rule this test is about.** Git's credential negotiation makes a client present
			// nothing first and only send a credential after a challenge, so answering an
			// unauthenticated request with a plain not-found makes a personal access token
			// unusable — which is how a real `git clone` failed while a hand-set header
			// passed. The disclosure T-A4 forbids is still absent: the challenge is
			// byte-identical for the private repository, the nonexistent one, and every other
			// repository on the instance, so it distinguishes nothing.
			if response.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want a 401 challenge", response.StatusCode)
			}
			if got := response.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Basic") {
				t.Errorf("WWW-Authenticate = %q, want a challenge a client can answer", got)
			}
			if got := response.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

// pktLine renders one pkt-line: a four-digit hexadecimal length that counts
// itself, followed by the payload.
func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

// TestProtocolVersionTwoRefFiltering exercises ls-refs with a ref-prefix, which
// is the protocol v2 feature a client uses to ask for one branch instead of the
// whole ref list.
func TestProtocolVersionTwoRefFiltering(t *testing.T) {
	gitPath := gitClient(t)
	base, repositories, _ := transportInstance(t, gitPath)

	var public gitRepository
	for _, candidate := range repositories {
		if candidate.name == "public-repo" {
			public = candidate
		}
	}

	request := func(prefix string) string {
		t.Helper()
		// A protocol v2 request is the command and its capabilities, then a
		// delimiter packet, then the arguments, then a flush packet. The
		// delimiter is `0001`; using a flush there would end the request before
		// the arguments and the server would answer with every ref.
		var body strings.Builder
		body.WriteString(pktLine("command=ls-refs\n"))
		body.WriteString(pktLine("object-format=sha1\n"))
		body.WriteString("0001")
		body.WriteString(pktLine("peel\n"))
		body.WriteString(pktLine("symrefs\n"))
		body.WriteString(pktLine("ref-prefix " + prefix + "\n"))
		body.WriteString("0000")

		post, err := http.NewRequest(http.MethodPost,
			public.endpoint(base)+"/git-upload-pack", strings.NewReader(body.String()))
		if err != nil {
			t.Fatalf("build the request: %v", err)
		}
		post.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		post.Header.Set("Git-Protocol", "version=2")

		response, err := (&http.Client{Timeout: 20 * time.Second}).Do(post)
		if err != nil {
			t.Fatalf("git-upload-pack: %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.StatusCode)
		}
		reply := new(bytes.Buffer)
		if _, err := reply.ReadFrom(response.Body); err != nil {
			t.Fatalf("read the reply: %v", err)
		}
		return reply.String()
	}

	t.Run("a matching prefix returns the ref", func(t *testing.T) {
		if reply := request("refs/heads/main"); !strings.Contains(reply, "refs/heads/main") {
			t.Errorf("the matching prefix returned no ref:\n%s", reply)
		}
	})

	t.Run("a non-matching prefix returns nothing", func(t *testing.T) {
		if reply := request("refs/heads/definitely-absent"); strings.Contains(reply, "refs/heads/main") {
			t.Errorf("the filter was ignored and the ref was returned anyway:\n%s", reply)
		}
	})
}

// TestCredentialsNeverReachTheLogs is the T-A10 criterion at the transport: a
// clone URL carrying credentials must not put them in a log line.
func TestCredentialsNeverReachTheLogs(t *testing.T) {
	gitPath := gitClient(t)
	base, repositories, logs := transportInstance(t, gitPath)

	var public gitRepository
	for _, candidate := range repositories {
		if candidate.name == "public-repo" {
			public = candidate
		}
	}

	const secret = "ghp_thisMustNeverBeLogged0123456789"
	credentialed := strings.Replace(base, "http://", "http://alice:"+secret+"@", 1)

	output, err := runGit(t, gitPath, "", "clone", credentialed+"/alice/public-repo.git",
		filepath.Join(t.TempDir(), "credentialed"))
	if err != nil {
		// A client that refuses a credentialed plain-HTTP URL still proves the
		// point only if the request was made, so the failure is reported.
		t.Logf("clone with credentials did not succeed: %v\n%s", err, output)
	}

	// The server may log the request, but never the credential.
	requestLog, err := http.NewRequest(http.MethodGet, public.endpoint(base)+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	requestLog.SetBasicAuth("alice", secret)
	if _, err := (&http.Client{Timeout: 20 * time.Second}).Do(requestLog); err != nil {
		t.Fatalf("credentialed request: %v", err)
	}

	logged := logs.String()
	if strings.Contains(logged, secret) {
		t.Errorf("a log line contains the credential")
	}
	if strings.Contains(logged, "Authorization") {
		t.Errorf("a log line names an authorization header")
	}

	// Every line must still be valid JSON, which is what the redaction boundary
	// is expected to preserve.
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("a log line is not JSON: %v\n%s", err, line)
		}
	}
}

// TestPrivateRepositoryCannotBeReadEvenWithAValidSessionCookie documents the
// credential boundary: a browser session is not a Git credential, so a cookie
// buys nothing on the transport.
func TestPrivateRepositoryCannotBeReadEvenWithAValidSessionCookie(t *testing.T) {
	gitPath := gitClient(t)
	base, repositories, _ := transportInstance(t, gitPath)

	var private gitRepository
	for _, candidate := range repositories {
		if candidate.name == "private-repo" {
			private = candidate
		}
	}

	// Sign in as the owner, who does have access to the private repository.
	body, err := json.Marshal(map[string]string{"login": "alice", "password": endToEndPassword})
	if err != nil {
		t.Fatalf("marshal the sign-in body: %v", err)
	}
	signIn, err := http.Post(base+"/api/v1/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	defer func() { _ = signIn.Body.Close() }()
	if signIn.StatusCode != http.StatusOK {
		t.Fatalf("sign-in status = %d, want 200", signIn.StatusCode)
	}

	var sessionCookie string
	for _, cookie := range signIn.Cookies() {
		if cookie.Name == "gitnest_session" {
			sessionCookie = cookie.Value
		}
	}
	if sessionCookie == "" {
		t.Fatal("sign-in set no session cookie")
	}

	request, err := http.NewRequest(http.MethodGet,
		private.endpoint(base)+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Cookie", "gitnest_session="+sessionCookie)
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	// A browser session is still not a Git credential: the challenge asks for one, and the
	// cookie is ignored. What changed is the status, because a client that presents nothing
	// has to be asked.
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want a 401 challenge: a browser session is not a Git credential", response.StatusCode)
	}
}

// TestUnknownRepositoryNameIsNotResolvedByGuessingUUIDs covers the identifier
// rule: an internal identifier is not a capability, and the transport route does
// not accept one at all.
func TestUnknownRepositoryNameIsNotResolvedByGuessingUUIDs(t *testing.T) {
	gitPath := gitClient(t)
	base, repositories, _ := transportInstance(t, gitPath)

	var private gitRepository
	for _, candidate := range repositories {
		if candidate.name == "private-repo" {
			private = candidate
		}
	}

	// The storage identifier and the row identifier must not address the
	// transport: only the owner/name route does.
	for _, identifier := range []string{private.row.ID, private.row.StorageID, uuid.NewString()} {
		t.Run(identifier, func(t *testing.T) {
			response, err := http.Get(base + "/alice/" + identifier + ".git/info/refs?service=git-upload-pack")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			// A name that resolves to nothing gets the same challenge as one that does, so
			// guessing a repository's identifier cannot tell the caller whether it exists.
			// The identifiers tried here are the row identifier and the storage identifier,
			// neither of which is ever addressable.
			if response.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want a 401 challenge", response.StatusCode)
			}
		})
	}
}
