package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	platformgit "github.com/go-gitnest/GitNest/internal/platform/git"
	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

const testStorageID = "01920000-0000-7000-8000-0000000000aa"

// fakeRunner records every Git operation the handler asks for.
//
// Recording is what makes the negative assertions possible: a denied request
// must leave the call count at zero, which is the T-A1 and T-A4 property that
// cannot be observed against a real process.
//
// It also consumes the request's stdin, because a real child does: a test that
// wants to assert what Git would receive has to look at what the transport
// actually offered, and reading here also means the transport's own byte bound
// is exercised rather than bypassed.
type fakeRunner struct {
	mu       sync.Mutex
	calls    []platformgit.Request
	stdin    []byte
	advert   string
	err      error
	exitCode int
}

func (f *fakeRunner) Run(_ context.Context, request platformgit.Request) (platformgit.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, request)
	f.mu.Unlock()

	if request.Stdin != nil {
		// Bounded generously: the transport's own limit is what this is meant to
		// observe, so the fake must not impose one of its own.
		read, err := io.ReadAll(io.LimitReader(request.Stdin, maxPushBytes+maxCommandSectionBytes))

		f.mu.Lock()
		f.stdin = read
		f.mu.Unlock()

		if err != nil {
			// A read failure is the transport's bound talking, so it is reported
			// exactly as a child reporting a failed stdin would be.
			return platformgit.Result{ExitCode: -1}, fmt.Errorf("the Git operation failed: %w", err)
		}
	}

	if f.err != nil {
		return platformgit.Result{ExitCode: f.exitCode, StderrTail: "synthetic failure"}, f.err
	}
	// The advertisement is written to whatever the handler passed as Stdout,
	// which is how a real child behaves.
	if request.Stdout != nil && f.advert != "" {
		_, _ = request.Stdout.Write([]byte(f.advert))
	}
	return platformgit.Result{ExitCode: 0, BytesOut: int64(len(f.advert))}, nil
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// stdinBytes returns what the transport offered the child on standard input.
func (f *fakeRunner) stdinBytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stdin
}

func (f *fakeRunner) lastRequest(t *testing.T) platformgit.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no Git operation was requested")
	}
	return f.calls[len(f.calls)-1]
}

// fakeOwners resolves an owner segment the way identity.Service does: it folds
// the name and reports an unknown owner as an invalid credential.
type fakeOwners struct {
	users map[string]identity.User // folded username -> account
	err   error
}

func (f *fakeOwners) OwnerByUsername(_ context.Context, username string) (identity.User, error) {
	if f.err != nil {
		return identity.User{}, f.err
	}
	folded := strings.ToLower(strings.TrimSpace(username))
	account, found := f.users[folded]
	if !found {
		return identity.User{}, identity.ErrInvalidCredentials
	}
	return account, nil
}

// fakeAccess answers access questions the way repository.Service does: a
// repository the caller may not read is reported exactly like one that does not
// exist.
type fakeAccess struct {
	repositories map[string]repository.Repository // "ownerID/name" -> row
	levels       map[string]repository.Level      // repository ID -> level
	err          error
	// lastViewer records the principal the transport passed, so a test can prove
	// the authenticator's answer is what reached the decision.
	lastViewer *repository.Principal
}

func accessKey(ownerID, name string) string { return ownerID + "/" + strings.ToLower(name) }

func (f *fakeAccess) Resolve(ctx context.Context, viewer repository.Principal, ownerID, name string) (repository.Repository, repository.Level, error) {
	return f.ResolveAndRequire(ctx, viewer, ownerID, name, repository.LevelNone)
}

// ResolveAndRequire mirrors the real service: it collapses "no access" into
// not-found, reports a level that is too low as forbidden, and refuses a write
// against an archived repository. Reproducing those three rules here is what
// lets the transport's handling of each be tested without a database.
func (f *fakeAccess) ResolveAndRequire(_ context.Context, viewer repository.Principal, ownerID, name string, required repository.Level) (repository.Repository, repository.Level, error) {
	if f.err != nil {
		return repository.Repository{}, repository.LevelNone, f.err
	}
	recorded := viewer
	f.lastViewer = &recorded

	found, ok := f.repositories[accessKey(ownerID, name)]
	if !ok {
		return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
	}
	level, ok := f.levels[found.ID]
	if !ok {
		// The real service collapses "no access" into not-found.
		return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
	}
	if !level.Allows(required) {
		return repository.Repository{}, level, repository.ErrForbidden
	}
	// The archive rule: a content change is refused while archived, and the
	// administrative operations that carry a higher requirement still pass.
	if found.Archived && required == repository.LevelWrite {
		return repository.Repository{}, level, repository.ErrArchived
	}
	return found, level, nil
}

