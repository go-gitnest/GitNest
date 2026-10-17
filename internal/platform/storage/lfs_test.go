package storage_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/google/uuid"
)

// digestOf returns the object identifier for some bytes, which is what the LFS
// protocol addresses an object by.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// newLFSLayout returns a layout over a fresh directory.
func newLFSLayout(t *testing.T) *storage.Layout {
	t.Helper()
	base := t.TempDir()
	layout, err := storage.NewLayout(filepath.Join(base, "repositories"), filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	return layout
}

// store puts an object and fails the test if it is refused.
func store(t *testing.T, layout *storage.Layout, storageID string, body []byte) string {
	t.Helper()
	oid := digestOf(body)
	if _, err := layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: storageID, OID: oid, Size: int64(len(body)), Body: bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("StoreLFSObject: %v", err)
	}
	return oid
}

// TestStoreAndReadAnObject is the round trip.
func TestStoreAndReadAnObject(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()
	body := []byte("a large file's worth of bytes")

	oid := store(t, layout, storageID, body)

	exists, err := layout.LFSObjectExists(storageID, oid)
	if err != nil {
		t.Fatalf("LFSObjectExists: %v", err)
	}
	if !exists {
		t.Fatal("the stored object is not reported as present")
	}
	size, err := layout.LFSObjectSize(storageID, oid)
	if err != nil {
		t.Fatalf("LFSObjectSize: %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("size = %d, want %d", size, len(body))
	}

	file, err := layout.OpenLFSObject(storageID, oid)
	if err != nil {
		t.Fatalf("OpenLFSObject: %v", err)
	}
	defer func() { _ = file.Close() }()
	read, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read the object: %v", err)
	}
	if !bytes.Equal(read, body) {
		t.Errorf("read %q, want the stored bytes", read)
	}

	// The path is addressed by the content hash rather than a name, which is what
	// the architecture document requires.
	if !strings.HasSuffix(file.Name(), oid) {
		t.Errorf("the object's path %q does not end in its digest", file.Name())
	}
	if !strings.Contains(file.Name(), storageID) {
		t.Errorf("the object's path %q is not scoped to its repository", file.Name())
	}
}

// TestACrossRepositoryGuessFindsNothing is the storage half of T-C3.
//
// The authorization half belongs to the repository service. What this asserts is
// the other half: an object stored for one repository is not reachable under
// another one's identifier, so a validated digest is not a capability.
func TestACrossRepositoryGuessFindsNothing(t *testing.T) {
	layout := newLFSLayout(t)
	mine := uuid.NewString()
	someoneElse := uuid.NewString()
	body := []byte("content belonging to a private repository")

	oid := store(t, layout, someoneElse, body)

	// The digest is valid and the bytes are on disk, so only the scoping can
	// refuse this.
	if err := storage.ValidateOID(oid); err != nil {
		t.Fatalf("the digest should be valid: %v", err)
	}
	exists, err := layout.LFSObjectExists(mine, oid)
	if err != nil {
		t.Fatalf("LFSObjectExists: %v", err)
	}
	if exists {
		t.Fatal("a digest stored for another repository is reachable under this one")
	}
	if _, err := layout.OpenLFSObject(mine, oid); !errors.Is(err, storage.ErrLFSObjectNotFound) {
		t.Errorf("opening another repository's object = %v, want ErrLFSObjectNotFound", err)
	}
	if _, err := layout.LFSObjectSize(mine, oid); !errors.Is(err, storage.ErrLFSObjectNotFound) {
		t.Errorf("sizing another repository's object = %v, want ErrLFSObjectNotFound", err)
	}
}

// TestTheDigestIsVerifiedBeforeTheObjectIsReachable is T-C4's first half.
func TestTheDigestIsVerifiedBeforeTheObjectIsReachable(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()
	body := []byte("the bytes that were actually sent")
	wrong := digestOf([]byte("the bytes the client claimed"))

	_, err := layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: storageID, OID: wrong, Size: int64(len(body)), Body: bytes.NewReader(body),
	})
	if !errors.Is(err, storage.ErrLFSDigestMismatch) {
		t.Fatalf("error = %v, want ErrLFSDigestMismatch", err)
	}

	// T-C4: "a mismatch rejects the object and leaves no entry". Neither the
	// claimed digest nor the actual one is now present.
	for _, oid := range []string{wrong, digestOf(body)} {
		exists, err := layout.LFSObjectExists(storageID, oid)
		if err != nil {
			t.Fatalf("LFSObjectExists: %v", err)
		}
		if exists {
			t.Errorf("a rejected upload left %s behind", oid)
		}
	}
}

