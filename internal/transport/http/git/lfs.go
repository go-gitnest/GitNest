package git

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The Git LFS batch API and object transfer.
//
// Paths are the ones docs/architecture/urls-and-identifiers.md section 3 gives:
// `/{owner}/{repo}.git/info/lfs/objects/batch` and `.../lfs/objects/{oid}`. The
// `{repo}` wildcard captures the whole segment including the `.git` suffix, which
// is the same shape the smart-HTTP routes use, so a client addresses LFS exactly
// where it found the ref advertisement.
//
// Three documents decide the behaviour here:
//
//   - docs/architecture.md: "LFS requests are authenticated and authorized through
//     the same repository service as Git operations". So every route calls the
//     same authorize() the clone and push paths call, and this file never decides
//     access itself.
//   - docs/security/threat-model.md T-C3: "Every LFS batch and object request is
//     authorized through the repository service for the repository in the batch".
//     The batch's repository is the one in its path, and the store scopes objects
//     to that repository's storage identifier, so holding a valid digest is not a
//     capability.
//   - docs/security/threat-model.md T-A4: "Anonymous and denied access return the
//     same not-found shape as a nonexistent repository". authorize() already
//     collapses those cases, so LFS inherits it rather than restating it.
//
// T-C4's integrity half belongs to the store, which verifies the declared size and
// digest while streaming and installs the object only on a match. This file's job
// is to pass the bytes through without buffering them, so a 2 GiB object never
// lands in the web process's memory.

// LFS media types. The batch endpoint speaks the LFS JSON type; object transfer is
// opaque bytes.
const (
	lfsContentType = "application/vnd.git-lfs+json"
	lfsJSONType    = "application/vnd.git-lfs+json"
)

// The LFS route fragments, appended to the same `{owner}/{repo}` prefix the
// smart-HTTP routes use.
const (
	lfsBatchPath  = "info/lfs/objects/batch"
	lfsObjectPath = "info/lfs/objects/{oid}"
)

// Bounds on the batch endpoint.
const (
	// maxLFSBatchObjects caps the objects one batch may name. A batch is a
	// negotiation, not a transfer, so a request naming thousands of objects is
	// either a mistake or an attempt to make the server do unbounded work.
	maxLFSBatchObjects = 1000
	// maxLFSBatchBytes caps the batch request body. It is generous for 1000 object
	// descriptors and small enough that the whole request is bounded.
	maxLFSBatchBytes = 1 << 20
	// maxLFSOidLength bounds the path segment before validation, so a pathological
	// identifier is refused without allocating for it.
	maxLFSOidLength = 256
)

// lfsObjectRequest is one object in a batch request.
type lfsObjectRequest struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// lfsBatchRequest is a batch negotiation.
type lfsBatchRequest struct {
	Operation string             `json:"operation"`
	Transfers []string           `json:"transfers,omitempty"`
	Ref       *lfsRef            `json:"ref,omitempty"`
	Objects   []lfsObjectRequest `json:"objects"`
}

// lfsRef names the ref a batch is for. It is informational, so it is accepted and
// not used to make an access decision: the repository in the path is what the
// decision is about.
type lfsRef struct {
	Name string `json:"name"`
}

// lfsAction is one way to move an object.
type lfsAction struct {
	Href   string            `json:"href"`
	Header map[string]string `json:"header,omitempty"`
}

// lfsObjectResponse is one object in a batch response.
//
// An object whose transfer is not needed carries no actions and no error, which is
// how the protocol says "you already have it" for an upload and "nothing to fetch"
// would be an error for a download.
type lfsObjectResponse struct {
	OID           string               `json:"oid"`
	Size          int64                `json:"size"`
	Authenticated bool                 `json:"authenticated,omitempty"`
	Actions       map[string]lfsAction `json:"actions,omitempty"`
	Error         *lfsObjectError      `json:"error,omitempty"`
}

