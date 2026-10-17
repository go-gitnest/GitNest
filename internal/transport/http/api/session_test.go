package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/transport/http/api"
)

const (
	password    = "correct horse battery staple"
	httpsURL    = "https://gitnest.example.com"
	loopbackURL = "http://127.0.0.1:8080"
)

// fakeAccounts is a scripted AccountService. Each field lets a test force one
// outcome without standing up a database, so every branch of the transport is
// reachable.
type fakeAccounts struct {
	login   func(ctx context.Context, input identity.LoginInput) (identity.Credentials, error)
	resolve func(ctx context.Context, token string) (identity.User, identity.Session, []byte, error)
	logout  func(ctx context.Context, token string) error
	invite  func(ctx context.Context, input identity.CreateInvitationInput) (identity.Invitation, string, error)
	accept  func(ctx context.Context, input identity.AcceptInvitationInput) (identity.User, error)

	loginInput  identity.LoginInput
	inviteInput identity.CreateInvitationInput
	acceptInput identity.AcceptInvitationInput
	logoutToken string
}

func (f *fakeAccounts) Login(ctx context.Context, input identity.LoginInput) (identity.Credentials, error) {
	f.loginInput = input
	if f.login == nil {
		return identity.Credentials{}, identity.ErrInvalidCredentials
	}
	return f.login(ctx, input)
}

func (f *fakeAccounts) ResolveSession(ctx context.Context, token string) (identity.User, identity.Session, []byte, error) {
	if f.resolve == nil {
		return identity.User{}, identity.Session{}, nil, identity.ErrInvalidToken
	}
	return f.resolve(ctx, token)
}

func (f *fakeAccounts) Logout(ctx context.Context, token string) error {
	f.logoutToken = token
	if f.logout == nil {
		return nil
	}
	return f.logout(ctx, token)
}

func (f *fakeAccounts) CreateInvitation(ctx context.Context, input identity.CreateInvitationInput) (identity.Invitation, string, error) {
	f.inviteInput = input
	if f.invite == nil {
		return identity.Invitation{}, "", identity.ErrNotAdmin
	}
	return f.invite(ctx, input)
}

func (f *fakeAccounts) AcceptInvitation(ctx context.Context, input identity.AcceptInvitationInput) (identity.User, error) {
	f.acceptInput = input
	if f.accept == nil {
		return identity.User{}, identity.ErrInvalidToken
	}
	return f.accept(ctx, input)
}

// newServer mounts the account routes with the given service and public URL.
func newServer(t *testing.T, accounts api.AccountService, publicURL string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api.RegisterAccounts(mux, accounts, publicURL)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// do sends a request with an optional CSRF token and cookie.
func do(t *testing.T, server *httptest.Server, method, path, body, csrf, cookie string) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	request, err := http.NewRequest(method, server.URL+path, reader)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		request.Header.Set(api.CSRFTokenHeader, csrf)
	}
	if cookie != "" {
		request.AddCookie(&http.Cookie{Name: api.SessionCookie, Value: cookie})
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func decodeBody(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode the response body: %v", err)
	}
	return body
}

// session returns a live-session stub for a signed-in account.
func session(csrfToken string) (identity.User, identity.Session, []byte) {
	user := identity.User{
		ID: "01920000-0000-7000-8000-000000000001", Username: "root", Email: "root@example.com",
		Status: identity.StatusActive, IsAdmin: true, CreatedAt: time.Unix(0, 0).UTC(),
	}
	row := identity.Session{
		ID: "01920000-0000-7000-8000-000000000002", UserID: user.ID,
		CreatedAt: time.Unix(0, 0).UTC(), ExpiresAt: time.Now().Add(time.Hour),
	}
	return user, row, identity.HashToken(csrfToken)
}

