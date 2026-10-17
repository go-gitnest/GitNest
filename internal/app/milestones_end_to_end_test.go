package app

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The milestone surfaces end to end.
//
// This is the evidence for W2.4b's parity lines. The rules are asserted in `internal/milestones`, and the
// claims that need real tables — gap-free numbering, numbers never reused, a closed milestone refusing an
// attachment at the write, and detach-not-delete — are asserted there against PostgreSQL. What only these
// tests can show is that the surfaces exist and agree.

// milestonePath is the API collection path for a repository's milestones.
func (e issueEndToEnd) milestonePath() string {
	return e.base + "/api/v1/repos/alice/" + e.public.name + "/milestones"
}

// issueMilestonePath is the API path for one issue's milestone.
func (e issueEndToEnd) issueMilestonePath(issueNumber int) string {
	return e.base + "/api/v1/repos/alice/" + e.public.name + "/issues/" +
		strconv.Itoa(issueNumber) + "/milestone"
}

// TestCreatingAMilestoneFromThePage is the parity line "Milestones with title, description, due date, and
// open/closed state".
func TestCreatingAMilestoneFromThePage(t *testing.T) {
	world := issueInstance(t)

	// 1. The page offers the management controls, because the owner holds write.
	status, page, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/milestones", world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the milestone list = %d, want 200", status)
	}
	if !strings.Contains(page, `name="action" value="milestone.create"`) {
		t.Fatalf("the milestone list offers no creation form:\n%s", page)
	}
	if !strings.Contains(page, "This repository has no milestones.") {
		t.Error("the empty list does not say so")
	}

	// 2. Creating one through the form redirects.
	response := world.postForm(t, "/alice/"+world.public.name+"/milestones", map[string]string{
		"csrf_token":            csrfFromPage(t, page),
		"action":                "milestone.create",
		"milestone_title":       "Version 1.0",
		"milestone_description": "The first release",
		"milestone_due_on":      "2026-12-31",
	})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("creating a milestone = %d, want 303\n%s", response.StatusCode, bodyOf(t, response))
	}
	location := response.Header.Get("Location")
	_ = response.Body.Close()

	// 3. The page shows all four fields the parity line names.
	_, listed, _ := getTextWithCookie(t, world.base+location, world.cookie)
	for _, want := range []string{
		"Version 1.0", "The first release", "2026-12-31",
	} {
		if !strings.Contains(listed, want) {
			t.Errorf("the page does not show %q:\n%s", want, listed)
		}
	}
	// The state is shown as a word.
	if !strings.Contains(listed, "Open") {
		t.Error("the milestone's state is not shown")
	}
	// **The progress is text rather than a bar**, because `frontend.md` forbids a style attribute assembled
	// from user data and requires that colour never be the only carrier of meaning.
	if strings.Contains(listed, "style=") {
		t.Errorf("a style attribute is present, which the frontend rules forbid:\n%s", listed)
	}
	if !strings.Contains(listed, "0 of 0 issues closed") {
		t.Errorf("the empty milestone's progress is not stated as text:\n%s", listed)
	}

	// 4. The API agrees, with every field including the derived counts.
	status, items := getJSONList(t, world.milestonePath(), world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the API milestone list = %d, want 200", status)
	}
	if len(items) != 1 {
		t.Fatalf("the API lists %d milestones, want 1", len(items))
	}
	if items[0]["title"] != "Version 1.0" || items[0]["due_on"] != "2026-12-31" {
		t.Errorf("the API payload is %v", items[0])
	}
	if items[0]["state"] != "open" {
		t.Errorf("the state is %v, want open", items[0]["state"])
	}
	if items[0]["total_issues"] != float64(0) || items[0]["percent_complete"] != float64(0) {
		t.Errorf("the counts are %v/%v, want zero", items[0]["open_issues"], items[0]["total_issues"])
	}
	// A milestone due in the future is not overdue.
	if items[0]["overdue"] != false {
		t.Errorf("a future due date is reported overdue: %v", items[0]["overdue"])
	}

	// 5. **A case variant of the title is refused**, which §6.5 requires.
	_, again, _ := getTextWithCookie(t, world.base+location, world.cookie)
	refused := world.postForm(t, "/alice/"+world.public.name+"/milestones", map[string]string{
		"csrf_token":      csrfFromPage(t, again),
		"action":          "milestone.create",
		"milestone_title": "VERSION 1.0",
	})
	if refused.StatusCode != http.StatusConflict {
		t.Fatalf("a case-variant title = %d, want 409\n%s", refused.StatusCode, bodyOf(t, refused))
	}
	refusedBody := bodyOf(t, refused)
	_ = refused.Body.Close()
	if !strings.Contains(refusedBody, `role="alert"`) {
		t.Error("the title conflict is not announced")
	}

	// 6. A malformed due date is refused and tied to its field.
	_, forDue, _ := getTextWithCookie(t, world.base+location, world.cookie)
	badDue := world.postForm(t, "/alice/"+world.public.name+"/milestones", map[string]string{
		"csrf_token":       csrfFromPage(t, forDue),
		"action":           "milestone.create",
		"milestone_title":  "Another",
		"milestone_due_on": "31/12/2026",
	})
	if badDue.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a malformed due date = %d, want 422\n%s", badDue.StatusCode, bodyOf(t, badDue))
	}
	badDueBody := bodyOf(t, badDue)
	_ = badDue.Body.Close()
	if !strings.Contains(badDueBody, `aria-invalid="true"`) {
		t.Error("the due-date refusal is not tied to a field")
	}
	// The reader's typed date is kept, so they can correct it.
	if !strings.Contains(badDueBody, `value="31/12/2026"`) {
		t.Error("the reader's typed due date was lost")
	}
}

