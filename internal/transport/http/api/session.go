package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-gitnest/GitNest/internal/comments"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/labels"
	"github.com/go-gitnest/GitNest/internal/milestones"
)

// SessionCookie is the browser session cookie. The name is not part of any
// published contract, so it is GitNest's own rather than a GitHub-compatible one.
const SessionCookie = "gitnest_session"

// CSRFTokenHeader carries the token that must accompany a state-changing request.
// The name matches the convention Git clients and scripts already expect
// (docs/architecture/urls-and-identifiers.md does not fix it, so parity wins).
const CSRFTokenHeader = "X-CSRF-Token"

// ErrorResponse is the single error body shape.
//
// The fields are the ones docs/architecture.md section 11 requires: a
// machine-readable code and a human-readable message, with no internal detail.
// `documentation_url` arrives with the API workstream in phase 4, which owns the
// published shape; until then the field is simply absent rather than empty.
type ErrorResponse struct {
	Message string       `json:"message"`
	Errors  []FieldError `json:"errors,omitempty"`
}

// FieldError names one rejected field.
type FieldError struct {
	Resource string `json:"resource,omitempty"`
	Field    string `json:"field,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

// writeError sends the error envelope. It never includes a database error, a
// token, or a stack trace, and it deliberately does not vary its message with
// the reason a credential failed.
func writeError(w http.ResponseWriter, status int, message string, fieldErrors ...FieldError) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// An authenticated response must not be stored by a shared cache, and an
	// error can reveal whether a resource exists, so nothing here is cacheable.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Message: message, Errors: fieldErrors})
}

// writeJSON sends a JSON body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// maxRequestBody bounds a request body, so a client cannot make the server read
// an unbounded amount of JSON.
const maxRequestBody = 64 << 10

// decodeJSON reads a JSON body into target.
//
// DisallowUnknownFields is on because silently ignoring a misspelled field is
// how a client sets something it did not mean to, and because a body carrying
// an unexpected privilege field should be refused rather than accepted and
// ignored (docs/security/threat-model.md).
//
// The two failures are reported differently on purpose: a malformed body is a
// client bug with no field to name, while an unknown field is a client bug the
// message can point at. A caller staring at "could not be read" has no way to
// find the typo.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	err := decoder.Decode(target)
	if err == nil {
		return true
	}
	if field, ok := unknownField(err); ok {
		const message = "the request body contains a field this endpoint does not accept"
		writeError(w, http.StatusUnprocessableEntity, message, FieldError{
			Field:   field,
			Code:    "unknown_field",
			Message: message,
		})
		return false
	}
	writeError(w, http.StatusUnprocessableEntity, "the request body could not be read")
	return false
}

// unknownField extracts the field name from the decoder's unknown-field error,
// which is the only place it appears. A future Go release could reword that
// message; the fallback is then a 422 without a field name, which is still the
// documented status for this case.
func unknownField(err error) (string, bool) {
	const marker = "unknown field "
	message := err.Error()
	index := strings.Index(message, marker)
	if index < 0 {
		return "", false
	}
	name := strings.Trim(strings.TrimSpace(message[index+len(marker):]), `"`)
	if name == "" {
		return "", false
	}
	return name, true
}

// UserResponse is the public view of an account. It exists so a handler can
// never serialize the internal type by accident, which is how a password hash
// would reach a client.
type UserResponse struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	IsAdmin   bool   `json:"is_admin"`
	CreatedAt string `json:"created_at"`
}

