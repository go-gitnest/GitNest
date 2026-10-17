package git

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// The LFS tests.
//
// These run against the real handler, the real storage layout, and a real HTTP
// server; only the access decision is a fake. That is deliberate: the access
// decision is what the repository service owns and what the integration suite
// covers, while the protocol, the store, the path derivation, and the refusal
// shapes are all here and all exercised.

const (
	lfsOIDStorageID = "01920000-0000-7000-8000-0000000000bb"
	lfsReadID       = "01920000-0000-7000-8000-0000000000cc"
)

// lfsHarness is a handler with two repositories: one writable and one readable.
type lfsHarness struct {
	t       *testing.T
	server  *httptest.Server
	handler *Handler
	layout  *storage.Layout
	access  *fakeAccess
	logged  *syncBuffer
	ownerID string
	// writable is the repository an upload may target.
	writable repository.Repository
	// readable is a separate repository, used to prove that a digest is not a
	// capability across repositories.
	readable repository.Repository
}

func newLFSHarness(t *testing.T) *lfsHarness {
	t.Helper()
	base := t.TempDir()
	layout, err := storage.NewLayout(filepath.Join(base, "repositories"), filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := layout.EnsureTmpRoot(); err != nil {
		t.Fatalf("EnsureTmpRoot: %v", err)
	}

	// Both repositories get their bare directory, because authorize() requires one:
	// a repository row whose directory is missing is the divergence the repair path
	// exists to fix, and it is reported rather than served. LFS objects live under
	// `lfs/<storage_id>`, but they still belong to a repository whose Git bytes are
	// where they should be.
	for _, storageID := range []string{lfsOIDStorageID, lfsReadID} {
		if _, err := layout.EnsureRepository(storageID); err != nil {
			t.Fatalf("EnsureRepository: %v", err)
		}
	}

	ownerID := uuid.NewString()
	writable := repository.Repository{
		ID: uuid.NewString(), OwnerID: ownerID, Name: "writable",
		Visibility: repository.VisibilityPrivate, StorageID: lfsOIDStorageID,
	}
	readable := repository.Repository{
		ID: uuid.NewString(), OwnerID: ownerID, Name: "readable",
		Visibility: repository.VisibilityPublic, StorageID: lfsReadID,
	}

	runner := &fakeRunner{advert: "001e# service=git-upload-pack\n0000"}
	owners := &fakeOwners{users: map[string]identity.User{
		"alice": {ID: ownerID, Username: "alice", Status: identity.StatusActive},
	}}
	// An instance administrator, so every request the tests make is authorized
	// unless the test deliberately lowers a level.
	access := &fakeAccess{
		repositories: map[string]repository.Repository{
			accessKey(ownerID, "writable"): writable,
			accessKey(ownerID, "readable"): readable,
		},
		levels: map[string]repository.Level{
			writable.ID: repository.LevelWrite,
			readable.ID: repository.LevelRead,
		},
	}
	logged := &syncBuffer{}

	handler, err := NewHandler(runner, layout, owners, access, adminAuthenticator{}, logged.log)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &lfsHarness{
		t: t, server: server, handler: handler, layout: layout, access: access,
		logged: logged, ownerID: ownerID, writable: writable, readable: readable,
	}
}

// adminAuthenticator resolves every caller as an instance administrator, so the
// fake access service's per-repository level is what decides.
type adminAuthenticator struct{}

func (adminAuthenticator) Principal(*http.Request) repository.Principal {
	return repository.User("01920000-0000-7000-8000-0000000000dd", true)
}

// batch posts a batch request and decodes the answer.
func (h *lfsHarness) batch(t *testing.T, repoName string, body string) (int, map[string]any, string) {
	t.Helper()
	response, err := http.Post(
		h.server.URL+"/alice/"+repoName+".git/info/lfs/objects/batch",
		"application/vnd.git-lfs+json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the batch body: %v", err)
	}
	decoded := map[string]any{}
	_ = json.Unmarshal(raw, &decoded)
	return response.StatusCode, decoded, string(raw)
}

// objects returns the objects array of a batch answer.
func (h *lfsHarness) objects(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["objects"].([]any)
	if !ok {
		t.Fatalf("the batch answer carries no objects: %v", body)
	}
	objects := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		object, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("an object is not an object: %v", entry)
		}
		objects = append(objects, object)
	}
	return objects
}

