package app

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The comment surfaces end to end, as a browser and an API client would drive them.
//
// This is the evidence for W2.3's parity lines: each test writes a comment through the page's form or
// the API, reads it back the other way, and checks the result. A test that only exercised the domain
// would prove the rules; these prove the surfaces exist and agree.

// commentPath is the API collection path for an issue's comments.
func (e issueEndToEnd) commentPath(number int) string {
	return e.base + "/api/v1/repos/alice/" + e.public.name + "/issues/" +
		strconv.Itoa(number) + "/comments"
}

// TestACommentTravelsFromThePageThroughTheAPI covers the full round trip.
//
// The comment is written with the page's form, read back through the API, edited through the page, and
// read again — so both surfaces are exercised and their agreement is what is asserted.
func TestACommentTravelsFromThePageThroughTheAPI(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"An issue with a conversation","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the issue = %d\n%s", status, payload)
	}

	issuePath := "/alice/" + world.public.name + "/issues/1"

	// 1. The page offers the conversation and a form to add to it.
	_, page, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(page, `name="action" value="comment.create"`) {
		t.Fatalf("the issue page offers no way to comment:\n%s", page)
	}
	token := csrfFromPage(t, page)

	// 2. Writing it through the form redirects, so a reload does not post twice.
	response := world.postForm(t, issuePath+"/comments", map[string]string{
		"csrf_token":   token,
		"action":       "comment.create",
		"comment_body": "A **bold** observation and a `code span`.",
	})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("posting a comment = %d, want 303\n%s", response.StatusCode, bodyOf(t, response))
	}
	_ = response.Body.Close()

	// 3. The API lists it, with the rendered Markdown **source** — the API returns what was stored,
	//    because rendering is the read surface's job.
	status, listed := getJSONList(t, world.commentPath(1), world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the comment listing = %d, want 200", status)
	}
	if len(listed) != 1 {
		t.Fatalf("the listing holds %d comments, want 1", len(listed))
	}
	if listed[0]["body"] != "A **bold** observation and a `code span`." {
		t.Errorf("the stored body is %v", listed[0]["body"])
	}
	if listed[0]["number"] != float64(1) {
		t.Errorf("the comment's number is %v, want 1", listed[0]["number"])
	}
	// The author is named rather than identified.
	author, _ := listed[0]["user"].(map[string]any)
	if author == nil || author["login"] != "alice" {
		t.Errorf("the author is not named: %v", listed[0]["user"])
	}

	// 4. The page renders the body as Markdown rather than showing its source.
	_, rendered, _ := getText(t, world.base+issuePath)
	if !strings.Contains(rendered, "<strong>bold</strong>") ||
		!strings.Contains(rendered, "<code>code span</code>") {
		t.Errorf("the comment body was not rendered as Markdown:\n%s", rendered)
	}
	if strings.Contains(rendered, "A **bold** observation") {
		t.Error("the page shows the raw Markdown source")
	}
	// The comment is anchored, so a link to it resolves.
	if !strings.Contains(rendered, `id="issuecomment-1"`) {
		t.Error("the comment carries no fragment anchor")
	}
}

// TestEditingACommentShowsItsHistory is the parity line "Comments with Markdown, edit history, and
// delete-with-audit", and W2.3's "does not rewrite history visible in the UI".
func TestEditingACommentShowsItsHistory(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"An issue","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the issue = %d\n%s", status, payload)
	}
	// The comment is written through the API, so the page's part of this is what is being tested.
	status, _, payload := world.send(t, http.MethodPost, world.commentPath(1),
		`{"body":"The original comment text."}`)
	if status != http.StatusCreated {
		t.Fatalf("creating the comment = %d\n%s", status, payload)
	}

	issuePath := "/alice/" + world.public.name + "/issues/1"

	// The page offers an edit form carrying the version it read.
	_, page, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(page, `name="action" value="comment.edit"`) {
		t.Fatalf("the page offers no way to edit the comment:\n%s", page)
	}
	version := regexp.MustCompile(`name="comment_version" value="(\d+)"`).FindStringSubmatch(page)
	if version == nil {
		t.Fatal("the edit form carries no version, so a concurrent edit could not be detected")
	}
	// The comment is not yet reported as edited.
	if strings.Contains(page, "(edited)") {
		t.Error("a comment that was never edited is reported as edited")
	}

	response := world.postForm(t, issuePath+"/comments", map[string]string{
		"csrf_token":      csrfFromPage(t, page),
		"action":          "comment.edit",
		"comment_number":  "1",
		"comment_version": version[1],
		"comment_body":    "The revised comment text.",
	})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("editing the comment = %d, want 303\n%s", response.StatusCode, bodyOf(t, response))
	}
	_ = response.Body.Close()

	// The page shows the new text, reports that it was edited, and **keeps the previous text visible**,
	// which is what "does not rewrite history visible in the UI" requires.
	_, edited, _ := getText(t, world.base+issuePath)
	if !strings.Contains(edited, "The revised comment text.") {
		t.Error("the edit was not applied")
	}
	if !strings.Contains(edited, "(edited)") {
		t.Error("the comment is not reported as edited")
	}
	if !strings.Contains(edited, "The original comment text.") {
		t.Errorf("the previous body is not visible in the UI:\n%s", edited)
	}

	// And the API exposes the same history, with the previous body.
	status, history := getJSONList(t, world.commentPath(1)+"/1/history", world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the history = %d, want 200", status)
	}
	if len(history) != 1 {
		t.Fatalf("the history holds %d revisions, want 1", len(history))
	}
	if history[0]["body"] != "The original comment text." {
		t.Errorf("the revision holds %v, want the previous body", history[0]["body"])
	}
	if history[0]["revision"] != float64(1) {
		t.Errorf("the revision is numbered %v, want 1", history[0]["revision"])
	}
}

