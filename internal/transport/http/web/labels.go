package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/go-gitnest/GitNest/internal/i18n"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/labels"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The label surfaces.
//
// Two pages carry them. `/{owner}/{repo}/labels` is the repository's label list, which
// `urls-and-identifiers.md` §2.4 lists as "Label management and per-label issue list", so the
// management controls live there. The issue page shows and changes the labels on one issue, because
// that is where a reader wants them.
//
// # The colour is shown as text, and two rules decide it
//
// `docs/architecture/frontend.md` states both, and together they rule out the swatch a reader might
// expect:
//
//   - **"No inline style strings assembled from user data."** A swatch needs the colour in a `style`
//     attribute or a generated stylesheet — the first is forbidden outright, and the second would mean
//     a stylesheet with a rule per possible value. Even though the grammar makes the value inert (six
//     lowercase hex digits), the rule is about the shape of the code rather than about whether one
//     particular value happens to be safe.
//   - **"Colour is never the only carrier of meaning."** A bare swatch would make the colour the only
//     thing distinguishing one label from another to a reader who can see it, and invisible to one who
//     cannot.
//
// So the colour is rendered as the text `#ff0000` beside the label's name. That is legible to every
// reader, needs no style, and is exactly the value a client would pass back. **It is a deliberate
// divergence from GitHub's appearance**, taken because the project's own rules are explicit; the
// reasoning is recorded rather than left as an apparent omission.
//
// # The forms are plain forms
//
// As with issues and comments, `frontend.md`'s "a page must be usable before hydration" decides the
// shape: every label operation is a form, so it works with scripting disabled and is verifiable in this
// environment.

// The label form actions.
const (
	labelActionCreate = "label.create"
	labelActionDelete = "label.delete"
)

// The label form field names, which the templates and the handlers must agree on.
const (
	labelFieldName        = "label_name"
	labelFieldColor       = "label_color"
	labelFieldDescription = "label_description"
)

// labelView is one rendered label.
type labelView struct {
	Name string
	// ColorText is the rendered form, `#` included, shown as text rather than as a swatch.
	ColorText string
	// Description is shown as text, so it is escaped by the template rather than rendered as Markdown.
	// A label description is one line of metadata, not a document.
	Description string
	// IssueCount is how many issues carry the label.
	IssueCount int
	// Href is the per-label issue list.
	Href string
	// CanManage reports whether the reader may change the repository's labels, which needs admin.
	CanManage bool
}

// labelsView is the label list page's data.
type labelsView struct {
	*resolved
	formStatus
	Labels []labelView
	Action string
	Form   labelFormView
	// Outcome reports a completed change, which the redirect carries so a reload does not repeat it.
	Outcome string
}

// labelFormView is what the label page's forms need.
type labelFormView struct {
	CSRF       string
	CanManage  bool
	Name       string
	Color      string
	Error      string
	NameError  string
	ColorError string
}

// issueLabelView is the label section of an issue page.
type issueLabelView struct {
	Labels []labelView
	Action string
	// CanApply reports whether the reader may apply or remove a label, which needs write.
	CanApply bool
	CSRF     string
	// Error is a message for the section, and NameError one tied to the apply field.
	Error     string
	NameError string
	// Name is what the reader typed when an application was refused, so their input is not lost.
	Name string
}

// WithLabels gives the handler the label service. It returns the handler so a caller can chain, and a
// nil service leaves the label sections unrendered.
func (h *Handler) WithLabels(service LabelService) *Handler {
	h.labels = service
	return h
}

// LabelService is the part of the label service these pages use.
//
// It is the **shared service** rather than a store, so a page cannot bypass a rule the API would be held
// to: the two levels, the name folding, and the colour grammar are applied by the same code whichever
// surface calls it.
type LabelService interface {
	Create(ctx context.Context, input labels.CreateInput) (labels.Label, error)
	List(ctx context.Context, input labels.ListInput) ([]labels.Label, error)
	Delete(ctx context.Context, input labels.DeleteInput) error
	ForIssue(ctx context.Context, input labels.IssueLabelsInput) ([]labels.Label, error)
	Attach(ctx context.Context, input labels.AttachInput) (bool, error)
	Detach(ctx context.Context, input labels.DetachInput) error
	Counts(ctx context.Context, input labels.CountsInput) (map[string]int, error)
}

// labelList renders a repository's labels.
func (h *Handler) labelList(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}

	data := &labelsView{
		resolved: view,
		Action:   h.labelsHref(view),
		Form:     h.labelFormFor(c, view),
		Outcome:  r.URL.Query().Get(formOutcomeParam),
	}
	data.Labels = h.labelsForRepository(c, view)
	for index := range data.Labels {
		data.Labels[index].CanManage = data.Form.CanManage
	}
	c.render("labels.html", data)
}

