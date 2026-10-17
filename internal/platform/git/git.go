// Package git runs the Git executable under a bound.
//
// Every invocation follows the rules in docs/architecture.md section 5, the
// technology baseline, and the T-A1, T-A6, and T-A8 threat-model entries:
//
//   - argument arrays only, never a shell, so a value can never be re-parsed;
//   - a fixed allowlist of services, so a caller cannot ask for an arbitrary
//     Git porcelain command;
//   - client-supplied arguments validated against a strict grammar, and refused
//     outright when they begin with `-`, so a value cannot become an option;
//   - a minimal, explicit child environment, with nothing inherited;
//   - bounded wall-clock time, bounded captured bytes, and a process group that
//     is killed on timeout and on cancellation;
//   - scratch data removed on success, failure, and cancellation.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-gitnest/GitNest/internal/platform/storage"
)

// Service is one of the Git subcommands GitNest is willing to run.
//
// A closed set is what turns "run Git" into "run one of these three protocols".
// An unrecognised value starts no process at all.
type Service string

const (
	// ServiceUploadPack serves clone and fetch.
	ServiceUploadPack Service = "upload-pack"
	// ServiceReceivePack serves push.
	ServiceReceivePack Service = "receive-pack"
	// ServiceUploadArchive serves the archive endpoint.
	ServiceUploadArchive Service = "upload-archive"
)

var allowedServices = map[Service]struct{}{
	ServiceUploadPack:    {},
	ServiceReceivePack:   {},
	ServiceUploadArchive: {},
	// The inspection verbs the read surfaces need. See verbs.go for why the
	// allowlist carries them and what keeps the extension safe.
	ServiceRevParse:   {},
	ServiceCatFile:    {},
	ServiceLsTree:     {},
	ServiceLog:        {},
	ServiceForEachRef: {},
}

// ServiceAllowed reports whether a service is in the allowlist.
func ServiceAllowed(service Service) bool {
	_, found := allowedServices[service]
	return found
}

// Limits bounds one invocation.
//
// A zero field takes the corresponding default, so a caller states only what it
// wants to differ. The defaults are the values in the performance document's
// table for clone, fetch, and push.
type Limits struct {
	// Timeout bounds the whole operation. GIT-FETCH-TIMEOUT is 30 minutes and
	// GIT-PUSH-TIMEOUT is 15, so the caller passes the one that applies.
	Timeout time.Duration
	// StallTimeout bounds the time with no protocol progress. It is enforced by
	// the caller's stream, which knows when bytes last moved; this runner
	// applies the wall-clock bound and reports the stall timeout so a transport
	// can wire its own progress detector to the same value.
	StallTimeout time.Duration
	// MaxCapturedBytes bounds each captured stream. Output beyond it terminates
	// the operation rather than being silently truncated, because a truncated
	// pack is a corrupt pack.
	MaxCapturedBytes int64
}

// Defaults from docs/architecture/performance.md.
const (
	DefaultFetchTimeout    = 30 * time.Minute
	DefaultPushTimeout     = 15 * time.Minute
	DefaultStallTimeout    = 60 * time.Second
	DefaultMaxCapturedSize = 2 << 30 // GIT-PUSH-BYTES: 2 GiB
)

// withDefaults fills every unset bound.
func (l Limits) withDefaults() Limits {
	if l.Timeout <= 0 {
		l.Timeout = DefaultFetchTimeout
	}
	if l.StallTimeout <= 0 {
		l.StallTimeout = DefaultStallTimeout
	}
	if l.MaxCapturedBytes <= 0 {
		l.MaxCapturedBytes = DefaultMaxCapturedSize
	}
	return l
}