func TestLoginSetsASessionCookie(t *testing.T) {
	const csrfToken = "csrf-token-value"
	user, row, _ := session(csrfToken)
	accounts := &fakeAccounts{
		login: func(context.Context, identity.LoginInput) (identity.Credentials, error) {
			return identity.Credentials{
				User: user, Session: row, SessionToken: "session-token-value", CSRFToken: csrfToken,
			}, nil
		},
	}
	server := newServer(t, accounts, httpsURL)

	response := do(t, server, http.MethodPost, "/api/v1/login",
		`{"login":"root","password":"`+password+`"}`, "", "")

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.StatusCode, readAll(t, response))
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("set %d cookies, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != api.SessionCookie {
		t.Errorf("cookie name = %q, want %q", cookie.Name, api.SessionCookie)
	}
	if cookie.Value != "session-token-value" {
		t.Errorf("cookie value = %q, want the issued token", cookie.Value)
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	if !cookie.Secure {
		t.Error("the session cookie is not Secure on an https instance")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("cookie path = %q, want /", cookie.Path)
	}

	body := decodeBody(t, response)
	if body["csrf_token"] != csrfToken {
		t.Errorf("csrf_token = %v, want the issued token in the body", body["csrf_token"])
	}
	// The password hash must never appear, and neither must the internal type.
	raw := readAll(t, response)
	for _, forbidden := range []string{"argon2", "$argon2id$", "password_hash"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("the response body contains %q", forbidden)
		}
	}
}

// TestLoginOmitsSecureOnALoopbackInstance documents the deliberate exception:
// a browser drops a Secure cookie that arrives over http, which would break the
// documented development setup. The public URL validator refuses http for any
// non-loopback host, so this cannot reach a deployment.
func TestLoginOmitsSecureOnALoopbackInstance(t *testing.T) {
	user, row, _ := session("csrf")
	accounts := &fakeAccounts{
		login: func(context.Context, identity.LoginInput) (identity.Credentials, error) {
			return identity.Credentials{User: user, Session: row, SessionToken: "token", CSRFToken: "csrf"}, nil
		},
	}
	server := newServer(t, accounts, loopbackURL)

	response := do(t, server, http.MethodPost, "/api/v1/login", `{"login":"root","password":"x"}`, "", "")
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("set %d cookies, want 1", len(cookies))
	}
	if cookies[0].Secure {
		t.Error("the cookie is Secure on a loopback http instance, where the browser would drop it")
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Error("HttpOnly and SameSite=Lax must hold on every instance")
	}
}

// TestLoginRejectsBadCredentialsGenerically pins that the response does not
// disclose whether the account exists.
func TestLoginRejectsBadCredentialsGenerically(t *testing.T) {
	accounts := &fakeAccounts{
		login: func(context.Context, identity.LoginInput) (identity.Credentials, error) {
			return identity.Credentials{}, identity.ErrInvalidCredentials
		},
	}
	server := newServer(t, accounts, httpsURL)

	response := do(t, server, http.MethodPost, "/api/v1/login", `{"login":"root","password":"wrong"}`, "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
	body := decodeBody(t, response)
	if body["message"] != "invalid credentials" {
		t.Errorf("message = %v, want the generic message", body["message"])
	}
	if len(response.Cookies()) != 0 {
		t.Error("a failed sign-in set a cookie")
	}
}

func TestLoginReportsThrottling(t *testing.T) {
	accounts := &fakeAccounts{
		login: func(context.Context, identity.LoginInput) (identity.Credentials, error) {
			return identity.Credentials{}, identity.ErrRateLimited
		},
	}
	server := newServer(t, accounts, httpsURL)

	response := do(t, server, http.MethodPost, "/api/v1/login", `{"login":"root","password":"wrong"}`, "", "")
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.StatusCode)
	}
}

// TestLoginRejectsAnUnexpectedField covers the privilege-escalation shape: a
// body naming a field the endpoint does not accept is refused, not ignored, and
// the refusal names the field so the caller can find the mistake.
func TestLoginRejectsAnUnexpectedField(t *testing.T) {
	accounts := &fakeAccounts{}
	server := newServer(t, accounts, httpsURL)

	response := do(t, server, http.MethodPost, "/api/v1/login",
		`{"login":"root","password":"x","is_admin":true}`, "", "")
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", response.StatusCode)
	}
	body := decodeBody(t, response)
	fields, _ := body["errors"].([]any)
	if len(fields) == 0 {
		t.Fatalf("body = %v, want a field-level error naming the rejected field", body)
	}
	first, _ := fields[0].(map[string]any)
	if first["field"] != "is_admin" {
		t.Errorf("field = %v, want is_admin named in the response", first["field"])
	}
	if accounts.loginInput.Login != "" {
		t.Error("the service was called with a body that should have been refused")
	}
}

