// Package git serves the smart HTTP Git protocol.
//
// It is a thin adapter. Every access question is answered by
// internal/repository, and every Git process is started by internal/platform/git
// under its bounds; this package decides only how to shape an HTTP response and
// which service verb a route asks for. A transport that inspects a visibility
// value, a role, or a token scope to answer an access question is a defect
// (docs/security/threat-model.md T-A3).
//
// # Routes
//
//	GET  /{owner}/{repo}.git/info/refs?service=git-upload-pack
//	POST /{owner}/{repo}.git/git-upload-pack
//
// The paths are the ones docs/architecture/urls-and-identifiers.md section 3
// lists for the Git transport, and both are `no-store` by protocol requirement
// (section 9). Push arrives in W1.8, and LFS in W1.11.
//
// # Existence disclosure
//
// A repository that does not exist, one the caller may not read, and one whose
// name is malformed produce the same response: the same status, the same body,
// the same headers, and no difference in what the server does before answering.
// That is the T-A4 control, and it is why a denied fetch is not a `403`.
//
// # Credentials
//
// This version resolves every caller as anonymous. The model that turns an
// `Authorization` header into a principal is the credential workstream in phase
// 4, and W1.7's acceptance criteria are anonymous-scoped. A presented header is
// therefore ignored rather than partly honoured, and it is never logged.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/go-gitnest/GitNest/internal/identity"
	platformgit "github.com/go-gitnest/GitNest/internal/platform/git"
	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// Service names as they appear in the `service` query parameter and in the path.
const (
	infoRefsPath    = "info/refs"
	uploadPackPath  = "git-upload-pack"
	receivePackPath = "git-receive-pack"
	uploadPackName  = "git-upload-pack"
	receivePackName = "git-receive-pack"
)

// Content types the smart HTTP protocol requires.
const (
	advertisementContentType = "application/x-git-upload-pack-advertisement"
	requestContentType       = "application/x-git-upload-pack-request"
	resultContentType        = "application/x-git-upload-pack-result"
)

// gitDirectorySuffix is the path suffix that marks a Git transport URL, so
// `/{owner}/{repo}.git/...` distinguishes the transport from the browser route
// `/{owner}/{repo}`.
const gitDirectorySuffix = ".git"

// protocolHeader carries the client's protocol version preference. Passing it
// through is what enables protocol v2.
const protocolHeader = "Git-Protocol"

// maxProtocolValue bounds the header before it reaches the child environment.
const maxProtocolValue = 64

// serviceLine is the pkt-line that opens a smart HTTP advertisement. Its length
// prefix counts itself, which is why it is computed rather than fixed.
var serviceLine = []byte("# service=git-upload-pack\n")

// Authenticator turns a request into the principal it acts as.
//
// It is the seam the credential model plugs into. The architecture requires a
// transport to pass a principal and never to inspect a scope field itself
// (docs/security/threat-model.md T-A3), so this package asks one question —
// "who is this?" — and leaves every access decision to internal/repository.
//
// **The production implementation is AnonymousAuthenticator, and that is
// deliberate.** The model that turns an `Authorization` header into a verified
// principal is the credential workstream in phase 4 (W4.8). Until it exists, a
// credential this server cannot verify must not appear to authenticate anyone,
// so the header is ignored rather than partly honoured. The interface exists now
// so that plugging the model in is a wiring change, and so that the
// authorization paths beyond anonymous are testable rather than unreachable.
type Authenticator interface {
	Principal(r *http.Request) repository.Principal
}

// AnonymousAuthenticator resolves every caller as anonymous.
type AnonymousAuthenticator struct{}

// Principal returns the anonymous principal.
func (AnonymousAuthenticator) Principal(*http.Request) repository.Principal {
	return repository.Anonymous()
}

// OwnerLookup resolves a route's `{owner}` segment to an account.
//
// It is defined here, at the consumer, and is satisfied by *identity.Service.
type OwnerLookup interface {
	OwnerByUsername(ctx context.Context, username string) (identity.User, error)
}

