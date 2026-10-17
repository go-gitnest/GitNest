package web

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The server-rendered issue read surfaces.
//
// # Why a read surface and not the interactive one
//
// `docs/architecture/frontend.md` splits the surfaces: the read surfaces are rendered on the
// server, and the interactive ones are a React bundle. A list of issues and one issue's detail
// are reads, so they belong here — and rendering them server-side is also what makes them
// reachable without script, which the accessibility requirements ask for.
//
// **Creating, editing, closing, and deleting from a browser go through the interactive bundle**,
// which calls the same versioned API W2.2b added. That is why the pages below show a hint rather
// than a form: this package has no CSRF-issuing mutation path, and adding one would be a second
// way to change an issue alongside the API the React surface already uses.
//
// # The history is the audit ledger
//
// The detail page shows an issue's recorded changes by reading the ledger for the issue's
// subject. There is no second history table, and the record outlives the issue — so a page for a
// deleted issue can still show that it existed and who removed it.
//
// # Access
//
// Both handlers resolve the repository at `repository.LevelRead`, so a caller who may not read
// the repository gets the not-found page a nonexistent repository gets. **The issue itself is
// then read through the issue service**, which applies the same rule again against the same
// shared decision — so the page and the API cannot answer differently.

// issuesView is the issue list page's data.
type issuesView struct {
	*resolved
	// State is the filter in force: empty for all, otherwise `open` or `closed`.
	State    string
	Issues   []issueRowView
	MoreHref string
	// ListHref is this page without a filter, so a template can build the filter links.
	ListHref string
	// NewHref is the creation form's path, offered only to a reader who may file an issue.
	NewHref string
}

// issueRowView is one row of the list.
type issueRowView struct {
	Href       string
	Number     int64
	Title      string
	State      string
	AuthorName string
}

// issueDetailView is one issue's page.
type issueDetailView struct {
	*resolved
	// The form is rendered on this page, because the controls that change an issue belong where the
	// issue is. The status travels with it so a refused submission answers with an error status
	// rather than a 200 that would read as success.
	formStatus
	Issue struct {
		Number       int64
		Title        string
		State        string
		AuthorName   string
		ClosedByName string
		// Version is the version this page was rendered from, shown nowhere but carried by the edit
		// form — which is what makes a concurrent edit a conflict rather than an overwrite.
		Version int
		// RenderedBody is W2.1's sanitized HTML, placed as element content only.
		RenderedBody template.HTML
	}
	// Action is where the mutation form posts, and ListHref is the collection it belongs to.
	Action   string
	ListHref string
	Form     issueFormView
	// CommentList is the conversation, and Comments is what its forms need. Both are empty when the
	// handler has no comment service, so the section is simply absent.
	CommentList []commentView
	Comments    commentFormView
	// Labels is the issue's labels and the forms that change them, on the same terms.
	Labels issueLabelView
	// Milestones is the issue's milestone and the form that changes it.
	Milestones issueMilestoneView
	// Saved reports that the reader arrived from a successful change, so a redirect can say so
	// without a flash store.
	Saved   bool
	History []issueHistoryView
}

// issueHistoryView is one recorded change.
type issueHistoryView struct {
	Action     string
	ActorName  string
	At         string
	Transition string
}

// issuePageSize bounds one page of the list.
const issuePageSize = 30

// WithIssues gives the handler the issue service. It returns the handler so a caller can chain,
// and a nil service leaves the issue routes unregistered rather than serving a broken page.
//
// It is a setter rather than a constructor parameter because the issue surfaces are a later
// addition than the repository ones, and a setter keeps every existing caller of `NewHandler`
// working unchanged.
func (h *Handler) WithIssues(service IssueService) *Handler {
	h.issues = service
	return h
}

// WithAccounts gives the handler the account lookup the issue history names actors with.
func (h *Handler) WithAccounts(accounts AccountLookup) *Handler {
	h.accounts = accounts
	return h
}

// WithAudit gives the handler the ledger reader the issue history is read from.
func (h *Handler) WithAudit(reader *audit.Reader) *Handler {
	h.audit = reader
	return h
}

// IssueService is the part of the issue service these pages use.
//
// It is the **shared service** rather than a store, so a page cannot bypass a rule the API would be
// held to: the access decision, the author allowance, and the version guard are all applied by the
// same code whichever surface calls it.
type IssueService interface {
	Create(ctx context.Context, input issues.CreateInput) (issues.Issue, error)
	Get(ctx context.Context, input issues.GetInput) (issues.Issue, error)
	List(ctx context.Context, input issues.ListInput) (issues.Page, error)
	Edit(ctx context.Context, input issues.EditInput) (issues.Issue, error)
	Close(ctx context.Context, input issues.CloseInput) (issues.Issue, error)
	Reopen(ctx context.Context, input issues.ReopenInput) (issues.Issue, error)
	Delete(ctx context.Context, input issues.DeleteInput) error
}