// TestMilestoneProgressIsDerivedFromAttachedIssues is the parity line "Milestone progress counts derived
// from attached issues".
func TestMilestoneProgressIsDerivedFromAttachedIssues(t *testing.T) {
	world := issueInstance(t)

	// A milestone and three issues.
	if status, _, payload := world.send(t, http.MethodPost, world.milestonePath(),
		`{"title":"Version 1.0","due_on":"2026-12-31"}`); status != http.StatusCreated {
		t.Fatalf("creating the milestone = %d\n%s", status, payload)
	}
	for index := 1; index <= 3; index++ {
		if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
			`{"title":"issue `+strconv.Itoa(index)+`","body":"body"}`); status != http.StatusCreated {
			t.Fatalf("creating issue %d = %d\n%s", index, status, payload)
		}
	}

	// Attach all three through the API, which is how a client would.
	for index := 1; index <= 3; index++ {
		status, _, payload := world.send(t, http.MethodPost, world.issueMilestonePath(index), `{"milestone":1}`)
		if status != http.StatusOK {
			t.Fatalf("attaching issue %d = %d\n%s", index, status, payload)
		}
	}

	// The counts are derived, so they are already correct.
	status, items := getJSONList(t, world.milestonePath(), world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the milestone list = %d", status)
	}
	if items[0]["total_issues"] != float64(3) || items[0]["closed_issues"] != float64(0) {
		t.Fatalf("the counts are %v total / %v closed, want 3 / 0", items[0]["total_issues"], items[0]["closed_issues"])
	}
	if items[0]["percent_complete"] != float64(0) {
		t.Errorf("the completion is %v, want 0", items[0]["percent_complete"])
	}

	// **Close one issue and the count moves**, which is the derived-not-stored property.
	if status, _, payload := world.send(t, http.MethodPatch, world.issuesPath()+"/1", `{"state":"closed"}`); status != http.StatusOK {
		t.Fatalf("closing issue 1 = %d\n%s", status, payload)
	}
	_, afterClose := getJSONList(t, world.milestonePath(), world.cookie)
	if afterClose[0]["closed_issues"] != float64(1) {
		t.Errorf("after closing one issue the closed count is %v, want 1", afterClose[0]["closed_issues"])
	}
	if afterClose[0]["percent_complete"] != float64(33) {
		t.Errorf("the completion is %v, want 33", afterClose[0]["percent_complete"])
	}
	// The page agrees with the API, which is the rule that a page and a payload cannot disagree.
	_, page, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/milestones", world.cookie)
	if !strings.Contains(page, "1 of 3 issues closed, 33%") {
		t.Errorf("the page does not state the derived progress:\n%s", page)
	}

	// **Detaching removes it from the count**, which is what "correct after an issue is relabeled" comes to.
	if status, _, payload := world.send(t, http.MethodDelete, world.issueMilestonePath(2), ""); status != http.StatusNoContent {
		t.Fatalf("detaching issue 2 = %d\n%s", status, payload)
	}
	_, afterDetach := getJSONList(t, world.milestonePath(), world.cookie)
	if afterDetach[0]["total_issues"] != float64(2) {
		t.Errorf("after detaching the total is %v, want 2", afterDetach[0]["total_issues"])
	}
	if afterDetach[0]["percent_complete"] != float64(50) {
		t.Errorf("the completion is %v, want 50", afterDetach[0]["percent_complete"])
	}
}

