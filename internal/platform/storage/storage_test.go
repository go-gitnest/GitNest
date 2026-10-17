package storage

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// newLayout returns a layout rooted in a temporary directory the test owns.
func newLayout(t *testing.T) *Layout {
	t.Helper()
	base := t.TempDir()
	layout, err := NewLayout(filepath.Join(base, "repositories"), filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	return layout
}

func TestNewLayoutRequiresRoots(t *testing.T) {
	for _, test := range []struct {
		name    string
		root    string
		tmpRoot string
	}{
		{name: "empty root", root: "", tmpRoot: "tmp"},
		{name: "whitespace root", root: "   ", tmpRoot: "tmp"},
		{name: "empty tmp", root: "repos", tmpRoot: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewLayout(test.root, test.tmpRoot); err == nil {
				t.Fatal("an incomplete layout was accepted")
			}
		})
	}
}

func TestNewLayoutMakesRootsAbsolute(t *testing.T) {
	layout, err := NewLayout(filepath.Join("relative", "repos"), filepath.Join("relative", "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if !filepath.IsAbs(layout.Root()) {
		t.Errorf("Root() = %q, want an absolute path", layout.Root())
	}
	if !filepath.IsAbs(layout.TmpRoot()) {
		t.Errorf("TmpRoot() = %q, want an absolute path", layout.TmpRoot())
	}
}

// TestRepositoryPathIsDerivedFromTheIdentifier pins the layout: a two-level
// fan-out over the identifier's hex digits, ending in `.git`.
func TestRepositoryPathIsDerivedFromTheIdentifier(t *testing.T) {
	layout := newLayout(t)
	const storageID = "01a12afe-c7e2-7832-8a8b-1c0203d5d9e1"

	path, err := layout.RepositoryPath(storageID)
	if err != nil {
		t.Fatalf("RepositoryPath: %v", err)
	}
	want := filepath.Join(layout.Root(), "01", "a1", storageID+".git")
	if path != want {
		t.Errorf("RepositoryPath = %q, want %q", path, want)
	}
}

func TestRepositoryPathIsStableAcrossSpellings(t *testing.T) {
	layout := newLayout(t)
	const hyphenated = "01a12afe-c7e2-7832-8a8b-1c0203d5d9e1"
	const hyphenless = "01a12afec7e278328a8b1c0203d5d9e1"

	fromHyphenated, err := layout.RepositoryPath(hyphenated)
	if err != nil {
		t.Fatalf("RepositoryPath(hyphenated): %v", err)
	}
	fromHyphenless, err := layout.RepositoryPath(hyphenless)
	if err != nil {
		t.Fatalf("RepositoryPath(hyphenless): %v", err)
	}
	if fromHyphenated != fromHyphenless {
		t.Errorf("the two spellings produced %q and %q, want one path",
			fromHyphenated, fromHyphenless)
	}
	// Trimmed and upper-case input resolves to the canonical form too.
	fromUntidy, err := layout.RepositoryPath("  01A12AFE-C7E2-7832-8A8B-1C0203D5D9E1  ")
	if err != nil {
		t.Fatalf("RepositoryPath(untidy): %v", err)
	}
	if fromUntidy != fromHyphenated {
		t.Errorf("an untidy casing produced %q, want %q", fromUntidy, fromHyphenated)
	}
}

// TestRepositoryPathRefusesAnythingThatIsNotAUUID is the traversal control: a
// path escape cannot even be expressed, because the input must parse as a UUID.
func TestRepositoryPathRefusesAnythingThatIsNotAUUID(t *testing.T) {
	layout := newLayout(t)

	for _, test := range []struct {
		name      string
		storageID string
	}{
		{name: "empty", storageID: ""},
		{name: "whitespace", storageID: "   "},
		{name: "traversal", storageID: "../.."},
		{name: "traversal with separators", storageID: "../../etc/passwd"},
		{name: "absolute path", storageID: "/etc/passwd"},
		{name: "windows absolute path", storageID: `C:\Windows\System32`},
		{name: "backslash traversal", storageID: `..\..\windows`},
		{name: "null byte", storageID: "01a12afe-c7e2-7832-8a8b-1c0203d5d9e1\x00"},
		{name: "suffix injection", storageID: "01a12afe-c7e2-7832-8a8b-1c0203d5d9e1/../.."},
		{name: "too short", storageID: "01a1"},
		{name: "not hex", storageID: "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"},
		{name: "url", storageID: "file:///etc/passwd"},
		{name: "shell metacharacters", storageID: "; rm -rf /"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := layout.RepositoryPath(test.storageID)
			if !errors.Is(err, ErrInvalidStorageID) {
				t.Fatalf("RepositoryPath(%q) = %q, %v; want ErrInvalidStorageID",
					test.storageID, path, err)
			}
			if path != "" {
				t.Errorf("a path was returned alongside the error: %q", path)
			}
		})
	}
}