// issueList renders a repository's issues.
func (h *Handler) issueList(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}

	// The filter is validated by the domain, so an unknown value is a not-found rather than a
	// silently unfiltered page — which would show a reader issues they did not ask for and might
	// mistake for the filtered set.
	state, err := issues.ParseStateFilter(r.URL.Query().Get("state"))
	if err != nil {
		c.notFound()
		return
	}

	data := &issuesView{
		resolved: view,
		State:    string(state),
		ListHref: h.issuesHref(view, ""),
	}
	// The creation link is offered only to a signed-in reader, because an issue needs an author.
	// Showing the link to somebody who cannot use it would send them to a page that refuses them.
	if c.authenticated() {
		data.NewHref = h.issueFormHref(view)
	}
	before, ok := parseCursor(r.URL.Query().Get("before"))
	if !ok {
		c.notFound()
		return
	}

	found, err := h.issues.List(r.Context(), issues.ListInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, State: state,
		Limit: issuePageSize, Before: before,
	})
	if err != nil {
		// A refusal and an absent repository are the same answer, which is what the repository
		// resolution above already applied.
		c.notFound()
		return
	}

	for _, issue := range found.Issues {
		data.Issues = append(data.Issues, issueRowView{
			Href:   h.issueHref(view, issue.Number),
			Number: issue.Number, Title: issue.Title, State: string(issue.State),
			AuthorName: issue.AuthorName,
		})
	}
	// The "more" link carries the cursor rather than a page number, because the store pages by
	// keyset: a page number would force the store to walk from the beginning, and a cursor both
	// costs nothing and cannot repeat a row when an issue is opened while a reader is paging.
	if found.HasMore && found.Next != nil {
		data.MoreHref = h.issuesHrefCursor(view, data.State, *found.Next)
	}
	c.render("issues.html", data)
}

// issueDetail renders one issue and its recorded history.
func (h *Handler) issueDetail(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}

	number, err := strconv.ParseInt(r.PathValue("number"), 10, 64)
	if err != nil || number < 1 {
		c.notFound()
		return
	}

	found, err := h.issues.Get(r.Context(), issues.GetInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
	})
	if err != nil {
		c.notFound()
		return
	}

	data := h.issuePage(c, view, found)
	data.Saved = r.URL.Query().Get(formOutcomeParam) == "saved"
	c.render("issue.html", data)
}

// issuePage assembles one issue's page.
//
// It is shared by the success path and the refusal path, so a re-rendered form cannot differ from a
// freshly loaded page in any way other than the message and the values it carries. Two assemblies
// would be two things to keep in step, and the one that drifted would do so silently.
func (h *Handler) issuePage(c requestContext, view *resolved, found issues.Issue) *issueDetailView {
	data := &issueDetailView{resolved: view}
	data.Issue.Number = found.Number
	data.Issue.Title = found.Title
	data.Issue.State = string(found.State)
	data.Issue.AuthorName = found.AuthorName
	data.Issue.ClosedByName = found.ClosedByName
	data.Issue.Version = found.Version
	// The body goes through W2.1's renderer, which escapes raw HTML and sanitizes the rest. The
	// result is marked safe **only** because that sanitizer ran, and it is placed as element content
	// by the template.
	data.Issue.RenderedBody = h.markdown.Render(found.Body)

	data.Action = h.issueHref(view, found.Number)
	data.ListHref = h.issuesHref(view, "")
	// The form is offered only when a token was issued and the caller may change the issue. Both
	// answers come from outside this function: the token from the issuer, the permission from the
	// repository level the read already resolved and the issue's own author.
	if form, issued := h.formFor(c); issued {
		form.Version = found.Version
		form.CanChange, form.CanDelete = changeFacts(view.level, found, c.viewer())
		if form.CanChange {
			data.Form = form
		}
	}
	data.History = h.issueHistory(c.request.Context(), c, found)
	// The conversation and the labels are assembled last, because their forms need the token and the
	// permission the issue's own form computed — one decision, shared by every section.
	data.CommentList, data.Comments = h.commentSection(c, view, found, data.Form)
	data.Labels = h.issueLabelSection(c, view, found, data.Form)
	data.Milestones = h.issueMilestoneSection(c, view, found, data.Form)
	return data
}

