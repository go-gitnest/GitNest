package api

import (
	"context"
	"net/http"

	"github.com/go-gitnest/GitNest/internal/identity"
)

// AccountService is the account use cases this transport needs.
//
// The interface is defined here, at the consumer, and is satisfied by
// *identity.Service. Authorization happens inside that service, never in this
// package: a handler may only decide what to do with an outcome, not who is
// allowed to ask.
type AccountService interface {
	Login(ctx context.Context, input identity.LoginInput) (identity.Credentials, error)
	ResolveSession(ctx context.Context, sessionToken string) (identity.User, identity.Session, []byte, error)
	Logout(ctx context.Context, sessionToken string) error
	CreateInvitation(ctx context.Context, input identity.CreateInvitationInput) (identity.Invitation, string, error)
	AcceptInvitation(ctx context.Context, input identity.AcceptInvitationInput) (identity.User, error)
}

// sessionRoutes registers the account endpoints.
//
// Paths come from docs/architecture/urls-and-identifiers.md section 3, which
// names `/api/v1/login`, `/api/v1/logout`, and `/api/v1/me`. Invitation
// creation and acceptance are not in that table; they are placed under the
// account's own invitations collection, which is where GitHub-compatible
// clients look, and a test-token header carries the invitation secret so it
// never appears in a URL, a referrer, or an access log.
//
//	publicURL supplies the scheme that decides whether the session cookie is
//	`Secure`.
func sessionRoutes(mux *http.ServeMux, accounts AccountService, publicURL string) {
	mux.HandleFunc("POST /api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Login    string `json:"login"`
			Password string `json:"password"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		credentials, err := accounts.Login(r.Context(), identity.LoginInput{
			Login:    body.Login,
			Password: body.Password,
			Source:   clientSource(r),
		})
		if err != nil {
			// The status and message do not vary with the reason the credential
			// failed, so a caller cannot learn whether the account exists.
			writeDomainError(w, err)
			return
		}

		maxAge := int(credentials.Session.ExpiresAt.Sub(credentials.Session.CreatedAt).Seconds())
		http.SetCookie(w, sessionCookie(SessionCookie, credentials.SessionToken, publicURL, maxAge))
		// The CSRF token is returned in the body, not in a cookie: a
		// non-HttpOnly cookie could be set by a subdomain, and the body is the
		// one place only the same-origin script can read it.
		writeJSON(w, http.StatusOK, struct {
			User         UserResponse `json:"user"`
			CSRFToken    string       `json:"csrf_token"`
			SessionID    string       `json:"session_id"`
			ExpiresAtUTC string       `json:"expires_at"`
		}{
			User:         newUserResponse(credentials.User),
			CSRFToken:    credentials.CSRFToken,
			SessionID:    credentials.Session.ID,
			ExpiresAtUTC: credentials.Session.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	})

	// Sign-out is state-changing, so it requires the CSRF token even though the
	// caller is already authenticated: without it, a cross-site form post could
	// sign a user out (docs/security/threat-model.md, T-K4).
	mux.HandleFunc("POST /api/v1/logout", func(w http.ResponseWriter, r *http.Request) {
		user, csrfDigest, ok := requireSession(w, r, accounts)
		if !ok {
			return
		}
		if !requireCSRF(w, r, csrfDigest) {
			return
		}
		if err := accounts.Logout(r.Context(), accessToken(r)); err != nil {
			writeDomainError(w, err)
			return
		}
		http.SetCookie(w, clearSessionCookie(publicURL))
		writeJSON(w, http.StatusOK, struct {
			Message  string `json:"message"`
			Username string `json:"username"`
		}{Message: "signed out", Username: user.Username})
	})

	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		user, _, ok := requireSession(w, r, accounts)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, newUserResponse(user))
	})

	// Invitation creation is administrator-only. The service answers that
	// question; this handler only reports its answer.
	mux.HandleFunc("POST /api/v1/user/invitations", func(w http.ResponseWriter, r *http.Request) {
		user, csrfDigest, ok := requireSession(w, r, accounts)
		if !ok {
			return
		}
		if !requireCSRF(w, r, csrfDigest) {
			return
		}
		var body struct {
			Email string `json:"email"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		invitation, token, err := accounts.CreateInvitation(r.Context(), identity.CreateInvitationInput{
			ActorID: user.ID,
			Email:   body.Email,
		})
		if err != nil {
			writeInvitationError(w, err)
			return
		}
		// The raw token is returned exactly once; only its digest is stored.
		writeJSON(w, http.StatusCreated, struct {
			ID           string `json:"id"`
			Email        string `json:"email"`
			Token        string `json:"token"`
			ExpiresAtUTC string `json:"expires_at"`
		}{
			ID:           invitation.ID,
			Email:        invitation.Email,
			Token:        token,
			ExpiresAtUTC: invitation.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	})

	// Acceptance is unauthenticated by definition: the invitee has no account
	// yet. The token arrives in a header rather than the path so it cannot be
	// captured in a referrer or an access log.
	mux.HandleFunc("POST /api/v1/invitations/accept", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(InvitationTokenHeader)
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		user, err := accounts.AcceptInvitation(r.Context(), identity.AcceptInvitationInput{
			Token:    token,
			Username: body.Username,
			Password: body.Password,
		})
		if err != nil {
			writeAcceptError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, newUserResponse(user))
	})
}

// InvitationTokenHeader carries the invitation secret on acceptance.
const InvitationTokenHeader = "X-Invitation-Token"

// writeInvitationError reports an invitation failure.
//
// A validation message names its field; anything else is mapped by the shared
// status table, so a permission failure is a 403 and never a 500.
func writeInvitationError(w http.ResponseWriter, err error) {
	if status, _ := statusFor(err); status == 0 {
		writeValidationError(w, err)
		return
	}
	writeDomainError(w, err)
}

// writeAcceptError reports an acceptance failure.
//
// Every rejection — an unknown, expired, revoked, or already-used token, and a
// rejected username or password — is reported the same way to a caller that was
// not authenticated to begin with, except that a validation message still names
// its field so the invitee can fix their input.
func writeAcceptError(w http.ResponseWriter, err error) {
	if status, _ := statusFor(err); status != 0 {
		writeDomainError(w, err)
		return
	}
	field := validationField(err)
	if field == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	writeValidationError(w, err)
}

// clientSource identifies the client for throttling. It is used only as a
// limiter key and never contributes to authentication.
func clientSource(r *http.Request) string {
	if host := r.RemoteAddr; host != "" {
		return host
	}
	return "unknown"
}

// RegisterAccounts adds the account endpoints to mux.
func RegisterAccounts(mux *http.ServeMux, accounts AccountService, publicURL string) {
	sessionRoutes(mux, accounts, publicURL)
}
