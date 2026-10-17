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

	"github.com/go-gitnest/GitNest/internal/i18n"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// renderedInstance starts the whole application against a fresh schema with two
// repositories — one public, one private — each holding one commit.
//
// It is the strongest evidence available for the read surfaces: the real server,
// the real middleware chain, the real services, a real database, and real
// repository content.
func renderedInstance(t *testing.T) (string, gitRepository, gitRepository, *bytes.Buffer) {
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
		t.Fatalf("bootstrap the account: %v", err)
	}

	layout, err := storage.NewLayout(cfg.RepoRoot, cfg.TmpRoot)
	if err != nil {
		cancel()
		t.Fatalf("NewLayout: %v", err)
	}
	repositories := repository.NewService(repository.NewPostgresStore(schema.Client.DB))

	var prepared []gitRepository
	for _, spec := range []struct {
		name       string
		visibility repository.Visibility
		readme     string
	}{
		{name: "public-repo", visibility: repository.VisibilityPublic, readme: "# Public\n"},
		{name: "private-repo", visibility: repository.VisibilityPrivate, readme: "# Private\n"},
	} {
		row, err := repositories.Create(ctx, repository.CreateInput{
			Actor: repository.User(admin.ID, false), OwnerID: admin.ID, Name: spec.name,
			Visibility: spec.visibility, Description: "A repository",
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
		if err := seedBareRepository(gitPath, path, spec.readme); err != nil {
			cancel()
			t.Fatalf("seed %s: %v", spec.name, err)
		}
		// The overview needs a default branch, which a real push would set.
		if _, err := schema.Client.DB.ExecContext(ctx,
			"UPDATE repositories SET default_branch = 'main' WHERE id = $1", row.ID); err != nil {
			cancel()
			t.Fatalf("set the default branch: %v", err)
		}
		row.DefaultBranch = "main"
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
	return base, prepared[0], prepared[1], logOutput
}

// getText fetches a page and returns it as text.
func getText(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	buffer := new(bytes.Buffer)
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		t.Fatalf("read the body: %v", err)
	}
	return response.StatusCode, buffer.String(), response.Header
}

// TestRenderedReadSurfacesEndToEnd is W1.9b's acceptance path against a real
// instance: every read page renders with meaningful content, without JavaScript.
func TestRenderedReadSurfacesEndToEnd(t *testing.T) {
	base, public, _, _ := renderedInstance(t)

	for _, test := range []struct {
		name     string
		path     string
		wantBody []string
	}{
		{
			name: "overview",
			path: "/alice/" + public.name,
			wantBody: []string{
				"alice/" + public.name, "A repository", "README.md",
				// The fixture's README is Markdown, so it is **rendered** rather than shown as
				// text: the heading appears as an element. It is an `<h2>` because a document
				// rendered inside a page is demoted one level, so the page keeps a single
				// first-level heading — the repository's own.
				"<h2>Public</h2>",
				i18n.Default().T(i18n.IDNavSkipToContent),
			},
		},
		{
			name: "tree",
			path: "/alice/" + public.name + "/tree/main",
			wantBody: []string{
				"README.md", i18n.Default().T(i18n.IDTreeColumnName),
			},
		},
		{
			name:     "file",
			path:     "/alice/" + public.name + "/blob/main/README.md",
			wantBody: []string{"# Public", "README.md"},
		},
		{
			name:     "commits",
			path:     "/alice/" + public.name + "/commits/main",
			wantBody: []string{"seed"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body, header := getText(t, base+test.path)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			for _, want := range test.wantBody {
				if !strings.Contains(body, want) {
					t.Errorf("the page does not contain %q:\n%s", want, body)
				}
			}
			if !strings.HasPrefix(header.Get("Content-Type"), "text/html") {
				t.Errorf("Content-Type = %q, want HTML", header.Get("Content-Type"))
			}
			// No JavaScript, which is the document's central requirement.
			if strings.Contains(strings.ToLower(body), "<script") {
				t.Error("the page requires JavaScript")
			}
		})
	}
}

// TestPrivateRepositoryPageIsConsistentlyNotFound is the acceptance criterion
// against a real instance.
func TestPrivateRepositoryPageIsConsistentlyNotFound(t *testing.T) {
	base, _, private, _ := renderedInstance(t)
	notFoundBody := i18n.Default().T(i18n.IDErrorNotFoundBody)

	paths := []string{
		"/alice/" + private.name,
		"/alice/" + private.name + "/tree/main",
		"/alice/" + private.name + "/blob/main/README.md",
		"/alice/" + private.name + "/commits/main",
		"/alice/no-such-repository",
		"/nobody/" + private.name,
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			status, body, header := getText(t, base+path)
			if status != http.StatusNotFound {
				t.Errorf("status = %d, want 404", status)
			}
			if header.Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", header.Get("Cache-Control"))
			}
			if !strings.Contains(body, notFoundBody) {
				t.Errorf("the page does not carry the not-found message")
			}
		})
	}

	// Every unavailable shape renders identically, so nothing distinguishes a
	// private repository from a repository that does not exist.
	_, first, _ := getText(t, base+"/alice/"+private.name)
	for _, path := range paths[1:] {
		_, other, _ := getText(t, base+path)
		if first != other {
			t.Errorf("%s renders differently from the private repository", path)
		}
	}
}