func newUserResponse(user identity.User) UserResponse {
	return UserResponse{
		ID:        user.ID,
		Username:  user.Username,
		Email:     user.Email,
		Status:    string(user.Status),
		IsAdmin:   user.IsAdmin,
		CreatedAt: user.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// statusFor maps a domain error to an HTTP status.
//
// The mapping is the contract a client codes against, so it lives in one place
// rather than at each call site:
//
//	401 unauthenticated — a missing, wrong, or unusable credential
//	403 authenticated but not permitted
//	404 the subject does not exist, or the caller may not know that it does
//	422 the request was understood and rejected
//	429 throttled
func statusFor(err error) (int, string) {
	switch {
	case errors.Is(err, identity.ErrInvalidCredentials):
		// Deliberately generic: it must not disclose whether the account exists.
		return http.StatusUnauthorized, "invalid credentials"
	case errors.Is(err, identity.ErrInvalidToken):
		return http.StatusUnauthorized, "not authenticated"
	case errors.Is(err, identity.ErrAccountDisabled):
		return http.StatusForbidden, "this account is disabled"
	case errors.Is(err, identity.ErrNotAdmin):
		return http.StatusForbidden, "administrator rights are required"
	case errors.Is(err, identity.ErrRateLimited):
		return http.StatusTooManyRequests, "too many attempts; try again later"
	case errors.Is(err, identity.ErrSchemaNotMigrated):
		return http.StatusServiceUnavailable, "the database is not migrated"
	case errors.Is(err, identity.ErrUsernameTaken), errors.Is(err, identity.ErrEmailTaken), errors.Is(err, identity.ErrAdminExists):
		return http.StatusConflict, err.Error()

	// The issue domain. The mapping lives here with the rest so there is one place that turns a
	// domain error into a status, rather than one per handler.
	case errors.Is(err, issues.ErrNotFound):
		// The same answer a repository that does not exist gets, which is the T-A4 shape: an
		// issue a caller may not reach is indistinguishable from one that is not there.
		return http.StatusNotFound, "not found"
	case errors.Is(err, issues.ErrAnonymousAuthor):
		return http.StatusUnauthorized, err.Error()
	case errors.Is(err, issues.ErrForbidden):
		// A 403 rather than a 404, because the caller can already read the repository — the
		// repository service resolved their level — so refusing plainly discloses nothing.
		return http.StatusForbidden, err.Error()
	case errors.Is(err, issues.ErrConflict):
		return http.StatusConflict, err.Error()
	case errors.Is(err, issues.ErrInvalidTitle), errors.Is(err, issues.ErrInvalidBody),
		errors.Is(err, issues.ErrInvalidState), errors.Is(err, issues.ErrNoChange),
		errors.Is(err, issues.ErrAlreadyInState):
		return http.StatusUnprocessableEntity, err.Error()

	// The comment domain, on the same terms.
	//
	// **This mapping was missing at first, and every refusal became a 500.** `writeDomainError` treats
	// an unmapped error as an internal failure, so `comments.ErrNotFound` — a refusal the service
	// produces deliberately — reached a client as "the request could not be completed". The
	// end-to-end tests caught it; a unit test of the comment service would not, because the service
	// was behaving correctly and only the mapping was absent.
	case errors.Is(err, comments.ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, comments.ErrAnonymousAuthor):
		return http.StatusUnauthorized, err.Error()
	case errors.Is(err, comments.ErrForbidden):
		return http.StatusForbidden, err.Error()
	case errors.Is(err, comments.ErrConflict):
		return http.StatusConflict, err.Error()
	case errors.Is(err, comments.ErrEmptyBody), errors.Is(err, comments.ErrBodyTooLong),
		errors.Is(err, comments.ErrNoChange), errors.Is(err, comments.ErrDeleted):
		return http.StatusUnprocessableEntity, err.Error()

	// The label domain, on the same terms.
	case errors.Is(err, labels.ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, labels.ErrForbidden):
		return http.StatusForbidden, err.Error()
	case errors.Is(err, labels.ErrNameTaken):
		return http.StatusConflict, err.Error()
	case errors.Is(err, labels.ErrNotAttached):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, labels.ErrInvalidName), errors.Is(err, labels.ErrInvalidColor),
		errors.Is(err, labels.ErrInvalidDescription), errors.Is(err, labels.ErrNoChange):
		return http.StatusUnprocessableEntity, err.Error()

	// The milestone domain, on the same terms — and added here in the same change as its web counterpart,
	// because omitting one of the two has now happened twice and the symptom is always a deliberate refusal
	// reported as a 422.
	case errors.Is(err, milestones.ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, milestones.ErrForbidden):
		return http.StatusForbidden, err.Error()
	case errors.Is(err, milestones.ErrTitleTaken):
		return http.StatusConflict, err.Error()
	case errors.Is(err, milestones.ErrNotAttached):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, milestones.ErrClosed):
		// **409 rather than 422**: the request is well formed and names a real milestone, but the
		// milestone's state conflicts with it. A client can fix this by reopening the milestone, which is
		// not true of a malformed request.
		return http.StatusConflict, err.Error()
	case errors.Is(err, milestones.ErrInvalidTitle), errors.Is(err, milestones.ErrInvalidDescription),
		errors.Is(err, milestones.ErrInvalidDueDate), errors.Is(err, milestones.ErrInvalidState),
		errors.Is(err, milestones.ErrNoChange), errors.Is(err, milestones.ErrAlreadyInState):
		return http.StatusUnprocessableEntity, err.Error()
	}
	return 0, ""
}

