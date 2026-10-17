package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-gitnest/GitNest/internal/i18n"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// Mount points for the interactive surfaces.
//
// The paths are the ones docs/architecture/urls-and-identifiers.md section 2
// names for the surfaces `frontend.md` assigns to React: `/new` for repository
// creation and `/{owner}/{repo}/settings` for repository settings. Both are
// application routes, so the Go process owns the URL and returns the shell; the
// client router handles navigation inside the area once it is running.
const NewRepositoryPath = "/new"

// SettingsPath is the repository settings surface.
//
// `docs/architecture/urls-and-identifiers.md` section 2 lists
// `/{owner}/{repo}/settings` as "General settings: name, description, visibility,
// features, archive", and section 7 requires repository admin for the whole
// `/settings/**` subtree with a `private, no-store` cache directive. The
// permission is enforced by the API this surface calls, which is the shared
// decision point; the shell itself reveals nothing by rendering, because it
// carries no repository data beyond the two path segments the caller already
// typed.
const SettingsPath = "/{owner}/{repo}/settings"

// The areas a shell can mount.
//
// They are exported because the client bundle names the same areas, and a
// mismatch between the two would render an empty shell: the value crosses the
// boundary as a string, so it is the contract rather than a type.
const (
	// AreaCreate is the repository creation surface.
	AreaCreate = "create"
	// AreaSettings is the repository settings surface. It is declared here
	// because the boot payload's shape is shared, and it is registered by the
	// workstream that delivers the surface.
	AreaSettings = "settings"
)

// bootPayload is what the shell hands the client.
//
// It carries the session's CSRF token so a state-changing `fetch` can attach it,
// which is how the browser surface satisfies the same requirement the API
// enforces. It deliberately carries no session token: the cookie is `HttpOnly`
// precisely so a script cannot read it, and putting it here would undo that.
type bootPayload struct {
	// Locale is the negotiated locale, so the client formats dates and numbers
	// the same way the server renders them.
	Locale string `json:"locale"`
	// CSRF is the token a state-changing request must carry, absent when the
	// caller is anonymous.
	//
	// It is issued for the document being rendered rather than carried over from
	// the sign-in response, because only the token's digest is stored: the value
	// cannot be recovered later, so a page load has to be given a fresh one. That
	// is what makes a reload able to submit.
	CSRF string `json:"csrf_token,omitempty"`
	// Account is the signed-in account's name, absent when anonymous.
	Account string `json:"account,omitempty"`
	// Messages are the catalogue entries this shell needs. They are sent as
	// identifiers and text rather than as a translated document, so the client
	// reads the same namespace the server does.
	Messages map[string]string `json:"messages"`
	// Area is which surface this shell mounts, so one bundle serves both.
	Area string `json:"area"`
	// Owner and Repo name the repository a settings surface acts on. They are the
	// path the document was served from rather than anything the client derives,
	// so a settings form cannot be pointed at another repository by editing the
	// page.
	Owner string `json:"owner,omitempty"`
	Repo  string `json:"repo,omitempty"`
	// ConfirmationWord is the acknowledgement a reducing visibility transition
	// requires.
	//
	// It travels from the server because the server is the only side that decides
	// what a confirmation must be. A client that carried its own copy would
	// disagree with the server the moment the rule changed, and the reader would
	// be told to type something that does not work.
	ConfirmationWord string `json:"confirmation_word,omitempty"`
}

// shellPage is the data the shell template reads.
type shellPage struct {
	Title          string
	Locale         string
	CanonicalURL   string
	EntryScript    string
	StyleSheets    []string
	ModulePreloads []string
	// BootJSON is the payload above, already encoded, marked safe because the
	// template cannot know it is JSON inside a JSON script element.
	BootJSON template.JS
}

// AppHandler serves the interactive surfaces' shell.
type AppHandler struct {
	assets    *Assets
	sessions  SessionResolver
	csrf      CSRFIssuer
	templates map[string]*template.Template
}

// CSRFIssuer issues the token a state-changing request from a document must
// carry. It is satisfied by *identity.Service.
//
// It is a separate dependency from SessionResolver because the two answer
// different questions: one says who is calling, the other produces the token that
// proves the request came from this instance's own script. A surface that could
// only resolve a session could not render a working form.
type CSRFIssuer interface {
	RotateCSRFToken(ctx context.Context, sessionToken string) (string, error)
}

// Errors a wiring mistake produces at startup.
var (
	errNoAssets   = errors.New("the application shell needs the built assets")
	errNoSessions = errors.New("the application shell needs a session resolver")
	errNoCSRF     = errors.New("the application shell needs a CSRF issuer")
)