// AccessService answers every access question about a repository.
//
// It is satisfied by *repository.Service. The transport asks one question —
// "may this caller do this?" — and reads the answer; it never derives a level
// itself, which is why the archive rule and the permission rule arrive in one
// call rather than as a level the transport then inspects.
type AccessService interface {
	ResolveAndRequire(ctx context.Context, viewer repository.Principal, ownerID, name string, required repository.Level) (repository.Repository, repository.Level, error)
}

// GitRunner starts a bounded Git operation. It is satisfied by
// *platformgit.Runner.
//
// It is an interface so a test can prove the negative that matters: that a
// denied request starts no process at all. Asserting that against a fake is the
// only way to observe it directly.
type GitRunner interface {
	Run(ctx context.Context, request platformgit.Request) (platformgit.Result, error)
}

// Handler serves the Git transport.
type Handler struct {
	runner        GitRunner
	layout        *storage.Layout
	owners        OwnerLookup
	repositories  AccessService
	authenticator Authenticator
	// pushByteLimit bounds a push body. It is a field rather than a constant so
	// that a test can exercise the bound without streaming two gigabytes, and it
	// is set only by NewHandler in production.
	pushByteLimit int64
	// publicURL is the instance's own base URL, used to build the absolute action
	// URLs an LFS batch returns. It is empty until WithPublicURL is called.
	publicURL string
	// lfsOptions carries the LFS bounds, which docs/architecture/performance.md
	// gives as settings (`git.lfs_max_object_bytes`) rather than constants. They are
	// a field for the same reason pushByteLimit is: a test can exercise the cap
	// without storing two gigabytes.
	lfsOptions storage.LFSOptions
	// log receives the operator-facing notice of an inconsistency or of a push
	// outcome.
	log func(message string, attributes ...any)
}

// NewHandler returns a handler wired to the runner, the storage layout, the two
// services that answer identity and access questions, and the authenticator that
// says who the caller is.
func NewHandler(
	runner GitRunner,
	layout *storage.Layout,
	owners OwnerLookup,
	repositories AccessService,
	authenticator Authenticator,
	log func(string, ...any),
) (*Handler, error) {
	// A nil interface and a nil pointer stored in an interface both have to be
	// refused, because either would panic on the first request rather than at
	// startup.
	if runner == nil {
		return nil, errors.New("the Git transport needs a runner")
	}
	if value := reflect.ValueOf(runner); value.Kind() == reflect.Pointer && value.IsNil() {
		return nil, errors.New("the Git transport needs a runner")
	}
	if layout == nil {
		return nil, errors.New("the Git transport needs a storage layout")
	}
	if owners == nil {
		return nil, errors.New("the Git transport needs an owner lookup")
	}
	if repositories == nil {
		return nil, errors.New("the Git transport needs the repository service")
	}
	if authenticator == nil {
		return nil, errors.New("the Git transport needs an authenticator")
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	if err := storage.DefaultLFSOptions().Validate(); err != nil {
		// A default that is not usable is a programming error rather than a
		// configuration one, and it is caught here rather than on the first upload.
		return nil, err
	}
	return &Handler{
		runner: runner, layout: layout, owners: owners, repositories: repositories,
		authenticator: authenticator, pushByteLimit: maxPushBytes,
		lfsOptions: storage.DefaultLFSOptions(), log: log,
	}, nil
}

// WithPublicURL sets the instance's base URL, which the LFS batch uses to build
// the absolute action URLs the protocol requires.
//
// git-lfs 3.7.1 refuses a relative href — it reports `missing protocol` and the
// transfer never happens — so the URL must be absolute. Building it from the
// configured `GITNEST_PUBLIC_URL` rather than from the request's Host header is
// both more correct behind a proxy and free of the Host header entirely.
//
// When it is not set, the handler falls back to the scheme and Host of the request.
// That can only redirect the client that supplied the header, and the batch
// response is `no-store`, so it is not a cache-poisoning vector; the configured
// value is preferred because it is also right when the process sits behind a proxy.
func (h *Handler) WithPublicURL(baseURL string) *Handler {
	h.publicURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return h
}

// WithLFSOptions replaces the LFS bounds.
//
// docs/architecture/performance.md gives the object cap as the setting
// `git.lfs_max_object_bytes` rather than a constant, so the value is applied at
// wiring time. It is deliberately not a route-level decision: a bound that could
// change per request would be a bound an attacker could talk their way out of.
//
// The handler is returned so the call reads as wiring, and it panics on an
// unusable value because a bad bound is a startup fault rather than a request
// fault.
func (h *Handler) WithLFSOptions(options storage.LFSOptions) *Handler {
	if err := options.Validate(); err != nil {
		panic("git transport: invalid LFS options: " + err.Error())
	}
	h.lfsOptions = options
	return h
}

// Register adds the Git transport routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{owner}/{repo}/"+infoRefsPath, h.infoRefs)
	mux.HandleFunc("POST /{owner}/{repo}/"+uploadPackPath, h.uploadPack)
	mux.HandleFunc("POST /{owner}/{repo}/"+receivePackPath, h.receivePack)
	// Git LFS shares this handler's authorization path and storage layout, so it is
	// registered here rather than by a second handler that would be a second place
	// for an access decision to be made.
	h.registerLFS(mux)
}