// labelsForRepository reads a repository's labels and their issue counts.
//
// A failure leaves the page with an empty list rather than turning it into an error: the repository is
// readable, and losing its label list is not a reason to lose the page. That mirrors the conversation
// section's decision.
func (h *Handler) labelsForRepository(c requestContext, view *resolved) []labelView {
	if h.labels == nil {
		return nil
	}
	found, err := h.labels.List(c.request.Context(), labels.ListInput{
		Actor: c.viewer(), RepositoryID: view.row.ID,
	})
	if err != nil {
		return nil
	}
	// The counts come in one query for the whole list, because a label list shows the count beside every
	// label and a request per label would be one query each. A failure here leaves the counts at zero
	// rather than losing the labels, which are the more important half.
	counts, err := h.labels.Counts(c.request.Context(), labels.CountsInput{
		Actor: c.viewer(), RepositoryID: view.row.ID,
	})
	if err != nil {
		counts = map[string]int{}
	}

	rendered := make([]labelView, 0, len(found))
	for _, label := range found {
		rendered = append(rendered, newLabelView(label, counts[label.ID], h.labelsHrefFor(view, label.Name)))
	}
	return rendered
}

// newLabelView renders one label.
func newLabelView(label labels.Label, issueCount int, href string) labelView {
	return labelView{
		Name:        label.Name,
		ColorText:   label.DisplayColor(),
		Description: label.Description,
		IssueCount:  issueCount,
		Href:        href,
	}
}

// labelFormFor assembles the label page's form.
//
// `CanManage` comes from the shared decision: the level the read already resolved, compared against the
// admin level managing a label requires. A reader who may not manage labels sees the list without the
// forms rather than forms that would be refused.
func (h *Handler) labelFormFor(c requestContext, view *resolved) labelFormView {
	form := labelFormView{CanManage: view.level.Allows(repository.LevelAdmin)}
	if !form.CanManage {
		return form
	}
	issueForm, issued := h.formFor(c)
	if !issued {
		// A form without a token could never be submitted, so none is offered.
		form.CanManage = false
		return form
	}
	form.CSRF = issueForm.CSRF
	return form
}

// labelCreate handles a submitted label form.
func (h *Handler) labelCreate(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}
	if !h.requireFormSession(c) {
		return
	}
	// The form is read so its fields are parsed and its size bounded, even though the values this
	// handler needs are named fields rather than the issue form's.
	if _, ok := readForm(c); !ok {
		return
	}

	switch strings.TrimSpace(c.request.PostFormValue(formFieldAction)) {
	case labelActionCreate:
		h.applyLabelCreate(c, view)
	case labelActionDelete:
		h.applyLabelDelete(c, view)
	default:
		c.renderError(http.StatusBadRequest, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
	}
}

// applyLabelCreate creates a label.
func (h *Handler) applyLabelCreate(c requestContext, view *resolved) {
	name := c.request.PostFormValue(labelFieldName)
	color := c.request.PostFormValue(labelFieldColor)
	description := c.request.PostFormValue(labelFieldDescription)

	if _, err := h.labels.Create(c.request.Context(), labels.CreateInput{
		Actor: c.viewer(), RepositoryID: view.row.ID,
		Name: name, Color: color, Description: description,
		Request: formRequestContext(c.request),
	}); err != nil {
		h.renderLabelError(c, view, err, name, color)
		return
	}
	// A redirect after a successful post, so a reload does not create the label twice.
	http.Redirect(c.writer, c.request, h.labelsHref(view)+"?outcome=saved", http.StatusSeeOther)
}

// applyLabelDelete removes a label, which removes its attachments and no issues.
func (h *Handler) applyLabelDelete(c requestContext, view *resolved) {
	name := c.request.PostFormValue(labelFieldName)
	if err := h.labels.Delete(c.request.Context(), labels.DeleteInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, Name: name,
		Request: formRequestContext(c.request),
	}); err != nil {
		h.renderLabelError(c, view, err, "", "")
		return
	}
	http.Redirect(c.writer, c.request, h.labelsHref(view)+"?outcome=saved", http.StatusSeeOther)
}

// renderLabelError re-renders the label page with the refusal and the reader's input.
func (h *Handler) renderLabelError(c requestContext, view *resolved, cause error, name, color string) {
	data := &labelsView{
		resolved:   view,
		formStatus: formStatus{Status: statusForFormError(cause)},
		Action:     h.labelsHref(view),
		Form:       h.labelFormFor(c, view),
	}
	data.Labels = h.labelsForRepository(c, view)
	data.Form.Name, data.Form.Color = name, color
	data.Form.Error, data.Form.NameError, data.Form.ColorError = h.labelMessageFor(c, cause)
	c.render("labels.html", data)
}

// labelMessageFor turns a label error into messages for the form.
//
// The messages come from the catalogue rather than from the domain error's text, on the same reasoning
// `messageFor` records: a page is a translated surface, and the domain's errors are English sentences
// written for a log or an API client.
func (h *Handler) labelMessageFor(c requestContext, err error) (form, name, color string) {
	catalogue := c.catalogue()
	switch {
	case errors.Is(err, labels.ErrInvalidName):
		return "", catalogue.T(i18n.IDLabelsNameInvalid), ""
	case errors.Is(err, labels.ErrInvalidColor):
		return "", "", catalogue.T(i18n.IDLabelsColorInvalid)
	case errors.Is(err, labels.ErrInvalidDescription):
		return catalogue.T(i18n.IDLabelsDescriptionInvalid), "", ""
	case errors.Is(err, labels.ErrNameTaken):
		return "", catalogue.T(i18n.IDLabelsNameTaken), ""
	case errors.Is(err, labels.ErrForbidden):
		return catalogue.T(i18n.IDLabelsForbidden), "", ""
	case errors.Is(err, labels.ErrNotFound):
		return catalogue.T(i18n.IDLabelsGone), "", ""
	default:
		return catalogue.T(i18n.IDAppErrorGeneric), "", ""
	}
}

