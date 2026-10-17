package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The issue surface end to end.
//
// This is the wiring test: it drives a real server, creates an issue through the versioned API,
// reads it back through the rendered page, and checks that the two surfaces answer the same
// access question. The rules themselves are asserted in `internal/issues`, and the HTTP contract
// in `internal/transport/http/api`; what only this test can show is that the pieces are connected
// to each other.

// issueEndToEnd is the parts of a running instance these tests need.
type issueEndToEnd struct {
	base   string
	public gitRepository
	cookie string
}

// issueInstance boots an instance and signs in, because opening an issue needs an account.
func issueInstance(t *testing.T) issueEndToEnd {
	t.Helper()
	base, public, _, _ := renderedInstance(t)

	body, err := json.Marshal(map[string]string{"login": "alice", "password": endToEndPassword})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	signIn, err := http.Post(base+"/api/v1/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	defer func() { _ = signIn.Body.Close() }()
	if signIn.StatusCode != http.StatusOK {
		t.Fatalf("sign-in status = %d, want 200", signIn.StatusCode)
	}
	var cookie string
	for _, candidate := range signIn.Cookies() {
		if candidate.Name == "gitnest_session" {
			cookie = candidate.Name + "=" + candidate.Value
		}
	}
	if cookie == "" {
		t.Fatal("sign-in set no session cookie")
	}
	return issueEndToEnd{base: base, public: public, cookie: cookie}
}

// issuesPath is the API collection path for the public repository.
func (e issueEndToEnd) issuesPath() string {
	return e.base + "/api/v1/repos/alice/" + e.public.name + "/issues"
}

// send dispatches a request with the session cookie.
func (e issueEndToEnd) send(t *testing.T, method, url, body string) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Cookie", e.cookie)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	return response.StatusCode, response.Header, payload
}

// TestAnIssueTravelsFromTheAPIThroughThePage is the wiring assertion.
//
// It opens an issue with the API, reads it from the page, and checks that the page shows what the
// API stored — including the Markdown rendering, which is the join between this workstream and
// W2.1's renderer.
func TestAnIssueTravelsFromTheAPIThroughThePage(t *testing.T) {
	world := issueInstance(t)

	status, headers, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"An issue about the docs","body":"A **bold** claim and a `+"`code span`"+`."}`)
	if status != http.StatusCreated {
		t.Fatalf("creating an issue = %d, want 201\n%s", status, payload)
	}
	// The Location names the number, which is the identity a client uses from here on.
	location := headers.Get("Location")
	if !strings.HasSuffix(location, "/issues/1") {
		t.Errorf("Location = %q, want it to end with /issues/1", location)
	}

	// The list page names it.
	listStatus, listBody, _ := getText(t, world.base+"/alice/"+world.public.name+"/issues")
	if listStatus != http.StatusOK {
		t.Fatalf("the issue list = %d, want 200", listStatus)
	}
	if !strings.Contains(listBody, "An issue about the docs") {
		t.Errorf("the list does not show the issue:\n%s", listBody)
	}
	if !strings.Contains(listBody, "/issues/1") {
		t.Error("the list does not link to the issue")
	}

	// The detail page renders the body as Markdown, not as its source.
	detailStatus, detailBody, _ := getText(t, world.base+"/alice/"+world.public.name+"/issues/1")
	if detailStatus != http.StatusOK {
		t.Fatalf("the issue page = %d, want 200", detailStatus)
	}
	if !strings.Contains(detailBody, "An issue about the docs") {
		t.Error("the page does not show the title")
	}
	if !strings.Contains(detailBody, "<strong>bold</strong>") {
		t.Errorf("the body was not rendered as Markdown:\n%s", detailBody)
	}
	if !strings.Contains(detailBody, "<code>code span</code>") {
		t.Error("the code span in the body was not rendered")
	}
	// The literal Markdown must be gone, which is what distinguishes rendering from echoing.
	if strings.Contains(detailBody, "A **bold** claim") {
		t.Error("the page shows the raw Markdown source")
	}
}

// TestClosingAnIssueRecordsAHistoryThePageShows is the join between the write path, the ledger,
// and the read surface.
//
// It is the acceptance criterion "closed and reopened issues keep a state-change history" observed
// the way a reader sees it: close, reopen, then read the page and find both changes named.
func TestClosingAnIssueRecordsAHistoryThePageShows(t *testing.T) {
	world := issueInstance(t)

	if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"A lifecycle","body":"body"}`); status != http.StatusCreated {
		t.Fatalf("creating an issue = %d\n%s", status, payload)
	}

	// Close it through the API.
	one := world.issuesPath() + "/1"
	status, _, payload := world.send(t, http.MethodPatch, one, `{"state":"closed"}`)
	if status != http.StatusOK {
		t.Fatalf("closing = %d, want 200\n%s", status, payload)
	}
	// Reopen it.
	status, _, payload = world.send(t, http.MethodPatch, one, `{"state":"open"}`)
	if status != http.StatusOK {
		t.Fatalf("reopening = %d, want 200\n%s", status, payload)
	}

	// The page shows both transitions, read from the ledger.
	pageStatus, page, _ := getText(t, world.base+"/alice/"+world.public.name+"/issues/1")
	if pageStatus != http.StatusOK {
		t.Fatalf("the issue page = %d, want 200", pageStatus)
	}
	for _, action := range []string{"issue.created", "issue.closed", "issue.reopened"} {
		if !strings.Contains(page, action) {
			t.Errorf("the history does not name %q:\n%s", action, page)
		}
	}
	// The transition itself is readable rather than left as an event name a reader must decode.
	for _, transition := range []string{"open → closed", "closed → open"} {
		if !strings.Contains(page, transition) {
			t.Errorf("the history does not show the transition %q:\n%s", transition, page)
		}
	}
}