// infoRefs answers the ref advertisement that begins every clone, fetch, and
// push.
//
// The advertisement is buffered before it is written, because the protocol
// requires the service pkt-line to precede it and the child's output cannot be
// interleaved with a header written after it.
//
// The service decides both the level the caller must hold and the content type,
// so the two halves of the protocol cannot drift apart: a fetch needs read, a
// push needs write, and a caller who holds read does not learn from this endpoint
// whether pushing would be more revealing than reading.
func (h *Handler) infoRefs(w http.ResponseWriter, r *http.Request) {
	requested := r.URL.Query().Get("service")

	var (
		service     platformgit.Service
		contentType string
		required    repository.Level
		timeout     = platformgit.DefaultFetchTimeout
	)
	switch requested {
	case uploadPackName:
		service, contentType, required = platformgit.ServiceUploadPack, advertisementContentType, repository.LevelRead
	case receivePackName:
		// A push advertisement lists the refs and the hooks a client may run, so
		// it is only for a caller who could actually push.
		service, contentType, required = platformgit.ServiceReceivePack, receiveAdvertisementContentType, repository.LevelWrite
		timeout = platformgit.DefaultPushTimeout
	default:
		// The dumb protocol is not served: the route table lists no object or
		// HEAD path, so a client asking for one is told the request is wrong
		// rather than being handed a directory listing.
		writeProtocolError(w, http.StatusBadRequest, "unsupported service")
		return
	}

	found, path, ok := h.authorize(w, r, required)
	if !ok {
		return
	}

	var advertisement bytes.Buffer
	result, err := h.runner.Run(r.Context(), platformgit.Request{
		Service:        service,
		RepositoryPath: path,
		Options:        []string{"--stateless-rpc", "--advertise-refs"},
		Environment:    protocolEnvironment(r),
		Stdout:         &advertisement,
		Limits:         platformgit.Limits{Timeout: timeout},
	})
	if err != nil {
		h.writeGitError(w, found, result, err)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)

	// The service announcement is a pkt-line whose length includes its own four
	// bytes, followed by a flush packet.
	line := serviceLineFor(requested)
	if _, err := fmt.Fprintf(w, "%04x", len(line)+4); err != nil {
		return
	}
	if _, err := w.Write(line); err != nil {
		return
	}
	if _, err := w.Write([]byte("0000")); err != nil {
		return
	}
	_, _ = advertisement.WriteTo(w)
}