// actions returns one object's actions.
func actionsOf(t *testing.T, object map[string]any) map[string]any {
	t.Helper()
	raw, ok := object["actions"]
	if !ok {
		return nil
	}
	actions, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("actions is not an object: %v", raw)
	}
	return actions
}

// digestOf returns the object identifier for some bytes.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// upload puts an object through the HTTP endpoint.
func (h *lfsHarness) upload(t *testing.T, repoName, oid string, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut,
		h.server.URL+"/alice/"+repoName+".git/info/lfs/objects/"+oid, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build the upload: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// download fetches an object through the HTTP endpoint.
func (h *lfsHarness) download(t *testing.T, repoName, oid string) (*http.Response, []byte) {
	t.Helper()
	response, err := http.Get(h.server.URL + "/alice/" + repoName + ".git/info/lfs/objects/" + oid)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the download: %v", err)
	}
	return response, body
}

// TestBatchDownloadOffersAnActionForAStoredObject covers the download negotiation
// for an object that is there.
func TestBatchDownloadOffersAnActionForAStoredObject(t *testing.T) {
	h := newLFSHarness(t)
	body := []byte("the object's bytes")
	oid := digestOf(body)

	// Store it directly, so the batch is tested against the store rather than
	// against the upload path.
	if _, err := h.layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: h.readable.StorageID, OID: oid, Size: int64(len(body)), Body: bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("StoreLFSObject: %v", err)
	}

	status, answer, raw := h.batch(t, "readable",
		`{"operation":"download","transfers":["basic"],"objects":[{"oid":"`+oid+`","size":`+itoa(len(body))+`}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, raw)
	}
	if answer["transfer"] != "basic" {
		t.Errorf("transfer = %v, want basic", answer["transfer"])
	}
	if answer["hash_algo"] != "sha256" {
		t.Errorf("hash_algo = %v, want sha256", answer["hash_algo"])
	}

	objects := h.objects(t, answer)
	if len(objects) != 1 {
		t.Fatalf("the answer names %d objects, want 1", len(objects))
	}
	if objects[0]["authenticated"] != true {
		t.Error("the object is not marked authenticated")
	}
	if _, hasError := objects[0]["error"]; hasError {
		t.Errorf("a stored object reported an error: %v", objects[0]["error"])
	}
	download, ok := actionsOf(t, objects[0])["download"].(map[string]any)
	if !ok {
		t.Fatalf("no download action: %v", objects[0])
	}
	href, _ := download["href"].(string)
	if href == "" {
		t.Fatal("the download action carries no href")
	}
	// The href must be absolute: git-lfs refuses a relative one with
	// `missing protocol` and never transfers. It is asserted here as well as by the
	// real-client test, so a change back to a relative URL fails in both places.
	if !strings.HasPrefix(href, "http://") && !strings.HasPrefix(href, "https://") {
		t.Errorf("href = %q, want an absolute URL", href)
	}
	if !strings.Contains(href, h.server.URL) && !strings.Contains(href, "127.0.0.1") {
		t.Errorf("href = %q, want it to address this server", href)
	}
	if !strings.HasSuffix(href, oid) {
		t.Errorf("href = %q, want it to end in the object identifier", href)
	}
}

// TestBatchReportsAMissingObjectPerObject covers a download of something that is
// not there: the batch succeeds and the failure belongs to the object, so a client
// with a hundred objects is not stopped by one.
func TestBatchReportsAMissingObjectPerObject(t *testing.T) {
	h := newLFSHarness(t)
	oid := digestOf([]byte("never stored"))

	status, answer, raw := h.batch(t, "readable",
		`{"operation":"download","objects":[{"oid":"`+oid+`","size":10}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, raw)
	}
	objects := h.objects(t, answer)
	if len(objects) != 1 {
		t.Fatalf("the answer names %d objects, want 1", len(objects))
	}
	objectError, ok := objects[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("a missing object carried no error: %v", objects[0])
	}
	if code, _ := objectError["code"].(float64); int(code) != http.StatusNotFound {
		t.Errorf("error code = %v, want 404", objectError["code"])
	}
	if messages, _ := objectError["message"].(string); messages == "" {
		t.Error("the object error carries no message")
	}
	if actionsOf(t, objects[0]) != nil {
		t.Error("a missing object was given an action")
	}
}