// issueLabelSection assembles the label part of an issue page.
//
// It never fails the page, on the same reasoning the conversation and history sections record.
func (h *Handler) issueLabelSection(c requestContext, view *resolved, issue issues.Issue, form issueFormView) issueLabelView {
	section := issueLabelView{
		Action:   h.issueLabelsHref(view, issue.Number),
		CSRF:     form.CSRF,
		CanApply: form.CSRF != "" && view.level.Allows(repository.LevelWrite),
	}
	if h.labels == nil {
		return section
	}

	found, err := h.labels.ForIssue(c.request.Context(), labels.IssueLabelsInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID,
	})
	if err != nil {
		return section
	}

	// The issue's labels are sorted for display, because the service returns them in an order the page
	// should not depend on.
	sorted := append([]labels.Label(nil), found...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return labels.Fold(sorted[i].Name) < labels.Fold(sorted[j].Name)
	})
	for _, label := range sorted {
		// `CanManage` is deliberately false here: managing a repository's labels belongs on the label
		// list, and offering the control on an issue would put an admin operation in a place a writer
		// reads.
		section.Labels = append(section.Labels,
			newLabelView(label, 0, h.labelsHrefFor(view, label.Name)))
	}
	return section
}

// labelApply handles a submitted label application or removal on an issue.
func (h *Handler) labelApply(w http.ResponseWriter, r *http.Request) {
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
	// The form is read so its fields are parsed and its size bounded, even though the values this
	// handler needs are named fields rather than the issue form's.
	if _, ok := readForm(c); !ok {
		return
	}

	issue, err := h.issues.Get(r.Context(), issues.GetInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
	})
	if err != nil {
		c.notFound()
		return
	}

	name := c.request.PostFormValue(labelFieldName)
	switch strings.TrimSpace(c.request.PostFormValue(formFieldAction)) {
	case "label.attach":
		if _, err := h.labels.Attach(c.request.Context(), labels.AttachInput{
			Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID, Name: name,
		}); err != nil {
			h.renderIssueLabelError(c, view, issue, err, name)
			return
		}
	case "label.detach":
		if err := h.labels.Detach(c.request.Context(), labels.DetachInput{
			Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID, Name: name,
		}); err != nil {
			h.renderIssueLabelError(c, view, issue, err, name)
			return
		}
	default:
		c.renderError(http.StatusBadRequest, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
		return
	}
	h.redirectToIssue(c, view, issue.Number)
}

// renderIssueLabelError re-renders the issue page with the label refusal.
func (h *Handler) renderIssueLabelError(c requestContext, view *resolved, issue issues.Issue, cause error, name string) {
	data := h.issuePage(c, view, issue)
	data.formStatus = formStatus{Status: statusForFormError(cause)}
	data.Labels.Error = h.labelSectionMessageFor(c, cause)
	data.Labels.Name = name
	c.render("issue.html", data)
}

// labelSectionMessageFor turns an attachment failure into a message.
func (h *Handler) labelSectionMessageFor(c requestContext, err error) string {
	catalogue := c.catalogue()
	switch {
	case errors.Is(err, labels.ErrNotFound):
		return catalogue.T(i18n.IDLabelsUnknown)
	case errors.Is(err, labels.ErrNotAttached):
		return catalogue.T(i18n.IDLabelsNotApplied)
	case errors.Is(err, labels.ErrForbidden):
		return catalogue.T(i18n.IDLabelsForbidden)
	default:
		return catalogue.T(i18n.IDAppErrorGeneric)
	}
}

// labelsHref is the repository's label list.
func (h *Handler) labelsHref(view *resolved) string {
	return h.basePath + "/" + view.FullName() + "/labels"
}

// labelsHrefFor is the per-label issue list, which `urls-and-identifiers.md` §2.4 lists as part of the
// label routes.
func (h *Handler) labelsHrefFor(view *resolved, name string) string {
	return h.labelsHref(view) + "/" + urlPathEscape(name)
}

// issueLabelsHref is where an issue's label forms post.
func (h *Handler) issueLabelsHref(view *resolved, number int64) string {
	return h.issueHref(view, number) + "/labels"
}

// urlPathEscape percent-escapes one path segment.
//
// A label name may contain any printable character, including `/` and a space, so a segment has to be
// escaped rather than concatenated. `url.PathEscape` percent-encodes a space as `%20`, which is what a
// path needs — a `+` would be a query-string convention and would not round-trip through the router.
func urlPathEscape(segment string) string {
	return url.PathEscape(segment)
}