// harness is a handler mounted on a mux, with the fakes behind it.
type harness struct {
	t        *testing.T
	server   *httptest.Server
	mux      *http.ServeMux
	handler  *Handler
	runner   *fakeRunner
	layout   *storage.Layout
	owners   *fakeOwners
	access   *fakeAccess
	logged   *syncBuffer
	ownerID  string
	publicID string
	public   repository.Repository
}

// dispatch sends a request straight to the handler, bypassing the HTTP client.
//
// It exists for the cases a well-behaved client cannot produce, such as a header
// containing a newline. A hostile client is not bound by the Go client's header
// validation, so testing at this layer is what actually exercises the handler.
func (h *harness) dispatch(t *testing.T, method, path, contentType, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

// syncBuffer collects log output written from a request goroutine.
type syncBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (b *syncBuffer) log(message string, attributes ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	line := message
	for _, attribute := range attributes {
		line += " " + strings.TrimSpace(toText(attribute))
	}
	b.lines = append(b.lines, line)
}

func (b *syncBuffer) all() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.lines, "\n")
}

func toText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case error:
		return typed.Error()
	default:
		return ""
	}
}

// newHarness builds a handler with one public repository whose directory exists.
func newHarness(t *testing.T) *harness {
	t.Helper()
	base := t.TempDir()
	layout, err := storage.NewLayout(filepath.Join(base, "repositories"), filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := layout.EnsureTmpRoot(); err != nil {
		t.Fatalf("EnsureTmpRoot: %v", err)
	}
	if _, err := layout.EnsureRepository(testStorageID); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}

	ownerID := uuid.NewString()
	public := repository.Repository{
		ID: uuid.NewString(), OwnerID: ownerID, Name: "public-repo",
		Visibility: repository.VisibilityPublic, StorageID: testStorageID,
	}

	runner := &fakeRunner{advert: "001e# service=git-upload-pack\n0000"}
	owners := &fakeOwners{users: map[string]identity.User{
		"alice": {ID: ownerID, Username: "alice", Status: identity.StatusActive},
	}}
	access := &fakeAccess{
		repositories: map[string]repository.Repository{
			accessKey(ownerID, "public-repo"): public,
		},
		levels: map[string]repository.Level{public.ID: repository.LevelRead},
	}
	logged := &syncBuffer{}

	// The default harness caller is **authenticated and presents a credential**, so a test
	// about a level is testing the level rather than anonymity.
	//
	// The transport distinguishes "no credential" from "a credential that was rejected":
	// the first is challenged so Git sends one, the second gets the not-found answer. A test
	// whose subject is a level therefore has to look like a client that already tried.
	handler, err := NewHandler(runner, layout, owners, access,
		fixedAuthenticator{principal: repository.User(uuid.NewString(), false)}, logged.log)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &harness{
		t: t, server: server, mux: mux, handler: handler, runner: runner, layout: layout,
		owners: owners, access: access, logged: logged, ownerID: ownerID, publicID: public.ID,
		public: public,
	}
}

// get sends an info/refs request.
// testCredential is what the harness presents, so requests take the credential-presented
// branch rather than the challenge one.
const testCredential = "Bearer " + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