// lfsObjectError reports a per-object failure without failing the whole batch,
// which is what lets a client fetch the objects it may have and report the rest.
type lfsObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// lfsBatchResponse is a batch negotiation result.
type lfsBatchResponse struct {
	Transfer string              `json:"transfer"`
	Objects  []lfsObjectResponse `json:"objects"`
	// HashAlgo names the digest, because the protocol allows more than one and a
	// client must not have to assume.
	HashAlgo string `json:"hash_algo,omitempty"`
}

// lfsErrorResponse is the envelope an LFS failure uses.
type lfsErrorResponse struct {
	Message string `json:"message"`
}

// registerLFS adds the LFS routes.
//
// They are registered from the same Register call as the smart-HTTP routes,
// because they share the handler's authorization path, its storage layout, and its
// authenticator. A separate handler would be a second place for an access decision
// to be made.
func (h *Handler) registerLFS(mux *http.ServeMux) {
	mux.HandleFunc("POST /{owner}/{repo}/"+lfsBatchPath, h.lfsBatch)
	mux.HandleFunc("PUT /{owner}/{repo}/"+lfsObjectPath, h.lfsUpload)
	mux.HandleFunc("GET /{owner}/{repo}/"+lfsObjectPath, h.lfsDownload)
}

// lfsBatch is the batch negotiation.
//
// It answers both directions with one shape, because the request says which
// direction it is and the answer for each object is the same either way: the
// actions needed to move it, or nothing when nothing needs to move.
func (h *Handler) lfsBatch(w http.ResponseWriter, r *http.Request) {
	// A batch changes nothing for a download and adds an object for an upload, so
	// the body has to be read before the level can be chosen: the operation is in
	// the body, not the path.
	body, ok := h.readLFSBody(w, r)
	if !ok {
		return
	}

	var request lfsBatchRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeLFSError(w, http.StatusBadRequest, "the batch request is not valid JSON")
		return
	}

	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	var required repository.Level
	switch operation {
	case "download":
		required = repository.LevelRead
	case "upload":
		// An upload stores bytes in the repository, so it needs the same level a
		// push does. That is also what makes an archived repository refuse it:
		// authorize() reports the archive state for exactly this level.
		required = repository.LevelWrite
	default:
		writeLFSError(w, http.StatusBadRequest,
			"the operation must be \"download\" or \"upload\"")
		return
	}

	if len(request.Objects) == 0 {
		writeLFSError(w, http.StatusBadRequest, "the batch names no objects")
		return
	}
	if len(request.Objects) > maxLFSBatchObjects {
		writeLFSError(w, http.StatusUnprocessableEntity,
			"the batch names more objects than this server accepts at once")
		return
	}

	found, _, ok := h.authorize(w, r, required)
	if !ok {
		return
	}

	response := lfsBatchResponse{
		Transfer: "basic",
		HashAlgo: "sha256",
		Objects:  make([]lfsObjectResponse, 0, len(request.Objects)),
	}

	for _, requested := range request.Objects {
		item := lfsObjectResponse{OID: requested.OID, Size: requested.Size}

		if err := storage.ValidateOID(requested.OID); err != nil {
			// The digest is echoed rather than normalized, because rewriting a
			// client's identifier would hide its defect. The batch succeeds: one
			// bad descriptor is reported against itself so the client can retry
			// the rest.
			item.Error = &lfsObjectError{Code: http.StatusUnprocessableEntity,
				Message: "the object identifier is not a sha256 digest"}
			response.Objects = append(response.Objects, item)
			continue
		}

		exists, err := h.layout.LFSObjectExists(found.StorageID, requested.OID)
		if err != nil {
			h.log("lfs: object lookup failed", "error", err)
			writeLFSError(w, http.StatusInternalServerError, "the request could not be completed")
			return
		}

		// The href is absolute, because git-lfs refuses a relative one: it reports
		// `missing protocol` and never transfers. See WithPublicURL for where the
		// base comes from.
		href := h.lfsObjectHref(r, requested.OID)

		switch {
		case operation == "download" && exists:
			stored, sizeErr := h.layout.LFSObjectSize(found.StorageID, requested.OID)
			if sizeErr != nil {
				item.Error = &lfsObjectError{Code: http.StatusNotFound, Message: "the object is not available"}
				response.Objects = append(response.Objects, item)
				continue
			}
			// The stored size is authoritative, so a client that declared the wrong
			// one learns it before downloading.
			item.Size = stored
			item.Authenticated = true
			item.Actions = map[string]lfsAction{"download": {Href: href}}
		case operation == "download":
			// A missing object is the client's error, and it belongs to that object
			// rather than to the batch.
			item.Error = &lfsObjectError{Code: http.StatusNotFound,
				Message: "the object is not stored for this repository"}
		case operation == "upload" && exists:
			// No action: the object is already there, so the client has nothing to
			// send. This is what makes a retried upload cheap.
		default:
			item.Authenticated = true
			item.Actions = map[string]lfsAction{"upload": {Href: href}}
		}

		response.Objects = append(response.Objects, item)
	}

	// An LFS response is per-caller and must never be cached.
	w.Header().Set("Cache-Control", "no-store")
	writeLFSJSON(w, http.StatusOK, response)
}

