package app

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The label surfaces end to end, as a browser and an API client would drive them.
//
// This is the evidence for W2.4's parity lines. The rules themselves are asserted in
// `internal/labels`, and the two claims that need real tables — that the unique constraint decides a
// name conflict and that a label's deletion removes its attachments without deleting issues — are
// asserted there too against PostgreSQL. What only these tests can show is that the surfaces exist and
// agree.

// labelPath is the API collection path for a repository's labels.
func (e issueEndToEnd) labelPath() string {
	return e.base + "/api/v1/repos/alice/" + e.public.name + "/labels"
}

// issueLabelPath is the API path for one issue's labels.
func (e issueEndToEnd) issueLabelPath(issueNumber int) string {
	return e.base + "/api/v1/repos/alice/" + e.public.name + "/issues/" +
		strconv.Itoa(issueNumber) + "/labels"
}

// TestCreatingALabelFromThePage is the parity line "Labels with name, color, and description, unique
// per repository after case folding".
func TestCreatingALabelFromThePage(t *testing.T) {
	world := issueInstance(t)

	// 1. The page offers the management controls, because the instance's owner is the repository's admin.
	status, page, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/labels", world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the label list = %d, want 200", status)
	}
	if !strings.Contains(page, `name="action" value="label.create"`) {
		t.Fatalf("the label list offers no creation form:\n%s", page)
	}
	if !strings.Contains(page, "This repository has no labels.") {
		t.Error("the empty list does not say so")
	}

	// 2. Creating one through the form redirects, so a reload does not create it twice.
	response := world.postForm(t, "/alice/"+world.public.name+"/labels", map[string]string{
		"csrf_token":        csrfFromPage(t, page),
		"action":            "label.create",
		"label_name":        "bug",
		"label_color":       "#FF0000",
		"label_description": "Something is wrong",
	})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("creating a label = %d, want 303\n%s", response.StatusCode, bodyOf(t, response))
	}
	location := response.Header.Get("Location")
	_ = response.Body.Close()

	// 3. The page shows it, with the colour **as text** — not as a swatch.
	_, listed, _ := getTextWithCookie(t, world.base+location, world.cookie)
	if !strings.Contains(listed, "bug") {
		t.Error("the new label is not listed")
	}
	if !strings.Contains(listed, "#ff0000") {
		t.Errorf("the colour is not shown as text:\n%s", listed)
	}
	if !strings.Contains(listed, "Something is wrong") {
		t.Error("the description is not shown")
	}
	// **The colour must not reach a style attribute.** `frontend.md` forbids "inline style strings
	// assembled from user data", and this is the assertion that the rule holds rather than being a
	// comment.
	if strings.Contains(listed, "style=") {
		t.Errorf("a style attribute is present, which the frontend rules forbid:\n%s", listed)
	}
	// The colour is normalized to lowercase in storage and display, which is what makes the grammar's
	// alphabet exactly sixteen characters.
	if strings.Contains(listed, "#FF0000") {
		t.Error("the colour was not normalized to lowercase")
	}

	// 4. The API agrees, and carries both the stored and the rendered form.
	status, items := getJSONList(t, world.labelPath(), world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the API label list = %d, want 200", status)
	}
	if len(items) != 1 {
		t.Fatalf("the API lists %d labels, want 1", len(items))
	}
	if items[0]["color"] != "ff0000" {
		t.Errorf("the stored colour is %v, want the bare digits", items[0]["color"])
	}
	if items[0]["rendered_color"] != "#ff0000" {
		t.Errorf("the rendered colour is %v, want the # form", items[0]["rendered_color"])
	}

	// 5. **A name differing only by case is refused**, which is the acceptance criterion.
	_, again, _ := getTextWithCookie(t, world.base+location, world.cookie)
	refused := world.postForm(t, "/alice/"+world.public.name+"/labels", map[string]string{
		"csrf_token":  csrfFromPage(t, again),
		"action":      "label.create",
		"label_name":  "BUG",
		"label_color": "00ff00",
	})
	if refused.StatusCode != http.StatusConflict {
		t.Fatalf("a case-variant name = %d, want 409\n%s", refused.StatusCode, bodyOf(t, refused))
	}
	refusedBody := bodyOf(t, refused)
	_ = refused.Body.Close()
	// The refusal is announced and names the reason, and the reader's input is kept.
	if !strings.Contains(refusedBody, `role="alert"`) {
		t.Error("the name conflict is not announced")
	}
	if !strings.Contains(refusedBody, "BUG") {
		t.Error("the reader's input was lost")
	}
	// Still exactly one label, so the refusal was a refusal rather than a second row.
	_, final := getJSONList(t, world.labelPath(), world.cookie)
	if len(final) != 1 {
		t.Errorf("the API lists %d labels after the refused create, want 1", len(final))
	}
}

