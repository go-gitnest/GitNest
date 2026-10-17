package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/google/uuid"
)

// The LFS object store.
//
// Three documents decide its shape, and this file is where they are enforced:
//
//   - docs/architecture.md: "LFS requests are authenticated and authorized
//     through the same repository service as Git operations, and object storage is
//     addressed by a content hash rather than a user-supplied path." So an object
//     is named by its digest and nothing a client sends becomes a path.
//   - docs/architecture/performance.md: "Uploads write to a temporary path inside
//     the storage root, verify a disk-space check against the declared or streamed
//     size, and rename into place. An interrupted upload leaves no blob reachable
//     from a version record." So staging happens inside the storage root, which is
//     what makes the install an atomic rename on one filesystem, and an object
//     becomes reachable only after its bytes are complete and verified.
//   - docs/security/threat-model.md T-C4: "The declared size and digest are
//     verified against the received bytes before the object is finalized; a
//     mismatch rejects the object and leaves no entry." So verification happens
//     during the stream rather than after the file is in place.
//
// The isolation in T-C3 ("objects are addressed by verified content hash and
// served only for a repository the principal can read") has two halves. The
// authorization half belongs to the repository service, and the store deliberately
// does not implement it. The other half is here: an object lives under the
// repository's own storage identifier, so a caller who guesses a valid digest
// belonging to another repository finds nothing rather than someone else's bytes.

const (
	// lfsDirectory is the subtree of the storage root that holds LFS objects. It
	// is a reserved name beside the repository fan-out, so it can never collide
	// with a repository directory — those end in ".git".
	lfsDirectory = "lfs"
	// lfsStagingDirectory holds uploads that are not yet installed. It sits inside
	// the storage root, not the temporary root, so the install is a rename within
	// one filesystem: a rename across filesystems fails, and a copy would not be
	// atomic.
	lfsStagingDirectory = ".staging"
	// lfsFanOutDepth matches the repository layout, so a directory holding many
	// objects does not grow one enormous directory.
	lfsFanOutDepth = 2
	// oidLength is the length of a SHA-256 digest in lowercase hexadecimal, which
	// is the object identifier the LFS protocol uses.
	oidLength = 64
)

// LFSOidLength is the length of a valid object identifier.
//
// It is exported because a transport has to validate the identifier a client
// supplied before it does anything else with it.
const LFSOidLength = oidLength

// Errors the object store reports.
var (
	// ErrInvalidOID reports an object identifier that is not a SHA-256 digest.
	ErrInvalidOID = errors.New("an LFS object identifier must be 64 lowercase hexadecimal characters")
	// ErrLFSDigestMismatch reports bytes whose digest is not the identifier they
	// were uploaded under.
	ErrLFSDigestMismatch = errors.New("the uploaded bytes do not match the object identifier")
	// ErrLFSSizeMismatch reports bytes whose length is not the declared size.
	ErrLFSSizeMismatch = errors.New("the uploaded bytes are not the declared size")
	// ErrLFSObjectTooLarge reports an object over the documented per-object cap.
	ErrLFSObjectTooLarge = errors.New("the object exceeds the maximum LFS object size")
	// ErrLFSObjectNotFound reports an object the store does not hold.
	ErrLFSObjectNotFound = errors.New("the LFS object is not stored")
	// ErrLFSQuotaExceeded reports an object that would take a repository past its
	// byte allowance.
	ErrLFSQuotaExceeded = errors.New("the repository's LFS storage quota would be exceeded")
)

// LFSOptions are the bounds a store applies. They come from startup
// configuration, because docs/architecture/performance.md gives them as
// settings rather than constants.
type LFSOptions struct {
	// MaxObjectBytes is `git.lfs_max_object_bytes`, documented as 2 GiB.
	MaxObjectBytes int64
	// RepositoryBytes is `git.lfs_repository_bytes`, documented as 50 GiB per
	// repository. A value of zero or less means the repository-level allowance is
	// not enforced, which is what a caller that only cares about the per-object cap
	// wants.
	//
	// It is deliberately independent of MaxObjectBytes: the allowance is a budget
	// and the cap is a per-file bound, so an operator may set a tight budget with a
	// generous per-file cap. Coupling them would refuse to start on a coherent
	// policy.
	RepositoryBytes int64
}

