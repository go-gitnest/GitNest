package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-gitnest/GitNest/internal/comments"
	"github.com/go-gitnest/GitNest/internal/i18n"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The comment surfaces, rendered as forms on the issue page.
//
// The reasoning is the same as `issue_forms.go`, and `frontend.md` is again what decides it: "A page
// must be usable before hydration. The React islands enhance a working page; they do not build it."
// A conversation is part of the issue page, so its controls belong there as plain forms — which also
// means every comment operation works with scripting disabled and is verifiable in this environment.
//
// # One path, four actions
//
// As with issues, the operation travels in a form field rather than in a path, because an HTML form
// can send only `GET` and `POST`. The comment's own position is what identifies it, which is the same
// number a reader sees and the same one the API addresses — `urls-and-identifiers.md` L29 forbids a
// stable internal UUID in a URL.
//
// # The history is shown where the comment is
//
// W2.3 requires that "editing a comment records the previous body and does not rewrite history
// visible in the UI". So an edited comment shows its previous bodies inline, under the current one:
// a reader sees what changed rather than only that something did.

// The comment form actions.
const (
	commentActionWrite  = "comment.create"
	commentActionEdit   = "comment.edit"
	commentActionDelete = "comment.delete"
)

// commentFields are the form field names, which the templates and the handlers must agree on.
const (
	commentFieldID      = "comment_number"
	commentFieldBody    = "comment_body"
	commentFieldVersion = "comment_version"
)

// commentView is one rendered comment.
type commentView struct {
	Number     int
	AuthorName string
	// Body is W2.1's sanitized HTML, placed as element content only.
	//
	// **`template.HTML`, not `string`, and the type is load-bearing.** It is what tells
	// `html/template` not to escape the value again; converting the renderer's result to a string
	// loses that, and the page then shows the sanitized markup as visible text — `<p>A <strong>…`
	// rather than a paragraph with bold in it. The escaping was safe and the output was useless, which
	// is why this was caught by an assertion about what a reader sees rather than by a security test.
	Body template.HTML
	// Deleted reports a soft deletion, which the page shows as a tombstone so the conversation keeps
	// its shape.
	Deleted bool
	// Edited reports that the comment has been edited at least once.
	Edited    bool
	CreatedAt string
	EditedAt  string
	// History is the previous bodies, oldest first. It is empty for a comment that was never edited.
	History []commentRevisionView
	// CanChange reports whether this reader may edit the comment, and CanDelete whether they may
	// remove it. Both come from the shared decision.
	CanChange bool
	CanDelete bool
	// Version is the current version, which an edit carries so a concurrent edit conflicts rather
	// than overwriting.
	Version int
	// EditBody is what the reader submitted when an edit was refused, so their text is not lost.
	EditBody string
}

// commentRevisionView is one previous body.
type commentRevisionView struct {
	Revision int
	// Body is `template.HTML` for the same reason `commentView.Body` is.
	Body       template.HTML
	EditorName string
	CreatedAt  string
}

// commentFormView is what the page's comment forms need.
type commentFormView struct {
	// Action is where the comment forms post.
	//
	// **It is a different path from the issue's own form**, and that is not cosmetic: the comment
	// handler is registered on `.../issues/{number}/comments`, so a comment form that posted to the
	// issue's path would reach the *issue* update handler and be refused as an unknown action — which
	// is exactly the 400 the end-to-end tests caught.
	Action string
	CSRF   string
	// CanWrite reports whether the reader may add a comment, which needs an account.
	CanWrite bool
	// Body is what the reader submitted when a creation was refused.
	Body string
	// Error is a message for the whole comment area, or empty.
	Error string
	// BodyError is a message tied to the body field, or empty.
	BodyError string
	// EditNumber is the comment whose edit was refused, so its form is the one that carries the
	// message and the submitted text.
	EditNumber int
}

// WithComments gives the handler the comment service. It returns the handler so a caller can chain,
// and a nil service leaves the comment sections unrendered.
func (h *Handler) WithComments(service CommentService) *Handler {
	h.comments = service
	return h
}

// CommentService is the part of the comment service these pages use.
//
// It is the **shared service** rather than a store, so a page cannot bypass a rule the API would be
// held to: the access decision, the author allowance, and the version guard are applied by the same
// code whichever surface calls it.
type CommentService interface {
	Create(ctx context.Context, input comments.CreateInput) (comments.Comment, error)
	GetByNumber(ctx context.Context, input comments.GetByNumberInput) (comments.Comment, error)
	List(ctx context.Context, input comments.ListInput) (comments.Page, error)
	Edit(ctx context.Context, input comments.EditInput) (comments.Comment, error)
	Delete(ctx context.Context, input comments.DeleteInput) error
	History(ctx context.Context, input comments.HistoryInput) ([]comments.Revision, error)
}

