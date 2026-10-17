package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/go-gitnest/GitNest/internal/transport/http/web"
)

// The issue mutation forms.
//
// These tests are the reason the surface is a set of server-rendered forms rather than a React
// island: a form submission is a request a Go test can make, so every path — the allowed one and
// each refusal — is exercised here rather than asserted about a bundle that cannot be built in the
// development environment.

// fakeCSRF issues the token a form carries.
type fakeCSRF struct {
	// token is what it hands out, so a test can present it or a wrong value.
	token string
	err   error
}

func (f *fakeCSRF) RotateCSRFToken(_ context.Context, sessionToken string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if sessionToken == "" {
		return "", identity.ErrInvalidToken
	}
	return f.token, nil
}

// formHarness mounts the renderer with the issue forms behind it.
type formHarness struct {
	t       *testing.T
	mux     *http.ServeMux
	service *fakeIssueService
	csrf    string
	// owner is whether the session resolves to the repository's owner, which decides the author
	// allowance.
	owner bool
}

// formToken is what a valid submission must carry.
//
// **It is the value the shared session harness binds its digest to.** `fakeSessions.ResolveSession`
// returns `identity.HashToken("csrf")` as the stored digest, so a form that presents anything else
// is refused — which is the check working, not a defect in the test. A wrong-token case below
// presents a different value deliberately.
const formToken = "csrf"

// newFormHarness prepares a public repository with one open issue and a working session.
func newFormHarness(t *testing.T, level repository.Level, signedIn bool) *formHarness {
	t.Helper()
	row := repository.Repository{
		ID: repoID, OwnerID: ownerID, Name: "my-repo", Description: "A repository",
		Visibility: repository.VisibilityPublic, DefaultBranch: "main",
		StorageID: "01920000-0000-7000-8000-0000000000b3",
	}
	repos := &fakeRepositories{row: row, level: level}
	owners := &fakeOwners{users: map[string]identity.User{
		"alice": {ID: ownerID, Username: "alice", Status: identity.StatusActive},
	}}

	// The session resolves to the owner, which makes the caller the issue's author. A test that
	// wants a non-author switches it to another account.
	sessions := &fakeSessions{users: map[string]identity.User{}}
	if signedIn {
		sessions.users["valid-session"] = identity.User{
			ID: ownerID, Username: "alice", Status: identity.StatusActive,
		}
	}

	service := &fakeIssueService{
		issue: issues.Issue{
			ID: "01920000-0000-7000-8000-0000000000c1", RepositoryID: repoID, Number: 1,
			Title: "The first issue", Body: "A body",
			AuthorID: ownerID, AuthorName: "alice", State: issues.StateOpen, Version: 1,
		},
	}

	handler, err := web.NewHandler(repos, &fakeContent{}, owners, sessions, "")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	handler = handler.WithIssues(service).WithCSRF(&fakeCSRF{token: formToken})

	mux := http.NewServeMux()
	handler.Register(mux)
	return &formHarness{t: t, mux: mux, service: service, csrf: formToken, owner: true}
}

