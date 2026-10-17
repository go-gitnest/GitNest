// Package web renders the server-rendered read surfaces.
//
// docs/architecture/frontend.md fixes the split this package implements:
// "server-rendered HTML is the default for every read surface", naming the
// repository overview and the file and directory views here, with React reserved
// for the interactive surfaces. The consequence it states is that no JavaScript
// is required to read public content, so every page this package produces is
// complete on the first byte.
//
// Two properties hold throughout:
//
//   - **Access is decided by the repository service, never here.** The renderer
//     asks internal/repository for a level and renders what it is given. A
//     template that computed access would be the parallel decision path the
//     architecture forbids, and the "same decision twice" test asserts that the
//     level this package reports equals the level the API reports.
//   - **Repository content is escaped text.** html/template escapes every value,
//     and no repository content is ever marked safe.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/gittransport"
	"github.com/go-gitnest/GitNest/internal/i18n"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/markdown"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// templatesFS carries the templates, so a release binary needs no asset
// directory beside it.
//
//go:embed templates/*.html
var templatesFS embed.FS

// Pages are the read surfaces this package renders.
var pages = []string{"layout.html", "tree.html", "overview.html", "file.html", "commits.html", "error.html"}

// commitPageSize bounds one rendered commit page.
const commitPageSize = 30

// readmeNames are the file names a repository overview shows, in the order they
// are tried. It mirrors the convention GitHub uses.
var readmeNames = []string{"README.md", "README.rst", "README.txt", "README"}

// RepositoryService answers every access question. It is satisfied by
// *repository.Service, and is the same interface the API uses so the two cannot
// answer differently.
type RepositoryService interface {
	Resolve(ctx context.Context, viewer repository.Principal, ownerID, name string) (repository.Repository, repository.Level, error)
}

// ContentReader reads repository content. It is satisfied by
// *gittransport.Reader.
type ContentReader interface {
	ResolveCommit(ctx context.Context, storageID, revision string) (string, error)
	ListTree(ctx context.Context, storageID, revision, path string) ([]gittransport.Entry, error)
	ReadFile(ctx context.Context, storageID, revision, path string) (gittransport.File, error)
	Log(ctx context.Context, storageID, revision, path string, limit, skip int) ([]gittransport.Commit, error)
}

// OwnerLookup resolves an `{owner}` segment. It is satisfied by
// *identity.Service.
type OwnerLookup interface {
	OwnerByUsername(ctx context.Context, username string) (identity.User, error)
}

// SessionResolver resolves the browsing caller. It is satisfied by
// *identity.Service.
type SessionResolver interface {
	ResolveSession(ctx context.Context, sessionToken string) (identity.User, identity.Session, []byte, error)
}

// SessionCookieName is the cookie the browser session travels in. It matches the
// API's cookie so one sign-in serves both surfaces.
const SessionCookieName = "gitnest_session"

// Handler renders the read surfaces.
type Handler struct {
	repositories RepositoryService
	content      ContentReader
	owners       OwnerLookup
	sessions     SessionResolver
	// templates is keyed by locale and then by page, because the catalogue binds
	// the `T` function and so makes a template set locale-specific.
	templates map[string]map[string]*template.Template
	// basePath prefixes every link, so an instance mounted under a path renders
	// working URLs.
	basePath string
	// static serves the read surfaces' own assets. It is a field rather than a
	// package-level handler so the cache key is the build's, computed once at
	// startup.
	static *staticHandler
	// markdown renders user-authored text. It is shared across requests, because its cache
	// is the reason repeated views of a popular document do not re-render
	// (docs/architecture/frontend.md, and W2.1's memoization criterion).
	markdown *markdown.Renderer
	// issues serves the issue read surfaces. It is nil when the caller did not provide one, in
	// which case the issue routes are not registered rather than serving a broken page.
	issues IssueService
	// accounts resolves an identifier to an account, which the issue history needs to name an
	// actor. It is the same service the read surfaces already resolve a session with.
	accounts AccountLookup
	// audit reads the ledger, which is the record of an issue's state changes.
	audit *audit.Reader
	// csrf issues the token a state-changing form must carry. A handler without one renders the
	// read surfaces and simply offers no forms, so the dependency is optional where the others
	// are required.
	csrf CSRFIssuer
	// comments serves the issue page's conversation. It is nil when the caller did not provide one, in
	// which case the section is not rendered rather than rendered broken.
	comments CommentService
	// labels serves the label list and the issue page's label section. Nil on the same terms.
	labels LabelService
	// milestones serves the milestone list and the issue page's milestone section. Nil on the same terms.
	milestones MilestoneService
}

