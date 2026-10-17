package api

import (
	"context"
	"net/http"

	"github.com/go-gitnest/GitNest/internal/labels"
)

// The versioned label API.
//
// The paths are the ones `docs/architecture/urls-and-identifiers.md` section 3 lists for the Issues
// family: `/labels` and `/labels/{name}` for a repository's labels, and
// `/issues/{issue_number}/labels` beneath an issue.
//
// # A label is addressed by name, not by identifier
//
// `urls-and-identifiers.md` L29 permits "names, slugs, numbers, refs, and content digests" in a URL and
// requires that a stable internal UUID never reaches a user-visible payload. A label's name is its
// address, which is what §6.5 means when it says labels "carry no route segment of their own beyond the
// numeric milestone route" — the name *is* the segment. The lookup folds the name, so a URL's case is
// irrelevant, which is exactly what §6.1's "compared after Unicode-free ASCII folding" is for.
//
// # The colour
//
// A response carries the six bare digits, which is what §6.5 fixes as the stored form, **and** the
// rendered form with `#` in a second field. The reason is that a client should not have to know the
// grammar to display a colour, and a client that concatenates `"#" + color` itself would be a second
// place the rule has to be right. Submitting a colour accepts either form, because that is how a person
// writes one.

// LabelService is the label half of the API's dependency.
//
// It is the shared service rather than a store, so this transport cannot bypass a rule the browser would
// be held to.
type LabelService interface {
	Create(ctx context.Context, input labels.CreateInput) (labels.Label, error)
	List(ctx context.Context, input labels.ListInput) ([]labels.Label, error)
	Get(ctx context.Context, input labels.GetInput) (labels.Label, error)
	Update(ctx context.Context, input labels.UpdateInput) (labels.Label, error)
	Delete(ctx context.Context, input labels.DeleteInput) error
	ForIssue(ctx context.Context, input labels.IssueLabelsInput) ([]labels.Label, error)
	Attach(ctx context.Context, input labels.AttachInput) (bool, error)
	Detach(ctx context.Context, input labels.DetachInput) error
	Counts(ctx context.Context, input labels.CountsInput) (map[string]int, error)
}

// labelResponse is one label.
type labelResponse struct {
	Name string `json:"name"`
	// Color is the bare six lowercase hexadecimal digits, which is the stored form §6.5 fixes.
	Color string `json:"color"`
	// RenderedColor is the same value with a leading `#`, so a client does not have to know the rule.
	RenderedColor string `json:"rendered_color"`
	Description   string `json:"description"`
	// Issues counts how many issues carry the label, which is what a label list shows beside it.
	Issues int `json:"issues"`
}

// newLabelResponse renders one label.
func newLabelResponse(label labels.Label, issueCount int) labelResponse {
	return labelResponse{
		Name:          label.Name,
		Color:         label.Color,
		RenderedColor: label.DisplayColor(),
		Description:   label.Description,
		Issues:        issueCount,
	}
}

// labelBody is the accepted request body for a create.
type labelBody struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// labelPatch is the accepted request body for an update.
//
// Every field is a pointer, because "absent" and "set to the empty value" are different requests: a
// description may legitimately be cleared, while a name may not be omitted from a change that names one.
type labelPatch struct {
	Name        *string `json:"name"`
	Color       *string `json:"color"`
	Description *string `json:"description"`
}

// labelAttachBody is the accepted request body for applying a label to an issue.
type labelAttachBody struct {
	Name string `json:"name"`
}

