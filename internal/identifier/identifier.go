// Package identifier holds the naming rules that every user-facing identifier
// shares.
//
// The rules live here rather than in each domain package because
// docs/architecture/urls-and-identifiers.md section 6.1 defines them once for
// usernames, organization logins, repository names, and the rest. Two
// implementations of the same folding or reserved-name rule would drift, which
// is the schema-and-convention drift data-model.md exists to prevent.
//
// Each domain still owns its own *grammar* (a username and a repository name
// allow different characters and lengths); only the shared parts live here.
package identifier

// reservedNames cannot be registered, because a route owns them. The list is
// docs/architecture/urls-and-identifiers.md section 6.9 verbatim. The
// dot-prefixed and traversal entries are handled by each domain's grammar
// rather than listed here.
//
// It is enforced case-insensitively, is versioned with the code, and adding a
// name never renames an existing account or repository: it only blocks new
// registrations.
var reservedNames = []string{
	// Infrastructure.
	"api", "apis", "app", "apps", "assets", "auth", "camo", "cdn", "collector",
	"graphql", "health", "hooks", "internal", "live", "metrics", "media",
	"objects", "proxy", "raw", "ready", "static", "status", "storage",
	"uploads", "v2",

	// Product surfaces.
	"about", "account", "admin", "billing", "blog", "codespaces", "contact",
	"dashboard", "discussions", "enterprise", "enterprises", "explore",
	"features", "gist", "gists", "home", "issues", "jobs", "join", "login",
	"logout", "marketplace", "new", "notifications", "organizations", "orgs",
	"packages", "pages", "pricing", "projects", "pulls", "register", "search",
	"security", "sessions", "settings", "signup", "site", "sponsors", "stars",
	"teams", "terms", "topics", "trending", "users", "watching", "wiki",
	"workspaces",

	// Git protocol.
	"git-upload-pack", "git-receive-pack", "info", "lfs",

	// GitNest-specific.
	"gitnest", "_health", "_status",
}

var reservedIndex = func() map[string]struct{} {
	index := make(map[string]struct{}, len(reservedNames))
	for _, name := range reservedNames {
		index[name] = struct{}{}
	}
	return index
}()

// Fold returns the comparison form of a name: ASCII case folding only.
//
// ASCII-only is deliberate. It makes the form independent of the database
// collation and of the locale, so the stored folded column, an index lookup, and
// a Go comparison all agree (docs/architecture/urls-and-identifiers.md
// section 6.1).
func Fold(name string) string {
	folded := []byte(name)
	for i, b := range folded {
		if b >= 'A' && b <= 'Z' {
			folded[i] = b + ('a' - 'A')
		}
	}
	return string(folded)
}

// Reserved reports whether a name is owned by a route.
func Reserved(name string) bool {
	_, found := reservedIndex[Fold(name)]
	return found
}

// ReservedNames returns the list, for documentation generation and tests that
// assert it matches the specification.
func ReservedNames() []string {
	names := make([]string, len(reservedNames))
	copy(names, reservedNames)
	return names
}