// DefaultLFSOptions returns the documented defaults.
func DefaultLFSOptions() LFSOptions {
	return LFSOptions{
		MaxObjectBytes:  2 << 30,
		RepositoryBytes: 50 << 30,
	}
}

// Validate checks that the bounds are usable, so a misconfiguration fails at
// startup rather than on the first upload.
func (o LFSOptions) Validate() error {
	if o.MaxObjectBytes <= 0 {
		return errors.New("the maximum LFS object size must be positive")
	}
	// A non-positive RepositoryBytes means "not enforced" rather than invalid, so a
	// caller that bounds only the object size is not forced to invent a budget.
	return nil
}

// ValidateOID reports whether a value is a well-formed object identifier.
//
// Lowercase only: the digest is compared and used as a path component, and
// accepting both cases would create two names for one object. A client sending
// uppercase is rejected rather than normalized, because silently rewriting a
// client's identifier hides a client defect.
func ValidateOID(oid string) error {
	if len(oid) != oidLength {
		return fmt.Errorf("%w: got %d characters", ErrInvalidOID, len(oid))
	}
	for i := 0; i < len(oid); i++ {
		c := oid[i]
		isDigit := c >= '0' && c <= '9'
		isLower := c >= 'a' && c <= 'f'
		if !isDigit && !isLower {
			return fmt.Errorf("%w: %q is not a lowercase hexadecimal digit", ErrInvalidOID, string(c))
		}
	}
	return nil
}

// LFSPath returns the path an object is stored at, without touching the
// filesystem.
//
// The path is derived from the repository's storage identifier and the validated
// digest, so no client-supplied value reaches the filesystem and a digest from
// another repository resolves under that repository rather than this one.
func (l *Layout) LFSPath(storageID, oid string) (string, error) {
	canonical, err := canonicalStorageID(storageID)
	if err != nil {
		return "", err
	}
	if err := ValidateOID(oid); err != nil {
		return "", err
	}

	parts := []string{l.root, lfsDirectory, canonical}
	for i := 0; i < lfsFanOutDepth; i++ {
		parts = append(parts, oid[i*2:i*2+2])
	}
	parts = append(parts, oid)

	derived := filepath.Join(parts...)
	if err := l.checkContained(derived); err != nil {
		return "", err
	}
	return derived, nil
}

// LFSObjectExists reports whether an object is stored for a repository.
func (l *Layout) LFSObjectExists(storageID, oid string) (bool, error) {
	path, err := l.LFSPath(storageID, oid)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat the LFS object: %w", err)
	}
	// A directory at an object path is not an object; it is either damage or a
	// deliberate attempt to make a path behave unexpectedly.
	if info.IsDir() {
		return false, nil
	}
	return true, nil
}

// LFSObjectSize returns the stored size of an object.
func (l *Layout) LFSObjectSize(storageID, oid string) (int64, error) {
	path, err := l.LFSPath(storageID, oid)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrLFSObjectNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("stat the LFS object: %w", err)
	}
	if info.IsDir() {
		return 0, ErrLFSObjectNotFound
	}
	return info.Size(), nil
}

// OpenLFSObject opens an object for reading.
//
// The caller owns the returned file. It is opened read-only, so a download cannot
// modify what it serves.
func (l *Layout) OpenLFSObject(storageID, oid string) (*os.File, error) {
	path, err := l.LFSPath(storageID, oid)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrLFSObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open the LFS object: %w", err)
	}
	return file, nil
}

// LFSObjectPut describes an object a caller is storing.
type LFSObjectPut struct {
	// StorageID is the repository the object belongs to.
	StorageID string
	// OID is the declared digest, which the bytes must match.
	OID string
	// Size is the declared length. A negative value means the size was not
	// declared, in which case only the digest is verified.
	Size int64
	// Body is the object's bytes.
	Body io.Reader
}

