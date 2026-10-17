// Package backup builds, verifies, and restores a GitNest recovery set.
//
// # Why one set
//
// docs/architecture.md section 8 states the rule this package exists to satisfy:
// "Database and repository-storage backups are one recovery set with one
// documented procedure. Database transactions cannot make filesystem moves atomic,
// so any cross-boundary operation needs an idempotent repair path."
//
// The consequence is that a recovery set is not "a database dump" plus "a storage
// copy" that an operator assembles by hand. It is one directory with one manifest
// that describes both halves, records what the database said should be there, and
// is verified as a unit — because a set whose two halves were captured at
// different moments, or one of whose halves is incomplete, is the failure mode
// this package is written to make loud.
//
// # What a set contains
//
//	<target>/manifest.json     what the set is, and the evidence it is complete
//	<target>/database.dump     the PostgreSQL dump, in the format the dumper chose
//	<target>/repositories/     a copy of the repository root
//
// The repository root holds both the bare Git repositories and the LFS objects,
// because the LFS store lives under `<root>/lfs` (see
// internal/platform/storage/lfs.go). One copy therefore captures both, and there is
// no second path that can be forgotten.
//
// # The sensitivity rule
//
// docs/architecture/administration.md: "Backup artifacts are the most sensitive
// data the instance holds." So a set is created 0700 with 0600 files, it is refused
// inside the repository root or the temporary root, and it never contains a
// `GITNEST_SECRET_KEY` — docs/architecture/secrets.md requires the key to be kept
// "separately from the database backup, because a backup that contains both the
// ciphertext and the key protects nothing". Nothing in this package reads that
// variable.
//
// # Scope in phase 1
//
// This is the single-node baseline procedure. docs/roadmap/10-operations-and-scale.md
// W10.11 extends it into a quiesced, lock-coordinated set for a multi-node
// deployment with object storage, "built from the earlier backup work" — this one.
// What phase 1 does not do is stated in the operator documentation rather than left
// for an operator to discover: there is no point-in-time recovery, no incremental
// capture, and no coordination with a second node.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The names a set is made of. They are constants because the operator
// documentation names them and a mismatch would be silent.
const (
	// ManifestName is the file that describes the set.
	ManifestName = "manifest.json"
	// DatabaseName is the database dump.
	DatabaseName = "database.dump"
	// RepositoriesName is the directory holding the repository root's copy.
	RepositoriesName = "repositories"
)

// FormatVersion identifies the set layout.
//
// It is recorded so a restore can refuse a set written by a newer GitNest rather
// than misreading it. A restore of an older *supported* format is a decision for the
// release that changes it, and the constant is where that decision is made.
const FormatVersion = 1

// Errors this package reports.
var (
	// ErrTargetExists reports a target directory that already holds something.
	ErrTargetExists = errors.New("the backup target already holds a set")
	// ErrUnsafeTarget reports a target the rules forbid.
	ErrUnsafeTarget = errors.New("the backup target is not a usable location")
	// ErrSetIncomplete reports a set that failed verification.
	ErrSetIncomplete = errors.New("the recovery set is incomplete or inconsistent")
	// ErrUnsupportedFormat reports a set written by a newer GitNest.
	ErrUnsupportedFormat = errors.New("the recovery set's format is not supported by this build")
	// ErrRestoreNotConfirmed reports a restore that was not explicitly confirmed.
	ErrRestoreNotConfirmed = errors.New("restoring overwrites the instance; pass the confirmation flag to proceed")
	// ErrDatabaseMissing reports a set whose dump is absent.
	ErrDatabaseMissing = errors.New("the recovery set carries no database dump")
)

// Manifest describes a recovery set and carries the evidence that it is complete.
//
// It is the reason a set is verifiable at all: without it, "the backup finished"
// is a claim about a command's exit status rather than about the bytes on disk.
type Manifest struct {
	// Format is the set layout version.
	Format int `json:"format"`
	// CreatedAt is when the set was captured, in UTC.
	CreatedAt time.Time `json:"created_at"`
	// BuildVersion is the GitNest build that captured it, so an operator can tell
	// which procedure produced it.
	BuildVersion string `json:"build_version"`
	// SchemaVersion is the newest applied migration the database reported. A
	// restore into a build older than this is refused by the migration runner
	// rather than by this package, but recording it is what lets an operator see
	// the mismatch before trying.
	SchemaVersion string `json:"schema_version"`
	// Database describes the dump.
	Database Artifact `json:"database"`
	// Repositories is one entry per repository the database knew about at capture
	// time. It is the cross-boundary evidence: a restore can say whether a
	// repository the database references has its bytes in the set.
	Repositories []Repository `json:"repositories"`
	// RepositoriesBytes is the total size of the copied repository root.
	RepositoriesBytes int64 `json:"repositories_bytes"`
	// RepositoriesFiles is the file count of the copied repository root.
	RepositoriesFiles int `json:"repositories_files"`
	// Notes are limits of the capture that the operator should know, recorded in
	// the set itself so they travel with it.
	Notes []string `json:"notes,omitempty"`
}

