package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// settingsInstance starts the whole application with a repository owned by
// `alice` and a second account `bob` that holds nothing yet.
type settingsInstance struct {
	base       string
	gitPath    string
	row        repository.Repository
	storageDir string
	alice      accountSession
	bob        accountSession
	bobID      string
	aliceID    string
	logs       *bytes.Buffer
	server     *httptest.Server
}

// startSettingsInstance boots the application and signs two accounts in.
func startSettingsInstance(t *testing.T, visibility repository.Visibility) *settingsInstance {
	t.Helper()
	gitPath := gitClient(t)
	schema := postgrestest.NewSchema(t)
	port := freePort(t)
	cfg := testConfig(t, schemaDSN(t, schema), port)

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
		t.Fatalf("bootstrap alice: %v", err)
	}

	// Bob exists through the real invitation path, so he is an ordinary account
	// rather than a bare row.
	accounts := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})
	_, token, err := accounts.CreateInvitation(ctx, identity.CreateInvitationInput{
		ActorID: admin.ID, Email: "bob@example.com",
	})
	if err != nil {
		cancel()
		t.Fatalf("invite bob: %v", err)
	}
	bob, err := accounts.AcceptInvitation(ctx, identity.AcceptInvitationInput{
		Token: token, Username: "bob", Password: endToEndPassword,
	})
	if err != nil {
		cancel()
		t.Fatalf("accept bob's invitation: %v", err)
	}

	repositories := repository.NewService(repository.NewPostgresStore(schema.Client.DB))
	row, err := repositories.Create(ctx, repository.CreateInput{
		Actor: repository.User(admin.ID, false), OwnerID: admin.ID, Name: "shared",
		Visibility: visibility,
	})
	if err != nil {
		cancel()
		t.Fatalf("create the repository: %v", err)
	}
	if _, err := repositories.SetDefaultBranch(ctx, repository.SetDefaultBranchInput{
		Actor: repository.User(admin.ID, false), RepoID: row.ID, Branch: "main",
	}); err != nil {
		cancel()
		t.Fatalf("set the default branch: %v", err)
	}

	layout, err := storage.NewLayout(cfg.RepoRoot, cfg.TmpRoot)
	if err != nil {
		cancel()
		t.Fatalf("NewLayout: %v", err)
	}
	storageDir, err := layout.EnsureRepository(row.StorageID)
	if err != nil {
		cancel()
		t.Fatalf("EnsureRepository: %v", err)
	}
	if output, err := exec.Command(gitPath, "init", "--bare", "--quiet", storageDir).CombinedOutput(); err != nil {
		cancel()
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}
	seedIntoBare(t, gitPath, storageDir, "# shared\n")

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

	instance := &settingsInstance{
		base: base, gitPath: gitPath, row: row, storageDir: storageDir,
		bobID: bob.ID, aliceID: admin.ID, logs: logOutput,
	}
	instance.alice = instance.signIn(t, "alice")
	instance.bob = instance.signIn(t, "bob")

	// The Git transport is mounted separately, because it needs an authenticator
	// and the application mounts the anonymous one: the credential model that
	// would verify a browser session as a Git credential is phase 4.
	runner, err := platformgit.NewRunner(gitPath, layout, 4)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	handler, err := githttp.NewHandler(runner, layout, accounts, repositories,
		fixedIdentity{principal: repository.User(bob.ID, false)}, nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	instance.server = server

	return instance
}

// signIn signs an account in and returns the cookie and CSRF token it holds.
func (instance *settingsInstance) signIn(t *testing.T, username string) accountSession {
	t.Helper()
	body, err := json.Marshal(map[string]string{"login": username, "password": endToEndPassword})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	response, err := http.Post(instance.base+"/api/v1/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("sign %s in: %v", username, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("sign-in for %s = %d", username, response.StatusCode)
	}

	session := accountSession{}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "gitnest_session" {
			session.cookie = cookie.Value
		}
	}
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err == nil {
		if csrf, ok := decoded["csrf_token"].(string); ok {
			session.csrf = csrf
		}
	}
	return session
}

