package middleware

import "net/http"

// SecurityHeaders sets the response headers that constrain what a page may load
// and what it may tell the next origin.
//
// docs/security/threat-model.md T-B6 requires that "referrer policy and
// content-security policy are set instance-wide", and names the control for
// cross-repository and cross-origin resource embedding: a page that could load a
// remote image or iframe would leak the viewer's address, the referrer, and
// whether a private path exists. The policy below is the enforcement of that
// control, so it is applied by the chain rather than by each surface: a surface
// that forgot it would be the leak.
//
// The policy allows only the instance itself, which is what the architecture
// requires — assets are served same-origin and no read surface embeds a remote
// resource.
//   - `default-src 'self'` denies every external origin, including images and
//     frames, so an injected remote reference cannot call home.
//   - `frame-ancestors 'none'` refuses to be framed, which replaces
//     `X-Frame-Options` and also covers the clickjacking case for a modern
//     browser.
//   - `base-uri 'none'` and `form-action 'self'` stop an injected `<base>` or
//     form target from redirecting a submission.
//   - `object-src 'none'` refuses plugins.
//
// `script-src 'self'` rather than `'none'`: the interactive surfaces load a
// hashed module from this origin, and the read surfaces ship no script at all.
// A nonce would be stricter for the shell, but a nonce per response means the
// shell cannot be cached, and the shell carries no viewer-dependent content.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self'; font-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"

// referrerPolicy keeps a private path off the next origin's referrer header.
//
// `no-referrer` rather than `strict-origin-when-cross-origin`: a GitNest URL can
// name a private repository, and the origin alone already discloses that the
// instance exists. Nothing here needs a referrer to work.
const referrerPolicy = "no-referrer"

// SecurityHeaders applies the policy to every response.
//
// It is safe for a JSON response: a content type that cannot load a subresource
// simply ignores the policy, and the referrer header matters for an API response
// too, because an API URL can name a private repository.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		// Set rather than Add: a handler that set its own policy would otherwise
		// append a second header, and a browser takes the intersection, which is
		// not what a handler intends by setting a value.
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("Referrer-Policy", referrerPolicy)
		// A response whose content type a browser might sniff is how an uploaded
		// or repository-provided file becomes a script. It is not named by the
		// matrix, and it is one line of the same control.
		header.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// ContentSecurityPolicy is the policy this package applies, exported so a test
// asserts the shipped value rather than a copy of it.
func ContentSecurityPolicy() string { return contentSecurityPolicy }

// ReferrerPolicy is the policy this package applies.
func ReferrerPolicy() string { return referrerPolicy }