// TestTheIssueSurfacesAgreeAboutRefusal is the architectural rule observed end to end: the API
// and the page must not answer an access question differently.
func TestTheIssueSurfacesAgreeAboutRefusal(t *testing.T) {
	world := issueInstance(t)

	// The private repository, which an anonymous caller may not read.
	_, _, private, _ := renderedInstance(t)

	for _, test := range []struct {
		name string
		repo string
	}{
		{name: "public", repo: world.public.name},
		{name: "private", repo: private.name},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Asked anonymously, through both surfaces.
			apiRequest, err := http.NewRequest(http.MethodGet,
				world.base+"/api/v1/repos/alice/"+test.repo+"/issues", nil)
			if err != nil {
				t.Fatalf("build the API request: %v", err)
			}
			// An anonymous API request, built from the URL rather than through a recorded request,
			// because a client refuses to send one carrying a RequestURI.
			response, err := http.DefaultClient.Do(apiRequest)
			if err != nil {
				t.Fatalf("the API request: %v", err)
			}
			apiStatus := response.StatusCode
			_ = response.Body.Close()

			pageStatus, _, _ := getText(t, world.base+"/alice/"+test.repo+"/issues")

			if apiStatus != pageStatus {
				t.Errorf("the API answered %d and the page answered %d", apiStatus, pageStatus)
			}
		})
	}
}

// TestAnIssueNumberIsNeverReusedAcrossADeletion covers the numbering rule as a client observes it.
func TestAnIssueNumberIsNeverReusedAcrossADeletion(t *testing.T) {
	world := issueInstance(t)

	for i := 0; i < 2; i++ {
		if status, _, payload := world.send(t, http.MethodPost, world.issuesPath(),
			`{"title":"an issue","body":""}`); status != http.StatusCreated {
			t.Fatalf("creating issue %d = %d\n%s", i+1, status, payload)
		}
	}

	// Delete the first, which requires write and the owner holds it.
	if status, _, payload := world.send(t, http.MethodDelete, world.issuesPath()+"/1", ""); status != http.StatusNoContent {
		t.Fatalf("deleting = %d, want 204\n%s", status, payload)
	}

	// The next issue takes number 3, not the freed 1.
	status, headers, payload := world.send(t, http.MethodPost, world.issuesPath(),
		`{"title":"after the deletion","body":""}`)
	if status != http.StatusCreated {
		t.Fatalf("creating after a deletion = %d\n%s", status, payload)
	}
	if location := headers.Get("Location"); !strings.HasSuffix(location, "/issues/3") {
		t.Errorf("Location = %q, want /issues/3 — number 1 must not be reused", location)
	}
}