func TestLoginRejectsAMalformedBody(t *testing.T) {
	server := newServer(t, &fakeAccounts{}, httpsURL)
	response := do(t, server, http.MethodPost, "/api/v1/login", `{"login":`, "", "")
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", response.StatusCode)
	}
}

func TestLoginPassesTheClientSource(t *testing.T) {
	accounts := &fakeAccounts{
		login: func(context.Context, identity.LoginInput) (identity.Credentials, error) {
			return identity.Credentials{}, identity.ErrInvalidCredentials
		},
	}
	server := newServer(t, accounts, httpsURL)
	do(t, server, http.MethodPost, "/api/v1/login", `{"login":"root","password":"x"}`, "", "")

	if accounts.loginInput.Source == "" {
		t.Error("the limiter source was not passed to the service")
	}
}

func TestMeRequiresASession(t *testing.T) {
	server := newServer(t, &fakeAccounts{}, httpsURL)

	response := do(t, server, http.MethodGet, "/api/v1/me", "", "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a cookie status = %d, want 401", response.StatusCode)
	}
	if len(response.Cookies()) != 0 {
		t.Error("an unauthenticated response set a cookie")
	}

	// An unknown or expired token is indistinguishable from a missing one.
	failing := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return identity.User{}, identity.Session{}, nil, identity.ErrInvalidToken
		},
	}
	server = newServer(t, failing, httpsURL)
	response = do(t, server, http.MethodGet, "/api/v1/me", "", "", "some-token")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("with a bad token status = %d, want 401", response.StatusCode)
	}
}

func TestMeReturnsTheAccount(t *testing.T) {
	user, row, digest := session("csrf")
	accounts := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return user, row, digest, nil
		},
	}
	server := newServer(t, accounts, httpsURL)

	response := do(t, server, http.MethodGet, "/api/v1/me", "", "", "good-token")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.StatusCode, readAll(t, response))
	}
	body := decodeBody(t, response)
	if body["username"] != "root" || body["is_admin"] != true {
		t.Errorf("body = %v, want the account", body)
	}
	// The response must not carry the CSRF digest or any credential material.
	raw := readAll(t, response)
	for _, forbidden := range []string{"csrf", "token_hash", "password"} {
		if strings.Contains(strings.ToLower(raw), forbidden) {
			t.Errorf("the response body contains %q: %s", forbidden, raw)
		}
	}
}

// TestLogoutRequiresACSFToken is the documented control: sign-out is
// state-changing and is rejected before the handler does anything.
func TestLogoutRequiresACSFToken(t *testing.T) {
	user, row, digest := session("the-csrf-token")
	accounts := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return user, row, digest, nil
		},
	}
	server := newServer(t, accounts, httpsURL)

	for _, test := range []struct {
		name string
		csrf string
		want int
	}{
		{name: "missing", csrf: "", want: http.StatusForbidden},
		{name: "wrong", csrf: "not-the-token", want: http.StatusForbidden},
		{name: "correct", csrf: "the-csrf-token", want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := do(t, server, http.MethodPost, "/api/v1/logout", "", test.csrf, "good-token")
			if response.StatusCode != test.want {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, test.want, readAll(t, response))
			}
		})
	}
	if accounts.logoutToken != "good-token" {
		t.Errorf("the service received token %q, want the presented session token", accounts.logoutToken)
	}
}

// TestLogoutWithoutACSFTokenNeverReachesTheService proves the control runs
// before any state change.
func TestLogoutWithoutACSFTokenNeverReachesTheService(t *testing.T) {
	user, row, digest := session("the-csrf-token")
	accounts := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return user, row, digest, nil
		},
		logout: func(context.Context, string) error {
			t.Error("the service was asked to sign the caller out without a CSRF token")
			return nil
		},
	}
	server := newServer(t, accounts, httpsURL)

	response := do(t, server, http.MethodPost, "/api/v1/logout", "", "", "good-token")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.StatusCode)
	}
}

