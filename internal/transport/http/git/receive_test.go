package git

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	platformgit "github.com/go-gitnest/GitNest/internal/platform/git"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The platform names this suite asserts on, aliased so the assertions read as
// the transport's own vocabulary.
const (
	platformServiceReceivePack = platformgit.ServiceReceivePack
	platformPushTimeout        = platformgit.DefaultPushTimeout
)

// fixedAuthenticator resolves every caller as one principal.
//
// It stands in for the credential model of phase 4 so the authorization paths
// beyond anonymous are exercised rather than merely written. Production wiring
// uses AnonymousAuthenticator, which is why the tests that matter here are the
// denied ones plus the ones that prove the seam works.
type fixedAuthenticator struct {
	principal repository.Principal
}

func (f fixedAuthenticator) Principal(*http.Request) repository.Principal { return f.principal }

// pkt renders one pkt-line for a request body.
func pkt(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

// pushBody builds a receive-pack request body: the update commands, a flush
// packet, and a synthetic pack that Git will reject but this layer never
// inspects.
func pushBody(commands ...string) string {
	var body strings.Builder
	for index, command := range commands {
		line := command
		if index == 0 {
			// The first command may carry NUL-separated capabilities.
			line += "\x00report-status side-band-64k agent=git/2.54.0"
		}
		body.WriteString(pkt(line + "\n"))
	}
	body.WriteString("0000")
	body.WriteString("PACK\x00\x00\x00\x02")
	return body.String()
}

const (
	zeroID  = "0000000000000000000000000000000000000000"
	firstID = "1111111111111111111111111111111111111111"
	otherID = "2222222222222222222222222222222222222222"
)

// newPushHarness returns a harness whose handler treats the caller as holding
// the given level on the public repository.
func newPushHarness(t *testing.T, level repository.Level) *harness {
	t.Helper()
	h := newHarness(t)
	h.access.levels[h.publicID] = level
	return h
}

// newAuthedHarness rebuilds the handler with a fixed principal, so the
// authorization path above anonymous can be tested.
//
// The point of the rebuild is that the principal source is the only thing that
// changes: the layout, the owners, and the access service stay the same, so a
// difference in outcome is attributable to the principal alone.
func newAuthedHarness(t *testing.T, principal repository.Principal, level repository.Level) *harness {
	t.Helper()
	h := newHarness(t)
	h.access.levels[h.publicID] = level

	handler, err := NewHandler(h.runner, h.layout, h.owners, h.access, fixedAuthenticator{principal: principal}, h.logged.log)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	h.mux = mux
	h.server = server
	return h
}

// TestPushRequiresWriteAccess is the enforcement assertion: read is not enough.
func TestPushRequiresWriteAccess(t *testing.T) {
	for _, test := range []struct {
		name  string
		level repository.Level
		want  int
	}{
		{name: "no access", level: repository.LevelNone, want: http.StatusNotFound},
		{name: "read only", level: repository.LevelRead, want: http.StatusNotFound},
		{name: "write", level: repository.LevelWrite, want: http.StatusOK},
		{name: "admin", level: repository.LevelAdmin, want: http.StatusOK},
		{name: "owner", level: repository.LevelOwner, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPushHarness(t, test.level)
			response := h.get("/alice/public-repo.git/info/refs?service=git-receive-pack")
			if response.StatusCode != test.want {
				t.Fatalf("advertisement status = %d, want %d", response.StatusCode, test.want)
			}
			// A caller below write must not reach Git at all.
			if test.want != http.StatusOK && h.runner.callCount() != 0 {
				t.Errorf("a refused advertisement started %d Git operations", h.runner.callCount())
			}
			if test.want == http.StatusOK {
				request := h.runner.lastRequest(t)
				if request.Service != platformServiceReceivePack {
					t.Errorf("service = %q, want receive-pack", request.Service)
				}
			}
		})
	}
}

