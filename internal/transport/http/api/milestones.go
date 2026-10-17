package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-gitnest/GitNest/internal/milestones"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The versioned milestone API.
//
// The paths are the ones `docs/architecture/urls-and-identifiers.md` section 3 lists for the Issues
// family, and the route shape §6.5 fixes: `/milestones` for a repository's milestones and
// `/milestones/{number}` for one, numbered per repository in its own sequence.
//
// # A milestone is addressed by number
//
// §6.5 gives milestones a numeric route and §6.1 forbids a stable internal UUID in a URL, so the number is
// the address — the same rule that makes an issue and a comment addressable. It is allocated from a counter
// on the repository, never reused, which is what distinguishes it from a comment's derived rank.
//
// # The progress travels with the list
//
// `GET /milestones` carries each milestone's counts, because a milestone list shows them beside every one
// and a request per milestone would be one query each. The counts are derived on every read rather than
// stored, so they cannot go stale — which is what W2.4's acceptance requires when it says they stay correct
// after an issue is closed, reopened, or relabeled.

// MilestoneService is the milestone half of the API's dependency.
//
// It is the shared service rather than a store, so this transport cannot bypass a rule the browser would be
// held to.
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
}

// milestoneResponse is one milestone.
type milestoneResponse struct {
	Number      int64  `json:"number"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	// DueOn is the calendar date as `YYYY-MM-DD`, or omitted when the milestone has no due date. It is a
	// date rather than an instant, which is why it is not RFC 3339.
	DueOn    string `json:"due_on,omitempty"`
	ClosedAt string `json:"closed_at,omitempty"`
	// Open, Closed, and Total are the derived counts, and percent_complete the rounded fraction.
	Open            int `json:"open_issues"`
	Closed          int `json:"closed_issues"`
	Total           int `json:"total_issues"`
	PercentComplete int `json:"percent_complete"`
	// Overdue reports whether the due date has passed on an open milestone, so a client does not have to
	// compare dates against its own clock or the server's timezone.
	Overdue   bool   `json:"overdue"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// newMilestoneResponse renders one milestone with its counts.
func newMilestoneResponse(milestone milestones.Milestone, counts milestones.Progress, overdue bool) milestoneResponse {
	response := milestoneResponse{
		Number:          milestone.Number,
		Title:           milestone.Title,
		Description:     milestone.Description,
		State:           string(milestone.State),
		DueOn:           milestones.FormatDueDate(milestone.DueOn),
		Open:            counts.Open(),
		Closed:          counts.Closed,
		Total:           counts.Total,
		PercentComplete: counts.PercentComplete(),
		Overdue:         overdue,
		CreatedAt:       formatIssueTime(milestone.CreatedAt),
		UpdatedAt:       formatIssueTime(milestone.UpdatedAt),
	}
	if milestone.ClosedAt != nil {
		response.ClosedAt = formatIssueTime(*milestone.ClosedAt)
	}
	return response
}

// milestoneBody is the accepted request body for a create.
type milestoneBody struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	// DueOn is a calendar date as `YYYY-MM-DD`.
	DueOn string `json:"due_on"`
}

// milestonePatch is the accepted request body for an update.
type milestonePatch struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	// DueOn is a pointer so "absent" is distinguishable from "cleared", which is what lets an empty string
	// remove a due date.
	DueOn *string `json:"due_on"`
	// State changes the lifecycle as part of the same request, which the settings surface's shape already
	// established.
	State *string `json:"state"`
}

// milestoneAttachBody is the accepted request body for putting an issue on a milestone.
//
// The milestone is named by **number**, which is how it is addressed, and a null value detaches — so one
// field expresses both operations, as the issue endpoint's shape does.
type milestoneAttachBody struct {
	Milestone *int64 `json:"milestone"`
}

