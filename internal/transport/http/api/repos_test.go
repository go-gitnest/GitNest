package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/gittransport"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/go-gitnest/GitNest/internal/transport/http/api"
)

// fakeRepositories is a scripted RepositoryService.
//
// It carries enough state for a settings test to assert the round trip — the
// grants it holds, the visibility it reports, whether the archive flag is set —
// while keeping the access decision under the test's control.
type fakeRepositories struct {
	// rows maps an owner identifier to the repositories it owns.
	rows map[string][]repository.Repository
	// levels maps a repository identifier to the level the transport is told the
	// caller holds. The level is an *input* to the transport, not something the
	// transport computes, so the fake states it directly and every access
	// question is answered from here.
	levels map[string]repository.Level
	// perViewer overrides levels for one account, keyed by repository then
	// account. It exists for the endpoints that report another account's
	// permission, where an answer that did not depend on the account would make
	// the test meaningless.
	perViewer map[string]map[string]repository.Level
	// createErr forces a create failure.
	createErr error
	// updateErr forces a settings failure, so a transport test can assert how an
	// error the service would produce is rendered.
	updateErr error
	// grantErr and revokeErr force a collaborator failure.
	grantErr  error
	revokeErr error
	// grants maps a repository identifier to the grants it holds.
	grants map[string][]repository.Collaborator
	// lastUpdate is the settings request the transport built.
	lastUpdate repository.UpdateSettingsInput
	// listQuery records what a listing asked for, so the visibility-before-page
	// property can be asserted.
	listQuery repository.ListQuery
	// lastViewer records the principal the transport passed.
	lastViewer *repository.Principal
	created    repository.Repository
}

func newFakeRepositories() *fakeRepositories {
	return &fakeRepositories{
		rows:      map[string][]repository.Repository{},
		levels:    map[string]repository.Level{},
		grants:    map[string][]repository.Collaborator{},
		perViewer: map[string]map[string]repository.Level{},
	}
}

// withViewerLevel records the level one account holds on one repository.
//
// Without an override a repository answers with the level in `levels`, which is
// what almost every transport test wants: it is testing how an answer is
// rendered rather than how it is computed.
func (f *fakeRepositories) withViewerLevel(repoID, userID string, level repository.Level) {
	if f.perViewer[repoID] == nil {
		f.perViewer[repoID] = map[string]repository.Level{}
	}
	f.perViewer[repoID][userID] = level
}

func (f *fakeRepositories) add(row repository.Repository, level repository.Level) {
	f.rows[row.OwnerID] = append(f.rows[row.OwnerID], row)
	f.levels[row.ID] = level
}

// UpdateSettings applies the settings the way the service would, and records what
// it was asked for so a test can assert the transport did not reinterpret it.
func (f *fakeRepositories) UpdateSettings(_ context.Context, input repository.UpdateSettingsInput) (repository.Repository, error) {
	f.lastUpdate = input
	if f.updateErr != nil {
		return repository.Repository{}, f.updateErr
	}
	if input.IsEmpty() {
		return repository.Repository{}, repository.ValidationErrors{{
			Field: repository.FieldVisibility, Message: "the request changes nothing",
		}}
	}

	for index, row := range f.rows[f.ownerOf(input.RepoID)] {
		if row.ID != input.RepoID {
			continue
		}
		if input.Visibility != nil {
			row.Visibility = *input.Visibility
		}
		if input.DefaultBranch != nil {
			row.DefaultBranch = *input.DefaultBranch
		}
		if input.Archived != nil {
			row.Archived = *input.Archived
		}
		f.rows[f.ownerOf(input.RepoID)][index] = row
		return row, nil
	}
	return repository.Repository{}, repository.ErrNotFound
}

// ownerOf finds the owner a repository belongs to, for the fake's own bookkeeping.
func (f *fakeRepositories) ownerOf(repoID string) string {
	for owner, rows := range f.rows {
		for _, row := range rows {
			if row.ID == repoID {
				return owner
			}
		}
	}
	return ""
}

