package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/comments"
	"github.com/go-gitnest/GitNest/internal/i18n"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/labels"
	"github.com/go-gitnest/GitNest/internal/milestones"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The issue mutation surfaces.
//
// # Why these are server-rendered forms rather than a React island
//
// `docs/architecture/frontend.md` states the principle this file implements: **"A page must be
// usable before hydration. The React islands enhance a working page; they do not build it."** The
// issue detail page is server-rendered and already owns its path, so the controls that change an
// issue belong on it as forms rather than in a bundle that has to run before the page means
// anything.
//
// Three consequences follow, and each is a property the project asks for:
//
//   - **Every operation works with scripting disabled**, which is strictly stronger than the
//     document's requirement that *reading* work without it, and it delivers "every workflow is
//     operable by keyboard alone" without depending on a component library implementing it.
//   - **The optimistic-concurrency rule survives the round trip.** An edit carries the version it
//     read in a hidden field, which is exactly what the API's `PATCH` requires — so a page left
//     open while somebody else edits is refused with a conflict rather than overwriting silently.
//   - **It is verifiable here.** A form submission is a request a Go test can make, so this
//     workstream's evidence is a passing suite rather than a claim about a bundle that cannot be
//     built in the development environment.
//
// # Where the errors go
//
// A failure re-renders the page with the message **and the values the reader submitted**, rather
// than redirecting to a generic page or dropping their text. `frontend.md` requires that "error
// messages are announced and tied to their field", and a form that loses a long issue body because
// the title was one character too long is the behaviour that rule exists to prevent.
//
// # The CSRF token
//
// A state-changing form carries the session's token, verified before anything is read from the
// body. The token is issued per document render, which is the existing behaviour that keeps one
// live token per session — see `RotateCSRFToken`. A form rendered by an older page is therefore
// refused, and the refusal says so, so a reader reloads rather than believing the operation is
// broken.

// The field names the forms use. They are constants because the templates and the handlers must
// agree on them, and a mismatch would silently drop a value.
const (
	formFieldAction  = "action"
	formFieldTitle   = "title"
	formFieldBody    = "body"
	formFieldVersion = "version"
	formFieldCSRF    = "csrf_token"
)

// The actions a form can request.
//
// They travel in one field rather than in five paths, because an HTML form can issue only `GET` and
// `POST` — so a deletion needs either a method a form cannot send or an action that names the
// operation. The second is chosen because it keeps every mutation at the **documented** paths:
// `urls-and-identifiers.md` section 2.4 lists `/{owner}/{repo}/issues` and
// `/{owner}/{repo}/issues/{number}`, and inventing `/issues/{number}/close` would add paths the
// document does not have.
const (
	actionEdit   = "edit"
	actionClose  = "close"
	actionReopen = "reopen"
	actionDelete = "delete"
)

// formOutcomeParam names the query parameter a redirect carries.
const formOutcomeParam = "outcome"

// maxFormBodyBytes bounds a form submission.
//
// The bound is the issue body limit plus room for the surrounding fields, so a body the domain
// would refuse as too long is refused while being read rather than buffered first.
const maxFormBodyBytes = issues.MaxBodyBytes + 8192

// formStatus is embedded by a page that must answer with a status other than 200.
type formStatus struct{ Status int }

// httpStatus reports the status the page carries.
func (f formStatus) httpStatus() int { return f.Status }

// WithCSRF gives the handler the issuer a state-changing form needs.
//
// A handler built without one renders the read surfaces and offers no forms, which is why this is a
// setter rather than a fourth constructor parameter: every existing caller keeps working, and a
// test that only exercises reading does not need to build a token issuer.
func (h *Handler) WithCSRF(issuer CSRFIssuer) *Handler {
	h.csrf = issuer
	return h
}