// TestAnonymousPushIsRefusedLikeAMissingRepository is the T-A4 property for the
// push half: a caller who may not push cannot tell a private repository from an
// absent one.
func TestAnonymousPushIsRefusedLikeAMissingRepository(t *testing.T) {
	h := newHarness(t)

	private := repository.Repository{
		ID: "01920000-0000-7000-8000-0000000000bb", OwnerID: h.ownerID,
		Name: "private-repo", Visibility: repository.VisibilityPrivate, StorageID: h.public.StorageID,
	}
	h.access.repositories[accessKey(h.ownerID, "private-repo")] = private
	delete(h.access.levels, private.ID)

	for _, path := range []string{
		"/alice/private-repo.git/info/refs?service=git-receive-pack",
		"/alice/no-such-repo.git/info/refs?service=git-receive-pack",
	} {
		t.Run(path, func(t *testing.T) {
			response := h.get(path)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", response.StatusCode)
			}
		})
	}

	// A public repository is readable but not writable by an anonymous caller:
	// the fetch advertisement succeeds and the push advertisement does not.
	if got := h.get("/alice/public-repo.git/info/refs?service=git-upload-pack").StatusCode; got != http.StatusOK {
		t.Errorf("the fetch advertisement = %d, want 200", got)
	}
	if got := h.get("/alice/public-repo.git/info/refs?service=git-receive-pack").StatusCode; got != http.StatusNotFound {
		t.Errorf("the push advertisement = %d, want 404", got)
	}
}

// TestPushAdvertisementCarriesTheServiceLine covers the served shape and the
// content type, which differs from the fetch advertisement.
func TestPushAdvertisementCarriesTheServiceLine(t *testing.T) {
	h := newPushHarness(t, repository.LevelWrite)
	response := h.get("/alice/public-repo.git/info/refs?service=git-receive-pack")

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != receiveAdvertisementContentType {
		t.Errorf("Content-Type = %q, want %q", got, receiveAdvertisementContentType)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	body := bodyOf(t, response)
	if !strings.HasPrefix(body, "001f# service=git-receive-pack\n0000") {
		t.Errorf("the advertisement does not open with the receive-pack service line: %q", body)
	}
	// The push timeout applies, not the fetch timeout.
	if got := h.runner.lastRequest(t).Limits.Timeout; got != platformPushTimeout {
		t.Errorf("timeout = %s, want the push timeout %s", got, platformPushTimeout)
	}
}

// TestPushPassesTheObjectSizeLimit keeps GIT-OBJECT-BYTES wired to Git.
func TestPushPassesTheObjectSizeLimit(t *testing.T) {
	h := newPushHarness(t, repository.LevelWrite)
	postWithHeaders(t, h.server.URL+"/alice/public-repo.git/git-receive-pack",
		receiveRequestContentType, pushBody(zeroID+" "+firstID+" refs/heads/main"), nil)

	request := h.runner.lastRequest(t)
	if request.Service != platformServiceReceivePack {
		t.Fatalf("service = %q, want receive-pack", request.Service)
	}
	joined := strings.Join(request.Config, " ")
	if !strings.Contains(joined, "receive.maxInputSize="+fmt.Sprintf("%d", maxObjectBytes)) {
		t.Errorf("the object-size limit is not passed to Git: %v", request.Config)
	}
	// The limit is a configuration override, so it must precede the subcommand,
	// which the runner is responsible for.
	if !contains(request.Options, "--stateless-rpc") {
		t.Errorf("options = %v, want --stateless-rpc", request.Options)
	}
}

// TestPushReplaysTheValidatedCommands checks that the bytes this layer read are
// handed back to Git unchanged: Git must receive the request byte for byte.
func TestPushReplaysTheValidatedCommands(t *testing.T) {
	h := newPushHarness(t, repository.LevelWrite)
	body := pushBody(
		zeroID+" "+firstID+" refs/heads/main",
		otherID+" "+zeroID+" refs/heads/gone",
	)
	postWithHeaders(t, h.server.URL+"/alice/public-repo.git/git-receive-pack",
		receiveRequestContentType, body, nil)

	request := h.runner.lastRequest(t)
	if request.Service != platformServiceReceivePack {
		t.Fatalf("service = %q, want receive-pack", request.Service)
	}
	if got := string(h.runner.stdinBytes()); got != body {
		t.Errorf("the stream Git received differs from the request:\ngot  %q\nwant %q", got, body)
	}

	// The outcome record names the refs and what happened to each.
	logged := h.logged.all()
	for _, want := range []string{"refs/heads/main", "refs/heads/gone", "created", "deleted"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the push outcome record does not mention %q:\n%s", want, logged)
		}
	}
}