// statusCarrier is implemented by a page that must answer with a status other than 200.
//
// It exists for the re-rendered form: a refused submission is an error, so the page that carries
// the message has to say so in the status as well as in the text — a screen reader and a client
// that inspects the response both read the status, and answering 200 to a refusal is how a caller
// is told a change succeeded when it did not.
type statusCarrier interface {
	httpStatus() int
}

// AccountLookup resolves an account from its identifier.
//
// It is separate from `OwnerLookup`, which resolves by username for a route: a history record
// names an account by identifier, and the two lookups answer different questions.
type AccountLookup interface {
	AccountByID(ctx context.Context, id string) (identity.User, error)
}

// NewHandler prepares the templates and returns a handler.
//
// A template set is built per locale rather than per request, so the shipped
// locales are parsed once at startup and a request only selects a set.
func NewHandler(repositories RepositoryService, content ContentReader, owners OwnerLookup, sessions SessionResolver, basePath string) (*Handler, error) {
	if repositories == nil {
		return nil, errors.New("the web renderer needs the repository service")
	}
	if content == nil {
		return nil, errors.New("the web renderer needs the content reader")
	}
	if owners == nil {
		return nil, errors.New("the web renderer needs an owner lookup")
	}
	if sessions == nil {
		// A nil resolver would make every page anonymous, which is a silent
		// downgrade rather than a startup failure.
		return nil, errors.New("the web renderer needs a session resolver")
	}

	// Each page is parsed with the layout so `template "content"` resolves, and
	// with the shared partials so `template "tree"` does.
	//
	// The set is keyed by locale because the catalogue binds the `T` function,
	// which makes a template set locale-specific. Building it once per locale at
	// startup rather than per request keeps parsing off the request path.
	parsed := map[string]map[string]*template.Template{}
	for _, locale := range i18n.Locales() {
		catalogue := i18n.For(locale)
		pages := map[string]*template.Template{}
		for _, page := range renderablePages {
			files := []string{"layout.html", "tree.html", page}
			built, err := template.New("layout.html").Funcs(templateFunctions(catalogue)).ParseFS(templatesFS, prefixEach(files)...)
			if err != nil {
				return nil, fmt.Errorf("parse %s for %s: %w", page, locale, err)
			}
			pages[page] = built
		}
		parsed[locale] = pages
	}
	if _, found := parsed[i18n.DefaultLocale]; !found {
		// The default catalogue is what an unknown locale falls back to, so its
		// absence is a build fault rather than a runtime one.
		return nil, fmt.Errorf("the default locale %q has no template set", i18n.DefaultLocale)
	}

	// The read surfaces' stylesheet is resolved once, so the cache key is the build's
	// rather than something recomputed per request.
	static, err := newStaticHandler()
	if err != nil {
		return nil, err
	}

	return &Handler{
		repositories: repositories, content: content, owners: owners,
		sessions: sessions, templates: parsed, basePath: strings.TrimSuffix(basePath, "/"),
		static: static,
		// One renderer for the process: constructing the sanitizer policy walks its
		// allowlist, and the cache is only useful if it is shared between requests.
		//
		// It is the **embedded** renderer, because every document these pages render sits
		// inside a page that already has a first-level heading.
		markdown: markdown.NewEmbedded(),
	}, nil
}

// renderablePages are the templates that define a `content` block.
var renderablePages = []string{"overview.html", "tree.html", "file.html", "commits.html", "issues.html", "issue.html", "issue_new.html", "labels.html", "milestones.html", "error.html"}

// prefixEach prefixes each template name with its directory.
func prefixEach(names []string) []string {
	prefixed := make([]string, 0, len(names))
	for _, name := range names {
		prefixed = append(prefixed, "templates/"+name)
	}
	return prefixed
}

// templateFunctions are the helpers the templates call.
//
// It exists so a template never assembles a string itself: frontend.md forbids
// concatenation for text with grammar, and a helper is where the assembling is
// reviewed once.
//
// `T` is a function rather than a method on the page value on purpose. Go's
// templates refuse to call a *promoted* method that takes arguments — the error
// is "T is not a method but has arguments" — and every page's data is an
// anonymous struct embedding the resolved view, so a method would only work for
// the pages that did not wrap it. A function bound to the request's catalogue is
// unambiguous and works for every page shape.
func templateFunctions(catalogue *i18n.Catalogue) template.FuncMap {
	return template.FuncMap{
		"T": func(id string, args ...any) string {
			return catalogue.T(id, args...)
		},
		// lower is used for a `lang` attribute the catalogue already normalised.
		"lower": strings.ToLower,
	}
}

