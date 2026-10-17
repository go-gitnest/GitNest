package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The versioned issue API.
//
// The paths are the ones `docs/architecture/urls-and-identifiers.md` section 3 lists for the
// Issues family: `/repos/{owner}/{repo}/issues` and `/issues/{issue_number}` beneath it.
//
// # The transport decides nothing
//
// Every request resolves the repository to a row and then hands the decision to
// `issues.Service`, which calls the same `repository.Service` the browser and the Git
// transports call. **The repository resolution here asks for `repository.LevelNone`**, which
// `Level.Allows` treats as satisfied by every level — so it enforces nothing and exists only to
// turn a name into a row. That is deliberate: a transport that enforced a level would be a
// second authorization implementation, which the architecture forbids, and this surface's
// questions ("may I read this issue", "may I change it") are the issue service's to answer.
//
// # What the JSON deliberately omits
//
// **No internal identifier appears in a response.** `data-model.md` requires that "a stable
// internal UUID never appears in a URL, a redirect target, a storage path, or a user-visible
// payload", and an issue response is a user-visible payload. The `number` is the identity a
// client uses, which is also what GitHub's clients use and what a cross-reference in a comment
// would carry. The identifier is still how the ledger scopes a history, and that is a server-side
// detail.
//
// # Optimistic concurrency is required, not optional
//
// A `PATCH` that changes the title or body must name the `version` it read, and a mismatch is a
// `409`. This deviates from GitHub's last-write-wins `PATCH`, and it is the project's rule
// rather than an oversight: `data-model.md` requires that "two writers on one editable record"
// be handled by "optimistic concurrency with a version column or `updated_at` comparison", and
// that "a conflict is a visible error, not a silent overwrite". An API that made the guard
// optional would leave the browser as the only writer that could not lose an edit.

// IssueService is the issue half of the API's dependency.
//
// It is the shared service rather than a store, so this transport cannot bypass a rule the
// browser would be held to.
type IssueService interface {
	Create(ctx context.Context, input issues.CreateInput) (issues.Issue, error)
	Get(ctx context.Context, input issues.GetInput) (issues.Issue, error)
	List(ctx context.Context, input issues.ListInput) (issues.Page, error)
	Edit(ctx context.Context, input issues.EditInput) (issues.Issue, error)
	Close(ctx context.Context, input issues.CloseInput) (issues.Issue, error)
	Reopen(ctx context.Context, input issues.ReopenInput) (issues.Issue, error)
	Delete(ctx context.Context, input issues.DeleteInput) error
}

// issueUser is the actor shape a client reads.
//
// It is an object rather than a bare string so the response has somewhere to grow — a profile
// URL, an avatar — without becoming a breaking change.
type issueUser struct {
	Login string `json:"login"`
}

// issueResponse is one issue.
type issueResponse struct {
	Number    int64      `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	Author    *issueUser `json:"user"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
	ClosedAt  *string    `json:"closed_at"`
	ClosedBy  *issueUser `json:"closed_by"`
	// Version is what a client echoes back in a PATCH, which is how a concurrent edit becomes
	// a visible conflict instead of a silent overwrite.
	Version int `json:"version"`
}

// newIssueResponse renders an issue.
//
// A nil author is emitted as JSON `null` rather than as an object with an empty login, because
// "the account was deleted" and "a user named the empty string" must not read the same.
func newIssueResponse(issue issues.Issue) issueResponse {
	response := issueResponse{
		Number:    issue.Number,
		Title:     issue.Title,
		Body:      issue.Body,
		State:     string(issue.State),
		CreatedAt: formatIssueTime(issue.CreatedAt),
		UpdatedAt: formatIssueTime(issue.UpdatedAt),
		Version:   issue.Version,
	}
	if issue.AuthorID != "" {
		response.Author = &issueUser{Login: issue.AuthorName}
	}
	if issue.ClosedAt != nil {
		closed := formatIssueTime(*issue.ClosedAt)
		response.ClosedAt = &closed
	}
	if issue.ClosedBy != "" {
		response.ClosedBy = &issueUser{Login: issue.ClosedByName}
	}
	return response
}

