package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/go-gitnest/GitNest/internal/transport/http/api"
)

// The issue API's contract.
//
// These tests are about the HTTP surface: the status codes, the response shape, the headers,
// and which refusals are distinguishable from which. The **rules** behind them — numbering,
// the author allowance, optimistic concurrency — are asserted in `internal/issues`, and the
// full stack is exercised in `internal/app`. A fake service is used here because a transport
// test that also re-tested the domain would fail for reasons that have nothing to do with HTTP.

// fakeIssueService records what it was asked and returns what a test configures.
//
// It enforces only the one rule that the transport itself is responsible for — that an
// anonymous caller cannot open an issue, which the handler checks before the body is read —
// and otherwise reports the domain error the test set, so the status mapping is what gets
// asserted.
type fakeIssueService struct {
	issue issues.Issue
	page  issues.Page

	createErr, getErr, listErr, editErr, stateErr, deleteErr error

	created    *issues.CreateInput
	edited     *issues.EditInput
	closed     *issues.CloseInput
	reopened   *issues.ReopenInput
	deleted    *issues.DeleteInput
	listedWith *issues.ListInput
}

func (f *fakeIssueService) Create(_ context.Context, input issues.CreateInput) (issues.Issue, error) {
	f.created = &input
	if f.createErr != nil {
		return issues.Issue{}, f.createErr
	}
	created := f.issue
	created.Title, created.Body = input.Title, input.Body
	return created, nil
}

func (f *fakeIssueService) Get(_ context.Context, input issues.GetInput) (issues.Issue, error) {
	if f.getErr != nil {
		return issues.Issue{}, f.getErr
	}
	if input.Number != f.issue.Number {
		return issues.Issue{}, issues.ErrNotFound
	}
	return f.issue, nil
}

func (f *fakeIssueService) List(_ context.Context, input issues.ListInput) (issues.Page, error) {
	f.listedWith = &input
	if f.listErr != nil {
		return issues.Page{}, f.listErr
	}
	return f.page, nil
}

func (f *fakeIssueService) Edit(_ context.Context, input issues.EditInput) (issues.Issue, error) {
	f.edited = &input
	if f.editErr != nil {
		return issues.Issue{}, f.editErr
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
	if f.stateErr != nil {
		return issues.Issue{}, f.stateErr
	}
	closed := f.issue
	closed.State = issues.StateClosed
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	closed.ClosedAt = &at
	closed.ClosedBy, closed.ClosedByName = "alice", "alice"
	closed.Version++
	return closed, nil
}

func (f *fakeIssueService) Reopen(_ context.Context, input issues.ReopenInput) (issues.Issue, error) {
	f.reopened = &input
	if f.stateErr != nil {
		return issues.Issue{}, f.stateErr
	}
	reopened := f.issue
	reopened.State = issues.StateOpen
	reopened.ClosedAt, reopened.ClosedBy, reopened.ClosedByName = nil, "", ""
	reopened.Version++
	return reopened, nil
}

func (f *fakeIssueService) Delete(_ context.Context, input issues.DeleteInput) error {
	f.deleted = &input
	return f.deleteErr
}

// issueHarness mounts the issue routes with the fakes behind them.
type issueHarness struct {
	t       *testing.T
	mux     *http.ServeMux
	issues  *fakeIssueService
	row     repository.Repository
	ownerID string
	otherID string
}

const (
	issueOwnerID = "01920000-0000-7000-8000-0000000000a1"
	issueOtherID = "01920000-0000-7000-8000-0000000000a2"
)

// newIssueHarness prepares a repository the owner admins and a second account whose level a
// test chooses.
func newIssueHarness(t *testing.T, otherLevel repository.Level) *issueHarness {
	t.Helper()
	row := repository.Repository{
		ID: "01920000-0000-7000-8000-0000000000a3", OwnerID: issueOwnerID, Name: "issue-repo",
		Description: "a repository", Visibility: repository.VisibilityPrivate,
		DefaultBranch: "main", StorageID: "01920000-0000-7000-8000-0000000000a4",
	}
	repositories := newFakeRepositories()
	repositories.add(row, repository.LevelAdmin)
	if otherLevel != repository.LevelNone {
		repositories.withViewerLevel(row.ID, issueOtherID, otherLevel)
	} else {
		repositories.withViewerLevel(row.ID, issueOtherID, repository.LevelNone)
	}

	alice := identity.User{ID: issueOwnerID, Username: "alice", Status: identity.StatusActive}
	bob := identity.User{ID: issueOtherID, Username: "bob", Status: identity.StatusActive}
	owners := &fakeOwners{
		users: map[string]identity.User{"alice": alice, "bob": bob},
		byID:  map[string]identity.User{issueOwnerID: alice, issueOtherID: bob},
	}

	// A session resolving to the owner, which is the account every request carries unless a test
	// says otherwise.
	accounts := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return alice, identity.Session{ID: "session"}, identity.HashToken(csrfTokenValue), nil
		},
	}

	service := &fakeIssueService{
		issue: issues.Issue{
			ID: "01920000-0000-7000-8000-0000000000a5", RepositoryID: row.ID, Number: 1,
			Title: "the first issue", Body: "a body",
			AuthorID: issueOwnerID, AuthorName: "alice", State: issues.StateOpen,
			CreatedAt: time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC),
			Version:   1,
		},
		page: issues.Page{Issues: []issues.Issue{}},
	}

	mux := http.NewServeMux()
	api.RegisterIssues(mux, accounts, owners, repositories, service)

	return &issueHarness{t: t, mux: mux, issues: service, row: row, ownerID: issueOwnerID, otherID: issueOtherID}
}

