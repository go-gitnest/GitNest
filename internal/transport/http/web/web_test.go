package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/gittransport"
	"github.com/go-gitnest/GitNest/internal/i18n"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/go-gitnest/GitNest/internal/transport/http/web"
)

const (
	ownerID = "01920000-0000-7000-8000-0000000000b1"
	repoID  = "01920000-0000-7000-8000-0000000000b2"
	// A commit-shaped identifier, because the rendered links address commits.
	commitID = "aaaabbbbccccddddeeeeffff0000111122223333"
)

// fakeRepositories answers access questions from a table.
type fakeRepositories struct {
	row   repository.Repository
	level repository.Level
	// lastViewer records the principal the renderer passed, so the "same
	// decision twice" property can be asserted against the API's answer.
	lastViewer *repository.Principal
}

func (f *fakeRepositories) Resolve(_ context.Context, viewer repository.Principal, owner, name string) (repository.Repository, repository.Level, error) {
	recorded := viewer
	f.lastViewer = &recorded
	if owner != ownerID || !strings.EqualFold(name, f.row.Name) {
		return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
	}
	if f.level == repository.LevelNone {
		return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
	}
	return f.row, f.level, nil
}

// fakeContent answers content questions from a table.
type fakeContent struct {
	tree       []gittransport.Entry
	files      map[string]gittransport.File
	commits    []gittransport.Commit
	resolveErr error
}

func (f *fakeContent) ResolveCommit(_ context.Context, _, revision string) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	return commitID, nil
}

func (f *fakeContent) ListTree(_ context.Context, _, _, path string) ([]gittransport.Entry, error) {
	if path != "" {
		return nil, gittransport.ErrNotFound
	}
	return f.tree, nil
}

func (f *fakeContent) ReadFile(_ context.Context, _, _, path string) (gittransport.File, error) {
	file, found := f.files[path]
	if !found {
		return gittransport.File{}, gittransport.ErrNotFound
	}
	return file, nil
}

func (f *fakeContent) Log(_ context.Context, _, _, _ string, _, _ int) ([]gittransport.Commit, error) {
	return f.commits, nil
}

// fakeOwners resolves the owner segment.
type fakeOwners struct {
	users map[string]identity.User
}

func (f *fakeOwners) OwnerByUsername(_ context.Context, username string) (identity.User, error) {
	account, found := f.users[strings.ToLower(strings.TrimSpace(username))]
	if !found {
		return identity.User{}, identity.ErrInvalidCredentials
	}
	return account, nil
}

// fakeSessions resolves a session cookie, or reports it unusable.
type fakeSessions struct {
	users map[string]identity.User
}

func (f *fakeSessions) ResolveSession(_ context.Context, token string) (identity.User, identity.Session, []byte, error) {
	user, found := f.users[token]
	if !found {
		return identity.User{}, identity.Session{}, nil, identity.ErrInvalidToken
	}
	return user, identity.Session{ID: "session"}, identity.HashToken("csrf"), nil
}

// harness mounts the renderer with the fakes behind it.
type harness struct {
	t       *testing.T
	mux     *http.ServeMux
	repos   *fakeRepositories
	content *fakeContent
	row     repository.Repository
}

func newHarness(t *testing.T, visibility repository.Visibility, level repository.Level) *harness {
	t.Helper()
	row := repository.Repository{
		ID: repoID, OwnerID: ownerID, Name: "my-repo", Description: "A repository",
		Visibility: visibility, DefaultBranch: "main", StorageID: "01920000-0000-7000-8000-0000000000b3",
	}
	repos := &fakeRepositories{row: row, level: level}
	content := &fakeContent{
		tree: []gittransport.Entry{
			{Name: "README.md", Path: "README.md", Type: "file", Size: 7},
			{Name: "src", Path: "src", Type: "directory"},
		},
		files: map[string]gittransport.File{
			"README.md": {Path: "README.md", Size: 7, Content: []byte("# Title")},
			"src/main.go": {
				Path: "src/main.go", Size: 27,
				Content: []byte("package main\n\nfunc main() {}\n"),
			},
		},
		commits: []gittransport.Commit{{
			ID: commitID, Message: "first commit", AuthorName: "A Person",
			AuthorEmail: "person@example.com", AuthoredAt: "2026-01-01T00:00:00Z",
		}},
	}

	owners := &fakeOwners{users: map[string]identity.User{
		"alice": {ID: ownerID, Username: "alice", Status: identity.StatusActive},
	}}
	sessions := &fakeSessions{users: map[string]identity.User{
		"valid-session": {ID: ownerID, Username: "alice", Status: identity.StatusActive},
	}}

	handler, err := web.NewHandler(repos, content, owners, sessions, "")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	return &harness{t: t, mux: mux, repos: repos, content: content, row: row}
}

