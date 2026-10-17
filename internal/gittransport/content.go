// Package gittransport holds the Git-facing layer that the read surfaces and the
// Git transports share.
//
// It answers one kind of question — "what does this repository contain at this
// revision?" — by running the inspection verbs through the bounded runner. It
// never answers an access question: a caller resolves the repository and its
// permission through internal/repository first, and passes only a storage
// identifier here, so there is no path by which this package could be asked
// about a repository its caller was not allowed to see.
package gittransport

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	platformgit "github.com/go-gitnest/GitNest/internal/platform/git"
	"github.com/go-gitnest/GitNest/internal/platform/storage"
)

// Reading bounds.
const (
	// readTimeout bounds one inspection. These read a few megabytes at most, so
	// the protocol timeouts would leave a stuck read holding a slot for half an
	// hour.
	readTimeout = 30 * time.Second
	// MaxFileBytes bounds a file this package will return. A larger file is
	// reported as too large rather than truncated: half a source file is worse
	// than a clear answer, and the raw download path is where large content
	// belongs.
	MaxFileBytes = 1 << 20
	// maxTreeEntries bounds one directory listing, so a repository with a
	// million-entry tree cannot exhaust memory through a browse request.
	maxTreeEntries = 5000
	// maxCommitPage bounds one commit page.
	maxCommitPage = 100
	// maxLogBytes bounds the raw log output before parsing.
	maxLogBytes = 4 << 20
)

// Errors a caller maps onto a response.
var (
	// ErrNotFound reports an unknown revision or a path that is not there. The
	// two are not distinguished, for the same reason a missing repository and an
	// unreadable one are not.
	ErrNotFound = errors.New("the revision or path does not exist")
	// ErrNotAFile reports a path that is not a regular file.
	ErrNotAFile = errors.New("the path is not a file")
	// ErrFileTooLarge reports a file beyond MaxFileBytes.
	ErrFileTooLarge = errors.New("the file is too large to display")
	// ErrEmptyRepository reports a repository with no commits yet.
	ErrEmptyRepository = errors.New("the repository has no commits")
)

// Runner starts a bounded Git operation. It is satisfied by
// *platformgit.Runner.
type Runner interface {
	Run(ctx context.Context, request platformgit.Request) (platformgit.Result, error)
}

// Reader reads repository content.
type Reader struct {
	runner Runner
	layout *storage.Layout
}

// NewReader returns a reader over a runner and a storage layout.
func NewReader(runner Runner, layout *storage.Layout) (*Reader, error) {
	if runner == nil {
		return nil, errors.New("the content reader needs a runner")
	}
	if layout == nil {
		return nil, errors.New("the content reader needs a storage layout")
	}
	return &Reader{runner: runner, layout: layout}, nil
}

// Entry is one item in a directory listing.
type Entry struct {
	Name string
	Path string
	// Type is `file`, `directory`, `symlink`, or `submodule`.
	Type string
	// Size is the object size in bytes. It is zero for a directory.
	Size int64
}

// File is a file's content at a revision.
type File struct {
	Path    string
	Size    int64
	Content []byte
}

// Commit is one commit in a history listing.
type Commit struct {
	ID          string
	Message     string
	AuthorName  string
	AuthorEmail string
	AuthoredAt  string
}

// ResolveCommit resolves a revision to the commit it identifies.
//
// The revision is validated before it becomes an argument, and the answer comes
// from `log -1`, which peels a tag to its commit: `rev-parse` would return the
// tag object's own identifier and the caller would then be addressing the wrong
// object.
func (r *Reader) ResolveCommit(ctx context.Context, storageID, revision string) (string, error) {
	if err := platformgit.ValidateRevision(revision); err != nil {
		return "", err
	}
	out, err := r.run(ctx, storageID, platformgit.ServiceLog,
		[]string{"-1", "--format=%H"}, []string{revision}, 0)
	if err != nil {
		// A repository with no refs cannot resolve anything, and saying so is
		// different from saying a named revision is unknown: a browse surface
		// shows "no commits yet" for the first and "not found" for the second.
		if errors.Is(err, ErrNotFound) {
			if empty, probeErr := r.hasNoRefs(ctx, storageID); probeErr == nil && empty {
				return "", ErrEmptyRepository
			}
		}
		return "", err
	}
	resolved := strings.TrimSpace(out)
	if resolved == "" {
		return "", ErrEmptyRepository
	}
	return resolved, nil
}