// TestPushRejectsUnacceptableRefUpdates covers the ref-update validation: a
// refused push starts no process, so no ref changes.
func TestPushRejectsUnacceptableRefUpdates(t *testing.T) {
	for _, test := range []struct {
		name string
		ref  string
	}{
		{name: "outside refs", ref: "HEAD"},
		{name: "reserved namespace", ref: "refs/notes/commits"},
		{name: "pull request refs", ref: "refs/pull/1/head"},
		{name: "remote-tracking ref", ref: "refs/remotes/origin/main"},
		{name: "traversal", ref: "refs/heads/../../etc"},
		{name: "double dot", ref: "refs/heads/a..b"},
		{name: "at-brace", ref: "refs/heads/a@{b}"},
		{name: "lock suffix", ref: "refs/heads/main.lock"},
		{name: "leading dot in a component", ref: "refs/heads/.hidden"},
		{name: "space", ref: "refs/heads/a b"},
		{name: "control character", ref: "refs/heads/a\x01b"},
		{name: "backslash", ref: "refs/heads/a\\b"},
		{name: "tilde", ref: "refs/heads/a~b"},
		{name: "caret", ref: "refs/heads/a^b"},
		{name: "colon", ref: "refs/heads/a:b"},
		{name: "question mark", ref: "refs/heads/a?b"},
		{name: "asterisk", ref: "refs/heads/a*b"},
		{name: "bracket", ref: "refs/heads/a[b"},
		{name: "trailing slash", ref: "refs/heads/main/"},
		{name: "double slash", ref: "refs/heads//main"},
		{name: "trailing dot", ref: "refs/heads/main."},
		{name: "empty", ref: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPushHarness(t, repository.LevelWrite)
			response := postWithHeaders(t, h.server.URL+"/alice/public-repo.git/git-receive-pack",
				receiveRequestContentType, pushBody(zeroID+" "+firstID+" "+test.ref), nil)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.StatusCode)
			}
			if h.runner.callCount() != 0 {
				t.Errorf("a refused update started %d Git operations", h.runner.callCount())
			}
			// The refusal is recorded, so an operator can see attempted writes.
			if !strings.Contains(h.logged.all(), "refused") {
				t.Errorf("the refusal was not recorded:\n%s", h.logged.all())
			}
		})
	}
}

// TestPushRejectsMalformedCommandSections covers what a client can send instead
// of update commands.
func TestPushRejectsMalformedCommandSections(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "no flush packet", body: pkt("refs/heads/main\n")},
		{name: "not a length prefix", body: "zzzz"},
		{name: "delimiter packet", body: "0001"},
		{name: "response-end packet", body: "0002"},
		{name: "two fields", body: pkt(zeroID+" refs/heads/main\n") + "0000"},
		{name: "short object id", body: pkt("abc def refs/heads/main\n") + "0000"},
		{name: "non-hex object id", body: pkt("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz "+firstID+" refs/heads/main\n") + "0000"},
		{name: "truncated payload", body: "0030" + "refs/heads/main\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPushHarness(t, repository.LevelWrite)
			response := postWithHeaders(t, h.server.URL+"/alice/public-repo.git/git-receive-pack",
				receiveRequestContentType, test.body, nil)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.StatusCode)
			}
			if h.runner.callCount() != 0 {
				t.Errorf("a malformed push started %d Git operations", h.runner.callCount())
			}
		})
	}
}