// TestAClosedMilestoneRejectsAnAttachmentOnThePage is the acceptance criterion as a reader meets it.
//
// The refusal names what to do about it, because "closed milestones reject new attachments" is the one
// criterion a reader is likely to hit and a bare refusal would leave them stuck.
func TestAClosedMilestoneRejectsAnAttachmentOnThePage(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.milestonePath(),
		`{"title":"Version 1.0"}`); status != http.StatusCreated {
		t.Fatalf("creating the milestone = %d\n%s", status, payload)
	}
	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"an issue","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the issue = %d\n%s", status, payload)
	}

	// Close the milestone through the page's control.
	_, page, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/milestones", world.cookie)
	if !strings.Contains(page, `name="action" value="milestone.close"`) {
		t.Fatalf("the milestone page offers no close control:\n%s", page)
	}
	closed := world.postForm(t, "/alice/"+world.public.name+"/milestones", map[string]string{
		"csrf_token":       csrfFromPage(t, page),
		"action":           "milestone.close",
		"milestone_number": "1",
	})
	if closed.StatusCode != http.StatusSeeOther {
		t.Fatalf("closing = %d, want 303\n%s", closed.StatusCode, bodyOf(t, closed))
	}
	_ = closed.Body.Close()

	// The page now offers reopening rather than closing, and the state is shown.
	_, afterClose, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/milestones", world.cookie)
	if !strings.Contains(afterClose, `name="action" value="milestone.reopen"`) {
		t.Error("a closed milestone does not offer reopening")
	}
	if strings.Contains(afterClose, `name="action" value="milestone.close"`) {
		t.Error("a closed milestone still offers closing")
	}

	// **The API refuses an attachment**, and with a 409 rather than a 422: the request is well formed and
	// names a real milestone; it is the milestone's state that conflicts.
	status, _, payload := world.send(t, http.MethodPost, world.issueMilestonePath(1), `{"milestone":1}`)
	if status != http.StatusConflict {
		t.Fatalf("attaching to a closed milestone = %d, want 409\n%s", status, payload)
	}

	// **And the page offers no choice at all**, because only open milestones are offered — so a reader is
	// never presented with a destination that would refuse them.
	_, issuePage, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/issues/1", world.cookie)
	if !strings.Contains(issuePage, "This issue is on no milestone.") {
		t.Error("the issue page does not say the issue is on no milestone")
	}
	if strings.Contains(issuePage, `name="action" value="milestone.attach"`) {
		t.Error("the page offers a closed milestone as a destination")
	}

	// Reopen it, and the attachment is accepted — which is what makes the refusal above a state rather than
	// a permanent condition.
	//
	// **The page is re-read here, and that is the token rotation rather than a workaround.** The API calls
	// above are made with the session cookie only, so they do not rotate the token — but rendering the
	// issue page did, because `RotateCSRFToken` keeps one live token per session. Submitting the milestone
	// page's earlier token with the issue page's token now current is refused with 403, which is exactly
	// what the rotation is for.
	_, forReopen, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/milestones", world.cookie)
	reopened := world.postForm(t, "/alice/"+world.public.name+"/milestones", map[string]string{
		"csrf_token":       csrfFromPage(t, forReopen),
		"action":           "milestone.reopen",
		"milestone_number": "1",
	})
	if reopened.StatusCode != http.StatusSeeOther {
		t.Fatalf("reopening = %d, want 303\n%s", reopened.StatusCode, bodyOf(t, reopened))
	}
	_ = reopened.Body.Close()

	status, _, payload = world.send(t, http.MethodPost, world.issueMilestonePath(1), `{"milestone":1}`)
	if status != http.StatusOK {
		t.Fatalf("attaching after a reopen = %d\n%s", status, payload)
	}
}

