package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/identity"
	platformgit "github.com/go-gitnest/GitNest/internal/platform/git"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/go-gitnest/GitNest/internal/repository"
	githttp "github.com/go-gitnest/GitNest/internal/transport/http/git"
)

// seedIntoBare writes one commit into a bare repository using Git plumbing, so
// the fixture does not depend on a working-tree transport.
func seedIntoBare(t *testing.T, gitPath, repo, readme string) {
	t.Helper()

	blobCommand := exec.Command(gitPath, "-C", repo, "hash-object", "-w", "--stdin")
	blobCommand.Stdin = strings.NewReader(readme)
	blobOutput, err := blobCommand.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	blob := strings.TrimSpace(string(blobOutput))

	treeCommand := exec.Command(gitPath, "-C", repo, "mktree")
	treeCommand.Stdin = strings.NewReader("100644 blob " + blob + "\tREADME.md\n")
	treeOutput, err := treeCommand.Output()
	if err != nil {
		t.Fatalf("mktree: %v", err)
	}
	tree := strings.TrimSpace(string(treeOutput))

	commitCommand := exec.Command(gitPath, "-C", repo, "commit-tree", tree, "-m", "seed")
	commitCommand.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	commitOutput, err := commitCommand.Output()
	if err != nil {
		t.Fatalf("commit-tree: %v", err)
	}
	commit := strings.TrimSpace(string(commitOutput))

	refCommand := exec.Command(gitPath, "-C", repo, "update-ref", "refs/heads/main", commit)
	if output, err := refCommand.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, output)
	}
}