// post submits a form.
func (h *formHarness) post(path string, fields map[string]string, withCookie bool) *httptest.ResponseRecorder {
	h.t.Helper()
	form := url.Values{}
	for name, value := range fields {
		form.Set(name, value)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if withCookie {
		request.AddCookie(&http.Cookie{Name: "gitnest_session", Value: "valid-session"})
	}
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

// get reads a page.
func (h *formHarness) get(path string, withCookie bool) *httptest.ResponseRecorder {
	h.t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if withCookie {
		request.AddCookie(&http.Cookie{Name: "gitnest_session", Value: "valid-session"})
	}
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

// validFields is a submission that should succeed.
func (h *formHarness) validFields(action string, extra map[string]string) map[string]string {
	fields := map[string]string{
		"csrf_token": formToken,
		"action":     action,
	}
	for name, value := range extra {
		fields[name] = value
	}
	return fields
}

// TestTheCreationFormIsOfferedToASignedInReader covers the form's rendering.
func TestTheCreationFormIsOfferedToASignedInReader(t *testing.T) {
	h := newFormHarness(t, repository.LevelRead, true)

	recorder := h.get("/alice/my-repo/issues/new", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the creation form = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	// The form posts to the documented collection path and carries the token as a hidden field.
	if !strings.Contains(body, `action="/alice/my-repo/issues"`) {
		t.Errorf("the form does not post to the collection:\n%s", body)
	}
	if !strings.Contains(body, `name="csrf_token" value="`+formToken+`"`) {
		t.Error("the form carries no CSRF token")
	}
	// It is a plain form, so the workflow works without scripting.
	if !strings.Contains(body, `name="title"`) || !strings.Contains(body, `name="body"`) {
		t.Error("the form has no title or body field")
	}
}

// TestTheCreationFormRequiresASession covers the refusal before the form is rendered.
//
// An issue needs an author, so a reader with no session is told to sign in rather than shown a form
// that cannot work.
func TestTheCreationFormRequiresASession(t *testing.T) {
	h := newFormHarness(t, repository.LevelRead, false)

	recorder := h.get("/alice/my-repo/issues/new", false)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("the creation form without a session = %d, want 401", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), `name="csrf_token"`) {
		t.Error("a form was rendered for an anonymous reader")
	}
}

// TestCreatingAnIssueRedirectsAfterThePost covers the create path and the redirect that stops a
// reload from filing twice.
func TestCreatingAnIssueRedirectsAfterThePost(t *testing.T) {
	h := newFormHarness(t, repository.LevelRead, true)

	recorder := h.post("/alice/my-repo/issues",
		h.validFields("", map[string]string{"title": "A new issue", "body": "With a body"}), true)

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("creating = %d, want 303\n%s", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); location != "/alice/my-repo/issues" {
		t.Errorf("Location = %q, want the issue list", location)
	}
	if h.service.created == nil {
		t.Fatal("the service was not asked to create")
	}
	if h.service.created.Title != "A new issue" || h.service.created.Body != "With a body" {
		t.Errorf("the service was given %+v", h.service.created)
	}
	// The actor is the session's account, so the issue has an author.
	if h.service.created.Actor.UserID != ownerID {
		t.Errorf("the actor is %q, want the session's account", h.service.created.Actor.UserID)
	}
}

// TestACreationWithoutCSRFIsRefused covers the forgery guard.
func TestACreationWithoutCSRFIsRefused(t *testing.T) {
	h := newFormHarness(t, repository.LevelRead, true)

	for _, test := range []struct {
		name   string
		fields map[string]string
		cookie bool
		want   int
	}{
		{
			name:   "no token",
			fields: map[string]string{"action": "", "title": "x"},
			cookie: true, want: http.StatusForbidden,
		},
		{
			name:   "a wrong token",
			fields: map[string]string{"csrf_token": "not-the-token", "title": "x"},
			cookie: true, want: http.StatusForbidden,
		},
		{
			name:   "no session at all",
			fields: map[string]string{"csrf_token": formToken, "title": "x"},
			cookie: false, want: http.StatusUnauthorized,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h.service.created = nil
			recorder := h.post("/alice/my-repo/issues", test.fields, test.cookie)
			if recorder.Code != test.want {
				t.Errorf("status = %d, want %d", recorder.Code, test.want)
			}
			// The point of the guard: nothing was created.
			if h.service.created != nil {
				t.Error("a submission without a valid token reached the service")
			}
		})
	}
}

// TestARefusedCreationKeepsTheReadersText is the accessibility requirement made checkable.
//
// A form that loses a long body because the title was one character too long is the behaviour that
// "error messages are announced and tied to their field" exists to prevent.
func TestARefusedCreationKeepsTheReadersText(t *testing.T) {
	h := newFormHarness(t, repository.LevelRead, true)
	h.service.mutateErr = issues.ErrInvalidTitle

	long := strings.Repeat("a long body ", 40)
	recorder := h.post("/alice/my-repo/issues",
		h.validFields("", map[string]string{"title": "", "body": long}), true)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a refused creation = %d, want 422", recorder.Code)
	}
	body := recorder.Body.String()
	// The reader's text is still there.
	if !strings.Contains(body, "a long body") {
		t.Error("the reader's body was lost")
	}
	// The error is announced and tied to the field.
	if !strings.Contains(body, `role="alert"`) {
		t.Error("the error is not announced")
	}
	if !strings.Contains(body, `aria-invalid="true"`) ||
		!strings.Contains(body, `aria-describedby="issue-title-error"`) {
		t.Error("the error is not tied to the title field")
	}
	// And the message is translated text, not the domain's English sentence.
	if strings.Contains(body, issues.ErrInvalidTitle.Error()) {
		t.Error("the page shows the domain error's text rather than a catalogue message")
	}
}

// TestTheIssuePageOffersTheControlsTheReaderMayUse is the permission prediction.
func TestTheIssuePageOffersTheControlsTheReaderMayUse(t *testing.T) {
	for _, test := range []struct {
		name       string
		level      repository.Level
		wantEdit   bool
		wantDelete bool
	}{
		{name: "a reader who is the author", level: repository.LevelRead, wantEdit: true, wantDelete: false},
		{name: "a writer", level: repository.LevelWrite, wantEdit: true, wantDelete: true},
		{name: "an admin", level: repository.LevelAdmin, wantEdit: true, wantDelete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFormHarness(t, test.level, true)

			recorder := h.get("/alice/my-repo/issues/1", true)
			if recorder.Code != http.StatusOK {
				t.Fatalf("the issue page = %d, want 200", recorder.Code)
			}
			body := recorder.Body.String()

			// The edit form is what carries the action field, so its presence is the edit control.
			hasEdit := strings.Contains(body, `name="action" value="edit"`)
			if hasEdit != test.wantEdit {
				t.Errorf("the edit control is offered=%v, want %v", hasEdit, test.wantEdit)
			}
			hasDelete := strings.Contains(body, `name="action" value="delete"`)
			if hasDelete != test.wantDelete {
				t.Errorf("the delete control is offered=%v, want %v", hasDelete, test.wantDelete)
			}
			// The state control is there whenever the edit control is, because they share the
			// allowance.
			hasState := strings.Contains(body, `name="action" value="close"`) ||
				strings.Contains(body, `name="action" value="reopen"`)
			if hasState != test.wantEdit {
				t.Errorf("the state control is offered=%v, want %v", hasState, test.wantEdit)
			}
		})
	}
}

