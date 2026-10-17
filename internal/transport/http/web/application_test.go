package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-gitnest/GitNest/internal/i18n"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// shellSessions resolves a cookie the way identity.Service does.
type shellSessions struct {
	users map[string]identity.User
	csrf  string
	// issue is the token a rotation hands out, and issued is the digest it
	// leaves behind, so a test can assert that a page load issued something.
	issue         string
	issued        []byte
	lastIssuedFor string
	err           error
}

// RotateCSRFToken issues a token the way the service does: it replaces the
// session's digest and returns the value.
func (s *shellSessions) RotateCSRFToken(_ context.Context, token string) (string, error) {
	user, found := s.users[token]
	if !found {
		return "", identity.ErrInvalidToken
	}
	issued := s.issue
	if issued == "" {
		issued = "issued-csrf-token"
	}
	s.issued = identity.HashToken(issued)
	s.lastIssuedFor = user.ID
	return issued, nil
}

func (s *shellSessions) ResolveSession(_ context.Context, token string) (identity.User, identity.Session, []byte, error) {
	user, found := s.users[token]
	if !found {
		return identity.User{}, identity.Session{}, nil, identity.ErrInvalidToken
	}
	digest := s.issued
	if digest == nil {
		digest = identity.HashToken(s.csrf)
	}
	return user, identity.Session{ID: "session"}, digest, nil
}

// newShellHandler mounts the application shell over a built asset set.
// shellSessions answers both questions a shell asks: who is calling, and what
// token the document may carry. One type satisfies both because the real
// identity.Service does.
func newShellHandler(t *testing.T, sessions *shellSessions) *http.ServeMux {
	t.Helper()
	assets, err := NewAssets(builtAssets())
	if err != nil {
		t.Fatalf("NewAssets: %v", err)
	}
	handler, err := NewAppHandler(assets, sessions, sessions)
	if err != nil {
		t.Fatalf("NewAppHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	return mux
}

// bootFrom extracts the shell's boot payload.
func bootFrom(t *testing.T, body string) map[string]any {
	t.Helper()
	pattern := regexp.MustCompile(`(?s)<script type="application/json" id="gitnest-boot">(.*?)</script>`)
	match := pattern.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("the shell carries no boot payload:\n%s", body)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(match[1]), &decoded); err != nil {
		t.Fatalf("the boot payload is not JSON: %v", err)
	}
	return decoded
}

// TestShellRendersTheDocumentAndTheMountPoint covers the served shape.
func TestShellRendersTheDocumentAndTheMountPoint(t *testing.T) {
	mux := newShellHandler(t, &shellSessions{users: map[string]identity.User{}})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, NewRepositoryPath, nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{
		"<!DOCTYPE html>",
		`id="root"`,
		"/assets/index-abc123.js",
		"/assets/index-def456.css",
		"gitnest-boot",
		i18n.Default().T(i18n.IDNavSkipToContent),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the shell does not carry %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type = %q, want HTML", recorder.Header().Get("Content-Type"))
	}
}

// TestShellIsPrivateAndUncacheable keeps a viewer's boot payload out of a shared
// cache: it carries a CSRF token for a signed-in reader.
func TestShellIsPrivateAndUncacheable(t *testing.T) {
	mux := newShellHandler(t, &shellSessions{users: map[string]identity.User{}})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, NewRepositoryPath, nil))

	if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "private") {
		t.Errorf("Cache-Control = %q, want private", got)
	}
	if got := recorder.Header().Get("Vary"); !strings.Contains(got, "Cookie") {
		t.Errorf("Vary = %q, want Cookie named", got)
	}
}

// TestBootPayloadCarriesTheSessionAndTheCatalogue covers what the client reads.
func TestBootPayloadCarriesTheSessionAndTheCatalogue(t *testing.T) {
	sessions := &shellSessions{
		users: map[string]identity.User{
			"a-session": {ID: "01920000-0000-7000-8000-0000000000f1", Username: "alice", Status: identity.StatusActive},
		},
	}
	mux := newShellHandler(t, sessions)

	request := httptest.NewRequest(http.MethodGet, NewRepositoryPath, nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "a-session"})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	payload := bootFrom(t, recorder.Body.String())
	if payload["account"] != "alice" {
		t.Errorf("account = %v, want the signed-in account", payload["account"])
	}
	if payload["area"] != AreaCreate {
		t.Errorf("area = %v, want %q", payload["area"], AreaCreate)
	}
	messages, _ := payload["messages"].(map[string]any)
	if len(messages) == 0 {
		t.Fatal("the boot payload carries no messages")
	}
	// Every message is a resolved string rather than an identifier, which is what
	// makes the catalogue load-bearing rather than decorative.
	for id, text := range messages {
		if text == id {
			t.Errorf("the message %q resolved to its own identifier", id)
		}
	}

	// The session token must not travel: the cookie is HttpOnly so a script
	// cannot read it, and putting it in the payload would undo that.
	if strings.Contains(recorder.Body.String(), "a-session") {
		t.Error("the shell echoes the session token")
	}
}