func TestLogoutClearsTheCookie(t *testing.T) {
	user, row, digest := session("the-csrf-token")
	accounts := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return user, row, digest, nil
		},
	}
	server := newServer(t, accounts, httpsURL)

	response := do(t, server, http.MethodPost, "/api/v1/logout", "", "the-csrf-token", "good-token")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("set %d cookies, want 1 to clear the session", len(cookies))
	}
	if cookies[0].Value != "" || cookies[0].MaxAge >= 0 {
		t.Errorf("cookie = %+v, want an expired empty cookie", cookies[0])
	}
}

func TestInvitationCreationRequiresAuthenticationAndCSRF(t *testing.T) {
	user, row, digest := session("the-csrf-token")
	accounts := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return user, row, digest, nil
		},
		invite: func(context.Context, identity.CreateInvitationInput) (identity.Invitation, string, error) {
			return identity.Invitation{ID: "inv-1", Email: "new@example.com", ExpiresAt: time.Now().Add(time.Hour)}, "raw-token", nil
		},
	}
	server := newServer(t, accounts, httpsURL)

	// No session.
	if got := do(t, server, http.MethodPost, "/api/v1/user/invitations", `{"email":"new@example.com"}`, "", "").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("without a session status = %d, want 401", got)
	}
	// Session but no CSRF token.
	if got := do(t, server, http.MethodPost, "/api/v1/user/invitations", `{"email":"new@example.com"}`, "", "good-token").StatusCode; got != http.StatusForbidden {
		t.Errorf("without CSRF status = %d, want 403", got)
	}
	// Authorized.
	response := do(t, server, http.MethodPost, "/api/v1/user/invitations", `{"email":"new@example.com"}`, "the-csrf-token", "good-token")
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", response.StatusCode, readAll(t, response))
	}
	body := decodeBody(t, response)
	if body["token"] != "raw-token" {
		t.Errorf("token = %v, want the raw token returned once", body["token"])
	}
	if accounts.inviteInput.ActorID != user.ID {
		t.Errorf("actor = %q, want the authenticated account, never a body field", accounts.inviteInput.ActorID)
	}
}

// TestInvitationCreationDeniedForANonAdministrator is the denied path: the
// service refuses, and the transport turns that into 403 rather than 500.
func TestInvitationCreationDeniedForANonAdministrator(t *testing.T) {
	user, row, digest := session("the-csrf-token")
	accounts := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return user, row, digest, nil
		},
		invite: func(context.Context, identity.CreateInvitationInput) (identity.Invitation, string, error) {
			return identity.Invitation{}, "", identity.ErrNotAdmin
		},
	}
	server := newServer(t, accounts, httpsURL)

	response := do(t, server, http.MethodPost, "/api/v1/user/invitations", `{"email":"new@example.com"}`, "the-csrf-token", "good-token")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.StatusCode)
	}
}

func TestInvitationCreationReportsAValidationFailure(t *testing.T) {
	user, row, digest := session("the-csrf-token")
	accounts := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return user, row, digest, nil
		},
		invite: func(context.Context, identity.CreateInvitationInput) (identity.Invitation, string, error) {
			return identity.Invitation{}, "", errors.New("email address must contain exactly one @")
		},
	}
	server := newServer(t, accounts, httpsURL)

	response := do(t, server, http.MethodPost, "/api/v1/user/invitations", `{"email":"nope"}`, "the-csrf-token", "good-token")
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", response.StatusCode, readAll(t, response))
	}
	body := decodeBody(t, response)
	fields, ok := body["errors"].([]any)
	if !ok || len(fields) == 0 {
		t.Fatalf("body = %v, want a field-level error", body)
	}
	first, _ := fields[0].(map[string]any)
	if first["field"] != "email" {
		t.Errorf("field = %v, want email so the client can attach the error", first["field"])
	}
}