// TestPushRefCountIsBounded covers GIT-REFS.
func TestPushRefCountIsBounded(t *testing.T) {
	h := newPushHarness(t, repository.LevelWrite)

	commands := make([]string, 0, maxRefsPerPush+1)
	for index := 0; index <= maxRefsPerPush; index++ {
		commands = append(commands, fmt.Sprintf("%s %s refs/heads/branch-%d", zeroID, firstID, index))
	}
	response := postWithHeaders(t, h.server.URL+"/alice/public-repo.git/git-receive-pack",
		receiveRequestContentType, pushBody(commands...), nil)

	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.StatusCode)
	}
	if h.runner.callCount() != 0 {
		t.Errorf("an over-limit push started %d Git operations", h.runner.callCount())
	}
	if !strings.Contains(bodyOf(t, response), fmt.Sprintf("at most %d refs", maxRefsPerPush)) {
		t.Errorf("the refusal does not state the bound: %q", bodyOf(t, response))
	}

	// A push at the bound is accepted, so the limit is not off by one.
	atLimit := make([]string, 0, maxRefsPerPush)
	for index := 0; index < maxRefsPerPush; index++ {
		atLimit = append(atLimit, fmt.Sprintf("%s %s refs/heads/branch-%d", zeroID, firstID, index))
	}
	h2 := newPushHarness(t, repository.LevelWrite)
	response = postWithHeaders(t, h2.server.URL+"/alice/public-repo.git/git-receive-pack",
		receiveRequestContentType, pushBody(atLimit...), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a push at the ref bound = %d, want 200", response.StatusCode)
	}
}

// TestPushDeclaredLengthIsRefusedBeforeAnythingReads keeps an oversized push
// from reaching Git even in principle.
func TestPushDeclaredLengthIsRefusedBeforeAnythingReads(t *testing.T) {
	h := newPushHarness(t, repository.LevelWrite)

	request := httptest.NewRequest(http.MethodPost, "/alice/public-repo.git/git-receive-pack",
		strings.NewReader(pushBody(zeroID+" "+firstID+" refs/heads/main")))
	request.Header.Set("Content-Type", receiveRequestContentType)
	request.ContentLength = maxPushBytes + 1

	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", recorder.Code)
	}
	if h.runner.callCount() != 0 {
		t.Errorf("an oversized push started %d Git operations", h.runner.callCount())
	}
}

// TestPushStreamedOverflowIsRefused covers a client that streams without
// declaring a length.
//
// The handler's bound is lowered for this test rather than streaming the real
// two gigabytes: the property under test is that the bound is enforced as the
// bytes flow, and a bound of any size exercises the same code.
func TestPushStreamedOverflowIsRefused(t *testing.T) {
	h := newPushHarness(t, repository.LevelWrite)
	h.handler.pushByteLimit = 256

	// A valid command section followed by far more pack data than the bound
	// allows, so the refusal must come from the size bound rather than from
	// validation.
	request := httptest.NewRequest(http.MethodPost, "/alice/public-repo.git/git-receive-pack",
		io.NopCloser(io.MultiReader(
			strings.NewReader(pushBody(zeroID+" "+firstID+" refs/heads/main")),
			io.LimitReader(endless{'x'}, 4096))))
	request.Header.Set("Content-Type", receiveRequestContentType)
	// A negative length tells Go the size is unknown, so the reader is streamed.
	request.ContentLength = -1

	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", recorder.Code)
	}
	// The process did start, because a streamed length is unknown until it is
	// read. What matters is that it could not consume the body: Git applies no
	// ref update when its input ends early, and the refusal is recorded.
	if h.runner.callCount() != 1 {
		t.Fatalf("Git operations = %d, want the single operation whose input was cut off", h.runner.callCount())
	}
	if !strings.Contains(h.logged.all(), "push outcome") {
		t.Errorf("the refused push was not recorded:\n%s", h.logged.all())
	}
}