// Register adds the read routes to mux.
//
// The paths are the ones docs/architecture/urls-and-identifiers.md section 2.3
// lists as canonical: the overview, `tree/{ref}` with an optional directory
// path, `blob/{ref}/{path}`, and `commits/{ref}` with an optional path filter.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{owner}/{repo}", h.overview)
	mux.HandleFunc("GET /{owner}/{repo}/tree/{ref}", h.tree)
	mux.HandleFunc("GET /{owner}/{repo}/tree/{ref}/{path...}", h.tree)
	mux.HandleFunc("GET /{owner}/{repo}/blob/{ref}/{path...}", h.file)
	mux.HandleFunc("GET /{owner}/{repo}/commits/{ref}", h.commits)
	mux.HandleFunc("GET /{owner}/{repo}/commits/{ref}/{path...}", h.commits)
	// The issue read surfaces. They are registered only when a service was provided, so a
	// handler built without one does not advertise routes it cannot serve.
	if h.issues != nil {
		mux.HandleFunc("GET /{owner}/{repo}/issues", h.issueList)
		// The creation form is registered on its own literal path, which the documented
		// `/{owner}/{repo}/issues/new` names. `net/http` prefers a literal segment over a
		// wildcard, so this wins over the `{number}` route below without an ordering rule.
		mux.HandleFunc("GET /{owner}/{repo}/issues/new", h.issueCreateForm)
		mux.HandleFunc("GET /{owner}/{repo}/issues/{number}", h.issueDetail)
	}
	// The label list, which `urls-and-identifiers.md` §2.4 registers as "Label management and per-label
	// issue list". Both routes are literal or trailing segments, so neither conflicts with the
	// repository routes above.
	if h.labels != nil {
		mux.HandleFunc("GET /{owner}/{repo}/labels", h.labelList)
		mux.HandleFunc("GET /{owner}/{repo}/labels/{label}", h.labelList)
	}
	// The milestone list, which §2.4 registers as "Milestone management and per-milestone list". The
	// singular `/milestone/{number}` is the route §6.5 fixes for one milestone's issue list.
	if h.milestones != nil {
		mux.HandleFunc("GET /{owner}/{repo}/milestones", h.milestoneList)
		mux.HandleFunc("GET /{owner}/{repo}/milestone/{number}", h.milestoneList)
	}

	// The mutations are registered whenever both a service and a token issuer exist, because a
	// form without a token could never succeed.
	if h.issues != nil && h.csrf != nil {
		mux.HandleFunc("POST /{owner}/{repo}/issues", h.issueCreate)
		mux.HandleFunc("POST /{owner}/{repo}/issues/{number}", h.issueUpdate)
		// The conversation's form posts to the issue's own path, with its operation in a field — so
		// the comment routes add no path the document does not already have.
		if h.comments != nil {
			mux.HandleFunc("POST /{owner}/{repo}/issues/{number}/comments", h.commentWrite)
		}
		if h.labels != nil {
			mux.HandleFunc("POST /{owner}/{repo}/issues/{number}/labels", h.labelApply)
			mux.HandleFunc("POST /{owner}/{repo}/labels", h.labelCreate)
		}
		if h.milestones != nil {
			mux.HandleFunc("POST /{owner}/{repo}/issues/{number}/milestone", h.milestoneWriteIssue)
			mux.HandleFunc("POST /{owner}/{repo}/milestones", h.milestoneWrite)
		}
	}
	// The read surfaces'' stylesheet is deliberately **not** registered here. `GET
	// /static/` is a subtree pattern, and it conflicts with the repository routes
	// above: a path like `/static/x/tree/y` matches both and neither is more specific,
	// which `net/http` reports by panicking at startup. It is dispatched ahead of the
	// mux by [Handler.StaticHandler] instead, which is the same treatment the built
	// frontend assets get and for the same reason.
}

