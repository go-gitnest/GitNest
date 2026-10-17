package systemd_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/platform/systemd"
)

// These tests drive the notification over a **real unix datagram socket**, because the
// protocol is the point: a fake would assert that the code builds a string, while the
// failures that matter are an address systemd passes in a form this code mishandles and
// a message whose fields run together.

// listeningSocket returns a bound unix datagram socket and its path.
//
// It skips where the platform cannot create one, rather than failing: the notification
// is a no-op outside systemd by design, so a platform without unix sockets is not a
// defect.
func listeningSocket(t *testing.T) (*net.UnixConn, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.sock")

	address := &net.UnixAddr{Name: path, Net: "unixgram"}
	connection, err := net.ListenUnixgram("unixgram", address)
	if err != nil {
		t.Skipf("this platform cannot create a unix datagram socket: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection, path
}

// receive reads one datagram with a deadline, so a missing message fails rather than
// hangs.
func receive(t *testing.T, connection *net.UnixConn) string {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	buffer := make([]byte, 4096)
	read, err := connection.Read(buffer)
	if err != nil {
		t.Fatalf("read the notification: %v", err)
	}
	return string(buffer[:read])
}

func TestReadySendsTheDatagramSystemdWaitsFor(t *testing.T) {
	connection, path := listeningSocket(t)
	t.Setenv("NOTIFY_SOCKET", path)

	if err := systemd.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	// The exact bytes matter: systemd parses newline-delimited `KEY=value` pairs, and
	// anything else is ignored — which would leave the unit waiting until it times out.
	if got := receive(t, connection); got != "READY=1" {
		t.Errorf("the datagram is %q, want READY=1", got)
	}
}

func TestStoppingCarriesTheReason(t *testing.T) {
	connection, path := listeningSocket(t)
	t.Setenv("NOTIFY_SOCKET", path)

	if err := systemd.Stopping("draining in-flight requests"); err != nil {
		t.Fatalf("Stopping: %v", err)
	}
	got := receive(t, connection)
	if !strings.HasPrefix(got, "STOPPING=1") {
		t.Errorf("the datagram is %q, want it to start with STOPPING=1", got)
	}
	if !strings.Contains(got, "STATUS=draining in-flight requests") {
		t.Errorf("the datagram does not carry the reason: %q", got)
	}
}

// TestAValueCannotInjectAnotherField is the security property of the protocol: a
// newline inside a value would be read as the start of the next field, which is how a
// message that happened to contain one could suppress the notification it was attached
// to or forge a field.
func TestAValueCannotInjectAnotherField(t *testing.T) {
	connection, path := listeningSocket(t)
	t.Setenv("NOTIFY_SOCKET", path)

	injected := "a reason\nREADY=1\nSTATUS=forged"
	if err := systemd.Status(injected); err != nil {
		t.Fatalf("Status: %v", err)
	}
	got := receive(t, connection)

	if strings.Count(got, "\n") != 0 {
		t.Errorf("the datagram contains a newline, so a value forged another field: %q", got)
	}
	if strings.Contains(got, "READY=1") {
		t.Errorf("an injected field survived: %q", got)
	}
	if !strings.HasPrefix(got, "STATUS=") {
		t.Errorf("the datagram is %q, want a single STATUS field", got)
	}
}

// TestControlCharactersAreDropped covers the same rule for the characters that are not
// newlines but would still corrupt a journal line.
func TestControlCharactersAreDropped(t *testing.T) {
	connection, path := listeningSocket(t)
	t.Setenv("NOTIFY_SOCKET", path)

	if err := systemd.Status("a\x00reason\x07with\x1bcontrols"); err != nil {
		t.Fatalf("Status: %v", err)
	}
	got := receive(t, connection)
	for _, forbidden := range []string{"\x00", "\x07", "\x1b"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the datagram carries the control character %q: %q", forbidden, got)
		}
	}
}

// TestALongValueIsBounded keeps one pathological message from dominating the journal
// entry that carries it.
func TestALongValueIsBounded(t *testing.T) {
	connection, path := listeningSocket(t)
	t.Setenv("NOTIFY_SOCKET", path)

	if err := systemd.Status(strings.Repeat("x", 10000)); err != nil {
		t.Fatalf("Status: %v", err)
	}
	got := receive(t, connection)
	if len(got) > 1024 {
		t.Errorf("the datagram is %d bytes, want a bounded field", len(got))
	}
}

// TestTheAbstractNamespaceIsAddressedCorrectly covers the one address form systemd
// passes that a naive dial mishandles: a name beginning with `@` denotes the abstract
// namespace, which is addressed with a leading NUL rather than the `@` itself.
func TestTheAbstractNamespaceIsAddressedCorrectly(t *testing.T) {
	// An abstract socket is a Linux facility; elsewhere the listener cannot be created
	// and the test says so rather than asserting a platform behaviour it cannot reach.
	name := "\x00gitnest-notify-test"
	address := &net.UnixAddr{Name: name, Net: "unixgram"}
	connection, err := net.ListenUnixgram("unixgram", address)
	if err != nil {
		t.Skipf("abstract unix sockets are not available here: %v", err)
	}
	defer func() { _ = connection.Close() }()

	// systemd expresses the same address with a leading `@`.
	t.Setenv("NOTIFY_SOCKET", "@gitnest-notify-test")
	if err := systemd.Ready(); err != nil {
		t.Fatalf("Ready against an abstract socket: %v", err)
	}
	if got := receive(t, connection); got != "READY=1" {
		t.Errorf("the datagram is %q, want READY=1", got)
	}
}

// TestNotificationIsANoOpWithoutSystemd is the property that lets every caller invoke
// this unconditionally: outside systemd there is no socket, and that is not an error.
//
// The binary runs under a shell, in a container without systemd, and in tests, and none
// of those should have to branch.
func TestNotificationIsANoOpWithoutSystemd(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")

	if systemd.Enabled() {
		t.Error("Enabled reports true with no socket")
	}
	for name, send := range map[string]func() error{
		"Ready":    systemd.Ready,
		"Stopping": func() error { return systemd.Stopping("a reason") },
		"Status":   func() error { return systemd.Status("a status") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := send(); err != nil {
				t.Errorf("%s returned an error with no socket: %v", name, err)
			}
		})
	}
}

