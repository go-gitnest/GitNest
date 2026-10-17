package app

import (
	"context"
	"encoding/base64"
	"io"
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
)

// The phase-1 exit criterion, end to end.
//
// `docs/roadmap/01-identity-and-repository-hosting.md` requires that "an invited
// contributor can sign in, create a private repository, push a local branch, browse the
// result, and clone it from a second working copy". Until this workstream that was
// impossible for a structural reason: every Git caller resolved as anonymous, because no
// phase-1 workstream delivered a credential a Git client can present.
//
// What these tests prove is the whole chain with nothing injected: an account, a token
// issued through the identity service and stored as a digest in PostgreSQL, a real `git`
// client presenting it over HTTP, the token authenticator resolving it, and the repository
// service deciding access. The only stand-in is the fake `git` runner the transport suites
// already use for its own process bounds — the fixture repository is a real bare
// repository on disk.

// tokenWorld is an instance with an owner, a token-holding contributor, and a repository.
type tokenWorld struct {
	t        *testing.T
	gitPath  string
	base     string
	server   *httptest.Server
	layout   *storage.Layout
	row      repository.Repository
	ownerID  string
	token    string
	tokenID  string
	accounts *identity.Service
}

// newTokenWorld prepares the instance and issues a token for a contributor holding the
// given grant.
func newTokenWorld(t *testing.T, grant repository.Level, visibility repository.Visibility) *tokenWorld {
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

	// The contributor exists through the real invitation path, so it is an ordinary
	// account rather than a bare row — which is what "an invited contributor" means.
	_, invitation, err := accounts.CreateInvitation(ctx, identity.CreateInvitationInput{
		ActorID: owner.ID, Email: "bob@example.com",
	})
	if err != nil {
		t.Fatalf("invite the contributor: %v", err)
	}
	contributor, err := accounts.AcceptInvitation(ctx, identity.AcceptInvitationInput{
		Token: invitation, Username: "bob", Password: endToEndPassword,
	})
	if err != nil {
		t.Fatalf("the contributor accepts: %v", err)
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
		Actor: repository.User(owner.ID, false), OwnerID: owner.ID, Name: "private-repo",
		Visibility: visibility,
	})
	if err != nil {
		t.Fatalf("create the repository: %v", err)
	}
	if _, err := repositories.SetDefaultBranch(ctx, repository.SetDefaultBranchInput{
		Actor: repository.User(owner.ID, false), RepoID: row.ID, Branch: "main",
	}); err != nil {
		t.Fatalf("set the default branch: %v", err)
	}

	// The grant is what the repository service decides against.
	if grant > repository.LevelNone {
		if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
			Actor: repository.User(owner.ID, false), RepoID: row.ID,
			UserID: contributor.ID, Role: grant,
		}); err != nil {
			t.Fatalf("grant %s: %v", grant, err)
		}
	}

	// The repository has one commit, so a clone has something to check out and a push has
	// something to update.
	storagePath, err := layout.EnsureRepository(row.StorageID)
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	if output, err := exec.Command(gitPath, "init", "--bare", "--quiet", "--initial-branch=main", storagePath).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}
	seedIntoBare(t, gitPath, storagePath, "# seeded\n")

	// The token is issued through the service, which is the path a real client's owner
	// would take, and it is stored as a digest.
	created, err := accounts.CreateToken(ctx, identity.CreateTokenInput{
		ActorID: contributor.ID, Name: "the contributor's laptop",
	})
	if err != nil {
		t.Fatalf("issue the contributor's token: %v", err)
	}

	// The **production** authenticator over the **real** identity service: nothing about
	// the credential path is faked.
	authenticator, err := githttp.NewTokenAuthenticator(accounts, nil)
	if err != nil {
		t.Fatalf("NewTokenAuthenticator: %v", err)
	}
	runner, err := platformgit.NewRunner(gitPath, layout, 4)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	handler, err := githttp.NewHandler(runner, layout, accounts, repositories, authenticator, nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &tokenWorld{
		t: t, gitPath: gitPath, base: base, server: server, layout: layout, row: row,
		ownerID: owner.ID, token: created.Value, tokenID: created.Token.ID, accounts: accounts,
	}
}