// do dispatches a request to the handler.
func (h *issueHarness) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

// authenticated is the session header the harness's session resolves from.
func authenticated() map[string]string {
	return map[string]string{"Authorization": "Bearer a-session-token"}
}

// base is the collection path.
func (h *issueHarness) base() string {
	return "/api/v1/repos/alice/" + h.row.Name + "/issues"
}

// one is the path of one issue.
func (h *issueHarness) one(number string) string { return h.base() + "/" + number }

// decodeIssue reads an issue response.
func decodeIssue(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not JSON: %v\n%s", err, recorder.Body.String())
	}
	return body
}

// TestOpeningAnIssueIsCreatedWithItsNumber covers the create contract: a 201, a Location naming
// the number, and a body carrying it.
func TestOpeningAnIssueIsCreatedWithItsNumber(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	recorder := h.do(http.MethodPost, h.base(), `{"title":"a new issue","body":"with text"}`, authenticated())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201\n%s", recorder.Code, recorder.Body.String())
	}

	body := decodeIssue(t, recorder)
	if body["number"] != float64(1) {
		t.Errorf("number = %v, want 1", body["number"])
	}
	if body["title"] != "a new issue" || body["body"] != "with text" {
		t.Errorf("the payload does not carry what was sent: %v", body)
	}
	if body["state"] != "open" {
		t.Errorf("state = %v, want open", body["state"])
	}

	// The Location names the number, which is the identity a client uses from here on.
	if location := recorder.Header().Get("Location"); !strings.HasSuffix(location, "/issues/1") {
		t.Errorf("Location = %q, want it to end with /issues/1", location)
	}

	// The service was given the caller's identity, not a level: the transport decides nothing.
	if h.issues.created == nil {
		t.Fatal("the service was not called")
	}
	if h.issues.created.Actor.UserID != issueOwnerID {
		t.Errorf("the create actor is %q, want the session's account", h.issues.created.Actor.UserID)
	}
}

// TestAnIssuePayloadCarriesNoInternalIdentifier is the data-model rule that "a stable internal
// UUID never appears in a URL, a redirect target, a storage path, or a user-visible payload".
func TestAnIssuePayloadCarriesNoInternalIdentifier(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	recorder := h.do(http.MethodGet, h.one("1"), "", authenticated())
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	// The returned identifier must not appear anywhere in the body, in any field.
	payload := recorder.Body.String()
	for _, forbidden := range []string{
		h.issues.issue.ID, h.issues.issue.RepositoryID, h.row.ID, h.row.StorageID, h.ownerID,
	} {
		if strings.Contains(payload, forbidden) {
			t.Errorf("the payload exposes the internal identifier %q:\n%s", forbidden, payload)
		}
	}
	// The author is named, not identified.
	if !strings.Contains(payload, `"login":"alice"`) {
		t.Errorf("the author is not named: %s", payload)
	}
}

// TestOpeningAnIssueRequiresAnAccount covers the one operation that needs a credential where
// reading does not.
func TestOpeningAnIssueRequiresAnAccount(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	// No session at all.
	recorder := h.do(http.MethodPost, h.base(), `{"title":"anonymous"}`, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous create = %d, want 401", recorder.Code)
	}
	if h.issues.created != nil {
		t.Error("an anonymous create reached the service")
	}
}