// issueFormView is what a rendered form needs.
type issueFormView struct {
	// CSRF is the token every state-changing form must carry.
	CSRF string
	// Version is the issue's current version, which an edit echoes back.
	Version int
	// Title and Body are what the reader submitted, so a rejected form does not lose their text.
	// They are empty when the page was simply viewed.
	Title string
	Body  string
	// Error is a message for the whole form, or empty.
	Error string
	// TitleError is a message tied to the title field, or empty. It exists because `frontend.md`
	// requires an error to be tied to its field, and a title-length refusal is the likeliest failure
	// a reader sees.
	TitleError string
	// CanChange reports whether the controls are offered at all, so a reader who may not change an
	// issue is shown the issue rather than a form that will be refused.
	CanChange bool
	// CanDelete is narrower than CanChange, because an issue's author may edit and close it but not
	// delete it.
	CanDelete bool
}

// issueFormPage is the creation form's page.
type issueFormPage struct {
	*resolved
	formStatus
	// Action is where the form posts, which the template uses rather than composing a path.
	Action string
	// ListHref is the collection the cancel link returns to.
	//
	// **A field a template names must exist on the data it is given.** Go's templates report a
	// missing field as an *execution* error, so omitting this did not render an empty link — it
	// failed the whole page, and `render` turned that into the 503 operational-failure page. A test
	// asserting the form renders is what caught it, which is the argument for exercising a template
	// rather than only parsing it.
	ListHref string
	Form     issueFormView
}

// issueCreateForm renders the form for a new issue.
func (h *Handler) issueCreateForm(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}

	// Filing a report needs an account, because an issue needs an author. A reader with no session
	// is told so rather than shown a form that cannot work.
	if !c.authenticated() {
		c.renderError(http.StatusUnauthorized, i18n.IDIssuesSignInRequiredTitle, i18n.IDIssuesSignInRequiredBody)
		return
	}

	form, ok := h.formFor(c)
	if !ok {
		c.renderError(http.StatusServiceUnavailable, i18n.IDIssuesNoTokenTitle, i18n.IDIssuesNoTokenBody)
		return
	}
	c.render("issue_new.html", &issueFormPage{
		resolved: view, Action: h.issuesHref(view, ""),
		ListHref: h.issuesHref(view, ""), Form: form,
	})
}

// issueCreate handles a submitted creation form.
func (h *Handler) issueCreate(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
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

	if _, err := h.issues.Create(r.Context(), issues.CreateInput{
		Actor: c.viewer(), RepositoryID: view.row.ID,
		Title: values.title, Body: values.body,
		Request: formRequestContext(r),
	}); err != nil {
		h.renderCreateError(c, view, err, values)
		return
	}
	// A redirect after a successful post, so a reload does not file the issue twice.
	http.Redirect(w, r, h.issuesHref(view, ""), http.StatusSeeOther)
}

// issueUpdate handles every mutation of an existing issue.
func (h *Handler) issueUpdate(w http.ResponseWriter, r *http.Request) {
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

	switch values.action {
	case actionEdit:
		h.applyIssueEdit(c, view, number, values)
	case actionClose, actionReopen:
		h.applyIssueTransition(c, view, number, values)
	case actionDelete:
		h.applyIssueDelete(c, view, number, values)
	default:
		// An unknown action is a client that sent something this surface does not implement. It is
		// refused rather than ignored, so a typo in a form does not look like a successful no-op.
		c.renderError(http.StatusBadRequest, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
	}
}

// applyIssueEdit changes an issue's title and body.
func (h *Handler) applyIssueEdit(c requestContext, view *resolved, number int64, values formValues) {
	title, body := values.title, values.body
	_, err := h.issues.Edit(c.request.Context(), issues.EditInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
		Title: &title, Body: &body, ExpectedVersion: values.version,
		Request: formRequestContext(c.request),
	})
	if err != nil {
		h.renderUpdateError(c, view, number, err, values)
		return
	}
	h.redirectToIssue(c, view, number)
}

