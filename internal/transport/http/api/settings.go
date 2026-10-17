package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// settingsRoutes registers the repository settings endpoints.
//
// The paths are the ones docs/architecture/urls-and-identifiers.md section 3.2
// lists: `/api/v1/repos/{owner}/{repo}` for the repository itself, so a settings
// change is a `PATCH` to the resource a read fetches, and
// `/api/v1/repos/{owner}/{repo}/collaborators` for direct grants.
//
// One rule governs this whole surface: the transport never decides access and
// never restates a policy. It resolves the caller, hands the service the request,
// and renders the answer — including the confirmation a visibility transition
// requires, which the service demands and the service explains. The level check
// that runs before a handler is a fast path, not the control: the operations
// enforce it again inside the service.
func settingsRoutes(mux *http.ServeMux, accounts AccountService, owners RepositoryOwnerLookup, repositories RepositoryService) {
	mux.HandleFunc("PATCH /api/v1/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		// A settings change is state-changing, so it carries the CSRF requirement
		// before anything else runs. The check runs before the resource resolves,
		// so a cross-site request learns nothing about whether the repository
		// exists.
		if !csrfFromSession(w, r, accounts) {
			return
		}
		row, owner, level, ok := resolveRepository(w, r, accounts, owners, repositories, repository.LevelAdmin)
		if !ok {
			return
		}

		var body struct {
			Visibility    *string `json:"visibility"`
			DefaultBranch *string `json:"default_branch"`
			Archived      *bool   `json:"archived"`
			Confirmation  string  `json:"confirmation"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}

		input := repository.UpdateSettingsInput{
			Actor:         principalFor(viewer(r, accounts)),
			RepoID:        row.ID,
			DefaultBranch: body.DefaultBranch,
			Archived:      body.Archived,
			Confirmation:  body.Confirmation,
			Request:       requestContextFor(r),
		}
		if body.Visibility != nil {
			visibility := repository.Visibility(*body.Visibility)
			input.Visibility = &visibility
		}

		updated, err := repositories.UpdateSettings(r.Context(), input)
		if err != nil {
			writeSettingsError(w, err)
			return
		}
		// The response is the resource as it now stands, so a client does not
		// have to fetch it again after a change it already knows the shape of.
		public := updated.Visibility == repository.VisibilityPublic && level == repository.LevelRead
		writeAPIJSON(w, http.StatusOK, public, newRepositoryResponse(updated, owner.Username, level))
	})

	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/collaborators", func(w http.ResponseWriter, r *http.Request) {
		row, _, _, ok := resolveRepository(w, r, accounts, owners, repositories, repository.LevelAdmin)
		if !ok {
			return
		}
		// The service decides who may list; this handler only renders the answer.
		collaborators, err := repositories.Collaborators(r.Context(), principalFor(viewer(r, accounts)), row.ID)
		if err != nil {
			writeSettingsError(w, err)
			return
		}

		items := make([]collaboratorResponse, 0, len(collaborators))
		for _, collaborator := range collaborators {
			item := collaboratorResponse{Login: collaborator.UserID, Role: collaborator.Role.String()}
			// A grant stores the account, and a name is what a caller can act on.
			// A grant whose account cannot be resolved is reported with the
			// identifier it was granted to rather than dropped: a listing that
			// silently omits a grant is worse than one that shows an identifier.
			if account, lookupErr := owners.AccountByID(r.Context(), collaborator.UserID); lookupErr == nil {
				item.Login = account.Username
			}
			items = append(items, item)
		}
		writeAPIJSON(w, http.StatusOK, false, items)
	})

	mux.HandleFunc("PUT /api/v1/repos/{owner}/{repo}/collaborators/{username}", func(w http.ResponseWriter, r *http.Request) {
		mutateCollaborator(w, r, accounts, owners, repositories, true)
	})

	mux.HandleFunc("DELETE /api/v1/repos/{owner}/{repo}/collaborators/{username}", func(w http.ResponseWriter, r *http.Request) {
		mutateCollaborator(w, r, accounts, owners, repositories, false)
	})

	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/collaborators/{username}/permission", func(w http.ResponseWriter, r *http.Request) {
		row, _, _, ok := resolveRepository(w, r, accounts, owners, repositories, repository.LevelAdmin)
		if !ok {
			return
		}
		target, ok := resolveAccount(w, r, owners, r.PathValue("username"))
		if !ok {
			return
		}

		// The permission reported for another account is the one the service
		// resolves for that account, not a value this transport derives. An
		// account with no access has a permission of `none`, which is an answer
		// rather than a failure: the caller can already list the grants, so
		// saying "none" discloses nothing they could not read.
		_, level, err := repositories.Resolve(r.Context(), repository.User(target.ID, target.IsAdmin), row.OwnerID, row.Name)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				writeAPIJSON(w, http.StatusOK, false, map[string]any{
					"permission": repository.LevelNone.String(),
					"role_name":  repository.LevelNone.String(),
				})
				return
			}
			writeSettingsError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, false, map[string]any{
			"permission": level.String(),
			"role_name":  level.String(),
		})
	})
}

// collaboratorResponse is one direct grant.
//
// `login` is the account name, which is the field a GitHub-compatible client
// reads, and `role_name` is the grant it holds.
type collaboratorResponse struct {
	Login string `json:"login"`
	Role  string `json:"role_name"`
}

// RegisterSettings adds the repository settings endpoints to mux.
func RegisterSettings(mux *http.ServeMux, accounts AccountService, owners RepositoryOwnerLookup, repositories RepositoryService) {
	settingsRoutes(mux, accounts, owners, repositories)
}

// mutateCollaborator grants or revokes a direct grant.
//
// One handler serves both directions because they differ only in which service
// method they call and which status they answer: a grant is `201` when the
// account had none and `200` when its role changed, and a revocation is `204`.
func mutateCollaborator(
	w http.ResponseWriter, r *http.Request, accounts AccountService,
	owners RepositoryOwnerLookup, repositories RepositoryService, grant bool,
) {
	// Both directions change state, so both carry the CSRF requirement. It runs
	// before anything else, so a cross-site request changes nothing.
	if !csrfFromSession(w, r, accounts) {
		return
	}
	row, _, _, ok := resolveRepository(w, r, accounts, owners, repositories, repository.LevelAdmin)
	if !ok {
		return
	}
	target, ok := resolveAccount(w, r, owners, r.PathValue("username"))
	if !ok {
		return
	}

	actor := principalFor(viewer(r, accounts))
	if !grant {
		err := repositories.RevokeAccess(r.Context(), repository.RevokeInput{
			Actor: actor, RepoID: row.ID, UserID: target.ID, Request: requestContextFor(r),
		})
		if err != nil {
			writeSettingsError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var body struct {
		Role string `json:"role_name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	role, err := parseRole(body.Role)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error(), FieldError{
			Resource: "collaborator", Field: repository.FieldRole, Code: "invalid", Message: err.Error(),
		})
		return
	}

	added, err := repositories.GrantAccess(r.Context(), repository.GrantInput{
		Actor: actor, RepoID: row.ID, UserID: target.ID, Role: role, Request: requestContextFor(r),
	})
	if err != nil {
		writeSettingsError(w, err)
		return
	}
	status := http.StatusOK
	if added {
		status = http.StatusCreated
	}
	writeAPIJSON(w, status, false, collaboratorResponse{Login: target.Username, Role: role.String()})
}