// StoreLFSObject writes an object, verifying it before it becomes reachable.
//
// The bytes are streamed once, into a staging file inside the storage root, while
// both the running digest and the length are computed. Only if both match the
// declaration is the object renamed into place — so an interrupted, oversized, or
// mismatched upload leaves no object behind, and a reader can never observe a
// partial one. That is T-C4's "verified against the received bytes before the
// object is finalized", and performance.md's "an interrupted upload leaves no blob
// reachable".
//
// It reports the number of bytes stored. Storing an object that already exists
// with the same content is not an error and rewrites nothing, so a client
// retrying a partial upload is idempotent, which the LFS protocol expects.
func (l *Layout) StoreLFSObject(options LFSOptions, put LFSObjectPut) (int64, error) {
	if err := options.Validate(); err != nil {
		return 0, err
	}
	path, err := l.LFSPath(put.StorageID, put.OID)
	if err != nil {
		return 0, err
	}
	if put.Body == nil {
		return 0, errors.New("an LFS object upload needs a body")
	}
	if put.Size > options.MaxObjectBytes {
		return 0, fmt.Errorf("%w: %d bytes exceeds the %d-byte cap",
			ErrLFSObjectTooLarge, put.Size, options.MaxObjectBytes)
	}

	// An object already stored is left alone. Its content is its digest, so a
	// second copy of the same digest has the same bytes by definition; verifying
	// that again would read the whole object to learn nothing.
	if exists, err := l.LFSObjectExists(put.StorageID, put.OID); err != nil {
		return 0, err
	} else if exists {
		return l.LFSObjectSize(put.StorageID, put.OID)
	}

	// The repository allowance is checked before anything is written, so an upload
	// that cannot fit is refused having transferred nothing. The declared size is
	// what it is checked against, because the accepted bytes are verified against it.
	//
	// **The check is best-effort under concurrency, and that is a known gap.**
	// docs/architecture/performance.md specifies this quota as checked "under a
	// transaction lock", which needs the per-repository counter that
	// docs/architecture/data-model.md does not define. Two uploads that pass this
	// check at the same moment can therefore both land and take a repository past
	// its allowance. The check still stops the ordinary case — one client filling the
	// disk — and the exact version awaits the schema decision recorded in the
	// roadmap. The per-object cap is unaffected: it is enforced on the bytes.
	if put.Size >= 0 && options.RepositoryBytes > 0 {
		used, err := l.LFSUsage(put.StorageID)
		if err != nil {
			return 0, err
		}
		if used+put.Size > options.RepositoryBytes {
			return 0, fmt.Errorf("%w: %d bytes stored plus %d requested exceeds the %d-byte allowance",
				ErrLFSQuotaExceeded, used, put.Size, options.RepositoryBytes)
		}
	}

	staging, err := l.newLFSStagingPath()
	if err != nil {
		return 0, err
	}
	// The staging file is removed on every exit path, including a panic, so a
	// failed upload cannot accumulate.
	defer func() { _ = os.Remove(staging) }()

	file, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create the LFS staging file: %w", err)
	}

	digest := sha256.New()
	// One byte past the cap is read deliberately: reaching it proves the body is
	// over the limit without buffering the whole thing, so an oversized upload is
	// refused having written only the cap.
	limit := options.MaxObjectBytes
	if put.Size >= 0 && put.Size < limit {
		limit = put.Size
	}
	written, copyErr := io.Copy(io.MultiWriter(file, digest), io.LimitReader(put.Body, limit+1))

	closeErr := file.Close()
	if copyErr != nil {
		return 0, fmt.Errorf("write the LFS object: %w", copyErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("write the LFS object: %w", closeErr)
	}

	if written > options.MaxObjectBytes {
		return 0, fmt.Errorf("%w: more than the %d-byte cap", ErrLFSObjectTooLarge, options.MaxObjectBytes)
	}
	// The declared size is checked before the digest, because a size mismatch is
	// the cheaper and more likely mistake and the message is more useful.
	if put.Size >= 0 && written != put.Size {
		return 0, fmt.Errorf("%w: declared %d, received %d", ErrLFSSizeMismatch, put.Size, written)
	}
	actual := hex.EncodeToString(digest.Sum(nil))
	if actual != put.OID {
		return 0, fmt.Errorf("%w: declared %s, received %s", ErrLFSDigestMismatch, put.OID, actual)
	}

	// The object becomes reachable here, and only here.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fmt.Errorf("create the LFS object directory: %w", err)
	}
	if err := os.Rename(staging, path); err != nil {
		return 0, fmt.Errorf("install the LFS object: %w", err)
	}
	return written, nil
}