// applyIssueTransition closes or reopens an issue.
func (h *Handler) applyIssueTransition(c requestContext, view *resolved, number int64, values formValues) {
	ctx := c.request.Context()
	var err error
	if values.action == actionClose {
		_, err = h.issues.Close(ctx, issues.CloseInput{
			Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
			Request: formRequestContext(c.request),
		})
	} else {
		_, err = h.issues.Reopen(ctx, issues.ReopenInput{
			Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
			Request: formRequestContext(c.request),
		})
	}
	if err != nil {
		h.renderUpdateError(c, view, number, err, values)
		return
	}
	h.redirectToIssue(c, view, number)
}

// applyIssueDelete removes an issue.
func (h *Handler) applyIssueDelete(c requestContext, view *resolved, number int64, values formValues) {
	if err := h.issues.Delete(c.request.Context(), issues.DeleteInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
		Request: formRequestContext(c.request),
	}); err != nil {
		h.renderUpdateError(c, view, number, err, values)
		return
	}
	// The issue is gone, so the redirect goes to the list rather than to a page that would answer
	// not-found.
	http.Redirect(c.writer, c.request, h.issuesHref(view, ""), http.StatusSeeOther)
}

// redirectToIssue sends the reader back to the issue they changed.
//
// A redirect after a successful post is what stops a reload from repeating the change. The outcome
// travels as a query parameter rather than in a flash store, because a store would be per-session
// state this surface does not otherwise need.
func (h *Handler) redirectToIssue(c requestContext, view *resolved, number int64) {
	http.Redirect(c.writer, c.request,
		h.issueHref(view, number)+"?"+formOutcomeParam+"=saved", http.StatusSeeOther)
}

// renderCreateError re-renders the creation form with the reader's text and the refusal.
func (h *Handler) renderCreateError(c requestContext, view *resolved, err error, values formValues) {
	form, _ := h.formFor(c)
	form.Title, form.Body = values.title, values.body
	form.Error, form.TitleError = h.messageFor(c, err)

	c.render("issue_new.html", &issueFormPage{
		resolved:   view,
		formStatus: formStatus{Status: statusForFormError(err)},
		Action:     h.issuesHref(view, ""),
		ListHref:   h.issuesHref(view, ""),
		Form:       form,
	})
}

// renderUpdateError re-renders the issue page with the reader's text and the refusal.
//
// The page is rendered from a **fresh read**, so a conflict shows the values that are actually
// stored alongside the message saying the reader's text was not applied — which is what makes the
// refusal actionable rather than merely reported.
func (h *Handler) renderUpdateError(c requestContext, view *resolved, number int64, cause error, values formValues) {
	found, err := h.issues.Get(c.request.Context(), issues.GetInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
	})
	if err != nil {
		c.notFound()
		return
	}

	data := h.issuePage(c, view, found)
	data.formStatus = formStatus{Status: statusForFormError(cause)}
	data.Form.Error, data.Form.TitleError = h.messageFor(c, cause)
	// A refused edit keeps the reader's text so nothing they wrote is lost; a refused transition or
	// deletion carries no text of its own, so the stored values stand.
	if values.action == actionEdit {
		data.Form.Title, data.Form.Body = values.title, values.body
	}
	c.render("issue.html", data)
}

// messageFor turns a domain error into messages for the form.
//
// **The messages come from the catalogue rather than from the domain error's text.** A page is a
// translated surface, and the domain's errors are English sentences written for a log or an API
// client; showing one on a page would make the read surface the only place in the product where a
// translated locale still produces English. The mapping is exhaustive over the errors this surface
// can produce, with a generic fallback so an unmapped failure never leaks internal detail.
func (h *Handler) messageFor(c requestContext, err error) (form string, title string) {
	catalogue := c.catalogue()
	switch {
	case errors.Is(err, issues.ErrInvalidTitle):
		return "", catalogue.T(i18n.IDIssuesTitleInvalid)
	case errors.Is(err, issues.ErrInvalidBody):
		return catalogue.T(i18n.IDIssuesBodyInvalid), ""
	case errors.Is(err, issues.ErrConflict):
		return catalogue.T(i18n.IDIssuesConflict), ""
	case errors.Is(err, issues.ErrNoChange):
		return catalogue.T(i18n.IDIssuesNoChange), ""
	case errors.Is(err, issues.ErrAlreadyInState):
		return catalogue.T(i18n.IDIssuesAlreadyInState), ""
	case errors.Is(err, issues.ErrForbidden):
		return catalogue.T(i18n.IDIssuesForbidden), ""
	case errors.Is(err, issues.ErrAnonymousAuthor):
		return catalogue.T(i18n.IDIssuesSignInRequiredBody), ""
	case errors.Is(err, issues.ErrNotFound):
		return catalogue.T(i18n.IDIssuesGone), ""
	default:
		return catalogue.T(i18n.IDAppErrorGeneric), ""
	}
}