// NewAppHandler returns the shell handler.
//
// The template is parsed per locale, for the same reason the read surfaces are:
// the catalogue binds the `T` function, which makes a template set
// locale-specific.
func NewAppHandler(assets *Assets, sessions SessionResolver, csrf CSRFIssuer) (*AppHandler, error) {
	if assets == nil {
		return nil, errNoAssets
	}
	if sessions == nil {
		return nil, errNoSessions
	}
	if csrf == nil {
		return nil, errNoCSRF
	}

	parsed := map[string]*template.Template{}
	for _, locale := range i18n.Locales() {
		catalogue := i18n.For(locale)
		built, err := template.New("app.html").Funcs(templateFunctions(catalogue)).
			ParseFS(templatesFS, "templates/app.html")
		if err != nil {
			return nil, fmt.Errorf("parse the application shell for %s: %w", locale, err)
		}
		parsed[locale] = built
	}
	if _, found := parsed[i18n.DefaultLocale]; !found {
		return nil, fmt.Errorf("the default locale %q has no application shell", i18n.DefaultLocale)
	}

	return &AppHandler{assets: assets, sessions: sessions, csrf: csrf, templates: parsed}, nil
}

// Register adds the shell routes to mux.
//
// The built assets are deliberately **not** registered here. `GET /assets/` is a
// subtree pattern, and it conflicts with the read surfaces' own
// `GET /{owner}/{repo}/tree/{ref}/{path...}`: a path like `/assets/repo/tree/ref`
// matches both and neither is more specific, which `net/http` reports by panicking
// at startup. The assets are served by [AppHandler.AssetHandler] ahead of the mux
// instead, which is also the clearer statement — an asset prefix is not a route in
// the same space as a repository path, it is a dispatcher in front of one.
func (h *AppHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+NewRepositoryPath, h.shellFor(AreaCreate))
	mux.HandleFunc("GET "+SettingsPath, h.shellFor(AreaSettings))
}

// AssetPrefix is the path prefix the built assets are served under.
//
// `docs/architecture/urls-and-identifiers.md` lists `assets` among the reserved
// infrastructure names, so a repository cannot be called this and the prefix
// cannot shadow a real repository path.
const AssetPrefix = "/assets/"

// AssetHandler serves the built assets.
//
// It is installed ahead of the mux rather than on it, for the routing reason in
// Register. It answers only for [AssetPrefix] and delegates everything else, so it
// is a dispatcher rather than a second router.
func (h *AppHandler) AssetHandler(next http.Handler) http.Handler {
	assets := h.assets.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, AssetPrefix) {
			assets.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// shellFor renders the shell for one area.
func (h *AppHandler) shellFor(area string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		catalogue := catalogueForRequest(r)

		page := &shellPage{
			Title:        catalogue.T(i18n.IDAppName),
			Locale:       catalogue.Locale(),
			CanonicalURL: r.URL.Path,
		}

		// A shell that cannot load its module is reported plainly rather than
		// served as a blank document.
		if !h.assets.Built() {
			writeShellNotBuilt(w, catalogue)
			return
		}
		script, stylesheets, err := h.assets.Entry()
		if err != nil {
			writeShellNotBuilt(w, catalogue)
			return
		}
		page.EntryScript = script
		page.StyleSheets = stylesheets
		page.ModulePreloads = h.assets.Preloads()

		payload, err := h.bootPayload(r, catalogue, area)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		page.BootJSON = template.JS(encoded)

		selected, found := h.template(catalogue)
		if !found {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		buffer := &strings.Builder{}
		if err := selected.ExecuteTemplate(buffer, "app.html", page); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		header := w.Header()
		header.Set("Content-Type", "text/html; charset=utf-8")
		// The shell varies by the caller, because the boot payload carries a CSRF
		// token for a signed-in reader. A shared cache must not hand one viewer's
		// token to another.
		header.Set("Cache-Control", "no-cache, private")
		header.Set("Vary", "Cookie, Accept-Language, Accept-Encoding")
		_, _ = w.Write([]byte(buffer.String()))
	}
}

// bootPayload assembles what the shell hands the client.
func (h *AppHandler) bootPayload(r *http.Request, catalogue *i18n.Catalogue, area string) (bootPayload, error) {
	payload := bootPayload{
		Locale: catalogue.Locale(),
		Area:   area,
		// A settings document names the repository it belongs to, taken from the
		// path the server routed on.
		Owner: r.PathValue("owner"),
		Repo:  r.PathValue("repo"),
		// Only meaningful for the settings surface, which is the only one that can
		// change a visibility.
		ConfirmationWord: repository.VisibilityConfirmationWord,
		// The messages the client needs are the shell's own; the rest arrive
		// through the API for the surface being rendered, so the payload does not
		// grow with the application.
		Messages: shellMessages(catalogue, area),
	}

	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return payload, nil
	}
	user, _, _, err := h.sessions.ResolveSession(r.Context(), cookie.Value)
	if err != nil {
		// An unusable session renders an anonymous shell rather than an error
		// page, which is what lets a stale cookie reach the sign-in surface.
		return payload, nil
	}
	payload.Account = user.Username

	// The token is issued for this document. A failure here does not fail the
	// page: the shell renders without a token, and the form that needs one reports
	// that the request could not be prepared rather than submitting without it.
	if token, tokenErr := h.csrf.RotateCSRFToken(r.Context(), cookie.Value); tokenErr == nil {
		payload.CSRF = token
	}
	return payload, nil
}