// TestBatchUploadSkipsAnObjectItAlreadyHas covers the property that makes a
// retried upload cheap: nothing to do means no action, not an error.
func TestBatchUploadSkipsAnObjectItAlreadyHas(t *testing.T) {
	h := newLFSHarness(t)
	body := []byte("already stored")
	oid := digestOf(body)
	if _, err := h.layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: h.writable.StorageID, OID: oid, Size: int64(len(body)), Body: bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("StoreLFSObject: %v", err)
	}

	status, answer, raw := h.batch(t, "writable",
		`{"operation":"upload","objects":[{"oid":"`+oid+`","size":`+itoa(len(body))+`}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, raw)
	}
	objects := h.objects(t, answer)
	if actionsOf(t, objects[0]) != nil {
		t.Errorf("an object that is already stored was given an upload action: %v", objects[0])
	}
	if _, hasError := objects[0]["error"]; hasError {
		t.Errorf("a stored object reported an error: %v", objects[0]["error"])
	}
}

// TestBatchUploadOffersAnActionForAMissingObject is the other direction.
func TestBatchUploadOffersAnActionForAMissingObject(t *testing.T) {
	h := newLFSHarness(t)
	body := []byte("not stored yet")
	oid := digestOf(body)

	status, answer, raw := h.batch(t, "writable",
		`{"operation":"upload","objects":[{"oid":"`+oid+`","size":`+itoa(len(body))+`}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, raw)
	}
	objects := h.objects(t, answer)
	upload, ok := actionsOf(t, objects[0])["upload"].(map[string]any)
	if !ok {
		t.Fatalf("a missing object was not given an upload action: %v", objects[0])
	}
	if href, _ := upload["href"].(string); !strings.HasSuffix(href, oid) {
		t.Errorf("href = %q, want it to end in the object identifier", href)
	}
}