// Problem is one finding from a verification.
//
// The distinction it carries is the difference between a useless set and a damaged
// instance, and conflating the two was a real defect in an earlier version of this
// code: a capture refused to finish because one repository's directory was already
// missing, which threw away the chance to back up every other repository. So a
// finding about the *set* makes the set unusable, while a finding about the
// *instance* is reported and the capture proceeds — an instance that is already
// unwell is exactly when its backup matters most.
type Problem struct {
	// InSet reports whether the set itself is broken. False means the set is
	// complete and the finding describes the instance at capture time.
	InSet bool
	// Detail is the finding, phrased for an operator.
	Detail string
}

func (p Problem) String() string { return p.Detail }

// inSetProblems returns the details of the findings that make a set unusable.
func inSetProblems(problems []Problem) []string {
	var details []string
	for _, problem := range problems {
		if problem.InSet {
			details = append(details, problem.Detail)
		}
	}
	return details
}

// instanceFindings returns the details of the findings about the instance.
func instanceFindings(problems []Problem) []string {
	var details []string
	for _, problem := range problems {
		if !problem.InSet {
			details = append(details, problem.Detail)
		}
	}
	return details
}

// Artifact describes one file in the set.
type Artifact struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Repository is one repository's presence in the set.
type Repository struct {
	// ID is the row identifier, which is what an audit record and a URL use.
	ID string `json:"id"`
	// StorageID is the directory name under the repository root.
	StorageID string `json:"storage_id"`
	// Name is recorded for the operator's benefit, never to derive a path.
	Name string `json:"name"`
	// Files and Bytes are what the copy holds for this repository, so verification
	// can tell a truncated copy from a complete one.
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// Exists reports whether the directory was present at capture time. It is
	// recorded rather than assumed: a repository row without a directory is the
	// divergence the repair path exists to fix, and a backup is exactly when an
	// operator must learn about it.
	Exists bool `json:"exists"`
}

// DatabaseInfo is what a capture must learn from the database.
type DatabaseInfo struct {
	// SchemaVersion is the newest applied migration.
	SchemaVersion string
	// Repositories are the repository rows, so the manifest can record what should
	// be on disk.
	Repositories []RepositoryRef
}

// RepositoryRef is a repository row, as the database reports it.
type RepositoryRef struct {
	ID        string
	StorageID string
	Name      string
}

// Database captures and restores the database half of the set.
//
// It is an interface because the round trip has to be testable without a database,
// and because a restore into a clean instance is a different environment from the
// capture. The production implementation is in internal/platform/pgdump, which
// invokes `pg_dump` and `pg_restore` with argument arrays and credentials passed
// through the environment rather than the command line.
type Database interface {
	// Dump writes the database to w and reports what the set must record. The
	// writer receives the dump's bytes and nothing else, so the implementation
	// never sees the set's directory.
	Dump(ctx context.Context, w io.Writer) (DatabaseInfo, error)
	// Restore replaces the database's contents from r.
	Restore(ctx context.Context, r io.Reader) error
}

// Options are the inputs a capture takes.
type Options struct {
	// RepoRoot is the repository root to copy.
	RepoRoot string
	// TmpRoot is the temporary root, which a target may not be inside.
	TmpRoot string
	// Target is where the set is written.
	Target string
	// BuildVersion is recorded in the manifest.
	BuildVersion string
	// Database captures the database half.
	Database Database
	// Now is the clock, so a test can assert a deterministic manifest.
	Now func() time.Time
	// Log receives progress lines. It may be nil.
	Log func(string, ...any)
}