// uploadPack streams a clone or fetch.
//
// The request body is the client's negotiation and the response is a pack, so
// both are streamed rather than buffered: a pack is far too large to hold, and
// buffering would also defeat the client's ability to stop early.
func (h *Handler) uploadPack(w http.ResponseWriter, r *http.Request) {
	if !requestContentTypeIsValid(r.Header.Get("Content-Type")) {
		writeProtocolError(w, http.StatusUnsupportedMediaType, "the request is not a Git pack negotiation")
		return
	}

	found, path, ok := h.authorize(w, r, repository.LevelRead)
	if !ok {
		return
	}

	stream := &flushWriter{target: w}
	result, err := h.runner.Run(r.Context(), platformgit.Request{
		Service:        platformgit.ServiceUploadPack,
		RepositoryPath: path,
		Options:        []string{"--stateless-rpc"},
		Environment:    protocolEnvironment(r),
		Stdin:          r.Body,
		Stdout:         stream,
		Limits:         platformgit.Limits{Timeout: platformgit.DefaultFetchTimeout},
	})
	if err != nil {
		h.writeGitError(w, found, result, err)
		return
	}
}

// authorize resolves the repository and the caller's level, writing the uniform
// not-found response and reporting false when the request may not proceed.
//
// Every rejection returns the same thing, in the same way, so the response does
// not disclose whether the repository exists (T-A4). The name is validated
// before anything is looked up, a name that is invalid produces the same
// response as a name that is merely unavailable, and a caller who holds read but
// not write is refused exactly like one who holds nothing.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, required repository.Level) (repository.Repository, string, bool) {
	owner := r.PathValue("owner")
	repoSegment := r.PathValue("repo")

	name, ok := repositoryNameFromSegment(repoSegment)
	if !ok {
		writeNotFound(w)
		return repository.Repository{}, "", false
	}

	ownerAccount, err := h.owners.OwnerByUsername(r.Context(), owner)
	if err != nil {
		// An unknown owner and an owner whose name is malformed are both
		// reported as not found, so neither can be probed.
		if !errors.Is(err, identity.ErrInvalidCredentials) {
			h.log("git transport: owner lookup failed", "error", err)
		}
		writeNotFound(w)
		return repository.Repository{}, "", false
	}

	// The principal carries identity only, never a pre-decided permission: the
	// level comes from the repository service and from nowhere else.
	found, _, err := h.repositories.ResolveAndRequire(r.Context(), h.authenticator.Principal(r), ownerAccount.ID, name, required)
	if err != nil {
		switch {
		case errors.Is(err, repository.ErrArchived):
			// The caller held the level and the repository refuses writes
			// because it is archived. Answering not-found would be a lie that
			// hides an operational fact from someone already allowed to see the
			// repository, so the refusal is stated.
			writeArchived(w)
			return repository.Repository{}, "", false
		case errors.Is(err, repository.ErrNotFound), errors.Is(err, repository.ErrForbidden):
			// Two cases share this error and must be answered differently, because Git's
			// credential negotiation needs a challenge while the level check must not
			// disclose existence.
			//
			// An **anonymous** caller is challenged, which is what makes a client present
			// its credential at all. A caller that **presented a usable credential** and
			// still lacks the level gets the not-found answer, so holding a valid token does
			// not turn a push attempt into an existence oracle.
			// The distinction is who the caller **is**, not what header it sent. An
			// anonymous caller is challenged, which is what makes Git present a credential at
			// all; a caller the authenticator resolved is answered with the not-found shape,
			// which is the T-A4 rule and is what a client holding a real credential deserves
			// to be refused with.
			//
			// Testing the principal rather than the header matters because a credential can
			// arrive from somewhere other than `Authorization` — a browser session cookie, or
			// a future transport — and a header check would challenge a caller this server had
			// already identified.
			if h.authenticator.Principal(r).IsAnonymous() {
				writeChallenge(w)
				return repository.Repository{}, "", false
			}
			writeNotFound(w)
			return repository.Repository{}, "", false
		default:
			h.log("git transport: repository resolution failed", "error", err)
			writeNotFound(w)
			return repository.Repository{}, "", false
		}
	}

	path, err := h.layout.RepositoryPath(found.StorageID)
	if err != nil {
		h.log("git transport: storage path is invalid", "repository_id", found.ID, "error", err)
		writeNotFound(w)
		return repository.Repository{}, "", false
	}
	exists, err := h.layout.RepositoryExists(found.StorageID)
	if err != nil {
		h.log("git transport: storage check failed", "repository_id", found.ID, "error", err)
		writeProtocolError(w, http.StatusServiceUnavailable, "the repository is temporarily unavailable")
		return repository.Repository{}, "", false
	}
	if !exists {
		// The row exists and the directory does not, which is the divergence
		// W1.6's repair path exists to fix. It is reported to the operator
		// rather than disguised as not-found: the caller was already authorized,
		// so nothing is disclosed by saying the server is unwell.
		h.log("git transport: repository directory is missing", "repository_id", found.ID)
		writeProtocolError(w, http.StatusServiceUnavailable, "the repository is temporarily unavailable")
		return repository.Repository{}, "", false
	}
	return found, path, true
}

