package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-gitnest/GitNest/internal/i18n"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/milestones"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The milestone surfaces.
//
// `/{owner}/{repo}/milestones` is the repository's milestone list, which `urls-and-identifiers.md` §2.4
// lists as "Milestone management and per-milestone list", so the management controls live there. The issue
// page shows and changes the milestone on one issue, because that is where a reader wants it.
//
// # A progress bar is not used, and two rules decide it
//
// The obvious rendering of a completion percentage is a bar, and `frontend.md` rules it out twice over:
// **"No inline style strings assembled from user data"** forbids the `width` that a bar needs, and
// **"Colour is never the only carrier of meaning"** would be violated by a bar whose length is the only
// statement of progress. So the counts and the percentage are rendered as **text** — "3 of 5 issues
// closed, 60%" — which is legible to every reader and needs no style at all. This is the same decision the
// label colour records, taken for the same two rules.
//
// # Overdue is a word, not a colour
//
// For the same reason, an overdue milestone carries the text "overdue" rather than only a red date. The
// comparison is the service's, against the injected clock, so a page and the API cannot disagree about
// whether a date has passed.
//
// # The forms are plain forms
//
// As with issues, comments, and labels, `frontend.md`'s "a page must be usable before hydration" decides
// the shape: every milestone operation is a form.

// The milestone form actions.
const (
	milestoneActionCreate = "milestone.create"
	milestoneActionUpdate = "milestone.update"
	milestoneActionClose  = "milestone.close"
	milestoneActionReopen = "milestone.reopen"
	milestoneActionDelete = "milestone.delete"
	milestoneActionAttach = "milestone.attach"
	milestoneActionDetach = "milestone.detach"
)

// The milestone form field names, which the templates and the handlers must agree on.
const (
	milestoneFieldNumber      = "milestone_number"
	milestoneFieldTitle       = "milestone_title"
	milestoneFieldDescription = "milestone_description"
	milestoneFieldDue         = "milestone_due_on"
	milestoneFieldState       = "milestone_state"
)

// milestoneView is one rendered milestone.
type milestoneView struct {
	Number      int64
	Title       string
	Description string
	State       string
	// DueText is the calendar date as `YYYY-MM-DD`, or empty.
	DueText string
	// Counts is the derived progress, rendered as text.
	Counts milestones.Progress
	// Overdue reports whether the open milestone's due date has passed.
	Overdue bool
	// CanManage reports whether the reader may change the repository's milestones, which needs write.
	CanManage bool
	// Href is the per-milestone issue list.
	Href string
	// EditTitle, EditDescription, and EditDueDate are what the reader submitted when an edit was refused, so
	// their text is not lost.
	EditTitle       string
	EditDescription string
	EditDueDate     string
}

// milestonesView is the milestone list page's data.
type milestonesView struct {
	*resolved
	formStatus
	Milestones []milestoneView
	Action     string
	Form       milestoneFormView
	Outcome    string
}

// milestoneFormView is what the milestone page's forms need.
type milestoneFormView struct {
	CSRF      string
	CanManage bool
	Title     string
	DueDate   string
	Error     string
	// TitleError is a message tied to the title field, and DueError one tied to the due-date field.
	TitleError string
	DueError   string
	// EditNumber is the milestone whose edit was refused, so its own form carries the message.
	EditNumber int64
}

// issueMilestoneView is the milestone section of an issue page.
type issueMilestoneView struct {
	// Milestone is the one the issue carries, or nil.
	Milestone *milestoneView
	// Choices are the open milestones the reader may move the issue to.
	Choices []milestoneView
	Action  string
	// CanChange reports whether the reader may set or clear the issue's milestone, which needs write.
	CanChange bool
	CSRF      string
	// Error is a message for the section, and NumberError one tied to the selection field.
	Error       string
	NumberError string
	// Selected is what the reader chose when a change was refused.
	Selected string
}

// WithMilestones gives the handler the milestone service. It returns the handler so a caller can chain, and
// a nil service leaves the milestone sections unrendered.
func (h *Handler) WithMilestones(service MilestoneService) *Handler {
	h.milestones = service
	return h
}

