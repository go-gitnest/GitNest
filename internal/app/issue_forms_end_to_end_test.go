package app

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// The issue forms end to end, as a browser would drive them.
//
// This is the evidence for the parity lines: each test loads a page, takes the CSRF token the page
// carries, submits the form exactly as a browser does, and reads the result. A test that called the
// API directly would prove the API works — these prove the *page* does, which is what the parity
// lines are about.
//
// The flow is deliberately scriptless: no JavaScript is involved at any point, because the forms
// are plain HTML submissions.

// csrfFromPage reads the token a rendered form carries.
func csrfFromPage(t *testing.T, page string) string {
	t.Helper()
	match := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(page)
	if match == nil {
		t.Fatalf("the page carries no CSRF token:\n%s", page)
	}
	return match[1]
}

// postForm submits a form the way a browser does, returning the response.
//
// Redirects are **not** followed, so a test can assert the redirect rather than its destination —
// the redirect is what stops a reload from repeating the change.
func (e issueEndToEnd) postForm(t *testing.T, path string, fields map[string]string) *http.Response {
	t.Helper()
	form := url.Values{}
	for name, value := range fields {
		form.Set(name, value)
	}
	request, err := http.NewRequest(http.MethodPost, e.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build the form post: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Cookie", e.cookie)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("submit the form: %v", err)
	}
	return response
}

// bodyOf reads and closes a response body.
func bodyOf(t *testing.T, response *http.Response) string {
	t.Helper()
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	return string(payload)
}

// TestCreatingAnIssueFromThePage is the parity line "Create an issue with title and Markdown body",
// driven the way a reader drives it.
func TestCreatingAnIssueFromThePage(t *testing.T) {
	world := issueInstance(t)

	// 1. The page offers the creation link **to a signed-in reader**. The cookie matters: an
	// anonymous reader is deliberately offered no link, because an issue needs an author — and that
	// refusal is asserted on its own further down.
	listStatus, list, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/issues", world.cookie)
	if listStatus != http.StatusOK {
		t.Fatalf("the issue list = %d, want 200", listStatus)
	}
	if !strings.Contains(list, "/issues/new") {
		t.Fatalf("the issue list offers no creation link:\n%s", list)
	}

	// 2. The form loads and carries a token.
	formStatus, form, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/issues/new", world.cookie)
	if formStatus != http.StatusOK {
		t.Fatalf("the creation form = %d, want 200", formStatus)
	}
	token := csrfFromPage(t, form)

	// 3. Submitting it creates the issue and redirects, so a reload does not file it twice.
	response := world.postForm(t, "/alice/"+world.public.name+"/issues", map[string]string{
		"csrf_token": token,
		"title":      "An issue opened from the page",
		"body":       "A **bold** claim and a `code span`.",
	})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("submitting the form = %d, want 303\n%s", response.StatusCode, bodyOf(t, response))
	}
	_ = response.Body.Close()

	// 4. The issue is listed and its page renders the body as Markdown.
	_, detail, _ := getText(t, world.base+"/alice/"+world.public.name+"/issues/1")
	if !strings.Contains(detail, "An issue opened from the page") {
		t.Error("the issue page does not show the title that was submitted")
	}
	if !strings.Contains(detail, "<strong>bold</strong>") || !strings.Contains(detail, "<code>code span</code>") {
		t.Errorf("the Markdown body was not rendered:\n%s", detail)
	}
	if strings.Contains(detail, "A **bold** claim") {
		t.Error("the raw Markdown is shown instead of being rendered")
	}
}