// endpoint is the clone URL, with no credential in it.
func (w *tokenWorld) endpoint() string {
	return w.server.URL + "/alice/private-repo.git"
}

// credentialURL is the clone URL carrying the token, in the shape a user types at a
// prompt.
//
// The credential is in the URL rather than in a header because a real `git` invocation
// cannot easily set one, and because this is the documented shape: a token used as the
// password. `git` redacts it from its own output, and the test asserts the token does not
// reach the server's log.
func (w *tokenWorld) credentialURL() string {
	scheme, rest, found := strings.Cut(w.endpoint(), "://")
	if !found {
		w.t.Fatalf("the endpoint has no scheme: %q", w.endpoint())
	}
	return scheme + "://" + "x-access-token:" + w.token + "@" + rest
}

// gitEnv is the environment every git invocation gets, with the global config disabled so
// a developer's proxy setting cannot decide the outcome.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
}

// runGit runs git and returns its combined output.
func (w *tokenWorld) runGit(dir string, args ...string) (string, error) {
	w.t.Helper()
	command := exec.Command(w.gitPath, args...)
	if dir != "" {
		command.Dir = dir
	}
	command.Env = gitEnv()
	output, err := command.CombinedOutput()
	return string(output), err
}

// branches lists the bare repository's branches.
func (w *tokenWorld) branches() string {
	w.t.Helper()
	path, err := w.layout.RepositoryPath(w.row.StorageID)
	if err != nil {
		w.t.Fatalf("RepositoryPath: %v", err)
	}
	output, err := w.runGit("", "-C", path, "for-each-ref", "--format=%(refname)", "refs/heads/")
	if err != nil {
		w.t.Fatalf("for-each-ref: %v\n%s", err, output)
	}
	return output
}