func TestEnsureRepositoryCreatesTheDirectoryAndIsIdempotent(t *testing.T) {
	layout := newLayout(t)
	storageID := uuid.NewString()

	first, err := layout.EnsureRepository(storageID)
	if err != nil {
		t.Fatalf("first EnsureRepository: %v", err)
	}
	second, err := layout.EnsureRepository(storageID)
	if err != nil {
		t.Fatalf("second EnsureRepository: %v", err)
	}
	if first != second {
		t.Errorf("the second call returned %q, want the same path %q", second, first)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatalf("stat the created directory: %v", err)
	}
	if !info.IsDir() {
		t.Error("the created path is not a directory")
	}

	// Marking the directory makes the idempotence visible: a second call that
	// recreated the directory would lose the marker.
	marker := filepath.Join(first, "marker")
	if err := os.WriteFile(marker, []byte("kept"), 0o600); err != nil {
		t.Fatalf("write the marker: %v", err)
	}
	if _, err := layout.EnsureRepository(storageID); err != nil {
		t.Fatalf("third EnsureRepository: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the directory contents were discarded: %v", err)
	}
}

func TestRepositoryExists(t *testing.T) {
	layout := newLayout(t)
	storageID := uuid.NewString()

	exists, err := layout.RepositoryExists(storageID)
	if err != nil {
		t.Fatalf("RepositoryExists before creation: %v", err)
	}
	if exists {
		t.Error("a repository exists before it was created")
	}

	if _, err := layout.EnsureRepository(storageID); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	exists, err = layout.RepositoryExists(storageID)
	if err != nil {
		t.Fatalf("RepositoryExists after creation: %v", err)
	}
	if !exists {
		t.Error("the created repository was not reported as existing")
	}

	// An invalid identifier is an error, not a false.
	if _, err := layout.RepositoryExists("../etc"); !errors.Is(err, ErrInvalidStorageID) {
		t.Errorf("RepositoryExists(traversal) = %v, want ErrInvalidStorageID", err)
	}
}

func TestRemoveRepositoryIsIdempotent(t *testing.T) {
	layout := newLayout(t)
	storageID := uuid.NewString()
	path, err := layout.EnsureRepository(storageID)
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("write a file: %v", err)
	}

	if err := layout.RemoveRepository(storageID); err != nil {
		t.Fatalf("first RemoveRepository: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory still exists after removal: %v", err)
	}
	// Removing it again converges rather than failing, so a repair pass that
	// runs twice is safe.
	if err := layout.RemoveRepository(storageID); err != nil {
		t.Errorf("second RemoveRepository: %v", err)
	}
}

func TestTempPathIsUniqueBoundedAndContained(t *testing.T) {
	layout := newLayout(t)

	first, err := layout.TempPath("upload")
	if err != nil {
		t.Fatalf("TempPath: %v", err)
	}
	second, err := layout.TempPath("upload")
	if err != nil {
		t.Fatalf("second TempPath: %v", err)
	}
	if first == second {
		t.Error("two temporary paths are identical, so two operations would collide")
	}
	for _, path := range []string{first, second} {
		if !strings.HasPrefix(path, layout.TmpRoot()) {
			t.Errorf("%q is not under the temporary root %q", path, layout.TmpRoot())
		}
	}

	for _, purpose := range []string{"", "../escape", "has space", "UPPER", "semi;colon", strings.Repeat("a", 40)} {
		t.Run("reject "+purpose, func(t *testing.T) {
			if _, err := layout.TempPath(purpose); err == nil {
				t.Errorf("TempPath(%q) accepted an invalid purpose", purpose)
			}
		})
	}
}

func TestCleanupTempRemovesEverything(t *testing.T) {
	layout := newLayout(t)
	if err := layout.EnsureTmpRoot(); err != nil {
		t.Fatalf("EnsureTmpRoot: %v", err)
	}
	nested := filepath.Join(layout.TmpRoot(), "leftover", "deep")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("create nested scratch data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "pack"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("write scratch data: %v", err)
	}

	if err := layout.CleanupTemp(); err != nil {
		t.Fatalf("CleanupTemp: %v", err)
	}
	entries, err := os.ReadDir(layout.TmpRoot())
	if err != nil {
		t.Fatalf("read the temporary root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the temporary root still holds %d entries", len(entries))
	}
	// The root itself survives, so the next operation does not have to create it.
	if info, err := os.Stat(layout.TmpRoot()); err != nil || !info.IsDir() {
		t.Errorf("the temporary root was removed rather than cleared: %v", err)
	}
}