// TestEditingAnIssueFromThePage is the parity line "Edit issue title and body".
//
// **The version guard is exercised here**, because it is the property that makes the form safe: a
// page left open while somebody else edits is refused rather than overwriting. The parity line that
// also asks for an "append-only revision record" is **not** satisfied — the event catalogue defines
// no `issue.edited` — so this test asserts the edit and the conflict, not a revision history.
func TestEditingAnIssueFromThePage(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"Before the edit","body":"The original body"}`); status != http.StatusCreated {
		t.Fatalf("creating = %d\n%s", status, payload)
	}

	issuePath := "/alice/" + world.public.name + "/issues/1"
	_, page, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	token := csrfFromPage(t, page)

	// The page carries the version it was rendered from.
	version := regexp.MustCompile(`name="version" value="(\d+)"`).FindStringSubmatch(page)
	if version == nil {
		t.Fatalf("the page carries no version, so a concurrent edit could not be detected:\n%s", page)
	}

	response := world.postForm(t, issuePath, map[string]string{
		"csrf_token": token,
		"action":     "edit",
		"title":      "After the edit",
		"body":       "The new body with **emphasis**.",
		"version":    version[1],
	})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("the edit = %d, want 303\n%s", response.StatusCode, bodyOf(t, response))
	}
	// The redirect names the outcome, which is how the confirmation survives the redirect that stops
	// a reload from repeating the edit.
	location := response.Header.Get("Location")
	_ = response.Body.Close()
	if !strings.Contains(location, "outcome=saved") {
		t.Fatalf("the redirect is %q, want it to report the outcome", location)
	}

	// Following the redirect shows the confirmation and the applied text.
	_, updated, _ := getText(t, world.base+location)
	if !strings.Contains(updated, "After the edit") {
		t.Error("the edit was not applied")
	}
	if !strings.Contains(updated, "<strong>emphasis</strong>") {
		t.Error("the new body was not rendered as Markdown")
	}
	if !strings.Contains(updated, "saved") {
		t.Error("the page does not confirm that the change was saved")
	}
	// A plain reload — the redirect's own target without the outcome — carries no confirmation, so
	// the notice cannot be mistaken for a permanent part of the page.
	_, reload, _ := getText(t, world.base+issuePath)
	if strings.Contains(reload, "Your change was saved") {
		t.Error("the confirmation is shown on a plain reload, where nothing was saved")
	}

	// **A stale version is refused**, so an edit made against a page that has moved on does not
	// silently overwrite. The token is re-read because the redirect rotated it.
	_, fresh, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	staleToken := csrfFromPage(t, fresh)
	stale := world.postForm(t, issuePath, map[string]string{
		"csrf_token": staleToken,
		"action":     "edit",
		"title":      "A stale edit",
		"body":       "This must not be stored",
		"version":    version[1],
	})
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("a stale edit = %d, want 409\n%s", stale.StatusCode, bodyOf(t, stale))
	}
	_ = stale.Body.Close()

	// And the stored text was not overwritten.
	_, after, _ := getText(t, world.base+issuePath)
	if strings.Contains(after, "A stale edit") {
		t.Error("a stale edit overwrote the stored text")
	}
	if !strings.Contains(after, "After the edit") {
		t.Error("the stored text was lost")
	}
}

// TestClosingAndReopeningFromThePage is the parity line "Close and reopen an issue with
// state-change history".
func TestClosingAndReopeningFromThePage(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"A lifecycle from the page","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating = %d\n%s", status, payload)
	}
	issuePath := "/alice/" + world.public.name + "/issues/1"

	// Close through the button the page offers.
	_, open, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(open, `name="action" value="close"`) {
		t.Fatalf("an open issue offers no close control:\n%s", open)
	}
	closed := world.postForm(t, issuePath, map[string]string{
		"csrf_token": csrfFromPage(t, open),
		"action":     "close",
	})
	if closed.StatusCode != http.StatusSeeOther {
		t.Fatalf("closing = %d, want 303\n%s", closed.StatusCode, bodyOf(t, closed))
	}
	_ = closed.Body.Close()

	// The page now reports the closed state and offers reopening instead.
	_, afterClose, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(afterClose, `name="action" value="reopen"`) {
		t.Errorf("a closed issue offers no reopen control:\n%s", afterClose)
	}
	if strings.Contains(afterClose, `name="action" value="close"`) {
		t.Error("a closed issue still offers closing")
	}

	// Reopen.
	reopened := world.postForm(t, issuePath, map[string]string{
		"csrf_token": csrfFromPage(t, afterClose),
		"action":     "reopen",
	})
	if reopened.StatusCode != http.StatusSeeOther {
		t.Fatalf("reopening = %d, want 303\n%s", reopened.StatusCode, bodyOf(t, reopened))
	}
	_ = reopened.Body.Close()

	// **The history names both changes**, read from the audit ledger — which is what the parity line
	// means by "state-change history".
	_, history, _ := getText(t, world.base+issuePath)
	for _, action := range []string{"issue.created", "issue.closed", "issue.reopened"} {
		if !strings.Contains(history, action) {
			t.Errorf("the history does not name %q:\n%s", action, history)
		}
	}
	for _, transition := range []string{"open → closed", "closed → open"} {
		if !strings.Contains(history, transition) {
			t.Errorf("the history does not show %q:\n%s", transition, history)
		}
	}
}

// TestDeletingAnIssueFromThePage is the parity line "Delete an issue with audit".
func TestDeletingAnIssueFromThePage(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"To be deleted from the page","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating = %d\n%s", status, payload)
	}
	issuePath := "/alice/" + world.public.name + "/issues/1"

	_, page, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(page, `name="action" value="delete"`) {
		t.Fatalf("the owner is offered no delete control:\n%s", page)
	}

	response := world.postForm(t, issuePath, map[string]string{
		"csrf_token": csrfFromPage(t, page),
		"action":     "delete",
	})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("deleting = %d, want 303\n%s", response.StatusCode, bodyOf(t, response))
	}
	// The redirect goes to the list, because the issue no longer exists to return to.
	if location := response.Header.Get("Location"); location != "/alice/"+world.public.name+"/issues" {
		t.Errorf("Location = %q, want the issue list", location)
	}
	_ = response.Body.Close()

	// The issue is gone.
	status, _, _ := getText(t, world.base+issuePath)
	if status != http.StatusNotFound {
		t.Errorf("the deleted issue answers %d, want 404", status)
	}

	// **The deletion is audited**, which is what "with audit" requires. That half is asserted where
	// it can be observed: `TestADeletionKeepsItsRecord` in `internal/issues` reads the ledger for the
	// deleted issue's identifier and finds the `issue.deleted` record, with the title it removed,
	// still present after the row is gone. Asserting it here would mean a second reader of the same
	// ledger and would prove nothing further — the page's part of this line is that a reader can
	// delete an issue, which the assertions above cover.
}

// TestAFormWithoutAValidTokenIsRefusedEndToEnd covers the forgery guard on the real server.
//
// A cross-site submission carries the session cookie but cannot read the token — that is the whole
// point of putting it in the document — so it is refused before anything is read from the body.
func TestAFormWithoutAValidTokenIsRefusedEndToEnd(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"Untouched","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating = %d\n%s", status, payload)
	}
	issuePath := "/alice/" + world.public.name + "/issues/1"

	for _, test := range []struct {
		name   string
		fields map[string]string
		want   int
	}{
		{
			name:   "no token",
			fields: map[string]string{"action": "delete"},
			want:   http.StatusForbidden,
		},
		{
			name:   "a forged token",
			fields: map[string]string{"csrf_token": "forged", "action": "delete"},
			want:   http.StatusForbidden,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := world.postForm(t, issuePath, test.fields)
			if response.StatusCode != test.want {
				t.Errorf("status = %d, want %d", response.StatusCode, test.want)
			}
			_ = response.Body.Close()

			// The issue survived, which is the property that matters.
			status, _, _ := getText(t, world.base+issuePath)
			if status != http.StatusOK {
				t.Errorf("the issue is no longer there: %d", status)
			}
		})
	}
}

// TestAnAnonymousCallerIsOfferedNoIssueControls covers the read surface for a caller with no
// session: the issue is readable on a public repository, and no control is offered.
func TestAnAnonymousCallerIsOfferedNoIssueControls(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"Public and readable","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating = %d\n%s", status, payload)
	}

	// Read anonymously, with no cookie.
	_, page, _ := getText(t, world.base+"/alice/"+world.public.name+"/issues/1")
	if !strings.Contains(page, "Public and readable") {
		t.Error("a public issue is not readable anonymously")
	}
	for _, forbidden := range []string{`name="csrf_token"`, `name="action" value="edit"`, `name="action" value="delete"`} {
		if strings.Contains(page, forbidden) {
			t.Errorf("an anonymous reader is offered %q", forbidden)
		}
	}
	// And the creation link is absent, because an issue needs an author.
	_, list, _ := getText(t, world.base+"/alice/"+world.public.name+"/issues")
	if strings.Contains(list, `href="/alice/`+world.public.name+`/issues/new"`) {
		t.Error("an anonymous reader is offered the creation link")
	}
}
