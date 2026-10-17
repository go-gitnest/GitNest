package git

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	platformgit "github.com/go-gitnest/GitNest/internal/platform/git"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// Content types the receive-pack half of the protocol defines.
const (
	receiveAdvertisementContentType = "application/x-git-receive-pack-advertisement"
	receiveRequestContentType       = "application/x-git-receive-pack-request"
	receiveResultContentType        = "application/x-git-receive-pack-result"
)

// serviceLineFor renders the service announcement pkt-line for a service.
func serviceLineFor(service string) []byte {
	return []byte("# service=" + service + "\n")
}

// Push limits, from the performance document's table.
const (
	// maxPushBytes is GIT-PUSH-BYTES: pack bytes accepted per push. The check
	// runs on `Content-Length` and again on the streamed bytes, so a client that
	// lies about its length or streams without one is still stopped.
	maxPushBytes = 2 << 30 // 2 GiB
	// maxObjectBytes is GIT-OBJECT-BYTES: the largest single object accepted.
	// Git enforces it through `receive.maxInputSize`.
	maxObjectBytes = 2 << 30 // 2 GiB
	// maxRefsPerPush is GIT-REFS: refs changed per push. The ref transaction is
	// all-or-nothing, so refusing a push is enough to leave every ref alone.
	maxRefsPerPush = 100
	// maxCommandSectionBytes bounds the update-command section this process
	// reads before it hands the stream to Git. A well-formed section is a few
	// hundred bytes per ref; the bound stops a client from making this process
	// buffer without limit before Git sees anything.
	maxCommandSectionBytes = 64 << 10
)

// errRequestTooLarge reports a push whose body exceeded the accepted size.
//
// It is a sentinel so the handler can answer `413` rather than a generic server
// error: the client's request was simply too big, and saying so is useful.
var errRequestTooLarge = errors.New("the push exceeded the accepted size")

// errCommandSectionInvalid reports an update section this process refused.
var errCommandSectionInvalid = errors.New("the push was refused")

// refCommand is one ref update a push asks for.
type refCommand struct {
	oldValue string
	newValue string
	ref      string
}

// deleted reports whether the command removes the ref.
func (c refCommand) deleted() bool {
	return isZeroObjectID(c.newValue)
}

// isZeroObjectID reports whether an object identifier is the all-zero value Git
// uses for "absent", in either the SHA-1 or the SHA-256 width.
func isZeroObjectID(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] != '0' {
			return false
		}
	}
	return len(value) == 40 || len(value) == 64
}