// MilestoneService is the part of the milestone service these pages use.
//
// It is the **shared service** rather than a store, so a page cannot bypass a rule the API would be held to.
type MilestoneService interface {
	Create(ctx context.Context, input milestones.CreateInput) (milestones.Milestone, error)
	Get(ctx context.Context, input milestones.GetInput) (milestones.Milestone, error)
	List(ctx context.Context, input milestones.ListInput) ([]milestones.Milestone, error)
	Update(ctx context.Context, input milestones.UpdateInput) (milestones.Milestone, error)
	Close(ctx context.Context, input milestones.CloseInput) (milestones.Milestone, error)
	Reopen(ctx context.Context, input milestones.ReopenInput) (milestones.Milestone, error)
	Delete(ctx context.Context, input milestones.DeleteInput) error
	Progress(ctx context.Context, input milestones.ProgressInput) (map[string]milestones.Progress, error)
	Attach(ctx context.Context, input milestones.AttachInput) error
	Detach(ctx context.Context, input milestones.DetachInput) error
	Overdue(milestones.Milestone) bool
	MilestoneOf(ctx context.Context, input milestones.MilestoneOfInput) (int64, error)
}

// milestoneList renders a repository's milestones.
func (h *Handler) milestoneList(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}
	// The filter is validated by the domain, so an unknown value is a not-found rather than a silently
	// unfiltered page.
	state, err := milestones.ParseStateFilter(r.URL.Query().Get("state"))
	if err != nil {
		c.notFound()
		return
	}

	data := &milestonesView{
		resolved: view,
		Action:   h.milestonesHref(view),
		Form:     h.milestoneFormFor(c, view),
		Outcome:  r.URL.Query().Get(formOutcomeParam),
	}
	data.Milestones = h.milestonesForRepository(c, view, state)
	for index := range data.Milestones {
		data.Milestones[index].CanManage = data.Form.CanManage
	}
	c.render("milestones.html", data)
}

// milestonesForRepository reads a repository's milestones with their derived progress.
//
// A failure leaves the page with an empty list rather than turning it into an error, on the same reasoning
// the conversation and label sections record.
func (h *Handler) milestonesForRepository(c requestContext, view *resolved, state milestones.State) []milestoneView {
	if h.milestones == nil {
		return nil
	}
	found, err := h.milestones.List(c.request.Context(), milestones.ListInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, State: state,
	})
	if err != nil {
		return nil
	}
	counts, err := h.milestones.Progress(c.request.Context(), milestones.ProgressInput{
		Actor: c.viewer(), RepositoryID: view.row.ID,
	})
	if err != nil {
		counts = map[string]milestones.Progress{}
	}

	rendered := make([]milestoneView, 0, len(found))
	for _, milestone := range found {
		rendered = append(rendered, milestoneView{
			Number:      milestone.Number,
			Title:       milestone.Title,
			Description: milestone.Description,
			State:       string(milestone.State),
			DueText:     milestones.FormatDueDate(milestone.DueOn),
			Counts:      counts[milestone.ID],
			Overdue:     h.milestones.Overdue(milestone),
			Href:        h.milestoneHref(view, milestone.Number),
		})
	}
	return rendered
}