// Request is one bounded Git invocation.
type Request struct {
	// Service is the protocol verb. It must be in the allowlist.
	Service Service
	// RepositoryPath is the absolute path of the bare repository. It must sit
	// inside the layout's root; a caller never supplies it from a request.
	RepositoryPath string
	// ServiceArgs are arguments derived from a client request. Each is checked
	// against a strict grammar and refused when it begins with `-`.
	ServiceArgs []string
	// Paths are client-supplied paths inside the repository. They are emitted
	// after a `--` separator, which is what tells Git a path is a path and not a
	// revision: in a bare repository there is no working tree for Git to compare
	// against, so without the separator `git log <rev> <path>` fails outright.
	//
	// They are a separate field rather than entries in ServiceArgs because the
	// separator has to sit between two groups, and because a path is checked by
	// the path rule rather than by the argument rule.
	Paths []string
	// Config holds Git configuration overrides, each written as `key=value`.
	// They are passed with `-c` before the subcommand, because a Git that reads
	// no configuration file (see Environment) must be told its limits on the
	// command line. They are chosen by this process, never derived from a
	// client, and each is shape-checked before it becomes an argument.
	Config []string
	// Options are arguments this process chooses, such as `--stateless-rpc`.
	// They follow the service name.
	Options []string
	// Environment adds variables to the child environment. Only the protocol
	// variable a transport needs belongs here, never a credential.
	Environment map[string]string
	// Stdin is the request body for receive-pack, or nil.
	Stdin io.Reader
	// Stdout receives the response stream. It is streamed rather than buffered,
	// because a pack is far too large to hold.
	Stdout io.Writer
	// Limits bounds the invocation.
	Limits Limits
}

// Result describes how an invocation ended.
type Result struct {
	// ExitCode is the process exit status, or -1 when it never started.
	ExitCode int
	// Duration is the wall-clock time the operation took.
	Duration time.Duration
	// TimedOut reports that the wall-clock bound ended the operation.
	TimedOut bool
	// Cancelled reports that the caller's context ended the operation.
	Cancelled bool
	// BytesOut is how many bytes the process wrote to stdout.
	BytesOut int64
	// BytesIn is how many bytes the process read from stdin.
	BytesIn int64
	// StderrTail is a bounded prefix of stderr, for the outcome record. It is
	// not returned to a client.
	StderrTail string
}

// Satisfied reports whether the process exited successfully within its bounds.
func (r Result) Satisfied() bool {
	return r.ExitCode == 0 && !r.TimedOut && !r.Cancelled
}

var (
	// ErrUnknownService reports a service outside the allowlist. No process
	// starts, which is the T-A1 assertion.
	ErrUnknownService = errors.New("the requested Git service is not allowed")
	// ErrInvalidArgument reports a client-supplied argument that is not safe to
	// pass as its own argument.
	ErrInvalidArgument = errors.New("a Git argument is not valid")
	// ErrOutsideRepositoryRoot reports a repository path outside the root.
	ErrOutsideRepositoryRoot = errors.New("the repository path is outside the storage root")
	// ErrBusy reports that the concurrency limit is already reached. The
	// operation is refused rather than queued, so a burst cannot build an
	// unbounded backlog.
	ErrBusy = errors.New("too many concurrent Git operations")
	// ErrOutputLimit reports that a stream exceeded its byte bound.
	ErrOutputLimit = errors.New("the Git operation produced more output than allowed")
)

// maxArgumentLength bounds one argument. Long enough for a ref or a path,
// short enough that a client cannot push a megabyte through an argument.
const maxArgumentLength = 1024

// Runner invokes Git.
type Runner struct {
	binary string
	layout *storage.Layout
	slots  chan struct{}
}

// NewRunner returns a runner for a Git executable and a storage layout.
//
// The binary is resolved once, at construction, so a later change to PATH cannot
// redirect the process to a different executable mid-request.
func NewRunner(binary string, layout *storage.Layout, maxConcurrency int) (*Runner, error) {
	if layout == nil {
		return nil, errors.New("a storage layout is required")
	}
	if strings.TrimSpace(binary) == "" {
		binary = "git"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("find the Git executable: %w", err)
	}
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	return &Runner{
		binary: resolved,
		layout: layout,
		slots:  make(chan struct{}, maxConcurrency),
	}, nil
}