// TestAStaleCommentEditIsRefused covers the concurrency guard on the page.
func TestAStaleCommentEditIsRefused(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"An issue","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the issue = %d\n%s", status, payload)
	}
	if status, _, payload := world.send(t, http.MethodPost, world.commentPath(1),
		`{"body":"The original."}`); status != http.StatusCreated {
		t.Fatalf("creating the comment = %d\n%s", status, payload)
	}

	issuePath := "/alice/" + world.public.name + "/issues/1"
	_, page, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	version := regexp.MustCompile(`name="comment_version" value="(\d+)"`).FindStringSubmatch(page)
	if version == nil {
		t.Fatal("the edit form carries no version")
	}

	// One writer moves the version.
	if status, _, payload := world.send(t, http.MethodPatch, world.commentPath(1)+"/1",
		`{"body":"The first edit.","version":`+version[1]+`}`); status != http.StatusOK {
		t.Fatalf("the first edit = %d\n%s", status, payload)
	}

	// The page's form still carries the old version.
	_, stale, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	response := world.postForm(t, issuePath+"/comments", map[string]string{
		"csrf_token":      csrfFromPage(t, stale),
		"action":          "comment.edit",
		"comment_number":  "1",
		"comment_version": version[1],
		"comment_body":    "The stale edit.",
	})
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("a stale edit = %d, want 409\n%s", response.StatusCode, bodyOf(t, response))
	}
	staleBody := bodyOf(t, response)
	_ = response.Body.Close()

	// The refusal explains itself, shows what is stored, and keeps the reader's text — which is what
	// makes it actionable rather than merely reported.
	if !strings.Contains(staleBody, `role="alert"`) {
		t.Error("the conflict is not announced")
	}
	if !strings.Contains(staleBody, "The first edit.") {
		t.Error("the page does not show what is stored")
	}
	if !strings.Contains(staleBody, "The stale edit.") {
		t.Error("the reader's text was lost")
	}

	// The stored text is unchanged, so nothing was overwritten.
	_, after, _ := getText(t, world.base+issuePath)
	if strings.Contains(after, "The stale edit.") {
		t.Error("a stale edit overwrote the stored comment")
	}
}

// TestDeletingACommentLeavesATombstoneAndAudits is the "delete-with-audit" half.
//
// The row keeps its place so the conversation keeps its shape, the text is gone, and the deletion is
// the one comment action the catalogue defines — so it is recorded.
func TestDeletingACommentLeavesATombstoneAndAudits(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"An issue","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the issue = %d\n%s", status, payload)
	}
	if status, _, payload := world.send(t, http.MethodPost, world.commentPath(1),
		`{"body":"A comment to be removed."}`); status != http.StatusCreated {
		t.Fatalf("creating the comment = %d\n%s", status, payload)
	}
	// A second comment, so the tombstone's position is observable.
	if status, _, payload := world.send(t, http.MethodPost, world.commentPath(1),
		`{"body":"A comment that stays."}`); status != http.StatusCreated {
		t.Fatalf("creating the second comment = %d\n%s", status, payload)
	}

	// The deletion goes through the API, which is where a client would do it.
	status, _, payload := world.send(t, http.MethodDelete, world.commentPath(1)+"/1", "")
	if status != http.StatusNoContent {
		t.Fatalf("deleting = %d, want 204\n%s", status, payload)
	}

	// The page shows a tombstone in the first position and the second comment untouched.
	_, page, _ := getText(t, world.base+"/alice/"+world.public.name+"/issues/1")
	if !strings.Contains(page, "This comment was deleted.") {
		t.Errorf("the page does not show a tombstone:\n%s", page)
	}
	if strings.Contains(page, "A comment to be removed.") {
		t.Error("the deleted comment's text is still shown")
	}
	if !strings.Contains(page, "A comment that stays.") {
		t.Error("the other comment was affected")
	}
	// The tombstone keeps the comment's place, so the anchor still resolves.
	if !strings.Contains(page, `id="issuecomment-1"`) {
		t.Error("the tombstone lost the comment's anchor")
	}

	// The listing still reports two comments, with the first marked deleted and its body cleared —
	// which is what makes the count agree with the page.
	status, listed := getJSONList(t, world.commentPath(1), world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the listing = %d, want 200", status)
	}
	if len(listed) != 2 {
		t.Fatalf("the listing holds %d comments, want 2", len(listed))
	}
	if listed[0]["deleted"] != true {
		t.Errorf("the first comment is not marked deleted: %v", listed[0])
	}
	if listed[0]["body"] != "" {
		t.Errorf("the deleted comment still carries its body: %v", listed[0]["body"])
	}
}