// Create captures a recovery set.
//
// It verifies what it wrote before returning, because a backup nobody has read back
// is a hope rather than a backup. The verification is the same one `Verify` runs, so
// a set that passes here passes later.
func Create(ctx context.Context, options Options) (Manifest, error) {
	if err := options.validate(); err != nil {
		return Manifest{}, err
	}
	if err := ensureEmptyTarget(options.Target); err != nil {
		return Manifest{}, err
	}
	if err := os.MkdirAll(options.Target, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("create the backup target: %w", err)
	}

	manifest := Manifest{
		Format:       FormatVersion,
		CreatedAt:    options.now().UTC(),
		BuildVersion: options.BuildVersion,
		Notes:        captureNotes(),
	}

	// The database first: it is what says which repositories should be on disk, so
	// capturing it first means the storage copy is checked against the state that
	// was current when the set began.
	databasePath := filepath.Join(options.Target, DatabaseName)
	dumpFile, err := os.OpenFile(databasePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Manifest{}, fmt.Errorf("create the database dump: %w", err)
	}
	digest := sha256.New()
	info, dumpErr := options.Database.Dump(ctx, io.MultiWriter(dumpFile, digest))
	closeErr := dumpFile.Close()
	if dumpErr != nil {
		// A half-written dump is removed rather than left to look like a set.
		_ = os.Remove(databasePath)
		return Manifest{}, fmt.Errorf("dump the database: %w", dumpErr)
	}
	if closeErr != nil {
		_ = os.Remove(databasePath)
		return Manifest{}, fmt.Errorf("dump the database: %w", closeErr)
	}
	// The dump becomes read-only like every other artifact in the set. Leaving it
	// writable while the manifest is not was an inconsistency worth fixing: a set is
	// never modified after it is written, and a writable member is the one an
	// accident can reach.
	if err := makeReadOnly(databasePath); err != nil {
		return Manifest{}, err
	}

	manifest.SchemaVersion = info.SchemaVersion
	size, err := fileSize(databasePath)
	if err != nil {
		return Manifest{}, err
	}
	if size == 0 {
		// An empty dump is a failure, not a small database: it means the dumper
		// produced nothing, and restoring it would silently empty the instance.
		_ = os.Remove(databasePath)
		return Manifest{}, fmt.Errorf("%w: the dump is empty", ErrSetIncomplete)
	}
	manifest.Database = Artifact{Bytes: size, SHA256: hex.EncodeToString(digest.Sum(nil))}

	// The storage copy. LFS objects are inside the repository root, so this one
	// copy captures them with the repositories.
	copied, err := copyTree(options.RepoRoot, filepath.Join(options.Target, RepositoriesName))
	if err != nil {
		return Manifest{}, fmt.Errorf("copy the repository root: %w", err)
	}
	manifest.RepositoriesBytes = copied.Bytes
	manifest.RepositoriesFiles = copied.Files

	// Each repository the database named, with what the copy holds for it. A row
	// whose directory is absent is recorded as absent rather than omitted: the
	// operator has to learn about the divergence, and a backup is when.
	for _, reference := range info.Repositories {
		entry := Repository{ID: reference.ID, StorageID: reference.StorageID, Name: reference.Name}
		path := filepath.Join(options.Target, RepositoriesName, repositoryDirectory(reference.StorageID))
		if measured, err := measureTree(path); err == nil {
			entry.Exists = true
			entry.Files = measured.Files
			entry.Bytes = measured.Bytes
		}
		manifest.Repositories = append(manifest.Repositories, entry)
	}
	sort.Slice(manifest.Repositories, func(i, j int) bool {
		return manifest.Repositories[i].StorageID < manifest.Repositories[j].StorageID
	})

	if err := writeManifest(options.Target, manifest); err != nil {
		return Manifest{}, err
	}
	options.log("backup captured", "target", options.Target,
		"repositories", len(manifest.Repositories), "bytes", manifest.RepositoriesBytes)

	// Read it back. This is the step that turns "the command exited zero" into "the
	// set is complete".
	//
	// Only a finding about the *set* fails the capture. A repository the database
	// references but the disk does not hold is a finding about the instance, and
	// refusing to finish because of it would discard the backup of every other
	// repository — the opposite of what an operator in trouble needs.
	problems, err := Verify(options.Target)
	if err != nil {
		return Manifest{}, err
	}
	if details := inSetProblems(problems); len(details) > 0 {
		return Manifest{}, fmt.Errorf("%w: %s", ErrSetIncomplete, strings.Join(details, "; "))
	}
	for _, finding := range instanceFindings(problems) {
		options.log("the instance is already inconsistent; the set records it", "finding", finding)
	}
	return manifest, nil
}