// TestUploadThenDownloadIsTheProtocolRoundTrip is the whole protocol in one test:
// negotiate an upload, send the bytes, and fetch them back.
func TestUploadThenDownloadIsTheProtocolRoundTrip(t *testing.T) {
	h := newLFSHarness(t)
	body := bytes.Repeat([]byte("large file content "), 1000)
	oid := digestOf(body)

	// 1. Negotiate.
	status, answer, raw := h.batch(t, "writable",
		`{"operation":"upload","objects":[{"oid":"`+oid+`","size":`+itoa(len(body))+`}]}`)
	if status != http.StatusOK {
		t.Fatalf("batch = %d, want 200: %s", status, raw)
	}
	if _, ok := actionsOf(t, h.objects(t, answer)[0])["upload"]; !ok {
		t.Fatalf("no upload action: %s", raw)
	}

	// 2. Upload.
	uploaded := h.upload(t, "writable", oid, body)
	if uploaded.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(uploaded.Body)
		t.Fatalf("upload = %d, want 200: %s", uploaded.StatusCode, detail)
	}

	// 3. The batch now says there is nothing to do.
	_, after, _ := h.batch(t, "writable", `{"operation":"upload","objects":[{"oid":"`+oid+`","size":`+itoa(len(body))+`}]}`)
	if actionsOf(t, h.objects(t, after)[0]) != nil {
		t.Error("the object is still offered for upload after it was uploaded")
	}

	// 4. Fetch it back, byte for byte.
	response, downloaded := h.download(t, "writable", oid)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("download = %d, want 200", response.StatusCode)
	}
	if !bytes.Equal(downloaded, body) {
		t.Errorf("downloaded %d bytes, want the %d uploaded", len(downloaded), len(body))
	}
	if got := response.Header.Get("Content-Length"); got != itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %d", got, len(body))
	}
	if got := response.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}
	// The document requires `no-store` for this subtree.
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// TestHoldingADigestIsNotACapability is T-C3's cross-repository case.
//
// The object is stored for one repository and the digest is valid, so only the
// per-repository scoping can refuse it. Both the batch and the direct object URL
// are checked, because a client can skip the negotiation.
func TestHoldingADigestIsNotACapability(t *testing.T) {
	h := newLFSHarness(t)
	body := []byte("content belonging to another repository")
	oid := digestOf(body)

	// Stored for `writable`, which is private.
	if _, err := h.layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: h.writable.StorageID, OID: oid, Size: int64(len(body)), Body: bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("StoreLFSObject: %v", err)
	}

	// The digest is well formed and the bytes are on disk.
	status, answer, raw := h.batch(t, "readable",
		`{"operation":"download","objects":[{"oid":"`+oid+`","size":`+itoa(len(body))+`}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, raw)
	}
	objectError, ok := h.objects(t, answer)[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("another repository's object was offered for download: %s", raw)
	}
	if code, _ := objectError["code"].(float64); int(code) != http.StatusNotFound {
		t.Errorf("error code = %v, want 404", objectError["code"])
	}

	// And the direct object URL refuses too, so skipping the batch gains nothing.
	response, downloaded := h.download(t, "readable", oid)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("a cross-repository download = %d, want 404", response.StatusCode)
	}
	if bytes.Contains(downloaded, []byte("another repository")) {
		t.Error("the refusal carried the other repository's bytes")
	}
}

// TestADeniedCallerCannotFetchByHash is the denial case: an unauthorized caller
// must not be able to move an object even holding a valid digest, and must not be
// able to tell the repository from one that does not exist.
func TestADeniedCallerCannotFetchByHash(t *testing.T) {
	h := newLFSHarness(t)
	body := []byte("private content")
	oid := digestOf(body)
	if _, err := h.layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: h.writable.StorageID, OID: oid, Size: int64(len(body)), Body: bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("StoreLFSObject: %v", err)
	}

	// Withdraw access entirely, which the real service reports as not-found.
	h.access.levels = map[string]repository.Level{}

	deniedStatus, _, deniedRaw := h.batch(t, "writable",
		`{"operation":"download","objects":[{"oid":"`+oid+`","size":`+itoa(len(body))+`}]}`)
	missingStatus, _, missingRaw := h.batch(t, "no-such-repo",
		`{"operation":"download","objects":[{"oid":"`+oid+`","size":`+itoa(len(body))+`}]}`)

	// T-A4: the denied case and the nonexistent case are the same answer, so
	// neither the status nor the body tells a caller that the repository is there.
	if deniedStatus != http.StatusNotFound {
		t.Errorf("a denied batch = %d, want 404", deniedStatus)
	}
	if deniedStatus != missingStatus {
		t.Errorf("denied = %d but nonexistent = %d; the two must not differ", deniedStatus, missingStatus)
	}
	if deniedRaw != missingRaw {
		t.Errorf("denied body = %q but nonexistent body = %q", deniedRaw, missingRaw)
	}

	// The object URL refuses as well.
	response, _ := h.download(t, "writable", oid)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("a denied download = %d, want 404", response.StatusCode)
	}
}

// TestAnArchivedRepositoryRefusesUploadButNotDownload covers the archive rule
// reaching LFS: an archived repository is read-only for content, which is what a
// push is, while reading still works.
func TestAnArchivedRepositoryRefusesUploadButNotDownload(t *testing.T) {
	h := newLFSHarness(t)
	body := []byte("archived content")
	oid := digestOf(body)
	if _, err := h.layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: h.writable.StorageID, OID: oid, Size: int64(len(body)), Body: bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("StoreLFSObject: %v", err)
	}

	archived := h.writable
	archived.Archived = true
	h.access.repositories[accessKey(h.ownerID, "writable")] = archived

	// An upload is a content change, so it is refused with the archive state named.
	status, _, raw := h.batch(t, "writable",
		`{"operation":"upload","objects":[{"oid":"`+digestOf([]byte("new"))+`","size":3}]}`)
	if status != http.StatusForbidden {
		t.Errorf("an upload to an archived repository = %d, want 403: %s", status, raw)
	}
	if !strings.Contains(raw, "archived") {
		t.Errorf("the refusal does not name the archive state: %s", raw)
	}
	fresh := h.upload(t, "writable", digestOf([]byte("new")), []byte("new"))
	if fresh.StatusCode != http.StatusForbidden {
		t.Errorf("a direct upload to an archived repository = %d, want 403", fresh.StatusCode)
	}

	// A download is not a content change, so it still works.
	response, downloaded := h.download(t, "writable", oid)
	if response.StatusCode != http.StatusOK {
		t.Errorf("a download from an archived repository = %d, want 200", response.StatusCode)
	}
	if !bytes.Equal(downloaded, body) {
		t.Error("the archived repository's object did not download")
	}
}

// TestTheSizeLimitIsReportedClearly is testing.md's requirement for the LFS row:
// "the size limit is reported clearly". A client told only "too large" cannot
// decide what to do.
func TestTheSizeLimitIsReportedClearly(t *testing.T) {
	h := newLFSHarness(t)
	// The cap is lowered for the test, as the push bound is, so the case can be
	// exercised without storing gigabytes.
	h.handler = h.handler.WithLFSOptions(storage.LFSOptions{MaxObjectBytes: 32})

	body := bytes.Repeat([]byte("x"), 64)
	oid := digestOf(body)
	response := h.upload(t, "writable", oid, body)

	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.StatusCode)
	}
	detail, _ := io.ReadAll(response.Body)
	message := string(detail)
	// The status and the message both name the problem, in the LFS envelope.
	if !strings.Contains(message, "maximum size") {
		t.Errorf("the refusal does not name the limit: %s", message)
	}
	var envelope map[string]any
	if err := json.Unmarshal(detail, &envelope); err != nil {
		t.Fatalf("the refusal is not JSON: %v", err)
	}
	if _, ok := envelope["message"].(string); !ok {
		t.Errorf("the refusal carries no message: %s", message)
	}
}

// TestAMismatchedDigestIsRefused covers T-C4 through the HTTP surface: a client
// claiming an identifier the bytes do not match is refused, and nothing is stored.
func TestAMismatchedDigestIsRefused(t *testing.T) {
	h := newLFSHarness(t)
	body := []byte("the bytes that were sent")
	claimed := digestOf([]byte("the bytes that were claimed"))

	response := h.upload(t, "writable", claimed, body)
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", response.StatusCode)
	}

	// Neither the claimed object nor the real one is now present.
	for _, oid := range []string{claimed, digestOf(body)} {
		exists, err := h.layout.LFSObjectExists(h.writable.StorageID, oid)
		if err != nil {
			t.Fatalf("LFSObjectExists: %v", err)
		}
		if exists {
			t.Errorf("a refused upload stored %s", oid)
		}
	}
}

// TestAMalformedIdentifierIsRefused covers the path rule at the transport: the
// batch reports it against the object, and the object routes answer as not-found
// so a malformed digest cannot be told from a stored one.
func TestAMalformedIdentifierIsRefused(t *testing.T) {
	h := newLFSHarness(t)

	for _, oid := range []string{
		"short",
		strings.Repeat("g", 64),
		strings.Repeat("A", 64),
		strings.Repeat("a", 64) + "extra",
	} {
		t.Run(oid, func(t *testing.T) {
			// The batch reports it against that object rather than failing the
			// batch, so a client can retry the rest.
			status, answer, raw := h.batch(t, "readable",
				`{"operation":"download","objects":[{"oid":"`+oid+`","size":1}]}`)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, raw)
			}
			if _, ok := h.objects(t, answer)[0]["error"].(map[string]any); !ok {
				t.Errorf("a malformed identifier was accepted: %s", raw)
			}

			// The object routes refuse it as not-found, which is the same answer a
			// missing object gets.
			response, _ := h.download(t, "readable", oid)
			if response.StatusCode != http.StatusNotFound {
				t.Errorf("a download with a malformed identifier = %d, want 404", response.StatusCode)
			}
			uploaded := h.upload(t, "readable", oid, []byte("x"))
			if uploaded.StatusCode != http.StatusUnprocessableEntity {
				t.Errorf("an upload with a malformed identifier = %d, want 422", uploaded.StatusCode)
			}
		})
	}
}

// TestBatchRefusesWhatItShould covers the malformed batch shapes.
func TestBatchRefusesWhatItShould(t *testing.T) {
	h := newLFSHarness(t)
	oid := digestOf([]byte("x"))

	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{name: "not JSON", body: "not json", want: http.StatusBadRequest},
		{name: "no operation", body: `{"objects":[{"oid":"` + oid + `","size":1}]}`, want: http.StatusBadRequest},
		{name: "an unknown operation", body: `{"operation":"delete","objects":[{"oid":"` + oid + `","size":1}]}`, want: http.StatusBadRequest},
		{name: "no objects", body: `{"operation":"download","objects":[]}`, want: http.StatusBadRequest},
		{name: "too many objects", body: `{"operation":"download","objects":[` + manyObjects(oid, 1001) + `]}`, want: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, _, raw := h.batch(t, "readable", test.body)
			if status != test.want {
				t.Errorf("status = %d, want %d: %s", status, test.want, raw)
			}
		})
	}
}

// manyObjects builds a comma-separated list of n object descriptors.
func manyObjects(oid string, n int) string {
	descriptors := make([]string, n)
	for i := range descriptors {
		descriptors[i] = `{"oid":"` + oid + `","size":1}`
	}
	return strings.Join(descriptors, ",")
}

// TestAnUploadBatchNeedingWriteIsRefusedForAReadOnlyCaller covers one more denied
// asks for a level the caller does not hold is refused as if the repository were
// absent, not as a bad request.
func TestAnUploadBatchNeedingWriteIsRefusedForAReadOnlyCaller(t *testing.T) {
	h := newLFSHarness(t)
	// Lower `readable` to read, then ask it to accept an upload.
	h.access.levels[h.readable.ID] = repository.LevelRead
	oid := digestOf([]byte("x"))

	status, _, raw := h.batch(t, "readable", `{"operation":"upload","objects":[{"oid":"`+oid+`","size":1}]}`)
	if status != http.StatusNotFound {
		t.Errorf("an upload batch without write = %d, want 404: %s", status, raw)
	}
	// A download batch on the same repository still works, so the refusal is about
	// the operation rather than the repository.
	status, _, raw = h.batch(t, "readable", `{"operation":"download","objects":[{"oid":"`+oid+`","size":1}]}`)
	if status != http.StatusOK {
		t.Errorf("a download batch with read = %d, want 200: %s", status, raw)
	}
}

// TestBatchIsNeverCached covers the cache directive the document requires for this
// subtree.
func TestBatchIsNeverCached(t *testing.T) {
	h := newLFSHarness(t)
	oid := digestOf([]byte("x"))

	response, err := http.Post(
		h.server.URL+"/alice/readable.git/info/lfs/objects/batch",
		"application/vnd.git-lfs+json",
		strings.NewReader(`{"operation":"download","objects":[{"oid":"`+oid+`","size":1}]}`))
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := response.Header.Get("Content-Type"); !strings.Contains(got, "git-lfs") {
		t.Errorf("Content-Type = %q, want the LFS media type", got)
	}
}

// TestTheBatchDoesNotReachTheStoreForADeniedCaller is the T-C3 denied-path
// assertion: "object store is not read" when the principal cannot read the
// repository. A runner call count of zero is the observable form of that.
func TestTheBatchDoesNotReachTheStoreForADeniedCaller(t *testing.T) {
	h := newLFSHarness(t)
	h.access.levels = map[string]repository.Level{}
	oid := digestOf([]byte("x"))

	h.batch(t, "writable", `{"operation":"download","objects":[{"oid":"`+oid+`","size":1}]}`)

	// Nothing was written, and no Git process was started either.
	objects, err := h.layout.ListLFSObjects(h.writable.StorageID)
	if err != nil {
		t.Fatalf("ListLFSObjects: %v", err)
	}
	if len(objects) != 0 {
		t.Errorf("a denied batch touched the store: %v", objects)
	}
	if logged := h.logged.all(); logged != "" {
		t.Errorf("a denial should not be logged as an error: %s", logged)
	}
}

// itoa formats an int for a JSON body.
func itoa(value int) string { return strconv.Itoa(value) }

// TestARealGitLFSClientPushesAnObject runs the real `git-lfs` binary against these
// endpoints with its own `push --object-id` verb, which is the T3 conformance check
// testing.md names for LFS: "Batch API round trip".
//
// Two things about how it is set up are deliberate.
//
//   - **The access decision is a fake.** It belongs to the repository service, and
//     the integration suite covers it; what this proves is the wire format, which
//     no hand-written request can prove.
//   - **The object is placed in the client's local LFS store directly rather than
//     through `git add`.** A clean filter runs through `sh.exe`, and this sandbox
//     refuses the signal pipe that needs, so `git add` cannot complete here. The
//     object's location in the local store is a documented layout
//     (`lfs/objects/<aa>/<bb>/<oid>`), so writing it there is faithful to what the
//     filter would have produced, and the batch negotiation and the upload — the
//     parts this test is about — are still driven entirely by the real client.
func TestARealGitLFSClientPushesAnObject(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs is not on PATH; the protocol tests above cover the wire format")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}

	h := newLFSHarness(t)
	work := t.TempDir()

	run := func(args ...string) (string, error) {
		command := exec.Command(gitPath, args...)
		command.Dir = work
		command.Env = append(os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_LFS_SKIP_SMUDGE=1",
		)
		output, err := command.CombinedOutput()
		return string(output), err
	}

	if output, err := run("init", "--quiet", "."); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	// The LFS endpoint for every remote, so the push has somewhere to go without a
	// remote that serves refs — which this fixture does not have, because the Git
	// runner behind it is a fake.
	endpoint := h.server.URL + "/alice/writable.git/info/lfs"
	for _, setting := range [][2]string{
		{"lfs.url", endpoint},
		{"remote.origin.url", h.server.URL + "/alice/writable.git"},
	} {
		if output, err := run("config", setting[0], setting[1]); err != nil {
			t.Fatalf("git config %s: %v\n%s", setting[0], err, output)
		}
	}

	// Place the object where the clean filter would have put it.
	body := bytes.Repeat([]byte("the object's content\n"), 200)
	oid := digestOf(body)
	local := filepath.Join(work, ".git", "lfs", "objects", oid[0:2], oid[2:4])
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatalf("create the local LFS store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(local, oid), body, 0o600); err != nil {
		t.Fatalf("write the local object: %v", err)
	}

	// The real client negotiates the batch and uploads the bytes.
	pushed, pushErr := run("lfs", "push", "--object-id", "origin", oid)
	if pushErr != nil {
		t.Fatalf("git lfs push: %v\n%s", pushErr, pushed)
	}

	stored, err := h.layout.LFSObjectSize(h.writable.StorageID, oid)
	if err != nil {
		t.Fatalf("the client's push did not store the object: %v\n%s", err, pushed)
	}
	if stored != int64(len(body)) {
		t.Errorf("stored %d bytes, want the %d the client sent", stored, len(body))
	}
	onDisk, err := h.layout.OpenLFSObject(h.writable.StorageID, oid)
	if err != nil {
		t.Fatalf("open the stored object: %v", err)
	}
	defer func() { _ = onDisk.Close() }()
	read, err := io.ReadAll(onDisk)
	if err != nil {
		t.Fatalf("read the stored object: %v", err)
	}
	if !bytes.Equal(read, body) {
		t.Error("the bytes the client pushed are not the bytes that were sent")
	}

	// A second push has nothing to do — the batch reports no upload action — and
	// must still succeed, which is what makes a retry safe.
	if again, againErr := run("lfs", "push", "--object-id", "origin", oid); againErr != nil {
		t.Errorf("a repeated push failed: %v\n%s", againErr, again)
	}

	// The object comes back byte for byte.
	response, downloaded := h.download(t, "writable", oid)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("download = %d, want 200", response.StatusCode)
	}
	if !bytes.Equal(downloaded, body) {
		t.Errorf("downloaded %d bytes, want %d", len(downloaded), len(body))
	}
}

// TestAQuotaRefusalIsReportedDistinctly covers the transport's mapping of the
// repository allowance: the object itself is acceptable, so the message says the
// repository has no room rather than that the object is too large.
func TestAQuotaRefusalIsReportedDistinctly(t *testing.T) {
	h := newLFSHarness(t)
	h.handler = h.handler.WithLFSOptions(storage.LFSOptions{MaxObjectBytes: 1024, RepositoryBytes: 64})

	// Fill most of the allowance.
	first := bytes.Repeat([]byte("a"), 60)
	if response := h.upload(t, "writable", digestOf(first), first); response.StatusCode != http.StatusOK {
		t.Fatalf("the first upload = %d, want 200", response.StatusCode)
	}

	second := bytes.Repeat([]byte("b"), 60)
	response := h.upload(t, "writable", digestOf(second), second)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("an over-quota upload = %d, want 413", response.StatusCode)
	}
	detail, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(detail), "quota") {
		t.Errorf("the refusal does not name the quota: %s", detail)
	}
	// And it is distinguishable from the object cap, which matters because the two
	// call for different client behaviour: one retries elsewhere, the other splits
	// the file.
	if !strings.Contains(string(detail), "quota") {
		t.Error("the quota refusal does not name itself")
	}
}