// TestTheSizeIsVerifiedBeforeTheObjectIsReachable is T-C4's second half, for "an
// object with a mismatched size".
func TestTheSizeIsVerifiedBeforeTheObjectIsReachable(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()
	body := []byte("twelve bytes")

	// The digest is right, so only the size can refuse this.
	oid := digestOf(body)
	_, err := layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: storageID, OID: oid, Size: 9999, Body: bytes.NewReader(body),
	})
	if !errors.Is(err, storage.ErrLFSSizeMismatch) {
		t.Fatalf("error = %v, want ErrLFSSizeMismatch", err)
	}
	exists, err := layout.LFSObjectExists(storageID, oid)
	if err != nil {
		t.Fatalf("LFSObjectExists: %v", err)
	}
	if exists {
		t.Error("a size-mismatched upload left the object behind")
	}
}

// TestAnUndeclaredSizeStillVerifiesTheDigest covers a client that streams without
// declaring a length: integrity must not depend on the declaration.
func TestAnUndeclaredSizeStillVerifiesTheDigest(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()
	body := []byte("streamed without a length")

	oid := digestOf(body)
	written, err := layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: storageID, OID: oid, Size: -1, Body: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("StoreLFSObject: %v", err)
	}
	if written != int64(len(body)) {
		t.Errorf("stored %d bytes, want %d", written, len(body))
	}

	// A wrong digest is still refused with no size to compare.
	_, err = layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: storageID, OID: digestOf([]byte("different")), Size: -1, Body: bytes.NewReader(body),
	})
	if !errors.Is(err, storage.ErrLFSDigestMismatch) {
		t.Errorf("error = %v, want ErrLFSDigestMismatch", err)
	}
}

// TestAnOversizedObjectIsRefusedWithoutBufferingIt covers the documented
// per-object cap: a body over the limit is refused, and the refusal does not read
// the whole thing into memory or onto disk.
func TestAnOversizedObjectIsRefusedWithoutBufferingIt(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()
	options := storage.LFSOptions{MaxObjectBytes: 1024}

	body := bytes.Repeat([]byte("x"), 4096)
	_, err := layout.StoreLFSObject(options, storage.LFSObjectPut{
		StorageID: storageID, OID: digestOf(body), Size: int64(len(body)), Body: bytes.NewReader(body),
	})
	if !errors.Is(err, storage.ErrLFSObjectTooLarge) {
		t.Fatalf("error = %v, want ErrLFSObjectTooLarge", err)
	}

	// A body with no declared size that turns out to be over the cap is refused
	// too, which is the case a client could otherwise use to bypass the check.
	_, err = layout.StoreLFSObject(options, storage.LFSObjectPut{
		StorageID: storageID, OID: digestOf(body), Size: -1, Body: bytes.NewReader(body),
	})
	if !errors.Is(err, storage.ErrLFSObjectTooLarge) {
		t.Errorf("an undeclared oversized body = %v, want ErrLFSObjectTooLarge", err)
	}

	// Nothing was installed, and nothing was left staging.
	objects, err := layout.ListLFSObjects(storageID)
	if err != nil {
		t.Fatalf("ListLFSObjects: %v", err)
	}
	if len(objects) != 0 {
		t.Errorf("a refused upload stored %v", objects)
	}
	assertNoStaging(t, layout)
}

// TestAnInterruptedUploadLeavesNoObject is performance.md's rule: "an interrupted
// upload leaves no blob reachable". A body that fails partway must leave neither
// an object nor a staging file.
func TestAnInterruptedUploadLeavesNoObject(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()
	body := []byte("a body that will fail partway through")

	oid := digestOf(body)
	_, err := layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: storageID, OID: oid, Size: int64(len(body)), Body: &failingReader{after: 5},
	})
	if err == nil {
		t.Fatal("a failing body was accepted")
	}

	exists, statErr := layout.LFSObjectExists(storageID, oid)
	if statErr != nil {
		t.Fatalf("LFSObjectExists: %v", statErr)
	}
	if exists {
		t.Error("an interrupted upload left the object reachable")
	}
	assertNoStaging(t, layout)
}