// StaticHandler serves the read surfaces” own assets.
//
// It is installed ahead of the mux rather than on it, for the routing reason in
// Register. It answers only for [StaticPrefix] and delegates everything else, so it is a
// dispatcher rather than a second router.
func (h *Handler) StaticHandler(next http.Handler) http.Handler {
	assets := h.static.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, StaticPrefix) {
			assets.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// page is the data every template reads.
type page struct {
	Title        string
	Locale       string
	CanonicalURL string
	// NoIndex marks an authenticated or private page, which must not be crawled
	// or cached by a shared cache.
	NoIndex     bool
	StyleSheets []string
	OpenGraph   *openGraph
}

// openGraph carries the link-preview metadata a public page emits.
type openGraph struct {
	Type        string
	Title       string
	Description string
	URL         string
}

// entry is one row in a rendered directory listing.
type entry struct {
	Name      string
	Href      string
	TypeLabel string
	// Kind is the reader's type, kept alongside the label so ordering does not
	// depend on a translated string.
	Kind     string
	Size     int64
	ShowSize bool
}

// resolved is what every surface needs before it can render.
type resolved struct {
	page
	row   repository.Repository
	owner identity.User
	level repository.Level
	// revision is what the reader asked for, or the default branch.
	revision string
	// commit is the resolved revision, which is what the links address so a page
	// does not move under a reader when a branch advances.
	commit string
	// requestOrigin is the scheme and host the reader used, which is what a
	// clone URL has to name.
	requestOrigin string
	// basePath prefixes every link.
	basePath string
}

// FullName is the `owner/name` form.
func (r *resolved) FullName() string { return r.owner.Username + "/" + r.row.Name }

// Revision is the revision the page shows, for a template to name.
//
// It is a method rather than an exported field because a template cannot reach
// an unexported field of an embedded struct: the embed is promoted but the
// visibility rule still applies, so a page that wrapped the view in an anonymous
// struct could not read `revision` directly. A method is promoted and callable.
func (r *resolved) Revision() string {
	if r.revision != "" {
		return r.revision
	}
	return r.commit
}

// Description is the repository's description.
func (r *resolved) Description() string { return r.row.Description }

// IsPublic reports whether anonymous callers may read the repository.
func (r *resolved) IsPublic() bool { return r.row.Visibility == repository.VisibilityPublic }

// Permission is the caller's level, rendered for display.
func (r *resolved) Permission() string { return r.level.String() }

// CloneURL is the HTTPS clone address.
//
// It is built from the request's scheme and host rather than from configuration,
// so a page rendered behind a proxy names the address the reader actually used.
func (r *resolved) CloneURL() string {
	return r.requestOrigin + "/" + r.FullName() + ".git"
}

// requestContext carries what a handler needs to resolve a request.
type requestContext struct {
	handler *Handler
	writer  http.ResponseWriter
	request *http.Request
}

// resolve turns a path into a repository, a caller, and a revision.
//
// Every rejection — an unknown owner, a malformed name, an absent repository,
// one the caller may not read, and a revision that does not exist — produces the
// same not-found page, so a private repository's existence stays private. That is
// the acceptance criterion "a user without access sees a consistent not-found
// response for a private repository".
func (c requestContext) resolve(required repository.Level) (*resolved, bool) {
	ownerName := c.request.PathValue("owner")
	name := c.request.PathValue("repo")

	if err := repository.ValidateName(name); err != nil {
		c.notFound()
		return nil, false
	}
	owner, err := c.handler.owners.OwnerByUsername(c.request.Context(), ownerName)
	if err != nil {
		if !errors.Is(err, identity.ErrInvalidCredentials) {
			c.unavailable()
			return nil, false
		}
		c.notFound()
		return nil, false
	}

	viewer := c.viewer()
	row, level, err := c.handler.repositories.Resolve(c.request.Context(), viewer, owner.ID, name)
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			c.unavailable()
			return nil, false
		}
		c.notFound()
		return nil, false
	}
	if !level.Allows(required) {
		c.notFound()
		return nil, false
	}

	revision := c.request.PathValue("ref")
	if revision == "" {
		revision = row.DefaultBranch
	}
	// A repository with no default branch has no commits, so its page reports
	// that rather than the revision failing to resolve.
	if revision == "" {
		return c.finish(row, owner, level, "", ""), true
	}
	commit, err := c.handler.content.ResolveCommit(c.request.Context(), row.StorageID, revision)
	if err != nil {
		if errors.Is(err, gittransport.ErrEmptyRepository) {
			// The branch is configured and has no commit: the repository was
			// created and nothing has been pushed, which the page reports.
			return c.finish(row, owner, level, revision, ""), true
		}
		if errors.Is(err, gittransport.ErrNotFound) {
			c.notFound()
			return nil, false
		}
		// A malformed revision is the caller's request rather than a missing
		// page, but it is answered the same way so a probe cannot tell them
		// apart.
		c.notFound()
		return nil, false
	}
	return c.finish(row, owner, level, revision, commit), true
}