func TestListRepositoriesFindsOnlyItsOwnDirectories(t *testing.T) {
	layout := newLayout(t)

	created := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for _, storageID := range created {
		if _, err := layout.EnsureRepository(storageID); err != nil {
			t.Fatalf("EnsureRepository: %v", err)
		}
	}

	// Plant things that must not be reported as repositories.
	if err := os.MkdirAll(filepath.Join(layout.Root(), "not-hex", "zz", "stray.git"), 0o750); err != nil {
		t.Fatalf("create a stray directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(layout.Root(), "01", "a1", "not-a-uuid.git"), 0o750); err != nil {
		t.Fatalf("create a misnamed directory: %v", err)
	}
	// An identifier at the wrong fan-out prefix is not where this layout would
	// put it, so it is not claimed.
	if err := os.MkdirAll(filepath.Join(layout.Root(), "ff", "ff", created[0]+".git"), 0o750); err != nil {
		t.Fatalf("create a misplaced directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layout.Root(), "README"), []byte("notes"), 0o600); err != nil {
		t.Fatalf("write a stray file: %v", err)
	}

	found, err := layout.ListRepositories()
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(found) != len(created) {
		t.Fatalf("found %d repositories (%v), want %d", len(found), found, len(created))
	}
	// The listing is sorted by identifier, not by creation order, so it is
	// compared as a set.
	want := make(map[string]struct{}, len(created))
	for _, storageID := range created {
		want[storageID] = struct{}{}
	}
	for _, storageID := range found {
		if _, ok := want[storageID]; !ok {
			t.Errorf("the listing reported %q, which was never created", storageID)
		}
	}
	if !sort.StringsAreSorted(found) {
		t.Errorf("the listing is not sorted: %v", found)
	}
}

func TestListRepositoriesOnAMissingRoot(t *testing.T) {
	layout := newLayout(t)
	found, err := layout.ListRepositories()
	if err != nil {
		t.Fatalf("ListRepositories on a missing root: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("found %v, want nothing", found)
	}
}

// TestReconcileReportsBothDirections is the acceptance criterion about a row
// without a directory and the reverse.
func TestReconcileReportsBothDirections(t *testing.T) {
	layout := newLayout(t)

	// One repository on both sides, one only in the database, one only on disk.
	shared := uuid.NewString()
	onlyInDatabase := uuid.NewString()
	onlyOnDisk := uuid.NewString()
	if _, err := layout.EnsureRepository(shared); err != nil {
		t.Fatalf("EnsureRepository(shared): %v", err)
	}
	if _, err := layout.EnsureRepository(onlyOnDisk); err != nil {
		t.Fatalf("EnsureRepository(onlyOnDisk): %v", err)
	}

	report, err := layout.Reconcile([]string{shared, onlyInDatabase})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.IsConsistent() {
		t.Fatal("the report claims consistency while two divergences exist")
	}
	counts := report.Counts()
	if counts[RowWithoutDirectory] != 1 {
		t.Errorf("row_without_directory = %d, want 1", counts[RowWithoutDirectory])
	}
	if counts[DirectoryWithoutRow] != 1 {
		t.Errorf("directory_without_row = %d, want 1", counts[DirectoryWithoutRow])
	}
	for _, divergence := range report.Divergences {
		switch divergence.Kind {
		case RowWithoutDirectory:
			if divergence.StorageID != onlyInDatabase {
				t.Errorf("row_without_directory named %q, want %q", divergence.StorageID, onlyInDatabase)
			}
		case DirectoryWithoutRow:
			if divergence.StorageID != onlyOnDisk {
				t.Errorf("directory_without_row named %q, want %q", divergence.StorageID, onlyOnDisk)
			}
		}
	}
}

// TestRepairCreatesMissingDirectoriesAndIsIdempotent covers the forward repair.
func TestRepairCreatesMissingDirectoriesAndIsIdempotent(t *testing.T) {
	layout := newLayout(t)
	missing := uuid.NewString()
	shared := uuid.NewString()
	if _, err := layout.EnsureRepository(shared); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}

	report, err := layout.Repair([]string{missing, shared}, false)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !report.IsConsistent() {
		t.Errorf("after repair the report still shows %v", report.Divergences)
	}
	exists, err := layout.RepositoryExists(missing)
	if err != nil {
		t.Fatalf("RepositoryExists: %v", err)
	}
	if !exists {
		t.Error("the missing directory was not created")
	}

	// A second pass changes nothing and reports nothing.
	second, err := layout.Repair([]string{missing, shared}, false)
	if err != nil {
		t.Fatalf("second Repair: %v", err)
	}
	if !second.IsConsistent() {
		t.Errorf("the second pass reported %v, want consistency", second.Divergences)
	}
}

// TestRepairLeavesOrphansUnlessAsked is the safety property: bytes are never
// deleted as a side effect of a consistency check.
func TestRepairLeavesOrphansUnlessAsked(t *testing.T) {
	layout := newLayout(t)
	orphan := uuid.NewString()
	path, err := layout.EnsureRepository(orphan)
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}

	// Without the flag the orphan survives, and the report still names it.
	report, err := layout.Repair(nil, false)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if report.IsConsistent() {
		t.Error("an orphan directory was reported as consistent")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the orphan was removed without being asked for: %v", err)
	}

	// With the flag it is removed, and the next pass is clean.
	if _, err := layout.Repair(nil, true); err != nil {
		t.Fatalf("Repair(removeOrphans): %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the orphan still exists: %v", err)
	}

	final, err := layout.Reconcile(nil)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !final.IsConsistent() {
		t.Errorf("after removing the orphan the report shows %v", final.Divergences)
	}
}

func TestReconcileRejectsANonUUIDFromTheDatabase(t *testing.T) {
	layout := newLayout(t)
	if _, err := layout.Reconcile([]string{"not-a-uuid"}); err == nil {
		t.Fatal("a non-UUID identifier from the database was accepted")
	}
}
