package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-gitnest/GitNest/internal/gittransport"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// RepositoryService is the repository use cases this transport needs.
//
// It is satisfied by *repository.Service. Every access question goes through it,
// so the API, the renderer, and the Git transports cannot disagree about who may
// see what, and every policy rule — the archive refusal, the confirmation a
// visibility transition needs — is applied by the service rather than restated
// here.
type RepositoryService interface {
	Create(ctx context.Context, input repository.CreateInput) (repository.Repository, error)
	Resolve(ctx context.Context, viewer repository.Principal, ownerID, name string) (repository.Repository, repository.Level, error)
	List(ctx context.Context, query repository.ListQuery) (repository.Page, error)
	// UpdateSettings applies repository settings, validating the whole request
	// before it changes anything.
	UpdateSettings(ctx context.Context, input repository.UpdateSettingsInput) (repository.Repository, error)
	// GrantAccess gives an account a direct grant. It reports whether the grant
	// was new, so a transport can answer 201 or 200.
	GrantAccess(ctx context.Context, input repository.GrantInput) (bool, error)
	// RevokeAccess removes a direct grant.
	RevokeAccess(ctx context.Context, input repository.RevokeInput) error
	// Collaborators lists a repository's direct grants.
	Collaborators(ctx context.Context, actor repository.Principal, repoID string) ([]repository.Collaborator, error)
}

// ContentReader reads repository content. It is satisfied by
// *gittransport.Reader.
type ContentReader interface {
	ResolveCommit(ctx context.Context, storageID, revision string) (string, error)
	ListTree(ctx context.Context, storageID, revision, path string) ([]gittransport.Entry, error)
	ReadFile(ctx context.Context, storageID, revision, path string) (gittransport.File, error)
	Log(ctx context.Context, storageID, revision, path string, limit, skip int) ([]gittransport.Commit, error)
}

// RepositoryOwnerLookup resolves an `{owner}` segment and an account
// identifier. It is satisfied by *identity.Service, and is the same resolution
// the Git transport uses.
type RepositoryOwnerLookup interface {
	OwnerByUsername(ctx context.Context, username string) (identity.User, error)
	// AccountByID resolves an account from its identifier, which a collaborator
	// grant stores instead of a name.
	AccountByID(ctx context.Context, id string) (identity.User, error)
}

// repositoryResponse is the public shape of a repository.
//
// It is an explicit struct rather than the domain type, so a field added to the
// domain cannot reach a client by accident, and so the JSON names are a contract
// rather than a reflection of Go field names.
type repositoryResponse struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Owner         string `json:"owner"`
	Description   string `json:"description"`
	Visibility    string `json:"visibility"`
	DefaultBranch string `json:"default_branch"`
	// Permission is the caller's resolved level, which is what makes the
	// response explain what the caller may do rather than leaving it to be
	// inferred from a status code.
	Permission string `json:"permission"`
}

func newRepositoryResponse(row repository.Repository, owner string, level repository.Level) repositoryResponse {
	return repositoryResponse{
		ID:            row.ID,
		Name:          row.Name,
		FullName:      owner + "/" + row.Name,
		Owner:         owner,
		Description:   row.Description,
		Visibility:    string(row.Visibility),
		DefaultBranch: row.DefaultBranch,
		Permission:    level.String(),
	}
}

// treeEntryResponse is one directory entry.
type treeEntryResponse struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// fileResponse is a file's content.
type fileResponse struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// commitResponse is one commit in a listing.
type commitResponse struct {
	SHA         string `json:"sha"`
	Message     string `json:"message"`
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
	AuthoredAt  string `json:"authored_at"`
}

