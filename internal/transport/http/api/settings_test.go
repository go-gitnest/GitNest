package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/go-gitnest/GitNest/internal/transport/http/api"
)

// settingsHarness mounts the settings routes with the fakes behind them.
type settingsHarness struct {
	t            *testing.T
	mux          *http.ServeMux
	repositories *fakeRepositories
	owners       *fakeOwners
	ownerID      string
	otherID      string
	row          repository.Repository
}

// newSettingsHarness prepares an owner with admin level on one repository and a
// second account that can be granted access.
func newSettingsHarness(t *testing.T) *settingsHarness {
	t.Helper()
	const (
		ownerID = "01920000-0000-7000-8000-0000000000e1"
		otherID = "01920000-0000-7000-8000-0000000000e2"
	)
	row := repository.Repository{
		ID: "01920000-0000-7000-8000-0000000000e3", OwnerID: ownerID, Name: "settings-repo",
		Description: "a repository", Visibility: repository.VisibilityPrivate,
		DefaultBranch: "main", StorageID: "01920000-0000-7000-8000-0000000000e4",
	}

	repositories := newFakeRepositories()
	repositories.add(row, repository.LevelAdmin)

	alice := identity.User{ID: ownerID, Username: "alice", Status: identity.StatusActive}
	bob := identity.User{ID: otherID, Username: "bob", Status: identity.StatusActive}
	owners := &fakeOwners{
		users: map[string]identity.User{"alice": alice, "bob": bob},
		byID:  map[string]identity.User{ownerID: alice, otherID: bob},
	}

	// A session that resolves to the owner, so a state-changing request can carry
	// the CSRF token a session-bound check requires.
	accounts := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return alice, identity.Session{ID: "session"}, identity.HashToken(csrfTokenValue), nil
		},
	}

	mux := http.NewServeMux()
	api.RegisterSettings(mux, accounts, owners, repositories)

	return &settingsHarness{
		t: t, mux: mux, repositories: repositories, owners: owners,
		ownerID: ownerID, otherID: otherID, row: row,
	}
}

// csrfTokenValue is the CSRF token the harness's session is bound to.
const csrfTokenValue = "settings-csrf-token"

// do dispatches a request straight to the handler.
//
// It is used rather than a real client because the harness needs to set a session
// cookie and a CSRF header on every state-changing request, and because a
// response is asserted on its raw body.
func (h *settingsHarness) do(method, path, body string, authenticated bool) *httptest.ResponseRecorder {
	h.t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if authenticated {
		request.AddCookie(&http.Cookie{Name: api.SessionCookie, Value: "any"})
		request.Header.Set(api.CSRFTokenHeader, csrfTokenValue)
	}
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

// decodeInto reads a JSON body from a recorder.
func decodeInto(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
}

// TestSettingsChangeAppliesAndReportsTheResource covers the happy path.
func TestSettingsChangeAppliesAndReportsTheResource(t *testing.T) {
	h := newSettingsHarness(t)

	recorder := h.do(http.MethodPatch, "/api/v1/repos/alice/settings-repo",
		`{"default_branch":"develop"}`, true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}

	var body map[string]any
	decodeInto(t, recorder, &body)
	if body["default_branch"] != "develop" {
		t.Errorf("default_branch = %v, want the new branch", body["default_branch"])
	}
	if body["permission"] != repository.LevelAdmin.String() {
		t.Errorf("permission = %v, want admin", body["permission"])
	}
	if h.repositories.lastUpdate.DefaultBranch == nil || *h.repositories.lastUpdate.DefaultBranch != "develop" {
		t.Errorf("the service received %+v, want the branch", h.repositories.lastUpdate)
	}
}

// TestSettingsChangeRequiresAdministration is the denied path: a caller who may
// read but not administer is refused.
func TestSettingsChangeRequiresAdministration(t *testing.T) {
	for _, level := range []repository.Level{repository.LevelNone, repository.LevelRead, repository.LevelWrite} {
		t.Run(level.String(), func(t *testing.T) {
			h := newSettingsHarness(t)
			h.repositories.levels[h.row.ID] = level

			recorder := h.do(http.MethodPatch, "/api/v1/repos/alice/settings-repo",
				`{"default_branch":"develop"}`, true)
			// A caller with no access and a caller who may read but not administer
			// are answered the same way, so the endpoint does not disclose which
			// they are.
			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", recorder.Code)
			}
			if h.repositories.lastUpdate.DefaultBranch != nil {
				t.Error("the service was asked to change something it refused")
			}
		})
	}
}

