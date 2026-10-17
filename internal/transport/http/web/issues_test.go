package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/go-gitnest/GitNest/internal/transport/http/web"
)

// The issue read surfaces.
//
// The rules behind them are asserted in `internal/issues` and the full stack in `internal/app`,
// so these tests are about what a page renders and — importantly — that a page for a repository
// the caller may not read is the same not-found page a nonexistent repository gets.

// fakeIssueService serves one issue and one page of them.
type fakeIssueService struct {
	issue issues.Issue
	list  []issues.Issue

	getErr  error
	listErr error
	// mutateErr is what every mutation reports, so a refusal path can be asserted without a
	// different fake per operation.
	mutateErr error

	created  *issues.CreateInput
	edited   *issues.EditInput
	closed   *issues.CloseInput
	reopened *issues.ReopenInput
	deleted  *issues.DeleteInput
	// asked records the last call, so a test can assert that the page asked the shared service
	// rather than reaching for a store of its own.
	getInput *issues.GetInput
}

func (f *fakeIssueService) Get(_ context.Context, input issues.GetInput) (issues.Issue, error) {
	f.getInput = &input
	if f.getErr != nil {
		return issues.Issue{}, f.getErr
	}
	if input.Number != f.issue.Number {
		return issues.Issue{}, issues.ErrNotFound
	}
	return f.issue, nil
}

func (f *fakeIssueService) Create(_ context.Context, input issues.CreateInput) (issues.Issue, error) {
	f.created = &input
	if f.mutateErr != nil {
		return issues.Issue{}, f.mutateErr
	}
	return f.issue, nil
}

func (f *fakeIssueService) Edit(_ context.Context, input issues.EditInput) (issues.Issue, error) {
	f.edited = &input
	if f.mutateErr != nil {
		return issues.Issue{}, f.mutateErr
	}
	updated := f.issue
	if input.Title != nil {
		updated.Title = *input.Title
	}
	if input.Body != nil {
		updated.Body = *input.Body
	}
	updated.Version++
	return updated, nil
}

func (f *fakeIssueService) Close(_ context.Context, input issues.CloseInput) (issues.Issue, error) {
	f.closed = &input
	if f.mutateErr != nil {
		return issues.Issue{}, f.mutateErr
	}
	closed := f.issue
	closed.State = issues.StateClosed
	return closed, nil
}

func (f *fakeIssueService) Reopen(_ context.Context, input issues.ReopenInput) (issues.Issue, error) {
	f.reopened = &input
	if f.mutateErr != nil {
		return issues.Issue{}, f.mutateErr
	}
	reopened := f.issue
	reopened.State = issues.StateOpen
	return reopened, nil
}

func (f *fakeIssueService) Delete(_ context.Context, input issues.DeleteInput) error {
	f.deleted = &input
	return f.mutateErr
}

func (f *fakeIssueService) List(_ context.Context, _ issues.ListInput) (issues.Page, error) {
	if f.listErr != nil {
		return issues.Page{}, f.listErr
	}
	return issues.Page{Issues: f.list}, nil
}

// fakeAccountLookup resolves the actor an audit record names.
type fakeAccountLookup struct{}

func (fakeAccountLookup) AccountByID(_ context.Context, id string) (identity.User, error) {
	if id == ownerID {
		return identity.User{ID: ownerID, Username: "alice", Status: identity.StatusActive}, nil
	}
	return identity.User{}, identity.ErrInvalidCredentials
}

// issueHarness mounts the renderer with the issue surface behind it.
type issueHarness struct {
	t       *testing.T
	mux     *http.ServeMux
	service *fakeIssueService
}