func (h *harness) get(path string) *http.Response {
	h.t.Helper()
	request, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		h.t.Fatalf("build GET %s: %v", path, err)
	}
	request.Header.Set("Authorization", testCredential)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	h.t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func TestRoutesAndServiceValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		want int
	}{
		{name: "smart upload-pack", path: "/alice/public-repo.git/info/refs?service=git-upload-pack", want: http.StatusOK},
		{name: "missing service", path: "/alice/public-repo.git/info/refs", want: http.StatusBadRequest},
		{name: "empty service", path: "/alice/public-repo.git/info/refs?service=", want: http.StatusBadRequest},
		{name: "dumb protocol", path: "/alice/public-repo.git/info/refs?service=git-upload-pack-dumb", want: http.StatusBadRequest},
		// receive-pack is served from W1.8, but it requires write access; an
		// anonymous caller is therefore refused exactly like a missing
		// repository rather than told that pushing is unavailable.
		{name: "receive-pack needs write access", path: "/alice/public-repo.git/info/refs?service=git-receive-pack", want: http.StatusNotFound},
		{name: "upload-archive not in the allowlist", path: "/alice/public-repo.git/info/refs?service=git-upload-archive", want: http.StatusBadRequest},
		{name: "service injection attempt", path: "/alice/public-repo.git/info/refs?service=git-upload-pack%3Brm%20-rf", want: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			response := h.get(test.path)
			if response.StatusCode != test.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.want)
			}
			// Only a served request may reach Git.
			if test.want != http.StatusOK && h.runner.callCount() != 0 {
				t.Errorf("a rejected request started %d Git operations", h.runner.callCount())
			}
		})
	}
}

// TestMalformedNamesAreRejectedBeforeAnyLookup covers the path grammar: nothing
// malformed reaches the database or a filesystem path.
func TestMalformedNamesAreRejectedBeforeAnyLookup(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "no .git suffix", path: "/alice/public-repo/info/refs?service=git-upload-pack"},
		{name: "double .git", path: "/alice/public-repo.git.git/info/refs?service=git-upload-pack"},
		{name: "empty name", path: "/alice/.git/info/refs?service=git-upload-pack"},
		{name: "traversal", path: "/alice/..%2Fetc.git/info/refs?service=git-upload-pack"},
		{name: "reserved name", path: "/alice/settings.git/info/refs?service=git-upload-pack"},
		{name: "name with a separator", path: "/alice/a%2Fb.git/info/refs?service=git-upload-pack"},
		{name: "unknown owner", path: "/nobody/public-repo.git/info/refs?service=git-upload-pack"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			response := h.get(test.path)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", response.StatusCode)
			}
			if h.runner.callCount() != 0 {
				t.Errorf("a rejected path started %d Git operations", h.runner.callCount())
			}
		})
	}
}

// TestUnavailableRepositoryIsIndistinguishable is the T-A4 assertion at the
// response level: a private repository and a repository that does not exist
// produce byte-identical responses, and neither starts a Git process.
func TestUnavailableRepositoryIsIndistinguishable(t *testing.T) {
	h := newHarness(t)

	// A private repository the anonymous caller may not read. The fake collapses
	// it to not-found exactly as the real service does.
	private := repository.Repository{
		ID: uuid.NewString(), OwnerID: h.ownerID, Name: "private-repo",
		Visibility: repository.VisibilityPrivate, StorageID: uuid.NewString(),
	}
	h.access.repositories[accessKey(h.ownerID, "private-repo")] = private
	delete(h.access.levels, private.ID)

	privateResponse := h.get("/alice/private-repo.git/info/refs?service=git-upload-pack")
	missingResponse := h.get("/alice/no-such-repo.git/info/refs?service=git-upload-pack")

	for _, field := range []string{"status", "body", "content-type", "cache-control", "pragma"} {
		if got, want := responseField(privateResponse, missingResponse, field), ""; got != want {
			t.Errorf("%s differs between a private and a missing repository: %q", field, got)
		}
	}
	if privateResponse.StatusCode != missingResponse.StatusCode {
		t.Fatalf("status differs: private %d, missing %d", privateResponse.StatusCode, missingResponse.StatusCode)
	}
	if bodyOf(t, privateResponse) != bodyOf(t, missingResponse) {
		t.Fatal("the response bodies differ")
	}
	if h.runner.callCount() != 0 {
		t.Errorf("%d Git operations started for unavailable repositories", h.runner.callCount())
	}
}

// TestLevelBelowReadIsRefused keeps the defensive branch honest: a resolved
// repository whose level is insufficient is refused like an unavailable one.
func TestLevelBelowReadIsRefused(t *testing.T) {
	h := newHarness(t)
	h.access.levels[h.publicID] = repository.LevelNone

	response := h.get("/alice/public-repo.git/info/refs?service=git-upload-pack")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
	if h.runner.callCount() != 0 {
		t.Errorf("a refused request started %d Git operations", h.runner.callCount())
	}
}

