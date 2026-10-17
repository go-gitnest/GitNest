package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-gitnest/GitNest/internal/comments"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The versioned comment API.
//
// The paths are the nested ones `docs/architecture/urls-and-identifiers.md` section 3 lists for the
// Issues family: `/issues/{issue_number}/comments` beneath an issue.
//
// # The transport decides nothing
//
// Every request resolves the repository to a row and then hands the decision to `comments.Service`,
// which calls the same `repository.Service` the browser and the Git transports call. The repository
// resolution asks for `repository.LevelNone` — which `Level.Allows` treats as satisfied by every
// level, so it enforces nothing and exists only to turn a name into a row.
//
// # A comment is addressed by number within its issue, not by identifier
//
// `urls-and-identifiers.md` L29 permits "names, slugs, **numbers**, refs, and content digests" in a
// URL and requires that "a stable internal UUID never appears in a URL, a redirect target, a storage
// path, or a user-visible payload". L159's `{id}` list names installation, run, job, artifact,
// delivery, and workspace resources — and **not** comments.
//
// So a comment is addressed by its position in its issue. The position is **derived, not allocated**:
// the order is `(created_at, id)`, which is total, and a deletion is soft so the row keeps its place —
// therefore the rank is stable for the life of the conversation, and a counter would be a second
// scheme to keep in step with an order that already exists.
//
// # One documented path is deliberately not registered
//
// Section 3 also lists a flat `/issues/comments/{comment_id}`, which addresses a comment without its
// issue. Registering it needs an answer the documents do not give: with no issue in the path, a number
// would have to be unique across the repository — which means either a repository-scoped comment
// counter (a schema decision, and the issue numbering work put its counter on the repository for the
// same reason) or a global per-instance identifier, which is what GitHub uses. Both are contract
// decisions rather than implementation details, so the path is left unregistered and the question is
// recorded in the roadmap instead of being settled by inventing a scheme.

// CommentService is the comment half of the API's dependency.
//
// It is the shared service rather than a store, so this transport cannot bypass a rule the browser
// would be held to.
type CommentService interface {
	Create(ctx context.Context, input comments.CreateInput) (comments.Comment, error)
	GetByNumber(ctx context.Context, input comments.GetByNumberInput) (comments.Comment, error)
	List(ctx context.Context, input comments.ListInput) (comments.Page, error)
	Edit(ctx context.Context, input comments.EditInput) (comments.Comment, error)
	Delete(ctx context.Context, input comments.DeleteInput) error
	History(ctx context.Context, input comments.HistoryInput) ([]comments.Revision, error)
}

// commentResponse is one comment.
type commentResponse struct {
	Number int        `json:"number"`
	Body   string     `json:"body"`
	Author *issueUser `json:"user"`
	// Deleted reports a soft deletion. A deleted comment's body is empty, which is why `deleted` has
	// to be read rather than inferred from the body.
	Deleted   bool   `json:"deleted"`
	CreatedAt string `json:"created_at"`
	// EditedAt is present only once a comment has been edited, so "edited" is a fact a client reads
	// rather than a value it computes.
	EditedAt *string `json:"edited_at"`
	// Version is what a client echoes back in a PATCH, which is how a concurrent edit becomes a
	// visible conflict rather than a silent overwrite.
	Version int `json:"version"`
}

// revisionResponse is one entry in a comment's edit history.
type revisionResponse struct {
	Revision int `json:"revision"`
	// Body is the body **as it was before that edit**, which is what "records the previous body"
	// means.
	Body      string     `json:"body"`
	Editor    *issueUser `json:"editor"`
	CreatedAt string     `json:"created_at"`
}

// newCommentResponse renders one comment.
func newCommentResponse(comment comments.Comment) commentResponse {
	response := commentResponse{
		Number:    comment.Number,
		Body:      comment.Body,
		Deleted:   comment.Deleted,
		CreatedAt: formatIssueTime(comment.CreatedAt),
		Version:   comment.Version,
	}
	if comment.AuthorID != "" {
		response.Author = &issueUser{Login: comment.AuthorName}
	}
	if comment.EditedAt != nil {
		edited := formatIssueTime(*comment.EditedAt)
		response.EditedAt = &edited
	}
	return response
}

// newRevisionResponse renders one history entry.
func newRevisionResponse(revision comments.Revision) revisionResponse {
	response := revisionResponse{
		Revision:  revision.Revision,
		Body:      revision.Body,
		CreatedAt: formatIssueTime(revision.CreatedAt),
	}
	if revision.EditorID != "" {
		response.Editor = &issueUser{Login: revision.EditorName}
	}
	return response
}

// commentBody is the accepted request body for a create.
type commentBody struct {
	Body string `json:"body"`
}