// TestBootPayloadOmitsAnUnknownSession keeps a stale cookie from producing an
// error page instead of the sign-in surface.
func TestBootPayloadOmitsAnUnknownSession(t *testing.T) {
	mux := newShellHandler(t, &shellSessions{users: map[string]identity.User{}})

	request := httptest.NewRequest(http.MethodGet, NewRepositoryPath, nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "stale"})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	payload := bootFrom(t, recorder.Body.String())
	if _, found := payload["account"]; found {
		t.Error("an unusable session produced an account in the payload")
	}
}

// TestUnbuiltShellSaysWhatToRun covers the helpful failure: a blank page would
// send a reader looking for a defect instead of the build step they skipped.
func TestUnbuiltShellSaysWhatToRun(t *testing.T) {
	assets, err := NewAssets(fstest.MapFS{"dist/.gitkeep": {Data: []byte("placeholder")}})
	if err != nil {
		t.Fatalf("NewAssets: %v", err)
	}
	handler, err := NewAppHandler(assets, &shellSessions{}, &shellSessions{})
	if err != nil {
		t.Fatalf("NewAppHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, NewRepositoryPath, nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "npm run build") {
		t.Errorf("the message does not name the build command:\n%s", body)
	}
}

// TestShellNegotiatesTheLocale covers the documented precedence step that exists
// today.
func TestShellNegotiatesTheLocale(t *testing.T) {
	mux := newShellHandler(t, &shellSessions{})

	request := httptest.NewRequest(http.MethodGet, NewRepositoryPath, nil)
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	if !strings.Contains(recorder.Body.String(), `lang="en-US"`) {
		t.Errorf("the shell does not declare the negotiated locale:\n%s", recorder.Body.String())
	}
	// An unsupported locale falls back rather than failing.
	request = httptest.NewRequest(http.MethodGet, NewRepositoryPath, nil)
	request.Header.Set("Accept-Language", "zz-ZZ")
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Errorf("an unsupported locale = %d, want 200", recorder.Code)
	}
}

// TestNewAppHandlerValidatesItsDependencies keeps a wiring mistake at startup.
func TestNewAppHandlerValidatesItsDependencies(t *testing.T) {
	assets, err := NewAssets(builtAssets())
	if err != nil {
		t.Fatalf("NewAssets: %v", err)
	}
	if _, err := NewAppHandler(nil, &shellSessions{}, &shellSessions{}); err == nil {
		t.Error("a nil asset set was accepted")
	}
	if _, err := NewAppHandler(assets, nil, &shellSessions{}); err == nil {
		t.Error("a nil session resolver was accepted")
	}
}

// TestTheShellIssuesATokenForTheDocument covers the criterion that a reload does
// not lose the ability to submit: the token is issued for the document being
// rendered, so two loads both produce a usable value.
func TestTheShellIssuesATokenForTheDocument(t *testing.T) {
	sessions := &shellSessions{
		users: map[string]identity.User{
			"a-session": {ID: "01920000-0000-7000-8000-0000000000a1", Username: "alice", Status: identity.StatusActive},
		},
		issue: "issued-token",
	}
	mux := newShellHandler(t, sessions)

	request := httptest.NewRequest(http.MethodGet, NewRepositoryPath, nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "a-session"})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	payload := bootFrom(t, recorder.Body.String())
	if payload["csrf_token"] != "issued-token" {
		t.Errorf("csrf_token = %v, want the issued token", payload["csrf_token"])
	}
	// The token was issued for this session, and the session now carries its
	// digest rather than the value.
	if sessions.lastIssuedFor == "" {
		t.Error("no token was issued for the session")
	}
	if !identity.DigestsMatch(identity.HashToken("issued-token"), sessions.issued) {
		t.Error("the session does not carry the issued token's digest")
	}
}

