// Package systemd reports service readiness to systemd.
//
// It exists because the deployment example in docs/architecture/packaging.md
// section 10 declares `Type=notify`, and a unit with that type makes systemd **wait**
// for a `READY=1` datagram before it considers the service started. A binary that
// never sends one is killed at `TimeoutStartSec` and reported as failed — so the
// documented unit does not work without this, and the readiness notification is not
// decoration: it is what makes readiness gating real.
//
// # Why readiness rather than "the process started"
//
// `Type=notify` is worth having precisely because the notification can be delayed. A
// process that is listening but whose database is unreachable must **not** report
// ready, or systemd (and a supervisor above it) would route traffic to it. So the
// signal is sent when the instance has confirmed it can serve, which is the same
// condition `/api/ready` answers.
//
// # Absent by design outside systemd
//
// Every function here is a no-op when `NOTIFY_SOCKET` is not set, which is the case
// outside systemd, in a container without it, and on Windows. Nothing has to branch on
// the platform to call it.
package systemd

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// notifySocketVariable is the environment variable systemd sets for the unit.
const notifySocketVariable = "NOTIFY_SOCKET"

// sendTimeout bounds the datagram write. The socket is a local unix socket, so a write
// either lands immediately or the receiver is gone; a bound exists so a pathological
// socket cannot hold startup.
const sendTimeout = 2 * time.Second

// Enabled reports whether the process was started by systemd with notification
// enabled.
//
// Absence is the normal case rather than an error: the binary runs under a shell, in a
// container without systemd, and in tests.
func Enabled() bool {
	return strings.TrimSpace(os.Getenv(notifySocketVariable)) != ""
}

// Ready tells systemd the service can serve.
//
// It is called once, after the instance has confirmed it is able to answer requests —
// not when the listener opens. Calling it earlier would defeat the type.
func Ready() error {
	return Notify("READY=1")
}

// Stopping tells systemd the service has begun a bounded shutdown.
//
// It is advisory: systemd already sent the signal and is waiting for the process to
// exit. The message is what a human sees in `systemctl status` during a slow drain,
// which is exactly when an operator is watching.
func Stopping(reason string) error {
	if reason == "" {
		return Notify("STOPPING=1")
	}
	return Notify(buildMessage("STOPPING=1", "STATUS="+sanitize(reason)))
}

// Status reports a one-line human-readable state, shown by `systemctl status`.
func Status(text string) error {
	return Notify(buildMessage("STATUS=" + sanitize(text)))
}

// buildMessage joins fields into one notification.
//
// It is separate from Notify so the encoding is testable without a socket: the
// properties that matter here — that a value cannot forge a field, and that no control
// character reaches the journal — are properties of the string, and a platform that
// cannot create a unix datagram socket should still be able to check them.
//
// The protocol is newline-delimited `KEY=value` pairs. It is **not** terminated: systemd
// reads one datagram as one notification, and a trailing newline would be read as an
// empty field.
func buildMessage(fields ...string) string {
	return strings.Join(fields, "\n")
}

// Notify sends one notification.
//
// The socket is a unix datagram socket whose address systemd passes in
// `NOTIFY_SOCKET`. An address beginning with `@` denotes the abstract namespace, which
// is addressed with a leading NUL — a detail `net.Dial("unix", ...)` handles only if
// the name is converted, which is what the check below does.
//
// It is a no-op, not an error, when systemd is not supervising this process. A caller
// should not have to know whether it is running under systemd to start correctly.
func Notify(message string) error {
	address := strings.TrimSpace(os.Getenv(notifySocketVariable))
	if address == "" {
		return nil
	}

	// The abstract namespace is written with a leading NUL rather than the `@` systemd
	// uses in the variable.
	dialAddress := address
	if strings.HasPrefix(dialAddress, "@") {
		dialAddress = "\x00" + dialAddress[1:]
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unixgram", dialAddress)
	if err != nil {
		return fmt.Errorf("connect to the systemd notification socket: %w", err)
	}
	defer func() { _ = connection.Close() }()

	if _, err := connection.Write([]byte(message)); err != nil {
		return fmt.Errorf("send the systemd notification: %w", err)
	}
	return nil
}

// sanitize makes a value safe for a notification field.
//
// The protocol is newline-delimited `KEY=value` pairs, so a newline inside a value
// would be read as the start of another field — which is how an error message that
// happened to contain one could suppress the notification it was attached to. Control
// characters are dropped for the same reason.
func sanitize(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range value {
		if r == '\n' || r == '\r' || r == 0 {
			builder.WriteRune(' ')
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		builder.WriteRune(r)
	}
	// The field is bounded so one long line cannot dominate the journal entry.
	const limit = 512
	text := builder.String()
	if len(text) > limit {
		text = text[:limit]
	}
	return text
}