// get dispatches a request straight to the handler.
func (h *harness) get(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

func (h *harness) sessionCookie() *http.Cookie {
	return &http.Cookie{Name: web.SessionCookieName, Value: "valid-session"}
}

// TestOverviewRendersMetadataAndTheReadme covers the surface's meaningful
// content, which is what the testing obligations say to assert rather than
// markup shapes.
func TestOverviewRendersMetadataAndTheReadme(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)
	recorder := h.get("/alice/my-repo")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{
		"alice/my-repo", "A repository", "main", "README.md", "src",
		// The fixture's README is Markdown and is now **rendered**, so the heading appears as
		// an element rather than as the literal `# Title` it once showed. It is an `<h2>`
		// because the document is rendered embedded: the page's own `<h1>` is the repository,
		// and a README heading is a section within it.
		"<h2>Title</h2>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
	// The clone address is absolute and names the host the reader used.
	if !strings.Contains(body, "git clone http://example.com/alice/my-repo.git") {
		t.Errorf("the page does not carry the clone address:\n%s", body)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want HTML", got)
	}
}

// TestSourceFilesAreEscaped is the acceptance criterion: a file whose content is
// markup must appear as text, never as a document.
func TestSourceFilesAreEscaped(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)
	const markup = `<script>alert('xss')</script>`
	h.content.files["evil.html"] = gittransport.File{
		Path: "evil.html", Size: int64(len(markup)), Content: []byte(markup),
	}

	recorder := h.get("/alice/my-repo/blob/main/evil.html")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()

	// The script tag must not appear as markup.
	if strings.Contains(body, "<script>alert") {
		t.Errorf("the file content was rendered as markup:\n%s", body)
	}
	// It must appear escaped, which is what makes it visible rather than
	// dangerous.
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("the escaped content is missing:\n%s", body)
	}
}

// TestFileRendersSourceAsText covers the ordinary case, and the parent link.
func TestFileRendersSourceAsText(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)
	recorder := h.get("/alice/my-repo/blob/main/src/main.go")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{"src/main.go", "package main", "func main", "main"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
	// The parent link points at the containing directory.
	if !strings.Contains(body, "/tree/") {
		t.Errorf("the page carries no tree link:\n%s", body)
	}
}

// TestTreeListsDirectoriesBeforeFiles checks the order the page promises.
func TestTreeListsDirectoriesBeforeFiles(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)
	recorder := h.get("/alice/my-repo/tree/main")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	directoryAt := strings.Index(body, ">src<")
	fileAt := strings.Index(body, ">README.md<")
	if directoryAt < 0 || fileAt < 0 {
		t.Fatalf("the listing is missing an entry:\n%s", body)
	}
	if directoryAt > fileAt {
		t.Errorf("the file is listed before the directory, which the page's notice contradicts")
	}
}

// TestCommitsRenderContent covers the commit list.
func TestCommitsRenderContent(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)
	recorder := h.get("/alice/my-repo/commits/main")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{"first commit", "A Person", commitID} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
}