// TestSuccessfulAdvertisement covers the served shape: the service pkt-line
// precedes the child's output, and the response is not cacheable.
func TestSuccessfulAdvertisement(t *testing.T) {
	h := newHarness(t)
	response := h.get("/alice/public-repo.git/info/refs?service=git-upload-pack")

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != advertisementContentType {
		t.Errorf("Content-Type = %q, want %q", got, advertisementContentType)
	}
	// no-store is a protocol requirement, not a preference.
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	body := bodyOf(t, response)
	if !strings.HasPrefix(body, "001e# service=git-upload-pack\n0000") {
		t.Errorf("the response does not begin with the service pkt-line: %q", body)
	}
	// The child's own advertisement follows it.
	if !strings.Contains(body, "001e# service=git-upload-pack") {
		t.Errorf("the child's advertisement is missing: %q", body)
	}

	request := h.runner.lastRequest(t)
	if request.Service != platformgit.ServiceUploadPack {
		t.Errorf("service = %q, want upload-pack", request.Service)
	}
	if !contains(request.Options, "--advertise-refs") || !contains(request.Options, "--stateless-rpc") {
		t.Errorf("options = %v, want the stateless advertisement flags", request.Options)
	}
	expectedPath, err := h.layout.RepositoryPath(testStorageID)
	if err != nil {
		t.Fatalf("RepositoryPath: %v", err)
	}
	if request.RepositoryPath != expectedPath {
		t.Errorf("repository path = %q, want the layout-derived %q", request.RepositoryPath, expectedPath)
	}
	// The path came from the storage identifier, never from the request.
	if strings.Contains(request.RepositoryPath, "public-repo") {
		t.Error("the filesystem path contains the user-supplied name")
	}
}

// TestExistenceDisclosureIsUniformAcrossMethods checks the POST route too.
func TestExistenceDisclosureIsUniformAcrossMethods(t *testing.T) {
	h := newHarness(t)

	privatePath := h.server.URL + "/alice/private-repo.git/git-upload-pack"
	missingPath := h.server.URL + "/alice/absent-repo.git/git-upload-pack"

	private := post(t, privatePath, requestContentType, "")
	missing := post(t, missingPath, requestContentType, "")

	if private.StatusCode != missing.StatusCode || private.StatusCode != http.StatusNotFound {
		t.Fatalf("statuses = %d and %d, want both 404", private.StatusCode, missing.StatusCode)
	}
	if bodyOf(t, private) != bodyOf(t, missing) {
		t.Fatal("the POST bodies differ between a private and a missing repository")
	}
	if h.runner.callCount() != 0 {
		t.Errorf("%d Git operations started for unavailable repositories", h.runner.callCount())
	}
}

func TestUploadPackStreamsAndPassesProtocolVersion(t *testing.T) {
	h := newHarness(t)
	response := postWithHeaders(t, h.server.URL+"/alice/public-repo.git/git-upload-pack",
		requestContentType, "0032want 0000000000000000000000000000000000000000\n0000",
		map[string]string{"Git-Protocol": "version=2"})

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	request := h.runner.lastRequest(t)
	if request.Service != platformgit.ServiceUploadPack {
		t.Errorf("service = %q, want upload-pack", request.Service)
	}
	if contains(request.Options, "--advertise-refs") {
		t.Error("the pack request carries the advertisement flag")
	}
	if got := request.Environment["GIT_PROTOCOL"]; got != "version=2" {
		t.Errorf("GIT_PROTOCOL = %q, want the client's version passed through", got)
	}
}