// TestPushStreamedWithinTheBoundSucceeds is the counterpart: the bound must not
// refuse a push that fits.
func TestPushStreamedWithinTheBoundSucceeds(t *testing.T) {
	h := newPushHarness(t, repository.LevelWrite)
	h.handler.pushByteLimit = 8192

	body := pushBody(zeroID + " " + firstID + " refs/heads/main")
	request := httptest.NewRequest(http.MethodPost, "/alice/public-repo.git/git-receive-pack",
		strings.NewReader(body))
	request.Header.Set("Content-Type", receiveRequestContentType)
	request.ContentLength = -1

	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if got := string(h.runner.stdinBytes()); got != body {
		t.Errorf("the child received %q, want the request verbatim", got)
	}
}

// endless produces a repeating character for the streaming-overflow case.
type endless struct{ c byte }

func (e endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = e.c
	}
	return len(p), nil
}

func TestPushWrongContentTypeStartsNoProcess(t *testing.T) {
	h := newPushHarness(t, repository.LevelWrite)
	response := post(t, h.server.URL+"/alice/public-repo.git/git-receive-pack",
		"text/plain", pushBody(zeroID+" "+firstID+" refs/heads/main"))
	if response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", response.StatusCode)
	}
	if h.runner.callCount() != 0 {
		t.Errorf("a refused content type started %d Git operations", h.runner.callCount())
	}
}

// TestPushDeniedForACredentialThisServerCannotVerify covers the deliberate
// boundary: an Authorization header buys nothing until the credential model
// exists, so a read-only caller with a header is still refused.
func TestPushDeniedForACredentialThisServerCannotVerify(t *testing.T) {
	h := newPushHarness(t, repository.LevelRead)
	response := postWithHeaders(t, h.server.URL+"/alice/public-repo.git/git-receive-pack",
		receiveRequestContentType, pushBody(zeroID+" "+firstID+" refs/heads/main"),
		map[string]string{"Authorization": "Basic YWxpY2U6dG9rZW4="})

	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
	if h.runner.callCount() != 0 {
		t.Errorf("an unverifiable credential started %d Git operations", h.runner.callCount())
	}
}

// TestFixedAuthenticatorReachesTheService proves the seam: a principal supplied
// by an authenticator is the principal the repository service decides on.
func TestFixedAuthenticatorReachesTheService(t *testing.T) {
	h := newAuthedHarness(t, repository.User("an-account", false), repository.LevelWrite)

	response := h.get("/alice/public-repo.git/info/refs?service=git-receive-pack")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if h.access.lastViewer == nil {
		t.Fatal("the access service was not consulted")
	}
	if h.access.lastViewer.UserID != "an-account" || h.access.lastViewer.IsAnonymous() {
		t.Errorf("the service saw %+v, want the authenticated principal", *h.access.lastViewer)
	}
}

func TestValidateGitRefNameAccepts(t *testing.T) {
	for _, ref := range []string{
		"refs/heads/main",
		"refs/heads/feature/deep/branch",
		"refs/tags/v1.0.0",
		"refs/heads/release-1.0",
		"refs/heads/UPPER",
	} {
		if err := validateGitRefName(ref); err != nil {
			t.Errorf("validateGitRefName(%q): %v", ref, err)
		}
	}
}

func TestValidateRefCommandNamespaces(t *testing.T) {
	for _, test := range []struct {
		ref   string
		valid bool
	}{
		{ref: "refs/heads/main", valid: true},
		{ref: "refs/tags/v1", valid: true},
		{ref: "refs/notes/commits", valid: false},
		{ref: "refs/remotes/origin/main", valid: false},
		{ref: "refs/pull/1/head", valid: false},
		{ref: "refs/stash", valid: false},
		{ref: "HEAD", valid: false},
	} {
		t.Run(test.ref, func(t *testing.T) {
			err := validateRefCommand(refCommand{oldValue: zeroID, newValue: firstID, ref: test.ref})
			if test.valid && err != nil {
				t.Errorf("validateRefCommand(%q): %v", test.ref, err)
			}
			if !test.valid && err == nil {
				t.Errorf("validateRefCommand(%q) accepted the ref", test.ref)
			}
		})
	}
}