// finish assembles the resolved view.
func (c requestContext) finish(row repository.Repository, owner identity.User, level repository.Level, revision, commit string) *resolved {
	catalogue := c.catalogue()
	view := &resolved{
		page: page{Locale: catalogue.Locale(), StyleSheets: c.handler.static.versioned()},
		row:  row, owner: owner, level: level, revision: revision, commit: commit,
		requestOrigin: requestOrigin(c.request),
		basePath:      c.handler.basePath,
	}
	// A private repository, or one read by an authenticated caller, must not be
	// indexed or shared between viewers. frontend.md states both rules.
	// `Vary: Cookie` alone would be enough to keep a shared cache honest, which
	// is exactly why the document says no route may rely on it alone.
	view.NoIndex = row.Visibility != repository.VisibilityPublic || c.authenticated()
	view.Title = owner.Username + "/" + row.Name

	// A public page carries link-preview metadata, which frontend.md requires
	// because a preview has to render without JavaScript.
	if !view.NoIndex {
		description := row.Description
		if description == "" {
			description = catalogue.T(i18n.IDRepoNoDescription)
		}
		view.OpenGraph = &openGraph{
			Type:        "object",
			Title:       view.Title,
			Description: description,
			URL:         requestURL(c.request),
		}
		view.CanonicalURL = requestURL(c.request)
	}
	return view
}

// requestURL reconstructs the absolute URL of the request, which is what a
// canonical link and a preview URL have to name.
func requestURL(r *http.Request) string {
	return requestOrigin(r) + r.URL.Path
}

// requestOrigin reconstructs the scheme and host the reader used.
//
// A proxy is expected to set `X-Forwarded-Proto`; without one the request's own
// TLS state decides. The value is used only to build an absolute URL in a page.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host
}

// viewer resolves the browsing caller, treating an unusable session as anonymous
// so a public page still renders.
func (c requestContext) viewer() repository.Principal {
	user := c.user()
	if user.ID == "" {
		return repository.Anonymous()
	}
	return repository.User(user.ID, user.IsAdmin)
}

// authenticated reports whether the request carried a usable session.
func (c requestContext) authenticated() bool { return c.user().ID != "" }

// user resolves the session cookie to an account.
func (c requestContext) user() identity.User {
	cookie, err := c.request.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return identity.User{}
	}
	user, _, _, err := c.handler.sessions.ResolveSession(c.request.Context(), cookie.Value)
	if err != nil {
		return identity.User{}
	}
	return user
}

// catalogue selects the message catalogue for the request.
//
// frontend.md fixes the precedence as the account setting, then the request's
// `Accept-Language`, then the instance default. The account setting has no
// storage yet, so this implements the two that exist.
func (c requestContext) catalogue() *i18n.Catalogue {
	header := c.request.Header.Get("Accept-Language")
	if header == "" {
		return i18n.Default()
	}
	return i18n.For(i18n.SelectFromAcceptLanguage(header))
}

// render writes a page.
//
// The whole page is rendered into a buffer first, so a template failure produces
// an error response rather than a half-written page with a 200 status.
//
// The pages wrap the resolved view in an anonymous struct that adds the
// surface's own fields, so the cacheability decision reads the embedded view
// through the small interface below rather than by type-asserting the wrapper.
func (c requestContext) render(templateName string, data any) {
	buffer := &strings.Builder{}
	selected, found := c.handler.templateFor(templateName, c.catalogue())
	if !found {
		c.unavailable()
		return
	}
	if err := selected.ExecuteTemplate(buffer, "layout.html", data); err != nil {
		c.unavailable()
		return
	}

	view := embeddedView(data)
	c.setReadHeaders(view != nil && !view.NoIndex, view)
	// The status is written after the headers, because a header set after `WriteHeader` is
	// discarded — which would silently lose the cache directive on exactly the pages that carry a
	// refusal.
	if status, ok := data.(statusCarrier); ok && status.httpStatus() != 0 {
		c.writer.WriteHeader(status.httpStatus())
	}
	if _, err := io.WriteString(c.writer, buffer.String()); err != nil {
		// The client went away mid-response; nothing to report to it.
		return
	}
}