// TestSettingsChangeRequiresCSRF keeps the state-changing control in force.
func TestSettingsChangeRequiresCSRF(t *testing.T) {
	h := newSettingsHarness(t)

	// A request without the session and token is unauthenticated.
	if got := h.do(http.MethodPatch, "/api/v1/repos/alice/settings-repo",
		`{"default_branch":"develop"}`, false).Code; got != http.StatusUnauthorized {
		t.Errorf("without a session status = %d, want 401", got)
	}

	// A session without the CSRF token is refused before anything changes.
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/settings-repo",
		strings.NewReader(`{"default_branch":"develop"}`))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: api.SessionCookie, Value: "any"})
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Errorf("without CSRF status = %d, want 403", recorder.Code)
	}
	if h.repositories.lastUpdate.DefaultBranch != nil {
		t.Error("a request without CSRF reached the service")
	}
}

// TestSettingsRejectsAnEmptyRequest keeps a no-op from looking like a success.
func TestSettingsRejectsAnEmptyRequest(t *testing.T) {
	h := newSettingsHarness(t)
	recorder := h.do(http.MethodPatch, "/api/v1/repos/alice/settings-repo", `{}`, true)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", recorder.Code, recorder.Body.String())
	}
}

// TestSettingsReportsEveryRejectedField covers the envelope's field-level part:
// a request with two mistakes reports both, so one round trip is enough.
func TestSettingsReportsEveryRejectedField(t *testing.T) {
	h := newSettingsHarness(t)
	h.repositories.updateErr = repository.ValidationErrors{
		{Field: repository.FieldDefaultBranch, Message: "branch name cannot contain \"..\""},
		{Field: repository.FieldArchived, Message: "the archived value is not valid"},
	}

	recorder := h.do(http.MethodPatch, "/api/v1/repos/alice/settings-repo", `{"default_branch":"a..b"}`, true)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", recorder.Code, recorder.Body.String())
	}

	var body struct {
		Message string `json:"message"`
		Errors  []struct {
			Field   string `json:"field"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	decodeInto(t, recorder, &body)
	if len(body.Errors) != 2 {
		t.Fatalf("the response carries %d field errors, want 2: %s", len(body.Errors), recorder.Body.String())
	}
	if body.Errors[0].Field != repository.FieldDefaultBranch || body.Errors[1].Field != repository.FieldArchived {
		t.Errorf("the fields are %q and %q, want both named", body.Errors[0].Field, body.Errors[1].Field)
	}
}

// TestVisibilityConfirmationIsRequiredAndExplained covers the confirmation rule
// at the transport: the refusal names the field and tells the caller what to
// supply.
func TestVisibilityConfirmationIsRequiredAndExplained(t *testing.T) {
	h := newSettingsHarness(t)
	h.repositories.updateErr = repository.ErrConfirmationRequired

	recorder := h.do(http.MethodPatch, "/api/v1/repos/alice/settings-repo",
		`{"visibility":"public"}`, true)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", recorder.Code, recorder.Body.String())
	}

	var body struct {
		Errors []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		} `json:"errors"`
	}
	decodeInto(t, recorder, &body)
	if len(body.Errors) == 0 {
		t.Fatalf("the refusal carries no field: %s", recorder.Body.String())
	}
	if body.Errors[0].Field != repository.FieldConfirmation {
		t.Errorf("field = %q, want %q", body.Errors[0].Field, repository.FieldConfirmation)
	}
	if body.Errors[0].Code != "confirmation_required" {
		t.Errorf("code = %q, want confirmation_required", body.Errors[0].Code)
	}
}

// TestArchivedRefusalIsForbiddenNotNotFound pins the deliberate difference: the
// caller held the level, so the answer names the state rather than pretending the
// repository is absent.
func TestArchivedRefusalIsForbiddenNotNotFound(t *testing.T) {
	h := newSettingsHarness(t)
	h.repositories.updateErr = repository.ErrArchived

	recorder := h.do(http.MethodPatch, "/api/v1/repos/alice/settings-repo",
		`{"default_branch":"develop"}`, true)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "archived") {
		t.Errorf("the message does not name the state: %s", recorder.Body.String())
	}
}

// TestUnknownRepositoryIsNotFound keeps the settings surface from disclosing
// whether a repository exists.
func TestUnknownRepositoryIsNotFound(t *testing.T) {
	h := newSettingsHarness(t)

	for _, path := range []string{
		"/api/v1/repos/alice/no-such-repo",
		"/api/v1/repos/nobody/settings-repo",
		"/api/v1/repos/alice/-bad",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := h.do(http.MethodPatch, path, `{"default_branch":"develop"}`, true)
			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", recorder.Code)
			}
			// A denial must not be remembered.
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

// TestCollaboratorGrantAndRevoke covers both directions, including the
// created-versus-updated distinction.
func TestCollaboratorGrantAndRevoke(t *testing.T) {
	h := newSettingsHarness(t)

	// A first grant creates the row.
	granted := h.do(http.MethodPut, "/api/v1/repos/alice/settings-repo/collaborators/bob",
		`{"role_name":"write"}`, true)
	if granted.Code != http.StatusCreated {
		t.Fatalf("a new grant status = %d, want 201: %s", granted.Code, granted.Body.String())
	}
	var body map[string]any
	decodeInto(t, granted, &body)
	if body["login"] != "bob" || body["role_name"] != "write" {
		t.Errorf("body = %v, want the grant", body)
	}

	// A second grant changes the role rather than creating one.
	updated := h.do(http.MethodPut, "/api/v1/repos/alice/settings-repo/collaborators/bob",
		`{"role_name":"admin"}`, true)
	if updated.Code != http.StatusOK {
		t.Fatalf("a role change status = %d, want 200: %s", updated.Code, updated.Body.String())
	}

	// The listing reports it.
	listed := h.do(http.MethodGet, "/api/v1/repos/alice/settings-repo/collaborators", "", true)
	if listed.Code != http.StatusOK {
		t.Fatalf("a listing status = %d, want 200: %s", listed.Code, listed.Body.String())
	}
	var collaborators []map[string]any
	decodeInto(t, listed, &collaborators)
	if len(collaborators) != 1 || collaborators[0]["login"] != "bob" || collaborators[0]["role_name"] != "admin" {
		t.Errorf("the listing = %v, want one admin grant for bob", collaborators)
	}

	// A revocation removes it and answers with no content.
	revoked := h.do(http.MethodDelete, "/api/v1/repos/alice/settings-repo/collaborators/bob", "", true)
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("a revocation status = %d, want 204: %s", revoked.Code, revoked.Body.String())
	}
	after := h.do(http.MethodGet, "/api/v1/repos/alice/settings-repo/collaborators", "", true)
	var remaining []map[string]any
	decodeInto(t, after, &remaining)
	if len(remaining) != 0 {
		t.Errorf("the listing = %v, want nothing after the revocation", remaining)
	}
}

// TestCollaboratorGrantAcceptsTheGitHubVocabulary keeps a client written against
// the GitHub role names working.
func TestCollaboratorGrantAcceptsTheGitHubVocabulary(t *testing.T) {
	for _, test := range []struct {
		supplied string
		want     string
	}{
		{supplied: "read", want: "read"},
		{supplied: "pull", want: "read"},
		{supplied: "write", want: "write"},
		{supplied: "push", want: "write"},
		{supplied: "admin", want: "admin"},
		{supplied: "ADMIN", want: "admin"},
		{supplied: " admin ", want: "admin"},
	} {
		t.Run(test.supplied, func(t *testing.T) {
			h := newSettingsHarness(t)
			recorder := h.do(http.MethodPut, "/api/v1/repos/alice/settings-repo/collaborators/bob",
				`{"role_name":"`+test.supplied+`"}`, true)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", recorder.Code, recorder.Body.String())
			}
			var body map[string]any
			decodeInto(t, recorder, &body)
			if body["role_name"] != test.want {
				t.Errorf("role = %v, want %q", body["role_name"], test.want)
			}
		})
	}

	// Ownership is refused with a message that says why, because a client asking
	// for it has a real misunderstanding to correct.
	h := newSettingsHarness(t)
	refused := h.do(http.MethodPut, "/api/v1/repos/alice/settings-repo/collaborators/bob",
		`{"role_name":"owner"}`, true)
	if refused.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an owner grant status = %d, want 422", refused.Code)
	}
	if !strings.Contains(refused.Body.String(), "ownership") {
		t.Errorf("the refusal does not explain: %s", refused.Body.String())
	}

	// An unrecognised role is refused too.
	if got := h.do(http.MethodPut, "/api/v1/repos/alice/settings-repo/collaborators/bob",
		`{"role_name":"superuser"}`, true).Code; got != http.StatusUnprocessableEntity {
		t.Errorf("an unknown role status = %d, want 422", got)
	}
}

// TestCollaboratorMutationRequiresCSRF keeps the control on both directions.
func TestCollaboratorMutationRequiresCSRF(t *testing.T) {
	h := newSettingsHarness(t)

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			request := httptest.NewRequest(method, "/api/v1/repos/alice/settings-repo/collaborators/bob",
				strings.NewReader(`{"role_name":"read"}`))
			request.Header.Set("Content-Type", "application/json")
			request.AddCookie(&http.Cookie{Name: api.SessionCookie, Value: "any"})
			recorder := httptest.NewRecorder()
			h.mux.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 without a CSRF token", recorder.Code)
			}
		})
	}
	if len(h.repositories.grants[h.row.ID]) != 0 {
		t.Error("a request without CSRF changed the grants")
	}
}

// TestUnknownCollaboratorIsNotFound keeps the endpoint from enumerating accounts.
func TestUnknownCollaboratorIsNotFound(t *testing.T) {
	h := newSettingsHarness(t)

	// A name that does not resolve is answered as not-found. The malformed shapes
	// are exercised through the owner segment rather than a URL, because a URL
	// containing a space is rejected by the request builder before the handler
	// sees it — which is a different layer's behaviour and belongs in a test of
	// that layer.
	for _, username := range []string{"nobody", "-bad", "no-such-account"} {
		t.Run(username, func(t *testing.T) {
			recorder := h.do(http.MethodPut,
				"/api/v1/repos/alice/settings-repo/collaborators/"+username, `{"role_name":"read"}`, true)
			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", recorder.Code)
			}
		})
	}
}

// TestCollaboratorListingRequiresAdministration covers the listing's denied path.
func TestCollaboratorListingRequiresAdministration(t *testing.T) {
	h := newSettingsHarness(t)
	h.repositories.levels[h.row.ID] = repository.LevelRead

	// The level check in the transport refuses before the service is consulted.
	recorder := h.do(http.MethodGet, "/api/v1/repos/alice/settings-repo/collaborators", "", true)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", recorder.Code)
	}
}

// TestPermissionLookupReportsNoneRatherThanFailing covers an account with no
// access: the caller may already list the grants, so reporting `none` discloses
// nothing they could not read.
//
// The account is given an explicit level of none, because a repository that
// answered with the same level for every caller would make this assertion
// meaningless: the answer has to depend on who is being asked about.
func TestPermissionLookupReportsNoneRatherThanFailing(t *testing.T) {
	h := newSettingsHarness(t)
	h.repositories.withViewerLevel(h.row.ID, h.otherID, repository.LevelNone)

	recorder := h.do(http.MethodGet, "/api/v1/repos/alice/settings-repo/collaborators/bob/permission", "", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	decodeInto(t, recorder, &body)
	if body["permission"] != repository.LevelNone.String() {
		t.Errorf("permission = %v, want none", body["permission"])
	}
}

// TestPermissionLookupReportsTheAccountThatHolds is the other half: an account
// with a grant is reported with the grant it holds, not with the caller's level.
func TestPermissionLookupReportsTheAccountThatHolds(t *testing.T) {
	h := newSettingsHarness(t)
	h.repositories.withViewerLevel(h.row.ID, h.otherID, repository.LevelWrite)

	recorder := h.do(http.MethodGet, "/api/v1/repos/alice/settings-repo/collaborators/bob/permission", "", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	decodeInto(t, recorder, &body)
	if body["permission"] != repository.LevelWrite.String() {
		t.Errorf("permission = %v, want write; the answer must depend on the account asked about", body["permission"])
	}
}

// TestUnmappedServiceFailureIsAServerError keeps an unexpected error from looking
// like the caller's mistake.
func TestUnmappedServiceFailureIsAServerError(t *testing.T) {
	h := newSettingsHarness(t)
	h.repositories.updateErr = errors.New("the connection was reset")

	recorder := h.do(http.MethodPatch, "/api/v1/repos/alice/settings-repo",
		`{"default_branch":"develop"}`, true)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", recorder.Code, recorder.Body.String())
	}
	// The message is not echoed: a driver's text can name a host or a table.
	if strings.Contains(recorder.Body.String(), "connection was reset") {
		t.Errorf("the response echoes the internal error: %s", recorder.Body.String())
	}
}

// TestSettingsRejectsAnUnexpectedField keeps a privilege-shaped body from being
// accepted and ignored.
func TestSettingsRejectsAnUnexpectedField(t *testing.T) {
	h := newSettingsHarness(t)
	recorder := h.do(http.MethodPatch, "/api/v1/repos/alice/settings-repo",
		`{"default_branch":"develop","owner_id":"someone-else"}`, true)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", recorder.Code, recorder.Body.String())
	}
	if h.repositories.lastUpdate.DefaultBranch != nil {
		t.Error("a body naming an unexpected field was applied")
	}
}