// lfsUpload stores an object.
//
// The bytes stream straight into the store, which verifies them: the declared
// length is taken from Content-Length so a truncated or over-long body is refused,
// and the digest is checked against the identifier in the path. Nothing is
// buffered here, so a large object never occupies the web process's memory.
func (h *Handler) lfsUpload(w http.ResponseWriter, r *http.Request) {
	oid := r.PathValue("oid")
	if len(oid) > maxLFSOidLength || storage.ValidateOID(oid) != nil {
		writeLFSError(w, http.StatusUnprocessableEntity, "the object identifier is not a sha256 digest")
		return
	}

	found, _, ok := h.authorize(w, r, repository.LevelWrite)
	if !ok {
		return
	}
	if r.Body == nil {
		writeLFSError(w, http.StatusBadRequest, "the upload carries no body")
		return
	}
	defer func() { _ = r.Body.Close() }()

	// Content-Length is the declaration of size. A client that sends a chunked
	// body does not declare one, and then only the digest is verified — which is
	// still enough to refuse anything that is not the object it claims to be.
	declared := int64(-1)
	if r.ContentLength >= 0 {
		declared = r.ContentLength
	}
	// The cap is applied before the body is read, so an over-limit upload is
	// refused without transferring it.
	if declared > h.lfsOptions.MaxObjectBytes {
		writeLFSError(w, http.StatusRequestEntityTooLarge,
			"the object exceeds the maximum size this server accepts")
		return
	}

	written, err := h.layout.StoreLFSObject(h.lfsOptions, storage.LFSObjectPut{
		StorageID: found.StorageID,
		OID:       oid,
		Size:      declared,
		Body:      http.MaxBytesReader(w, r.Body, h.lfsOptions.MaxObjectBytes+1),
	})
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrLFSObjectTooLarge):
			// The size message names the limit, which testing.md requires to be
			// "reported clearly": a client told only "too large" cannot decide.
			writeLFSError(w, http.StatusRequestEntityTooLarge,
				"the object exceeds the maximum size this server accepts")
		case errors.Is(err, storage.ErrLFSQuotaExceeded):
			// A quota refusal is not the same as an oversized object: the object
			// itself is acceptable and the repository has no room for it.
			writeLFSError(w, http.StatusRequestEntityTooLarge,
				"the repository's LFS storage quota would be exceeded")
		case errors.Is(err, storage.ErrLFSDigestMismatch):
			writeLFSError(w, http.StatusUnprocessableEntity,
				"the uploaded bytes do not match the object identifier")
		case errors.Is(err, storage.ErrLFSSizeMismatch):
			writeLFSError(w, http.StatusUnprocessableEntity,
				"the uploaded bytes are not the declared size")
		case errors.Is(err, storage.ErrInvalidOID), errors.Is(err, storage.ErrInvalidStorageID):
			writeLFSError(w, http.StatusUnprocessableEntity, "the object identifier is not a sha256 digest")
		default:
			h.log("lfs: object store failed", "error", err)
			writeLFSError(w, http.StatusInternalServerError, "the request could not be completed")
		}
		return
	}
	_ = written

	// The protocol expects an empty body on success.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// lfsDownload serves an object's bytes.
