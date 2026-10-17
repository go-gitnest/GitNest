package backup_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/backup"
	"github.com/go-gitnest/GitNest/internal/identity"
	platformgit "github.com/go-gitnest/GitNest/internal/platform/git"
	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/go-gitnest/GitNest/internal/repository"
	githttp "github.com/go-gitnest/GitNest/internal/transport/http/git"
	"github.com/google/uuid"
)

// fakeDatabase is the database half of a set, in memory.
//
// It exists because the round trip has to be testable without a database — the
// recovery set's logic is about files, manifests, and refusals, none of which need
// PostgreSQL — and because a restore into a clean instance is a different
// environment from a capture, which a fake represents exactly.
type fakeDatabase struct {
	// contents is what a dump captures and a restore installs.
	contents []byte
	// schemaVersion is what the manifest records.
	schemaVersion string
	// repositories are the rows a capture sees.
	repositories []backup.RepositoryRef
	// dumpErr and restoreErr force a failure of either half.
	dumpErr    error
	restoreErr error
	// restored records what a restore installed, so a test can assert the round trip.
	restored []byte
	// dumps counts the captures.
	dumps int
}

func (f *fakeDatabase) Dump(_ context.Context, w io.Writer) (backup.DatabaseInfo, error) {
	if f.dumpErr != nil {
		return backup.DatabaseInfo{}, f.dumpErr
	}
	f.dumps++
	if _, err := w.Write(f.contents); err != nil {
		return backup.DatabaseInfo{}, err
	}
	return backup.DatabaseInfo{
		SchemaVersion: f.schemaVersion,
		Repositories:  f.repositories,
	}, nil
}

func (f *fakeDatabase) Restore(_ context.Context, r io.Reader) error {
	if f.restoreErr != nil {
		return f.restoreErr
	}
	contents, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.restored = contents
	return nil
}

// world is an instance with a repository root holding one repository and one LFS
// object, which is what a capture has to take.
type world struct {
	t         *testing.T
	base      string
	repoRoot  string
	tmpRoot   string
	target    string
	database  *fakeDatabase
	storageID string
	repoName  string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	base := t.TempDir()
	w := &world{
		t:         t,
		base:      base,
		repoRoot:  filepath.Join(base, "repositories"),
		tmpRoot:   filepath.Join(base, "tmp"),
		target:    filepath.Join(base, "backup"),
		storageID: uuid.NewString(),
		repoName:  "hosted",
	}
	if err := os.MkdirAll(w.tmpRoot, 0o755); err != nil {
		t.Fatalf("create the temporary root: %v", err)
	}
	w.database = &fakeDatabase{
		contents:      []byte("a database dump\n"),
		schemaVersion: "0005_repository_archive_and_audit.up.sql",
	}
	w.seed()
	return w
}

// seed writes a repository's bytes and an LFS object, so a capture has real content.
func (w *world) seed() {
	w.t.Helper()

	// The repository directory, laid out the way the storage package does:
	// <root>/<aa>/<bb>/<storage_id>.git
	if err := os.MkdirAll(w.repositoryPath(), 0o755); err != nil {
		w.t.Fatalf("create the repository directory: %v", err)
	}
	for name, body := range map[string]string{
		filepath.Join("HEAD"):                      "ref: refs/heads/main\n",
		filepath.Join("objects", "pack", "p.pack"): "packed objects",
		filepath.Join("refs", "heads", "main"):     "0123456789abcdef\n",
	} {
		if err := w.writeFile(filepath.Join(w.repositoryPath(), name), body); err != nil {
			w.t.Fatalf("write %s: %v", name, err)
		}
	}

	// An LFS object, which lives under the same root and so must be captured by the
	// same copy.
	oid := strings.Repeat("ab", 32)
	lfsPath := filepath.Join(w.repoRoot, "lfs", w.storageID, oid[0:2], oid[2:4], oid)
	if err := w.writeFile(lfsPath, "a large file's bytes"); err != nil {
		w.t.Fatalf("write the LFS object: %v", err)
	}

	w.database.repositories = []backup.RepositoryRef{
		{ID: uuid.NewString(), StorageID: w.storageID, Name: w.repoName},
	}
}