// TestAPrivateRepositoryIsNotDisclosedThroughIssues covers the T-A4 shape on this surface.
func TestAPrivateRepositoryIsNotDisclosedThroughIssues(t *testing.T) {
	h := newIssueHarness(t, repository.LevelNone)
	// The other account holds nothing on the repository, so the repository does not resolve for
	// it — which is what the repository service reports as not-found.
	h.issues.listErr = issues.ErrNotFound
	h.issues.getErr = issues.ErrNotFound

	for _, test := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "the listing", method: http.MethodGet, path: h.base()},
		{name: "one issue", method: http.MethodGet, path: h.one("1")},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := h.do(test.method, test.path, "", authenticated())
			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", recorder.Code)
			}
			// A negative answer must not be remembered, so a caller who gains access is not
			// served the refusal from a cache.
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

// TestAnUnknownOrMalformedNumberIsNotFound covers the routing answer: a number that does not
// exist and one that is not a number are the same thing to a caller.
func TestAnUnknownOrMalformedNumberIsNotFound(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	for _, number := range []string{"2", "999999", "abc", "1.5", "0", "-1", "1abc"} {
		t.Run(number, func(t *testing.T) {
			recorder := h.do(http.MethodGet, h.one(number), "", authenticated())
			if recorder.Code != http.StatusNotFound {
				t.Errorf("GET /issues/%s = %d, want 404", number, recorder.Code)
			}
		})
	}
}

// TestTheListingPagesWithALinkHeader covers the pagination contract.
func TestTheListingPagesWithALinkHeader(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)
	// Two issues, and the store reports more to come.
	h.issues.page = issues.Page{
		Issues: []issues.Issue{
			{Number: 2, Title: "second", State: issues.StateOpen, Version: 1},
			{Number: 1, Title: "first", State: issues.StateOpen, Version: 1},
		},
		HasMore: true,
	}

	recorder := h.do(http.MethodGet, h.base()+"?per_page=2", "", authenticated())
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", recorder.Code, recorder.Body.String())
	}

	var items []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatalf("the payload is not a JSON array: %v\n%s", err, recorder.Body.String())
	}
	if len(items) != 2 {
		t.Errorf("the page holds %d issues, want 2", len(items))
	}
	link := recorder.Header().Get("Link")
	if !strings.Contains(link, `rel="next"`) {
		t.Errorf("Link = %q, want a next relation", link)
	}
	// **On the first page there is no `first` and no `prev`**, because both would point at the
	// page the caller is already on. `linkHeader` omits a relation rather than emitting one that
	// names the current page, and the document requires a relation to be omitted rather than
	// emitted empty.
	if strings.Contains(link, `rel="first"`) || strings.Contains(link, `rel="prev"`) {
		t.Errorf("Link = %q must not name the current page", link)
	}
	// A total is deliberately absent: producing one would have to be bounded by visibility, and a
	// count would disclose how many issues a caller cannot see.
	if strings.Contains(link, `rel="last"`) {
		t.Errorf("Link = %q must not carry a last relation, which needs a total", link)
	}

	// The second page, where `first` and `prev` do belong.
	second := h.do(http.MethodGet, h.base()+"?per_page=2&page=2", "", authenticated())
	secondLink := second.Header().Get("Link")
	for _, relation := range []string{`rel="first"`, `rel="prev"`} {
		if !strings.Contains(secondLink, relation) {
			t.Errorf("Link on page 2 = %q, want %s", secondLink, relation)
		}
	}

	// The state filter went to the service, which is the only place that validates it.
	if h.issues.listedWith == nil || h.issues.listedWith.State != "" {
		t.Errorf("the listing was not asked for every state: %+v", h.issues.listedWith)
	}
}

// TestTheStateFilterIsValidated covers an invalid filter, which the domain refuses.
func TestTheStateFilterIsValidated(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	recorder := h.do(http.MethodGet, h.base()+"?state=merged", "", authenticated())
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("an invalid state filter = %d, want 422\n%s", recorder.Code, recorder.Body.String())
	}
	if h.issues.listedWith != nil {
		t.Error("an invalid filter reached the service")
	}

	// A valid one is passed down.
	h.do(http.MethodGet, h.base()+"?state=closed", "", authenticated())
	if h.issues.listedWith == nil || h.issues.listedWith.State != issues.StateClosed {
		t.Errorf("the filter was not passed to the service: %+v", h.issues.listedWith)
	}
}