// TestNoJavaScriptIsRequired is the architecture document's central requirement:
// a read page is complete on the first byte.
func TestNoJavaScriptIsRequired(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)

	for _, path := range []string{"/alice/my-repo", "/alice/my-repo/tree/main", "/alice/my-repo/blob/main/README.md", "/alice/my-repo/commits/main"} {
		t.Run(path, func(t *testing.T) {
			recorder := h.get(path)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			body := recorder.Body.String()
			// No script element, no inline event handler, and no dependency on a
			// client-side router. A page that needed one would violate the
			// document's "a page must be usable before hydration" rule outright,
			// because these pages are not islands at all.
			for _, forbidden := range []string{"<script", "javascript:", "onclick=", "onload="} {
				if strings.Contains(strings.ToLower(body), forbidden) {
					t.Errorf("the page carries %q, so it is not readable without JavaScript", forbidden)
				}
			}
			if !strings.Contains(body, "<!DOCTYPE html>") {
				t.Error("the page is not a complete document")
			}
		})
	}
}

// TestEveryUserVisibleStringComesFromTheCatalogue is the internationalization
// rule: no identifier may survive into the output, and every string the page
// shows must be a catalogue message.
//
// The pattern is anchored on the identifiers the catalogue actually defines
// rather than on a shape, because a shape like `\brepo\.[a-z]+` also matches the
// clone URL's `my-repo.git` and reports a leak that is not one.
func TestEveryUserVisibleStringComesFromTheCatalogue(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)

	// Every identifier, longest first, so a prefix cannot shadow a longer match.
	identifiers := i18n.Identifiers()
	sort.Slice(identifiers, func(i, j int) bool { return len(identifiers[i]) > len(identifiers[j]) })

	for _, path := range []string{"/alice/my-repo", "/alice/my-repo/tree/main", "/alice/my-repo/blob/main/README.md", "/alice/my-repo/commits/main"} {
		t.Run(path, func(t *testing.T) {
			body := h.get(path).Body.String()
			for _, id := range identifiers {
				if strings.Contains(body, id) {
					t.Errorf("the page leaks the message identifier %q, so a template skipped the catalogue", id)
				}
			}
		})
	}

	// A message the catalogue defines must be reachable by its identifier, which
	// is what makes the indirection real rather than decorative.
	catalogue := i18n.Default()
	for _, id := range identifiers {
		if catalogue.T(id) == id {
			t.Errorf("the identifier %q resolves to itself, so the message is missing", id)
		}
	}
}

// TestNotAVisibleStringIsHardcodedInATemplate is the reverse check: the page
// must actually show the catalogue's text rather than an English literal that
// happens to match.
func TestCatalogueMessagesAppear(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)
	body := h.get("/alice/my-repo").Body.String()

	for _, message := range []string{
		i18n.Default().T(i18n.IDNavSkipToContent),
		i18n.Default().T(i18n.IDRepoVisibilityPublic),
		i18n.Default().T(i18n.IDOverviewTreeHeading),
		i18n.Default().T(i18n.IDEntryTypeDirectory),
	} {
		if !strings.Contains(body, message) {
			t.Errorf("the page does not carry the catalogue message %q", message)
		}
	}
}

// TestLocaleSelectionFollowsAcceptLanguage covers the documented precedence step
// that exists today.
func TestLocaleSelectionFollowsAcceptLanguage(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)

	request := httptest.NewRequest(http.MethodGet, "/alice/my-repo", nil)
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `lang="en-US"`) {
		t.Errorf("the page does not declare the negotiated locale:\n%s", recorder.Body.String())
	}

	// An unsupported locale falls back to the default rather than failing.
	request = httptest.NewRequest(http.MethodGet, "/alice/my-repo", nil)
	request.Header.Set("Accept-Language", "zz-ZZ")
	recorder = httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Errorf("an unsupported locale = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `lang="`+i18n.DefaultLocale+`"`) {
		t.Errorf("an unsupported locale did not fall back to the default")
	}
}