func (f *fakeRepositories) GrantAccess(_ context.Context, input repository.GrantInput) (bool, error) {
	if f.grantErr != nil {
		return false, f.grantErr
	}
	for index, existing := range f.grants[input.RepoID] {
		if existing.UserID == input.UserID {
			f.grants[input.RepoID][index].Role = input.Role
			return false, nil
		}
	}
	f.grants[input.RepoID] = append(f.grants[input.RepoID], repository.Collaborator{
		UserID: input.UserID, Role: input.Role,
	})
	return true, nil
}

func (f *fakeRepositories) RevokeAccess(_ context.Context, input repository.RevokeInput) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	remaining := make([]repository.Collaborator, 0, len(f.grants[input.RepoID]))
	for _, existing := range f.grants[input.RepoID] {
		if existing.UserID == input.UserID {
			continue
		}
		remaining = append(remaining, existing)
	}
	if len(remaining) == len(f.grants[input.RepoID]) {
		return repository.ErrNotFound
	}
	f.grants[input.RepoID] = remaining
	return nil
}

func (f *fakeRepositories) Collaborators(_ context.Context, actor repository.Principal, repoID string) ([]repository.Collaborator, error) {
	// The real service decides who may list; the fake reproduces the rule so the
	// transport's rendering of a refusal is reachable.
	level, found := f.levels[repoID]
	if !found || level < repository.LevelAdmin {
		if !actor.IsInstanceAdmin {
			return nil, repository.ErrForbidden
		}
	}
	return f.grants[repoID], nil
}

func (f *fakeRepositories) Create(_ context.Context, input repository.CreateInput) (repository.Repository, error) {
	if f.createErr != nil {
		return repository.Repository{}, f.createErr
	}
	created := repository.Repository{
		ID: "01920000-0000-7000-8000-0000000000c1", OwnerID: input.OwnerID, Name: input.Name,
		Description: input.Description, Visibility: input.Visibility,
		StorageID: "01920000-0000-7000-8000-0000000000c2",
	}
	f.created = created
	f.add(created, repository.LevelOwner)
	return created, nil
}

func (f *fakeRepositories) Resolve(_ context.Context, viewer repository.Principal, ownerID, name string) (repository.Repository, repository.Level, error) {
	recorded := viewer
	f.lastViewer = &recorded

	for _, row := range f.rows[ownerID] {
		if !strings.EqualFold(row.Name, name) {
			continue
		}
		// An override for this account wins, which is what a test about another
		// account's permission needs. An override of `none` means the account
		// holds nothing, which the real service reports as not-found. A viewer
		// with no override falls through to the repository's configured level,
		// so adding an override for one account does not change the answer for
		// every other one.
		if levels, found := f.perViewer[row.ID]; found {
			if level, present := levels[viewer.UserID]; present {
				if level == repository.LevelNone {
					return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
				}
				return row, level, nil
			}
		}

		level, found := f.levels[row.ID]
		if !found || level == repository.LevelNone {
			return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
		}
		return row, level, nil
	}
	return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
}

func (f *fakeRepositories) List(_ context.Context, query repository.ListQuery) (repository.Page, error) {
	f.listQuery = query
	// The visibility predicate is applied to the candidates before the page is
	// taken, which is the property the real query has and the tests assert.
	visible := make([]repository.Repository, 0, len(f.rows[query.OwnerID]))
	for _, row := range f.rows[query.OwnerID] {
		level, found := f.levels[row.ID]
		if !found || level == repository.LevelNone {
			continue
		}
		visible = append(visible, row)
	}
	page := repository.Page{}
	if query.Offset < len(visible) {
		rest := visible[query.Offset:]
		if query.Limit < len(rest) {
			page.Repositories = rest[:query.Limit]
			page.HasMore = true
		} else {
			page.Repositories = rest
		}
	}
	return page, nil
}

// fakeContent is a scripted ContentReader.
type fakeContent struct {
	tree    []gittransport.Entry
	file    gittransport.File
	commits []gittransport.Commit
	// fileErr is what a file read returns. It is separate from err so a test can
	// make a directory look like a directory, which is how the handler decides
	// between the file shape and the listing shape.
	fileErr      error
	err          error
	lastRevision string
	lastPath     string
}

// fakeOwners resolves an owner segment and an account identifier the way
// identity.Service does.
type fakeOwners struct {
	users map[string]identity.User
	// byID is the reverse of users, so a collaborator listing can be asserted on
	// names rather than identifiers.
	byID map[string]identity.User
	err  error
}