// shellMessages are the catalogue entries the shell itself renders.
//
// They are listed per area rather than sending the whole catalogue: the client's
// strings belong to the surface it mounts, and shipping every message to every
// page is how a catalogue becomes a payload. Both sides read the same identifier
// namespace, which is what `frontend.md` requires — a message is written once and
// translated once, and `web/scripts/lint-strings.mjs` fails on a component that
// carries English of its own.
func shellMessages(catalogue *i18n.Catalogue, area string) map[string]string {
	identifiers := []string{
		i18n.IDAppLoading,
		i18n.IDAppNoScript,
		i18n.IDAppName,
		i18n.IDAppSignedInAs,
		i18n.IDAppSignOut,
		i18n.IDAppSignInNeeded,
		i18n.IDAppErrorGeneric,
		i18n.IDAppErrorNetwork,
		i18n.IDNavSkipToContent,
		i18n.IDNavRepositories,
	}

	switch area {
	case AreaSettings:
		identifiers = append(identifiers,
			i18n.IDAppSettingsTitle,
			i18n.IDAppSettingsVisibility,
			i18n.IDAppSettingsVisibilityHint,
			i18n.IDAppSettingsDefaultBranch,
			i18n.IDAppSettingsDefaultBranchHint,
			i18n.IDAppSettingsArchive,
			i18n.IDAppSettingsArchiveHint,
			i18n.IDAppSettingsConfirmation,
			i18n.IDAppSettingsConfirmationHint,
			i18n.IDAppSettingsSave,
			i18n.IDAppSettingsSaving,
			i18n.IDAppSettingsSaved,
			i18n.IDAppSettingsCollaborators,
			i18n.IDAppSettingsCollaboratorsHint,
			i18n.IDAppSettingsCollaborator,
			i18n.IDAppSettingsRole,
			i18n.IDAppSettingsAdd,
			i18n.IDAppSettingsAdding,
			i18n.IDAppSettingsRemove,
			i18n.IDAppSettingsNoCollaborators,
			i18n.IDAppSettingsArchivedNotice,
			i18n.IDAppSettingsLoadFailed,
			i18n.IDRepoVisibilityPrivate,
			i18n.IDRepoVisibilityPublic,
			i18n.IDRoleRead,
			i18n.IDRoleWrite,
			i18n.IDRoleAdmin,
		)
	case AreaCreate:
		identifiers = append(identifiers,
			i18n.IDAppCreateTitle,
			i18n.IDAppCreateName,
			i18n.IDAppCreateDescription,
			i18n.IDAppCreateVisibility,
			i18n.IDAppCreateVisibilityHint,
			i18n.IDAppCreateSubmit,
			i18n.IDAppCreateWorking,
			i18n.IDAppCreated,
			i18n.IDAppSignInTitle,
			i18n.IDAppSignInLogin,
			i18n.IDAppSignInPassword,
			i18n.IDAppSignInSubmit,
			i18n.IDAppSignInWorking,
			i18n.IDRepoVisibilityPrivate,
			i18n.IDRepoVisibilityPublic,
		)
	}

	messages := make(map[string]string, len(identifiers))
	for _, id := range identifiers {
		messages[id] = catalogue.T(id)
	}
	return messages
}

// catalogueForRequest negotiates the locale for a shell.
func catalogueForRequest(r *http.Request) *i18n.Catalogue {
	header := r.Header.Get("Accept-Language")
	if header == "" {
		return i18n.Default()
	}
	return i18n.For(i18n.SelectFromAcceptLanguage(header))
}

// template returns the parsed shell for a catalogue's locale.
func (h *AppHandler) template(catalogue *i18n.Catalogue) (*template.Template, bool) {
	selected, found := h.templates[catalogue.Locale()]
	if !found {
		selected, found = h.templates[i18n.DefaultLocale]
	}
	return selected, found
}

// writeShellNotBuilt reports a checkout whose frontend has not been built.
//
// The message names the commands to run. A blank page, or a page whose module
// 404s, would send a reader looking for a defect in the application instead of
// for the build step they skipped.
func writeShellNotBuilt(w http.ResponseWriter, catalogue *i18n.Catalogue) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("<!DOCTYPE html>\n<html lang=\"" + catalogue.Locale() + "\">\n" +
		"<head><meta charset=\"utf-8\"><title>" + catalogue.T(i18n.IDAppNotBuilt) + "</title></head>\n" +
		"<body><h1>" + catalogue.T(i18n.IDAppNotBuilt) + "</h1>" +
		"<p>" + catalogue.T(i18n.IDAppNotBuiltHow) + "</p></body></html>\n"))
}