// writeGitError maps a runner failure onto a response.
//
// A busy instance is the one case a client can usefully retry, and the
// performance document gives it a status and a hint. Everything else is a
// server-side problem, so it is reported generically: the child's stderr can
// name paths and is never returned to a client.
func (h *Handler) writeGitError(w http.ResponseWriter, found repository.Repository, result platformgit.Result, err error) {
	switch {
	case errors.Is(err, errRequestTooLarge):
		// The client streamed more than the bound. The bound is enforced as the
		// bytes flow, so Git sees a read failure and the push applies nothing.
		writeProtocolError(w, http.StatusRequestEntityTooLarge, "the push is too large")
	case errors.Is(err, platformgit.ErrBusy):
		w.Header().Set("Retry-After", "5")
		writeProtocolError(w, http.StatusServiceUnavailable, "the server is busy")
	case errors.Is(err, platformgit.ErrUnknownService), errors.Is(err, platformgit.ErrInvalidArgument),
		errors.Is(err, platformgit.ErrOutsideRepositoryRoot):
		// A request this process constructed badly is not the client's fault,
		// so it is a server error rather than a 4xx that would blame them.
		h.log("git transport: rejected a request it built", "repository_id", found.ID, "error", err)
		writeProtocolError(w, http.StatusInternalServerError, "the request could not be completed")
	default:
		h.log("git transport: Git operation failed",
			"repository_id", found.ID,
			"exit_code", result.ExitCode,
			"timed_out", result.TimedOut,
			"cancelled", result.Cancelled,
			"stderr_tail", result.StderrTail,
			"error", err)
		writeProtocolError(w, http.StatusInternalServerError, "the request could not be completed")
	}
}

// repositoryNameFromSegment strips the `.git` suffix and validates what is left.
//
// A segment without the suffix, or one whose name is invalid, is refused here so
// nothing reaches the database or a filesystem path.
func repositoryNameFromSegment(segment string) (string, bool) {
	if !strings.HasSuffix(segment, gitDirectorySuffix) {
		return "", false
	}
	name := strings.TrimSuffix(segment, gitDirectorySuffix)
	if err := repository.ValidateName(name); err != nil {
		return "", false
	}
	return name, true
}

// smartService reports whether the requested service is one this transport
// serves, and is the HTTP-layer half of the runner's service allowlist.
//
// A service outside it starts no process: the allowlist is enforced here and
// again in the runner, so a mistake in either place is not enough to run an
// unexpected verb.
func smartService(requested string) bool {
	return requested == uploadPackName || requested == receivePackName
}

// protocolEnvironment passes the client's protocol preference to the child.
//
// The value is a protocol version, so it is checked against a narrow shape
// before it becomes part of the child environment: a client-supplied string has
// no business being an environment variable unchecked.
func protocolEnvironment(r *http.Request) map[string]string {
	value := strings.TrimSpace(r.Header.Get(protocolHeader))
	if value == "" || len(value) > maxProtocolValue || !allowedProtocolValue(value) {
		return nil
	}
	return map[string]string{"GIT_PROTOCOL": value}
}