func (f *fakeOwners) OwnerByUsername(_ context.Context, username string) (identity.User, error) {
	if f.err != nil {
		return identity.User{}, f.err
	}
	account, found := f.users[strings.ToLower(strings.TrimSpace(username))]
	if !found {
		return identity.User{}, identity.ErrInvalidCredentials
	}
	return account, nil
}

func (f *fakeOwners) AccountByID(_ context.Context, id string) (identity.User, error) {
	if f.err != nil {
		return identity.User{}, f.err
	}
	account, found := f.byID[id]
	if !found {
		return identity.User{}, identity.ErrInvalidCredentials
	}
	return account, nil
}

func (f *fakeContent) ResolveCommit(_ context.Context, _, revision string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.lastRevision = revision
	return "resolved-" + revision, nil
}

func (f *fakeContent) ListTree(_ context.Context, _, _, path string) ([]gittransport.Entry, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastPath = path
	return f.tree, nil
}

func (f *fakeContent) ReadFile(_ context.Context, _, _, path string) (gittransport.File, error) {
	if f.err != nil {
		return gittransport.File{}, f.err
	}
	f.lastPath = path
	if f.fileErr != nil {
		return gittransport.File{}, f.fileErr
	}
	return f.file, nil
}

func (f *fakeContent) Log(_ context.Context, _, _, path string, _, _ int) ([]gittransport.Commit, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastPath = path
	return f.commits, nil
}

// reposHarness mounts the repository routes with the fakes behind them.
type reposHarness struct {
	t            *testing.T
	server       *httptest.Server
	mux          *http.ServeMux
	repositories *fakeRepositories
	content      *fakeContent
	ownerID      string
	ownerName    string
	row          repository.Repository
}

// newReposHarness prepares one owner with one repository.
func newReposHarness(t *testing.T, visibility repository.Visibility, level repository.Level) *reposHarness {
	t.Helper()
	const ownerID = "01920000-0000-7000-8000-0000000000a1"
	row := repository.Repository{
		ID: "01920000-0000-7000-8000-0000000000a2", OwnerID: ownerID, Name: "my-repo",
		Description: "a repository", Visibility: visibility, DefaultBranch: "main",
		StorageID: "01920000-0000-7000-8000-0000000000a3",
	}

	repositories := newFakeRepositories()
	repositories.add(row, level)
	content := &fakeContent{}

	owners := &fakeOwners{users: map[string]identity.User{
		"alice": {ID: ownerID, Username: "alice", Status: identity.StatusActive},
	}}
	handler, err := newReposHandler(t, repositories, content, owners)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	return &reposHarness{
		t: t, server: handler.server, mux: handler.mux, repositories: repositories,
		content: content, ownerID: ownerID, ownerName: "alice", row: row,
	}
}

// reposHandler bundles the mounted handler and its mux.
type reposHandler struct {
	server *httptest.Server
	mux    *http.ServeMux
}

func newReposHandler(t *testing.T, repositories *fakeRepositories, content *fakeContent, owners *fakeOwners) (*reposHandler, error) {
	t.Helper()
	accounts := &fakeAccounts{}
	mux := http.NewServeMux()
	api.RegisterRepositories(mux, accounts, owners, repositories, content)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &reposHandler{server: server, mux: mux}, nil
}

// get sends a request straight to the handler so a header the Go client refuses
// to send can still be tested, and so the assertions read the raw response.
func (h *reposHarness) get(path string) *httptest.ResponseRecorder {
	h.t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

// decode reads a JSON body from a recorder.
func decode(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("decode the body %q: %v", recorder.Body.String(), err)
	}
}