// repositoryRoutes registers the repository half of the versioned API.
//
// The paths are the ones docs/architecture/urls-and-identifiers.md section 3
// lists: `/api/v1/user/repos` for the caller's own repositories and
// `/api/v1/repos/{owner}/{repo}` for one repository, with `contents` and
// `commits` beneath it.
func repositoryRoutes(mux *http.ServeMux, accounts AccountService, owners RepositoryOwnerLookup, repositories RepositoryService, content ContentReader) {
	mux.HandleFunc("GET /api/v1/user/repos", func(w http.ResponseWriter, r *http.Request) {
		user := viewer(r, accounts)
		if user.ID == "" {
			// A personal repository list is meaningless without an account.
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		page, err := parsePagination(r)
		if err != nil {
			writeValidationError(w, err)
			return
		}

		// The listing is scoped to the caller's own namespace rather than
		// filtered afterwards, so the visibility predicate and the page apply to
		// the same rows.
		found, err := repositories.List(r.Context(), repository.ListQuery{
			Viewer:  repository.User(user.ID, user.IsAdmin),
			OwnerID: user.ID,
			Limit:   page.perPage,
			Offset:  page.offset(),
		})
		if err != nil {
			writeDomainError(w, err)
			return
		}

		items := make([]repositoryResponse, 0, len(found.Repositories))
		for _, row := range found.Repositories {
			viewerLevel, err := levelFor(r.Context(), repositories, user, row)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			items = append(items, newRepositoryResponse(row, user.Username, viewerLevel))
		}
		if link := linkHeader(r, page, found.HasMore); link != "" {
			w.Header().Set("Link", link)
		}
		writeAPIJSON(w, http.StatusOK, false, items)
	})

	mux.HandleFunc("POST /api/v1/user/repos", func(w http.ResponseWriter, r *http.Request) {
		user := viewer(r, accounts)
		if user.ID == "" {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		// Creating a repository changes the caller's own state, so it carries the
		// same CSRF requirement as the other state-changing account endpoints.
		if !csrfFromSession(w, r, accounts) {
			return
		}

		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Visibility  string `json:"visibility"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}

		created, err := repositories.Create(r.Context(), repository.CreateInput{
			// The owner is the authenticated account, never a body field: an
			// explicit request struct with an allowlist of settable fields is
			// what stops a client creating a repository in someone else's
			// namespace (docs/security/threat-model.md T-E4).
			Actor:       repository.User(user.ID, user.IsAdmin),
			OwnerID:     user.ID,
			Name:        body.Name,
			Description: body.Description,
			Visibility:  repository.Visibility(body.Visibility),
		})
		if err != nil {
			writeRepositoryValidation(w, err)
			return
		}
		writeAPIJSON(w, http.StatusCreated, false, newRepositoryResponse(created, user.Username, repository.LevelOwner))
	})

	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		row, owner, level, ok := resolveRepository(w, r, accounts, owners, repositories, repository.LevelRead)
		if !ok {
			return
		}
		// A public repository read by an anonymous caller may be cached briefly;
		// anything else is caller-dependent.
		public := row.Visibility == repository.VisibilityPublic && level == repository.LevelRead
		writeAPIJSON(w, http.StatusOK, public, newRepositoryResponse(row, owner.Username, level))
	})

	// The read surfaces below all answer the same way a rendered page would, so
	// the browser and a script cannot disagree.
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		row, _, level, ok := resolveRepository(w, r, accounts, owners, repositories, repository.LevelRead)
		if !ok {
			return
		}
		path := strings.Trim(r.PathValue("path"), "/")

		revision, ok := revisionFor(w, r, content, row)
		if !ok {
			return
		}

		// A file and a directory are distinguished by asking for the file first
		// and falling back, which is what lets one endpoint serve both shapes.
		file, fileErr := content.ReadFile(r.Context(), row.StorageID, revision, path)
		switch {
		case fileErr == nil:
			public := row.Visibility == repository.VisibilityPublic && level == repository.LevelRead
			writeAPIJSON(w, http.StatusOK, public, fileResponse{
				Name:     baseName(path),
				Path:     file.Path,
				Size:     file.Size,
				Encoding: "utf-8",
				Content:  string(file.Content),
			})
			return
		case errors.Is(fileErr, gittransport.ErrFileTooLarge):
			writeError(w, http.StatusUnprocessableEntity,
				"the file is too large to return; download it through the Git transport instead")
			return
		case !errors.Is(fileErr, gittransport.ErrNotFound) && !errors.Is(fileErr, gittransport.ErrNotAFile):
			writeContentError(w, fileErr)
			return
		}

		entries, err := content.ListTree(r.Context(), row.StorageID, revision, path)
		if err != nil {
			writeContentError(w, err)
			return
		}
		items := make([]treeEntryResponse, 0, len(entries))
		for _, entry := range entries {
			items = append(items, treeEntryResponse{Name: entry.Name, Path: entry.Path, Type: entry.Type, Size: entry.Size})
		}
		public := row.Visibility == repository.VisibilityPublic && level == repository.LevelRead
		writeAPIJSON(w, http.StatusOK, public, items)
	})

	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/commits", func(w http.ResponseWriter, r *http.Request) {
		row, _, level, ok := resolveRepository(w, r, accounts, owners, repositories, repository.LevelRead)
		if !ok {
			return
		}
		page, err := parsePagination(r)
		if err != nil {
			writeValidationError(w, err)
			return
		}
		revision, ok := revisionFor(w, r, content, row)
		if !ok {
			return
		}
		path := strings.Trim(r.URL.Query().Get("path"), "/")

		commits, err := content.Log(r.Context(), row.StorageID, revision, path, page.perPage, page.offset())
		if err != nil {
			writeContentError(w, err)
			return
		}
		items := make([]commitResponse, 0, len(commits))
		for _, commit := range commits {
			items = append(items, commitResponse{
				SHA: commit.ID, Message: commit.Message, AuthorName: commit.AuthorName,
				AuthorEmail: commit.AuthorEmail, AuthoredAt: commit.AuthoredAt,
			})
		}
		// A full page implies there may be more, which is the same
		// extra-row-free heuristic the listing uses: the reader asked for exactly
		// one page and cannot see beyond it without running another command.
		if link := linkHeader(r, page, len(items) == page.perPage); link != "" {
			w.Header().Set("Link", link)
		}
		public := row.Visibility == repository.VisibilityPublic && level == repository.LevelRead
		writeAPIJSON(w, http.StatusOK, public, items)
	})
}

// RegisterRepositories adds the repository endpoints to mux.
func RegisterRepositories(mux *http.ServeMux, accounts AccountService, owners RepositoryOwnerLookup, repositories RepositoryService, content ContentReader) {
	repositoryRoutes(mux, accounts, owners, repositories, content)
}

// resolveRepository turns a path into a repository, enforcing a level.
//
// Every failure — an unknown owner, a malformed name, a repository that does not
// exist, and one the caller may not read — produces the same not-found response,
// so a private repository's existence stays private.
func resolveRepository(
	w http.ResponseWriter, r *http.Request, accounts AccountService,
	owners RepositoryOwnerLookup, repositories RepositoryService, required repository.Level,
) (repository.Repository, identity.User, repository.Level, bool) {
	ownerName := r.PathValue("owner")
	name := r.PathValue("repo")

	// The name is validated before anything is looked up, so a malformed name
	// costs no query and cannot reach a path.
	if err := repository.ValidateName(name); err != nil {
		writeAPINotFound(w)
		return repository.Repository{}, identity.User{}, repository.LevelNone, false
	}

	owner, err := owners.OwnerByUsername(r.Context(), ownerName)
	if err != nil {
		if !errors.Is(err, identity.ErrInvalidCredentials) {
			writeDomainError(w, err)
			return repository.Repository{}, identity.User{}, repository.LevelNone, false
		}
		writeAPINotFound(w)
		return repository.Repository{}, identity.User{}, repository.LevelNone, false
	}

	user := viewer(r, accounts)
	principal := repository.Principal{}
	if user.ID != "" {
		principal = repository.User(user.ID, user.IsAdmin)
	}

	row, level, err := repositories.Resolve(r.Context(), principal, owner.ID, name)
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			writeDomainError(w, err)
			return repository.Repository{}, identity.User{}, repository.LevelNone, false
		}
		writeAPINotFound(w)
		return repository.Repository{}, identity.User{}, repository.LevelNone, false
	}
	if !level.Allows(required) {
		// Indistinguishable from absence, for the same reason.
		writeAPINotFound(w)
		return repository.Repository{}, identity.User{}, repository.LevelNone, false
	}
	return row, owner, level, true
}

// revisionFor resolves the revision a request asks for.
//
// An absent revision means the repository's default branch, which is what a
// browse request without a ref wants. A repository with no default branch has no
// commits, which is reported as such rather than as a missing revision.
func revisionFor(w http.ResponseWriter, r *http.Request, content ContentReader, row repository.Repository) (string, bool) {
	revision := strings.TrimSpace(r.URL.Query().Get("ref"))
	if revision == "" {
		if row.DefaultBranch == "" {
			writeError(w, http.StatusNotFound, "the repository has no commits yet")
			return "", false
		}
		revision = row.DefaultBranch
	}
	resolved, err := content.ResolveCommit(r.Context(), row.StorageID, revision)
	if err != nil {
		if errors.Is(err, gittransport.ErrEmptyRepository) {
			writeError(w, http.StatusNotFound, "the repository has no commits yet")
			return "", false
		}
		writeContentError(w, err)
		return "", false
	}
	return resolved, true
}

// writeContentError maps a content failure onto a response.
//
// An unknown revision and an absent path are both not-found, and a validation
// failure is the caller's request rather than a server fault.
func writeContentError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, gittransport.ErrNotFound), errors.Is(err, gittransport.ErrNotAFile):
		writeAPINotFound(w)
	case errors.Is(err, gittransport.ErrFileTooLarge):
		writeError(w, http.StatusUnprocessableEntity, "the file is too large to return")
	default:
		// Validation errors from the reader carry their own message; anything
		// else is reported generically so a path never reaches a client.
		message := err.Error()
		if strings.Contains(message, "is not valid") || strings.Contains(message, "must not") ||
			strings.Contains(message, "must be") || strings.Contains(message, "is longer than") {
			writeValidationError(w, err)
			return
		}
		writeError(w, http.StatusInternalServerError, "the request could not be completed")
	}
}

// writeRepositoryValidation maps a repository failure onto a response.
func writeRepositoryValidation(w http.ResponseWriter, err error) {
	if status, _ := statusFor(err); status != 0 {
		writeDomainError(w, err)
		return
	}
	if errors.Is(err, repository.ErrNameTaken) {
		writeError(w, http.StatusConflict, err.Error(), FieldError{
			Resource: "repository", Field: "name", Code: "already_exists", Message: err.Error(),
		})
		return
	}
	// A name or description that does not satisfy the rules is the caller's
	// input, so it is a field-level validation failure.
	field := "name"
	if strings.Contains(strings.ToLower(err.Error()), "description") {
		field = "description"
	}
	writeError(w, http.StatusUnprocessableEntity, err.Error(), FieldError{
		Resource: "repository", Field: field, Code: "invalid", Message: err.Error(),
	})
}

// levelFor returns the caller's level on one repository of a listing.
//
// A listing row already passed the visibility predicate, so asking again costs
// one lookup and gives the response the level it reports.
func levelFor(ctx context.Context, repositories RepositoryService, user identity.User, row repository.Repository) (repository.Level, error) {
	principal := repository.Principal{}
	if user.ID != "" {
		principal = repository.User(user.ID, user.IsAdmin)
	}
	_, level, err := repositories.Resolve(ctx, principal, row.OwnerID, row.Name)
	if err != nil {
		// A row the listing returned but the resolve refuses cannot happen,
		// because both use the same predicate; reporting no access rather than
		// failing keeps the listing useful if they ever disagree.
		if errors.Is(err, repository.ErrNotFound) {
			return repository.LevelNone, nil
		}
		return repository.LevelNone, err
	}
	return level, nil
}

// baseName returns the final path component.
func baseName(path string) string {
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[index+1:]
	}
	return path
}

// csrfFromSession verifies the CSRF token bound to the caller's session.
//
// It resolves the session again rather than trusting the viewer, because the CSRF
// digest belongs to the session row and the session is what proves the caller is
// the person the browser signed in as.
func csrfFromSession(w http.ResponseWriter, r *http.Request, accounts AccountService) bool {
	token := accessToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return false
	}
	_, _, csrfDigest, err := accounts.ResolveSession(r.Context(), token)
	if err != nil {
		writeDomainError(w, err)
		return false
	}
	return requireCSRF(w, r, csrfDigest)
}