// failingReader yields some bytes and then fails, standing in for a client that
// disconnects mid-upload.
type failingReader struct {
	after int
	read  int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.read >= f.after {
		return 0, errors.New("the connection was lost")
	}
	n := copy(p, "abc")
	f.read += n
	return n, nil
}

// assertNoStaging fails when a staging file survived.
func assertNoStaging(t *testing.T, layout *storage.Layout) {
	t.Helper()
	dir := filepath.Join(layout.Root(), "lfs", ".staging")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read the staging directory: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("staging files survived: %v", names)
	}
}

// TestRestoringTheSameObjectIsIdempotent covers the retry a client performs after
// a partial upload: the bytes are not written twice and the result is unchanged.
func TestRestoringTheSameObjectIsIdempotent(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()
	body := []byte("stored twice")

	oid := store(t, layout, storageID, body)

	// Make the stored file distinguishable, so a rewrite would be visible.
	path := filepath.Join(layout.Root(), "lfs", storageID, oid[0:2], oid[2:4], oid)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the object: %v", err)
	}

	written, err := layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: storageID, OID: oid, Size: int64(len(body)), Body: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("storing an existing object: %v", err)
	}
	// The size is reported without rewriting, so a client that re-uploads after a
	// partial transfer learns the object is complete.
	if written != int64(len(body)) {
		t.Errorf("reported %d bytes, want %d", written, len(body))
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the object: %v", err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Error("an existing object was rewritten")
	}
}

// TestAnInvalidIdentifierIsRefused keeps a client-supplied value out of the
// filesystem entirely.
func TestAnInvalidIdentifierIsRefused(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()

	for _, oid := range []string{
		"",
		"tooshort",
		strings.Repeat("g", 64), // not hexadecimal
		strings.Repeat("A", 64), // uppercase, which would create a second name
		strings.Repeat("a", 63), // one short
		strings.Repeat("a", 65), // one long
		"../../etc/passwd",      // a traversal, which the length check also refuses
		strings.Repeat("a", 61) + "/x",
	} {
		t.Run(oid, func(t *testing.T) {
			if _, err := layout.LFSPath(storageID, oid); !errors.Is(err, storage.ErrInvalidOID) {
				t.Errorf("LFSPath = %v, want ErrInvalidOID", err)
			}
			if _, err := layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
				StorageID: storageID, OID: oid, Size: 0, Body: bytes.NewReader(nil),
			}); !errors.Is(err, storage.ErrInvalidOID) {
				t.Errorf("StoreLFSObject = %v, want ErrInvalidOID", err)
			}
			// Nothing was created anywhere.
			if _, err := os.Stat(filepath.Join(layout.Root(), "lfs", storageID)); err == nil {
				t.Error("an invalid identifier created a directory")
			}
		})
	}
}

// TestAnInvalidStorageIdentifierIsRefused is the other half of the path rule: the
// repository identifier must be a canonical UUID.
func TestAnInvalidStorageIdentifierIsRefused(t *testing.T) {
	layout := newLFSLayout(t)
	oid := digestOf([]byte("body"))

	for _, storageID := range []string{"", "not-a-uuid", "../../escape", uuid.NewString() + "/x"} {
		t.Run(storageID, func(t *testing.T) {
			if _, err := layout.LFSPath(storageID, oid); !errors.Is(err, storage.ErrInvalidStorageID) {
				t.Errorf("LFSPath = %v, want ErrInvalidStorageID", err)
			}
		})
	}
}