// parseRole reads a grant role from a payload.
//
// It accepts the three grantable roles, and their push/pull synonyms, because a
// client written against the GitHub vocabulary will send one or the other. It
// refuses `owner` with a message that says why, because a client asking for it
// has a real misunderstanding to correct rather than a typo.
func parseRole(value string) (repository.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "read", "pull":
		return repository.LevelRead, nil
	case "write", "push":
		return repository.LevelWrite, nil
	case "admin":
		return repository.LevelAdmin, nil
	case "owner":
		return repository.LevelNone, errors.New(
			"ownership is not a grant; it follows from the repository's owner")
	default:
		return repository.LevelNone, errors.New("the role must be one of read, write, or admin")
	}
}

// resolveAccount turns a `{username}` segment into an account.
//
// An unknown account and a malformed one are both answered as not-found, so the
// endpoint cannot be used to enumerate accounts.
func resolveAccount(w http.ResponseWriter, r *http.Request, owners RepositoryOwnerLookup, username string) (identity.User, bool) {
	account, err := owners.OwnerByUsername(r.Context(), username)
	if err != nil {
		if !errors.Is(err, identity.ErrInvalidCredentials) {
			writeDomainError(w, err)
			return identity.User{}, false
		}
		writeAPINotFound(w)
		return identity.User{}, false
	}
	return account, true
}