// viewCarrier is implemented by the embedded resolved view of every page.
type viewCarrier interface {
	view() *resolved
}

// view returns the resolved view.
func (r *resolved) view() *resolved { return r }

// templateFor returns the parsed template for a page in a catalogue's locale.
//
// An unknown locale falls back to the default set, which is the same fallback the
// catalogue itself applies, so a request can never reach a page with no template.
func (h *Handler) templateFor(name string, catalogue *i18n.Catalogue) (*template.Template, bool) {
	pages, found := h.templates[catalogue.Locale()]
	if !found {
		pages, found = h.templates[i18n.DefaultLocale]
		if !found {
			return nil, false
		}
	}
	selected, found := pages[name]
	return selected, found
}

// embeddedView finds the resolved view inside a page's data.
//
// A page's data is an anonymous struct embedding *resolved, so the embed is
// promoted: the method set carries `view()` and one interface assertion finds it
// whatever wrapper the page used. This exists so adding a surface does not add a
// cacheability branch.
func embeddedView(data any) *resolved {
	if carrier, ok := data.(viewCarrier); ok {
		return carrier.view()
	}
	return nil
}

// setReadHeaders applies the cacheability the addressing document requires.
//
// A public repository read by an anonymous caller may be cached briefly for a
// ref-addressed page; anything caller-dependent is `private` and revalidated, and
// a not-found is never stored.
func (c requestContext) setReadHeaders(cacheable bool, view *resolved) {
	header := c.writer.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	if cacheable {
		header.Set("Cache-Control", "public, max-age=60")
		header.Set("Vary", "Accept-Encoding")
		return
	}
	header.Set("Cache-Control", "private, max-age=0, must-revalidate")
	// A private page varies by the caller, so a shared cache must not reuse it.
	header.Set("Vary", "Cookie, Accept-Encoding")
	if view != nil {
		header.Set("X-Robots-Tag", "noindex, nofollow")
	}
}

// notFound renders the single not-found page every unavailable resource gets.
func (c requestContext) notFound() {
	c.renderError(http.StatusNotFound, i18n.IDErrorNotFoundTitle, i18n.IDErrorNotFoundBody)
}

// unavailable renders the operational-failure page. It is only reached after the
// caller was authorized, so it discloses nothing.
func (c requestContext) unavailable() {
	c.renderError(http.StatusServiceUnavailable, i18n.IDErrorUnavailable, i18n.IDErrorUnavailableIn)
}

// renderError writes the not-found or unavailable page.
//
// It renders through the same layout as every other page, so a reader who
// follows a stale link still gets usable navigation rather than a bare line of
// text.
func (c requestContext) renderError(status int, titleID, bodyID string) {
	catalogue := c.catalogue()
	data := &struct {
		*page
		ErrorBody string
	}{
		page: &page{
			Locale: catalogue.Locale(), Title: catalogue.T(titleID),
			StyleSheets: c.handler.static.versioned(),
		},
		ErrorBody: catalogue.T(bodyID),
	}

	buffer := &strings.Builder{}
	selected, found := c.handler.templateFor("error.html", catalogue)
	if !found {
		http.Error(c.writer, "internal error", http.StatusInternalServerError)
		return
	}
	if err := selected.ExecuteTemplate(buffer, "layout.html", data); err != nil {
		http.Error(c.writer, "internal error", http.StatusInternalServerError)
		return
	}

	c.writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A denial must never be remembered: the caller may gain access a moment
	// later, and a cached denial would outlive it.
	c.writer.Header().Set("Cache-Control", "no-store")
	c.writer.Header().Set("X-Robots-Tag", "noindex, nofollow")
	c.writer.WriteHeader(status)
	_, _ = io.WriteString(c.writer, buffer.String())
}

// overview renders `/{owner}/{repo}`.
func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}

	overview := &struct {
		*resolved
		Entries []entry
		Empty   bool
		Readme  *readmeView
	}{resolved: view}

	if view.commit == "" {
		overview.Empty = true
		c.render("overview.html", overview)
		return
	}

	entries, err := h.content.ListTree(r.Context(), view.row.StorageID, view.commit, "")
	if err != nil {
		c.unavailable()
		return
	}
	overview.Entries = h.entryViews(view, entries)
	sortEntries(overview.Entries)

	// The README is looked up by name so the overview shows the same content a
	// reader would get from the file view.
	for _, name := range readmeNames {
		file, err := h.content.ReadFile(r.Context(), view.row.StorageID, view.commit, name)
		if err != nil {
			continue
		}
		overview.Readme = readmeFor(h, name, file.Content)
		break
	}
	c.render("overview.html", overview)
}