// repositoryPath is where the repository's bytes live.
func (w *world) repositoryPath() string {
	flat := strings.ReplaceAll(w.storageID, "-", "")
	return filepath.Join(w.repoRoot, flat[0:2], flat[2:4], w.storageID+".git")
}

// lfsPath is where the LFS object lives.
func (w *world) lfsPath() string {
	oid := strings.Repeat("ab", 32)
	return filepath.Join(w.repoRoot, "lfs", w.storageID, oid[0:2], oid[2:4], oid)
}

func (w *world) writeFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o600)
}

// create captures a set and fails the test if it is refused.
func (w *world) create() backup.Manifest {
	w.t.Helper()
	manifest, err := backup.Create(context.Background(), backup.Options{
		RepoRoot: w.repoRoot, TmpRoot: w.tmpRoot, Target: w.target,
		BuildVersion: "test", Database: w.database,
		Now: func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		w.t.Fatalf("Create: %v", err)
	}
	return manifest
}

// TestACaptureRecordsWhatItTook covers the manifest's contents: it is the evidence
// that makes a set verifiable rather than merely present.
func TestACaptureRecordsWhatItTook(t *testing.T) {
	w := newWorld(t)
	manifest := w.create()

	if manifest.Format != backup.FormatVersion {
		t.Errorf("format = %d, want %d", manifest.Format, backup.FormatVersion)
	}
	if manifest.BuildVersion != "test" {
		t.Errorf("build version = %q, want the one supplied", manifest.BuildVersion)
	}
	if manifest.SchemaVersion != "0005_repository_archive_and_audit.up.sql" {
		t.Errorf("schema version = %q, want what the database reported", manifest.SchemaVersion)
	}
	if manifest.CreatedAt.IsZero() {
		t.Error("the manifest records no capture time")
	}
	// The dump's size and digest are recorded, so a later verification can prove the
	// file is the one that was written.
	if manifest.Database.Bytes != int64(len("a database dump\n")) {
		t.Errorf("dump bytes = %d, want %d", manifest.Database.Bytes, len("a database dump\n"))
	}
	if len(manifest.Database.SHA256) != 64 {
		t.Errorf("the dump digest is %q, want a sha256", manifest.Database.SHA256)
	}

	// Every repository the database named is recorded with what the copy holds.
	if len(manifest.Repositories) != 1 {
		t.Fatalf("the manifest names %d repositories, want 1", len(manifest.Repositories))
	}
	entry := manifest.Repositories[0]
	if entry.StorageID != w.storageID {
		t.Errorf("storage id = %q, want %q", entry.StorageID, w.storageID)
	}
	if !entry.Exists {
		t.Error("the repository was recorded as absent though its directory exists")
	}
	if entry.Files != 3 {
		t.Errorf("files = %d, want the 3 the repository holds", entry.Files)
	}

	// The limits of the capture travel in the set itself.
	if len(manifest.Notes) == 0 {
		t.Error("the manifest records no limits, so an operator must read a document to learn them")
	}
	joined := strings.Join(manifest.Notes, " ")
	if !strings.Contains(joined, "point-in-time") {
		t.Errorf("the notes do not state the point-in-time limit: %v", manifest.Notes)
	}
	if !strings.Contains(joined, "secret key") {
		t.Errorf("the notes do not state that the key is separate: %v", manifest.Notes)
	}
}

// TestACaptureTakesTheRepositoryRootIncludingLFS covers the reason one copy is
// enough: the LFS objects live under the repository root, so the same copy captures
// them and there is no second path to forget.
func TestACaptureTakesTheRepositoryRootIncludingLFS(t *testing.T) {
	w := newWorld(t)
	w.create()

	// The repository's bytes.
	copied := filepath.Join(w.target, backup.RepositoriesName)
	flat := strings.ReplaceAll(w.storageID, "-", "")
	pack := filepath.Join(copied, flat[0:2], flat[2:4], w.storageID+".git", "objects", "pack", "p.pack")
	body, err := os.ReadFile(pack)
	if err != nil {
		t.Fatalf("the repository's objects are not in the set: %v", err)
	}
	if string(body) != "packed objects" {
		t.Errorf("pack content = %q", body)
	}

	// And the LFS object, which is the half a naive "dump the .git directories"
	// backup would lose.
	oid := strings.Repeat("ab", 32)
	lfs := filepath.Join(copied, "lfs", w.storageID, oid[0:2], oid[2:4], oid)
	if _, err := os.Stat(lfs); err != nil {
		t.Fatalf("the LFS object is not in the set: %v", err)
	}
}

// TestVerificationPassesOnACompleteSet is the property Create relies on when it
// reads its own output back.
func TestVerificationPassesOnACompleteSet(t *testing.T) {
	w := newWorld(t)
	w.create()

	problems, err := backup.Verify(w.target)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("a complete set reported %v", problems)
	}
}