// writeDomainError sends the envelope for a domain error, and reports whether it
// handled the error at all. An unmapped error becomes a 500 with a generic
// message, because the alternative is leaking whatever the database said.
func writeDomainError(w http.ResponseWriter, err error) {
	if status, message := statusFor(err); status != 0 {
		writeError(w, status, message)
		return
	}
	writeError(w, http.StatusInternalServerError, "the request could not be completed")
}

// validationField guesses which field a validation message is about, so a
// client can attach the error to the right input. The domain error text is the
// only signal available; the alternative would be a second validation path in
// the transport, which the architecture forbids.
func validationField(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "username"):
		return "username"
	case strings.Contains(message, "email"):
		return "email"
	case strings.Contains(message, "password"):
		return "password"
	}
	return ""
}

// writeValidationError sends a 422 with the offending field named.
func writeValidationError(w http.ResponseWriter, err error) {
	field := validationField(err)
	writeError(w, http.StatusUnprocessableEntity, err.Error(), FieldError{
		Resource: "account",
		Field:    field,
		Code:     "invalid",
		Message:  err.Error(),
	})
}

// accessToken returns the session token presented by the request.
//
// The browser sends it in the cookie; a script or a future API client sends the
// same value as a bearer token, so one handler set serves both without either
// transport having to know about the other.
func accessToken(r *http.Request) string {
	if cookie, err := r.Cookie(SessionCookie); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	header := r.Header.Get("Authorization")
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

// requireSession resolves the caller's session or rejects the request.
//
// It returns the account and the session's CSRF token digest, so a
// state-changing handler can verify the token before doing anything else.
func requireSession(w http.ResponseWriter, r *http.Request, accounts AccountService) (identity.User, []byte, bool) {
	token := accessToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return identity.User{}, nil, false
	}
	user, _, csrfDigest, err := accounts.ResolveSession(r.Context(), token)
	if err != nil {
		writeDomainError(w, err)
		return identity.User{}, nil, false
	}
	return user, csrfDigest, true
}

// requireCSRF verifies the session-bound CSRF token.
//
// docs/security/threat-model.md T-K4 requires it on every state-changing
// request, verified before the handler runs: this function is called before any
// work is done, and the comparison is constant time.
func requireCSRF(w http.ResponseWriter, r *http.Request, csrfDigest []byte) bool {
	presented := r.Header.Get(CSRFTokenHeader)
	if presented == "" {
		writeError(w, http.StatusForbidden, "a CSRF token is required for this request")
		return false
	}
	if !identity.DigestsMatch(identity.HashToken(presented), csrfDigest) {
		writeError(w, http.StatusForbidden, "the CSRF token is not valid")
		return false
	}
	return true
}

// sessionCookie builds the session cookie.
//
// HttpOnly keeps the token away from browser scripts and SameSite=Lax keeps it
// off cross-site requests. Secure is set whenever the instance's public URL uses
// https. It is deliberately not forced on an http loopback instance: a browser
// drops a Secure cookie that arrives over http, so forcing it would break the
// documented development setup entirely. The public URL validator refuses http
// for any non-loopback host, so the one exception cannot reach a deployment.
func sessionCookie(name, value, publicURL string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   strings.HasPrefix(publicURL, "https://"),
	}
}

// clearSessionCookie expires the cookie, using the same shape so a browser
// replaces it rather than keeping both.
func clearSessionCookie(publicURL string) *http.Cookie {
	return sessionCookie(SessionCookie, "", publicURL, -1)
}

// requireBearerOrCookie is a small helper for messages that must name the
// accepted transports without listing what was tried.
func authHint() string {
	return fmt.Sprintf("send the %s cookie or a bearer token", SessionCookie)
}