// issueHistory reads an issue's recorded changes from the ledger.
//
// A failure to read the ledger is not a failure of the page: the issue itself is readable, and
// losing its history would be a worse outcome than showing it without one. The error is therefore
// swallowed deliberately, and the template renders its "no history" line — which is the same thing
// a newly opened issue shows, so the page cannot be distinguished from a working one by a reader.
func (h *Handler) issueHistory(ctx context.Context, c requestContext, issue issues.Issue) []issueHistoryView {
	if h.audit == nil {
		return nil
	}
	entries, err := h.audit.ForSubject(ctx, audit.SubjectIssue, issue.ID)
	if err != nil {
		return nil
	}

	// Names are resolved from the accounts the ledger names, so the history reads as people
	// rather than as identifiers.
	names := map[string]string{}
	history := make([]issueHistoryView, 0, len(entries))
	for _, entry := range entries {
		row := issueHistoryView{
			Action: entry.Action,
			At:     entry.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if entry.ActorID != "" {
			if name, found := names[entry.ActorID]; found {
				row.ActorName = name
			} else if account, err := h.accounts.AccountByID(ctx, entry.ActorID); err == nil {
				row.ActorName = account.Username
				names[entry.ActorID] = account.Username
			}
		}
		// The state transition, when the record carries one, so a reader sees `open → closed`
		// rather than having to know what `issue.closed` means.
		row.Transition = transitionOf(entry.Detail)
		history = append(history, row)
	}
	return history
}

// transitionOf renders a recorded state change, or the empty string when there is none.
//
// **The detail is decoded from JSON, so it arrives as a `map[string]any` and not as the
// `audit.Change` the writer put in.** A type assertion against `audit.Change` therefore never
// matched, and the transition was silently absent from every history — the defect the end-to-end
// test caught, because the domain test asserted the record's shape while only this test observed
// what a reader is actually shown.
//
// Both shapes are accepted, so the reader does not depend on which side of the serialization it
// sits on.
func transitionOf(detail map[string]any) string {
	raw, found := detail["state"]
	if !found {
		return ""
	}
	if fields, ok := raw.(map[string]any); ok {
		return renderTransition(fields["from"], fields["to"])
	}
	if change, ok := raw.(audit.Change); ok {
		return renderTransition(change.From, change.To)
	}
	return ""
}

// renderTransition formats a pair of states.
func renderTransition(from, to any) string {
	fromText, fromOK := from.(string)
	toText, toOK := to.(string)
	if !fromOK || !toOK || fromText == "" || toText == "" {
		return ""
	}
	return fromText + " → " + toText
}

// issuesHref is the list path, optionally filtered.
func (h *Handler) issuesHref(view *resolved, state string) string {
	base := h.basePath + "/" + view.FullName() + "/issues"
	if state != "" {
		base += "?state=" + state
	}
	return base
}

// issuesHrefCursor is the list path continued from a cursor.
//
// The cursor is rendered as `<RFC3339Nano>,<id>` in one parameter, which is opaque to a reader
// and cheap to parse. It is deliberately **not** a page number: the store pages by keyset, so a
// page number would have to be translated by walking from the start — and a row inserted while a
// reader pages would shift every later page, repeating or skipping one.
func (h *Handler) issuesHrefCursor(view *resolved, state string, cursor issues.Cursor) string {
	base := h.issuesHref(view, state)
	separator := "?"
	if state != "" {
		separator = "&"
	}
	return base + separator + "before=" + url.QueryEscape(cursorValue(cursor))
}

// cursorValue renders a cursor as one opaque parameter.
func cursorValue(cursor issues.Cursor) string {
	return cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + "," + cursor.ID
}

// parseCursor reads a `before` parameter.
//
// An absent parameter is the first page. A malformed one is refused rather than ignored, because
// silently serving the first page to a reader who followed a "more" link would look like the
// list had ended and started again.
func parseCursor(value string) (*issues.Cursor, bool) {
	if value == "" {
		return nil, true
	}
	at, id, found := strings.Cut(value, ",")
	if !found || id == "" {
		return nil, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, false
	}
	return &issues.Cursor{CreatedAt: parsed, ID: id}, true
}

// issueHref is one issue's path.
func (h *Handler) issueHref(view *resolved, number int64) string {
	return h.basePath + "/" + view.FullName() + "/issues/" + strconv.FormatInt(number, 10)
}

// issueCommentsHref is one issue's conversation, which is where the comment forms post.
func (h *Handler) issueCommentsHref(view *resolved, number int64) string {
	return h.issueHref(view, number) + "/comments"
}