// milestoneFormFor assembles the milestone page's form.
//
// `CanManage` comes from the shared decision, compared against **write** — which is the level the catalogue
// gives a milestone and not the admin a label needs.
func (h *Handler) milestoneFormFor(c requestContext, view *resolved) milestoneFormView {
	form := milestoneFormView{CanManage: view.level.Allows(repository.LevelWrite)}
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

// milestoneWrite handles a submitted milestone form.
func (h *Handler) milestoneWrite(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}
	if !h.requireFormSession(c) {
		return
	}
	if _, ok := readForm(c); !ok {
		return
	}

	switch strings.TrimSpace(c.request.PostFormValue(formFieldAction)) {
	case milestoneActionCreate:
		h.applyMilestoneCreate(c, view)
	case milestoneActionUpdate:
		h.applyMilestoneUpdate(c, view)
	case milestoneActionClose, milestoneActionReopen:
		h.applyMilestoneState(c, view)
	case milestoneActionDelete:
		h.applyMilestoneDelete(c, view)
	default:
		c.renderError(http.StatusBadRequest, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
	}
}

// applyMilestoneCreate creates a milestone.
func (h *Handler) applyMilestoneCreate(c requestContext, view *resolved) {
	title := c.request.PostFormValue(milestoneFieldTitle)
	description := c.request.PostFormValue(milestoneFieldDescription)
	due := c.request.PostFormValue(milestoneFieldDue)

	if _, err := h.milestones.Create(c.request.Context(), milestones.CreateInput{
		Actor: c.viewer(), RepositoryID: view.row.ID,
		Title: title, Description: description, DueDate: due,
	}); err != nil {
		h.renderMilestoneError(c, view, err, title, due, 0)
		return
	}
	http.Redirect(c.writer, c.request, h.milestonesHref(view)+"?outcome=saved", http.StatusSeeOther)
}

// applyMilestoneUpdate changes a milestone.
func (h *Handler) applyMilestoneUpdate(c requestContext, view *resolved) {
	number := postInt64(c, milestoneFieldNumber)
	title := c.request.PostFormValue(milestoneFieldTitle)
	description := c.request.PostFormValue(milestoneFieldDescription)
	due := c.request.PostFormValue(milestoneFieldDue)
	if number < 1 {
		c.renderError(http.StatusUnprocessableEntity, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
		return
	}

	if _, err := h.milestones.Update(c.request.Context(), milestones.UpdateInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
		Title: &title, Description: &description, DueDate: &due,
	}); err != nil {
		h.renderMilestoneError(c, view, err, title, due, number)
		return
	}
	http.Redirect(c.writer, c.request, h.milestonesHref(view)+"?outcome=saved", http.StatusSeeOther)
}

// applyMilestoneState closes or reopens a milestone.
func (h *Handler) applyMilestoneState(c requestContext, view *resolved) {
	number := postInt64(c, milestoneFieldNumber)
	if number < 1 {
		c.renderError(http.StatusUnprocessableEntity, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
		return
	}

	action := strings.TrimSpace(c.request.PostFormValue(formFieldAction))
	var err error
	if action == milestoneActionClose {
		_, err = h.milestones.Close(c.request.Context(), milestones.CloseInput{
			Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
			Request: formRequestContext(c.request),
		})
	} else {
		_, err = h.milestones.Reopen(c.request.Context(), milestones.ReopenInput{
			Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
		})
	}
	if err != nil {
		// The submitted values are not carried back, because a state change has no text of its own.
		h.renderMilestoneError(c, view, err, "", "", number)
		return
	}
	http.Redirect(c.writer, c.request, h.milestonesHref(view)+"?outcome=saved", http.StatusSeeOther)
}

// applyMilestoneDelete removes a milestone, which detaches its issues rather than deleting them.
func (h *Handler) applyMilestoneDelete(c requestContext, view *resolved) {
	number := postInt64(c, milestoneFieldNumber)
	if number < 1 {
		c.renderError(http.StatusUnprocessableEntity, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
		return
	}
	if err := h.milestones.Delete(c.request.Context(), milestones.DeleteInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, Number: number,
		Request: formRequestContext(c.request),
	}); err != nil {
		h.renderMilestoneError(c, view, err, "", "", number)
		return
	}
	http.Redirect(c.writer, c.request, h.milestonesHref(view)+"?outcome=saved", http.StatusSeeOther)
}

// renderMilestoneError re-renders the milestone page with the refusal and the reader's input.
func (h *Handler) renderMilestoneError(c requestContext, view *resolved, cause error, title, due string, number int64) {
	data := &milestonesView{
		resolved:   view,
		formStatus: formStatus{Status: statusForFormError(cause)},
		Action:     h.milestonesHref(view),
		Form:       h.milestoneFormFor(c, view),
	}
	data.Milestones = h.milestonesForRepository(c, view, "")
	for index := range data.Milestones {
		data.Milestones[index].CanManage = data.Form.CanManage
		if data.Milestones[index].Number == number {
			data.Milestones[index].EditTitle, data.Milestones[index].EditDescription, data.Milestones[index].EditDueDate = title, c.request.PostFormValue(milestoneFieldDescription), due
		}
	}
	data.Form.Title, data.Form.DueDate = title, due
	data.Form.EditNumber = number
	data.Form.Error, data.Form.TitleError, data.Form.DueError = h.milestoneMessageFor(c, cause)
	c.render("milestones.html", data)
}

// milestoneMessageFor turns a milestone error into messages for the form.
//
// The messages come from the catalogue rather than from the domain error's text, on the same reasoning the
// other surfaces record.
func (h *Handler) milestoneMessageFor(c requestContext, err error) (form, title, due string) {
	catalogue := c.catalogue()
	switch {
	case errors.Is(err, milestones.ErrInvalidTitle):
		return "", catalogue.T(i18n.IDMilestonesTitleInvalid), ""
	case errors.Is(err, milestones.ErrInvalidDueDate):
		return "", "", catalogue.T(i18n.IDMilestonesDueInvalid)
	case errors.Is(err, milestones.ErrInvalidDescription):
		return catalogue.T(i18n.IDMilestonesDescriptionInvalid), "", ""
	case errors.Is(err, milestones.ErrTitleTaken):
		return "", catalogue.T(i18n.IDMilestonesTitleTaken), ""
	case errors.Is(err, milestones.ErrAlreadyInState):
		return catalogue.T(i18n.IDMilestonesAlreadyInState), "", ""
	case errors.Is(err, milestones.ErrClosed):
		return catalogue.T(i18n.IDMilestonesClosed), "", ""
	case errors.Is(err, milestones.ErrForbidden):
		return catalogue.T(i18n.IDMilestonesForbidden), "", ""
	case errors.Is(err, milestones.ErrNotFound):
		return catalogue.T(i18n.IDMilestonesGone), "", ""
	default:
		return catalogue.T(i18n.IDAppErrorGeneric), "", ""
	}
}

// issueMilestoneSection assembles the milestone part of an issue page.
//
// It never fails the page, on the same reasoning the conversation, label, and history sections record.
func (h *Handler) issueMilestoneSection(c requestContext, view *resolved, issue issues.Issue, form issueFormView) issueMilestoneView {
	section := issueMilestoneView{
		Action:    h.issueMilestoneHref(view, issue.Number),
		CSRF:      form.CSRF,
		CanChange: form.CSRF != "" && view.level.Allows(repository.LevelWrite),
	}
	if h.milestones == nil {
		return section
	}

	// The milestones, their counts, and which one this issue carries. One read of the list gives the
	// choices; the issue's own milestone is found by matching the stored identifier, which is what keeps
	// this section to two queries rather than one per milestone.
	found, err := h.milestones.List(c.request.Context(), milestones.ListInput{
		Actor: c.viewer(), RepositoryID: view.row.ID,
	})
	if err != nil {
		return section
	}
	counts, err := h.milestones.Progress(c.request.Context(), milestones.ProgressInput{
		Actor: c.viewer(), RepositoryID: view.row.ID,
	})
	if err != nil {
		counts = map[string]milestones.Progress{}
	}

	current, err := h.milestones.MilestoneOf(c.request.Context(), milestones.MilestoneOfInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID,
	})
	if err != nil {
		return section
	}

	for _, milestone := range found {
		rendered := milestoneView{
			Number:      milestone.Number,
			Title:       milestone.Title,
			Description: milestone.Description,
			State:       string(milestone.State),
			DueText:     milestones.FormatDueDate(milestone.DueOn),
			Counts:      counts[milestone.ID],
			Overdue:     h.milestones.Overdue(milestone),
			Href:        h.milestoneHref(view, milestone.Number),
		}
		if milestone.Number == current {
			copied := rendered
			section.Milestone = &copied
			continue
		}
		// Only an **open** milestone is offered as a destination, because a closed one would refuse the
		// attachment — so offering it would be offering a choice that cannot succeed.
		if milestone.IsOpen() {
			section.Choices = append(section.Choices, rendered)
		}
	}
	return section
}

// milestoneWriteIssue handles a submitted milestone application or removal on an issue.
func (h *Handler) milestoneWriteIssue(w http.ResponseWriter, r *http.Request) {
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

	switch strings.TrimSpace(c.request.PostFormValue(formFieldAction)) {
	case milestoneActionAttach:
		h.applyIssueMilestoneAttach(c, view, issue)
	case milestoneActionDetach:
		if err := h.milestones.Detach(c.request.Context(), milestones.DetachInput{
			Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID,
		}); err != nil {
			h.renderIssueMilestoneError(c, view, issue, err, "")
			return
		}
	default:
		c.renderError(http.StatusBadRequest, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
		return
	}
	h.redirectToIssue(c, view, issue.Number)
}

// applyIssueMilestoneAttach puts an issue on a milestone.
func (h *Handler) applyIssueMilestoneAttach(c requestContext, view *resolved, issue issues.Issue) {
	raw := strings.TrimSpace(c.request.PostFormValue(milestoneFieldNumber))
	selected := postInt64(c, milestoneFieldNumber)
	if selected < 1 {
		c.renderError(http.StatusUnprocessableEntity, i18n.IDIssuesBadRequestTitle, i18n.IDIssuesBadRequestBody)
		return
	}
	if err := h.milestones.Attach(c.request.Context(), milestones.AttachInput{
		Actor: c.viewer(), RepositoryID: view.row.ID, IssueID: issue.ID, Number: selected,
	}); err != nil {
		h.renderIssueMilestoneError(c, view, issue, err, raw)
		return
	}
	h.redirectToIssue(c, view, issue.Number)
}

// renderIssueMilestoneError re-renders the issue page with the milestone refusal.
func (h *Handler) renderIssueMilestoneError(c requestContext, view *resolved, issue issues.Issue, cause error, selected string) {
	data := h.issuePage(c, view, issue)
	data.formStatus = formStatus{Status: statusForFormError(cause)}
	data.Milestones.Error = h.milestoneSectionMessageFor(c, cause)
	data.Milestones.Selected = selected
	c.render("issue.html", data)
}

// milestoneSectionMessageFor turns an attachment failure into a message.
func (h *Handler) milestoneSectionMessageFor(c requestContext, err error) string {
	catalogue := c.catalogue()
	switch {
	case errors.Is(err, milestones.ErrClosed):
		// The criterion a reader is most likely to hit, so it says what to do about it.
		return catalogue.T(i18n.IDMilestonesClosed)
	case errors.Is(err, milestones.ErrNotFound):
		return catalogue.T(i18n.IDMilestonesUnknown)
	case errors.Is(err, milestones.ErrNotAttached):
		return catalogue.T(i18n.IDMilestonesNotApplied)
	case errors.Is(err, milestones.ErrForbidden):
		return catalogue.T(i18n.IDMilestonesForbidden)
	default:
		return catalogue.T(i18n.IDAppErrorGeneric)
	}
}

// milestonesHref is the repository's milestone list.
func (h *Handler) milestonesHref(view *resolved) string {
	return h.basePath + "/" + view.FullName() + "/milestones"
}

// milestoneHref is the per-milestone issue list, which §6.5's route names.
func (h *Handler) milestoneHref(view *resolved, number int64) string {
	return h.basePath + "/" + view.FullName() + "/milestone/" + strconv.FormatInt(number, 10)
}

// issueMilestoneHref is where an issue's milestone form posts.
func (h *Handler) issueMilestoneHref(view *resolved, number int64) string {
	return h.issueHref(view, number) + "/milestone"
}

// postInt64 reads an integer form field, returning 0 when it is absent or malformed.
func postInt64(c requestContext, field string) int64 {
	raw := strings.TrimSpace(c.request.PostFormValue(field))
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}