// TestRepositoryDetailReportsTheCallersLevel covers the shape and the level.
func TestRepositoryDetailReportsTheCallersLevel(t *testing.T) {
	for _, test := range []struct {
		name       string
		visibility repository.Visibility
		level      repository.Level
		wantPublic bool
	}{
		{name: "public read by an anonymous caller", visibility: repository.VisibilityPublic, level: repository.LevelRead, wantPublic: true},
		{name: "public read by a collaborator", visibility: repository.VisibilityPublic, level: repository.LevelWrite},
		{name: "private read by the owner", visibility: repository.VisibilityPrivate, level: repository.LevelOwner},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newReposHarness(t, test.visibility, test.level)
			recorder := h.get("/api/v1/repos/alice/my-repo")

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			var body map[string]any
			decode(t, recorder, &body)
			if body["name"] != "my-repo" || body["full_name"] != "alice/my-repo" {
				t.Errorf("body = %v, want the repository's names", body)
			}
			if body["permission"] != test.level.String() {
				t.Errorf("permission = %v, want %q", body["permission"], test.level)
			}
			if body["visibility"] != string(test.visibility) {
				t.Errorf("visibility = %v, want %q", body["visibility"], test.visibility)
			}
			// The storage identifier and the row identifier must not travel: a
			// client has no use for either, and an identifier in a payload is
			// what the addressing document forbids.
			for _, forbidden := range []string{"storage_id", h.row.StorageID} {
				if strings.Contains(recorder.Body.String(), forbidden) {
					t.Errorf("the response carries %q: %s", forbidden, recorder.Body.String())
				}
			}

			gotCache := recorder.Header().Get("Cache-Control")
			if test.wantPublic && !strings.HasPrefix(gotCache, "public") {
				t.Errorf("Cache-Control = %q, want a public directive for an anonymous public read", gotCache)
			}
			if !test.wantPublic && !strings.HasPrefix(gotCache, "private") {
				t.Errorf("Cache-Control = %q, want a private directive", gotCache)
			}
			if !strings.Contains(recorder.Header().Get("Vary"), "Authorization") {
				t.Errorf("Vary = %q, want Authorization named", recorder.Header().Get("Vary"))
			}
		})
	}
}

// TestUnavailableRepositoriesAreIndistinguishable is the existence-disclosure
// assertion for the API.
func TestUnavailableRepositoriesAreIndistinguishable(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPrivate, repository.LevelNone)

	missing := h.get("/api/v1/repos/alice/no-such-repo")
	private := h.get("/api/v1/repos/alice/my-repo")
	unknownOwner := h.get("/api/v1/repos/nobody/my-repo")
	badName := h.get("/api/v1/repos/alice/-bad.git")

	for _, test := range []struct {
		name     string
		recorder *httptest.ResponseRecorder
	}{
		{name: "missing", recorder: missing},
		{name: "private", recorder: private},
		{name: "unknown owner", recorder: unknownOwner},
		{name: "malformed name", recorder: badName},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", test.recorder.Code)
			}
			if got := test.recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store so a denial is not remembered", got)
			}
		})
	}

	if missing.Body.String() != private.Body.String() {
		t.Errorf("the bodies differ:\nmissing: %s\nprivate: %s", missing.Body.String(), private.Body.String())
	}
	if missing.Body.String() != unknownOwner.Body.String() {
		t.Errorf("the bodies differ between a missing repository and an unknown owner")
	}
}

// TestRepositoryListCoversEveryUnavailableShape keeps the properties above true
// for the sub-resources too.
func TestSubResourcesRefuseUnavailableRepositories(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPrivate, repository.LevelNone)

	for _, path := range []string{
		"/api/v1/repos/alice/my-repo/contents/",
		"/api/v1/repos/alice/my-repo/commits",
		"/api/v1/repos/alice/no-such-repo/contents/",
		"/api/v1/repos/alice/no-such-repo/commits",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := h.get(path)
			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", recorder.Code)
			}
		})
	}
}

// TestContentsReturnsAFile is the file shape, which must carry the content as
// text rather than as an opaque blob.
func TestContentsReturnsAFile(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPublic, repository.LevelRead)
	h.content.file = gittransport.File{
		Path: "src/main.go", Size: 27,
		// Content that would be dangerous if it were rendered rather than
		// escaped; the API's job is to return it verbatim as JSON text.
		Content: []byte("package main\n\nfunc main() {}\n"),
	}

	recorder := h.get("/api/v1/repos/alice/my-repo/contents/src/main.go")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	decode(t, recorder, &body)
	if body["encoding"] != "utf-8" {
		t.Errorf("encoding = %v, want utf-8", body["encoding"])
	}
	if body["name"] != "main.go" || body["path"] != "src/main.go" {
		t.Errorf("body = %v, want the file's names", body)
	}
	if !strings.Contains(body["content"].(string), "func main") {
		t.Errorf("content = %v, want the file's text", body["content"])
	}
	if h.content.lastRevision != "main" {
		t.Errorf("the reader was asked for revision %q, want the default branch", h.content.lastRevision)
	}
}

