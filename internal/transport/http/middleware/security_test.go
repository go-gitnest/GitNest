package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/transport/http/middleware"
)

// TestSecurityHeadersAreOnEveryResponse is the header assertion the threat model
// requires for T-B6.
//
// The row states the control as "referrer policy and content-security policy are
// set instance-wide", so the assertion is that they are present on a response the
// chain produced, not that a constant has a value.
func TestSecurityHeadersAreOnEveryResponse(t *testing.T) {
	handler := middleware.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/alice/private-repo", nil))

	header := recorder.Header()
	if got := header.Get("Content-Security-Policy"); got != middleware.ContentSecurityPolicy() {
		t.Errorf("Content-Security-Policy = %q, want the shipped policy", got)
	}
	if got := header.Get("Referrer-Policy"); got != middleware.ReferrerPolicy() {
		t.Errorf("Referrer-Policy = %q, want %q", got, middleware.ReferrerPolicy())
	}
	if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// TestThePolicyForbidsExternalResources is the "disallowed embed case" the threat
// model names as the second half of the T-B6 control.
//
// A remote image or iframe would leak the viewer's address, the referrer, and
// whether a private path exists, so the policy has to deny every external origin
// rather than only scripts.
func TestThePolicyForbidsExternalResources(t *testing.T) {
	policy := middleware.ContentSecurityPolicy()

	for _, directive := range []string{
		// A remote image is the leak the row describes.
		"img-src 'self'",
		// A frame is the other half of it.
		"frame-ancestors 'none'",
		// A connection to another origin would carry the response's contents.
		"connect-src 'self'",
		// The catch-all: anything not named falls to this.
		"default-src 'self'",
	} {
		if !strings.Contains(policy, directive) {
			t.Errorf("the policy does not carry %q:\n%s", directive, policy)
		}
	}

	// No directive may allow every origin, which is what a `*` would do.
	if strings.Contains(policy, " *") || strings.Contains(policy, "*;") {
		t.Errorf("the policy allows an arbitrary origin:\n%s", policy)
	}
	// Nor `unsafe-inline` or `unsafe-eval`, either of which would defeat the
	// script rule.
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval"} {
		if strings.Contains(policy, forbidden) {
			t.Errorf("the policy carries %q:\n%s", forbidden, policy)
		}
	}
}

// TestReferrerPolicyKeepsPathsOffOtherOrigins covers why the policy is the
// strictest one rather than the browser default.
func TestReferrerPolicyKeepsPathsOffOtherOrigins(t *testing.T) {
	if got := middleware.ReferrerPolicy(); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer: a GitNest URL can name a private repository", got)
	}
}

// TestAHandlerCannotAppendASecondPolicy keeps a surface from weakening the
// instance-wide policy by setting its own: a browser takes the intersection of
// two policies, so an appended header is not what a handler intends.
func TestAHandlerCannotAppendASecondPolicy(t *testing.T) {
	handler := middleware.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src *")
		w.WriteHeader(http.StatusOK)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	values := recorder.Header().Values("Content-Security-Policy")
	if len(values) != 1 {
		t.Errorf("the response carries %d policies, want exactly one", len(values))
	}
}