// TestAnEditCarriesTheVersionItRead is the optimistic-concurrency rule on the form.
func TestAnEditCarriesTheVersionItRead(t *testing.T) {
	h := newFormHarness(t, repository.LevelRead, true)

	// The page carries the version in a hidden field.
	page := h.get("/alice/my-repo/issues/1", true).Body.String()
	if !strings.Contains(page, `name="version" value="1"`) {
		t.Errorf("the page carries no version:\n%s", page)
	}

	recorder := h.post("/alice/my-repo/issues/1",
		h.validFields("edit", map[string]string{"title": "Edited", "body": "New body", "version": "1"}), true)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("an edit = %d, want 303\n%s", recorder.Code, recorder.Body.String())
	}
	if h.service.edited == nil {
		t.Fatal("the service was not asked to edit")
	}
	if h.service.edited.ExpectedVersion != 1 {
		t.Errorf("the version passed is %d, want the one the page carried", h.service.edited.ExpectedVersion)
	}
	if location := recorder.Header().Get("Location"); !strings.Contains(location, "outcome=saved") {
		t.Errorf("Location = %q, want it to report the outcome", location)
	}
}

// TestAConcurrentEditIsRefusedAndShowsTheStoredText covers the conflict path.
//
// The refusal re-renders the page from a fresh read, so the reader sees what is actually stored
// beside the message saying their change was not applied — which is what makes the refusal
// actionable.
func TestAConcurrentEditIsRefusedAndShowsTheStoredText(t *testing.T) {
	h := newFormHarness(t, repository.LevelRead, true)
	h.service.mutateErr = issues.ErrConflict
	// The stored issue is what a fresh read returns.
	h.service.issue.Title = "Somebody else's version"

	recorder := h.post("/alice/my-repo/issues/1",
		h.validFields("edit", map[string]string{"title": "Mine", "body": "Mine", "version": "1"}), true)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("a conflicting edit = %d, want 409", recorder.Code)
	}
	body := recorder.Body.String()
	// The stored title is shown, so the reader can see what changed.
	if !strings.Contains(body, "Somebody else") {
		t.Error("the page does not show what is stored")
	}
	// And the reader's own text is kept, so nothing they wrote is lost.
	if !strings.Contains(body, "Mine") {
		t.Error("the reader's text was lost")
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Error("the conflict is not announced")
	}
}

// TestCloseAndReopenGoThroughTheTransitions covers the state controls.
func TestCloseAndReopenGoThroughTheTransitions(t *testing.T) {
	h := newFormHarness(t, repository.LevelWrite, true)

	recorder := h.post("/alice/my-repo/issues/1", h.validFields("close", nil), true)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("closing = %d, want 303\n%s", recorder.Code, recorder.Body.String())
	}
	if h.service.closed == nil {
		t.Fatal("the service was not asked to close")
	}

	// The page of a closed issue offers reopening instead.
	h.service.issue.State = issues.StateClosed
	page := h.get("/alice/my-repo/issues/1", true).Body.String()
	if !strings.Contains(page, `name="action" value="reopen"`) {
		t.Errorf("a closed issue does not offer reopening:\n%s", page)
	}
	if strings.Contains(page, `name="action" value="close"`) {
		t.Error("a closed issue still offers closing")
	}

	recorder = h.post("/alice/my-repo/issues/1", h.validFields("reopen", nil), true)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("reopening = %d, want 303\n%s", recorder.Code, recorder.Body.String())
	}
	if h.service.reopened == nil {
		t.Fatal("the service was not asked to reopen")
	}
}

// TestDeletingRedirectsToTheList covers the delete path, which cannot go back to the issue.
func TestDeletingRedirectsToTheList(t *testing.T) {
	h := newFormHarness(t, repository.LevelWrite, true)

	recorder := h.post("/alice/my-repo/issues/1", h.validFields("delete", nil), true)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("deleting = %d, want 303\n%s", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); location != "/alice/my-repo/issues" {
		t.Errorf("Location = %q, want the list, which still exists", location)
	}
	if h.service.deleted == nil || h.service.deleted.Number != 1 {
		t.Errorf("the service was not asked to delete: %+v", h.service.deleted)
	}
}