// TestAMissingSocketIsReported covers the misconfiguration: systemd set the variable
// and the socket is gone, which means the unit's timeout will fire. The caller decides
// whether that is fatal; this layer reports it.
func TestAMissingSocketIsReported(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "does-not-exist.sock"))

	if err := systemd.Ready(); err == nil {
		t.Error("a missing socket was reported as success, so a unit that cannot notify looks healthy")
	}
}

// TestEnabledReportsTheEnvironment covers the predicate a caller uses to decide whether
// a failure is worth logging loudly.
func TestEnabledReportsTheEnvironment(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "/run/systemd/notify")
	if !systemd.Enabled() {
		t.Error("Enabled reports false with a socket configured")
	}
	t.Setenv("NOTIFY_SOCKET", "   ")
	if systemd.Enabled() {
		t.Error("Enabled reports true for a blank socket")
	}
}

// TestReadyIsCalledOnceInTheRunMode is a structural check on the wiring: the
// notification belongs after the readiness check, not before, and a source-level
// assertion is what keeps a future edit from moving it.
//
// It is deliberately narrow — it asserts the order of two calls in one file rather than
// parsing Go — because the property is simple and a heavier check would be brittle.
func TestReadyIsCalledOnceInTheRunMode(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "app", "app.go"))
	if err != nil {
		t.Fatalf("read the run mode: %v", err)
	}
	content := string(source)

	readinessIndex := strings.Index(content, "db.Ready(")
	notifyIndex := strings.Index(content, "systemd.Ready()")
	if readinessIndex < 0 {
		t.Fatal("the run mode does not check database readiness")
	}
	if notifyIndex < 0 {
		t.Fatal("the run mode never reports readiness to systemd, so a Type=notify unit would time out")
	}
	if notifyIndex < readinessIndex {
		t.Error("readiness is reported to systemd before the database is confirmed reachable, " +
			"so a supervisor would route traffic to an instance that cannot serve")
	}
	if count := strings.Count(content, "systemd.Ready()"); count != 1 {
		t.Errorf("systemd.Ready() appears %d times, want exactly one call site", count)
	}
}
