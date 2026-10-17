package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/transport/http/middleware"
)

// startInstance boots the application on a free port against a schema and returns
// its base URL.
//
// It is the smallest useful instance: no repository and no Git fixture, because
// these tests are about the server's own behaviour rather than about a
// repository's.
func startInstance(t *testing.T, schema *postgrestest.Schema) string {
	t.Helper()
	port := freePort(t)
	cfg := testConfig(t, schemaDSN(t, schema), port)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- New(cfg).Run(ctx) }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForStatus(t, base+"/api/ready", http.StatusOK, 25*time.Second)
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(25 * time.Second):
			t.Error("the server did not stop during cleanup")
		}
	})
	return base
}

// TestTheServerCarriesTheSecurityHeadersEverywhere is the instance-wide half of
// the T-B6 control, asserted against the running application rather than against
// the middleware in isolation: a surface that bypassed the chain would still leak.
func TestTheServerCarriesTheSecurityHeadersEverywhere(t *testing.T) {
	schema := postgrestest.NewSchema(t)
	base := startInstance(t, schema)
	client := &http.Client{Timeout: 20 * time.Second}

	for _, path := range []string{
		"/api/health",
		"/api/v1/repos/alice/nothing",
		"/alice/nothing",
		"/new",
		"/assets/index-abc123.js",
	} {
		t.Run(path, func(t *testing.T) {
			response, err := client.Get(base + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer func() { _ = response.Body.Close() }()

			if got := response.Header.Get("Content-Security-Policy"); got != middleware.ContentSecurityPolicy() {
				t.Errorf("Content-Security-Policy = %q, want the shipped policy", got)
			}
			if got := response.Header.Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("Referrer-Policy = %q, want no-referrer", got)
			}
			if got := response.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

// TestTheInteractiveShellIsServedAndIsSelfContained covers the served shape and
// the criterion that the assets come from the same origin.
func TestTheInteractiveShellIsServedAndIsSelfContained(t *testing.T) {
	schema := postgrestest.NewSchema(t)
	base := startInstance(t, schema)

	response, err := (&http.Client{Timeout: 20 * time.Second}).Get(base + "/new")
	if err != nil {
		t.Fatalf("GET /new: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	// A checkout that has not built the frontend reports what to run rather than
	// serving a page that cannot work. Both answers are correct; which one arrives
	// depends on whether `web/dist` was built, and this test asserts the behaviour
	// of each rather than assuming a build.
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	body := string(raw)

	switch response.StatusCode {
	case http.StatusServiceUnavailable:
		if !strings.Contains(body, "npm run build") {
			t.Errorf("an unbuilt frontend did not name the build command:\n%s", body)
		}
		// The read surfaces still work on the same instance, which is the point
		// of treating an unbuilt frontend as non-fatal.
		page, err := (&http.Client{Timeout: 20 * time.Second}).Get(base + "/api/ready")
		if err != nil {
			t.Fatalf("GET /api/ready: %v", err)
		}
		defer func() { _ = page.Body.Close() }()
		if page.StatusCode != http.StatusOK {
			t.Errorf("readiness = %d while the frontend was unbuilt, want 200", page.StatusCode)
		}
	case http.StatusOK:
		for _, want := range []string{`id="root"`, "gitnest-boot", `type="module"`} {
			if !strings.Contains(body, want) {
				t.Errorf("the shell does not carry %q:\n%s", want, body)
			}
		}
		// Every referenced asset is same-origin: an absolute URL to another host
		// would be the cross-origin embedding the threat model refuses.
		if strings.Contains(body, `src="http://`) || strings.Contains(body, `src="https://`) {
			t.Errorf("the shell references an external script:\n%s", body)
		}

	default:
		t.Fatalf("status = %d, want 200 or 503:\n%s", response.StatusCode, body)
	}
}

// TestTheBootPayloadNeverCarriesTheSessionToken is the secrecy rule at the
// surface: a token in the document would defeat the HttpOnly cookie.
func TestTheBootPayloadNeverCarriesTheSessionToken(t *testing.T) {
	schema := postgrestest.NewSchema(t)
	base := startInstance(t, schema)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Sign in for real, so the request carries a live session cookie.
	accounts := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})
	_, err := accounts.CreateInitialAdmin(ctx, identity.CreateInitialAdminInput{
		Username: "alice", Email: "alice@example.com", Password: endToEndPassword,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	loginBody, err := json.Marshal(map[string]string{"login": "alice", "password": endToEndPassword})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	response, err := http.Post(base+"/api/v1/login", "application/json", strings.NewReader(string(loginBody)))
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	_ = response.Body.Close()

	var session *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == "gitnest_session" {
			session = cookie
		}
	}
	if session == nil {
		t.Fatal("the sign-in response carried no session cookie")
	}

	request, err := http.NewRequest(http.MethodGet, base+"/new", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.AddCookie(session)
	page, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET /new: %v", err)
	}
	defer func() { _ = page.Body.Close() }()

	raw, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	if strings.Contains(string(raw), session.Value) {
		t.Error("the shell echoes the session token, which defeats the HttpOnly cookie")
	}
}