// commentSection assembles the comments for an issue page.
//
// It returns the rendered comments and the form, and it **never fails the page**: an issue is
// readable even if its conversation cannot be loaded, so a failure leaves the section empty rather
// than turning the whole page into an error. That mirrors the history section's decision.
func (h *Handler) commentSection(c requestContext, view *resolved, issue issues.Issue, form issueFormView) ([]commentView, commentFormView) {
	section := commentFormView{CSRF: form.CSRF, Action: h.issueCommentsHref(view, issue.Number)}
	if h.comments == nil {
		return nil, section
	}
	// Only a reader who may change the issue may comment on it, because the two need the same access
	// and an account. The service decides; this only avoids rendering a form that would be refused.
	section.CanWrite = form.CSRF != "" && form.CanChange

	found, err := h.comments.List(c.request.Context(), comments.ListInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID,
		Limit: comments.MaxPageSize,
	})
	if err != nil {
		// A conversation the reader may not see is the same answer as none, which is what keeps a
		// private repository's comment count from leaking through the page.
		return nil, section
	}

	rendered := make([]commentView, 0, len(found.Comments))
	for _, comment := range found.Comments {
		row := commentView{
			Number:     comment.Number,
			AuthorName: comment.AuthorName,
			Deleted:    comment.Deleted,
			Edited:     comment.Edited(),
			CreatedAt:  formatPageTime(comment.CreatedAt),
			Version:    comment.Version,
		}
		// **A deleted comment's body is empty**, because the deletion cleared it, so there is nothing
		// to render and nothing to leak. The tombstone keeps the position.
		if !comment.Deleted {
			row.Body = h.markdown.Render(comment.Body)
		}
		if comment.EditedAt != nil {
			row.EditedAt = formatPageTime(*comment.EditedAt)
			// The previous bodies are rendered too, because "does not rewrite history visible in the
			// UI" is about what a reader sees rather than about what is stored.
			row.History = h.commentHistory(c, view, issue, comment)
		}
		row.CanChange, row.CanDelete = commentFacts(view.level, comment, c.viewer())
		rendered = append(rendered, row)
	}
	return rendered, section
}

// commentHistory renders a comment's previous bodies.
//
// A failure to read the history is not a failure of the page: the comment itself is readable, and
// losing the record of its edits would be worse than showing it without one. So the error is
// swallowed deliberately, and the comment renders as though it had not been edited — which is the
// same thing a never-edited comment shows.
func (h *Handler) commentHistory(c requestContext, view *resolved, issue issues.Issue, comment comments.Comment) []commentRevisionView {
	history, err := h.comments.History(c.request.Context(), comments.HistoryInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID, CommentID: comment.ID,
	})
	if err != nil {
		return nil
	}
	rendered := make([]commentRevisionView, 0, len(history))
	for _, revision := range history {
		rendered = append(rendered, commentRevisionView{
			Revision:   revision.Revision,
			Body:       h.markdown.Render(revision.Body),
			EditorName: revision.EditorName,
			CreatedAt:  formatPageTime(revision.CreatedAt),
		})
	}
	return rendered
}

// commentFacts reports whether a reader may edit a comment, and whether they may delete it.
//
// The answer comes from the shared decision rather than a page's own reasoning: the level is what the
// repository service returned for this caller, and the author rule is read off the comment. Editing
// and deleting have the **same** allowance here — the author or a repository writer — which is the
// catalogue's "deleted by its author or a moderator".
func commentFacts(level repository.Level, comment comments.Comment, actor repository.Principal) (canEdit, canDelete bool) {
	if comment.Deleted {
		// A deleted comment has no text to edit, and the service refuses a second deletion.
		return false, false
	}
	writer := level.Allows(repository.LevelWrite)
	allowed := writer || comment.AuthoredBy(actor.UserID)
	return allowed, allowed
}

// commentWrite handles a submitted comment form.
func (h *Handler) commentWrite(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}
	number, ok := formIssueNumber(c)
	if !ok {
		return
	}
	if !h.requireFormSession(c) {
		return
	}
	values, ok := readForm(c)
	if !ok {
		return
	}

	// The issue is read through its own service, so the caller's access is checked before its
	// identifier is known, and an issue the caller may not reach produces the not-found page.
	issue, err := h.issues.Get(r.Context(), issues.GetInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
	})
	if err != nil {
		c.notFound()
		return
	}

	action := strings.TrimSpace(c.request.PostFormValue(formFieldAction))
	switch action {
	case commentActionWrite:
		h.applyCommentCreate(c, view, issue, values)
	case commentActionEdit:
		h.applyCommentEdit(c, view, issue, values)
	case commentActionDelete:
		h.applyCommentDelete(c, view, issue, values)
	default:
		c.renderError(http.StatusBadRequest, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
	}
}