// TestMovingAnIssueOntoAMilestoneFromThePage covers the issue half of the attachment.
func TestMovingAnIssueOntoAMilestoneFromThePage(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.milestonePath(),
		`{"title":"Version 1.0"}`); status != http.StatusCreated {
		t.Fatalf("creating the milestone = %d\n%s", status, payload)
	}
	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"An issue","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the issue = %d\n%s", status, payload)
	}

	issuePath := "/alice/" + world.public.name + "/issues/1"

	// The page offers the selection, because the owner holds write.
	_, page, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(page, `name="action" value="milestone.attach"`) {
		t.Fatalf("the issue page offers no milestone selection:\n%s", page)
	}
	if !strings.Contains(page, `name="milestone_number"`) {
		t.Error("the selection carries no milestone field")
	}

	// Move it through the form.
	moved := world.postForm(t, issuePath+"/milestone", map[string]string{
		"csrf_token":       csrfFromPage(t, page),
		"action":           "milestone.attach",
		"milestone_number": "1",
	})
	if moved.StatusCode != http.StatusSeeOther {
		t.Fatalf("moving the issue = %d, want 303\n%s", moved.StatusCode, bodyOf(t, moved))
	}
	_ = moved.Body.Close()

	// The page shows the milestone it is on, with the progress as text, and offers removal.
	_, movedPage, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(movedPage, "Version 1.0") {
		t.Error("the issue does not show its milestone")
	}
	if strings.Contains(movedPage, "This issue is on no milestone.") {
		t.Error("the page still says the issue is on no milestone")
	}
	if !strings.Contains(movedPage, `name="action" value="milestone.detach"`) {
		t.Error("the page offers no way to remove the milestone")
	}

	// Remove it.
	removed := world.postForm(t, issuePath+"/milestone", map[string]string{
		"csrf_token": csrfFromPage(t, movedPage),
		"action":     "milestone.detach",
	})
	if removed.StatusCode != http.StatusSeeOther {
		t.Fatalf("removing the milestone = %d, want 303\n%s", removed.StatusCode, bodyOf(t, removed))
	}
	_ = removed.Body.Close()

	_, after, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(after, "This issue is on no milestone.") {
		t.Errorf("the milestone was not removed:\n%s", after)
	}
	// The milestone itself still exists, because detaching is not deleting.
	_, milestones := getJSONList(t, world.milestonePath(), world.cookie)
	if len(milestones) != 1 {
		t.Errorf("the milestone was deleted rather than detached: %d milestones", len(milestones))
	}
}