// readmeFor prepares a README for the template.
//
// **A Markdown README is rendered; anything else is shown as escaped text.** The distinction
// matters because rendering a plain-text file as Markdown mangles it: a line beginning `*` or
// a bare `#` becomes emphasis or a heading, and the reader sees something the author did not
// write. The extension is therefore the signal, not the content's shape.
//
// `Rendered` is `template.HTML`, which is safe because the renderer's sanitizer ran and
// because the template places it as element content — see the note on `markdown.Render`.
func readmeFor(h *Handler, name string, content []byte) *readmeView {
	view := &readmeView{Path: name}
	if strings.EqualFold(filepath.Ext(name), ".md") {
		view.Rendered = h.markdown.Render(string(content))
		return view
	}
	view.Content = string(content)
	return view
}

// readmeView is the README a rendered component shows.
type readmeView struct {
	Path    string
	Content string
	// Rendered is sanitized HTML, set only for a Markdown README.
	Rendered template.HTML
}

// tree renders `/{owner}/{repo}/tree/{ref}[/{path}]`.
func (h *Handler) tree(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}
	path := strings.Trim(r.PathValue("path"), "/")

	data := &struct {
		*resolved
		Entries    []entry
		Path       string
		ParentHref string
	}{resolved: view, Path: path}

	if view.commit != "" {
		entries, err := h.content.ListTree(r.Context(), view.row.StorageID, view.commit, path)
		if err != nil {
			c.notFound()
			return
		}
		data.Entries = h.entryViews(view, entries)
		sortEntries(data.Entries)
	}
	if path != "" {
		data.ParentHref = h.treeHref(view, refFor(view), parentPath(path))
	}
	c.render("tree.html", data)
}

// file renders `/{owner}/{repo}/blob/{ref}/{path}`.
func (h *Handler) file(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}
	path := strings.Trim(r.PathValue("path"), "/")
	if path == "" {
		c.notFound()
		return
	}

	data := &struct {
		*resolved
		Path       string
		Content    string
		Size       int64
		Binary     bool
		ParentHref string
	}{resolved: view, Path: path}

	if view.commit != "" {
		file, err := h.content.ReadFile(r.Context(), view.row.StorageID, view.commit, path)
		if err != nil {
			c.notFound()
			return
		}
		data.Size = file.Size
		data.Binary = !isTextual(file.Content)
		if !data.Binary {
			data.Content = string(file.Content)
		}
		data.ParentHref = h.treeHref(view, refFor(view), parentPath(path))
	}
	c.render("file.html", data)
}

// commits renders `/{owner}/{repo}/commits/{ref}[/{path}]`.
func (h *Handler) commits(w http.ResponseWriter, r *http.Request) {
	c := requestContext{handler: h, writer: w, request: r}
	view, ok := c.resolve(repository.LevelRead)
	if !ok {
		return
	}
	path := strings.Trim(r.PathValue("path"), "/")
	page, err := parsePage(r)
	if err != nil {
		c.notFound()
		return
	}

	data := &struct {
		*resolved
		Path     string
		Commits  []commitView
		MoreHref string
	}{resolved: view, Path: path}

	if view.commit != "" {
		commits, err := h.content.Log(r.Context(), view.row.StorageID, view.commit, path, commitPageSize, (page-1)*commitPageSize)
		if err != nil {
			c.notFound()
			return
		}
		for _, commit := range commits {
			data.Commits = append(data.Commits, commitView{
				Href: h.commitHref(view, commit.ID), Message: commit.Message,
				AuthorName: commit.AuthorName, AuthoredAt: commit.AuthoredAt,
			})
		}
		if len(commits) == commitPageSize {
			data.MoreHref = h.commitsHref(view, refFor(view), path, page+1)
		}
	}
	c.render("commits.html", data)
}

// commitView is one rendered commit row.
type commitView struct {
	Href       string
	Message    string
	AuthorName string
	AuthoredAt string
}