// TestVerificationNamesEveryProblem covers the failures that matter, and that it
// reports all of them rather than the first: an operator recovering from a bad
// backup needs the whole picture in one pass.
func TestVerificationNamesEveryProblem(t *testing.T) {
	t.Run("a truncated repository copy", func(t *testing.T) {
		w := newWorld(t)
		w.create()

		copied := filepath.Join(w.target, backup.RepositoriesName)
		flat := strings.ReplaceAll(w.storageID, "-", "")
		pack := filepath.Join(copied, flat[0:2], flat[2:4], w.storageID+".git", "objects", "pack", "p.pack")
		makeWritable(t, pack)
		if err := os.WriteFile(pack, []byte("truncated"), 0o600); err != nil {
			t.Fatalf("truncate the pack: %v", err)
		}

		problems, err := backup.Verify(w.target)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if len(problems) == 0 {
			t.Fatal("a truncated copy passed verification")
		}
		if !strings.Contains(details(problems), "bytes") {
			t.Errorf("the problem does not name the size mismatch: %v", details(problems))
		}
	})

	t.Run("a missing repository directory", func(t *testing.T) {
		w := newWorld(t)
		w.create()

		copied := filepath.Join(w.target, backup.RepositoriesName)
		flat := strings.ReplaceAll(w.storageID, "-", "")
		directory := filepath.Join(copied, flat[0:2], flat[2:4], w.storageID+".git")
		if err := os.RemoveAll(directory); err != nil {
			t.Fatalf("remove the copy: %v", err)
		}

		problems, err := backup.Verify(w.target)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !strings.Contains(details(problems), "missing from the set") {
			t.Errorf("a missing repository was not named as such: %v", details(problems))
		}
	})

	t.Run("a tampered database dump", func(t *testing.T) {
		w := newWorld(t)
		w.create()

		dump := filepath.Join(w.target, backup.DatabaseName)
		// A same-length edit, which only the digest can catch. This is why the digest
		// is recorded rather than the size alone.
		makeWritable(t, dump)
		if err := os.WriteFile(dump, []byte("a database dumpX"), 0o600); err != nil {
			t.Fatalf("tamper with the dump: %v", err)
		}

		problems, err := backup.Verify(w.target)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !strings.Contains(details(problems), "digest") {
			t.Errorf("a tampered dump was not caught by its digest: %v", details(problems))
		}
	})

	t.Run("a missing database dump", func(t *testing.T) {
		w := newWorld(t)
		w.create()
		if err := os.Remove(filepath.Join(w.target, backup.DatabaseName)); err != nil {
			t.Fatalf("remove the dump: %v", err)
		}

		problems, err := backup.Verify(w.target)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !strings.Contains(details(problems), "database dump is missing") {
			t.Errorf("a missing dump was not named: %v", details(problems))
		}
	})

	t.Run("several problems at once", func(t *testing.T) {
		w := newWorld(t)
		w.create()
		if err := os.Remove(filepath.Join(w.target, backup.DatabaseName)); err != nil {
			t.Fatalf("remove the dump: %v", err)
		}
		if err := os.RemoveAll(filepath.Join(w.target, backup.RepositoriesName)); err != nil {
			t.Fatalf("remove the storage copy: %v", err)
		}

		problems, err := backup.Verify(w.target)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		// Both halves are reported, so one pass tells the operator everything.
		if len(problems) < 2 {
			t.Errorf("verification reported %d problems, want both halves: %v", len(problems), problems)
		}
	})
}