// allowedProtocolValue permits the versions and feature names the protocol
// defines, such as `version=2` or `version=1`.
//
// An empty value is refused: a protocol preference is never empty, and treating
// "absent" and "empty" differently at the caller would be one more case to get
// wrong.
func allowedProtocolValue(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '=', c == '.', c == ':', c == ',':
		default:
			return false
		}
	}
	return true
}

// writeChallenge asks a caller for credentials.
//
// **Git's credential negotiation requires this, and without it a token is unusable.** A
// client does not send an `Authorization` header first: it sends an unauthenticated
// request, and only presents credentials after a `401` carrying `WWW-Authenticate`. A
// server that answers an unauthenticated request with a plain not-found therefore never
// sees a credential at all, which is exactly how a real `git clone` failed while the
// handler's own tests — which set the header directly — passed.
//
// This looks like it contradicts threat model T-A4 ("anonymous and denied access return the
// same not-found shape as a nonexistent repository"), and it does not, because the two
// cases are distinguished by whether a credential was *presented*:
//
//   - **No credential, or one this server cannot verify** -> `401` with a challenge. The
//     caller is anonymous, and the response is identical for a repository that exists and
//     one that does not. Nothing about existence is disclosed: the challenge is the same
//     for every repository on the instance.
//   - **A valid credential without the required level** -> `404`, the same answer a
//     nonexistent repository gets. This is the case T-A4 is about, and it is where the
//     disclosure would matter, because that caller has proven it holds a real credential.
//
// So the oracle T-A4 forbids is still absent: learning that a repository exists requires a
// credential that authenticates, and a caller holding one of those is not the audience the
// rule protects against.
func writeChallenge(w http.ResponseWriter) {
	// The realm is the instance's, not the repository's, so the challenge cannot be used to
	// tell one repository from another.
	w.Header().Set("WWW-Authenticate", `Basic realm="GitNest"`)
	writeProtocolError(w, http.StatusUnauthorized, "authentication is required")
}

// writeNotFound writes the single response every unavailable repository gets.
//
// It is deliberately plain: no HTML, no help link, nothing that would let a
// caller distinguish "does not exist" from "you may not read it".
func writeNotFound(w http.ResponseWriter) {
	writeProtocolError(w, http.StatusNotFound, "repository not found")
}

// writeArchived refuses a write to an archived repository.
//
// It is a distinct answer from not-found on purpose: the caller already held the
// required level, so telling them the repository is archived discloses nothing
// they did not know, and it is the only message that lets a client stop retrying.
func writeArchived(w http.ResponseWriter) {
	writeProtocolError(w, http.StatusForbidden, "this repository is archived and read-only")
}

func writeProtocolError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// A Git transport response is never cacheable, and the not-found shape must
	// not be stored by anything in front of the server either.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}

// flushWriter forwards each write to the response and flushes it, so a pack
// reaches the client as it is produced instead of collecting in a buffer until
// the process exits.
type flushWriter struct {
	target http.ResponseWriter
	wrote  bool
}

func (f *flushWriter) Write(p []byte) (int, error) {
	if !f.wrote {
		// The headers go out with the first byte of the pack. Writing them here
		// rather than before the operation means a failure that happens before
		// any output can still be reported as a status code.
		f.target.Header().Set("Content-Type", resultContentType)
		f.target.Header().Set("Cache-Control", "no-store")
		f.target.Header().Set("Pragma", "no-cache")
		f.target.WriteHeader(http.StatusOK)
		f.wrote = true
	}
	written, err := f.target.Write(p)
	if flusher, ok := f.target.(http.Flusher); ok {
		flusher.Flush()
	}
	return written, err
}

// requestContentTypeIsValid reports whether a POST carries the content type the
// protocol defines.
//
// An absent type is accepted, because a client is not required to send one and
// refusing would break a working client. A present-but-wrong type is refused,
// so a request that is plainly not a pack negotiation does not start a Git
// process.
func requestContentTypeIsValid(contentType string) bool {
	trimmed := strings.TrimSpace(contentType)
	if trimmed == "" {
		return true
	}
	base, _, _ := strings.Cut(trimmed, ";")
	return strings.TrimSpace(base) == requestContentType
}