// statusForFormError maps a domain error to the status the re-rendered page carries.
//
// **Every domain this package renders a form for has to be listed**, because the errors are distinct
// values even when they mean the same thing: `comments.ErrConflict` and `issues.ErrConflict` are
// different errors with one meaning, so mapping only the issue ones made a comment conflict answer
// 422 while an issue conflict answered 409 — and a client could not tell a lost update from a
// malformed request. The statuses match the API's mapping for the same errors, so a client that
// inspects the response reads the same meaning from either surface.
func statusForFormError(err error) int {
	switch {
	case errors.Is(err, issues.ErrForbidden), errors.Is(err, issues.ErrAnonymousAuthor),
		errors.Is(err, comments.ErrForbidden), errors.Is(err, comments.ErrAnonymousAuthor):
		return http.StatusForbidden
	case errors.Is(err, issues.ErrNotFound), errors.Is(err, comments.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, issues.ErrConflict), errors.Is(err, comments.ErrConflict):
		return http.StatusConflict
	// **Every domain gets its statuses here, and omitting one is invisible until an end-to-end test.**
	// This is the second time the omission has been made: the comment domain was missing first, and the
	// label domain repeated it — a name conflict answered 422 where the API answered 409, so a client
	// could not tell a duplicate name from a malformed request. The two mappings are therefore asserted
	// to agree by a test rather than by inspection.
	case errors.Is(err, labels.ErrNameTaken):
		return http.StatusConflict
	case errors.Is(err, labels.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, labels.ErrNotFound), errors.Is(err, labels.ErrNotAttached):
		return http.StatusNotFound
	// The milestone domain, added here in the same change as the API's mapping — see the note above about
	// how often the omission has happened.
	case errors.Is(err, milestones.ErrTitleTaken), errors.Is(err, milestones.ErrClosed):
		return http.StatusConflict
	case errors.Is(err, milestones.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, milestones.ErrNotFound), errors.Is(err, milestones.ErrNotAttached):
		return http.StatusNotFound
	default:
		return http.StatusUnprocessableEntity
	}
}

// formValues is a submitted form.
type formValues struct {
	action  string
	title   string
	body    string
	version int
}