// Verify checks a set against its manifest and returns every problem it found.
//
// It reports all of them rather than the first, because an operator recovering from
// a bad backup needs the whole picture in one pass. An empty result with a nil error
// means the set is complete and self-consistent.
//
// What it can prove and what it cannot is worth stating, because a verification
// that overstates itself is worse than none:
//
//   - It proves every file the manifest lists is present, that the dump's size and
//     digest are what was recorded, and that each repository's file count and byte
//     total still match.
//   - It does not prove a repository's *content* is uncorrupted beyond that: a
//     Git repository's integrity is what `git fsck` is for, and the operator
//     procedure runs it as a separate step. Nor does it prove the database dump
//     restores — only a restore into a clean instance proves that, which is what
//     the drill is.
func Verify(target string) ([]Problem, error) {
	manifest, err := ReadManifest(target)
	if err != nil {
		return nil, err
	}

	var problems []Problem
	report := func(inSet bool, format string, arguments ...any) {
		problems = append(problems, Problem{InSet: inSet, Detail: fmt.Sprintf(format, arguments...)})
	}

	databasePath := filepath.Join(target, DatabaseName)
	size, err := fileSize(databasePath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		report(true, "the database dump is missing")
	case err != nil:
		return nil, err
	case size != manifest.Database.Bytes:
		report(true, "the database dump is %d bytes but the manifest records %d", size, manifest.Database.Bytes)
	default:
		digest, err := fileDigest(databasePath)
		if err != nil {
			return nil, err
		}
		if digest != manifest.Database.SHA256 {
			// The dump changed after it was written, which no legitimate step does.
			report(true, "the database dump's digest does not match the manifest")
		}
	}

	// A missing copy directory is a finding rather than an error, because it is the
	// shape the "several things are wrong" case takes and an operator needs the rest
	// of the findings too.
	root := filepath.Join(target, RepositoriesName)
	total, measureErr := measureTree(root)
	if errors.Is(measureErr, fs.ErrNotExist) {
		report(true, "the repository copy is missing from the set")
	} else if measureErr != nil {
		return nil, measureErr
	} else {
		if total.Bytes != manifest.RepositoriesBytes {
			report(true, "the repository copy is %d bytes but the manifest records %d",
				total.Bytes, manifest.RepositoriesBytes)
		}
		if total.Files != manifest.RepositoriesFiles {
			report(true, "the repository copy holds %d files but the manifest records %d",
				total.Files, manifest.RepositoriesFiles)
		}
	}

	for _, entry := range manifest.Repositories {
		if !entry.Exists {
			// Recorded as absent at capture time. Saying so on every verification is
			// deliberate: the operator must not have to compare two manifest versions
			// to learn that a repository's bytes were never in the set. It is *not* a
			// reason to call the set unusable — the set holds everything that existed.
			report(false, "repository %s (%s) had no directory when the set was captured",
				entry.Name, entry.StorageID)
			continue
		}
		path := filepath.Join(root, repositoryDirectory(entry.StorageID))
		measured, err := measureTree(path)
		if errors.Is(err, fs.ErrNotExist) {
			report(true, "repository %s (%s) is missing from the set", entry.Name, entry.StorageID)
			continue
		}
		if err != nil {
			return nil, err
		}
		if measured.Files != entry.Files || measured.Bytes != entry.Bytes {
			report(true, "repository %s (%s) holds %d files and %d bytes but the manifest records %d and %d",
				entry.Name, entry.StorageID, measured.Files, measured.Bytes, entry.Files, entry.Bytes)
		}
	}
	return problems, nil
}

// RestoreOptions are the inputs a restore takes.
type RestoreOptions struct {
	// Source is the set to restore.
	Source string
	// RepoRoot is the repository root to write into.
	RepoRoot string
	// TmpRoot is the temporary root, which the source may not be inside.
	TmpRoot string
	// Database replaces the database's contents.
	Database Database
	// Confirm is the operator's explicit acknowledgement that this overwrites the
	// instance. Without it the call is refused, per the CLI rule that a
	// data-changing command does not run without an explicit confirmation.
	Confirm bool
	// Log receives progress lines. It may be nil.
	Log func(string, ...any)
}