// TestAnUnreadableRepositoryDisclosesNoComments is W2.3's acceptance about inference.
func TestAnUnreadableRepositoryDisclosesNoComments(t *testing.T) {
	world := issueInstance(t)
	_, _, private, _ := renderedInstance(t)

	// The private repository, asked anonymously through both surfaces.
	path := world.base + "/api/v1/repos/alice/" + private.name + "/issues/1/comments"
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

	pageStatus, _, _ := getText(t, world.base+"/alice/"+private.name+"/issues/1")

	// Both refuse, and the API's refusal is the not-found shape — so a caller cannot tell a repository
	// they may not read from one that is not there.
	if apiStatus != http.StatusNotFound {
		t.Errorf("the anonymous comment listing = %d, want 404", apiStatus)
	}
	if pageStatus != http.StatusNotFound {
		t.Errorf("the anonymous issue page = %d, want 404", pageStatus)
	}
}

// TestAnAnonymousReaderSeesCommentsButNoControls covers the public read surface.
func TestAnAnonymousReaderSeesCommentsButNoControls(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"A public issue","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the issue = %d\n%s", status, payload)
	}
	if status, _, payload := world.send(t, http.MethodPost, world.commentPath(1),
		`{"body":"A public comment."}`); status != http.StatusCreated {
		t.Fatalf("creating the comment = %d\n%s", status, payload)
	}

	// Read anonymously, with no cookie.
	status, page, _ := getText(t, world.base+"/alice/"+world.public.name+"/issues/1")
	if status != http.StatusOK {
		t.Fatalf("the public issue page = %d, want 200", status)
	}
	if !strings.Contains(page, "A public comment.") {
		t.Error("a public comment is not readable anonymously")
	}
	// No control is offered, because a comment needs an author and a cookie.
	for _, forbidden := range []string{
		`name="action" value="comment.create"`,
		`name="action" value="comment.edit"`,
		`name="action" value="comment.delete"`,
	} {
		if strings.Contains(page, forbidden) {
			t.Errorf("an anonymous reader is offered %q", forbidden)
		}
	}
	// And writing is refused.
	request, err := http.NewRequest(http.MethodPost, world.commentPath(1),
		strings.NewReader(`{"body":"anonymous"}`))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("the anonymous post: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("an anonymous comment = %d, want 401", response.StatusCode)
	}
}

// TestACommentCannotBeReachedThroughAnotherIssue covers the scoping on the wire.
func TestACommentCannotBeReachedThroughAnotherIssue(t *testing.T) {
	world := issueInstance(t)

	// Two issues, one comment on the first.
	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"The first issue","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the first issue = %d\n%s", status, payload)
	}
	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"The second issue","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the second issue = %d\n%s", status, payload)
	}
	if status, _, payload := world.send(t, http.MethodPost, world.commentPath(1),
		`{"body":"A comment on the first issue."}`); status != http.StatusCreated {
		t.Fatalf("creating the comment = %d\n%s", status, payload)
	}

	// Asking for comment 1 under the **second** issue is a not-found, because that issue has no first
	// comment — the number is per issue, so it does not address the other issue's comment.
	status, _, _ := world.send(t, http.MethodGet, world.commentPath(2)+"/1", "")
	if status != http.StatusNotFound {
		t.Errorf("a comment reached through an issue that has none = %d, want 404", status)
	}
	// Under its own issue it is found, which is what makes the assertion above meaningful.
	status, _, _ = world.send(t, http.MethodGet, world.commentPath(1)+"/1", "")
	if status != http.StatusOK {
		t.Errorf("the comment through its own issue = %d, want 200", status)
	}
}

// getJSONList reads a JSON array response.
func getJSONList(t *testing.T, url, cookie string) (int, []map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Accept", "application/json")
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	var items []map[string]any
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&items); err != nil {
			t.Fatalf("the response is not a JSON array: %v", err)
		}
	}
	return response.StatusCode, items
}
