package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
)

// bootPattern finds the shell's boot payload.
var bootPattern = regexp.MustCompile(`(?s)<script type="application/json" id="gitnest-boot">(.*?)</script>`)

// TestATokenIssuedByAPageLoadSubmits is the acceptance criterion at the strongest
// layer available: a token taken from a rendered page is accepted by a
// state-changing request, and taking it again from a fresh page load works too.
//
// It needs a built frontend, because the shell cannot render without one, and
// building the frontend is not possible in the development sandbox. It skips with
// that reason rather than passing on a weaker path — the token-submission half is
// covered unconditionally by TestAnIssuedTokenSubmitsThroughTheAPI, and the
// page-embedding half by the shell handler's own test.
func TestATokenIssuedByAPageLoadSubmits(t *testing.T) {
	schema := postgrestest.NewSchema(t)
	base := startInstance(t, schema)
	client := &http.Client{Timeout: 20 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	accounts := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})
	if _, err := accounts.CreateInitialAdmin(ctx, identity.CreateInitialAdminInput{
		Username: "alice", Email: "alice@example.com", Password: endToEndPassword,
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	session := signInForCookie(t, client, base)
	// A create needs no repository, so the whole flow exercises the token and
	// nothing else.
	post := func(token, name string) *http.Response {
		t.Helper()
		body, err := json.Marshal(map[string]any{"name": name})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		request, err := http.NewRequest(http.MethodPost, base+"/api/v1/user/repos", strings.NewReader(string(body)))
		if err != nil {
			t.Fatalf("build the request: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(session)
		if token != "" {
			request.Header.Set("X-CSRF-Token", token)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		return response
	}

	// A request with no token is refused, which keeps the control meaningful.
	refused := post("", "no-token")
	_ = refused.Body.Close()
	if refused.StatusCode != http.StatusForbidden {
		t.Errorf("a request with no token = %d, want 403", refused.StatusCode)
	}

	// A token taken from a page load is accepted.
	first := tokenFromPage(t, client, base, session)
	accepted := post(first, "from-a-page")
	_ = accepted.Body.Close()
	if accepted.StatusCode != http.StatusCreated {
		t.Fatalf("a request with a page-issued token = %d, want 201", accepted.StatusCode)
	}

	// Reloading the page issues a new token, and that one works as well: this is
	// what "a reload does not lose the ability to submit" means.
	second := tokenFromPage(t, client, base, session)
	if second == first {
		t.Error("a second page load issued the same token, so nothing was re-issued")
	}
	afterReload := post(second, "after-a-reload")
	_ = afterReload.Body.Close()
	if afterReload.StatusCode != http.StatusCreated {
		t.Fatalf("a request after a reload = %d, want 201", afterReload.StatusCode)
	}

	// The superseded token no longer works, which is the documented consequence of
	// keeping one digest per session.
	stale := post(first, "with-the-old-token")
	defer func() { _ = stale.Body.Close() }()
	if stale.StatusCode != http.StatusForbidden {
		t.Errorf("the superseded token = %d, want 403", stale.StatusCode)
	}
}

// tokenFromPage loads the shell and reads the token it issued.
func tokenFromPage(t *testing.T, client *http.Client, base string, session *http.Cookie) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, base+"/new", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.AddCookie(session)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET /new: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		// An unbuilt frontend is a legitimate state, and it means the shell cannot
		// issue a token at all. The test says so rather than failing on the build.
		t.Skipf("the interactive shell is not served (status %d); build the frontend to exercise this", response.StatusCode)
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	match := bootPattern.FindStringSubmatch(string(raw))
	if match == nil {
		t.Fatalf("the shell carries no boot payload:\n%s", raw)
	}
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(match[1]), &payload); err != nil {
		t.Fatalf("the boot payload is not JSON: %v", err)
	}
	token, _ := payload["csrf_token"].(string)
	if token == "" {
		t.Fatal("the shell issued no CSRF token for a signed-in reader")
	}
	return token
}

// signInForCookie signs in and returns the session cookie.
func signInForCookie(t *testing.T, client *http.Client, base string) *http.Cookie {
	t.Helper()
	body, err := json.Marshal(map[string]string{"login": "alice", "password": endToEndPassword})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	response, err := client.Post(base+"/api/v1/login", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("sign-in = %d", response.StatusCode)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "gitnest_session" {
			return cookie
		}
	}
	t.Fatal("the sign-in response carried no session cookie")
	return nil
}

// TestAnIssuedTokenSubmitsThroughTheAPI is the half of the acceptance criterion
// that does not depend on a built frontend: the token a page load receives is
// accepted by a state-changing request, a reload produces a new one that also
// works, and the superseded one is refused.
//
// The token is issued through the same call the shell makes, so the chain from
// "the server issues a token for a document" to "the API accepts it" is
// exercised even where the document itself cannot be rendered.
func TestAnIssuedTokenSubmitsThroughTheAPI(t *testing.T) {
	schema := postgrestest.NewSchema(t)
	base := startInstance(t, schema)
	client := &http.Client{Timeout: 20 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	accounts := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})
	if _, err := accounts.CreateInitialAdmin(ctx, identity.CreateInitialAdminInput{
		Username: "alice", Email: "alice@example.com", Password: endToEndPassword,
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	session := signInForCookie(t, client, base)
	sessionToken := session.Value

	post := func(token, name string) int {
		t.Helper()
		body, err := json.Marshal(map[string]any{"name": name})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		request, err := http.NewRequest(http.MethodPost, base+"/api/v1/user/repos", strings.NewReader(string(body)))
		if err != nil {
			t.Fatalf("build the request: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(session)
		if token != "" {
			request.Header.Set("X-CSRF-Token", token)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode
	}

	if got := post("", "no-token"); got != http.StatusForbidden {
		t.Errorf("a request with no token = %d, want 403", got)
	}

	first, err := accounts.RotateCSRFToken(ctx, sessionToken)
	if err != nil {
		t.Fatalf("issue a token the way a page load does: %v", err)
	}
	if got := post(first, "issued-once"); got != http.StatusCreated {
		t.Fatalf("a request with an issued token = %d, want 201", got)
	}

	// A reload issues again, and the new token works: this is what "a reload does
	// not lose the ability to submit" means.
	second, err := accounts.RotateCSRFToken(ctx, sessionToken)
	if err != nil {
		t.Fatalf("issue a second token: %v", err)
	}
	if second == first {
		t.Error("the second issue returned the same token, so nothing was re-issued")
	}
	if got := post(second, "issued-twice"); got != http.StatusCreated {
		t.Fatalf("a request after a reload = %d, want 201", got)
	}

	// The superseded token is refused, which is the documented consequence of one
	// digest per session.
	if got := post(first, "with-the-old-token"); got != http.StatusForbidden {
		t.Errorf("the superseded token = %d, want 403", got)
	}
}