// LFSUsage returns the bytes a repository's LFS objects occupy.
//
// It walks the repository's object tree, so its cost grows with the number of
// objects. That is the price of having no counter to read: the store is the source
// of truth, and the per-repository counter the document specifies as the place a
// quota is checked "under a transaction lock" has no table in the data model. See
// the note in StoreLFSObject.
func (l *Layout) LFSUsage(storageID string) (int64, error) {
	canonical, err := canonicalStorageID(storageID)
	if err != nil {
		return 0, err
	}
	dir := filepath.Join(l.root, lfsDirectory, canonical)
	if err := checkContainedIn(l.root, dir); err != nil {
		return 0, err
	}

	var total int64
	walkErr := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		total += info.Size()
		return nil
	})
	if walkErr != nil {
		return 0, fmt.Errorf("measure the repository's LFS usage: %w", walkErr)
	}
	return total, nil
}

// newLFSStagingPath returns a fresh staging path inside the storage root.
func (l *Layout) newLFSStagingPath() (string, error) {
	nonce, err := uuid.NewV7()
	if err != nil {
		nonce = uuid.New()
	}
	dir := filepath.Join(l.root, lfsDirectory, lfsStagingDirectory)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create the LFS staging directory: %w", err)
	}
	derived := filepath.Join(dir, nonce.String())
	// Containment is checked against the storage root, because a nonce that
	// somehow contained a separator must not escape.
	if err := checkContainedIn(l.root, derived); err != nil {
		return "", err
	}
	return derived, nil
}

// RemoveLFSObject removes one stored object.
//
// The fan-out directories are then removed if they are empty, so deleting an
// object does not leave an empty tree behind for a reconcile to walk. A directory
// that still holds another object makes the removal fail, and that is not worth
// reporting: it is the expected case and holds nothing.
func (l *Layout) RemoveLFSObject(storageID, oid string) error {
	path, err := l.LFSPath(storageID, oid)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrLFSObjectNotFound
		}
		return fmt.Errorf("remove the LFS object: %w", err)
	}

	// Walk up to the repository's own directory, never past it.
	stop := filepath.Join(l.root, lfsDirectory, storageID)
	for dir := filepath.Dir(path); len(dir) > len(stop); dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			// Not empty, or already gone. Either way there is nothing further up
			// that can now be empty.
			break
		}
	}
	return nil
}

// RemoveLFSRepository removes every object stored for one repository.
//
// It is how a repository's objects are removed with it, rather than left behind
// as unreachable bytes that a later reconcile would report as an orphan.
func (l *Layout) RemoveLFSRepository(storageID string) error {
	canonical, err := canonicalStorageID(storageID)
	if err != nil {
		return err
	}
	dir := filepath.Join(l.root, lfsDirectory, canonical)
	if err := checkContainedIn(l.root, dir); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove the repository's LFS objects: %w", err)
	}
	return nil
}

// ListLFSObjects returns the objects stored for one repository, sorted.
//
// It exists for reconciliation and for an operator asking what a repository is
// using, so it reports what is on disk rather than what a table claims.
func (l *Layout) ListLFSObjects(storageID string) ([]string, error) {
	canonical, err := canonicalStorageID(storageID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(l.root, lfsDirectory, canonical)
	if err := checkContainedIn(l.root, dir); err != nil {
		return nil, err
	}

	found := []string{}
	walkErr := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		found = append(found, entry.Name())
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("list the LFS objects: %w", walkErr)
	}
	sort.Strings(found)
	return found, nil
}

// CleanupLFSStaging removes staging files left by an interrupted upload.
//
// docs/security/threat-model.md T-C5 requires that "every temporary path is
// created under a per-operation root that is removed on success, failure,
// cancellation, and process restart; a startup sweep removes stale roots". The
// per-operation removal is the deferred cleanup in StoreLFSObject; this is the
// startup half, for a process that was killed mid-upload. It returns the number of
// files removed so the caller can report it.
func (l *Layout) CleanupLFSStaging() (int, error) {
	dir := filepath.Join(l.root, lfsDirectory, lfsStagingDirectory)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the LFS staging directory: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if err := checkContainedIn(l.root, path); err != nil {
			return removed, err
		}
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("remove the LFS staging path: %w", err)
		}
		removed++
	}
	return removed, nil
}