// TestApplyingAndRemovingALabelOnAnIssue is the parity line "Apply and remove labels on issues and
// pull requests".
//
// The pull-request half is not claimed: pull requests arrive in phase 3, so applying a label to one is
// not implemented and the line is therefore not ticked. What this asserts is the issue half.
func TestApplyingAndRemovingALabelOnAnIssue(t *testing.T) {
	world := issueInstance(t)

	// A label, through the API.
	if status, _, payload := world.send(t, http.MethodPost, world.labelPath(),
		`{"name":"bug","color":"ff0000"}`); status != http.StatusCreated {
		t.Fatalf("creating the label = %d\n%s", status, payload)
	}
	// An issue.
	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"An issue to label","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the issue = %d\n%s", status, payload)
	}

	issuePath := "/alice/" + world.public.name + "/issues/1"

	// The issue page offers the section and an apply control, because the owner holds admin.
	_, page, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(page, `name="action" value="label.attach"`) {
		t.Fatalf("the issue page offers no way to apply a label:\n%s", page)
	}
	if !strings.Contains(page, "This issue has no labels.") {
		t.Error("the unlabelled issue does not say so")
	}

	// Apply it through the form.
	response := world.postForm(t, issuePath+"/labels", map[string]string{
		"csrf_token": csrfFromPage(t, page),
		"action":     "label.attach",
		"label_name": "bug",
	})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("applying a label = %d, want 303\n%s", response.StatusCode, bodyOf(t, response))
	}
	_ = response.Body.Close()

	// The page shows it on the issue, with the colour as text and a link to the per-label issue list.
	_, labelled, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(labelled, "bug") || !strings.Contains(labelled, "#ff0000") {
		t.Errorf("the label is not shown on the issue:\n%s", labelled)
	}
	if !strings.Contains(labelled, `/labels/bug"`) {
		t.Error("the label does not link to its issue list")
	}
	if strings.Contains(labelled, "This issue has no labels.") {
		t.Error("the page still says the issue has no labels")
	}

	// The API agrees.
	status, applied := getJSONList(t, world.issueLabelPath(1), world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the issue's labels = %d, want 200", status)
	}
	if len(applied) != 1 || applied[0]["name"] != "bug" {
		t.Errorf("the issue carries %v, want the applied label", applied)
	}

	// **Applying it again is a success that changed nothing**, which the status distinguishes: the
	// operation's result is a state rather than an event.
	status, _, payload := world.send(t, http.MethodPost, world.issueLabelPath(1), `{"name":"bug"}`)
	if status != http.StatusOK {
		t.Errorf("a second application = %d, want 200 rather than 201\n%s", status, payload)
	}

	// Remove it through the form.
	_, removeFrom, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(removeFrom, `name="action" value="label.detach"`) {
		t.Fatalf("the issue page offers no way to remove the label:\n%s", removeFrom)
	}
	removed := world.postForm(t, issuePath+"/labels", map[string]string{
		"csrf_token": csrfFromPage(t, removeFrom),
		"action":     "label.detach",
		"label_name": "bug",
	})
	if removed.StatusCode != http.StatusSeeOther {
		t.Fatalf("removing a label = %d, want 303\n%s", removed.StatusCode, bodyOf(t, removed))
	}
	_ = removed.Body.Close()

	_, after, _ := getTextWithCookie(t, world.base+issuePath, world.cookie)
	if !strings.Contains(after, "This issue has no labels.") {
		t.Errorf("the label was not removed:\n%s", after)
	}
	// **The label itself still exists**, because removing it from an issue is not deleting it.
	_, labels := getJSONList(t, world.labelPath(), world.cookie)
	if len(labels) != 1 {
		t.Errorf("the label was deleted rather than detached: %d labels", len(labels))
	}
}