// TestAnEditRequiresTheVersionItRead is the project's optimistic-concurrency rule on the wire.
func TestAnEditRequiresTheVersionItRead(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	// Without a version the request is refused, because an edit that does not say what it read
	// cannot detect a concurrent change.
	recorder := h.do(http.MethodPatch, h.one("1"), `{"title":"changed"}`, authenticated())
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("an edit with no version = %d, want 422\n%s", recorder.Code, recorder.Body.String())
	}
	if h.issues.edited != nil {
		t.Error("an edit with no version reached the service")
	}

	// With the version it read, the edit applies.
	recorder = h.do(http.MethodPatch, h.one("1"), `{"title":"changed","version":1}`, authenticated())
	if recorder.Code != http.StatusOK {
		t.Fatalf("a versioned edit = %d, want 200\n%s", recorder.Code, recorder.Body.String())
	}
	if h.issues.edited == nil || h.issues.edited.ExpectedVersion != 1 {
		t.Errorf("the service was not given the version: %+v", h.issues.edited)
	}
	if h.issues.edited.Title == nil || *h.issues.edited.Title != "changed" {
		t.Errorf("the title was not passed: %+v", h.issues.edited)
	}

	// A stale version is a conflict, which is the visible error the rule requires.
	h.issues.editErr = issues.ErrConflict
	recorder = h.do(http.MethodPatch, h.one("1"), `{"title":"too late","version":1}`, authenticated())
	if recorder.Code != http.StatusConflict {
		t.Errorf("a stale edit = %d, want 409\n%s", recorder.Code, recorder.Body.String())
	}
}

// TestAnEditNamingNothingIsRefused covers a request that changes nothing, which is a client bug.
func TestAnEditNamingNothingIsRefused(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	recorder := h.do(http.MethodPatch, h.one("1"), `{}`, authenticated())
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("an empty patch = %d, want 422\n%s", recorder.Code, recorder.Body.String())
	}
	if h.issues.edited != nil {
		t.Error("an empty patch reached the service")
	}
}

// TestAnUnknownFieldIsRefused keeps a misspelling from being silently ignored — which for a
// request that guards on a version would be the difference between a conflict and an overwrite.
func TestAnUnknownFieldIsRefused(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	recorder := h.do(http.MethodPost, h.base(), `{"titel":"a typo"}`, authenticated())
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("an unknown field = %d, want 422\n%s", recorder.Code, recorder.Body.String())
	}
	if h.issues.created != nil {
		t.Error("a request with an unknown field reached the service")
	}
}

// TestClosingAndReopeningGoThroughTheTransitions covers the state machine on this surface.
func TestClosingAndReopeningGoThroughTheTransitions(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	recorder := h.do(http.MethodPatch, h.one("1"), `{"state":"closed"}`, authenticated())
	if recorder.Code != http.StatusOK {
		t.Fatalf("a close = %d, want 200\n%s", recorder.Code, recorder.Body.String())
	}
	if h.issues.closed == nil || h.issues.closed.Number != 1 {
		t.Errorf("the close did not reach the service: %+v", h.issues.closed)
	}
	body := decodeIssue(t, recorder)
	if body["state"] != "closed" {
		t.Errorf("state = %v, want closed", body["state"])
	}
	if body["closed_at"] == nil || body["closed_by"] == nil {
		t.Errorf("the close is not reported: %v", body)
	}
	if h.issues.edited != nil {
		t.Error("a state-only patch also ran an edit")
	}

	// Reopening.
	h.issues.issue.State = issues.StateClosed
	recorder = h.do(http.MethodPatch, h.one("1"), `{"state":"open"}`, authenticated())
	if recorder.Code != http.StatusOK {
		t.Fatalf("a reopen = %d, want 200\n%s", recorder.Code, recorder.Body.String())
	}
	if h.issues.reopened == nil || h.issues.reopened.Number != 1 {
		t.Errorf("the reopen did not reach the service: %+v", h.issues.reopened)
	}
	body = decodeIssue(t, recorder)
	if body["state"] != "open" {
		t.Errorf("state = %v, want open", body["state"])
	}
	// A reopen clears the close, so the payload must not still report one.
	if body["closed_at"] != nil || body["closed_by"] != nil {
		t.Errorf("the reopen still reports a close: %v", body)
	}

	// Naming the state an issue is already in is a no-op rather than an error, because a client
	// that sends the whole object back names the current state every time.
	//
	// The fake's read reports the state the fixture holds, so it is set to what the request will
	// name — which is the situation being asserted, and not something the service sees.
	h.issues.issue.State = issues.StateOpen
	h.issues.reopened = nil
	recorder = h.do(http.MethodPatch, h.one("1"), `{"state":"open"}`, authenticated())
	if recorder.Code != http.StatusOK {
		t.Errorf("a redundant state = %d, want 200\n%s", recorder.Code, recorder.Body.String())
	}
	if h.issues.reopened != nil {
		t.Error("a redundant state ran a transition")
	}
}