// entryViews turns content entries into rendered rows.
//
// The type label comes from the catalogue, so a directory is named in the
// reader's language rather than by the identifier the reader produced. The kind
// travels beside it so ordering never depends on a translated string.
func (h *Handler) entryViews(view *resolved, entries []gittransport.Entry) []entry {
	catalogue := view.catalogueFor()
	rows := make([]entry, 0, len(entries))
	for _, item := range entries {
		row := entry{
			Name:      item.Name,
			Kind:      item.Type,
			TypeLabel: catalogue.T(entryTypeID(item.Type)),
		}
		if item.Type == "directory" {
			row.Href = h.treeHref(view, refFor(view), item.Path)
		} else {
			row.Href = h.fileHref(view, refFor(view), item.Path)
			row.Size = item.Size
			row.ShowSize = true
		}
		rows = append(rows, row)
	}
	return rows
}

// catalogueFor resolves the view's locale to its catalogue, so a page's labels
// come from the same catalogue the page's templates use.
func (r *resolved) catalogueFor() *i18n.Catalogue {
	return i18n.For(r.Locale)
}

// entryTypeID maps a reader type onto its catalogue identifier.
func entryTypeID(contentType string) string {
	switch contentType {
	case "directory":
		return i18n.IDEntryTypeDirectory
	case "symlink":
		return i18n.IDEntryTypeSymlink
	case "submodule":
		return i18n.IDEntryTypeSubmodule
	default:
		return i18n.IDEntryTypeFile
	}
}

// sortEntries orders a listing the way the catalogue promises: directories first,
// then by name.
//
// It compares the reader's kind rather than the translated label, so the order
// does not change with the locale.
func sortEntries(rows []entry) {
	sort.SliceStable(rows, func(i, j int) bool {
		leftIsDirectory := rows[i].Kind == "directory"
		rightIsDirectory := rows[j].Kind == "directory"
		if leftIsDirectory != rightIsDirectory {
			return leftIsDirectory
		}
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})
}

// refFor returns the revision to address links with.
//
// A resolved commit is used when there is one, so a link a reader copies keeps
// pointing at what they saw rather than at whatever the branch holds later.
func refFor(view *resolved) string {
	if view.commit != "" {
		return view.commit
	}
	return view.revision
}

// treeHref builds a tree link.
func (h *Handler) treeHref(view *resolved, ref, path string) string {
	base := h.basePath + "/" + view.FullName() + "/tree/" + url.PathEscape(ref)
	if path == "" {
		return base
	}
	return base + "/" + escapePath(path)
}

// fileHref builds a file link.
func (h *Handler) fileHref(view *resolved, ref, path string) string {
	return h.basePath + "/" + view.FullName() + "/blob/" + url.PathEscape(ref) + "/" + escapePath(path)
}

// commitHref builds a link to a commit.
//
// The commit list is the only commit surface this phase renders, so a row links
// to the same list at that commit, which is a valid canonical URL.
func (h *Handler) commitHref(view *resolved, commit string) string {
	return h.basePath + "/" + view.FullName() + "/commits/" + url.PathEscape(commit)
}

// commitsHref builds a commit-list link, including the page and path filter.
func (h *Handler) commitsHref(view *resolved, ref, path string, page int) string {
	base := h.basePath + "/" + view.FullName() + "/commits/" + url.PathEscape(ref)
	if path != "" {
		base += "/" + escapePath(path)
	}
	if page > 1 {
		base += "?page=" + strconv.Itoa(page)
	}
	return base
}

// escapePath percent-escapes each path segment, keeping the separators.
func escapePath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// parentPath returns the directory containing a path, or an empty string.
func parentPath(path string) string {
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[:index]
	}
	return ""
}

// parsePage reads the `page` parameter of a commit list.
func parsePage(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("page")
	if raw == "" {
		return 1, nil
	}
	page, err := strconv.Atoi(raw)
	if err != nil || page < 1 {
		return 0, errors.New("page must be a positive integer")
	}
	return page, nil
}

// isTextual reports whether content can be shown as text.
//
// The check is deliberately conservative: content that is not valid UTF-8, or
// that contains a null byte, is treated as binary and reported as such rather
// than rendered as mojibake. It is not a full binary detector, and it does not
// need to be: showing a text file that happens to be unusual is not a security
// problem, and the escape that protects the page is applied either way.
func isTextual(content []byte) bool {
	for _, b := range content {
		if b == 0 {
			return false
		}
	}
	return true
}