// validObjectID reports whether a value is a well-formed object identifier.
func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// receivePack handles a push.
//
// The order of operations is what protects the instance: the caller is
// authorized before anything else, the request size is checked against the
// declared length before a byte is read, and the update commands are validated
// before the stream reaches Git. A refusal at any of those points starts no
// process and therefore changes no ref.
func (h *Handler) receivePack(w http.ResponseWriter, r *http.Request) {
	if !requestContentTypeIsValidFor(r.Header.Get("Content-Type"), receiveRequestContentType) {
		writeProtocolError(w, http.StatusUnsupportedMediaType, "the request is not a Git push")
		return
	}

	// The declared length is checked first, so an oversized push is refused
	// before any object could be written.
	if r.ContentLength > h.pushByteLimit {
		writeProtocolError(w, http.StatusRequestEntityTooLarge, "the push is too large")
		return
	}

	found, path, ok := h.authorize(w, r, repository.LevelWrite)
	if !ok {
		return
	}

	// The update commands are read and validated here, before Git sees them.
	// The bytes read are replayed to the child, so the stream Git receives is
	// exactly the one the client sent.
	//
	// The body is bounded by this layer rather than by http.MaxBytesReader,
	// because the bound has to surface as a distinguishable error: a push that
	// ran past it is the client's fault and deserves a 413, while a generic
	// read failure would be reported as a server error.
	body := &boundedBody{source: r.Body, remaining: h.pushByteLimit}
	commands, replay, err := readCommandSection(body)
	if err != nil {
		h.writePushRefusal(w, found, err)
		h.recordPushOutcome(found, nil, pushOutcome{Refused: err.Error()})
		return
	}

	// A push beyond the ref bound is refused rather than truncated: the ref
	// transaction is all-or-nothing, so a partial push is not a meaningful
	// outcome.
	if len(commands) > maxRefsPerPush {
		writeProtocolError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("a push may change at most %d refs", maxRefsPerPush))
		return
	}
	for _, command := range commands {
		if err := validateRefCommand(command); err != nil {
			writeProtocolError(w, http.StatusBadRequest, "the push contains an update this server does not accept")
			h.recordPushOutcome(found, commands, pushOutcome{Refused: err.Error()})
			return
		}
	}

	// The report Git writes after a push is small, so it is buffered: that way a
	// failure before any output can still be reported as a status code rather
	// than as a truncated success.
	var report strings.Builder
	result, err := h.runner.Run(r.Context(), platformRequestForPush(path, r, replay, body, &report))
	if err != nil {
		h.writeGitError(w, found, result, err)
		h.recordPushOutcome(found, commands, pushOutcome{
			ExitCode: result.ExitCode, Failed: err.Error(), TimedOut: result.TimedOut,
			Cancelled: result.Cancelled, Duration: result.Duration,
		})
		return
	}

	w.Header().Set("Content-Type", receiveResultContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, report.String())

	h.recordPushOutcome(found, commands, pushOutcome{
		ExitCode: result.ExitCode, Duration: result.Duration,
	})
}

// pushOutcome is what gets recorded about one push.
type pushOutcome struct {
	ExitCode  int
	Duration  time.Duration
	TimedOut  bool
	Cancelled bool
	Failed    string
	Refused   string
}

// recordPushOutcome writes the operator-facing record of one push.
//
// It carries the repository, the refs the client asked to change, whether each
// was a creation or a deletion, and how the operation ended. It deliberately
// carries no object content, no credential, and no client address.
//
// The durable record of a ref update — an activity entry, a push event, and the
// job that refreshes dependent state — belongs to later phases; the structured
// log is what an operator can act on today.
func (h *Handler) recordPushOutcome(found repository.Repository, commands []refCommand, outcome pushOutcome) {
	refs := make([]any, 0, len(commands))
	for _, command := range commands {
		refs = append(refs, command.ref)
	}
	change := make([]any, 0, len(commands))
	for _, command := range commands {
		switch {
		case command.deleted():
			change = append(change, "deleted")
		case isZeroObjectID(command.oldValue):
			change = append(change, "created")
		default:
			change = append(change, "updated")
		}
	}

	h.log("git transport: push outcome",
		"repository_id", found.ID,
		"ref_count", len(commands),
		"refs", strings.Join(toStrings(refs), ","),
		"changes", strings.Join(toStrings(change), ","),
		"exit_code", outcome.ExitCode,
		"duration_ms", float64(outcome.Duration.Microseconds())/1000,
		"timed_out", outcome.TimedOut,
		"cancelled", outcome.Cancelled,
		"failed", outcome.Failed,
		"refused", outcome.Refused,
	)
}

func toStrings(values []any) []string {
	converted := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			converted = append(converted, text)
		}
	}
	return converted
}

// writePushRefusal reports a refused update section.
//
// A body that ran past the accepted size is a `413`; anything else is a `400`,
// because the client sent something this server will not act on.
func (h *Handler) writePushRefusal(w http.ResponseWriter, found repository.Repository, err error) {
	switch {
	case errors.Is(err, errRequestTooLarge):
		writeProtocolError(w, http.StatusRequestEntityTooLarge, "the push is too large")
	default:
		h.log("git transport: refused a push before starting Git",
			"repository_id", found.ID, "error", err)
		writeProtocolError(w, http.StatusBadRequest, "the push could not be read")
	}
}