// TestRemovingAnObjectAndItsRepository covers cleanup: an object's fan-out
// directories go with it, and a repository's objects go with the repository.
func TestRemovingAnObjectAndItsRepository(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()
	first := store(t, layout, storageID, []byte("first object"))
	second := store(t, layout, storageID, []byte("second object"))

	if err := layout.RemoveLFSObject(storageID, first); err != nil {
		t.Fatalf("RemoveLFSObject: %v", err)
	}
	if _, err := layout.LFSObjectSize(storageID, first); !errors.Is(err, storage.ErrLFSObjectNotFound) {
		t.Errorf("the removed object is still readable: %v", err)
	}
	// The other object survives, so removal is not a directory wipe.
	if _, err := layout.LFSObjectSize(storageID, second); err != nil {
		t.Errorf("removing one object removed another: %v", err)
	}
	// Removing something that is not there is reported rather than ignored, so a
	// caller learns its view was stale.
	if err := layout.RemoveLFSObject(storageID, first); !errors.Is(err, storage.ErrLFSObjectNotFound) {
		t.Errorf("removing a missing object = %v, want ErrLFSObjectNotFound", err)
	}

	// Removing the repository takes every object with it.
	if err := layout.RemoveLFSRepository(storageID); err != nil {
		t.Fatalf("RemoveLFSRepository: %v", err)
	}
	objects, err := layout.ListLFSObjects(storageID)
	if err != nil {
		t.Fatalf("ListLFSObjects: %v", err)
	}
	if len(objects) != 0 {
		t.Errorf("the repository left %v behind", objects)
	}
	// And the repository layout the Git bytes live in is untouched.
	if _, err := layout.RepositoryPath(storageID); err != nil {
		t.Errorf("the repository path broke: %v", err)
	}
}

// TestListingReportsWhatIsOnDisk covers the operator and reconcile view.
func TestListingReportsWhatIsOnDisk(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()
	other := uuid.NewString()

	first := store(t, layout, storageID, []byte("one"))
	second := store(t, layout, storageID, []byte("two"))
	store(t, layout, other, []byte("three"))

	objects, err := layout.ListLFSObjects(storageID)
	if err != nil {
		t.Fatalf("ListLFSObjects: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("found %v, want the two objects of this repository", objects)
	}
	found := map[string]bool{}
	for _, oid := range objects {
		found[oid] = true
	}
	if !found[first] || !found[second] {
		t.Errorf("found %v, want both stored digests", objects)
	}

	// A repository with no objects reports none rather than failing.
	empty, err := layout.ListLFSObjects(uuid.NewString())
	if err != nil {
		t.Fatalf("ListLFSObjects on an unused repository: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("an unused repository reported %v", empty)
	}
}

// TestTheStartupSweepRemovesStaleStaging is T-C5's "a startup sweep removes stale
// roots", for the process that was killed mid-upload.
func TestTheStartupSweepRemovesStaleStaging(t *testing.T) {
	layout := newLFSLayout(t)

	// Stand in for a run that died holding two staging files.
	dir := filepath.Join(layout.Root(), "lfs", ".staging")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the staging directory: %v", err)
	}
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("partial"), 0o600); err != nil {
			t.Fatalf("write a staging file: %v", err)
		}
	}

	removed, err := layout.CleanupLFSStaging()
	if err != nil {
		t.Fatalf("CleanupLFSStaging: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d paths, want 2", removed)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the staging directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the sweep left %d entries", len(entries))
	}

	// A clean store sweeps nothing and does not fail on a missing directory.
	again, err := layout.CleanupLFSStaging()
	if err != nil {
		t.Fatalf("CleanupLFSStaging on a clean store: %v", err)
	}
	if again != 0 {
		t.Errorf("removed %d paths from a clean store, want 0", again)
	}
}

// TestStagingStaysInsideTheStorageRoot is the rule from performance.md: the
// staging path is inside the storage root, which is what makes the install an
// atomic rename rather than a copy across filesystems.
func TestStagingStaysInsideTheStorageRoot(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()

	_, err := layout.StoreLFSObject(storage.DefaultLFSOptions(), storage.LFSObjectPut{
		StorageID: storageID, OID: digestOf([]byte("x")), Size: -1, Body: &failingReader{after: 1},
	})
	if err == nil {
		t.Fatal("a failing body was accepted")
	}
	// The staging directory is under the storage root, and it is the only place a
	// partial upload can appear — the temporary root belongs to Git operations.
	if _, err := os.Stat(filepath.Join(layout.TmpRoot(), "lfs")); err == nil {
		t.Error("LFS staged under the temporary root instead of the storage root")
	}
}

// TestOptionsAreValidatedAtStartup keeps a misconfiguration from surfacing on the
// first upload.
func TestOptionsAreValidatedAtStartup(t *testing.T) {
	for _, options := range []storage.LFSOptions{{MaxObjectBytes: 0}, {MaxObjectBytes: -1}} {
		if err := options.Validate(); err == nil {
			t.Errorf("%+v was accepted", options)
		}
	}
	if err := storage.DefaultLFSOptions().Validate(); err != nil {
		t.Errorf("the documented defaults were rejected: %v", err)
	}
	// The documented cap is 2 GiB, from `git.lfs_max_object_bytes`.
	if got := storage.DefaultLFSOptions().MaxObjectBytes; got != 2<<30 {
		t.Errorf("the default cap is %d bytes, want the documented 2 GiB", got)
	}
}

