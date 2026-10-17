package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-gitnest/GitNest/internal/identity"
)

// viewer resolves the caller of a request that a public resource serves.
//
// Unlike requireSession, a missing or unusable credential is not an error here:
// an endpoint that serves a public resource answers the request as anonymous,
// and the repository service then decides what an anonymous caller may read.
// That keeps one access answer rather than two, because the transport never
// decides for itself whether anonymous access is enough.
//
// A credential that is present but unusable is treated as absent rather than as
// a failure, so an expired session browsing public content degrades to anonymous
// instead of producing an error page.
func viewer(r *http.Request, accounts AccountService) identity.User {
	token := accessToken(r)
	if token == "" {
		return identity.User{}
	}
	user, _, _, err := accounts.ResolveSession(r.Context(), token)
	if err != nil {
		return identity.User{}
	}
	return user
}

// pagination is a parsed `page`/`per_page` pair.
type pagination struct {
	page    int
	perPage int
}

// Pagination limits from docs/architecture/urls-and-identifiers.md section 9.
const (
	defaultPerPage = 30
	maxPerPage     = 100
)

// parsePagination reads the pagination parameters.
//
// The rules are the document's: `page` is 1-based and a non-integer or a value
// below 1 is a validation error, while a `per_page` above the maximum is clamped
// rather than rejected, for GitHub compatibility.
func parsePagination(r *http.Request) (pagination, error) {
	result := pagination{page: 1, perPage: defaultPerPage}

	if raw := r.URL.Query().Get("page"); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 {
			return pagination{}, errors.New("page must be a positive integer")
		}
		result.page = page
	}
	if raw := r.URL.Query().Get("per_page"); raw != "" {
		perPage, err := strconv.Atoi(raw)
		if err != nil || perPage < 1 {
			return pagination{}, errors.New("per_page must be a positive integer")
		}
		if perPage > maxPerPage {
			perPage = maxPerPage
		}
		result.perPage = perPage
	}
	return result, nil
}

// offset is the row offset the page starts at.
func (p pagination) offset() int { return (p.page - 1) * p.perPage }

// linkHeader renders the `Link` header for a page.
//
// `last` is deliberately absent: producing it needs a total, and a total would
// have to be computed before the visibility filter or would disclose how many
// repositories a caller cannot see. The document requires a relation to be
// omitted rather than emitted empty, which is what this does.
func linkHeader(r *http.Request, page pagination, hasMore bool) string {
	base := *r.URL
	query := base.Query()

	relations := make([]string, 0, 3)
	appendRelation := func(relation string, target int) {
		query.Set("page", strconv.Itoa(target))
		base.RawQuery = query.Encode()
		relations = append(relations, "<"+base.String()+">; rel=\""+relation+"\"")
	}

	if page.page > 1 {
		appendRelation("first", 1)
		appendRelation("prev", page.page-1)
	}
	if hasMore {
		appendRelation("next", page.page+1)
	}
	if len(relations) == 0 {
		return ""
	}
	return strings.Join(relations, ", ")
}

// writeAPIJSON sends a JSON body with the cacheability the API document
// requires: a caller-dependent answer is never stored by a shared cache, and a
// public answer may be held briefly.
//
// It encodes the body itself rather than delegating to writeJSON, because that
// helper forces `no-store` and this one has a cacheability decision to make.
func writeAPIJSON(w http.ResponseWriter, status int, public bool, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if public {
		w.Header().Set("Cache-Control", "public, max-age=60")
	} else {
		w.Header().Set("Cache-Control", "private, must-revalidate")
	}
	// The answer depends on the caller, so a cache in front of the server must
	// not reuse it across viewers.
	w.Header().Set("Vary", "Accept, Accept-Encoding, Authorization")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeAPINotFound sends the single not-found response every unavailable
// repository gets.
//
// It carries `no-store` rather than the usual API cacheability, because a
// negative answer must not be remembered: the caller may gain access a moment
// later.
func writeAPINotFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	writeError(w, http.StatusNotFound, "not found")
}
