package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	platformgit "github.com/go-gitnest/GitNest/internal/platform/git"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/go-gitnest/GitNest/internal/repository"
	githttp "github.com/go-gitnest/GitNest/internal/transport/http/git"
	"github.com/google/uuid"
)

// fixedIdentity answers every request as one account.
//
// It stands in for the credential model of phase 4, which is the workstream that
// would turn an `Authorization` header into a verified principal. Production
// wiring uses AnonymousAuthenticator, so each test below states which principal
// it pretends to be and the note on the first test explains why that is the only
// way to exercise the enforcement path today.
type fixedIdentity struct {
	principal repository.Principal
}

func (f fixedIdentity) Principal(*http.Request) repository.Principal { return f.principal }

// authenticatorFor adapts a principal to the transport's authenticator.
func authenticatorFor(principal repository.Principal) githttp.Authenticator {
	if principal.IsAnonymous() {
		return githttp.AnonymousAuthenticator{}
	}
	return fixedIdentity{principal: principal}
}

// pushWorld serves one repository over the real Git transport on a real HTTP
// listener, with a real Git client able to reach it.
type pushWorld struct {
	gitPath string
	base    string
	row     repository.Repository
	// storagePath is the bare directory on disk.
	storagePath string
	logs        *bytes.Buffer
	// pusherID is the account the authenticator resolves, empty when anonymous.
	pusherID string
}

// newPushWorld prepares a schema, an owner, an optional pushing account holding
// the given grant, and a repository, then mounts the Git transport.
//
// A grant of LevelNone means no pushing account is created and the caller is
// anonymous; that is the configuration production wiring produces.
func newPushWorld(t *testing.T, grant repository.Level, visibility repository.Visibility) *pushWorld {
	t.Helper()
	gitPath := gitClient(t)
	schema := postgrestest.NewSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	accounts := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})
	owner, err := accounts.CreateInitialAdmin(ctx, identity.CreateInitialAdminInput{
		Username: "alice", Email: "alice@example.com", Password: endToEndPassword,
	})
	if err != nil {
		t.Fatalf("bootstrap the owner: %v", err)
	}

	// The pushing account is a second account holding a direct grant, so the
	// grant level in the test name is what decides the outcome rather than the
	// owner's own level.
	pusherID := ""
	principal := repository.Anonymous()
	if grant > repository.LevelNone {
		pusherID = uuid.NewString()
		if _, err := schema.Client.DB.ExecContext(ctx, `
			INSERT INTO users (id, username, username_folded, email, password_hash)
			VALUES ($1, 'pusher', 'pusher', 'pusher@example.com', 'placeholder')`,
			pusherID); err != nil {
			t.Fatalf("create the pushing account: %v", err)
		}
		principal = repository.User(pusherID, false)
	}

	base := t.TempDir()
	layout, err := storage.NewLayout(filepath.Join(base, "repositories"), filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := layout.EnsureTmpRoot(); err != nil {
		t.Fatalf("EnsureTmpRoot: %v", err)
	}

	repositories := repository.NewService(repository.NewPostgresStore(schema.Client.DB))
	row, err := repositories.Create(ctx, repository.CreateInput{
		Actor: repository.User(owner.ID, false), OwnerID: owner.ID, Name: "pushable",
		Visibility: visibility,
	})
	if err != nil {
		t.Fatalf("create the repository: %v", err)
	}
	if grant > repository.LevelNone {
		if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
			Actor: repository.User(owner.ID, false), RepoID: row.ID,
			UserID: pusherID, Role: grant,
		}); err != nil {
			t.Fatalf("grant %s: %v", grant, err)
		}
	}

	storagePath, err := layout.EnsureRepository(row.StorageID)
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	if output, err := exec.Command(gitPath, "init", "--bare", "--quiet", storagePath).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}

	runner, err := platformgit.NewRunner(gitPath, layout, 4)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	// The record is rendered as JSON so an assertion reads the shape an operator
	// would see.
	logs := &bytes.Buffer{}
	handler, err := githttp.NewHandler(runner, layout, accounts, repositories,
		authenticatorFor(principal),
		func(message string, attributes ...any) {
			encoded, _ := json.Marshal(append([]any{"msg", message}, attributes...))
			fmt.Fprintln(logs, string(encoded))
		})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &pushWorld{
		gitPath: gitPath, base: server.URL, row: row, storagePath: storagePath,
		logs: logs, pusherID: pusherID,
	}
}

// endpoint is the clone and push URL for the repository.
func (w *pushWorld) endpoint() string {
	return w.base + "/alice/" + w.row.Name + ".git"
}

// branches lists the branches the bare repository actually has, which is how a
// test observes whether a push wrote anything.
func (w *pushWorld) branches(t *testing.T) string {
	t.Helper()
	output, err := runGit(t, w.gitPath, w.storagePath, "branch", "--list")
	if err != nil {
		t.Fatalf("git branch: %v\n%s", err, output)
	}
	return strings.TrimSpace(output)
}