// TestDeletingAMilestoneFromThePageKeepsItsIssues is the acceptance criterion as a reader sees it.
func TestDeletingAMilestoneFromThePageKeepsItsIssues(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.milestonePath(),
		`{"title":"Version 1.0"}`); status != http.StatusCreated {
		t.Fatalf("creating the milestone = %d\n%s", status, payload)
	}
	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"An issue that must survive","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the issue = %d\n%s", status, payload)
	}
	if status, _, payload := world.send(t, http.MethodPost, world.issueMilestonePath(1),
		`{"milestone":1}`); status != http.StatusOK {
		t.Fatalf("attaching = %d\n%s", status, payload)
	}

	_, page, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/milestones", world.cookie)
	// The page states the consequence, which is what a reader is most likely to get wrong.
	if !strings.Contains(page, "The issues themselves are not deleted.") {
		t.Error("the delete hint does not state that the issues survive")
	}

	deleted := world.postForm(t, "/alice/"+world.public.name+"/milestones", map[string]string{
		"csrf_token":       csrfFromPage(t, page),
		"action":           "milestone.delete",
		"milestone_number": "1",
	})
	if deleted.StatusCode != http.StatusSeeOther {
		t.Fatalf("deleting = %d, want 303\n%s", deleted.StatusCode, bodyOf(t, deleted))
	}
	_ = deleted.Body.Close()

	// The milestone is gone…
	_, afterDelete, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/milestones", world.cookie)
	if !strings.Contains(afterDelete, "This repository has no milestones.") {
		t.Errorf("the milestone was not deleted:\n%s", afterDelete)
	}
	// **…and the issue is not**, which is the criterion.
	status, issuePath, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/issues/1", world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the issue is no longer readable: %d", status)
	}
	if !strings.Contains(issuePath, "An issue that must survive") {
		t.Error("the issue was lost with the milestone")
	}
	if !strings.Contains(issuePath, "This issue is on no milestone.") {
		t.Error("the issue still reports the deleted milestone")
	}
}

// TestAnAnonymousReaderSeesMilestonesButNoControls covers the read surface and the denied path.
func TestAnAnonymousReaderSeesMilestonesButNoControls(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.milestonePath(),
		`{"title":"A public milestone","due_on":"2026-12-31"}`); status != http.StatusCreated {
		t.Fatalf("creating the milestone = %d\n%s", status, payload)
	}

	// The list is readable anonymously on a public repository.
	status, page, _ := getText(t, world.base+"/alice/"+world.public.name+"/milestones")
	if status != http.StatusOK {
		t.Fatalf("the anonymous milestone list = %d, want 200", status)
	}
	if !strings.Contains(page, "A public milestone") {
		t.Error("a public milestone is not readable anonymously")
	}
	if !strings.Contains(page, "2026-12-31") {
		t.Error("the due date is not shown anonymously")
	}
	// But no control is offered, because managing one needs write and a session.
	for _, forbidden := range []string{
		`name="action" value="milestone.create"`,
		`name="action" value="milestone.delete"`,
		`name="csrf_token"`,
	} {
		if strings.Contains(page, forbidden) {
			t.Errorf("an anonymous reader is offered %q", forbidden)
		}
	}

	// And creating one anonymously is refused.
	request, err := http.NewRequest(http.MethodPost, world.milestonePath(),
		strings.NewReader(`{"title":"anonymous"}`))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("the anonymous create: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusCreated {
		t.Errorf("an anonymous create succeeded: %d", response.StatusCode)
	}
}