// RegisterLabels adds the label endpoints to mux.
//
// The issue service is a dependency because a label is applied under an issue's number, so the issue's
// identifier has to be resolved — through the shared service, which checks the caller's read access
// before the identifier is known.
func RegisterLabels(mux *http.ServeMux, accounts AccountService, owners RepositoryOwnerLookup, repositories RepositoryService, labelService LabelService, issuesService IssueService) {
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/labels", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}

		found, err := labelService.List(r.Context(), labels.ListInput{
			Actor: principal, RepositoryID: row.ID,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		// The counts travel with the list, because a label list shows them beside every label and a
		// second request per label would be one query each.
		counts, err := labelService.Counts(r.Context(), labels.CountsInput{
			Actor: principal, RepositoryID: row.ID,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}

		items := make([]labelResponse, 0, len(found))
		for _, label := range found {
			items = append(items, newLabelResponse(label, counts[label.ID]))
		}
		writeAPIJSON(w, http.StatusOK, false, items)
	})

	mux.HandleFunc("POST /api/v1/repos/{owner}/{repo}/labels", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}

		var body labelBody
		if !decodeJSONBody(w, r, &body) {
			return
		}
		created, err := labelService.Create(r.Context(), labels.CreateInput{
			Actor: principal, RepositoryID: row.ID,
			Name: body.Name, Color: body.Color, Description: body.Description,
			Request: requestContextFor(r),
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		// 201 with a Location naming the label, because the name is the address from here on.
		w.Header().Set("Location", labelLocation(r, created.Name))
		writeAPIJSON(w, http.StatusCreated, false, newLabelResponse(created, 0))
	})

	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/labels/{label}", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}
		found, err := labelService.Get(r.Context(), labels.GetInput{
			Actor: principal, RepositoryID: row.ID, Name: r.PathValue("label"),
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		counts, err := labelService.Counts(r.Context(), labels.CountsInput{
			Actor: principal, RepositoryID: row.ID,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, false, newLabelResponse(found, counts[found.ID]))
	})

	mux.HandleFunc("PATCH /api/v1/repos/{owner}/{repo}/labels/{label}", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}

		var patch labelPatch
		if !decodeJSONBody(w, r, &patch) {
			return
		}
		if patch.Name == nil && patch.Color == nil && patch.Description == nil {
			writeError(w, http.StatusUnprocessableEntity,
				"a patch must name a name, a color, or a description")
			return
		}

		updated, err := labelService.Update(r.Context(), labels.UpdateInput{
			Actor: principal, RepositoryID: row.ID, Name: r.PathValue("label"),
			NewName: patch.Name, Color: patch.Color, Description: patch.Description,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		// A rename moves the address, so the new one is returned — which is what stops a client from
		// having to guess it from the request it sent.
		if patch.Name != nil {
			w.Header().Set("Location", labelLocation(r, updated.Name))
		}
		writeAPIJSON(w, http.StatusOK, false, newLabelResponse(updated, 0))
	})

	mux.HandleFunc("DELETE /api/v1/repos/{owner}/{repo}/labels/{label}", func(w http.ResponseWriter, r *http.Request) {
		row, principal, ok := resolveIssueRepository(w, r, accounts, owners, repositories)
		if !ok {
			return
		}
		if err := labelService.Delete(r.Context(), labels.DeleteInput{
			Actor: principal, RepositoryID: row.ID, Name: r.PathValue("label"),
			Request: requestContextFor(r),
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		// No body: the label is gone, and the issues it was on are not, which is what the acceptance
		// requires. A client that re-reads the issues finds them unchanged.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/issues/{number}/labels", func(w http.ResponseWriter, r *http.Request) {
		row, principal, issue, ok := resolveCommentIssue(w, r, accounts, owners, repositories, issuesService)
		if !ok {
			return
		}
		found, err := labelService.ForIssue(r.Context(), labels.IssueLabelsInput{
			Actor: principal, RepositoryID: row.ID, IssueID: issue.ID,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		items := make([]labelResponse, 0, len(found))
		for _, label := range found {
			items = append(items, newLabelResponse(label, 0))
		}
		writeAPIJSON(w, http.StatusOK, false, items)
	})

	mux.HandleFunc("POST /api/v1/repos/{owner}/{repo}/issues/{number}/labels", func(w http.ResponseWriter, r *http.Request) {
		row, principal, issue, ok := resolveCommentIssue(w, r, accounts, owners, repositories, issuesService)
		if !ok {
			return
		}

		var body labelAttachBody
		if !decodeJSONBody(w, r, &body) {
			return
		}
		created, err := labelService.Attach(r.Context(), labels.AttachInput{
			Actor: principal, RepositoryID: row.ID, IssueID: issue.ID, Name: body.Name,
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}
		// **201 when the attachment was new and 200 when it already existed**, because the operation's
		// result is a state rather than an event: applying a label the issue already carries is a
		// success that changed nothing, and the status says which happened.
		status := http.StatusOK
		if created {
			status = http.StatusCreated
			w.Header().Set("Location", labelLocation(r, body.Name))
		}
		writeAPIJSON(w, status, false, map[string]any{"name": body.Name, "applied": created})
	})

	mux.HandleFunc("DELETE /api/v1/repos/{owner}/{repo}/issues/{number}/labels/{label}", func(w http.ResponseWriter, r *http.Request) {
		row, principal, issue, ok := resolveCommentIssue(w, r, accounts, owners, repositories, issuesService)
		if !ok {
			return
		}
		if err := labelService.Detach(r.Context(), labels.DetachInput{
			Actor: principal, RepositoryID: row.ID, IssueID: issue.ID, Name: r.PathValue("label"),
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
}

// labelLocation renders the canonical path of a label.
//
// The name is used verbatim rather than escaped: `urls-and-identifiers.md` §6.5 permits any printable
// characters in a name, so a name may contain a space or a slash — and `net/http` matches a path value
// against the decoded path, so escaping here would produce a Location a client could not follow back.
func labelLocation(r *http.Request, name string) string {
	return "/api/v1/repos/" + r.PathValue("owner") + "/" + r.PathValue("repo") +
		"/labels/" + name
}