// clone makes a working copy with one commit and returns its path.
func (w *pushWorld) clone(t *testing.T) string {
	t.Helper()
	cloneDir := filepath.Join(t.TempDir(), "work")
	if output, err := runGit(t, w.gitPath, "", "clone", w.endpoint(), cloneDir); err != nil {
		t.Skipf("this environment cannot clone over HTTP: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "README.md"), []byte("# pushed\n"), 0o600); err != nil {
		t.Fatalf("write a file: %v", err)
	}
	if output, err := runGit(t, w.gitPath, cloneDir, "add", "README.md"); err != nil {
		t.Fatalf("git add: %v\n%s", err, output)
	}
	if output, err := runGit(t, w.gitPath, cloneDir, "commit", "--no-gpg-sign", "-m", "pushed"); err != nil {
		t.Fatalf("git commit: %v\n%s", err, output)
	}
	return cloneDir
}

// TestPushSucceedsForACollaboratorHoldingWrite is the acceptance criterion that a
// caller with write permission pushes a branch and sees it in the repository.
//
// The credential model that would make a production caller hold write permission
// is phase 4 (W4.8), and the personal access token that would carry it has no
// workstream in the plan at all. This test therefore supplies the principal
// through the transport's authenticator seam and drives a real `git push` at a
// real server. What it proves is the enforcement, the ref validation, the size
// bounds, the streaming, and the outcome record; what it cannot prove is that a
// deployed instance can authenticate a pusher, which is stated in the round
// report and in the roadmap note.
func TestPushSucceedsForACollaboratorHoldingWrite(t *testing.T) {
	world := newPushWorld(t, repository.LevelWrite, repository.VisibilityPrivate)

	if got := world.branches(t); got != "" {
		t.Fatalf("the repository already has branches: %q", got)
	}

	cloneDir := world.clone(t)
	output, err := runGit(t, world.gitPath, cloneDir, "push", "origin", "main")
	if err != nil {
		t.Fatalf("push failed: %v\n%s", err, output)
	}

	// The branch exists in the bare repository.
	if got := world.branches(t); !strings.Contains(got, "main") {
		t.Fatalf("the branch is not in the repository after the push: %q", got)
	}

	// A second clone sees the pushed content, which is the observable outcome.
	second := filepath.Join(t.TempDir(), "second")
	if output, err := runGit(t, world.gitPath, "", "clone", world.endpoint(), second); err != nil {
		t.Fatalf("the second clone failed: %v\n%s", err, output)
	}
	content, err := os.ReadFile(filepath.Join(second, "README.md"))
	if err != nil {
		t.Fatalf("read the pushed file: %v", err)
	}
	if string(content) != "# pushed\n" {
		t.Errorf("the pushed content is %q, want the committed content", content)
	}

	// The outcome was recorded, naming the ref and what happened to it.
	recorded := world.logs.String()
	for _, want := range []string{"refs/heads/main", "created", "push outcome"} {
		if !strings.Contains(recorded, want) {
			t.Errorf("the push outcome record does not carry %q:\n%s", want, recorded)
		}
	}
}

// TestPushIsDeniedForAReadOnlyCollaborator is the denied path with a principal
// that holds read: the push is refused and the repository is untouched.
func TestPushIsDeniedForAReadOnlyCollaborator(t *testing.T) {
	world := newPushWorld(t, repository.LevelRead, repository.VisibilityPrivate)

	// Reading works, so the repository is genuinely reachable to this caller.
	cloneDir := world.clone(t)

	output, err := runGit(t, world.gitPath, cloneDir, "push", "origin", "main")
	if err == nil {
		t.Fatalf("a read-only collaborator pushed successfully:\n%s", output)
	}
	if got := world.branches(t); got != "" {
		t.Errorf("a refused push created a branch: %q", got)
	}
	// A refusal must not read as a server fault.
	if strings.Contains(output, "Internal Server Error") || strings.Contains(output, "500") {
		t.Errorf("the refusal reads as a server error:\n%s", output)
	}
}

// TestPushIsDeniedForAnAnonymousCaller is the denial production wiring produces
// today, and the shape a client sees.
func TestPushIsDeniedForAnAnonymousCaller(t *testing.T) {
	world := newPushWorld(t, repository.LevelNone, repository.VisibilityPublic)

	response, err := http.Get(world.endpoint() + "/info/refs?service=git-receive-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	// The push advertisement needs write, so an anonymous caller is challenged rather than
	// served. A challenge rather than a not-found, because Git's negotiation makes a client
	// present nothing first: answering with not-found would mean a pusher's credential was
	// never sent at all.
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("the push advertisement = %d, want a 401 challenge", response.StatusCode)
	}

	// Reading still works for an anonymous caller on this public repository, which is what
	// makes the difference authorization rather than unreachability.
	fetch, err := http.Get(world.endpoint() + "/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = fetch.Body.Close() }()
	if fetch.StatusCode != http.StatusOK {
		t.Errorf("the fetch advertisement = %d, want 200", fetch.StatusCode)
	}

	// A real client's push against the same endpoint is refused, and the refusal asks for a
	// credential rather than reporting the repository absent — which is the answer that
	// discloses nothing, because a caller holding a valid credential would succeed.
	cloneDir := world.clone(t)
	output, err := runGit(t, world.gitPath, cloneDir, "push", "origin", "main")
	if err == nil {
		t.Fatalf("an anonymous push succeeded:\n%s", output)
	}
	if strings.Contains(output, "not found") {
		t.Errorf("the refusal reports the repository as missing:\n%s", output)
	}
	if !strings.Contains(strings.ToLower(output), "authentication") &&
		!strings.Contains(output, "could not read Username") {
		t.Errorf("the refusal does not ask for a credential:\n%s", output)
	}
	if got := world.branches(t); got != "" {
		t.Errorf("a refused push created a branch: %q", got)
	}
}