// newIssueHarness prepares a public repository with one open issue.
func newIssueHarness(t *testing.T, level repository.Level) *issueHarness {
	t.Helper()
	row := repository.Repository{
		ID: repoID, OwnerID: ownerID, Name: "my-repo", Description: "A repository",
		Visibility: repository.VisibilityPublic, DefaultBranch: "main",
		StorageID: "01920000-0000-7000-8000-0000000000b3",
	}
	repos := &fakeRepositories{row: row, level: level}
	content := &fakeContent{}
	owners := &fakeOwners{users: map[string]identity.User{
		"alice": {ID: ownerID, Username: "alice", Status: identity.StatusActive},
	}}
	sessions := &fakeSessions{users: map[string]identity.User{}}

	service := &fakeIssueService{
		issue: issues.Issue{
			ID: "01920000-0000-7000-8000-0000000000c1", RepositoryID: repoID, Number: 1,
			Title: "The first issue", Body: "# A heading\n\nWith **bold** text.",
			AuthorID: ownerID, AuthorName: "alice", State: issues.StateOpen,
			CreatedAt: time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC),
			Version:   1,
		},
		list: []issues.Issue{
			{
				ID: "01920000-0000-7000-8000-0000000000c1", RepositoryID: repoID, Number: 1,
				Title: "The first issue", AuthorID: ownerID, AuthorName: "alice",
				State: issues.StateOpen, Version: 1,
			},
			{
				ID: "01920000-0000-7000-8000-0000000000c2", RepositoryID: repoID, Number: 2,
				Title: "A closed one", AuthorID: ownerID, AuthorName: "bob",
				State: issues.StateClosed, Version: 3,
			},
		},
	}

	handler, err := web.NewHandler(repos, content, owners, sessions, "")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	handler = handler.WithIssues(service).WithAccounts(fakeAccountLookup{})

	mux := http.NewServeMux()
	handler.Register(mux)
	return &issueHarness{t: t, mux: mux, service: service}
}

// get dispatches a request.
func (h *issueHarness) get(path string) *httptest.ResponseRecorder {
	h.t.Helper()
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

// TestTheIssueListRendersTheIssues covers the list page.
func TestTheIssueListRendersTheIssues(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	recorder := h.get("/alice/my-repo/issues")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	for _, want := range []string{
		"The first issue", "A closed one",
		"/alice/my-repo/issues/1", "/alice/my-repo/issues/2",
	} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
	// The issue numbers are addressable, so a reader can follow one.
	if !strings.Contains(recorder.Body.String(), "#1") || !strings.Contains(recorder.Body.String(), "#2") {
		t.Error("the page does not show the issue numbers")
	}
}

// TestTheIssueListFiltersByState covers the filter, which the domain validates.
func TestTheIssueListFiltersByState(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	for state, want := range map[string]int{
		"open": http.StatusOK, "closed": http.StatusOK, "all": http.StatusOK,
	} {
		recorder := h.get("/alice/my-repo/issues?state=" + state)
		if recorder.Code != want {
			t.Errorf("?state=%s = %d, want %d", state, recorder.Code, want)
		}
	}

	// An unknown filter is a not-found rather than a silently unfiltered page, because a reader
	// who asked for one thing must not be shown another.
	recorder := h.get("/alice/my-repo/issues?state=merged")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("?state=merged = %d, want 404", recorder.Code)
	}

	// A malformed cursor is refused for the same reason: serving the first page to a reader who
	// followed a "more" link would look like the list had ended and restarted.
	for _, cursor := range []string{"not-a-cursor", "2026-10-14T10:00:00Z", ",", "2026-10-14T10:00:00Z,"} {
		recorder := h.get("/alice/my-repo/issues?before=" + cursor)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("?before=%s = %d, want 404", cursor, recorder.Code)
		}
	}
}