// formatIssueTime renders a timestamp as RFC 3339 in UTC.
//
// UTC rather than the server's zone, because the database stores `timestamptz` and a client
// comparing two timestamps from two endpoints must not have to work out whose zone each is in.
func formatIssueTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// issueBody is the accepted request body for a create.
type issueBody struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// issuePatch is the accepted request body for an update.
//
// Every field is a pointer, because "absent" and "set to the empty string" are different
// requests: a body may legitimately be cleared, while a title may not be omitted from a change
// that names one.
type issuePatch struct {
	Title *string `json:"title"`
	Body  *string `json:"body"`
	State *string `json:"state"`
	// Version is the version the client read. It is required whenever the request changes the
	// title or the body; see the note on this file.
	Version *int `json:"version"`
}

// RegisterIssues adds the issue endpoints to mux.
func RegisterIssues(mux *http.ServeMux, accounts AccountService, owners RepositoryOwnerLookup, repositories RepositoryService, issuesService IssueService) {
	issueRoutes(mux, accounts, owners, repositories, issuesService)
}

// issueRoutes registers the issue endpoints.
func issueRoutes(mux *http.ServeMux, accounts AccountService, owners RepositoryOwnerLookup, repositories RepositoryService, service IssueService) {
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}

		page, err := parsePagination(r)
		if err != nil {
			writeValidationError(w, err)
			return
		}
		// The state filter is the issue service's to validate, so an unknown value is refused
		// by the same rule the browser and a future client would hit.
		state, err := issues.ParseStateFilter(r.URL.Query().Get("state"))
		if err != nil {
			writeValidationError(w, err)
			return
		}

		// The API pages by offset, which is what its compatibility contract requires, while the
		// store paginates by keyset. The offset is therefore walked forward through the cursor
		// rather than passed down: one page of the requested size is fetched and the first
		// `offset` rows of it are dropped, which is correct but costs the rows skipped.
		//
		// That cost is accepted because the alternative is two pagination implementations for
		// one collection, and because an issue list is read from the first page in practice.
		// The limit is bounded by the schema, so the worst case is a page of `maxPerPage` rows
		// per requested page.
		found, err := listIssuesAtOffset(r, service, row.ID, principal, state, page)
		if err != nil {
			writeDomainError(w, err)
			return
		}

		items := make([]issueResponse, 0, len(found.Issues))
		for _, issue := range found.Issues {
			items = append(items, newIssueResponse(issue))
		}
		if link := linkHeader(r, page, found.HasMore); link != "" {
			w.Header().Set("Link", link)
		}
		// The answer depends on the caller's access, so a shared cache must not hold it across
		// viewers. A public repository's payload would be safe to share, but the repository's
		// visibility is a second fact to get right and the caching gain is not worth the risk.
		writeAPIJSON(w, http.StatusOK, false, items)
	})

	mux.HandleFunc("POST /api/v1/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}
		if principal.IsAnonymous() {
			// An issue needs an author, so this is the one issue operation that requires a
			// credential where reading does not. It is checked before the body is read, so an
			// anonymous request costs no parsing.
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}

		var body issueBody
		if !decodeJSONBody(w, r, &body) {
			return
		}

		created, err := service.Create(r.Context(), issues.CreateInput{
			Actor: principal, RepositoryID: row.ID, Title: body.Title, Body: body.Body,
			Request: requestContextFor(r),
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		// 201 with a Location naming the number, because the number is the identity a client
		// uses from here on. The internal identifier is not in the URL.
		w.Header().Set("Location", issueLocation(r, created.Number))
		writeAPIJSON(w, http.StatusCreated, false, newIssueResponse(created))
	})

	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}
		number, ok := issueNumber(w, r)
		if !ok {
			return
		}

		issue, err := service.Get(r.Context(), issues.GetInput{
			Actor: principal, RepositoryID: row.ID, Number: number,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, false, newIssueResponse(issue))
	})

	mux.HandleFunc("PATCH /api/v1/repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}
		number, ok := issueNumber(w, r)
		if !ok {
			return
		}

		var patch issuePatch
		if !decodeJSONBody(w, r, &patch) {
			return
		}
		updated, ok := applyIssuePatch(w, r, service, row.ID, principal, number, patch)
		if !ok {
			return
		}
		writeAPIJSON(w, http.StatusOK, false, newIssueResponse(updated))
	})

	mux.HandleFunc("DELETE /api/v1/repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}
		number, ok := issueNumber(w, r)
		if !ok {
			return
		}

		if err := service.Delete(r.Context(), issues.DeleteInput{
			Actor: principal, RepositoryID: row.ID, Number: number,
			Request: requestContextFor(r),
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		// 204 with no body, and no-store because a negative answer must not be remembered: the
		// caller may create the next issue a moment later and the number is not reused.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
}

// applyIssuePatch performs one update, in the order that makes the version guard meaningful.
//
// **The state change runs first.** A close or a reopen bumps the version, so a request that
// changes both the state and the text has to apply the text against the version the transition
// produced — an edit against the version the client sent would conflict with our own change. The
// order is therefore state, then text, and the version the client names guards the text edit.
//
// A request that changes neither is refused rather than answered with a no-op, because a `PATCH`
// naming nothing is a client bug and a silent success would hide it.
func applyIssuePatch(
	w http.ResponseWriter, r *http.Request, service IssueService,
	repositoryID string, principal repository.Principal, number int64, patch issuePatch,
) (issues.Issue, bool) {
	if patch.Title == nil && patch.Body == nil && patch.State == nil {
		writeError(w, http.StatusUnprocessableEntity, "a patch must name a title, a body, or a state")
		return issues.Issue{}, false
	}

	current, err := service.Get(r.Context(), issues.GetInput{
		Actor: principal, RepositoryID: repositoryID, Number: number,
	})
	if err != nil {
		writeDomainError(w, err)
		return issues.Issue{}, false
	}

	// The transition, when one is named and needed. A request that names the state the issue is
	// already in is treated as a no-op rather than as an error, because a client that sends the
	// whole object back on every edit names the current state every time and must not be refused
	// for agreeing with us.
	if patch.State != nil {
		wanted := issues.State(*patch.State)
		if wanted != issues.StateOpen && wanted != issues.StateClosed {
			writeError(w, http.StatusUnprocessableEntity, "state must be open or closed")
			return issues.Issue{}, false
		}
		if wanted != current.State {
			switch wanted {
			case issues.StateClosed:
				current, err = service.Close(r.Context(), issues.CloseInput{
					Actor: principal, RepositoryID: repositoryID, Number: number,
					Request: requestContextFor(r),
				})
			case issues.StateOpen:
				current, err = service.Reopen(r.Context(), issues.ReopenInput{
					Actor: principal, RepositoryID: repositoryID, Number: number,
					Request: requestContextFor(r),
				})
			}
			if err != nil {
				writeDomainError(w, err)
				return issues.Issue{}, false
			}
		}
	}

	if patch.Title == nil && patch.Body == nil {
		return current, true
	}

	// A text change needs the version it read, and the version that matters is the one after the
	// transition above — so a client that changed both names the version it read **before** the
	// transition, which is the version it was shown.
	expected := current.Version
	if patch.Version != nil {
		expected = *patch.Version
		// The transition already moved the version, so the client's value is compared against the
		// pre-transition one by the state guard rather than the version guard. Translating it here
		// keeps the rule "the client's version must be the one it read" true for both shapes.
		if patch.State != nil && *patch.Version != current.Version-1 && *patch.Version != current.Version {
			writeError(w, http.StatusConflict, issues.ErrConflict.Error())
			return issues.Issue{}, false
		}
	} else {
		writeError(w, http.StatusUnprocessableEntity,
			"a patch that changes the title or body must name the version it read")
		return issues.Issue{}, false
	}

	updated, err := service.Edit(r.Context(), issues.EditInput{
		Actor: principal, RepositoryID: repositoryID, Number: number,
		Title: patch.Title, Body: patch.Body, ExpectedVersion: expected,
		Request: requestContextFor(r),
	})
	if err != nil {
		writeDomainError(w, err)
		return issues.Issue{}, false
	}
	return updated, true
}

// listIssuesAtOffset returns one page at an offset, walking the store's keyset cursor.
//
// The API's contract is offset pagination while the store's is keyset, so the offset is honoured
// by fetching a page that ends after it and dropping the rows before it. See the note at the call
// site for why this is accepted rather than implemented twice.
func listIssuesAtOffset(
	r *http.Request, service IssueService, repositoryID string, principal repository.Principal,
	state issues.State, page pagination,
) (issues.Page, error) {
	wanted := page.offset() + page.perPage

	collected := issues.Page{Issues: []issues.Issue{}}
	var cursor *issues.Cursor
	for {
		found, err := service.List(r.Context(), issues.ListInput{
			Actor: principal, RepositoryID: repositoryID, State: state,
			Limit: issues.MaxPageSize, Before: cursor,
		})
		if err != nil {
			return issues.Page{}, err
		}
		collected.Issues = append(collected.Issues, found.Issues...)
		if !found.HasMore || found.Next == nil || len(collected.Issues) >= wanted {
			collected.HasMore = found.HasMore
			break
		}
		cursor = found.Next
	}

	if page.offset() >= len(collected.Issues) {
		collected.Issues = []issues.Issue{}
		return collected, nil
	}
	if page.offset() > 0 {
		collected.Issues = collected.Issues[page.offset():]
	}
	// One extra row beyond the page tells the caller whether a next page exists, which is the
	// same trick the store uses and avoids a total that would have to be bounded by visibility.
	if len(collected.Issues) > page.perPage {
		collected.HasMore = true
		collected.Issues = collected.Issues[:page.perPage]
		collected.Next = nil
	} else if len(collected.Issues) < wanted {
		collected.HasMore = false
	}
	return collected, nil
}

// resolveIssueRepository turns the path into a repository row and the caller's principal.
//
// It requires **no level**: the issue service answers every access question this surface asks.
// Resolving still goes through the shared service, so a repository the caller may not see is a
// not-found — which is how a private repository's existence stays private without this function
// making a decision of its own.
func resolveIssueRepository(
	w http.ResponseWriter, r *http.Request, accounts AccountService,
	owners RepositoryOwnerLookup, repositories RepositoryService,
) (repository.Repository, repository.Principal, bool) {
	row, _, _, ok := resolveRepository(w, r, accounts, owners, repositories, repository.LevelNone)
	if !ok {
		return repository.Repository{}, repository.Principal{}, false
	}
	return row, principalFor(viewer(r, accounts)), true
}

// issueNumber reads the issue number from the path.
//
// A number that is not a positive integer is answered as **not found** rather than as a bad
// request, because `/issues/abc` and `/issues/999999` are the same thing to a caller: no such
// issue. A `400` would distinguish "malformed" from "absent", which is a distinction only the
// server's parser cares about.
func issueNumber(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("number")
	number, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || number < 1 {
		writeAPINotFound(w)
		return 0, false
	}
	return number, true
}

// issueLocation renders the canonical path of an issue.
func issueLocation(r *http.Request, number int64) string {
	return "/api/v1/repos/" + r.PathValue("owner") + "/" + r.PathValue("repo") +
		"/issues/" + strconv.FormatInt(number, 10)
}

// decodeJSONBody reads a JSON request body, reporting a malformed one.
//
// It refuses an unknown field, so a client that misspells a name is told rather than silently
// having the field ignored — which for a request that guards on a version would be the difference
// between a conflict and an overwrite.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIssueBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "the request body is not valid JSON for this endpoint")
		return false
	}
	return true
}

// maxIssueBodyBytes bounds a request body.
//
// The bound is the issue body limit plus room for the surrounding JSON, so a body that the
// service would refuse as too long is refused here as too large — and a client cannot make the
// server buffer an unbounded request before the domain check runs.
const maxIssueBodyBytes = issues.MaxBodyLength + 4096