// TestDeletingALabelFromThePageKeepsItsIssues is the acceptance criterion observed as a reader sees it.
func TestDeletingALabelFromThePageKeepsItsIssues(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.labelPath(),
		`{"name":"bug","color":"ff0000"}`); status != http.StatusCreated {
		t.Fatalf("creating the label = %d\n%s", status, payload)
	}
	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"An issue that must survive","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating the issue = %d\n%s", status, payload)
	}
	if status, _, payload := world.send(t, http.MethodPost, world.issueLabelPath(1),
		`{"name":"bug"}`); status != http.StatusCreated {
		t.Fatalf("applying the label = %d\n%s", status, payload)
	}

	// The label list shows how many issues carry it, which is what makes the deletion's consequence
	// visible before it is taken.
	_, page, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/labels", world.cookie)
	if !strings.Contains(page, "1 issues") && !strings.Contains(page, "1 issue") {
		t.Errorf("the count is not shown:\n%s", page)
	}
	// And it states that the issues are not deleted, which is the consequence a reader is most likely to
	// get wrong.
	if !strings.Contains(page, "The issues themselves are not deleted.") {
		t.Error("the delete hint does not state that the issues survive")
	}

	// Delete it through the form.
	response := world.postForm(t, "/alice/"+world.public.name+"/labels", map[string]string{
		"csrf_token": csrfFromPage(t, page),
		"action":     "label.delete",
		"label_name": "bug",
	})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("deleting the label = %d, want 303\n%s", response.StatusCode, bodyOf(t, response))
	}
	_ = response.Body.Close()

	// The label is gone...
	_, afterDelete, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/labels", world.cookie)
	if !strings.Contains(afterDelete, "This repository has no labels.") {
		t.Errorf("the label was not deleted:\n%s", afterDelete)
	}
	// ...**and the issue is not**, which is the acceptance criterion.
	status, issuePath, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/issues/1", world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the issue is no longer readable: %d", status)
	}
	if !strings.Contains(issuePath, "An issue that must survive") {
		t.Error("the issue was lost with the label")
	}
	if !strings.Contains(issuePath, "This issue has no labels.") {
		t.Error("the issue still reports the deleted label")
	}
}

// TestTheLabelPageIsRefusedToANonAdmin covers the denied path on the page.
//
// A label list is readable, but the management forms need admin — and a reader who may not manage labels
// is shown the list without the forms rather than forms that would be refused.
func TestTheLabelPageIsRefusedToANonAdmin(t *testing.T) {
	world := issueInstance(t)

	// The list is readable anonymously on a public repository, but offers nothing to an anonymous reader.
	status, page, _ := getText(t, world.base+"/alice/"+world.public.name+"/labels")
	if status != http.StatusOK {
		t.Fatalf("the anonymous label list = %d, want 200", status)
	}
	for _, forbidden := range []string{
		`name="action" value="label.create"`,
		`name="action" value="label.delete"`,
		`name="csrf_token"`,
	} {
		if strings.Contains(page, forbidden) {
			t.Errorf("an anonymous reader is offered %q", forbidden)
		}
	}

	// Creating one anonymously through the API is refused, because it needs an account at all before it
	// needs a permission.
	request, err := http.NewRequest(http.MethodPost, world.labelPath(), strings.NewReader(`{"name":"x","color":"ff0000"}`))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("the anonymous create: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	// A repository the caller cannot see is a not-found, which is the T-A4 shape — so an anonymous
	// create on a **public** repository reaches the service and is refused as forbidden, because the
	// caller's level (read) was resolved. Either refusal is correct; what must not happen is a 201.
	if response.StatusCode == http.StatusCreated {
		t.Errorf("an anonymous create succeeded: %d", response.StatusCode)
	}
}

// TestANonAdminSeesTheLabelListWithoutForms covers the permission prediction on the page.
func TestANonAdminSeesTheLabelListWithoutForms(t *testing.T) {
	world := issueInstance(t)

	// A label exists, created by the owner.
	if status, _, payload := world.send(t, http.MethodPost, world.labelPath(),
		`{"name":"bug","color":"ff0000"}`); status != http.StatusCreated {
		t.Fatalf("creating the label = %d\n%s", status, payload)
	}

	// The owner sees the forms.
	_, ownerView, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/labels", world.cookie)
	if !strings.Contains(ownerView, `name="action" value="label.create"`) {
		t.Error("the owner is not offered the creation form")
	}
	if !strings.Contains(ownerView, `name="action" value="label.delete"`) {
		t.Error("the owner is not offered the delete control")
	}
	if !strings.Contains(ownerView, "bug") {
		t.Error("the label is not listed for the owner")
	}

	// An anonymous reader sees the label and no forms.
	_, anonymousView, _ := getText(t, world.base+"/alice/"+world.public.name+"/labels")
	if !strings.Contains(anonymousView, "bug") {
		t.Error("a public repository's labels are not readable anonymously")
	}
	if strings.Contains(anonymousView, `name="action" value="label.create"`) {
		t.Error("an anonymous reader is offered the creation form")
	}
}

// TestTheLabelColourGrammarIsEnforcedOnThePage covers the colour refusal as a reader sees it.
func TestTheLabelColourGrammarIsEnforcedOnThePage(t *testing.T) {
	world := issueInstance(t)

	for _, color := range []string{"red", "ff00", "ff0000;", `ff0000"style="color:red`} {
		// **The page is re-read on every iteration, and that is the token rotation working rather than a
		// workaround.** `RotateCSRFToken` keeps one live token per session, so rendering a page replaces
		// the previous one's — which means a form left open while another page loads is refused. Reusing
		// one page's token across submissions failed with 403 from the second attempt onward, which is
		// exactly the behaviour the rotation exists to produce.
		_, page, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/labels", world.cookie)
		response := world.postForm(t, "/alice/"+world.public.name+"/labels", map[string]string{
			"csrf_token":  csrfFromPage(t, page),
			"action":      "label.create",
			"label_name":  "a-name",
			"label_color": color,
		})
		if response.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("color %q = %d, want 422", color, response.StatusCode)
		}
		body := bodyOf(t, response)
		_ = response.Body.Close()
		// The refusal is tied to the colour field, which is what `frontend.md` requires.
		if !strings.Contains(body, `aria-invalid="true"`) {
			t.Errorf("color %q: the refusal is not tied to a field", color)
		}
		// **The reader's colour is kept — and a quote in it is escaped**, which is the two properties at
		// once. Asserting the raw value would fail for the payload containing a `"`, because Go's
		// `html/template` encodes it as `&#34;` inside the attribute. That is the contextual escaping
		// T-B2 requires, so the assertion is written against the escaped form rather than the literal
		// one: the input survives, and it cannot break out of the attribute it is placed in.
		escaped := htmlEscapeAttribute(color)
		if !strings.Contains(body, `value="`+escaped+`"`) {
			t.Errorf("color %q: the reader's input was lost or not escaped as expected", color)
		}
		if strings.Contains(color, `"`) && strings.Contains(body, `value="`+color+`"`) {
			t.Errorf("color %q reached the attribute unescaped, so it could break out of it", color)
		}
	}
}