// TestADeniedChangeIsRefused covers the authorization path end to end.
//
// The service refuses, and the page is re-rendered with the refusal rather than redirecting as
// though it had worked.
func TestADeniedChangeIsRefused(t *testing.T) {
	h := newFormHarness(t, repository.LevelRead, true)
	h.service.mutateErr = issues.ErrForbidden

	for _, action := range []string{"edit", "close", "delete"} {
		t.Run(action, func(t *testing.T) {
			fields := h.validFields(action, nil)
			if action == "edit" {
				fields["title"] = "x"
				fields["version"] = "1"
			}
			recorder := h.post("/alice/my-repo/issues/1", fields, true)
			if recorder.Code != http.StatusForbidden {
				t.Errorf("a denied %s = %d, want 403", action, recorder.Code)
			}
			// A refusal must not redirect, because a redirect reads as success.
			if recorder.Code == http.StatusSeeOther {
				t.Error("a refusal redirected, which reads as success")
			}
		})
	}
}

// TestAnUnknownActionIsRefused keeps a typo from looking like a successful no-op.
func TestAnUnknownActionIsRefused(t *testing.T) {
	h := newFormHarness(t, repository.LevelWrite, true)

	recorder := h.post("/alice/my-repo/issues/1", h.validFields("archive", nil), true)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("an unknown action = %d, want 400", recorder.Code)
	}
	if h.service.edited != nil || h.service.closed != nil || h.service.deleted != nil {
		t.Error("an unknown action reached the service")
	}
}

// TestAMalformedVersionIsRefused covers the guard on the hidden field.
//
// A version of zero could never match and would report a conflict for a request that is merely
// malformed, which is a misleading answer to a client bug.
func TestAMalformedVersionIsRefused(t *testing.T) {
	h := newFormHarness(t, repository.LevelRead, true)

	for _, version := range []string{"0", "-1", "abc"} {
		t.Run(version, func(t *testing.T) {
			h.service.edited = nil
			recorder := h.post("/alice/my-repo/issues/1",
				h.validFields("edit", map[string]string{"title": "x", "version": version}), true)
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Errorf("version %q = %d, want 422", version, recorder.Code)
			}
			if h.service.edited != nil {
				t.Error("a malformed version reached the service")
			}
		})
	}
}

// TestTheFormsAreAbsentWithoutATokenIssuer keeps a handler without an issuer from offering a form
// that could never be submitted.
func TestTheFormsAreAbsentWithoutATokenIssuer(t *testing.T) {
	row := repository.Repository{
		ID: repoID, OwnerID: ownerID, Name: "my-repo", Visibility: repository.VisibilityPublic,
		DefaultBranch: "main", StorageID: "01920000-0000-7000-8000-0000000000b3",
	}
	unknown := &unknownEmptyService{}
	handler, err := web.NewHandler(&fakeRepositories{row: row, level: repository.LevelWrite},
		&fakeContent{}, &fakeOwners{}, &fakeSessions{}, "")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	// Deliberately no `WithCSRF`.
	handler = handler.WithIssues(unknown)
	mux := http.NewServeMux()
	handler.Register(mux)

	for _, path := range []string{"/alice/my-repo/issues", "/alice/my-repo/issues/1"} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		// The mutation routes were never registered, so the request is a method mismatch.
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s without an issuer = %d, want 405", path, recorder.Code)
		}
	}
}

// unknownEmptyService satisfies the interface without answering anything, for the test above.
type unknownEmptyService struct{}

func (unknownEmptyService) Create(context.Context, issues.CreateInput) (issues.Issue, error) {
	return issues.Issue{}, issues.ErrNotFound
}
func (unknownEmptyService) Get(context.Context, issues.GetInput) (issues.Issue, error) {
	return issues.Issue{}, issues.ErrNotFound
}
func (unknownEmptyService) List(context.Context, issues.ListInput) (issues.Page, error) {
	return issues.Page{}, nil
}
func (unknownEmptyService) Edit(context.Context, issues.EditInput) (issues.Issue, error) {
	return issues.Issue{}, issues.ErrNotFound
}
func (unknownEmptyService) Close(context.Context, issues.CloseInput) (issues.Issue, error) {
	return issues.Issue{}, issues.ErrNotFound
}
func (unknownEmptyService) Reopen(context.Context, issues.ReopenInput) (issues.Issue, error) {
	return issues.Issue{}, issues.ErrNotFound
}
func (unknownEmptyService) Delete(context.Context, issues.DeleteInput) error {
	return issues.ErrNotFound
}