// hasNoRefs reports whether the repository has no refs at all, which is the state
// of a repository that has been created but never pushed to.
//
// It runs only after a resolve has already failed, so the common path stays one
// process.
func (r *Reader) hasNoRefs(ctx context.Context, storageID string) (bool, error) {
	out, err := r.run(ctx, storageID, platformgit.ServiceForEachRef,
		[]string{"--count=1", "--format=%(refname)"}, []string{"refs/"}, 0)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// ListTree lists the entries of a directory at a revision.
//
// An empty path lists the repository root.
func (r *Reader) ListTree(ctx context.Context, storageID, revision, path string) ([]Entry, error) {
	if err := platformgit.ValidateRevision(revision); err != nil {
		return nil, err
	}
	if err := platformgit.ValidateRepositoryPath(path); err != nil {
		return nil, err
	}

	// `ls-tree` addresses a path inside a revision with the `rev:path` form. The
	// colon is built here from two separately validated values, so no
	// client-supplied string acquires the meaning of a path separator.
	target := revision
	if path != "" {
		target = revision + ":" + path
	}

	out, err := r.run(ctx, storageID, platformgit.ServiceLsTree,
		[]string{"-z", "-l"}, []string{target}, maxLogBytes)
	if err != nil {
		return nil, err
	}
	return parseTreeListing(out, path)
}

// ReadFile reads a file at a revision.
func (r *Reader) ReadFile(ctx context.Context, storageID, revision, path string) (File, error) {
	if err := platformgit.ValidateRevision(revision); err != nil {
		return File{}, err
	}
	if err := platformgit.ValidateRepositoryPath(path); err != nil {
		return File{}, err
	}
	if path == "" {
		return File{}, ErrNotAFile
	}

	target := revision + ":" + path
	out, err := r.run(ctx, storageID, platformgit.ServiceCatFile,
		[]string{"blob"}, []string{target}, MaxFileBytes)
	if err != nil {
		if errors.Is(err, platformgit.ErrOutputLimit) {
			// The object is larger than this package will return as text.
			return File{}, ErrFileTooLarge
		}
		return File{}, err
	}
	return File{Path: path, Size: int64(len(out)), Content: []byte(out)}, nil
}

// Log lists commits reachable from a revision, newest first.
//
// An empty path lists every commit; a path restricts the history to commits that
// touched it, which is what a file's history view needs.
func (r *Reader) Log(ctx context.Context, storageID, revision, path string, limit, skip int) ([]Commit, error) {
	if err := platformgit.ValidateRevision(revision); err != nil {
		return nil, err
	}
	if err := platformgit.ValidateRepositoryPath(path); err != nil {
		return nil, err
	}
	if err := platformgit.ValidateCount(limit, maxCommitPage); err != nil {
		return nil, err
	}
	if skip < 0 {
		return nil, fmt.Errorf("%w: a skip must not be negative", platformgit.ErrInvalidArgument)
	}

	// The format uses ASCII record and unit separators so a commit message
	// containing a newline, a tab, or any other character cannot break the
	// parse. Percent escapes are Git's own, not the shell's.
	//
	// The options and the positional arguments are kept apart because the runner
	// validates the positional ones as arguments: a revision and a path that
	// travelled as options would skip that check entirely.
	const format = "--format=%H%x1f%s%x1f%an%x1f%ae%x1f%aI%x1e"
	options := []string{format, "-n", strconv.Itoa(limit)}
	if skip > 0 {
		options = append(options, "--skip="+strconv.Itoa(skip))
	}
	serviceArgs := []string{revision}
	var paths []string
	if path != "" {
		// The path travels in the request's Paths field so the runner can place
		// it after a `--` separator. Without that separator Git would try to
		// resolve the path as a revision, which in a bare repository always
		// fails.
		paths = []string{path}
	}

	out, err := r.runWithPaths(ctx, storageID, platformgit.ServiceLog, options, serviceArgs, paths, maxLogBytes)
	if err != nil {
		return nil, err
	}
	return parseLog(out)
}

// run executes one inspection verb against a resolved storage identifier.
//
// The path never comes from a client: it is derived from the identifier through
// the layout, so a repository outside the configured root cannot be addressed
// even by a mistake in a caller.
func (r *Reader) run(ctx context.Context, storageID string, service platformgit.Service, options, args []string, maxBytes int64) (string, error) {
	return r.runWithPaths(ctx, storageID, service, options, args, nil, maxBytes)
}

// runWithPaths is run with a set of client-supplied paths, which the runner
// places after a `--` separator.
func (r *Reader) runWithPaths(ctx context.Context, storageID string, service platformgit.Service, options, args, paths []string, maxBytes int64) (string, error) {
	path, err := r.layout.RepositoryPath(storageID)
	if err != nil {
		return "", err
	}
	exists, err := r.layout.RepositoryExists(storageID)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", ErrNotFound
	}

	limits := platformgit.Limits{Timeout: readTimeout}
	if maxBytes > 0 {
		limits.MaxCapturedBytes = maxBytes
	}

	var out strings.Builder
	_, err = r.runner.Run(ctx, platformgit.Request{
		Service:        service,
		RepositoryPath: path,
		Options:        options,
		ServiceArgs:    args,
		Paths:          paths,
		Stdout:         &out,
		Limits:         limits,
	})
	if err != nil {
		if errors.Is(err, platformgit.ErrOutputLimit) {
			return "", platformgit.ErrOutputLimit
		}
		// A failed inspection means the revision or path is not there for every
		// case a caller can cause, so it is reported as not-found rather than as
		// a server error. The child's diagnostics are dropped: they name paths.
		return "", ErrNotFound
	}
	return out.String(), nil
}