// TestADatabaseRowWithoutADirectoryIsRecordedAndReported covers the divergence a
// backup is exactly the right moment to learn about: the row exists and the bytes do
// not. It is recorded rather than omitted, so verification keeps saying so.
func TestADatabaseRowWithoutADirectoryIsRecordedAndReported(t *testing.T) {
	w := newWorld(t)
	// A second repository the database knows about and the disk does not.
	missing := uuid.NewString()
	w.database.repositories = append(w.database.repositories, backup.RepositoryRef{
		ID: uuid.NewString(), StorageID: missing, Name: "lost",
	})

	manifest := w.create()

	var entry *backup.Repository
	for i := range manifest.Repositories {
		if manifest.Repositories[i].StorageID == missing {
			entry = &manifest.Repositories[i]
		}
	}
	if entry == nil {
		t.Fatal("the divergent repository is absent from the manifest")
	}
	if entry.Exists {
		t.Error("a repository without a directory was recorded as present")
	}

	problems, err := backup.Verify(w.target)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	joined := details(problems)
	if !strings.Contains(joined, "lost") {
		t.Errorf("verification does not name the divergent repository: %v", joined)
	}
	if !strings.Contains(joined, "no directory") {
		t.Errorf("verification does not state what was wrong: %v", joined)
	}
	// The load-bearing distinction: a repository the instance had already lost does
	// not make the set unusable, because the set holds everything that existed.
	if inSet(problems) != 0 {
		t.Errorf("a finding about the instance was reported as a broken set: %v", joined)
	}
}

// TestASetRefusesToBeWrittenOverAnother is the target rule: a set is one capture,
// and a directory holding two intermixed captures is not a recovery point.
func TestASetRefusesToBeWrittenOverAnother(t *testing.T) {
	w := newWorld(t)
	w.create()

	_, err := backup.Create(context.Background(), backup.Options{
		RepoRoot: w.repoRoot, TmpRoot: w.tmpRoot, Target: w.target, Database: w.database,
	})
	if !errors.Is(err, backup.ErrTargetExists) {
		t.Fatalf("a second capture into the same target = %v, want ErrTargetExists", err)
	}
}

// TestASetRefusesAnUnsafeTarget covers the two locations an operator might reach for
// and must not use.
func TestASetRefusesAnUnsafeTarget(t *testing.T) {
	for _, test := range []struct {
		name   string
		target func(w *world) string
		why    string
	}{
		{
			name:   "inside the repository root",
			target: func(w *world) string { return filepath.Join(w.repoRoot, "backup") },
			why:    "a capture would copy the previous set into the next one",
		},
		{
			name:   "inside the temporary root",
			target: func(w *world) string { return filepath.Join(w.tmpRoot, "backup") },
			why:    "the artifacts are never written to a shared temporary directory, and the startup sweep deletes it",
		},
		{
			name:   "the repository root itself",
			target: func(w *world) string { return w.repoRoot },
			why:    "it would copy the root into itself",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := newWorld(t)
			_, err := backup.Create(context.Background(), backup.Options{
				RepoRoot: w.repoRoot, TmpRoot: w.tmpRoot, Target: test.target(w),
				Database: w.database,
			})
			if !errors.Is(err, backup.ErrUnsafeTarget) {
				t.Errorf("error = %v, want ErrUnsafeTarget (%s)", err, test.why)
			}
		})
	}
}

