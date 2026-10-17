package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/transport/http/api"
)

const endToEndPassword = "correct horse battery staple"

// accountSession is the state a signed-in caller holds: the cookie to send and
// the CSRF token to accompany a state-changing request.
type accountSession struct {
	cookie string
	csrf   string
}

// runningInstance starts the full application against a freshly migrated schema
// and returns its base URL along with the captured log output.
//
// It exercises the real server, the real middleware chain, the real service,
// and a real database, so it is the strongest evidence the browser flow works.
func runningInstance(t *testing.T) (string, *slog.Logger, *bytes.Buffer) {
	t.Helper()
	schema := postgrestest.NewSchema(t)
	port := freePort(t)
	cfg := testConfig(t, schemaDSN(t, schema), port)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)

	logOutput := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })

	// The administrator is created through the real bootstrap path.
	if _, err := CreateInitialAdmin(ctx, cfg, identity.CreateInitialAdminInput{
		Username: "root", Email: "root@example.com", Password: endToEndPassword,
	}); err != nil {
		cancel()
		t.Fatalf("bootstrap the administrator: %v", err)
	}

	go func() { stopped <- New(cfg).Run(ctx) }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForStatus(t, base+"/api/ready", http.StatusOK, 20*time.Second)

	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(20 * time.Second):
			t.Error("the server did not stop during cleanup")
		}
	})
	return base, logger, logOutput
}

// postJSON sends a JSON request and returns the response together with the
// decoded body. It never retries: a retry would hide a throttling result.
func postJSON(t *testing.T, base, path, body string, session *accountSession, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	return sendJSON(t, http.MethodPost, base, path, body, session, headers)
}

func sendJSON(t *testing.T, method, base, path, body string, session *accountSession, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	reader = strings.NewReader(body)
	request, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if session != nil {
		request.Header.Set("Cookie", api.SessionCookie+"="+session.cookie)
		if session.csrf != "" {
			request.Header.Set(api.CSRFTokenHeader, session.csrf)
		}
	}

	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	decoded := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return response, decoded
}

// signIn signs a caller in and returns the cookie and CSRF token.
func signIn(t *testing.T, base, login, password string) (*accountSession, *http.Response, map[string]any) {
	t.Helper()
	response, body := postJSON(t, base, "/api/v1/login",
		fmt.Sprintf(`{"login":%q,"password":%q}`, login, password), nil, nil)

	session := &accountSession{}
	for _, cookie := range response.Cookies() {
		if cookie.Name == api.SessionCookie {
			session.cookie = cookie.Value
			// The flags the browser relies on must be present on the real
			// response, not only in a unit test.
			if !cookie.HttpOnly {
				t.Error("the session cookie is not HttpOnly")
			}
			if cookie.SameSite != http.SameSiteLaxMode {
				t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
			}
		}
	}
	if csrf, ok := body["csrf_token"].(string); ok {
		session.csrf = csrf
	}
	return session, response, body
}

// TestBrowserSessionLifecycleEndToEnd is the whole W1.4 acceptance path against
// a real instance: sign in, read the account, sign out, and confirm the session
// is gone and no credential reached a response or a log line.
func TestBrowserSessionLifecycleEndToEnd(t *testing.T) {
	base, _, logOutput := runningInstance(t)

	session, response, body := signIn(t, base, "root", endToEndPassword)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("sign-in status = %d, want 200", response.StatusCode)
	}
	if session.cookie == "" {
		t.Fatal("sign-in did not set a session cookie")
	}
	if session.csrf == "" {
		t.Fatal("sign-in did not return a CSRF token")
	}
	user, _ := body["user"].(map[string]any)
	if user["username"] != "root" {
		t.Errorf("signed in as %v, want root", user["username"])
	}

	// The session resolves on the next request.
	response, body = sendJSON(t, http.MethodGet, base, "/api/v1/me", "", session, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("me status = %d, want 200", response.StatusCode)
	}
	if body["email"] != "root@example.com" {
		t.Errorf("me returned %v, want the account", body)
	}

	// Anonymous access is refused.
	if response, _ := sendJSON(t, http.MethodGet, base, "/api/v1/me", "", nil, nil); response.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous me status = %d, want 401", response.StatusCode)
	}

	// Signing out without the CSRF token is refused before anything happens.
	if response, _ := postJSON(t, base, "/api/v1/logout", "", &accountSession{cookie: session.cookie}, nil); response.StatusCode != http.StatusForbidden {
		t.Errorf("logout without CSRF status = %d, want 403", response.StatusCode)
	}

	// With the token it succeeds, and the session stops resolving.
	nobody := &accountSession{cookie: session.cookie, csrf: session.csrf}
	if response, _ := postJSON(t, base, "/api/v1/logout", "", nobody, nil); response.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", response.StatusCode)
	}
	if response, _ := sendJSON(t, http.MethodGet, base, "/api/v1/me", "", nobody, nil); response.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after sign-out status = %d, want 401", response.StatusCode)
	}

	// No credential may appear in any log line.
	assertNoSecretsInLog(t, logOutput.String(), session)
}