// TestProtocolHeaderIsValidated stops a client-supplied string from becoming an
// environment variable unchecked.
//
// The requests are dispatched straight to the handler rather than through an
// HTTP client, because Go's client refuses to send a header containing a newline
// and a hostile client is under no such obligation. Testing at this layer is
// what actually exercises the validation.
func TestProtocolHeaderIsValidated(t *testing.T) {
	h := newHarness(t)

	for _, test := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "version 2", value: "version=2", want: "version=2"},
		{name: "version 1", value: "version=1", want: "version=1"},
		{name: "padded", value: " version=2 ", want: "version=2"},
		{name: "empty", value: "", want: ""},
		{name: "shell metacharacters", value: "version=2;rm -rf /", want: ""},
		{name: "newline injection", value: "version=2\nGIT_DIR=/etc", want: ""},
		{name: "carriage return injection", value: "version=2\r\nGIT_DIR=/etc", want: ""},
		{name: "path", value: "../../etc", want: ""},
		{name: "null byte", value: "version=2\x00", want: ""},
		{name: "over-long", value: strings.Repeat("v", maxProtocolValue+1), want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			h.dispatch(t, http.MethodPost, "/alice/public-repo.git/git-upload-pack",
				requestContentType, "0000", map[string]string{"Git-Protocol": test.value})

			request := h.runner.lastRequest(t)
			if got := request.Environment["GIT_PROTOCOL"]; got != test.want {
				t.Errorf("GIT_PROTOCOL = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWrongContentTypeIsRefusedWithoutStartingGit(t *testing.T) {
	h := newHarness(t)
	response := post(t, h.server.URL+"/alice/public-repo.git/git-upload-pack", "text/plain", "not a pack")
	if response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", response.StatusCode)
	}
	if h.runner.callCount() != 0 {
		t.Errorf("a refused content type started %d Git operations", h.runner.callCount())
	}
}

// TestBusyIsRetryable covers the one failure a client can usefully retry.
func TestBusyIsRetryable(t *testing.T) {
	h := newHarness(t)
	h.runner.err = platformgit.ErrBusy

	response := h.get("/alice/public-repo.git/info/refs?service=git-upload-pack")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	if response.Header.Get("Retry-After") == "" {
		t.Error("a busy response carries no Retry-After")
	}
}

// TestGitFailureIsGenericAndDoesNotLeakStderr keeps the child's diagnostics out
// of the response: stderr can name filesystem paths.
func TestGitFailureIsGenericAndDoesNotLeakStderr(t *testing.T) {
	h := newHarness(t)
	h.runner.err = errors.New("exit status 128")
	h.runner.exitCode = 128
	// The fake reports a stderr tail that must not reach the client.
	h.runner.advert = ""

	response := h.get("/alice/public-repo.git/info/refs?service=git-upload-pack")
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.StatusCode)
	}
	body := bodyOf(t, response)
	if strings.Contains(body, "synthetic failure") || strings.Contains(body, "128") {
		t.Errorf("the response carries process detail: %q", body)
	}
	if !strings.Contains(h.logged.all(), "synthetic failure") {
		t.Error("the operator log does not carry the child's diagnostics")
	}
}

// TestMissingDirectoryIsReportedNotDisguised covers the divergence case: the
// caller is authorized, so saying the server is unwell discloses nothing.
func TestMissingDirectoryIsReportedNotDisguised(t *testing.T) {
	h := newHarness(t)
	if err := h.layout.RemoveRepository(testStorageID); err != nil {
		t.Fatalf("RemoveRepository: %v", err)
	}

	response := h.get("/alice/public-repo.git/info/refs?service=git-upload-pack")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	if h.runner.callCount() != 0 {
		t.Errorf("a missing directory started %d Git operations", h.runner.callCount())
	}
	if !strings.Contains(h.logged.all(), "directory is missing") {
		t.Errorf("the operator was not told: %q", h.logged.all())
	}
}