// Restore writes a verified set back into an instance.
//
// The order matters and is deliberate: the set is verified first, so a restore never
// begins overwriting an instance from a set that turns out to be incomplete. The
// database is restored before the storage, because a repository row whose bytes
// arrive second is recoverable — the repair path handles it — while bytes whose row
// never arrives are invisible to the instance.
func Restore(ctx context.Context, options RestoreOptions) error {
	if !options.Confirm {
		return ErrRestoreNotConfirmed
	}
	if options.Source == "" || options.RepoRoot == "" {
		return fmt.Errorf("%w: a restore needs a source and a repository root", ErrUnsafeTarget)
	}
	if options.Database == nil {
		return errors.New("a restore needs a database")
	}
	if err := assertOutside(options.Source, options.RepoRoot, options.TmpRoot, "source"); err != nil {
		return err
	}

	manifest, err := ReadManifest(options.Source)
	if err != nil {
		return err
	}
	problems, err := Verify(options.Source)
	if err != nil {
		return err
	}
	if details := inSetProblems(problems); len(details) > 0 {
		return fmt.Errorf("%w: %s", ErrSetIncomplete, strings.Join(details, "; "))
	}
	// A finding about the instance does not stop the restore: everything the set
	// holds is still worth restoring, and the operator is told what it did not hold.
	for _, finding := range instanceFindings(problems) {
		options.log("the set did not hold a repository the database references", "finding", finding)
	}
	options.log("restoring the set", "source", options.Source,
		"schema_version", manifest.SchemaVersion, "repositories", len(manifest.Repositories))

	dumpPath := filepath.Join(options.Source, DatabaseName)
	dump, err := os.Open(dumpPath)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDatabaseMissing, err)
	}
	restoreErr := options.Database.Restore(ctx, dump)
	closeErr := dump.Close()
	if restoreErr != nil {
		return fmt.Errorf("restore the database: %w", restoreErr)
	}
	if closeErr != nil {
		return fmt.Errorf("restore the database: %w", closeErr)
	}
	options.log("database restored")

	// The storage copy goes into the repository root, which the instance must not be
	// writing to. That is the maintenance window the operator documentation
	// describes: this step is not safe against a running server.
	copied, err := copyTree(filepath.Join(options.Source, RepositoriesName), options.RepoRoot)
	if err != nil {
		return fmt.Errorf("restore the repository root: %w", err)
	}
	options.log("repository storage restored", "files", copied.Files, "bytes", copied.Bytes)
	return nil
}

// log writes a progress line when the caller asked for one.
func (o RestoreOptions) log(message string, attributes ...any) {
	if o.Log != nil {
		o.Log(message, attributes...)
	}
}

// ReadManifest loads a set's manifest.
func ReadManifest(target string) (Manifest, error) {
	path := filepath.Join(target, ManifestName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Manifest{}, fmt.Errorf("%w: %s holds no %s", ErrSetIncomplete, target, ManifestName)
		}
		return Manifest{}, fmt.Errorf("read the manifest: %w", err)
	}

	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse the manifest: %w", err)
	}
	if manifest.Format != FormatVersion {
		return Manifest{}, fmt.Errorf("%w: the set is format %d and this build reads %d",
			ErrUnsupportedFormat, manifest.Format, FormatVersion)
	}
	return manifest, nil
}

// validate checks the inputs a capture needs.
func (o Options) validate() error {
	if o.Database == nil {
		return errors.New("a backup needs a database")
	}
	if o.RepoRoot == "" || o.Target == "" {
		return fmt.Errorf("%w: a backup needs a repository root and a target", ErrUnsafeTarget)
	}
	return assertOutside(o.Target, o.RepoRoot, o.TmpRoot, "target")
}

func (o Options) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}

func (o Options) log(message string, attributes ...any) {
	if o.Log != nil {
		o.Log(message, attributes...)
	}
}

// assertOutside refuses a location inside the repository root or the temporary
// root.
//
// Putting a set inside the repository root would make a later capture copy the
// previous set, so each backup would carry every earlier one. Putting it in the
// temporary root breaks the rule that the artifacts are "never written to a shared
// temporary directory" — and that directory is swept at startup, so the backup would
// be deleted by the next restart.
func assertOutside(path, repoRoot, tmpRoot, what string) error {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafeTarget, err)
	}
	for _, candidate := range []struct{ name, root string }{
		{"the repository root", repoRoot},
		{"the temporary root", tmpRoot},
	} {
		if candidate.root == "" {
			continue
		}
		root, err := filepath.Abs(candidate.root)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnsafeTarget, err)
		}
		if contains(root, resolved) {
			return fmt.Errorf("%w: the %s must not be inside %s (%s)",
				ErrUnsafeTarget, what, candidate.name, root)
		}
	}
	return nil
}