// applyCommentCreate posts a comment.
func (h *Handler) applyCommentCreate(c requestContext, view *resolved, issue issues.Issue, values formValues) {
	body := c.request.PostFormValue(commentFieldBody)
	if _, err := h.comments.Create(c.request.Context(), comments.CreateInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID, Body: body,
	}); err != nil {
		h.renderCommentError(c, view, issue, err, body, 0)
		return
	}
	h.redirectToIssue(c, view, issue.Number)
}

// applyCommentEdit changes a comment's body.
func (h *Handler) applyCommentEdit(c requestContext, view *resolved, issue issues.Issue, values formValues) {
	number := postInt(c, commentFieldID)
	body := c.request.PostFormValue(commentFieldBody)
	version := postInt(c, commentFieldVersion)
	if number < 1 || version < 1 {
		c.renderError(http.StatusUnprocessableEntity, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
		return
	}

	existing, err := h.comments.GetByNumber(c.request.Context(), comments.GetByNumberInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID, Number: number,
	})
	if err != nil {
		h.renderCommentError(c, view, issue, err, body, number)
		return
	}

	if _, err := h.comments.Edit(c.request.Context(), comments.EditInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID,
		CommentID: existing.ID, Body: body, ExpectedVersion: version,
	}); err != nil {
		h.renderCommentError(c, view, issue, err, body, number)
		return
	}
	h.redirectToIssue(c, view, issue.Number)
}

// applyCommentDelete removes a comment's text and history.
func (h *Handler) applyCommentDelete(c requestContext, view *resolved, issue issues.Issue, values formValues) {
	number := postInt(c, commentFieldID)
	if number < 1 {
		c.renderError(http.StatusUnprocessableEntity, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
		return
	}

	existing, err := h.comments.GetByNumber(c.request.Context(), comments.GetByNumberInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID, Number: number,
	})
	if err != nil {
		h.renderCommentError(c, view, issue, err, "", number)
		return
	}

	if err := h.comments.Delete(c.request.Context(), comments.DeleteInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID,
		CommentID: existing.ID, Request: formRequestContext(c.request),
	}); err != nil {
		h.renderCommentError(c, view, issue, err, "", number)
		return
	}
	h.redirectToIssue(c, view, issue.Number)
}

// renderCommentError re-renders the issue page with the reader's text and the refusal.
//
// The comment that failed is named, so its own form is the one that carries the message and the
// submitted text — which is what stops a refusal from looking like it happened to a different
// comment.
func (h *Handler) renderCommentError(c requestContext, view *resolved, issue issues.Issue, cause error, body string, number int) {
	data := h.issuePage(c, view, issue)
	data.formStatus = formStatus{Status: statusForFormError(cause)}
	data.Comments.Error, data.Comments.BodyError = h.commentMessageFor(c, cause)
	data.Comments.Body = body
	data.Comments.EditNumber = number
	for index := range data.CommentList {
		if data.CommentList[index].Number == number {
			data.CommentList[index].EditBody = body
		}
	}
	c.render("issue.html", data)
}

// commentMessageFor turns a comment error into messages for the forms.
//
// The messages come from the catalogue rather than from the domain error's text, on the same
// reasoning `messageFor` records: a page is a translated surface, and the domain's errors are English
// sentences written for a log or an API client.
func (h *Handler) commentMessageFor(c requestContext, err error) (form string, body string) {
	catalogue := c.catalogue()
	switch {
	case errors.Is(err, comments.ErrEmptyBody):
		return "", catalogue.T(i18n.IDCommentsEmpty)
	case errors.Is(err, comments.ErrBodyTooLong):
		return "", catalogue.T(i18n.IDCommentsTooLong)
	case errors.Is(err, comments.ErrConflict):
		return catalogue.T(i18n.IDCommentsConflict), ""
	case errors.Is(err, comments.ErrNoChange):
		return catalogue.T(i18n.IDCommentsNoChange), ""
	case errors.Is(err, comments.ErrDeleted):
		return catalogue.T(i18n.IDCommentsDeleted), ""
	case errors.Is(err, comments.ErrForbidden):
		return catalogue.T(i18n.IDCommentsForbidden), ""
	case errors.Is(err, comments.ErrAnonymousAuthor):
		return catalogue.T(i18n.IDCommentsSignIn), ""
	case errors.Is(err, comments.ErrNotFound):
		return catalogue.T(i18n.IDCommentsGone), ""
	default:
		return catalogue.T(i18n.IDAppErrorGeneric), ""
	}
}

// postInt reads an integer form field, returning 0 when it is absent or malformed.
func postInt(c requestContext, field string) int {
	raw := strings.TrimSpace(c.request.PostFormValue(field))
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return value
}

// formatPageTime renders a timestamp for a page.
func formatPageTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}
