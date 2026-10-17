// Package storage owns where repository bytes live on disk.
//
// Every path a client can influence is derived here, never accepted from a
// client, and every derived path is proven to sit inside the configured root
// before it is used (docs/security/threat-model.md T-A1 and A6,
// docs/architecture.md section 5).
//
// ## Layout
//
// A repository's bytes live at:
//
//	<root>/<first two hex>/<next two hex>/<storage_id>.git
//
// The fan-out keeps any one directory small, which matters for filesystems that
// degrade with very large directory listings, and it is stable: the path derives
// from the storage identifier, so a rename, a visibility change, and a future
// transfer all leave the bytes exactly where they are.
//
// The identifier in the path is the repository's `storage_id`, not its row
// `id`. The architecture document requires local storage "addressed by internal
// UUID rather than user-supplied names" (docs/architecture.md section 5), while
// urls-and-identifiers.md section 1 requires that the row's stable internal UUID
// never appear in a storage path. Two separate identifiers satisfy both: the row
// identifier reaches foreign keys and events, and this one addresses the bytes.
package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// repositorySuffix is the directory suffix for a bare repository. Git's own
// convention, and it keeps a repository directory recognisable in a listing.
const repositorySuffix = ".git"

// fanOutDepth is the number of two-character prefix directories before the
// repository directory.
const fanOutDepth = 2

var (
	// ErrOutsideRoot reports a path that would resolve outside the configured
	// root. It is returned instead of clamping or rejecting silently, so a
	// caller cannot mistake it for a missing directory.
	ErrOutsideRoot = errors.New("the path is outside the configured storage root")
	// ErrInvalidStorageID reports an identifier that is not a canonical UUID.
	ErrInvalidStorageID = errors.New("the storage identifier is not a valid UUID")
	// ErrNotRepository reports a directory that is not a bare repository.
	ErrNotRepository = errors.New("the directory is not a bare Git repository")
)

// Layout derives and creates repository paths under one root.
type Layout struct {
	// root is absolute, so every containment check compares two absolute paths.
	root    string
	tmpRoot string
}

// NewLayout returns a layout for a repository root and a temporary root.
//
// The roots are made absolute here, once, because a containment check against a
// relative root would depend on the process's working directory at the moment
// of the check rather than at the moment of configuration.
func NewLayout(root, tmpRoot string) (*Layout, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("a repository storage root is required")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve the repository storage root: %w", err)
	}
	if strings.TrimSpace(tmpRoot) == "" {
		return nil, errors.New("a temporary storage root is required")
	}
	absoluteTmp, err := filepath.Abs(tmpRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve the temporary storage root: %w", err)
	}
	return &Layout{root: absoluteRoot, tmpRoot: absoluteTmp}, nil
}

// Root returns the absolute repository root.
func (l *Layout) Root() string { return l.root }

// TmpRoot returns the absolute temporary root.
func (l *Layout) TmpRoot() string { return l.tmpRoot }

// RepositoryPath returns the directory for a storage identifier without touching
// the filesystem.
//
// It refuses an identifier that is not a canonical UUID, so a traversal attempt
// cannot even be expressed, and it checks containment afterwards as defence in
// depth: a future change to the fan-out logic cannot quietly introduce an escape.
func (l *Layout) RepositoryPath(storageID string) (string, error) {
	canonical, err := canonicalStorageID(storageID)
	if err != nil {
		return "", err
	}

	flat := strings.ReplaceAll(canonical, "-", "")
	parts := []string{l.root}
	for i := 0; i < fanOutDepth; i++ {
		parts = append(parts, flat[i*2:i*2+2])
	}
	parts = append(parts, canonical+repositorySuffix)

	derived := filepath.Join(parts...)
	if err := l.checkContained(derived); err != nil {
		return "", err
	}
	return derived, nil
}

// TempPath returns a directory under the temporary root for one operation.
//
// The name is composed from a caller-supplied purpose and a random component, so
// two concurrent operations cannot collide, and it is checked for containment
// against the *temporary* root, which is not necessarily under the repository
// root.
func (l *Layout) TempPath(purpose string) (string, error) {
	if err := validateTempPurpose(purpose); err != nil {
		return "", err
	}
	nonce, err := uuid.NewV7()
	if err != nil {
		nonce = uuid.New()
	}
	derived := filepath.Join(l.tmpRoot, purpose+"-"+nonce.String())
	if err := checkContainedIn(l.tmpRoot, derived); err != nil {
		return "", err
	}
	return derived, nil
}

// EnsureRepository creates the directory for a storage identifier and returns
// its path.
//
// It is idempotent: running it on an existing repository returns the same path
// and changes nothing, which is what makes the repair path safe to re-run.
func (l *Layout) EnsureRepository(storageID string) (string, error) {
	path, err := l.RepositoryPath(storageID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return "", fmt.Errorf("create the repository directory: %w", err)
	}
	return path, nil
}

// RepositoryExists reports whether the directory for a storage identifier is
// present.
func (l *Layout) RepositoryExists(storageID string) (bool, error) {
	path, err := l.RepositoryPath(storageID)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check the repository directory: %w", err)
	}
	return info.IsDir(), nil
}