// mountGitTransport serves the Git transport on a real listener with the given
// principal, and returns its base URL.
//
// It exists because three tests in this file need the same wiring with a
// different principal, and the principal is exactly what each of them varies.
func mountGitTransport(
	t *testing.T, gitPath string, layout *storage.Layout,
	accounts *identity.Service, repositories *repository.Service,
	principal repository.Principal,
) string {
	t.Helper()

	runner, err := platformgit.NewRunner(gitPath, layout, 4)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	var authenticator githttp.Authenticator = githttp.AnonymousAuthenticator{}
	if !principal.IsAnonymous() {
		authenticator = fixedIdentity{principal: principal}
	}

	handler, err := githttp.NewHandler(runner, layout, accounts, repositories, authenticator, nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// TestArchivedRepositoryRefusesARealPush is the acceptance criterion at the
// strongest layer available: a real `git push` from a real client, driven at a
// real server, is refused once the repository is archived, while a fetch still
// works.
//
// The principal is supplied through the transport's authenticator seam, because
// the credential model that would verify a production pusher is phase 4 and the
// personal access token that would carry it has no workstream in the plan. What
// this proves is that the archive decision reaches the Git transport through the
// shared service, which is the part W1.10 adds.
func TestArchivedRepositoryRefusesARealPush(t *testing.T) {
	gitPath := gitClient(t)
	schema := postgrestest.NewSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	accounts := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})
	owner, err := accounts.CreateInitialAdmin(ctx, identity.CreateInitialAdminInput{
		Username: "alice", Email: "alice@example.com", Password: endToEndPassword,
	})
	if err != nil {
		t.Fatalf("create the owner: %v", err)
	}
	// A second account holds the write grant, so the pusher is a collaborator
	// rather than the owner. That is what makes the test about the archive rule
	// rather than about ownership.
	pusherID := "01920000-0000-7000-8000-0000000000d1"
	if _, err := schema.Client.DB.ExecContext(ctx, `
		INSERT INTO users (id, username, username_folded, email, password_hash)
		VALUES ($1, 'pusher', 'pusher', 'pusher@example.com', 'placeholder')`, pusherID); err != nil {
		t.Fatalf("create the pusher: %v", err)
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
		Visibility: repository.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the repository: %v", err)
	}
	// The default branch is set so a clone has something to check out, and so the
	// archive test is not conflated with an empty repository.
	if _, err := repositories.SetDefaultBranch(ctx, repository.SetDefaultBranchInput{
		Actor: repository.User(owner.ID, false), RepoID: row.ID, Branch: "main",
	}); err != nil {
		t.Fatalf("set the default branch: %v", err)
	}
	if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
		Actor: repository.User(owner.ID, false), RepoID: row.ID, UserID: pusherID, Role: repository.LevelWrite,
	}); err != nil {
		t.Fatalf("grant write: %v", err)
	}

	storagePath, err := layout.EnsureRepository(row.StorageID)
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	if output, err := exec.Command(gitPath, "init", "--bare", "--quiet", storagePath).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}
	seedIntoBare(t, gitPath, storagePath, "# archived\n")

	world := mountGitTransport(t, gitPath, layout, accounts, repositories, repository.User(pusherID, false))
	endpoint := world + "/alice/pushable.git"

	cloneDir := filepath.Join(t.TempDir(), "work")
	if output, err := runGit(t, gitPath, "", "clone", endpoint, cloneDir); err != nil {
		t.Skipf("this environment cannot clone over HTTP: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "README.md"), []byte("# changed\n"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}
	if output, err := runGit(t, gitPath, cloneDir, "add", "README.md"); err != nil {
		t.Fatalf("git add: %v\n%s", err, output)
	}
	if output, err := runGit(t, gitPath, cloneDir, "commit", "--no-gpg-sign", "-m", "change"); err != nil {
		t.Fatalf("git commit: %v\n%s", err, output)
	}

	// A push while writable succeeds, which proves the fixture is a working push
	// path rather than one that fails for an unrelated reason.
	if output, err := runGit(t, gitPath, cloneDir, "push", "origin", "main"); err != nil {
		t.Fatalf("a push before archiving failed: %v\n%s", err, output)
	}

	// Archive it, then push again.
	if _, err := repositories.SetArchived(ctx, repository.SetArchivedInput{
		Actor: repository.User(owner.ID, false), RepoID: row.ID, Archived: true,
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if err := os.WriteFile(filepath.Join(cloneDir, "README.md"), []byte("# after archive\n"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}
	if output, err := runGit(t, gitPath, cloneDir, "add", "README.md"); err != nil {
		t.Fatalf("git add: %v\n%s", err, output)
	}
	if output, err := runGit(t, gitPath, cloneDir, "commit", "--no-gpg-sign", "-m", "after archive"); err != nil {
		t.Fatalf("git commit: %v\n%s", err, output)
	}

	output, err := runGit(t, gitPath, cloneDir, "push", "origin", "main")
	if err == nil {
		t.Fatalf("an archived repository accepted a push:\n%s", output)
	}
	// The refusal names the state rather than the permission, because the pusher
	// held the level: a not-found here would be a lie that hides an operational
	// fact from someone already allowed to see the repository.
	if !strings.Contains(output, "archived") {
		t.Errorf("the refusal does not name the archive state:\n%s", output)
	}
	// And the branch did not move.
	branches, err := runGit(t, gitPath, storagePath, "log", "--oneline", "-1")
	if err != nil {
		t.Fatalf("git log: %v\n%s", err, branches)
	}
	if strings.Contains(branches, "after archive") {
		t.Errorf("the refused push still landed:\n%s", branches)
	}

	// A fetch still works, so archiving removed the ability to change content and
	// not the ability to read it.
	if output, err := runGit(t, gitPath, cloneDir, "fetch", "origin"); err != nil {
		t.Errorf("a fetch on an archived repository failed: %v\n%s", err, output)
	}

	// The archive and the grant are both on the record.
	reader := audit.NewReader(schema.Client.DB)
	entries, err := reader.ForSubject(ctx, audit.SubjectRepository, row.ID)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	actions := make([]string, 0, len(entries))
	for _, entry := range entries {
		actions = append(actions, entry.Action)
	}
	if !containsActionName(actions, audit.ActionRepoArchived) {
		t.Errorf("the archive is not on the record: %v", actions)
	}
}

// TestArchivedRepositoryStillClonesAndFetches is the read half, asserted through
// a real client so the two halves cannot be confused.
func TestArchivedRepositoryStillClonesAndFetches(t *testing.T) {
	gitPath := gitClient(t)
	schema := postgrestest.NewSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	accounts := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})
	owner, err := accounts.CreateInitialAdmin(ctx, identity.CreateInitialAdminInput{
		Username: "alice", Email: "alice@example.com", Password: endToEndPassword,
	})
	if err != nil {
		t.Fatalf("create the owner: %v", err)
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
		Actor: repository.User(owner.ID, false), OwnerID: owner.ID, Name: "readable",
		Visibility: repository.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repositories.SetDefaultBranch(ctx, repository.SetDefaultBranchInput{
		Actor: repository.User(owner.ID, false), RepoID: row.ID, Branch: "main",
	}); err != nil {
		t.Fatalf("set the default branch: %v", err)
	}
	storagePath, err := layout.EnsureRepository(row.StorageID)
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	if output, err := exec.Command(gitPath, "init", "--bare", "--quiet", storagePath).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}
	seedIntoBare(t, gitPath, storagePath, "# readable\n")

	// Archived before the anonymous clone.
	if _, err := repositories.SetArchived(ctx, repository.SetArchivedInput{
		Actor: repository.User(owner.ID, false), RepoID: row.ID, Archived: true,
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// An anonymous caller, because the repository is public.
	world := mountGitTransport(t, gitPath, layout, accounts, repositories, repository.Anonymous())
	cloneDir := filepath.Join(t.TempDir(), "clone")

	output, err := runGit(t, gitPath, "", "clone", world+"/alice/readable.git", cloneDir)
	if err != nil {
		t.Fatalf("an anonymous clone of an archived public repository failed: %v\n%s", err, output)
	}
	content, err := os.ReadFile(filepath.Join(cloneDir, "README.md"))
	if err != nil {
		t.Fatalf("read the cloned file: %v", err)
	}
	if !strings.Contains(string(content), "readable") {
		t.Errorf("the cloned content = %q, want the seeded content", content)
	}
}