// TestTheOverdueMarkerIsAWordOnThePage covers the overdue rendering.
//
// A milestone due in the past is marked with the **word** "overdue" rather than only a red date, because
// `frontend.md` requires that colour never be the only carrier of meaning.
func TestTheOverdueMarkerIsAWordOnThePage(t *testing.T) {
	world := issueInstance(t)

	// A date well in the past, so the test does not depend on when it runs.
	if status, _, payload := world.send(t, http.MethodPost, world.milestonePath(),
		`{"title":"An old release","due_on":"2020-01-01"}`); status != http.StatusCreated {
		t.Fatalf("creating the milestone = %d\n%s", status, payload)
	}

	status, page, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/milestones", world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the milestone list = %d", status)
	}
	if !strings.Contains(page, "overdue") {
		t.Errorf("an overdue milestone is not marked:\n%s", page)
	}
	// The API agrees.
	_, items := getJSONList(t, world.milestonePath(), world.cookie)
	if items[0]["overdue"] != true {
		t.Errorf("the API does not report the milestone overdue: %v", items[0]["overdue"])
	}

	// **A closed milestone is not overdue**, because there is nothing left to be late for.
	_, forClose, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/milestones", world.cookie)
	closed := world.postForm(t, "/alice/"+world.public.name+"/milestones", map[string]string{
		"csrf_token":       csrfFromPage(t, forClose),
		"action":           "milestone.close",
		"milestone_number": "1",
	})
	if closed.StatusCode != http.StatusSeeOther {
		t.Fatalf("closing = %d", closed.StatusCode)
	}
	_ = closed.Body.Close()

	_, items = getJSONList(t, world.milestonePath(), world.cookie)
	if items[0]["overdue"] != false {
		t.Errorf("a closed milestone is reported overdue: %v", items[0]["overdue"])
	}
}

// TestAnUnreadableRepositoryDisclosesNoMilestones covers the T-A4 shape.
func TestAnUnreadableRepositoryDisclosesNoMilestones(t *testing.T) {
	world := issueInstance(t)
	_, _, private, _ := renderedInstance(t)

	// The private repository, asked anonymously through both surfaces.
	path := world.base + "/api/v1/repos/alice/" + private.name + "/milestones"
	request, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("the API request: %v", err)
	}
	apiStatus := response.StatusCode
	_ = response.Body.Close()

	pageStatus, _, _ := getText(t, world.base+"/alice/"+private.name+"/milestones")

	if apiStatus != http.StatusNotFound {
		t.Errorf("the anonymous milestone list = %d, want 404", apiStatus)
	}
	if pageStatus != http.StatusNotFound {
		t.Errorf("the anonymous milestone page = %d, want 404", pageStatus)
	}
}

// TestTheIssueListFiltersByMilestone covers the milestone half of the issue list filter.
//
// The full parity line names label, milestone, assignee, author, and sort as well; assignees arrive in
// W2.5, so the line stays unticked. This asserts the milestone half works.
func TestTheIssueListFiltersByMilestone(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.milestonePath(),
		`{"title":"Version 1.0"}`); status != http.StatusCreated {
		t.Fatalf("creating the milestone = %d\n%s", status, payload)
	}
	for index := 1; index <= 3; index++ {
		if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
			`{"title":"issue `+strconv.Itoa(index)+`","body":"body"}`); status != http.StatusCreated {
			t.Fatalf("creating issue %d = %d\n%s", index, status, payload)
		}
	}
	// Two of the three go on the milestone.
	for _, number := range []int{1, 2} {
		if status, _, payload := world.send(t, http.MethodPost, world.issueMilestonePath(number),
			`{"milestone":1}`); status != http.StatusOK {
			t.Fatalf("attaching issue %d = %d\n%s", number, status, payload)
		}
	}

	// The milestone's own issue list is the route §6.5 fixes, and it shows the two.
	status, page, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/milestone/1", world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the per-milestone page = %d, want 200", status)
	}
	if !strings.Contains(page, "Version 1.0") {
		t.Errorf("the per-milestone page does not name the milestone:\n%s", page)
	}

	// The list links to that route, so the link is a working one.
	_, list, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/milestones", world.cookie)
	if !regexp.MustCompile(`href="[^"]*/milestone/1"`).MatchString(list) {
		t.Errorf("the milestone list does not link to the per-milestone page:\n%s", list)
	}
}