func TestReadCommandSectionParsesCapabilitiesAndDeletions(t *testing.T) {
	body := pushBody(
		zeroID+" "+firstID+" refs/heads/main",
		firstID+" "+zeroID+" refs/heads/gone",
	)
	commands, replay, err := readCommandSection(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readCommandSection: %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("parsed %d commands, want 2", len(commands))
	}
	if commands[0].ref != "refs/heads/main" || commands[0].oldValue != zeroID || commands[0].newValue != firstID {
		t.Errorf("first command = %+v, want the creation of refs/heads/main", commands[0])
	}
	if !commands[1].deleted() {
		t.Errorf("second command = %+v, want a deletion", commands[1])
	}
	if commands[0].deleted() {
		t.Error("a creation was reported as a deletion")
	}
	// Only the command section is consumed; the pack is left for Git.
	if string(replay) == body {
		t.Error("the reader consumed the whole body, so Git would receive nothing")
	}
	if !strings.HasPrefix(body, string(replay)) {
		t.Errorf("the replayed bytes are not a prefix of the body:\n%q\n%q", replay, body)
	}
}

func TestIsZeroObjectID(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: strings.Repeat("0", 40), want: true},
		{value: strings.Repeat("0", 64), want: true},
		{value: strings.Repeat("0", 39), want: false},
		{value: firstID, want: false},
		{value: "", want: false},
		{value: strings.Repeat("0", 39) + "1", want: false},
	} {
		if got := isZeroObjectID(test.value); got != test.want {
			t.Errorf("isZeroObjectID(%q) = %t, want %t", test.value, got, test.want)
		}
	}
}

func TestValidObjectID(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: strings.Repeat("a", 40), want: true},
		{value: strings.Repeat("A", 40), want: true},
		{value: strings.Repeat("0", 64), want: true},
		{value: strings.Repeat("a", 39), want: false},
		{value: strings.Repeat("z", 40), want: false},
		{value: "", want: false},
	} {
		if got := validObjectID(test.value); got != test.want {
			t.Errorf("validObjectID(%q) = %t, want %t", test.value, got, test.want)
		}
	}
}

func TestSmartServiceAcceptsBothServedServices(t *testing.T) {
	for _, service := range []string{uploadPackName, receivePackName} {
		if !smartService(service) {
			t.Errorf("smartService(%q) = false, want true", service)
		}
	}
	for _, service := range []string{"", "git-upload-archive", "git-receive-pack;rm", "GIT-RECEIVE-PACK"} {
		if smartService(service) {
			t.Errorf("smartService(%q) = true, want false", service)
		}
	}
}

// TestPushFailureIsRecordedAndGeneric keeps the child's diagnostics out of the
// response while recording them for an operator.
func TestPushFailureIsRecordedAndGeneric(t *testing.T) {
	h := newPushHarness(t, repository.LevelWrite)
	h.runner.err = errors.New("exit status 128")
	h.runner.exitCode = 128

	response := postWithHeaders(t, h.server.URL+"/alice/public-repo.git/git-receive-pack",
		receiveRequestContentType, pushBody(zeroID+" "+firstID+" refs/heads/main"), nil)

	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.StatusCode)
	}
	if strings.Contains(bodyOf(t, response), "synthetic failure") {
		t.Error("the response carries the child's diagnostics")
	}
	logged := h.logged.all()
	if !strings.Contains(logged, "push outcome") || !strings.Contains(logged, "synthetic failure") {
		t.Errorf("the failure was not recorded for an operator:\n%s", logged)
	}
}