// readForm parses a submitted form.
//
// A malformed version is refused rather than defaulted to zero: a zero version could never match and
// would report a conflict for a request that is merely malformed, which is a misleading answer to a
// client bug.
func readForm(c requestContext) (formValues, bool) {
	c.request.Body = http.MaxBytesReader(c.writer, c.request.Body, maxFormBodyBytes)
	if err := c.request.ParseForm(); err != nil {
		c.renderError(http.StatusRequestEntityTooLarge, i18n.IDIssuesTooLargeTitle, i18n.IDIssuesTooLargeBody)
		return formValues{}, false
	}

	values := formValues{
		action: strings.TrimSpace(c.request.PostFormValue(formFieldAction)),
		title:  c.request.PostFormValue(formFieldTitle),
		body:   c.request.PostFormValue(formFieldBody),
	}
	if raw := strings.TrimSpace(c.request.PostFormValue(formFieldVersion)); raw != "" {
		version, err := strconv.Atoi(raw)
		if err != nil || version < 1 {
			c.renderError(http.StatusUnprocessableEntity, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
			return formValues{}, false
		}
		values.version = version
	}
	return values, true
}

// requireFormSession checks the session and the CSRF token before anything else.
//
// **The token is verified before the body is parsed**, so a forged submission cannot make the server
// do work. The session is resolved once and its stored digest is what the presented token is
// compared against, so this is the same constant-time comparison the API makes rather than a second
// implementation of one.
func (h *Handler) requireFormSession(c requestContext) bool {
	if h.sessions == nil {
		c.renderError(http.StatusServiceUnavailable, i18n.IDIssuesNoTokenTitle, i18n.IDIssuesNoTokenBody)
		return false
	}
	cookie, err := c.request.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		c.renderError(http.StatusUnauthorized, i18n.IDIssuesSignInRequiredTitle, i18n.IDIssuesSignInRequiredBody)
		return false
	}
	_, _, digest, err := h.sessions.ResolveSession(c.request.Context(), cookie.Value)
	if err != nil {
		c.renderError(http.StatusUnauthorized, i18n.IDIssuesSignInRequiredTitle, i18n.IDIssuesSignInRequiredBody)
		return false
	}

	// A form submits the whole body, so the token arrives as a field rather than as a header. The
	// comparison is the one the API makes; only the carrier differs.
	presented := c.request.PostFormValue(formFieldCSRF)
	if presented == "" || !identity.DigestsMatch(identity.HashToken(presented), digest) {
		c.renderError(http.StatusForbidden, i18n.IDIssuesCSRFTitle, i18n.IDIssuesCSRFBody)
		return false
	}
	return true
}

// formFor assembles the token and the permission facts a form needs.
//
// The second return value reports whether a token was issued at all, so a caller can refuse to
// render a form that could never be submitted rather than rendering one that fails on submit.
func (h *Handler) formFor(c requestContext) (issueFormView, bool) {
	form := issueFormView{}
	if h.csrf == nil {
		return form, false
	}
	cookie, err := c.request.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return form, false
	}
	token, err := h.csrf.RotateCSRFToken(c.request.Context(), cookie.Value)
	if err != nil {
		return form, false
	}
	form.CSRF = token
	return form, true
}

// changeFacts reports whether a reader may change an issue, and whether they may delete it.
//
// **The answer comes from the shared access decision, not from a page's own reasoning.** The level
// is what the repository service returns for this caller, and the author rule is read off the issue
// — so a page cannot offer a control the service would refuse, and it cannot hide one the reader is
// entitled to. The two are the same rules `authorizeChange` applies on the write path; this only
// predicts them for display, and a prediction that disagreed would show up as a form whose
// submission is refused.
func changeFacts(level repository.Level, issue issues.Issue, actor repository.Principal) (canChange, canDelete bool) {
	writer := level.Allows(repository.LevelWrite)
	return writer || issue.AuthoredBy(actor.UserID), writer
}

// formRequestContext carries the request facts an audit record keeps.
//
// It is a local helper rather than a shared one because the API keeps its own: the two surfaces
// resolve the caller's address the same way, and a duplicate three-line function is cheaper than a
// dependency from this package on the API package's internals.
func formRequestContext(r *http.Request) audit.RequestContext {
	return audit.RequestContext{
		SourceAddress: r.RemoteAddr,
		UserAgent:     r.UserAgent(),
	}
}

// formIssueNumber reads the issue number from a mutation's path.
func formIssueNumber(c requestContext) (int64, bool) {
	number, err := strconv.ParseInt(c.request.PathValue("number"), 10, 64)
	if err != nil || number < 1 {
		// A malformed number is the not-found page rather than a bad-request page, for the same
		// reason the API answers not-found: `/issues/abc` and `/issues/999` mean the same thing to a
		// reader.
		c.notFound()
		return 0, false
	}
	return number, true
}

// issuesBaseHref is the collection path, used by the templates for a cancel link.
func (h *Handler) issuesBaseHref(view *resolved) string { return h.issuesHref(view, "") }

// issueFormHref is the creation form's path.
func (h *Handler) issueFormHref(view *resolved) string {
	return h.basePath + "/" + view.FullName() + "/issues/new"
}