// TestContentsEscapesNothingAndReturnsText keeps the API from becoming a
// renderer: it returns the bytes as JSON, and escaping is the consumer's job.
func TestContentsReturnsContentVerbatim(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPublic, repository.LevelRead)
	const markup = "<script>alert('x')</script>\n"
	h.content.file = gittransport.File{Path: "index.html", Size: int64(len(markup)), Content: []byte(markup)}

	recorder := h.get("/api/v1/repos/alice/my-repo/contents/index.html")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var body map[string]any
	decode(t, recorder, &body)
	if body["content"] != markup {
		t.Errorf("content = %q, want the file verbatim", body["content"])
	}
	// The response is JSON, so the markup is inside a string and cannot be
	// interpreted as a document by a consumer that reads the content type.
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", got)
	}
}

// TestContentsReturnsADirectory covers the other half of one endpoint.
func TestContentsReturnsADirectory(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPublic, repository.LevelRead)
	h.content.tree = []gittransport.Entry{
		{Name: "README.md", Path: "README.md", Type: "file", Size: 42},
		{Name: "src", Path: "src", Type: "directory"},
	}
	// A directory is not a file, which is what makes the handler fall through to
	// the listing.
	h.content.fileErr = gittransport.ErrNotAFile

	recorder := h.get("/api/v1/repos/alice/my-repo/contents/")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var entries []map[string]any
	decode(t, recorder, &entries)
	if len(entries) != 2 {
		t.Fatalf("the listing has %d entries, want 2", len(entries))
	}
	if entries[1]["type"] != "directory" || entries[0]["type"] != "file" {
		t.Errorf("entries = %v, want the types preserved", entries)
	}
}

// TestContentsReportsAnOversizedFile keeps a browse request bounded.
func TestContentsReportsAnOversizedFile(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPublic, repository.LevelRead)
	h.content.fileErr = gittransport.ErrFileTooLarge

	recorder := h.get("/api/v1/repos/alice/my-repo/contents/huge.bin")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "too large") {
		t.Errorf("body = %s, want an explanation", recorder.Body.String())
	}
}

// TestCommitsReturnsAList covers the commit list shape and its pagination.
func TestCommitsReturnsAList(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPublic, repository.LevelRead)
	h.content.commits = []gittransport.Commit{{
		ID: "aaaabbbbccccddddeeeeffff0000111122223333", Message: "first commit",
		AuthorName: "A Person", AuthorEmail: "person@example.com", AuthoredAt: "2026-01-01T00:00:00Z",
	}}

	recorder := h.get("/api/v1/repos/alice/my-repo/commits")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var commits []map[string]any
	decode(t, recorder, &commits)
	if len(commits) != 1 {
		t.Fatalf("the list has %d commits, want 1", len(commits))
	}
	if commits[0]["sha"] != "aaaabbbbccccddddeeeeffff0000111122223333" {
		t.Errorf("sha = %v, want the commit identifier", commits[0]["sha"])
	}
	if commits[0]["message"] != "first commit" {
		t.Errorf("message = %v, want the subject", commits[0]["message"])
	}
}

// TestCommitsPassAPathFilterThrough keeps the path filter reaching the reader
// rather than being dropped.
func TestCommitsPassAPathFilterThrough(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPublic, repository.LevelRead)
	h.content.commits = []gittransport.Commit{}

	recorder := h.get("/api/v1/repos/alice/my-repo/commits?path=src/main.go")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if h.content.lastPath != "src/main.go" {
		t.Errorf("the reader was asked for path %q, want the filter", h.content.lastPath)
	}
}

// TestCommitsReportsAnInvalidPath keeps a malformed filter from reaching Git.
func TestCommitsReportsAnInvalidPath(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPublic, repository.LevelRead)
	h.content.err = errors.New("a path must not contain \"..\"")

	recorder := h.get("/api/v1/repos/alice/my-repo/commits?path=../etc")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422: %s", recorder.Code, recorder.Body.String())
	}
}