// boundedBody bounds how many bytes a push request may carry.
//
// The bound is enforced as the bytes flow, so the process consuming the stream
// stops rather than the server having to hold an oversized request to find out
// how big it was. It reports errRequestTooLarge so the caller can answer `413`
// instead of a generic server error.
type boundedBody struct {
	source    io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, errRequestTooLarge
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	read, err := b.source.Read(p)
	b.remaining -= int64(read)
	if err == io.EOF && b.remaining <= 0 {
		// The stream ended exactly at the bound, which is allowed.
		return read, io.EOF
	}
	return read, err
}

func (b *boundedBody) Close() error {
	return b.source.Close()
}

// readCommandSection reads the update-command section of a push.
//
// It returns the commands and the exact bytes it consumed, so the caller can
// replay them to Git: the child must receive the request byte for byte, and
// reading ahead is the only way to validate the commands before the process
// starts. The pack stream that follows is not read here.
func readCommandSection(body io.Reader) ([]refCommand, []byte, error) {
	consumed := make([]byte, 0, 256)
	scratch := make([]byte, 4)

	var commands []refCommand
	for {
		if len(consumed) > maxCommandSectionBytes {
			return nil, consumed, fmt.Errorf("%w: the update section is larger than %d bytes",
				errCommandSectionInvalid, maxCommandSectionBytes)
		}

		if _, err := io.ReadFull(body, scratch); err != nil {
			return nil, consumed, fmt.Errorf("%w: %v", errCommandSectionInvalid, err)
		}
		length, err := strconv.ParseUint(string(scratch), 16, 32)
		if err != nil {
			return nil, consumed, fmt.Errorf("%w: the update section is malformed", errCommandSectionInvalid)
		}
		consumed = append(consumed, scratch...)

		switch length {
		case 0:
			// A flush packet ends the command section. The pack that follows is
			// not read here: the child consumes it through the bounded reader,
			// which is what keeps the total inside the push bound.
			return commands, consumed, nil
		case 1, 2:
			// A delimiter or response-end packet has no place in a push.
			return nil, consumed, fmt.Errorf("%w: unexpected packet in the update section", errCommandSectionInvalid)
		}
		if length < 4 {
			return nil, consumed, fmt.Errorf("%w: the update section is malformed", errCommandSectionInvalid)
		}

		payloadLength := int(length) - 4
		if len(consumed)+payloadLength > maxCommandSectionBytes {
			return nil, consumed, fmt.Errorf("%w: the update section is larger than %d bytes",
				errCommandSectionInvalid, maxCommandSectionBytes)
		}
		payload := make([]byte, payloadLength)
		if _, err := io.ReadFull(body, payload); err != nil {
			return nil, consumed, fmt.Errorf("%w: %v", errCommandSectionInvalid, err)
		}
		consumed = append(consumed, payload...)

		command, err := parseRefCommand(payload)
		if err != nil {
			return nil, consumed, err
		}
		commands = append(commands, command)
	}
}

// parseRefCommand parses one `<old> <new> <ref>` line.
//
// The first command may carry NUL-separated capabilities after the ref name,
// which are ignored: this process does not use them, and Git reads them from the
// stream it receives.
func parseRefCommand(payload []byte) (refCommand, error) {
	line := string(payload)
	if index := strings.IndexByte(line, '\x00'); index >= 0 {
		line = line[:index]
	}
	line = strings.TrimRight(line, "\n")

	fields := strings.SplitN(line, " ", 3)
	if len(fields) != 3 {
		return refCommand{}, fmt.Errorf("%w: an update command is malformed", errCommandSectionInvalid)
	}
	command := refCommand{oldValue: fields[0], newValue: fields[1], ref: fields[2]}
	if !validObjectID(command.oldValue) || !validObjectID(command.newValue) {
		return refCommand{}, fmt.Errorf("%w: an update command names a malformed object", errCommandSectionInvalid)
	}
	return command, nil
}