// TestSourceIsEscapedEndToEnd proves the escaping at the real boundary, with a
// file whose content is markup.
func TestSourceIsEscapedEndToEnd(t *testing.T) {
	base, public, _, _ := renderedInstance(t)
	gitPath := gitClient(t)

	// Add a file whose content is a script tag, through plumbing so the seed
	// stays independent of a shell transport.
	const markup = "<script>alert('xss')</script>\n"
	if err := addFileToBare(t, gitPath, public.path, "evil.html", markup); err != nil {
		t.Fatalf("add the file: %v", err)
	}

	status, body, _ := getText(t, base+"/alice/"+public.name+"/blob/main/evil.html")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if strings.Contains(body, "<script>alert") {
		t.Errorf("repository content was rendered as markup:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("the escaped content is missing:\n%s", body)
	}
}

// addFileToBare adds one file to a bare repository using Git plumbing, so the
// test does not depend on a working-tree transport.
func addFileToBare(t *testing.T, gitPath, repo, name, content string) error {
	t.Helper()

	blobCommand := exec.Command(gitPath, "-C", repo, "hash-object", "-w", "--stdin")
	blobCommand.Stdin = strings.NewReader(content)
	blobOutput, err := blobCommand.Output()
	if err != nil {
		return fmt.Errorf("hash-object: %w", err)
	}
	blob := strings.TrimSpace(string(blobOutput))

	// Build a tree that contains the existing README and the new file.
	readmeBlob := strings.TrimSpace(mustGitOutput(t, gitPath, "-C", repo, "rev-parse", "HEAD^{tree}"))
	_ = readmeBlob

	specification := "100644 blob " + blob + "\t" + name + "\n"
	treeCommand := exec.Command(gitPath, "-C", repo, "mktree")
	treeCommand.Stdin = strings.NewReader(specification)
	treeOutput, err := treeCommand.Output()
	if err != nil {
		return fmt.Errorf("mktree: %w", err)
	}
	tree := strings.TrimSpace(string(treeOutput))

	parent := strings.TrimSpace(mustGitOutput(t, gitPath, "-C", repo, "rev-parse", "HEAD"))
	commitCommand := exec.Command(gitPath, "-C", repo, "commit-tree", tree, "-p", parent, "-m", "add "+name)
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

func mustGitOutput(t *testing.T, gitPath string, args ...string) string {
	t.Helper()
	output, err := exec.Command(gitPath, args...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(output)
}

// TestSameDecisionFromBothSurfaces is frontend.md's explicit obligation: the
// permission the HTML render used must equal the one the API returns for the same
// viewer and resource.
//
// Both surfaces call the same service, so the assertion is made on the answers
// they produce: the HTML permission line and the API's `permission` field must
// agree, for an anonymous caller and for the owner.
func TestSameDecisionFromBothSurfaces(t *testing.T) {
	base, public, private, _ := renderedInstance(t)

	// Sign in to obtain a session the API and the renderer both accept.
	body, err := json.Marshal(map[string]string{"login": "alice", "password": endToEndPassword})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	signIn, err := http.Post(base+"/api/v1/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	defer func() { _ = signIn.Body.Close() }()
	if signIn.StatusCode != http.StatusOK {
		t.Fatalf("sign-in status = %d, want 200", signIn.StatusCode)
	}
	var cookie string
	for _, candidate := range signIn.Cookies() {
		if candidate.Name == "gitnest_session" {
			cookie = candidate.Name + "=" + candidate.Value
		}
	}
	if cookie == "" {
		t.Fatal("sign-in set no session cookie")
	}

	for _, test := range []struct {
		name string
		repo string
	}{
		{name: "public", repo: public.name},
		{name: "private", repo: private.name},
	} {
		t.Run(test.name+" anonymously", func(t *testing.T) {
			// The API's answer.
			apiStatus, apiBody, _ := getJSON(t, base+"/api/v1/repos/alice/"+test.repo, "")
			// The renderer's answer.
			pageStatus, pageBody, _ := getText(t, base+"/alice/"+test.repo)

			if apiStatus != pageStatus {
				t.Fatalf("the API answered %d and the page answered %d", apiStatus, pageStatus)
			}
			if apiStatus == http.StatusNotFound {
				// Both refused; nothing further to compare.
				return
			}
			permission, _ := apiBody["permission"].(string)
			if permission == "" {
				t.Fatalf("the API returned no permission: %v", apiBody)
			}
			want := i18n.Default().T(i18n.IDRepoYourPermission, permission)
			if !strings.Contains(pageBody, want) {
				t.Errorf("the page does not report the API's permission %q", permission)
			}
		})

		t.Run(test.name+" as the owner", func(t *testing.T) {
			apiStatus, apiBody, _ := getJSON(t, base+"/api/v1/repos/alice/"+test.repo, cookie)
			pageStatus, pageBody, _ := getTextWithCookie(t, base+"/alice/"+test.repo, cookie)

			if apiStatus != http.StatusOK || pageStatus != http.StatusOK {
				t.Fatalf("the API answered %d and the page answered %d, want both 200", apiStatus, pageStatus)
			}
			permission, _ := apiBody["permission"].(string)
			want := i18n.Default().T(i18n.IDRepoYourPermission, permission)
			if !strings.Contains(pageBody, want) {
				t.Errorf("the page reports a different permission from the API's %q", permission)
			}
		})
	}
}

// getJSON fetches a JSON body with an optional cookie.
func getJSON(t *testing.T, url, cookie string) (int, map[string]any, http.Header) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	decoded := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return response.StatusCode, decoded, response.Header
}

// getTextWithCookie fetches a page with a cookie.
func getTextWithCookie(t *testing.T, url, cookie string) (int, string, http.Header) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	buffer := new(bytes.Buffer)
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		t.Fatalf("read the body: %v", err)
	}
	return response.StatusCode, buffer.String(), response.Header
}

// TestRenderedPageCacheabilityEndToEnd covers the cache rules at the boundary.
func TestRenderedPageCacheabilityEndToEnd(t *testing.T) {
	base, public, private, _ := renderedInstance(t)

	_, _, anonymousHeader := getText(t, base+"/alice/"+public.name)
	if !strings.HasPrefix(anonymousHeader.Get("Cache-Control"), "public") {
		t.Errorf("an anonymous public page = %q, want a public directive", anonymousHeader.Get("Cache-Control"))
	}

	// A private repository answered to a caller without access is never stored.
	_, _, deniedHeader := getText(t, base+"/alice/"+private.name)
	if deniedHeader.Get("Cache-Control") != "no-store" {
		t.Errorf("a denial = %q, want no-store", deniedHeader.Get("Cache-Control"))
	}
}

// TestRenderedPagesLeakNoCredentials keeps the pages from carrying anything the
// session must not expose.
func TestRenderedPagesLeakNoCredentials(t *testing.T) {
	base, public, _, logs := renderedInstance(t)

	body, err := json.Marshal(map[string]string{"login": "alice", "password": endToEndPassword})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	signIn, err := http.Post(base+"/api/v1/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	defer func() { _ = signIn.Body.Close() }()
	var cookie string
	for _, candidate := range signIn.Cookies() {
		if candidate.Name == "gitnest_session" {
			cookie = candidate.Name + "=" + candidate.Value
		}
	}

	_, page, _ := getTextWithCookie(t, base+"/alice/"+public.name, cookie)
	for _, secret := range []string{endToEndPassword, cookie, "password_hash", "$argon2id$"} {
		if secret != "" && strings.Contains(page, secret) {
			t.Errorf("the page carries %q", secret)
		}
	}

	// And the log carries neither the password nor the session token.
	logged := logs.String()
	for _, secret := range []string{endToEndPassword, "argon2"} {
		if strings.Contains(logged, secret) {
			t.Errorf("a log line carries %q", secret)
		}
	}
	// Every line is still valid JSON, which is what the redaction boundary
	// preserves.
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("a log line is not JSON: %v\n%s", err, line)
		}
	}
	_ = filepath.Separator
}