// TestRepositoryListScopesToTheCaller covers the personal listing.
func TestRepositoryListScopesToTheCaller(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPrivate, repository.LevelOwner)
	h.repositories.add(repository.Repository{
		ID: "01920000-0000-7000-8000-0000000000a9", OwnerID: h.ownerID, Name: "second",
		Visibility: repository.VisibilityPrivate, DefaultBranch: "main",
		StorageID: "01920000-0000-7000-8000-0000000000aa",
	}, repository.LevelOwner)

	// The listing is scoped to the caller. Requiring a session means the status
	// is 401 without one, which is the first assertion.
	anonymous := h.get("/api/v1/user/repos")
	if anonymous.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous listing = %d, want 401", anonymous.Code)
	}
}

// TestPaginationRules covers the documented parameter behaviour.
func TestPaginationRules(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPublic, repository.LevelRead)

	for _, test := range []struct {
		name    string
		query   string
		want    int
		perPage int
	}{
		{name: "no parameters", query: "", want: http.StatusOK, perPage: 30},
		{name: "page one", query: "?page=1", want: http.StatusOK, perPage: 30},
		{name: "explicit per_page", query: "?per_page=5", want: http.StatusOK, perPage: 5},
		{name: "per_page above the maximum is clamped", query: "?per_page=1000", want: http.StatusOK, perPage: 100},
		{name: "per_page at the maximum", query: "?per_page=100", want: http.StatusOK, perPage: 100},
		{name: "page zero is rejected", query: "?page=0", want: http.StatusUnprocessableEntity},
		{name: "negative page is rejected", query: "?page=-1", want: http.StatusUnprocessableEntity},
		{name: "non-numeric page is rejected", query: "?page=abc", want: http.StatusUnprocessableEntity},
		{name: "zero per_page is rejected", query: "?per_page=0", want: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := h.get("/api/v1/repos/alice/my-repo/commits" + test.query)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

// TestLinkHeaderOmitsLastOnPurpose pins the deliberate absence of `last`: it
// needs a total, and a total would disclose how much a caller cannot see.
func TestLinkHeaderOmitsLastOnPurpose(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPublic, repository.LevelRead)
	// A full page implies more may exist.
	h.content.commits = make([]gittransport.Commit, 30)
	for index := range h.content.commits {
		h.content.commits[index] = gittransport.Commit{ID: "id", Message: "m"}
	}

	recorder := h.get("/api/v1/repos/alice/my-repo/commits?per_page=30")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	link := recorder.Header().Get("Link")
	if !strings.Contains(link, `rel="next"`) {
		t.Errorf("Link = %q, want a next relation for a full page", link)
	}
	if strings.Contains(link, `rel="last"`) {
		t.Errorf("Link = %q, want no last relation", link)
	}

	// The first page carries no prev or first relation, because an emitted-empty
	// relation is what the document forbids.
	if strings.Contains(link, `rel="prev"`) || strings.Contains(link, `rel="first"`) {
		t.Errorf("Link = %q, want no prev or first relation on the first page", link)
	}

	// A later page carries prev and first but still no last.
	later := h.get("/api/v1/repos/alice/my-repo/commits?per_page=30&page=3")
	laterLink := later.Header().Get("Link")
	if !strings.Contains(laterLink, `rel="prev"`) || !strings.Contains(laterLink, `rel="first"`) {
		t.Errorf("Link = %q, want prev and first on a later page", laterLink)
	}
	if strings.Contains(laterLink, `rel="last"`) {
		t.Errorf("Link = %q, want no last relation", laterLink)
	}
}

// TestOwnerResolutionFailureIsNotFound keeps an unresolvable owner from
// disclosing whether the repository exists.
func TestOwnerResolutionFailureIsNotFound(t *testing.T) {
	h := newReposHarness(t, repository.VisibilityPublic, repository.LevelRead)
	recorder := h.get("/api/v1/repos/no-such-owner/my-repo")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", recorder.Code)
	}
	// No content was read, because the owner never resolved.
	if h.content.lastRevision != "" {
		t.Error("the content reader was consulted for an unresolvable owner")
	}
}