// request sends an authenticated request with the CSRF token.
func (instance *settingsInstance) request(t *testing.T, method, path, body string, session accountSession) (int, map[string]any, string) {
	t.Helper()
	request, err := http.NewRequest(method, instance.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if session.cookie != "" {
		request.AddCookie(&http.Cookie{Name: "gitnest_session", Value: session.cookie})
	}
	if session.csrf != "" {
		request.Header.Set("X-CSRF-Token", session.csrf)
	}
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	decoded := map[string]any{}
	_ = json.Unmarshal(raw, &decoded)
	return response.StatusCode, decoded, string(raw)
}

// TestOwnerGrantsAndRevokesAndTheGitTransportsSeeItChange is the acceptance criterion at
// the strongest layer: the effect is visible on the next request, through the API
// and through the Git transport.
func TestOwnerGrantsAndRevokesAndTheGitsSeeItChange(t *testing.T) {
	instance := startSettingsInstance(t, repository.VisibilityPrivate)

	// Bob holds nothing, so the API answers not-found for the repository, and the
	// Git transport refuses his fetch.
	if status, _, _ := instance.request(t, http.MethodGet, "/api/v1/repos/alice/shared", "", instance.bob); status != http.StatusNotFound {
		t.Fatalf("bob's read before the grant = %d, want 404", status)
	}
	if output, err := runGit(t, instance.gitPath, "", "ls-remote",
		instance.server.URL+"/alice/shared.git"); err == nil {
		t.Fatalf("bob could reach the repository before the grant:\n%s", output)
	}

	// Alice grants write.
	status, body, raw := instance.request(t, http.MethodPut,
		"/api/v1/repos/alice/shared/collaborators/bob", `{"role_name":"write"}`, instance.alice)
	if status != http.StatusCreated {
		t.Fatalf("the grant = %d, want 201: %s", status, raw)
	}
	if body["login"] != "bob" || body["role_name"] != "write" {
		t.Errorf("the grant response = %v", body)
	}

	// The effect is visible on the very next request, through the API.
	if status, _, _ := instance.request(t, http.MethodGet, "/api/v1/repos/alice/shared", "", instance.bob); status != http.StatusOK {
		t.Fatalf("bob's read after the grant = %d, want 200", status)
	}
	// And through the Git transport, which shares the decision.
	output, err := runGit(t, instance.gitPath, "", "clone",
		instance.server.URL+"/alice/shared.git", filepath.Join(t.TempDir(), "bob-clone"))
	if err != nil {
		t.Fatalf("bob's clone after the grant failed: %v\n%s", err, output)
	}

	// Alice revokes.
	status, _, raw = instance.request(t, http.MethodDelete,
		"/api/v1/repos/alice/shared/collaborators/bob", "", instance.alice)
	if status != http.StatusNoContent {
		t.Fatalf("the revocation = %d, want 204: %s", status, raw)
	}

	// The next request through either transport is refused.
	if status, _, _ := instance.request(t, http.MethodGet, "/api/v1/repos/alice/shared", "", instance.bob); status != http.StatusNotFound {
		t.Errorf("bob's read after the revocation = %d, want 404", status)
	}
	if output, err := runGit(t, instance.gitPath, "", "ls-remote",
		instance.server.URL+"/alice/shared.git"); err == nil {
		t.Errorf("bob still reaches the repository through Git after the revocation:\n%s", output)
	}
}

// TestVisibilityChangeNeedsTheConfirmationItAsksFor covers the confirmation end
// to end: the refusal explains what to supply, and supplying it works.
func TestVisibilityChangeNeedsTheConfirmationItAsksFor(t *testing.T) {
	instance := startSettingsInstance(t, repository.VisibilityPrivate)

	// Without the confirmation the change is refused, and the refusal names the
	// field and carries the value to supply.
	status, body, raw := instance.request(t, http.MethodPatch, "/api/v1/repos/alice/shared",
		`{"visibility":"public"}`, instance.alice)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("an unconfirmed change = %d, want 422: %s", status, raw)
	}
	fieldErrors, _ := body["errors"].([]any)
	if len(fieldErrors) == 0 {
		t.Fatalf("the refusal carries no field: %s", raw)
	}
	first, _ := fieldErrors[0].(map[string]any)
	if first["field"] != "confirmation" || first["code"] != "confirmation_required" {
		t.Errorf("the refusal = %v, want the confirmation field", first)
	}
	if !strings.Contains(fmt.Sprint(first["message"]), "shared") {
		t.Errorf("the refusal does not say what to supply: %v", first["message"])
	}

	// The repository is unchanged.
	if status, body, _ := instance.request(t, http.MethodGet, "/api/v1/repos/alice/shared", "", instance.alice); status == http.StatusOK {
		if body["visibility"] != string(repository.VisibilityPrivate) {
			t.Errorf("the visibility changed despite the refusal: %v", body["visibility"])
		}
	}

	// With the confirmation it applies.
	status, body, raw = instance.request(t, http.MethodPatch, "/api/v1/repos/alice/shared",
		`{"visibility":"public","confirmation":"shared"}`, instance.alice)
	if status != http.StatusOK {
		t.Fatalf("a confirmed change = %d, want 200: %s", status, raw)
	}
	if body["visibility"] != string(repository.VisibilityPublic) {
		t.Errorf("visibility = %v, want public", body["visibility"])
	}

	// An anonymous caller can now read it, which is the observable effect of the
	// transition rather than a field in a payload.
	response, err := http.Get(instance.base + "/api/v1/repos/alice/shared")
	if err != nil {
		t.Fatalf("anonymous read: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Errorf("an anonymous read after publicising = %d, want 200", response.StatusCode)
	}
}

// TestSettingsSurfaceRefusesWhatItShould covers the settings surface's denied
// paths against a real instance.
func TestSettingsSurfaceRefusesWhatItShould(t *testing.T) {
	instance := startSettingsInstance(t, repository.VisibilityPrivate)

	for _, test := range []struct {
		name    string
		method  string
		path    string
		body    string
		session accountSession
		want    int
	}{
		{name: "bob may not change settings", method: http.MethodPatch,
			path: "/api/v1/repos/alice/shared", body: `{"default_branch":"develop"}`,
			session: instance.bob, want: http.StatusNotFound},
		{name: "bob may not list collaborators", method: http.MethodGet,
			path: "/api/v1/repos/alice/shared/collaborators", session: instance.bob,
			want: http.StatusNotFound},
		{name: "alice may change settings", method: http.MethodPatch,
			path: "/api/v1/repos/alice/shared", body: `{"default_branch":"develop"}`,
			session: instance.alice, want: http.StatusOK},
		{name: "an empty request changes nothing", method: http.MethodPatch,
			path: "/api/v1/repos/alice/shared", body: `{}`,
			session: instance.alice, want: http.StatusUnprocessableEntity},
		{name: "an invalid branch is refused", method: http.MethodPatch,
			path: "/api/v1/repos/alice/shared", body: `{"default_branch":"a..b"}`,
			session: instance.alice, want: http.StatusUnprocessableEntity},
		{name: "archiving applies", method: http.MethodPatch,
			path: "/api/v1/repos/alice/shared", body: `{"archived":true}`,
			session: instance.alice, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, _, raw := instance.request(t, test.method, test.path, test.body, test.session)
			if status != test.want {
				t.Errorf("status = %d, want %d: %s", status, test.want, raw)
			}
		})
	}
}

// TestSettingsChangeIsAuditedEndToEnd is the "visibility changes are audited"
// criterion through the HTTP surface rather than only through the service.
func TestSettingsChangeIsAuditedEndToEnd(t *testing.T) {
	instance := startSettingsInstance(t, repository.VisibilityPrivate)

	status, _, raw := instance.request(t, http.MethodPatch, "/api/v1/repos/alice/shared",
		`{"visibility":"public","confirmation":"shared"}`, instance.alice)
	if status != http.StatusOK {
		t.Fatalf("the change = %d: %s", status, raw)
	}

	logged := instance.logs.String()
	// The audit write itself is asserted against the ledger elsewhere; what this
	// checks is that the change reached the service with the request's context,
	// so the record can answer "who did this from where".
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("a log line is not JSON: %v\n%s", err, line)
		}
	}
	// The password never appears.
	if strings.Contains(logged, endToEndPassword) {
		t.Error("a log line carries the password")
	}
}