// TestInvitationAcceptanceIsUnauthenticated covers the invitee path and the
// rule that the token travels in a header, never in a URL.
func TestInvitationAcceptanceIsUnauthenticated(t *testing.T) {
	accounts := &fakeAccounts{
		accept: func(_ context.Context, input identity.AcceptInvitationInput) (identity.User, error) {
			return identity.User{
				ID: "01920000-0000-7000-8000-000000000003", Username: input.Username,
				Email: "newcomer@example.com", Status: identity.StatusActive, CreatedAt: time.Unix(0, 0).UTC(),
			}, nil
		},
	}
	server := newServer(t, accounts, httpsURL)

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/invitations/accept",
		bytes.NewReader([]byte(`{"username":"newcomer","password":"`+password+`"}`)))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(api.InvitationTokenHeader, "invitation-token-value")

	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", response.StatusCode, readAll(t, response))
	}
	if accounts.acceptInput.Token != "invitation-token-value" {
		t.Errorf("token = %q, want the header value", accounts.acceptInput.Token)
	}
	body := decodeBody(t, response)
	if body["username"] != "newcomer" {
		t.Errorf("body = %v, want the created account", body)
	}
	if body["is_admin"] != false {
		t.Error("an invited account was reported as an administrator")
	}
}

func TestInvitationAcceptanceRejectsAnUnusableToken(t *testing.T) {
	accounts := &fakeAccounts{
		accept: func(context.Context, identity.AcceptInvitationInput) (identity.User, error) {
			return identity.User{}, identity.ErrInvalidToken
		},
	}
	server := newServer(t, accounts, httpsURL)

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/invitations/accept",
		bytes.NewReader([]byte(`{"username":"newcomer","password":"x"}`)))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(api.InvitationTokenHeader, "unknown-token")

	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
	body := decodeBody(t, response)
	if body["message"] != "not authenticated" {
		t.Errorf("message = %v, want the generic message", body["message"])
	}
}

func TestInvitationAcceptanceReportsAValidationFailure(t *testing.T) {
	accounts := &fakeAccounts{
		accept: func(context.Context, identity.AcceptInvitationInput) (identity.User, error) {
			return identity.User{}, errors.New("password must be at least 8 characters")
		},
	}
	server := newServer(t, accounts, httpsURL)

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/invitations/accept",
		bytes.NewReader([]byte(`{"username":"newcomer","password":"short"}`)))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(api.InvitationTokenHeader, "token")

	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", response.StatusCode, readAll(t, response))
	}
	body := decodeBody(t, response)
	fields, _ := body["errors"].([]any)
	if len(fields) == 0 {
		t.Fatalf("body = %v, want a field-level error", body)
	}
	first, _ := fields[0].(map[string]any)
	if first["field"] != "password" {
		t.Errorf("field = %v, want password", first["field"])
	}
}

// TestStateChangingRoutesAreAllCSRFGuarded is a guard against a future route
// being added without the control.
func TestStateChangingRoutesAreAllCSRFGuarded(t *testing.T) {
	user, row, digest := session("the-csrf-token")
	called := map[string]bool{}
	accounts := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return user, row, digest, nil
		},
		logout: func(context.Context, string) error { called["logout"] = true; return nil },
		invite: func(context.Context, identity.CreateInvitationInput) (identity.Invitation, string, error) {
			called["invite"] = true
			return identity.Invitation{}, "", nil
		},
	}
	server := newServer(t, accounts, httpsURL)

	for _, path := range []string{"/api/v1/logout", "/api/v1/user/invitations"} {
		t.Run(path, func(t *testing.T) {
			response := do(t, server, http.MethodPost, path, `{"email":"a@example.com"}`, "", "good-token")
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 for a state-changing request without CSRF", response.StatusCode)
			}
		})
	}
	if len(called) != 0 {
		t.Errorf("a state-changing handler ran without CSRF: %v", called)
	}
}

// TestBearerTokenIsAcceptedForScripts covers the second transport shape: the
// same handler set serves a script that has no cookie jar.
func TestBearerTokenIsAcceptedForScripts(t *testing.T) {
	user, row, digest := session("csrf")
	accounts := &fakeAccounts{
		resolve: func(context.Context, string) (identity.User, identity.Session, []byte, error) {
			return user, row, digest, nil
		},
	}
	server := newServer(t, accounts, httpsURL)

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/me", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer script-token")

	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.StatusCode, readAll(t, response))
	}
}

// readAll drains a response body for assertions in failure messages.
func readAll(t *testing.T, response *http.Response) string {
	t.Helper()
	if response.Body == nil {
		return ""
	}
	buffer := new(bytes.Buffer)
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		t.Fatalf("read the response body: %v", err)
	}
	return buffer.String()
}