// TestAnAnonymousShellCarriesNoToken keeps a token from being issued where there
// is no session to bind it to.
func TestAnAnonymousShellCarriesNoToken(t *testing.T) {
	sessions := &shellSessions{users: map[string]identity.User{}, issue: "issued-token"}
	mux := newShellHandler(t, sessions)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, NewRepositoryPath, nil))

	payload := bootFrom(t, recorder.Body.String())
	if _, found := payload["csrf_token"]; found {
		t.Error("an anonymous shell carried a CSRF token")
	}
	if sessions.lastIssuedFor != "" {
		t.Error("a token was issued with no session")
	}
}

// TestAStaleSessionGetsNoToken covers a cookie that no longer resolves: the shell
// renders anonymously rather than issuing a token for a session that is gone.
func TestAStaleSessionGetsNoToken(t *testing.T) {
	sessions := &shellSessions{users: map[string]identity.User{}, issue: "issued-token"}
	mux := newShellHandler(t, sessions)

	request := httptest.NewRequest(http.MethodGet, NewRepositoryPath, nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "stale"})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	payload := bootFrom(t, recorder.Body.String())
	if _, found := payload["csrf_token"]; found {
		t.Error("a stale session was issued a token")
	}
}

// TestTheSettingsShellNamesItsRepository covers the settings mount point: the
// shell renders, the boot payload names the repository from the routed path, and
// it carries the confirmation word the server enforces.
func TestTheSettingsShellNamesItsRepository(t *testing.T) {
	sessions := &shellSessions{
		users: map[string]identity.User{
			"a-session": {ID: "01920000-0000-7000-8000-0000000000b1", Username: "alice", Status: identity.StatusActive},
		},
		issue: "issued-token",
	}
	mux := newShellHandler(t, sessions)

	request := httptest.NewRequest(http.MethodGet, "/alice/settings-repo/settings", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "a-session"})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	payload := bootFrom(t, recorder.Body.String())
	if payload["area"] != AreaSettings {
		t.Errorf("area = %v, want %q", payload["area"], AreaSettings)
	}
	// The repository comes from the path the server routed on, so a settings form
	// cannot be pointed elsewhere by editing the page.
	if payload["owner"] != "alice" || payload["repo"] != "settings-repo" {
		t.Errorf("owner/repo = %v/%v, want the routed path", payload["owner"], payload["repo"])
	}
	// The confirmation word travels from the server, which is the only side that
	// decides it.
	word, _ := payload["confirmation_word"].(string)
	if word == "" {
		t.Error("the settings shell does not carry the confirmation word")
	}
	if word != repository.VisibilityConfirmationWord {
		t.Errorf("confirmation_word = %q, want the rule the service enforces", word)
	}

	// Both mount points are reachable, so a deep link works on first load.
	for _, path := range []string{NewRepositoryPath, "/someone/their-repo/settings"} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: an anonymous deep link still renders the shell",
				path, recorder.Code)
		}
	}
}

// TestTheSettingsShellRevealsNoRepositoryData keeps the shell from becoming a
// disclosure: it names only the two segments the caller already typed, and it
// renders for an anonymous caller because the API is what refuses.
func TestTheSettingsShellRevealsNoRepositoryData(t *testing.T) {
	sessions := &shellSessions{users: map[string]identity.User{}, issue: "issued-token"}
	mux := newShellHandler(t, sessions)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/alice/a-private-repo/settings", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	payload := bootFrom(t, body)

	// Nothing beyond the path segments, and no token, because there is no session.
	if _, found := payload["account"]; found {
		t.Error("an anonymous settings shell carried an account")
	}
	if _, found := payload["csrf_token"]; found {
		t.Error("an anonymous settings shell carried a CSRF token")
	}
	// The payload's shape is fixed, and asserting the exact key set is what makes
	// "the shell discloses no repository data" checkable. A field added later that
	// carried a repository's contents would fail this rather than slip past a
	// substring search — which the message identifiers alone defeat, because the
	// identifier `app.settings.collaborators` legitimately contains the word.
	_ = body
	allowed := map[string]bool{
		"locale": true, "area": true, "messages": true,
		"owner": true, "repo": true, "confirmation_word": true,
		// Present only for a signed-in caller, and asserted absent above.
		"account": true, "csrf_token": true,
	}
	for key := range payload {
		if !allowed[key] {
			t.Errorf("the boot payload carries an unexpected field %q, which may disclose repository data", key)
		}
	}
}