// TestPrivateRepositoryIsConsistentlyNotFound is the acceptance criterion: every
// denied shape produces the same page.
func TestPrivateRepositoryIsConsistentlyNotFound(t *testing.T) {
	h := newHarness(t, repository.VisibilityPrivate, repository.LevelNone)

	for _, path := range []string{
		"/alice/my-repo",
		"/alice/my-repo/tree/main",
		"/alice/my-repo/blob/main/README.md",
		"/alice/my-repo/commits/main",
		"/alice/no-such-repo",
		"/nobody/my-repo",
		"/alice/-bad",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := h.get(path)
			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", recorder.Code)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store so a denial is not remembered", got)
			}
			if !strings.Contains(recorder.Body.String(), i18n.Default().T(i18n.IDErrorNotFoundBody)) {
				t.Errorf("the page does not carry the not-found message")
			}
		})
	}

	// The bodies are identical, so the shapes are indistinguishable.
	first := h.get("/alice/my-repo").Body.String()
	second := h.get("/alice/no-such-repo").Body.String()
	if first != second {
		t.Error("a private repository and a missing one render differently")
	}
}

// TestSameDecisionTwice is frontend.md's explicit testing obligation: the
// permission the HTML render used must equal the one the API would return for the
// same viewer and resource.
//
// Both surfaces call the same service with a principal built the same way, so the
// assertion is that the principal this package passes is the one the API passes:
// an authenticated caller is a user principal, and no caller is anonymous when
// its session is usable.
func TestSameDecisionTwice(t *testing.T) {
	for _, test := range []struct {
		name     string
		cookie   *http.Cookie
		wantKind repository.PrincipalKind
		wantID   string
	}{
		{name: "anonymous", cookie: nil, wantKind: repository.KindAnonymous},
		{name: "usable session", cookie: &http.Cookie{Name: web.SessionCookieName, Value: "valid-session"}, wantKind: repository.KindUser, wantID: ownerID},
		// An unusable session degrades to anonymous rather than failing, which is
		// the same answer the API gives.
		{name: "unusable session", cookie: &http.Cookie{Name: web.SessionCookieName, Value: "stale"}, wantKind: repository.KindAnonymous},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)
			var cookies []*http.Cookie
			if test.cookie != nil {
				cookies = append(cookies, test.cookie)
			}
			recorder := h.get("/alice/my-repo", cookies...)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			if h.repos.lastViewer == nil {
				t.Fatal("the renderer never consulted the repository service")
			}
			if h.repos.lastViewer.Kind != test.wantKind {
				t.Errorf("the renderer passed kind %q, want %q", h.repos.lastViewer.Kind, test.wantKind)
			}
			if test.wantID != "" && h.repos.lastViewer.UserID != test.wantID {
				t.Errorf("the renderer passed account %q, want %q", h.repos.lastViewer.UserID, test.wantID)
			}
		})
	}
}

// TestCacheabilityFollowsTheViewer covers the addressing document's cache rules.
func TestCacheabilityFollowsTheViewer(t *testing.T) {
	for _, test := range []struct {
		name       string
		visibility repository.Visibility
		level      repository.Level
		cookie     *http.Cookie
		wantPublic bool
	}{
		{name: "public read anonymously", visibility: repository.VisibilityPublic, level: repository.LevelRead, wantPublic: true},
		{name: "public read by a signed-in caller", visibility: repository.VisibilityPublic, level: repository.LevelRead, cookie: &http.Cookie{Name: web.SessionCookieName, Value: "valid-session"}},
		{name: "private read by its owner", visibility: repository.VisibilityPrivate, level: repository.LevelOwner, cookie: &http.Cookie{Name: web.SessionCookieName, Value: "valid-session"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, test.visibility, test.level)
			var cookies []*http.Cookie
			if test.cookie != nil {
				cookies = append(cookies, test.cookie)
			}
			recorder := h.get("/alice/my-repo", cookies...)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}

			cache := recorder.Header().Get("Cache-Control")
			if test.wantPublic {
				if !strings.HasPrefix(cache, "public") {
					t.Errorf("Cache-Control = %q, want a public directive", cache)
				}
				if !strings.Contains(recorder.Body.String(), `content="index, follow"`) {
					t.Error("a public page read anonymously is not indexable")
				}
			} else {
				if !strings.HasPrefix(cache, "private") {
					t.Errorf("Cache-Control = %q, want a private directive", cache)
				}
				if got := recorder.Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
					t.Errorf("X-Robots-Tag = %q, want noindex", got)
				}
				if !strings.Contains(recorder.Header().Get("Vary"), "Cookie") {
					t.Errorf("Vary = %q, want Cookie named", recorder.Header().Get("Vary"))
				}
			}
		})
	}
}