// parseTreeListing decodes `ls-tree -z -l` output.
//
// Each record is `<mode> SP <type> SP <object> SP <size> TAB <name>`, with `-z`
// replacing the record separator with NUL so a name containing a newline or a
// tab is still one record.
func parseTreeListing(raw, parentPath string) ([]Entry, error) {
	records := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	entries := make([]Entry, 0, len(records))
	for _, record := range records {
		if record == "" {
			continue
		}
		metadata, name, found := strings.Cut(record, "\t")
		if !found {
			return nil, fmt.Errorf("unexpected tree entry %q", record)
		}
		fields := strings.Fields(metadata)
		if len(fields) < 4 {
			return nil, fmt.Errorf("unexpected tree entry metadata %q", metadata)
		}
		entryType := classifyMode(fields[0], fields[1])
		size := int64(0)
		if fields[3] != "-" {
			parsed, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil {
				// A size that cannot be read is reported as unknown rather than
				// failing the whole listing: the name and type are still useful.
				parsed = 0
			}
			size = parsed
		}
		path := name
		if parentPath != "" {
			path = parentPath + "/" + name
		}
		entries = append(entries, Entry{Name: name, Path: path, Type: entryType, Size: size})
		if len(entries) >= maxTreeEntries {
			break
		}
	}
	return entries, nil
}

// classifyMode turns an ls-tree mode into a name a response can carry.
//
// The type column Git prints is used when present, because it already
// distinguishes a tag from a commit and a blob from both; the mode is the
// fallback for the forms where Git prints `-`.
func classifyMode(mode, gitType string) string {
	switch {
	case mode == "040000" || gitType == "tree":
		return "directory"
	case mode == "120000":
		return "symlink"
	case mode == "160000" || gitType == "commit":
		return "submodule"
	default:
		return "file"
	}
}

// parseLog decodes the record-separated commit format.
func parseLog(raw string) ([]Commit, error) {
	records := strings.Split(raw, "\x1e")
	commits := make([]Commit, 0, len(records))
	for _, record := range records {
		record = strings.TrimLeft(record, "\n")
		if record == "" {
			continue
		}
		fields := strings.Split(record, "\x1f")
		if len(fields) < 5 {
			// A truncated final record is skipped rather than failing the page:
			// the records before it are still valid commits.
			continue
		}
		commits = append(commits, Commit{
			ID:          strings.TrimSpace(fields[0]),
			Message:     fields[1],
			AuthorName:  fields[2],
			AuthorEmail: fields[3],
			AuthoredAt:  strings.TrimSpace(fields[4]),
		})
	}
	return commits, nil
}