// contains reports whether path is root or lies inside it.
func contains(root, path string) bool {
	if root == path {
		return true
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// ensureEmptyTarget refuses a target that already holds something.
//
// It is refused rather than merged: a set is one capture, and a directory holding
// two intermixed captures is not a recovery point anybody can restore.
func ensureEmptyTarget(target string) error {
	entries, err := os.ReadDir(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the backup target: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s holds %d entries", ErrTargetExists, target, len(entries))
	}
	return nil
}

// repositoryDirectory returns the directory a storage identifier occupies under the
// repository root.
//
// It mirrors internal/platform/storage's fan-out. It is duplicated rather than
// imported because the backup reads a *copy* of the root, and a restore may read a
// set produced by another build — so this must not follow a change to the live
// layout without the format version moving with it.
func repositoryDirectory(storageID string) string {
	flat := strings.ReplaceAll(storageID, "-", "")
	if len(flat) < 4 {
		// A malformed identifier cannot be placed. The caller treats the absence as a
		// finding rather than a path.
		return storageID
	}
	return filepath.Join(flat[0:2], flat[2:4], storageID+".git")
}

// measurement is what a directory holds.
type measurement struct {
	Files int
	Bytes int64
}

// measureTree totals a directory's files and bytes.
func measureTree(root string) (measurement, error) {
	var measured measurement
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		measured.Files++
		measured.Bytes += info.Size()
		return nil
	})
	return measured, err
}

// copyTree copies a directory tree, preserving modes.
//
// It is a copy rather than a link, an archive, or a rename, because the set has to
// survive the source being deleted — that is what a backup is for. Symbolic links are
// recreated as links rather than followed: a repository can legitimately contain one,
// and following it would copy something outside the tree into the set.
func copyTree(source, target string) (measurement, error) {
	var copied measurement
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// A repository root that does not exist yet is not an error: an
				// instance with no repositories has nothing to copy, and the set is
				// still a valid set.
				return nil
			}
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)

		switch {
		case entry.IsDir():
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(destination, info.Mode().Perm())
		case entry.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			// A pre-existing link would make Readlink on the next run read the target
			// rather than the link, so it is removed first.
			_ = os.Remove(destination)
			return os.Symlink(link, destination)
		default:
			return copyFile(path, destination, &copied)
		}
	})
	return copied, err
}

// copyFile copies one regular file, counting what it wrote.
func copyFile(source, destination string, counted *measurement) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}

	from, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = from.Close() }()

	// A Git object is already read-only, so the copy's mode is taken from the
	// original with the write bit removed for the owner: a set is not modified
	// after it is written, and a read-only copy makes an accidental edit fail loudly
	// rather than silently invalidate the digest.
	mode := info.Mode().Perm() &^ 0o200
	to, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	written, copyErr := io.Copy(to, from)
	closeErr := to.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	counted.Files++
	counted.Bytes += written
	return nil
}

// writeManifest writes the manifest, then makes it read-only for the same reason a
// copied object is.
func writeManifest(target string, manifest Manifest) error {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the manifest: %w", err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join(target, ManifestName)
	if err := os.WriteFile(path, encoded, 0o400); err != nil {
		return fmt.Errorf("write the manifest: %w", err)
	}
	return nil
}

// makeReadOnly clears every write bit on a path.
//
// On Windows this becomes the read-only attribute, which is the only part of the
// mode an NTFS file carries; on POSIX the mode itself is the protection.
func makeReadOnly(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.Chmod(path, info.Mode().Perm()&^0o222); err != nil {
		return fmt.Errorf("protect %s: %w", path, err)
	}
	return nil
}

// fileSize returns a file's size.
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// fileDigest returns a file's SHA-256 digest in lowercase hexadecimal.
func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// captureNotes are the limits of a phase-1 capture, recorded in the set itself so
// they travel with it rather than living only in a document an operator may not have
// open.
func captureNotes() []string {
	return []string{
		"the capture is not point-in-time consistent across the database and the storage: " +
			"a write that lands during the capture can be in one half and not the other",
		"no point-in-time recovery and no incremental capture: this is a full set",
		"restoring is not safe against a running server; stop it first",
		"the instance secret key is deliberately not part of this set and must be backed up separately",
	}
}