// TestTheIssueDetailRendersTheBodyAsMarkdown is the join between this surface and W2.1.
func TestTheIssueDetailRendersTheBodyAsMarkdown(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	recorder := h.get("/alice/my-repo/issues/1")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()

	if !strings.Contains(body, "The first issue") {
		t.Error("the page does not show the title")
	}
	// The Markdown was rendered, not shown literally: a heading became an element and the
	// emphasis became one too. The heading is an `h2` because the document is rendered embedded,
	// so the page keeps a single first-level heading — the issue's own title.
	if !strings.Contains(body, "<h2>A heading</h2>") {
		t.Errorf("the body was not rendered as Markdown:\n%s", body)
	}
	if !strings.Contains(body, "<strong>bold</strong>") {
		t.Error("the emphasis in the body was not rendered")
	}
	// And the literal Markdown source is gone.
	if strings.Contains(body, "# A heading") {
		t.Error("the raw Markdown source is still shown")
	}
}

// TestAnUnreadableIssueIsTheNotFoundPage covers the denied path.
//
// A repository the caller may not read and one that does not exist produce the same page, which
// is the T-A4 shape at this surface.
func TestAnUnreadableIssueIsTheNotFoundPage(t *testing.T) {
	h := newIssueHarness(t, repository.LevelNone)

	for _, path := range []string{"/alice/my-repo/issues", "/alice/my-repo/issues/1"} {
		t.Run(path, func(t *testing.T) {
			recorder := h.get(path)
			if recorder.Code != http.StatusNotFound {
				t.Errorf("%s = %d, want 404", path, recorder.Code)
			}
			// The refusal must not name the issue, which would disclose that it exists.
			if strings.Contains(recorder.Body.String(), "The first issue") {
				t.Error("the refusal discloses the issue's title")
			}
		})
	}
}

// TestAnIssueTheCallerMayNotReachIsNotFound covers the service's own refusal being turned into
// the same page, rather than into a distinguishable error.
func TestAnIssueTheCallerMayNotReachIsNotFound(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)
	h.service.getErr = issues.ErrNotFound

	recorder := h.get("/alice/my-repo/issues/1")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", recorder.Code)
	}
}

// TestAnUnknownNumberIsNotFound covers a number with no issue, and one that is not a number.
func TestAnUnknownNumberIsNotFound(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	for _, number := range []string{"2", "abc", "0", "-1"} {
		recorder := h.get("/alice/my-repo/issues/" + number)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("/issues/%s = %d, want 404", number, recorder.Code)
		}
	}
}

// TestThePageReadsThroughTheSharedService is the architectural rule: a page must not answer an
// access question itself, so it asks the service and passes the caller's identity.
func TestThePageReadsThroughTheSharedService(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	h.get("/alice/my-repo/issues/1")
	if h.service.getInput == nil {
		t.Fatal("the page did not ask the issue service")
	}
	// The service is given the repository's identifier and the issue's number, and — crucially —
	// no level: the decision is the service's to make.
	if h.service.getInput.RepositoryID != repoID || h.service.getInput.Number != 1 {
		t.Errorf("the page asked for %+v", h.service.getInput)
	}
}

// TestTheIssueHistoryIsNotShownWithoutALedger covers the nil-reader case, so a handler built
// without one still renders a working page rather than failing.
func TestTheIssueHistoryIsNotShownWithoutALedger(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	recorder := h.get("/alice/my-repo/issues/1")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	// The history heading is present and its empty line stands in, because the reader is a
	// separate dependency: losing the history is not a reason to lose the issue.
	if !strings.Contains(recorder.Body.String(), "History") {
		t.Error("the page has no history section")
	}
}

// TestTheIssueRoutesAreAbsentWithoutAService keeps a handler built without an issue service from
// advertising routes it cannot serve.
func TestTheIssueRoutesAreAbsentWithoutAService(t *testing.T) {
	row := repository.Repository{
		ID: repoID, OwnerID: ownerID, Name: "my-repo", Visibility: repository.VisibilityPublic,
		DefaultBranch: "main", StorageID: "01920000-0000-7000-8000-0000000000b3",
	}
	handler, err := web.NewHandler(&fakeRepositories{row: row, level: repository.LevelRead},
		&fakeContent{}, &fakeOwners{}, &fakeSessions{}, "")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/alice/my-repo/issues", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("the issue list without a service = %d, want 404", recorder.Code)
	}
}