// RemoveRepository deletes a repository's directory and everything in it.
//
// It is idempotent: a directory that is already gone is not an error, so a
// repair pass that runs twice converges instead of failing the second time.
func (l *Layout) RemoveRepository(storageID string) error {
	path, err := l.RepositoryPath(storageID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove the repository directory: %w", err)
	}
	return nil
}

// ListRepositories returns the storage identifiers present under the root.
//
// It is the disk side of the repair path: comparing this list with the database
// is how a row without a directory and a directory without a row are both found.
// Only directories at the expected depth with a canonical UUID name are
// reported, so an unrelated file cannot be mistaken for a repository.
func (l *Layout) ListRepositories() ([]string, error) {
	entries, err := os.ReadDir(l.root)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the repository root: %w", err)
	}

	found := make([]string, 0, len(entries))
	for _, first := range entries {
		if !isFanOutDirectory(first) {
			continue
		}
		secondLevel, err := os.ReadDir(filepath.Join(l.root, first.Name()))
		if err != nil {
			// A directory that vanished between the two reads is not an error
			// worth failing the whole listing for.
			continue
		}
		for _, second := range secondLevel {
			if !isFanOutDirectory(second) {
				continue
			}
			repositories, err := os.ReadDir(filepath.Join(l.root, first.Name(), second.Name()))
			if err != nil {
				continue
			}
			for _, entry := range repositories {
				if !entry.IsDir() || !strings.HasSuffix(entry.Name(), repositorySuffix) {
					continue
				}
				name := strings.TrimSuffix(entry.Name(), repositorySuffix)
				canonical, err := canonicalStorageID(name)
				if err != nil {
					// Not one of ours: a differently named directory is left
					// alone rather than reported as a repository.
					continue
				}
				// The identifier must also match its own fan-out prefix, or the
				// directory is somewhere this layout would never put it.
				expected, err := l.RepositoryPath(canonical)
				if err != nil {
					continue
				}
				if expected != filepath.Join(l.root, first.Name(), second.Name(), entry.Name()) {
					continue
				}
				found = append(found, canonical)
			}
		}
	}
	sort.Strings(found)
	return found, nil
}

// EnsureTmpRoot creates the temporary root.
func (l *Layout) EnsureTmpRoot() error {
	if err := os.MkdirAll(l.tmpRoot, 0o750); err != nil {
		return fmt.Errorf("create the temporary root: %w", err)
	}
	return nil
}

// CleanupTemp removes everything under the temporary root.
//
// It runs at startup and after each operation, so a process that died mid-Git
// operation does not leave scratch data behind. Removing the root itself and
// recreating it is the simplest way to be sure nothing was missed.
func (l *Layout) CleanupTemp() error {
	if err := os.RemoveAll(l.tmpRoot); err != nil {
		return fmt.Errorf("clear the temporary root: %w", err)
	}
	return l.EnsureTmpRoot()
}

// checkContained refuses a path that is not strictly inside the root.
func (l *Layout) checkContained(path string) error {
	return checkContainedIn(l.root, path)
}

// checkContainedIn refuses a path that is not strictly inside base.
//
// The comparison is between resolved absolute paths rather than string prefixes,
// so `/srv/repos-evil` is not accepted as being inside `/srv/repos`.
func checkContainedIn(base, path string) error {
	relative, err := filepath.Rel(base, path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOutsideRoot, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(relative) {
		return fmt.Errorf("%w: %q", ErrOutsideRoot, path)
	}
	return nil
}

// isFanOutDirectory reports whether a directory entry has the two-character name
// the fan-out uses.
func isFanOutDirectory(entry os.DirEntry) bool {
	if !entry.IsDir() {
		return false
	}
	name := entry.Name()
	if len(name) != 2 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isHexDigit(name[i]) {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// canonicalStorageID validates an identifier and returns its canonical form.
//
// It accepts the hyphenated form the application generates and the hyphenless
// form a filesystem listing produces, and rejects everything else. Requiring a
// UUID is what makes traversal inexpressible rather than merely filtered.
func canonicalStorageID(storageID string) (string, error) {
	trimmed := strings.TrimSpace(storageID)
	if trimmed == "" {
		return "", ErrInvalidStorageID
	}
	parsed, err := uuid.Parse(trimmed)
	if err != nil {
		return "", ErrInvalidStorageID
	}
	return parsed.String(), nil
}

// validateTempPurpose checks the caller-supplied part of a temporary directory
// name, so a purpose string cannot introduce a separator or a traversal.
func validateTempPurpose(purpose string) error {
	if purpose == "" {
		return errors.New("a temporary directory needs a purpose")
	}
	if len(purpose) > 32 {
		return errors.New("a temporary directory purpose is too long")
	}
	for i := 0; i < len(purpose); i++ {
		c := purpose[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return fmt.Errorf("a temporary directory purpose may only contain lowercase letters, digits, - and _")
		}
	}
	return nil
}

// DiscardTemp removes one temporary directory, reporting nothing when it is
// already gone.
func DiscardTemp(path string) error {
	if path == "" {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove the temporary directory: %w", err)
	}
	return nil
}