// RegisterMilestones adds the milestone endpoints to mux.
//
// The issue service is a dependency because an issue's milestone is addressed under the issue's number.
func RegisterMilestones(mux *http.ServeMux, accounts AccountService, owners RepositoryOwnerLookup, repositories RepositoryService, milestoneService MilestoneService, issuesService IssueService) {
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/milestones", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}
		state, err := milestones.ParseStateFilter(r.URL.Query().Get("state"))
		if err != nil {
			writeValidationError(w, err)
			return
		}

		found, err := milestoneService.List(r.Context(), milestones.ListInput{
			Actor: principal, RepositoryID: row.ID, State: state,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		counts, err := milestoneService.Progress(r.Context(), milestones.ProgressInput{
			Actor: principal, RepositoryID: row.ID,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}

		items := make([]milestoneResponse, 0, len(found))
		for _, milestone := range found {
			items = append(items, newMilestoneResponse(milestone, counts[milestone.ID], milestoneService.Overdue(milestone)))
		}
		writeAPIJSON(w, http.StatusOK, false, items)
	})

	mux.HandleFunc("POST /api/v1/repos/{owner}/{repo}/milestones", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}
		var body milestoneBody
		if !decodeJSONBody(w, r, &body) {
			return
		}
		created, err := milestoneService.Create(r.Context(), milestones.CreateInput{
			Actor: principal, RepositoryID: row.ID,
			Title: body.Title, Description: body.Description, DueDate: body.DueOn,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		w.Header().Set("Location", milestoneLocation(r, created.Number))
		writeAPIJSON(w, http.StatusCreated, false,
			newMilestoneResponse(created, milestones.Progress{}, milestoneService.Overdue(created)))
	})

	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/milestones/{number}", func(w http.ResponseWriter, r *http.Request) {
		row, principal, number, ok := resolveMilestone(w, r, accounts, owners, repositories)
		if !ok {
			return
		}
		found, err := milestoneService.Get(r.Context(), milestones.GetInput{
			Actor: principal, RepositoryID: row.ID, Number: number,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		counts, err := milestoneService.Progress(r.Context(), milestones.ProgressInput{
			Actor: principal, RepositoryID: row.ID,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, false,
			newMilestoneResponse(found, counts[found.ID], milestoneService.Overdue(found)))
	})

	mux.HandleFunc("PATCH /api/v1/repos/{owner}/{repo}/milestones/{number}", func(w http.ResponseWriter, r *http.Request) {
		row, principal, number, ok := resolveMilestone(w, r, accounts, owners, repositories)
		if !ok {
			return
		}
		var patch milestonePatch
		if !decodeJSONBody(w, r, &patch) {
			return
		}
		if patch.Title == nil && patch.Description == nil && patch.DueOn == nil && patch.State == nil {
			writeError(w, http.StatusUnprocessableEntity,
				"a patch must name a title, a description, a due date, or a state")
			return
		}

		updated, ok := applyMilestonePatch(w, r, milestoneService, principal, row.ID, number, patch)
		if !ok {
			return
		}
		counts, err := milestoneService.Progress(r.Context(), milestones.ProgressInput{
			Actor: principal, RepositoryID: row.ID,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, false,
			newMilestoneResponse(updated, counts[updated.ID], milestoneService.Overdue(updated)))
	})

	mux.HandleFunc("DELETE /api/v1/repos/{owner}/{repo}/milestones/{number}", func(w http.ResponseWriter, r *http.Request) {
		row, principal, number, ok := resolveMilestone(w, r, accounts, owners, repositories)
		if !ok {
			return
		}
		if err := milestoneService.Delete(r.Context(), milestones.DeleteInput{
			Actor: principal, RepositoryID: row.ID, Number: number, Request: requestContextFor(r),
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		// A negative answer must not be remembered: the issues survive with no milestone, so a client that
		// re-reads them sees the detachment rather than a stale answer.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})

	// An issue's milestone. `POST` sets it and `DELETE` clears it, which is the shape the issue's own
	// endpoints use.
	mux.HandleFunc("POST /api/v1/repos/{owner}/{repo}/issues/{number}/milestone", func(w http.ResponseWriter, r *http.Request) {
		row, principal, issue, ok := resolveCommentIssue(w, r, accounts, owners, repositories, issuesService)
		if !ok {
			return
		}
		var body milestoneAttachBody
		if !decodeJSONBody(w, r, &body) {
			return
		}
		if body.Milestone == nil {
			// A null value means "remove the milestone", so the body's shape has one meaning per method
			// rather than two.
			writeError(w, http.StatusUnprocessableEntity,
				"a null milestone means removal; use DELETE for that")
			return
		}
		if err := milestoneService.Attach(r.Context(), milestones.AttachInput{
			Actor: principal, RepositoryID: row.ID, IssueID: issue.ID, Number: *body.Milestone,
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, false, map[string]any{
			"issue": strconv.FormatInt(issue.Number, 10), "milestone": *body.Milestone,
		})
	})

	mux.HandleFunc("DELETE /api/v1/repos/{owner}/{repo}/issues/{number}/milestone", func(w http.ResponseWriter, r *http.Request) {
		row, principal, issue, ok := resolveCommentIssue(w, r, accounts, owners, repositories, issuesService)
		if !ok {
			return
		}
		if err := milestoneService.Detach(r.Context(), milestones.DetachInput{
			Actor: principal, RepositoryID: row.ID, IssueID: issue.ID,
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
}

// applyMilestonePatch performs one update, applying the state change before the fields.
//
// The order matters for the same reason it does on an issue: a state change may be refused on its own terms
// — closing a closed milestone is an already-in-state refusal — and doing it first means a request that
// names both does not apply half of itself before finding out.
func applyMilestonePatch(
	w http.ResponseWriter, r *http.Request, service MilestoneService,
	principal repository.Principal, repositoryID string, number int64, patch milestonePatch,
) (milestones.Milestone, bool) {
	current, err := service.Get(r.Context(), milestones.GetInput{
		Actor: principal, RepositoryID: repositoryID, Number: number,
	})
	if err != nil {
		writeDomainError(w, err)
		return milestones.Milestone{}, false
	}

	// The transition, when one is named and needed. Naming the state a milestone is already in is a no-op
	// rather than an error, so a client that sends the whole object back is not refused for agreeing.
	if patch.State != nil {
		wanted := milestones.State(*patch.State)
		if wanted != milestones.StateOpen && wanted != milestones.StateClosed {
			writeError(w, http.StatusUnprocessableEntity, "state must be open or closed")
			return milestones.Milestone{}, false
		}
		if wanted != current.State {
			switch wanted {
			case milestones.StateClosed:
				current, err = service.Close(r.Context(), milestones.CloseInput{
					Actor: principal, RepositoryID: repositoryID, Number: number,
					Request: requestContextFor(r),
				})
			case milestones.StateOpen:
				current, err = service.Reopen(r.Context(), milestones.ReopenInput{
					Actor: principal, RepositoryID: repositoryID, Number: number,
				})
			}
			if err != nil {
				writeDomainError(w, err)
				return milestones.Milestone{}, false
			}
		}
	}

	if patch.Title == nil && patch.Description == nil && patch.DueOn == nil {
		return current, true
	}

	updated, err := service.Update(r.Context(), milestones.UpdateInput{
		Actor: principal, RepositoryID: repositoryID, Number: number,
		Title: patch.Title, Description: patch.Description, DueDate: patch.DueOn,
	})
	if err != nil {
		writeDomainError(w, err)
		return milestones.Milestone{}, false
	}
	return updated, true
}

// resolveMilestone turns a path into a repository, a caller, and a milestone number.
func resolveMilestone(
	w http.ResponseWriter, r *http.Request, accounts AccountService,
	owners RepositoryOwnerLookup, repositories RepositoryService,
) (repository.Repository, repository.Principal, int64, bool) {
	row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
	if !ok {
		return repository.Repository{}, repository.Principal{}, 0, false
	}
	number, err := strconv.ParseInt(r.PathValue("number"), 10, 64)
	if err != nil || number < 1 {
		// A number that is not a positive integer is the not-found answer rather than a bad request, for the
		// same reason the issue endpoints answer that way.
		writeAPINotFound(w)
		return repository.Repository{}, repository.Principal{}, 0, false
	}
	return row, principal, number, true
}

// milestoneLocation renders the canonical path of a milestone.
func milestoneLocation(r *http.Request, number int64) string {
	return "/api/v1/repos/" + r.PathValue("owner") + "/" + r.PathValue("repo") +
		"/milestones/" + strconv.FormatInt(number, 10)
}