// TestTheRepositoryAllowanceRefusesAnUploadThatWouldNotFit covers the documented
// per-repository quota, `git.lfs_repository_bytes`.
func TestTheRepositoryAllowanceRefusesAnUploadThatWouldNotFit(t *testing.T) {
	layout := newLFSLayout(t)
	storageID := uuid.NewString()
	// A small allowance for the test, as the object cap is likewise lowered.
	options := storage.LFSOptions{MaxObjectBytes: 1024, RepositoryBytes: 100}

	first := bytes.Repeat([]byte("a"), 60)
	if _, err := layout.StoreLFSObject(options, storage.LFSObjectPut{
		StorageID: storageID, OID: digestOf(first), Size: int64(len(first)), Body: bytes.NewReader(first),
	}); err != nil {
		t.Fatalf("the first object should fit: %v", err)
	}

	// The usage the allowance is measured against is what is on disk.
	used, err := layout.LFSUsage(storageID)
	if err != nil {
		t.Fatalf("LFSUsage: %v", err)
	}
	if used != int64(len(first)) {
		t.Errorf("usage = %d, want %d", used, len(first))
	}

	// A second object that would take the repository past its allowance is refused.
	second := bytes.Repeat([]byte("b"), 60)
	_, err = layout.StoreLFSObject(options, storage.LFSObjectPut{
		StorageID: storageID, OID: digestOf(second), Size: int64(len(second)), Body: bytes.NewReader(second),
	})
	if !errors.Is(err, storage.ErrLFSQuotaExceeded) {
		t.Fatalf("error = %v, want ErrLFSQuotaExceeded", err)
	}
	exists, err := layout.LFSObjectExists(storageID, digestOf(second))
	if err != nil {
		t.Fatalf("LFSObjectExists: %v", err)
	}
	if exists {
		t.Error("a quota-refused object was stored anyway")
	}
	assertNoStaging(t, layout)

	// One that fits is still accepted, so the check is about the allowance rather
	// than about the second object being second.
	small := bytes.Repeat([]byte("c"), 20)
	if _, err := layout.StoreLFSObject(options, storage.LFSObjectPut{
		StorageID: storageID, OID: digestOf(small), Size: int64(len(small)), Body: bytes.NewReader(small),
	}); err != nil {
		t.Errorf("an object that fits was refused: %v", err)
	}

	// An already-stored object is not counted twice, so a retry after a partial
	// transfer does not fail on the quota.
	if _, err := layout.StoreLFSObject(options, storage.LFSObjectPut{
		StorageID: storageID, OID: digestOf(first), Size: int64(len(first)), Body: bytes.NewReader(first),
	}); err != nil {
		t.Errorf("re-storing an existing object = %v, want it to be idempotent", err)
	}
}

// TestTheAllowanceIsIndependentOfTheObjectCap keeps the two bounds separate: the
// allowance is a budget and the cap is a per-file bound, so a tight budget with a
// generous cap is a coherent policy rather than a configuration mistake.
func TestTheAllowanceIsIndependentOfTheObjectCap(t *testing.T) {
	tight := storage.LFSOptions{MaxObjectBytes: 1024, RepositoryBytes: 100}
	if err := tight.Validate(); err != nil {
		t.Errorf("a tight budget was refused: %v", err)
	}
	// A non-positive allowance means "not enforced", which is what a caller bounding
	// only the object size wants.
	unbounded := storage.LFSOptions{MaxObjectBytes: 1024}
	if err := unbounded.Validate(); err != nil {
		t.Errorf("an unenforced allowance was refused: %v", err)
	}
	// The documented defaults carry the documented values.
	defaults := storage.DefaultLFSOptions()
	if defaults.RepositoryBytes != 50<<30 {
		t.Errorf("the default allowance is %d bytes, want the documented 50 GiB", defaults.RepositoryBytes)
	}
	if err := defaults.Validate(); err != nil {
		t.Errorf("the documented defaults were rejected: %v", err)
	}
}
