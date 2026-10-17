package storage

import (
	"fmt"
	"sort"
)

// Divergence is one difference between the database and the disk.
type Divergence struct {
	// StorageID is the repository the difference concerns.
	StorageID string
	// Kind says which side is missing.
	Kind DivergenceKind
}

// DivergenceKind names the direction of a difference.
type DivergenceKind string

const (
	// RowWithoutDirectory is a repository the database knows and the disk does
	// not: a clone would fail until the directory exists.
	RowWithoutDirectory DivergenceKind = "row_without_directory"
	// DirectoryWithoutRow is a directory no database row claims: it consumes
	// space and is invisible to every surface.
	DirectoryWithoutRow DivergenceKind = "directory_without_row"
)

// Report lists the differences found by a comparison.
type Report struct {
	Divergences []Divergence
}

// IsConsistent reports whether the two sides agree.
func (r Report) IsConsistent() bool { return len(r.Divergences) == 0 }

// Counts returns how many divergences of each kind were found, so an operator
// can see the shape of the problem without reading every entry.
func (r Report) Counts() map[DivergenceKind]int {
	counts := map[DivergenceKind]int{}
	for _, divergence := range r.Divergences {
		counts[divergence.Kind]++
	}
	return counts
}

// Reconcile compares the storage identifiers the database knows with the
// directories on disk and reports both directions of divergence.
//
// It changes nothing, so an operator can run it against a live instance to see
// whether a repair is needed before performing one.
func (l *Layout) Reconcile(expected []string) (Report, error) {
	onDisk, err := l.ListRepositories()
	if err != nil {
		return Report{}, err
	}

	known := make(map[string]struct{}, len(expected))
	for _, storageID := range expected {
		canonical, err := canonicalStorageID(storageID)
		if err != nil {
			return Report{}, fmt.Errorf("the database reported %w", err)
		}
		known[canonical] = struct{}{}
	}
	present := make(map[string]struct{}, len(onDisk))
	for _, storageID := range onDisk {
		present[storageID] = struct{}{}
	}

	var report Report
	for storageID := range known {
		if _, found := present[storageID]; !found {
			report.Divergences = append(report.Divergences, Divergence{
				StorageID: storageID,
				Kind:      RowWithoutDirectory,
			})
		}
	}
	for storageID := range present {
		if _, found := known[storageID]; !found {
			report.Divergences = append(report.Divergences, Divergence{
				StorageID: storageID,
				Kind:      DirectoryWithoutRow,
			})
		}
	}

	sort.Slice(report.Divergences, func(i, j int) bool {
		if report.Divergences[i].Kind != report.Divergences[j].Kind {
			return report.Divergences[i].Kind < report.Divergences[j].Kind
		}
		return report.Divergences[i].StorageID < report.Divergences[j].StorageID
	})
	return report, nil
}

// Repair makes the disk match the database.
//
// A repository the database knows and the disk does not is created as an empty
// directory, which is exactly the state a freshly created repository is in
// before its first push: the bytes are the database's to recreate, and the
// directory is Git's.
//
// A directory no row claims is only removed when removeOrphans is set, because
// deleting bytes is not reversible and a stale database snapshot must not be
// able to destroy a repository. It is a deliberate, explicit choice by an
// operator rather than a side effect of a consistency check.
//
// The whole operation is idempotent: running it twice leaves the same state and
// reports no divergence the second time.
func (l *Layout) Repair(expected []string, removeOrphans bool) (Report, error) {
	report, err := l.Reconcile(expected)
	if err != nil {
		return Report{}, err
	}

	for _, divergence := range report.Divergences {
		switch {
		case divergence.Kind == RowWithoutDirectory:
			if _, err := l.EnsureRepository(divergence.StorageID); err != nil {
				return report, err
			}
		case divergence.Kind == DirectoryWithoutRow && removeOrphans:
			if err := l.RemoveRepository(divergence.StorageID); err != nil {
				return report, err
			}
		}
	}

	// Re-read rather than assuming the repairs worked, so the returned report
	// describes the state that is now on disk.
	return l.Reconcile(expected)
}
