package git

import (
	"fmt"
	"strings"
)

// readOnlyServices are the inspection verbs the server runs to read repository
// content for a read surface.
//
// ## Why the allowlist was extended
//
// docs/security/threat-model.md T-A1 names a fixed allowlist of
// `upload-pack`, `receive-pack`, and `upload-archive`. The repository browser
// cannot serve a tree, a file, or a commit list without reading objects, so the
// allowlist now also carries these verbs. That is a deliberate extension of a
// security control, and the properties that keep it safe are:
//
//   - **Every one is read-only.** None writes a ref, an object, or a working
//     tree, so a caller cannot change state through this path even in principle.
//   - **None takes an option from a caller.** The options are chosen here; the
//     only client-derived values are a revision, a path, and a bounded count,
//     each validated before it becomes an argument.
//   - **The repository is selected by this process, not by a client**, and the
//     path is proven inside the storage root before the process starts.
//   - **The client still passes the repository permission service first.** The
//     allowlist narrows what can run; it does not replace authorization.
//
// A verb outside both lists starts no process at all.
var readOnlyServices = map[Service]struct{}{
	// Resolves a revision to a commit and reports whether it exists.
	ServiceRevParse: {},
	// Reads an object: its type, or its bytes.
	ServiceCatFile: {},
	// Lists a tree entry by entry.
	ServiceLsTree: {},
	// Walks history.
	ServiceLog: {},
	// Lists refs.
	ServiceForEachRef: {},
}

// The inspection verbs.
const (
	// ServiceRevParse resolves a revision.
	ServiceRevParse Service = "rev-parse"
	// ServiceCatFile reads an object.
	ServiceCatFile Service = "cat-file"
	// ServiceLsTree lists a tree.
	ServiceLsTree Service = "ls-tree"
	// ServiceLog walks history.
	ServiceLog Service = "log"
	// ServiceForEachRef lists refs.
	ServiceForEachRef Service = "for-each-ref"
)

// ReadOnly reports whether a service only reads.
//
// The distinction matters for the request builder: an inspection verb is given
// the repository through `-C`, while a protocol verb receives it as the trailing
// path its own command line defines.
func ReadOnly(service Service) bool {
	_, found := readOnlyServices[service]
	return found
}

// maxRevisionLength bounds a revision before it becomes an argument.
const maxRevisionLength = 256

// maxRepositoryPathLength bounds a path inside a repository.
const maxRepositoryPathLength = 1024

// ValidateRevision checks a client-supplied revision.
//
// It accepts what a browse surface needs — a branch, a tag, a full or
// abbreviated object identifier, and the `~`/`^` ancestry operators — and
// refuses everything that would let the value mean something else:
//
//   - a leading `-`, which is how a value becomes an option;
//   - `..`, which is a *range*, so accepting it would silently widen a request
//     for one commit into a walk over many;
//   - `:`, which introduces `rev:path` and would let the value address a path
//     this process never validated;
//   - `@{`, whose reflog and upstream forms resolve through state rather than
//     through a ref;
//   - control characters, whitespace, and the remaining Git special characters.
//
// It is applied to the revision *before* ValidateArgument, which is a coarser
// check; both must pass.
func ValidateRevision(revision string) error {
	if revision == "" {
		return fmt.Errorf("%w: a revision must not be empty", ErrInvalidArgument)
	}
	if len(revision) > maxRevisionLength {
		return fmt.Errorf("%w: a revision is longer than %d bytes", ErrInvalidArgument, maxRevisionLength)
	}
	if strings.HasPrefix(revision, "-") {
		return fmt.Errorf("%w: a revision must not begin with %q", ErrInvalidArgument, "-")
	}
	if strings.Contains(revision, "..") {
		return fmt.Errorf("%w: a revision must not contain %q, which denotes a range", ErrInvalidArgument, "..")
	}
	if strings.Contains(revision, ":") {
		return fmt.Errorf("%w: a revision must not contain %q, which introduces a path", ErrInvalidArgument, ":")
	}
	if strings.Contains(revision, "@{") {
		return fmt.Errorf("%w: a revision must not contain %q", ErrInvalidArgument, "@{")
	}
	if strings.Contains(revision, "//") || strings.HasPrefix(revision, "/") || strings.HasSuffix(revision, "/") {
		return fmt.Errorf("%w: a revision must not contain an empty path component", ErrInvalidArgument)
	}
	for i := 0; i < len(revision); i++ {
		c := revision[i]
		switch {
		case c < 0x20 || c == 0x7f:
			return fmt.Errorf("%w: a revision contains a control character", ErrInvalidArgument)
		// The shell metacharacters are refused as defence in depth rather than
		// because a shell runs: arguments are passed as an array and no shell is
		// involved, so `;` or a backtick is inert today. The set is complete so
		// that a later change which does reach a shell cannot exploit one that
		// was left out of an incomplete list.
		case c == ' ' || c == '\\' || c == '*' || c == '?' || c == '[' || c == ']' ||
			c == '"' || c == '\'' || c == '`' || c == '$' || c == ';' || c == '&' ||
			c == '|' || c == '<' || c == '>' || c == '(' || c == ')' || c == '{' || c == '}':
			return fmt.Errorf("%w: a revision contains %q", ErrInvalidArgument, string(c))
		}
	}
	return nil
}

// ValidateRepositoryPath checks a client-supplied path inside a repository.
//
// The rules match the addressing document's traversal section: no absolute path,
// no `..` or `.` component, no empty component, no backslash, no control
// character. An empty path is allowed and means the repository root.
//
// The value never becomes a filesystem path — it is a name inside a tree that
// this process addresses — but it is checked anyway, because a value that cannot
// be expressed cannot be misused by a later change.
func ValidateRepositoryPath(path string) error {
	if path == "" {
		return nil
	}
	if len(path) > maxRepositoryPathLength {
		return fmt.Errorf("%w: a path is longer than %d bytes", ErrInvalidArgument, maxRepositoryPathLength)
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("%w: a path must not be absolute", ErrInvalidArgument)
	}
	if strings.HasSuffix(path, "/") {
		return fmt.Errorf("%w: a path must not end with %q", ErrInvalidArgument, "/")
	}
	if strings.HasPrefix(path, "-") {
		return fmt.Errorf("%w: a path must not begin with %q", ErrInvalidArgument, "-")
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c < 0x20 || c == 0x7f:
			return fmt.Errorf("%w: a path contains a control character", ErrInvalidArgument)
		case c == '\\':
			return fmt.Errorf("%w: a path must not contain %q", ErrInvalidArgument, `\`)
		}
	}
	for _, component := range strings.Split(path, "/") {
		switch component {
		case "":
			return fmt.Errorf("%w: a path must not contain an empty component", ErrInvalidArgument)
		case ".", "..":
			return fmt.Errorf("%w: a path must not contain %q", ErrInvalidArgument, component)
		}
	}
	return nil
}

// ValidateCount checks a caller-supplied limit for an inspection command.
func ValidateCount(count, maximum int) error {
	if count <= 0 {
		return fmt.Errorf("%w: a count must be positive", ErrInvalidArgument)
	}
	if count > maximum {
		return fmt.Errorf("%w: a count must be at most %d", ErrInvalidArgument, maximum)
	}
	return nil
}