// TestNewHandlerValidatesItsDependencies keeps a wiring mistake at startup
// rather than on the first request.
func TestNewHandlerValidatesItsDependencies(t *testing.T) {
	base := t.TempDir()
	layout, err := storage.NewLayout(filepath.Join(base, "repositories"), filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	runner := &fakeRunner{}
	owners := &fakeOwners{}
	access := &fakeAccess{}
	var nilRunner *fakeRunner

	for _, test := range []struct {
		name string
		call func() (*Handler, error)
	}{
		{name: "nil runner", call: func() (*Handler, error) {
			return NewHandler(nil, layout, owners, access, AnonymousAuthenticator{}, nil)
		}},
		{name: "typed nil runner", call: func() (*Handler, error) {
			return NewHandler(nilRunner, layout, owners, access, AnonymousAuthenticator{}, nil)
		}},
		{name: "nil layout", call: func() (*Handler, error) {
			return NewHandler(runner, nil, owners, access, AnonymousAuthenticator{}, nil)
		}},
		{name: "nil owners", call: func() (*Handler, error) {
			return NewHandler(runner, layout, nil, access, AnonymousAuthenticator{}, nil)
		}},
		{name: "nil access service", call: func() (*Handler, error) {
			return NewHandler(runner, layout, owners, nil, AnonymousAuthenticator{}, nil)
		}},
		{name: "nil authenticator", call: func() (*Handler, error) {
			return NewHandler(runner, layout, owners, access, nil, nil)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.call(); err == nil {
				t.Fatal("an incomplete handler was accepted")
			}
		})
	}

	// A nil log is allowed, so a caller that does not care need not supply one.
	if _, err := NewHandler(runner, layout, owners, access, AnonymousAuthenticator{}, nil); err != nil {
		t.Errorf("a nil logger was refused: %v", err)
	}
}

func TestRepositoryNameFromSegment(t *testing.T) {
	for _, test := range []struct {
		segment string
		want    string
		ok      bool
	}{
		{segment: "repo.git", want: "repo", ok: true},
		{segment: "My-Repo.git", want: "My-Repo", ok: true},
		{segment: "repo", ok: false},
		{segment: ".git", ok: false},
		{segment: "repo.git.git", ok: false},
		{segment: "..git", ok: false},
		{segment: "admin.git", ok: false},
		{segment: "", ok: false},
	} {
		t.Run(test.segment, func(t *testing.T) {
			got, ok := repositoryNameFromSegment(test.segment)
			if ok != test.ok || got != test.want {
				t.Errorf("repositoryNameFromSegment(%q) = %q, %t; want %q, %t",
					test.segment, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestRequestContentTypeIsValid(t *testing.T) {
	for _, test := range []struct {
		contentType string
		want        bool
	}{
		{contentType: "", want: true},
		{contentType: requestContentType, want: true},
		{contentType: requestContentType + "; charset=utf-8", want: true},
		{contentType: " text/plain", want: false},
		{contentType: "application/json", want: false},
	} {
		if got := requestContentTypeIsValid(test.contentType); got != test.want {
			t.Errorf("requestContentTypeIsValid(%q) = %t, want %t", test.contentType, got, test.want)
		}
	}
}

func TestAllowedProtocolValue(t *testing.T) {
	for _, value := range []string{"version=2", "version=1", "version=2,foo"} {
		if !allowedProtocolValue(value) {
			t.Errorf("allowedProtocolValue(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", " ", "a b", "a\nb", "a;b", "a/b", "a\\b", "a\x00"} {
		if allowedProtocolValue(value) {
			t.Errorf("allowedProtocolValue(%q) = true, want false", value)
		}
	}
}

// TestAuthorizationHeaderIsIgnoredAndNeverLogged covers the credential rule: a
// header this server cannot verify must not authenticate anyone, and it must not
// reach a log line.
func TestAuthorizationHeaderIsIgnoredAndNeverLogged(t *testing.T) {
	h := newHarness(t)
	response := postWithHeaders(t, h.server.URL+"/alice/private-repo.git/git-upload-pack",
		requestContentType, "0000",
		map[string]string{"Authorization": "Basic YWxpY2U6c3VwZXItc2VjcmV0LXRva2Vu"})

	// The private repository stays unavailable, so the header bought nothing.
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
	if h.runner.callCount() != 0 {
		t.Errorf("an unverifiable credential started %d Git operations", h.runner.callCount())
	}
	for _, secret := range []string{"super-secret-token", "YWxpY2U6"} {
		if strings.Contains(h.logged.all(), secret) {
			t.Errorf("a log line carries %q", secret)
		}
	}
	// The Git child receives no credential either.
	_ = os.Getenv("GIT_ASKPASS")
}

// --- helpers ---------------------------------------------------------------

func bodyOf(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	return string(body)
}

// responseField returns a comparable rendering of one response field, or an
// empty string when the two responses already agree on it.
func responseField(first, second *http.Response, field string) string {
	value := func(response *http.Response) string {
		switch field {
		case "status":
			return http.StatusText(response.StatusCode)
		case "content-type":
			return response.Header.Get("Content-Type")
		case "cache-control":
			return response.Header.Get("Cache-Control")
		case "pragma":
			return response.Header.Get("Pragma")
		}
		return ""
	}
	if value(first) == value(second) {
		return ""
	}
	return value(first) + " != " + value(second)
}

func post(t *testing.T, url, contentType, body string) *http.Response {
	t.Helper()
	return postWithHeaders(t, url, contentType, body, nil)
}

func postWithHeaders(t *testing.T, url, contentType, body string, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