// TestAnUnknownStateIsRefused covers the guard on the state value.
func TestAnUnknownStateIsRefused(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	recorder := h.do(http.MethodPatch, h.one("1"), `{"state":"merged"}`, authenticated())
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("an unknown state = %d, want 422\n%s", recorder.Code, recorder.Body.String())
	}
	if h.issues.closed != nil || h.issues.reopened != nil {
		t.Error("an unknown state ran a transition")
	}
}

// TestADeniedChangeIsForbiddenRatherThanNotFound covers the two refusals being different: the
// caller can already read the repository — the repository service resolved their level — so
// telling them their level is too low discloses nothing.
func TestADeniedChangeIsForbiddenRatherThanNotFound(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)
	h.issues.editErr = issues.ErrForbidden
	h.issues.getErr = nil

	recorder := h.do(http.MethodPatch, h.one("1"), `{"title":"not mine","version":1}`, authenticated())
	if recorder.Code != http.StatusForbidden {
		t.Errorf("a denied edit = %d, want 403\n%s", recorder.Code, recorder.Body.String())
	}

	h.issues.deleteErr = issues.ErrForbidden
	recorder = h.do(http.MethodDelete, h.one("1"), "", authenticated())
	if recorder.Code != http.StatusForbidden {
		t.Errorf("a denied delete = %d, want 403\n%s", recorder.Code, recorder.Body.String())
	}
}

// TestDeletingAnswersNoContent covers the delete contract and its cache directive.
func TestDeletingAnswersNoContent(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	recorder := h.do(http.MethodDelete, h.one("1"), "", authenticated())
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204\n%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("a 204 carries a body: %s", recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if h.issues.deleted == nil || h.issues.deleted.Number != 1 {
		t.Errorf("the delete did not reach the service: %+v", h.issues.deleted)
	}

	h.issues.deleteErr = issues.ErrNotFound
	recorder = h.do(http.MethodDelete, h.one("1"), "", authenticated())
	if recorder.Code != http.StatusNotFound {
		t.Errorf("deleting an absent issue = %d, want 404", recorder.Code)
	}
}

// TestAMethodMismatchIsRefused covers the route registration rule: "method mismatch returns 405
// with an `Allow` header".
func TestAMethodMismatchIsRefused(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	// The collection has GET and POST; a PUT is neither.
	recorder := h.do(http.MethodPut, h.base(), `{}`, authenticated())
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT on the collection = %d, want 405", recorder.Code)
	}
	if allow := recorder.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q, want it to name the permitted methods", allow)
	}
}

// TestAnAnonymousCallerCannotChangeAnIssue covers the anonymous path on a repository a public
// caller could otherwise read: a patch with no session has no author to attribute the change to.
func TestAnAnonymousCallerCannotChangeAnIssue(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)
	h.issues.editErr = issues.ErrForbidden

	// Without a session the principal is anonymous, and the issue service refuses it as
	// forbidden rather than as not-found — the repository resolved, so the caller can read it.
	recorder := h.do(http.MethodPatch, h.one("1"), `{"title":"anonymous","version":1}`, nil)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("an anonymous edit = %d, want 403\n%s", recorder.Code, recorder.Body.String())
	}
}

// TestABodyOverTheBoundIsRefused keeps the request body from being buffered without limit.
func TestABodyOverTheBoundIsRefused(t *testing.T) {
	h := newIssueHarness(t, repository.LevelRead)

	// A title far past the bound, so the domain would refuse it — but the body is refused before
	// the domain sees it, which is what bounds the buffering.
	huge := strings.Repeat("x", issues.MaxBodyLength+8192)
	recorder := h.do(http.MethodPost, h.base(), `{"title":"large","body":"`+huge+`"}`, authenticated())
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("an oversized body = %d, want 422\n%s", recorder.Code, recorder.Body.String())
	}
	if h.issues.created != nil {
		t.Error("an oversized body reached the service")
	}
}