// commentPatch is the accepted request body for an edit.
type commentPatch struct {
	Body *string `json:"body"`
	// Version is the version the client read. It is required, because an edit that does not state what
	// it read cannot detect a concurrent change.
	Version *int `json:"version"`
}

// RegisterComments adds the comment endpoints to mux.
//
// The issue service is a dependency because a comment is addressed under its issue's **number**, so
// the issue's identifier has to be resolved — through the shared service, which checks the caller's
// read access before the identifier is known.
func RegisterComments(mux *http.ServeMux, accounts AccountService, owners RepositoryOwnerLookup, repositories RepositoryService, commentsService CommentService, issuesService IssueService) {
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request) {
		row, principal, issue, ok := resolveCommentIssue(w, r, accounts, owners, repositories, issuesService)
		if !ok {
			return
		}

		page, err := parsePagination(r)
		if err != nil {
			writeValidationError(w, err)
			return
		}

		found, err := commentsService.List(r.Context(), comments.ListInput{
			Actor: principal, RepositoryID: row.ID, IssueID: issue.ID,
			Limit: page.perPage, Before: commentCursor(r.URL.Query().Get("before")),
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}

		items := make([]commentResponse, 0, len(found.Comments))
		for _, comment := range found.Comments {
			items = append(items, newCommentResponse(comment))
		}
		// The next page is carried by a cursor rather than a page number, because the conversation
		// pages by keyset: a page number would have to be translated by walking from the start, and a
		// comment added while a reader pages would shift every later page.
		if found.HasMore && found.Next != nil {
			w.Header().Set("Link", commentLinkHeader(r, *found.Next))
		}
		writeAPIJSON(w, http.StatusOK, false, items)
	})

	mux.HandleFunc("POST /api/v1/repos/{owner}/{repo}/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request) {
		row, principal, issue, ok := resolveCommentIssue(w, r, accounts, owners, repositories, issuesService)
		if !ok {
			return
		}
		if principal.IsAnonymous() {
			// A comment needs an author, so this is the one comment operation that requires a
			// credential where reading does not.
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}

		var body commentBody
		if !decodeJSONBody(w, r, &body) {
			return
		}

		created, err := commentsService.Create(r.Context(), comments.CreateInput{
			Actor: principal, RepositoryID: row.ID, IssueID: issue.ID, Body: body.Body,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		w.Header().Set("Location", commentPath(r, created.Number))
		writeAPIJSON(w, http.StatusCreated, false, newCommentResponse(created))
	})

	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/issues/{number}/comments/{comment}", func(w http.ResponseWriter, r *http.Request) {
		// The resolution already read the comment through the service, so it **is** this request's
		// answer: reading it twice would be two queries for one request, and the first would be
		// discarded.
		_, _, _, found, ok := resolveOneComment(w, r, accounts, owners, repositories, commentsService, issuesService)
		if !ok {
			return
		}
		writeAPIJSON(w, http.StatusOK, false, newCommentResponse(found))
	})

	mux.HandleFunc("PATCH /api/v1/repos/{owner}/{repo}/issues/{number}/comments/{comment}", func(w http.ResponseWriter, r *http.Request) {
		_, principal, issue, existing, ok := resolveOneComment(w, r, accounts, owners, repositories, commentsService, issuesService)
		if !ok {
			return
		}

		var patch commentPatch
		if !decodeJSONBody(w, r, &patch) {
			return
		}
		if patch.Body == nil {
			writeError(w, http.StatusUnprocessableEntity, "a patch must name a body")
			return
		}
		if patch.Version == nil {
			writeError(w, http.StatusUnprocessableEntity, "a patch must name the version it read")
			return
		}

		updated, err := commentsService.Edit(r.Context(), comments.EditInput{
			Actor: principal, RepositoryID: issue.RepositoryID, IssueID: issue.ID,
			CommentID: existing.ID, Body: *patch.Body, ExpectedVersion: *patch.Version,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, false, newCommentResponse(updated))
	})

	mux.HandleFunc("DELETE /api/v1/repos/{owner}/{repo}/issues/{number}/comments/{comment}", func(w http.ResponseWriter, r *http.Request) {
		_, principal, issue, existing, ok := resolveOneComment(w, r, accounts, owners, repositories, commentsService, issuesService)
		if !ok {
			return
		}

		if err := commentsService.Delete(r.Context(), comments.DeleteInput{
			Actor: principal, RepositoryID: issue.RepositoryID, IssueID: issue.ID,
			CommentID: existing.ID, Request: requestContextFor(r),
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		// A negative answer must not be remembered: the comment still occupies its place, so a client
		// that re-reads sees the tombstone rather than a stale answer.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/issues/{number}/comments/{comment}/history", func(w http.ResponseWriter, r *http.Request) {
		_, principal, issue, existing, ok := resolveOneComment(w, r, accounts, owners, repositories, commentsService, issuesService)
		if !ok {
			return
		}

		history, err := commentsService.History(r.Context(), comments.HistoryInput{
			Actor: principal, RepositoryID: issue.RepositoryID, IssueID: issue.ID,
			CommentID: existing.ID,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		items := make([]revisionResponse, 0, len(history))
		for _, revision := range history {
			items = append(items, newRevisionResponse(revision))
		}
		writeAPIJSON(w, http.StatusOK, false, items)
	})
}

// resolveCommentIssue turns a path into a repository, a caller, and an issue.
func resolveCommentIssue(
	w http.ResponseWriter, r *http.Request, accounts AccountService,
	owners RepositoryOwnerLookup, repositories RepositoryService, issuesService IssueService,
) (repository.Repository, repository.Principal, issues.Issue, bool) {
	row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
	if !ok {
		return repository.Repository{}, repository.Principal{}, issues.Issue{}, false
	}
	number, ok := issueNumber(w, r)
	if !ok {
		return repository.Repository{}, repository.Principal{}, issues.Issue{}, false
	}

	// The issue is read through its own service, so the caller's read access is checked before the
	// issue's identifier is known, and an issue the caller may not reach produces the same not-found
	// the comment service would.
	issue, err := issuesService.Get(r.Context(), issues.GetInput{
		Actor: principal, RepositoryID: row.ID, Number: number,
	})
	if err != nil {
		writeDomainError(w, err)
		return repository.Repository{}, repository.Principal{}, issues.Issue{}, false
	}
	return row, principal, issue, true
}

// resolveOneComment resolves the issue and the comment at the requested position.
//
// It returns the **resolved comment**, so a handler that needs to change it has its identifier without
// a second read, and the one that only reads still gets the not-found shape for a number that does not
// exist.
func resolveOneComment(
	w http.ResponseWriter, r *http.Request, accounts AccountService,
	owners RepositoryOwnerLookup, repositories RepositoryService,
	service CommentService, issuesService IssueService,
) (repository.Repository, repository.Principal, issues.Issue, comments.Comment, bool) {
	row, principal, issue, ok := resolveCommentIssue(w, r, accounts, owners, repositories, issuesService)
	if !ok {
		return repository.Repository{}, repository.Principal{}, issues.Issue{}, comments.Comment{}, false
	}

	number := commentNumber(r)
	if number < 1 {
		// A number that is not a positive integer is the not-found answer rather than a bad request,
		// for the same reason the issue endpoints answer that way: `/comments/abc` and `/comments/999`
		// mean the same thing to a caller.
		writeAPINotFound(w)
		return repository.Repository{}, repository.Principal{}, issues.Issue{}, comments.Comment{}, false
	}

	found, err := service.GetByNumber(r.Context(), comments.GetByNumberInput{
		Actor: principal, RepositoryID: row.ID, IssueID: issue.ID, Number: number,
	})
	if err != nil {
		writeDomainError(w, err)
		return repository.Repository{}, repository.Principal{}, issues.Issue{}, comments.Comment{}, false
	}
	return row, principal, issue, found, true
}

// commentNumber reads the comment position from the path, returning 0 when it is not a number.
func commentNumber(r *http.Request) int {
	number, err := strconv.Atoi(r.PathValue("comment"))
	if err != nil || number < 1 {
		return 0
	}
	return number
}

// commentPath renders the canonical path of a comment.
func commentPath(r *http.Request, number int) string {
	return "/api/v1/repos/" + r.PathValue("owner") + "/" + r.PathValue("repo") +
		"/issues/" + r.PathValue("number") + "/comments/" + strconv.Itoa(number)
}

// commentCursor reads the `before` cursor from a listing request.
//
// A malformed cursor is ignored rather than refused, so a client that sends a stale one gets the first
// page — which is the behaviour a listing's cursor has everywhere else in this API.
func commentCursor(raw string) *comments.Cursor {
	if raw == "" {
		return nil
	}
	at, id, found := strings.Cut(raw, ",")
	if !found || id == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil
	}
	return &comments.Cursor{CreatedAt: parsed, ID: id}
}

// commentLinkHeader renders the next relation for a comment page.
func commentLinkHeader(r *http.Request, cursor comments.Cursor) string {
	base := *r.URL
	query := base.Query()
	query.Set("before", cursor.CreatedAt.UTC().Format(time.RFC3339Nano)+","+cursor.ID)
	base.RawQuery = query.Encode()
	return "<" + base.String() + `>; rel="next"`
}