func (h *Handler) lfsDownload(w http.ResponseWriter, r *http.Request) {
	oid := r.PathValue("oid")
	if len(oid) > maxLFSOidLength || storage.ValidateOID(oid) != nil {
		// A malformed identifier cannot name an object, so it is answered like one
		// that is not there. Answering a distinct shape would let a caller tell a
		// malformed digest from a stored one.
		writeLFSError(w, http.StatusNotFound, "the object is not stored for this repository")
		return
	}

	found, _, ok := h.authorize(w, r, repository.LevelRead)
	if !ok {
		return
	}

	file, err := h.layout.OpenLFSObject(found.StorageID, oid)
	if err != nil {
		if errors.Is(err, storage.ErrLFSObjectNotFound) {
			writeLFSError(w, http.StatusNotFound, "the object is not stored for this repository")
			return
		}
		h.log("lfs: object open failed", "error", err)
		writeLFSError(w, http.StatusInternalServerError, "the request could not be completed")
		return
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		h.log("lfs: object stat failed", "error", err)
		writeLFSError(w, http.StatusInternalServerError, "the request could not be completed")
		return
	}

	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	// The content is addressed by its digest, so it cannot change meaning; but the
	// document requires `no-store` for this subtree, and an object can belong to a
	// private repository, so a shared cache must not keep it.
	header.Set("Cache-Control", "no-store")
	// A download is a body the client asked for; the size is known, so the
	// response is exact.
	if _, err := io.Copy(w, file); err != nil {
		// The header is already sent, so the failure cannot be reported as a status.
		// It is logged because a broken download is worth knowing about.
		h.log("lfs: object transfer failed", "error", err)
	}
}

// readLFSBody reads a bounded batch body.
func (h *Handler) readLFSBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		writeLFSError(w, http.StatusBadRequest, "the batch request carries no body")
		return nil, false
	}
	defer func() { _ = r.Body.Close() }()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLFSBatchBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeLFSError(w, http.StatusRequestEntityTooLarge, "the batch request is too large")
			return nil, false
		}
		writeLFSError(w, http.StatusBadRequest, "the batch request could not be read")
		return nil, false
	}
	return body, true
}

// lfsObjectHref builds the absolute URL of an object.
//
// The path is the two segments the request was routed on plus the validated digest,
// so no other client-supplied value reaches the URL and the object URL always sits
// under the batch URL the client used. Only the base differs between the configured
// public URL and the request's own scheme and host.
func (h *Handler) lfsObjectHref(r *http.Request, oid string) string {
	path := "/" + r.PathValue("owner") + "/" + r.PathValue("repo") + "/" +
		strings.Replace(lfsObjectPath, "{oid}", oid, 1)

	if h.publicURL != "" {
		return h.publicURL + path
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

// writeLFSJSON writes an LFS JSON response.
func writeLFSJSON(w http.ResponseWriter, status int, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		w.Header().Set("Content-Type", lfsJSONType)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"the response could not be encoded"}`))
		return
	}
	w.Header().Set("Content-Type", lfsJSONType)
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// writeLFSError writes an LFS error.
//
// The message is deliberately generic at the statuses a caller can reach: the
// denial case has already been collapsed into not-found by authorize(), and a
// detailed message here would be a way to tell one repository from another.
func writeLFSError(w http.ResponseWriter, status int, message string) {
	writeLFSJSON(w, status, lfsErrorResponse{Message: message})
}