// TestInvitedContributorEndToEnd covers the administrator inviting someone and
// that person creating an account and signing in.
func TestInvitedContributorEndToEnd(t *testing.T) {
	base, _, logOutput := runningInstance(t)

	admin, response, _ := signIn(t, base, "root", endToEndPassword)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("administrator sign-in status = %d", response.StatusCode)
	}

	// A non-administrator cannot invite: create one, then try.
	response, body := postJSON(t, base, "/api/v1/user/invitations", `{"email":"newcomer@example.com"}`, admin, nil)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("invitation status = %d, want 201", response.StatusCode)
	}
	invitationToken, _ := body["token"].(string)
	if invitationToken == "" {
		t.Fatal("the invitation response did not return a token")
	}

	// The invitee has no session at all.
	response, body = sendJSON(t, http.MethodPost, base, "/api/v1/invitations/accept",
		fmt.Sprintf(`{"username":"newcomer","password":%q}`, endToEndPassword), nil,
		map[string]string{api.InvitationTokenHeader: invitationToken})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("acceptance status = %d, want 201: %v", response.StatusCode, body)
	}
	if body["email"] != "newcomer@example.com" {
		t.Errorf("created %v, want the invitation's address", body["email"])
	}
	if body["is_admin"] != false {
		t.Error("the invited account is an administrator")
	}

	// The new account signs in.
	member, response, _ := signIn(t, base, "newcomer", endToEndPassword)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("invitee sign-in status = %d, want 200", response.StatusCode)
	}

	// A member may not invite, and the refusal is a 403 rather than a 500.
	response, _ = postJSON(t, base, "/api/v1/user/invitations", `{"email":"third@example.com"}`, member, nil)
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("member invitation status = %d, want 403", response.StatusCode)
	}

	// The invitation is single use.
	response, _ = sendJSON(t, http.MethodPost, base, "/api/v1/invitations/accept",
		fmt.Sprintf(`{"username":"second","password":%q}`, endToEndPassword), nil,
		map[string]string{api.InvitationTokenHeader: invitationToken})
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("replayed acceptance status = %d, want 401", response.StatusCode)
	}

	assertNoSecretsInLog(t, logOutput.String(), admin, member)
}

// TestThrottlingIsReportedWithoutDisclosingTheAccount covers the documented
// status for a throttled sign-in.
func TestThrottlingIsReportedWithoutDisclosingTheAccount(t *testing.T) {
	base, _, _ := runningInstance(t)

	// The limit is five failures per source per window.
	var last *http.Response
	var lastBody map[string]any
	for attempt := 0; attempt < 6; attempt++ {
		last, lastBody = postJSON(t, base, "/api/v1/login", `{"login":"ghost","password":"wrong password"}`, nil, nil)
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status after the limit = %d, want 429", last.StatusCode)
	}
	message, _ := lastBody["message"].(string)
	if strings.Contains(strings.ToLower(message), "ghost") || strings.Contains(strings.ToLower(message), "exist") {
		t.Errorf("the throttled message discloses the account: %q", message)
	}
}

// assertNoSecretsInLog fails if any credential material reached the log.
func assertNoSecretsInLog(t *testing.T, logs string, sessions ...*accountSession) {
	t.Helper()
	forbidden := []string{endToEndPassword, "argon2", "password_hash"}
	for _, session := range sessions {
		if session == nil {
			continue
		}
		if session.cookie != "" {
			forbidden = append(forbidden, session.cookie)
		}
		if session.csrf != "" {
			forbidden = append(forbidden, session.csrf)
		}
	}
	for _, secret := range forbidden {
		if strings.Contains(logs, secret) {
			t.Errorf("a log line contains %q", secret)
		}
	}
}