// TestAnInvitedContributorPushesWithAToken is the exit criterion: a contributor
// authenticates a real `git push` with a real token, and the branch lands.
func TestAnInvitedContributorPushesWithAToken(t *testing.T) {
	world := newTokenWorld(t, repository.LevelWrite, repository.VisibilityPrivate)

	// 1. Clone with the token. This is the first time a credential is presented, and a
	// failure here means the whole chain is broken.
	cloneDir := filepath.Join(t.TempDir(), "work")
	if output, err := world.runGit("", "clone", world.credentialURL(), cloneDir); err != nil {
		t.Fatalf("cloning with a token failed: %v\n%s", err, output)
	}
	// The clone brought the seeded content, so the credential read the repository rather
	// than merely being accepted.
	seeded, err := os.ReadFile(filepath.Join(cloneDir, "README.md"))
	if err != nil {
		t.Fatalf("read the seeded file: %v", err)
	}
	if string(seeded) != "# seeded\n" {
		t.Errorf("the clone's content is %q, want the seeded content", seeded)
	}

	// 2. Commit and push with the same token.
	if err := os.WriteFile(filepath.Join(cloneDir, "README.md"), []byte("# pushed by a token\n"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}
	if output, err := world.runGit(cloneDir, "add", "README.md"); err != nil {
		t.Fatalf("git add: %v\n%s", err, output)
	}
	if output, err := world.runGit(cloneDir, "commit", "--no-gpg-sign", "-q", "-m", "a token push"); err != nil {
		t.Fatalf("git commit: %v\n%s", err, output)
	}
	// The remote URL carries the credential, so the push re-presents it.
	if output, err := world.runGit(cloneDir, "push", "--quiet", world.credentialURL(), "main"); err != nil {
		t.Fatalf("pushing with a token failed: %v\n%s", err, output)
	}

	// 3. The branch is in the bare repository, which is the observable outcome.
	if branches := world.branches(); !strings.Contains(branches, "main") {
		t.Fatalf("the branch did not land: %q", branches)
	}

	// 4. A second clone sees the pushed content — the exit criterion's "clone it from a
	// second working copy".
	second := filepath.Join(t.TempDir(), "second")
	if output, err := world.runGit("", "clone", world.credentialURL(), second); err != nil {
		t.Fatalf("the second clone failed: %v\n%s", err, output)
	}
	content, err := os.ReadFile(filepath.Join(second, "README.md"))
	if err != nil {
		t.Fatalf("read the pushed file: %v", err)
	}
	if string(content) != "# pushed by a token\n" {
		t.Errorf("the second clone's content is %q, want what was pushed", content)
	}
}

// TestARevokedTokenStopsWorkingOnTheNextRequest is the revocation rule proved through the
// Git transport: the next clone fails, from the same client, with no restart.
func TestARevokedTokenStopsWorkingOnTheNextRequest(t *testing.T) {
	world := newTokenWorld(t, repository.LevelWrite, repository.VisibilityPrivate)
	ctx := context.Background()

	// It works first, so the failure afterwards is the revocation rather than a broken
	// fixture.
	if output, err := world.runGit("", "clone", world.credentialURL(), filepath.Join(t.TempDir(), "before")); err != nil {
		t.Fatalf("the token did not work before revocation: %v\n%s", err, output)
	}

	revoked, err := world.accounts.RevokeToken(ctx, world.contributorID(t), world.tokenID)
	if err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if !revoked {
		t.Fatal("the revocation reported nothing revoked")
	}

	// The very next clone is refused. A revoked credential resolves to anonymous, and an
	// anonymous caller is **challenged** rather than told the repository does not exist —
	// which is what makes Git's credential negotiation work at all, and which is the
	// answer that discloses nothing: the challenge is identical for every repository.
	output, err := world.runGit("", "clone", world.credentialURL(), filepath.Join(t.TempDir(), "after"))
	if err == nil {
		t.Fatalf("a revoked token still cloned:\n%s", output)
	}
	if !strings.Contains(output, "Authentication failed") && !strings.Contains(output, "authentication") {
		t.Errorf("the refusal does not ask for a usable credential:\n%s", output)
	}
	// The refusal must not say the repository is absent, because it is not: the caller's
	// credential was the problem, and a client that had a valid one would succeed.
	if strings.Contains(output, "not found") {
		t.Errorf("the refusal reports the repository as missing:\n%s", output)
	}
}

// contributorID resolves the contributor through a token that still works, so the test
// does not need to carry the identifier around.
func (w *tokenWorld) contributorID(t *testing.T) string {
	t.Helper()
	user, _, err := w.accounts.ResolveToken(context.Background(), w.token)
	if err != nil {
		t.Fatalf("resolve the contributor: %v", err)
	}
	return user.ID
}

// TestAReadOnlyContributorIsRefusedAPushWithAToken is the denied path: the credential is
// valid, and the level is what refuses the write.
func TestAReadOnlyContributorIsRefusedAPushWithAToken(t *testing.T) {
	world := newTokenWorld(t, repository.LevelRead, repository.VisibilityPrivate)

	// The credential authenticates, so the clone succeeds: this proves the refusal below
	// is about the level rather than about the token.
	cloneDir := filepath.Join(t.TempDir(), "work")
	if output, err := world.runGit("", "clone", world.credentialURL(), cloneDir); err != nil {
		t.Fatalf("a read-only contributor could not clone: %v\n%s", err, output)
	}

	if err := os.WriteFile(filepath.Join(cloneDir, "README.md"), []byte("# not allowed\n"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}
	if output, err := world.runGit(cloneDir, "add", "README.md"); err != nil {
		t.Fatalf("git add: %v\n%s", err, output)
	}
	if output, err := world.runGit(cloneDir, "commit", "--no-gpg-sign", "-q", "-m", "should not land"); err != nil {
		t.Fatalf("git commit: %v\n%s", err, output)
	}

	output, err := world.runGit(cloneDir, "push", "--quiet", world.credentialURL(), "main")
	if err == nil {
		t.Fatalf("a read-only contributor pushed:\n%s", output)
	}
	// The refusal is a not-found rather than a permission error, so a caller holding read
	// cannot learn from a push attempt that pushing would reveal more.
	if !strings.Contains(output, "not found") && !strings.Contains(output, "404") {
		t.Errorf("the refusal is not the not-found shape:\n%s", output)
	}
	// And nothing landed.
	if branches := world.branches(); strings.Contains(branches, "should") {
		t.Error("a refused push changed the repository")
	}
}

// TestAnInvalidTokenIsRefusedIndistinguishablyFromNoToken is T-A4 at the Git transport.
//
// **It asserts the server's answer, not git's rendering of it.** That distinction cost a
// wrong assertion here: a real client with a credential in its URL reports the server's
// challenge, while one with no credential never sends a second request at all and fails
// locally with `could not read Username`. Comparing the two clients' output would therefore
// have measured git's behaviour rather than the server's, and the property the threat model
// is about — that a denied caller cannot tell what exists — lives entirely in the server's
// response.
//
// So two requests are made directly, and the two responses must be identical in status,
// in challenge, and in body. That is the strongest available form of the rule.
func TestAnInvalidTokenIsRefusedIndistinguishablyFromNoToken(t *testing.T) {
	world := newTokenWorld(t, repository.LevelWrite, repository.VisibilityPrivate)
	ctx := context.Background()

	// A value with the right shape that no account holds.
	unknown := strings.Repeat("A", identity.MinTokenLength)

	type answer struct {
		status      int
		challenge   string
		contentType string
		body        string
	}
	collect := func(name, authorization string) answer {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet,
			world.endpoint()+"/info/refs?service=git-upload-pack", nil)
		if err != nil {
			t.Fatalf("build the %s request: %v", name, err)
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("the %s request failed: %v", name, err)
		}
		defer func() { _ = response.Body.Close() }()
		body, _ := io.ReadAll(response.Body)
		return answer{
			status:      response.StatusCode,
			challenge:   response.Header.Get("WWW-Authenticate"),
			contentType: response.Header.Get("Content-Type"),
			body:        string(body),
		}
	}

	anonymous := collect("anonymous", "")
	wrong := collect("wrong token", "Bearer "+unknown)

	// Neither is served: the repository is private and neither caller proved it may read it.
	if anonymous.status == http.StatusOK || wrong.status == http.StatusOK {
		t.Fatalf("a private repository was served: anonymous=%d wrong=%d", anonymous.status, wrong.status)
	}
	// **The two answers are the same in every respect a client can observe.** This is the
	// property: presenting a wrong token teaches a caller nothing that presenting no token
	// would not.
	if anonymous.status != wrong.status {
		t.Errorf("status differs: anonymous=%d wrong=%d", anonymous.status, wrong.status)
	}
	if anonymous.challenge != wrong.challenge {
		t.Errorf("the challenge differs: anonymous=%q wrong=%q", anonymous.challenge, wrong.challenge)
	}
	if anonymous.body != wrong.body {
		t.Errorf("the body differs:\n anonymous=%q\n wrong=%q", anonymous.body, wrong.body)
	}

	// And the answer does not claim the repository is absent, because it is not: the
	// challenge is what a client needs to present a credential, and the same challenge is
	// returned for every repository on the instance.
	if anonymous.status != http.StatusUnauthorized {
		t.Errorf("the answer is %d, want a 401 challenge so a client presents its credential", anonymous.status)
	}
	if !strings.Contains(anonymous.challenge, "Basic") {
		t.Errorf("the challenge %q does not name a scheme", anonymous.challenge)
	}
	// The realm is the instance's rather than the repository's, so the challenge cannot be
	// used to tell one repository from another.
	if strings.Contains(anonymous.challenge, "private-repo") {
		t.Errorf("the challenge names the repository: %q", anonymous.challenge)
	}

	// A caller holding a valid credential but not the level gets the not-found answer,
	// which is the case T-A4 is about: that caller has proven it holds a real credential, so
	// telling it a repository is absent would be a disclosure worth avoiding. This is
	// asserted by the read-only push test, and the distinction is recorded here so the two
	// answers are not confused.
}

// serverAnswer is one probed response.
type serverAnswer struct {
	status      int
	challenge   string
	contentType string
	body        string
}

// TestTheTokenNeverReachesTheLog is the secrecy rule at the transport, with the token
// travelling in a URL — the shape most likely to leak, because a naive access log records
// the whole request line.
func TestTheTokenNeverReachesTheLog(t *testing.T) {
	gitPath := gitClient(t)
	schema := postgrestest.NewSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	accounts := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})
	owner, err := accounts.CreateInitialAdmin(ctx, identity.CreateInitialAdminInput{
		Username: "alice", Email: "alice@example.com", Password: endToEndPassword,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	created, err := accounts.CreateToken(ctx, identity.CreateTokenInput{
		ActorID: owner.ID, Name: "log-check",
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// The transport's own log is what is asserted, because that is the line an operator
	// reads and the one `performance.md` lists tokens against.
	var logged []string
	authenticator, err := githttp.NewTokenAuthenticator(accounts, func(message string, attributes ...any) {
		line := message
		for _, attribute := range attributes {
			if text, ok := attribute.(string); ok {
				line += " " + text
			}
		}
		logged = append(logged, line)
	})
	if err != nil {
		t.Fatalf("NewTokenAuthenticator: %v", err)
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
	if _, err := repositories.Create(ctx, repository.CreateInput{
		Actor: repository.User(owner.ID, false), OwnerID: owner.ID, Name: "private-repo",
		Visibility: repository.VisibilityPrivate,
	}); err != nil {
		t.Fatalf("create the repository: %v", err)
	}

	runner, err := platformgit.NewRunner(gitPath, layout, 2)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	handler, err := githttp.NewHandler(runner, layout, accounts, repositories, authenticator, nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	// A request presenting the valid token, which is accepted and therefore logs nothing.
	// A client request is built from the URL rather than from a server-side request
	// recorder: `httptest.NewRequest` sets `RequestURI`, and the HTTP client refuses to send
	// a request carrying one.
	accepted, err := http.NewRequestWithContext(ctx, http.MethodGet,
		server.URL+"/alice/private-repo.git/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	accepted.Header.Set("Authorization", "Basic "+base64Credential("x-access-token", created.Value))
	response, err := http.DefaultClient.Do(accepted)
	if err != nil {
		t.Fatalf("the authenticated request failed: %v", err)
	}
	_ = response.Body.Close()

	// And one presenting a wrong token, which is refused and therefore does log.
	refused, err := http.NewRequestWithContext(ctx, http.MethodGet,
		server.URL+"/alice/private-repo.git/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	refused.Header.Set("Authorization", "Bearer "+strings.Repeat("A", identity.MinTokenLength))
	response, err = http.DefaultClient.Do(refused)
	if err != nil {
		t.Fatalf("the refused request failed: %v", err)
	}
	_ = response.Body.Close()

	joined := strings.Join(logged, "\n")
	if strings.Contains(joined, created.Value) {
		t.Fatalf("the token reached the log:\n%s", joined)
	}
	// The prefix is stored in the clear for a listing, so a log carrying it is tolerable —
	// but the value's distinguishing tail must not appear anywhere.
	if strings.Contains(joined, created.Value[identity.MinTokenLength-16:]) {
		t.Errorf("the log carries the token's tail:\n%s", joined)
	}
	// The refusal was reported, which is what an operator needs.
	if !strings.Contains(joined, "refused") {
		t.Errorf("a refused credential was not reported:\n%s", joined)
	}
}

// base64Credential encodes a Basic credential.
func base64Credential(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}