// TestAFailedDumpLeavesNoSet covers a capture that fails partway: it must not leave a
// directory that looks like a set.
func TestAFailedDumpLeavesNoSet(t *testing.T) {
	w := newWorld(t)
	w.database.dumpErr = errors.New("the connection was lost")

	_, err := backup.Create(context.Background(), backup.Options{
		RepoRoot: w.repoRoot, TmpRoot: w.tmpRoot, Target: w.target, Database: w.database,
	})
	if err == nil {
		t.Fatal("a failed dump produced a set")
	}
	if _, statErr := os.Stat(filepath.Join(w.target, backup.ManifestName)); statErr == nil {
		t.Error("a failed capture wrote a manifest, so the directory looks like a set")
	}
	entries, err := os.ReadDir(w.target)
	if err != nil {
		t.Fatalf("read the target: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("a failed capture left %v behind", names)
	}
}

// TestAnEmptyDumpIsRefused covers the failure that looks like success: a dumper that
// exits zero having written nothing would otherwise produce a set whose restore
// silently empties the instance.
func TestAnEmptyDumpIsRefused(t *testing.T) {
	w := newWorld(t)
	w.database.contents = nil

	_, err := backup.Create(context.Background(), backup.Options{
		RepoRoot: w.repoRoot, TmpRoot: w.tmpRoot, Target: w.target, Database: w.database,
	})
	if !errors.Is(err, backup.ErrSetIncomplete) {
		t.Fatalf("an empty dump = %v, want ErrSetIncomplete", err)
	}
}

// TestTheArtifactsAreProtected covers the sensitivity rule: a set is the most
// sensitive data the instance holds, so the code writes 0700 directories and 0400
// files and never leaves a file writable.
//
// **What the operating system honours differs, and the test asserts what it
// actually gets rather than what was requested.** On POSIX the mode bits are the
// protection. On Windows they are not: Go synthesises the permission bits from the
// read-only attribute, so a directory reports 0777 whatever mode was passed. What
// Windows does honour is the read-only attribute, which is why the file assertion
// holds on both platforms — a set's files cannot be edited in place — while the
// directory assertion is POSIX-only. The operator documentation states this rather
// than implying a protection Windows does not provide.
func TestTheArtifactsAreProtected(t *testing.T) {
	w := newWorld(t)
	w.create()

	posix := runtime.GOOS != "windows"

	err := filepath.WalkDir(w.target, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if posix {
				if mode := info.Mode().Perm(); mode&0o077 != 0 {
					t.Errorf("the directory %s is %o, want no group or other access", path, mode)
				}
			}
			return nil
		}
		// A file is never writable, on either platform: the mode's owner-write bit is
		// what Windows maps to the read-only attribute.
		if mode := info.Mode().Perm(); mode&0o200 != 0 {
			t.Errorf("the file %s is %o, want it read-only", path, mode)
		}
		if posix && info.Mode().Perm()&0o077 != 0 {
			t.Errorf("the file %s is %o, want no group or other access", path, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the set: %v", err)
	}

	// The read-only attribute really is in force: writing to an artifact fails.
	dump := filepath.Join(w.target, backup.DatabaseName)
	if err := os.WriteFile(dump, []byte("an edit"), 0o600); err == nil {
		t.Error("an artifact was writable, so the read-only protection is not applied")
	}
}

// TestARestoreIsRefusedWithoutConfirmation is the CLI rule applied at the package
// boundary: a data-changing operation does not run without an explicit
// acknowledgement.
func TestARestoreIsRefusedWithoutConfirmation(t *testing.T) {
	w := newWorld(t)
	w.create()

	err := backup.Restore(context.Background(), backup.RestoreOptions{
		Source: w.target, RepoRoot: w.repoRoot, TmpRoot: w.tmpRoot, Database: w.database,
	})
	if !errors.Is(err, backup.ErrRestoreNotConfirmed) {
		t.Fatalf("an unconfirmed restore = %v, want ErrRestoreNotConfirmed", err)
	}
	if w.database.restored != nil {
		t.Error("an unconfirmed restore touched the database")
	}
}

// TestARestoreIsRefusedFromAnIncompleteSet covers the ordering the package promises:
// a restore never begins overwriting an instance from a set that turns out to be
// incomplete.
func TestARestoreIsRefusedFromAnIncompleteSet(t *testing.T) {
	w := newWorld(t)
	w.create()
	if err := os.Remove(filepath.Join(w.target, backup.DatabaseName)); err != nil {
		t.Fatalf("damage the set: %v", err)
	}

	err := backup.Restore(context.Background(), backup.RestoreOptions{
		Source: w.target, RepoRoot: w.repoRoot, TmpRoot: w.tmpRoot,
		Database: w.database, Confirm: true,
	})
	if !errors.Is(err, backup.ErrSetIncomplete) {
		t.Fatalf("an incomplete set = %v, want ErrSetIncomplete", err)
	}
	if w.database.restored != nil {
		t.Error("an incomplete set was restored anyway")
	}
}

// TestAnUnsupportedFormatIsRefused keeps a set written by a newer build from being
// misread.
func TestAnUnsupportedFormatIsRefused(t *testing.T) {
	w := newWorld(t)
	w.create()

	manifestPath := filepath.Join(w.target, backup.ManifestName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	// A newer format is the plausible case: the build is older than the set.
	updated := bytes.Replace(raw, []byte(`"format": 1`), []byte(`"format": 99`), 1)
	if bytes.Equal(updated, raw) {
		t.Fatal("the manifest does not carry a format field to change")
	}
	makeWritable(t, manifestPath)
	if err := os.WriteFile(manifestPath, updated, 0o400); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}

	if _, err := backup.ReadManifest(w.target); !errors.Is(err, backup.ErrUnsupportedFormat) {
		t.Errorf("a future format = %v, want ErrUnsupportedFormat", err)
	}
}

// TestADirectoryWithoutAManifestIsNotASet keeps a half-copied directory from being
// mistaken for a recovery point.
func TestADirectoryWithoutAManifestIsNotASet(t *testing.T) {
	base := t.TempDir()
	if _, err := backup.ReadManifest(base); !errors.Is(err, backup.ErrSetIncomplete) {
		t.Errorf("a directory with no manifest = %v, want ErrSetIncomplete", err)
	}
}

// TestTheDrillLosesNothing is the acceptance criterion as a test:
// "a maintainer who did not create the backup can restore it by following the
// documentation alone and clone the restored repositories".
//
// It captures a real Git repository and a real LFS object, destroys the instance's
// storage, restores from the set, and then **clones the restored repository with the
// real `git` client** — which is the only way to prove the restored bytes are a
// working repository rather than merely present.
func TestTheDrillLosesNothing(t *testing.T) {
	gitPath := requireGit(t)
	w := newWorld(t)

	// Build a real Git repository with plumbing rather than with a clone.
	//
	// `git clone` reaches a local source through `sh.exe`, whose signal pipe this
	// sandbox refuses, so the fixture is assembled from `git init --bare`,
	// `hash-object`, `mktree`, and `commit-tree` — the same approach the app-level
	// Git tests use, and one that needs no transport at all.
	if err := os.RemoveAll(w.repositoryPath()); err != nil {
		t.Fatalf("clear the placeholder: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(w.repositoryPath()), 0o755); err != nil {
		t.Fatalf("create the fan-out directory: %v", err)
	}
	run(t, gitPath, "", "init", "--quiet", "--bare", "--initial-branch=main", w.repositoryPath())

	blobCommand := exec.Command(gitPath, "-C", w.repositoryPath(), "hash-object", "-w", "--stdin")
	blobCommand.Env = gitEnvironment()
	blobCommand.Stdin = strings.NewReader("# restored\n")
	blobOutput, err := blobCommand.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}

	treeCommand := exec.Command(gitPath, "-C", w.repositoryPath(), "mktree")
	treeCommand.Env = gitEnvironment()
	treeCommand.Stdin = strings.NewReader("100644 blob " + strings.TrimSpace(string(blobOutput)) + "\tREADME.md\n")
	treeOutput, err := treeCommand.Output()
	if err != nil {
		t.Fatalf("mktree: %v", err)
	}

	commitCommand := exec.Command(gitPath, "-C", w.repositoryPath(), "commit-tree",
		strings.TrimSpace(string(treeOutput)), "-m", "the content to survive")
	commitCommand.Env = gitEnvironment()
	commitOutput, err := commitCommand.Output()
	if err != nil {
		t.Fatalf("commit-tree: %v", err)
	}
	run(t, gitPath, w.repositoryPath(), "update-ref", "refs/heads/main", strings.TrimSpace(string(commitOutput)))

	// Capture.
	manifest := w.create()
	if len(manifest.Repositories) != 1 {
		t.Fatalf("the set names %d repositories", len(manifest.Repositories))
	}

	// Destroy the instance's storage, the way a disk loss would.
	if err := os.RemoveAll(w.repoRoot); err != nil {
		t.Fatalf("destroy the repository root: %v", err)
	}
	if _, err := os.Stat(w.lfsPath()); err == nil {
		t.Fatal("the LFS object survived the destruction, so the drill proves nothing")
	}

	// Restore.
	if err := backup.Restore(context.Background(), backup.RestoreOptions{
		Source: w.target, RepoRoot: w.repoRoot, TmpRoot: w.tmpRoot,
		Database: w.database, Confirm: true,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The database half came back.
	if string(w.database.restored) != "a database dump\n" {
		t.Errorf("the restored database is %q, want what was captured", w.database.restored)
	}

	// The LFS object came back, byte for byte.
	restoredLFS, err := os.ReadFile(w.lfsPath())
	if err != nil {
		t.Fatalf("the LFS object was not restored: %v", err)
	}
	if string(restoredLFS) != "a large file's bytes" {
		t.Errorf("the restored LFS object is %q", restoredLFS)
	}

	// The restored repository passes its own integrity check, which is the separate
	// step the operator procedure runs.
	if output, err := runAllowError(t, gitPath, w.repositoryPath(), "fsck", "--strict"); err != nil {
		t.Errorf("the restored repository is damaged: %v\n%s", err, output)
	}
	// And its content is the content that was committed, read back through git
	// rather than through the filesystem, so the object graph is what is checked.
	shown, err := runAllowError(t, gitPath, w.repositoryPath(), "show", "HEAD:README.md")
	if err != nil {
		t.Fatalf("the restored repository's content is unreadable: %v\n%s", err, shown)
	}
	if shown != "# restored\n" {
		t.Errorf("the restored content is %q, want what was committed", shown)
	}

	// Finally the acceptance criterion itself: **clone the restored repository**.
	//
	// It is cloned over HTTP rather than from the local path, and that is not a
	// shortcut. Git's local transport reaches the source through `sh.exe`, whose
	// signal pipe this sandbox refuses (`Win32 error 5`), while an HTTP clone against
	// this server demonstrably works. Over HTTP is also what the criterion means: a
	// maintainer clones what the instance serves, not what is on its disk.
	endpoint, stop := serveRepository(t, gitPath, w, manifest.Repositories[0])
	defer stop()

	cloned := filepath.Join(w.base, "cloned")
	// The clone URL carries the owner segment, because the Git transport routes on
	// `/{owner}/{repo}`.
	if output, err := runAllowError(t, gitPath, "", "clone", "--quiet",
		endpoint+"/alice/"+w.repoName+".git", cloned); err != nil {
		t.Fatalf("cloning the restored repository failed: %v\n%s", err, output)
	}
	content, err := os.ReadFile(filepath.Join(cloned, "README.md"))
	if err != nil {
		t.Fatalf("the cloned repository has no README: %v", err)
	}
	if string(content) != "# restored\n" {
		t.Errorf("the cloned content is %q, want what was committed", content)
	}
}

// serveRepository mounts the Git transport over the restored repository root and
// returns its base URL and a stop function.
//
// The access decision is a fake that grants read, because the decision belongs to the
// repository service and is tested against a real database elsewhere. What this
// proves is that the *restored bytes* serve a clone.
func serveRepository(
	t *testing.T, gitPath string, w *world, repositoryRow backup.Repository,
) (string, func()) {
	t.Helper()

	layout, err := storage.NewLayout(w.repoRoot, w.tmpRoot)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	runner, err := platformgit.NewRunner(gitPath, layout, 2)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ownerID := uuid.NewString()
	row := repository.Repository{
		ID: repositoryRow.ID, OwnerID: ownerID, Name: w.repoName,
		Visibility: repository.VisibilityPublic, StorageID: repositoryRow.StorageID,
	}
	handler, err := githttp.NewHandler(runner, layout,
		&singleOwner{id: ownerID, username: "alice"},
		&readableAccess{row: row}, githttp.AnonymousAuthenticator{}, nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)
	return server.URL, server.Close
}

// singleOwner resolves the one account the drill serves.
type singleOwner struct {
	id       string
	username string
}

func (o *singleOwner) OwnerByUsername(_ context.Context, username string) (identity.User, error) {
	if !strings.EqualFold(strings.TrimSpace(username), o.username) {
		return identity.User{}, identity.ErrInvalidCredentials
	}
	return identity.User{ID: o.id, Username: o.username, Status: identity.StatusActive}, nil
}

// readableAccess grants read on the one repository the drill serves.
type readableAccess struct {
	row repository.Repository
}

func (a *readableAccess) ResolveAndRequire(
	_ context.Context, _ repository.Principal, ownerID, name string, required repository.Level,
) (repository.Repository, repository.Level, error) {
	if ownerID != a.row.OwnerID || !strings.EqualFold(name, a.row.Name) {
		return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
	}
	if !repository.LevelRead.Allows(required) {
		return repository.Repository{}, repository.LevelRead, repository.ErrForbidden
	}
	return a.row, repository.LevelRead, nil
}

// details renders problems as one string for an assertion.
func details(problems []backup.Problem) string {
	parts := make([]string, 0, len(problems))
	for _, problem := range problems {
		parts = append(parts, problem.String())
	}
	return strings.Join(parts, " | ")
}

// inSet counts the problems that make a set unusable.
func inSet(problems []backup.Problem) int {
	count := 0
	for _, problem := range problems {
		if problem.InSet {
			count++
		}
	}
	return count
}

// makeWritable removes the read-only attribute an artifact carries.
//
// A set's files are written 0400 so an accidental edit fails, and on Windows that
// becomes the read-only attribute, so a test that deliberately damages a set has to
// clear it first. On POSIX the owner bit is what matters.
func makeWritable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("make %s writable: %v", path, err)
	}
}

// requireGit returns the git executable or skips.
func requireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}
	return path
}

// run runs git and fails the test on a non-zero exit.
func run(t *testing.T, gitPath, dir string, args ...string) string {
	t.Helper()
	output, err := runAllowError(t, gitPath, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

// gitEnvironment is the environment every git invocation in this file gets.
//
// **The global configuration is disabled, and that is load-bearing rather than
// tidy.** `GIT_CONFIG_NOSYSTEM=1` skips only the system file; a developer's
// `~/.gitconfig` still applies, and a `http.proxy` setting there routes every HTTP
// request in the test through a proxy that is usually not running — which is exactly
// how this drill first failed, with `Failed to connect to 127.0.0.1 ... via
// 127.0.0.1`. `internal/platform/git` already points `GIT_CONFIG_GLOBAL` at the null
// device for the production runner for the same reason; the tests inherit the
// discipline so a personal setting cannot decide whether the suite passes.
func gitEnvironment() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	)
}

// runAllowError runs git and returns its output and error.
func runAllowError(t *testing.T, gitPath, dir string, args ...string) (string, error) {
	t.Helper()
	command := exec.Command(gitPath, args...)
	if dir != "" {
		command.Dir = dir
	}
	command.Env = gitEnvironment()
	output, err := command.CombinedOutput()
	return string(output), err
}