// validateRefCommand checks what the client asks to change.
//
// Two rules are enforced here, and both are phase-1 policy stated in
// docs/architecture/urls-and-identifiers.md section 7.4: a ref name must be a
// valid ref, and it must live in a namespace this phase serves. Push options and
// other capabilities are not consulted, because nothing in this phase acts on
// them.
func validateRefCommand(command refCommand) error {
	if err := validateGitRefName(command.ref); err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(command.ref, "refs/heads/"),
		strings.HasPrefix(command.ref, "refs/tags/"):
		return nil
	default:
		// The remaining `refs/` namespaces are reserved for the server, and a
		// client that pushes into one would be writing state the server owns.
		return fmt.Errorf("%w: %q is not a namespace this server accepts",
			errCommandSectionInvalid, command.ref)
	}
}

// validateGitRefName applies the Git ref rules to a full ref name.
//
// It cannot reuse repository.ValidateBranchName, which validates the short name
// of a branch: a full ref name has a different shape and a different set of
// reserved names, so it carries its own check.
func validateGitRefName(ref string) error {
	if ref == "" || len(ref) > 1024 {
		return fmt.Errorf("%w: an update names an invalid ref", errCommandSectionInvalid)
	}
	if !strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("%w: an update names a ref outside refs/", errCommandSectionInvalid)
	}
	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
		return fmt.Errorf("%w: an update names an invalid ref", errCommandSectionInvalid)
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return fmt.Errorf("%w: an update names an invalid ref", errCommandSectionInvalid)
	}
	if strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock") {
		return fmt.Errorf("%w: an update names an invalid ref", errCommandSectionInvalid)
	}
	if ref == "@" {
		return fmt.Errorf("%w: an update names an invalid ref", errCommandSectionInvalid)
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		switch {
		case c < 0x20 || c == 0x7f:
			return fmt.Errorf("%w: an update names a ref with a control character", errCommandSectionInvalid)
		case c == ' ' || c == '~' || c == '^' || c == ':' || c == '?' ||
			c == '*' || c == '[' || c == '\\':
			return fmt.Errorf("%w: an update names an invalid ref", errCommandSectionInvalid)
		}
	}
	for _, component := range strings.Split(ref, "/") {
		if component == "" || strings.HasPrefix(component, ".") {
			return fmt.Errorf("%w: an update names an invalid ref", errCommandSectionInvalid)
		}
	}
	return nil
}

// requestContentTypeIsValidFor reports whether a POST carries the content type
// the protocol defines for its service.
func requestContentTypeIsValidFor(contentType, expected string) bool {
	trimmed := strings.TrimSpace(contentType)
	if trimmed == "" {
		// A client is not required to send the type, and refusing an absent one
		// would break a working client.
		return true
	}
	base, _, _ := strings.Cut(trimmed, ";")
	return strings.TrimSpace(base) == expected
}

// platformRequestForPush builds the runner request for one push.
//
// The command section this layer already read is replayed ahead of the rest of
// the body, and the remainder arrives through the bounded reader, so Git receives
// the request byte for byte while nothing downstream of this point can read past
// the push bound.
func platformRequestForPush(path string, r *http.Request, replay []byte, rest io.Reader, report *strings.Builder) platformgit.Request {
	return platformgit.Request{
		Service:        platformgit.ServiceReceivePack,
		RepositoryPath: path,
		Options:        []string{"--stateless-rpc"},
		Config: []string{
			// GIT-OBJECT-BYTES: the largest single object Git will accept. It
			// overrides a repository-level setting on purpose: the instance's
			// documented bound is what protects the whole deployment, and an
			// operator-raised value would defeat it.
			"receive.maxInputSize=" + strconv.FormatInt(maxObjectBytes, 10),
		},
		Environment: protocolEnvironment(r),
		Stdin:       io.MultiReader(strings.NewReader(string(replay)), rest),
		Stdout:      report,
		Limits:      platformgit.Limits{Timeout: platformgit.DefaultPushTimeout},
	}
}