// principalFor turns a resolved session into the principal a service consumes.
//
// It carries identity only: no role, no scope, no pre-decided permission. Every
// access answer is the service's.
func principalFor(user identity.User) repository.Principal {
	if user.ID == "" {
		return repository.Anonymous()
	}
	return repository.User(user.ID, user.IsAdmin)
}

// requestContextFor keeps the part of a request an audit record stores.
func requestContextFor(r *http.Request) audit.RequestContext {
	return audit.RequestContext{
		SourceAddress: clientSource(r),
		UserAgent:     r.Header.Get("User-Agent"),
	}
}

// writeSettingsError maps a settings failure onto a response.
func writeSettingsError(w http.ResponseWriter, err error) {
	// A validation failure carries its own field list, so it is rendered as the
	// envelope's field-level part rather than collapsed into one message. Every
	// rejected field is reported at once, which is what makes one round trip
	// enough for a form with two mistakes in it.
	var problems repository.ValidationErrors
	if errors.As(err, &problems) {
		fieldErrors := make([]FieldError, 0, len(problems))
		for _, problem := range problems {
			fieldErrors = append(fieldErrors, FieldError{
				Resource: "repository",
				Field:    problem.Field,
				Code:     codeForField(problem.Field),
				Message:  problem.Message,
			})
		}
		writeError(w, http.StatusUnprocessableEntity, problems.Error(), fieldErrors...)
		return
	}

	switch {
	case errors.Is(err, repository.ErrConfirmationRequired):
		writeError(w, http.StatusUnprocessableEntity, err.Error(), FieldError{
			Resource: "repository", Field: repository.FieldConfirmation,
			Code: "confirmation_required", Message: err.Error(),
		})
	case errors.Is(err, repository.ErrArchived):
		// The caller held the level and the repository is read-only, so the
		// answer names the state: existence is not a secret from an
		// administrator, and a not-found would only hide the reason.
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, repository.ErrNoChange):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, repository.ErrForbidden):
		// A caller who may see a repository but not administer it is told as
		// much: existence is not a secret from someone who can read it.
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, repository.ErrNotFound):
		writeAPINotFound(w)
	case errors.Is(err, repository.ErrNameTaken):
		writeError(w, http.StatusConflict, err.Error(), FieldError{
			Resource: "repository", Field: "name", Code: "already_exists", Message: err.Error(),
		})
	case errors.Is(err, repository.ErrSchemaNotMigrated):
		writeError(w, http.StatusServiceUnavailable, "the database is not migrated")
	default:
		// Anything the identity package classifies keeps its status rather than
		// becoming a 500.
		if status, message := statusFor(err); status != 0 {
			writeError(w, status, message)
			return
		}
		// A message from the repository package is a request problem rather than
		// a server fault, so it is reported as a validation failure with the text
		// the domain produced.
		message := err.Error()
		if isRequestProblem(message) {
			writeError(w, http.StatusUnprocessableEntity, message)
			return
		}
		writeError(w, http.StatusInternalServerError, "the request could not be completed")
	}
}

// codeForField gives a machine-readable reason for a rejected field.
//
// The codes are stable identifiers a client can branch on, which is what the
// error envelope's `code` is for.
func codeForField(field string) string {
	if field == repository.FieldConfirmation {
		return "confirmation_required"
	}
	return "invalid"
}

// isRequestProblem reports whether a domain message describes what the caller
// asked for rather than what the server did.
//
// It is a heuristic over the message text, and it exists because the repository
// package returns plain errors for shape problems. It is deliberately narrow: a
// message it does not recognise becomes a 500, because reporting a server fault
// as the caller's mistake is worse than the reverse.
func isRequestProblem(message string) bool {
	for _, fragment := range []string{
		"must", "cannot", "is not", "at most", "at least", "between", "not valid", "requires",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}