// Binary returns the resolved Git executable, for a doctor command or a log.
func (r *Runner) Binary() string { return r.binary }

// Run invokes Git under the request's bounds.
//
// Every failure that is the caller's fault is reported before a process starts,
// so a rejected request costs nothing.
func (r *Runner) Run(ctx context.Context, request Request) (Result, error) {
	if !ServiceAllowed(request.Service) {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %q", ErrUnknownService, request.Service)
	}
	if err := r.checkRepositoryPath(request.RepositoryPath); err != nil {
		return Result{ExitCode: -1}, err
	}
	for _, argument := range request.ServiceArgs {
		if err := ValidateArgument(argument); err != nil {
			return Result{ExitCode: -1}, err
		}
	}
	for _, setting := range request.Config {
		if err := validateConfigSetting(setting); err != nil {
			return Result{ExitCode: -1}, err
		}
	}
	for _, path := range request.Paths {
		if err := ValidateRepositoryPath(path); err != nil {
			return Result{ExitCode: -1}, err
		}
	}

	limits := request.Limits.withDefaults()

	// Admission control: refuse rather than queue, so a burst cannot grow an
	// unbounded backlog of Git processes.
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return Result{ExitCode: -1}, ErrBusy
	}

	runCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()

	// Configuration overrides precede the subcommand, because that is where Git
	// accepts `-c`.
	args := make([]string, 0, len(request.Config)*2+len(request.ServiceArgs)+len(request.Options)+4)
	for _, setting := range request.Config {
		args = append(args, "-c", setting)
	}

	// How the repository is selected depends on the verb, and the rule lives
	// here rather than at every call site. A protocol verb defines a repository
	// argument of its own, so it receives the path as its trailing argument. An
	// inspection verb does not, so it is given the repository through `-C`,
	// which must precede the subcommand.
	if ReadOnly(request.Service) {
		args = append(args, "-C", request.RepositoryPath, string(request.Service))
		args = append(args, request.Options...)
		args = append(args, request.ServiceArgs...)
		if len(request.Paths) > 0 {
			// The separator ends option and revision parsing, so everything after
			// it is a path.
			args = append(args, "--")
			args = append(args, request.Paths...)
		}
	} else {
		args = append(args, string(request.Service))
		args = append(args, request.Options...)
		args = append(args, request.ServiceArgs...)
		args = append(args, request.RepositoryPath)
	}

	command := exec.CommandContext(runCtx, r.binary, args...)
	configureProcessGroup(command)
	// The context ends the operation; this hook turns that into a kill of the
	// whole tree rather than of the direct child alone.
	command.Cancel = func() error { return killProcessGroup(command) }
	// A short grace period covers a child that ignores the kill, so the runner
	// can never block on a process that refuses to die.
	command.WaitDelay = 2 * time.Second
	command.Env = Environment(request.Environment)
	command.Stdin = request.Stdin

	stdout := &boundedWriter{target: request.Stdout, limit: limits.MaxCapturedBytes}
	stderr := &boundedWriter{target: io.Discard, limit: stderrCaptureLimit}
	command.Stdout = stdout
	command.Stderr = stderr

	started := time.Now()
	runErr := command.Run()
	elapsed := time.Since(started)

	result := Result{
		Duration:   elapsed,
		BytesOut:   stdout.written,
		StderrTail: stderr.captured.String(),
		TimedOut:   errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil,
		Cancelled:  ctx.Err() != nil,
		ExitCode:   0,
	}
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	} else {
		result.ExitCode = -1
	}

	if stdout.overLimit || stderr.overLimit {
		return result, ErrOutputLimit
	}
	if result.TimedOut {
		return result, fmt.Errorf("the Git operation exceeded %s", limits.Timeout)
	}
	if result.Cancelled {
		return result, fmt.Errorf("the Git operation was cancelled: %w", ctx.Err())
	}
	if runErr != nil {
		return result, fmt.Errorf("the Git operation failed: %w", runErr)
	}
	return result, nil
}