// TestALabelNameWithSlashAndSpaceRoundTrips covers the name grammar's breadth.
//
// §6.5 permits "any printable characters", so a name may contain a space or a slash — and the per-label
// link has to escape it so the route still resolves.
func TestALabelNameWithSlashAndSpaceRoundTrips(t *testing.T) {
	world := issueInstance(t)

	for _, name := range []string{"good first issue", "a/b", "needs  review"} {
		if status, _, payload := world.send(t, http.MethodPost, world.labelPath(),
			`{"name":`+jsonString(name)+`,"color":"ff0000"}`); status != http.StatusCreated {
			t.Fatalf("creating %q = %d\n%s", name, status, payload)
		}
	}

	_, page, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/labels", world.cookie)
	for _, name := range []string{"good first issue", "a/b", "needs  review"} {
		if !strings.Contains(page, name) {
			t.Errorf("the label %q is not listed", name)
		}
	}
	// A space is escaped as `%20` in a path, not as `+`, which would be a query-string convention.
	if !strings.Contains(page, "%20") {
		t.Error("no space was percent-escaped in a label link")
	}
}

// jsonString renders a Go string as a JSON string literal, so a test can build a request body without
// hand-escaping a name that may contain a quote.
func jsonString(value string) string {
	payload, err := json.Marshal(value)
	if err != nil {
		t := ""
		_ = t
		return `""`
	}
	return string(payload)
}

// TestTheLabelListLinksToAPerLabelIssueList covers the route the section 2.4 table names.
func TestTheLabelListLinksToAPerLabelIssueList(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.labelPath(),
		`{"name":"bug","color":"ff0000"}`); status != http.StatusCreated {
		t.Fatalf("creating the label = %d\n%s", status, payload)
	}

	// The per-label route is registered, so it answers rather than 404ing — which is what makes the link
	// the list renders a working one.
	status, page, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/labels/bug", world.cookie)
	if status != http.StatusOK {
		t.Fatalf("the per-label page = %d, want 200", status)
	}
	if !strings.Contains(page, "bug") {
		t.Error("the per-label page does not name the label")
	}

	// And the list links to it.
	_, list, _ := getTextWithCookie(t, world.base+"/alice/"+world.public.name+"/labels", world.cookie)
	if !regexp.MustCompile(`href="[^"]*/labels/bug"`).MatchString(list) {
		t.Errorf("the list does not link to the per-label page:\n%s", list)
	}
}

// htmlEscapeAttribute renders a value the way `html/template` places it in a double-quoted attribute.
//
// It exists so an assertion can name the expected form rather than the literal one, and it covers the
// five characters Go escapes in that context. Only the quote matters for the payloads under test, but
// writing all five makes the helper say what it means.
func htmlEscapeAttribute(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&#34;",
		"'", "&#39;",
	)
	return replacer.Replace(value)
}