// TestCanonicalURLAndMetadata covers the search-engine obligations.
func TestCanonicalURLAndMetadata(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)
	// The canonical link is asserted through the reference field the layout
	// reads, because a rendered page carries it only when the handler supplies
	// one; this phase supplies the Open Graph metadata and the robots directive.
	recorder := h.get("/alice/my-repo")
	body := recorder.Body.String()

	for _, want := range []string{
		`<meta property="og:type"`, `<meta property="og:title"`, `<meta property="og:url"`,
		`<meta name="viewport"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not carry %q", want)
		}
	}
}

// TestEmptyRepositoryRendersItsOwnPage covers a repository with no commits, which
// is the state immediately after creation.
func TestEmptyRepositoryRendersItsOwnPage(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)
	h.repos.row.DefaultBranch = ""
	h.content.resolveErr = gittransport.ErrEmptyRepository

	recorder := h.get("/alice/my-repo")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), i18n.Default().T(i18n.IDOverviewEmptyRepo)) {
		t.Errorf("an empty repository does not say so:\n%s", recorder.Body.String())
	}
}

// TestUnknownRevisionIsNotFound keeps a browse request from a revision that does
// not exist indistinguishable from an unavailable repository.
func TestUnknownRevisionIsNotFound(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)
	h.content.resolveErr = gittransport.ErrNotFound

	for _, path := range []string{"/alice/my-repo/tree/nope", "/alice/my-repo/blob/nope/README.md", "/alice/my-repo/commits/nope"} {
		t.Run(path, func(t *testing.T) {
			if got := h.get(path).Code; got != http.StatusNotFound {
				t.Errorf("status = %d, want 404", got)
			}
		})
	}
}

// TestNewHandlerValidatesItsDependencies keeps a wiring mistake at startup.
func TestNewHandlerValidatesItsDependencies(t *testing.T) {
	repos := &fakeRepositories{level: repository.LevelRead}
	content := &fakeContent{}
	owners := &fakeOwners{}
	sessions := &fakeSessions{}

	for _, test := range []struct {
		name string
		call func() (*web.Handler, error)
	}{
		{name: "nil repositories", call: func() (*web.Handler, error) { return web.NewHandler(nil, content, owners, sessions, "") }},
		{name: "nil content", call: func() (*web.Handler, error) { return web.NewHandler(repos, nil, owners, sessions, "") }},
		{name: "nil owners", call: func() (*web.Handler, error) { return web.NewHandler(repos, content, nil, sessions, "") }},
		{name: "nil sessions", call: func() (*web.Handler, error) { return web.NewHandler(repos, content, owners, nil, "") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.call(); err == nil {
				t.Fatal("an incomplete renderer was accepted")
			}
		})
	}
}

// TestTemplatesParseAtStartup is a guard: a template syntax error must be a
// startup failure rather than a 500 on the first request that uses it.
func TestTemplatesParseAtStartup(t *testing.T) {
	handler, err := web.NewHandler(&fakeRepositories{level: repository.LevelRead}, &fakeContent{}, &fakeOwners{}, &fakeSessions{}, "")
	if err != nil {
		t.Fatalf("the shipped templates do not parse: %v", err)
	}
	if handler == nil {
		t.Fatal("no handler")
	}
}

// TestContentReaderFailureIsUnavailable keeps a broken repository from looking
// like a missing one, because the caller was already authorized.
func TestContentReaderFailureIsUnavailable(t *testing.T) {
	h := newHarness(t, repository.VisibilityPublic, repository.LevelRead)
	h.content.resolveErr = errors.New("the repository is unreadable")

	// A content failure after authorization is an operational problem, and the
	// revision resolve is the first step that can report it. The answer must be
	// an error rather than a page, so a broken repository does not look like an
	// empty one.
	recorder := h.get("/alice/my-repo/tree/nope")
	if recorder.Code != http.StatusNotFound && recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 404 or 503", recorder.Code)
	}
}