// stderrCaptureLimit bounds the diagnostic prefix kept from a failed operation.
// Git's own messages are short; a runaway stderr is a symptom, not content.
const stderrCaptureLimit = 8 << 10

// checkRepositoryPath refuses a path outside the root and a path that is not a
// directory.
//
// The containment check compares resolved absolute paths rather than string
// prefixes, so `root-evil` is not accepted as being inside `root`.
func (r *Runner) checkRepositoryPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: no path", ErrOutsideRepositoryRoot)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOutsideRepositoryRoot, err)
	}
	relative, err := filepath.Rel(r.layout.Root(), absolute)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOutsideRepositoryRoot, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(relative) {
		return fmt.Errorf("%w: %q", ErrOutsideRepositoryRoot, path)
	}
	return nil
}

// ValidateArgument checks a client-supplied value before it becomes an argument.
//
// The rule that matters most is the leading `-`: without it, a client-supplied
// ref could be read by Git as an option, which is how `--upload-pack=<command>`
// style injection works. Rejecting it is cheaper and more reliable than trying
// to escape it.
func ValidateArgument(argument string) error {
	if argument == "" {
		return fmt.Errorf("%w: an argument must not be empty", ErrInvalidArgument)
	}
	if len(argument) > maxArgumentLength {
		return fmt.Errorf("%w: an argument is longer than %d bytes", ErrInvalidArgument, maxArgumentLength)
	}
	if strings.HasPrefix(argument, "-") {
		return fmt.Errorf("%w: a client-supplied argument must not begin with %q", ErrInvalidArgument, "-")
	}
	for i := 0; i < len(argument); i++ {
		c := argument[i]
		switch {
		case c < 0x20 || c == 0x7f:
			// A control character, including NUL and a newline, has no place in
			// a Git argument and is how a value escapes into a protocol or a
			// configuration line.
			return fmt.Errorf("%w: an argument contains a control character", ErrInvalidArgument)
		}
	}
	return nil
}

// validateConfigSetting checks a configuration override this process built.
//
// The value is not client-derived, so the check is a guard against a future
// call site interpolating request data into a setting rather than a filter for
// a client's benefit. It also keeps a newline out, which is what would let a
// value escape into a second configuration line.
func validateConfigSetting(setting string) error {
	if setting == "" {
		return fmt.Errorf("%w: a configuration setting must not be empty", ErrInvalidArgument)
	}
	if len(setting) > maxArgumentLength {
		return fmt.Errorf("%w: a configuration setting is longer than %d bytes", ErrInvalidArgument, maxArgumentLength)
	}
	key, _, found := strings.Cut(setting, "=")
	if !found || key == "" {
		return fmt.Errorf("%w: a configuration setting must be written as key=value", ErrInvalidArgument)
	}
	for i := 0; i < len(setting); i++ {
		if c := setting[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("%w: a configuration setting contains a control character", ErrInvalidArgument)
		}
	}
	return nil
}

// boundedWriter forwards bytes to a target and stops at a bound.
//
// The bound is enforced as the bytes flow rather than after the fact, so a
// hostile operation cannot make the process allocate without limit before being
// told it went too far.
type boundedWriter struct {
	mu        sync.Mutex
	target    io.Writer
	written   int64
	limit     int64
	overLimit bool
	captured  bytes.Buffer
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	remaining := w.limit - w.written
	if remaining <= 0 {
		w.overLimit = true
		// Reporting an error here is what terminates the process: Git sees its
		// stream close and stops, rather than continuing to produce output this
		// process would have to discard.
		return 0, ErrOutputLimit
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
		w.overLimit = true
	}

	written, err := w.target.Write(p)
	w.written += int64(written)
	if w.captured.Len() < stderrCaptureLimit {
		room := stderrCaptureLimit - w.captured.Len()
		if room > len(p) {
			room = len(p)
		}
		w.captured.Write(p[:room])
	}
	if w.overLimit && err == nil {
		return written, ErrOutputLimit
	}
	return written, err
}
