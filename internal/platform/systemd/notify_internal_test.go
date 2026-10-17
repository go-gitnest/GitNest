package systemd

import (
	"strings"
	"testing"
)

// The encoding properties are tested here rather than only through a socket, because
// they are properties of the string — and because Windows cannot create a unix datagram
// socket, so a socket-only test would leave them unchecked on the platform this project
// is developed on.
//
// The wire format is systemd's: newline-delimited `KEY=value` pairs in one datagram. The
// failures that matter are a value forging a field, a control character reaching the
// journal, and an unbounded field.

func TestBuildMessageJoinsFieldsWithNewlines(t *testing.T) {
	got := buildMessage("READY=1", "STATUS=serving")
	if got != "READY=1\nSTATUS=serving" {
		t.Errorf("buildMessage = %q", got)
	}
}

func TestBuildMessageHasNoTrailingNewline(t *testing.T) {
	// A trailing newline is read as an empty field, which is not the same notification.
	if got := buildMessage("READY=1"); strings.HasSuffix(got, "\n") {
		t.Errorf("buildMessage = %q, want no trailing newline", got)
	}
}

func TestASingleFieldIsTheWholeMessage(t *testing.T) {
	if got := buildMessage("STOPPING=1"); got != "STOPPING=1" {
		t.Errorf("buildMessage = %q", got)
	}
}

// TestSanitizeRemovesNewlines is the injection defence: a newline in a value would be
// read as the start of another field, so a message that happened to contain one could
// forge `READY=1`, or split itself so the field it belonged to never arrived.
func TestSanitizeRemovesNewlines(t *testing.T) {
	for _, input := range []string{
		"a\nREADY=1",
		"a\rREADY=1",
		"a\r\nREADY=1",
		"a\x00READY=1",
	} {
		got := sanitize(input)
		if strings.ContainsAny(got, "\n\r\x00") {
			t.Errorf("sanitize(%q) = %q, which still carries a line break", input, got)
		}
		// The text survives, so the reason is still readable.
		if !strings.HasPrefix(got, "a") || !strings.Contains(got, "READY=1") {
			t.Errorf("sanitize(%q) = %q, which lost the text", input, got)
		}
	}
}

// TestSanitizeDropsOtherControlCharacters covers the characters that are not line breaks
// but would still corrupt a journal entry or a terminal: an escape sequence, a bell, a
// backspace.
func TestSanitizeDropsOtherControlCharacters(t *testing.T) {
	got := sanitize("a\x1bb\x07c\x08d\x7fe")
	if got != "abcde" {
		t.Errorf("sanitize = %q, want the control characters dropped", got)
	}
}

// TestSanitizeBoundsTheField keeps one pathological message from dominating the journal
// entry that carries it, and from making the datagram larger than a socket buffer.
func TestSanitizeBoundsTheField(t *testing.T) {
	long := strings.Repeat("x", 10000)
	got := sanitize(long)
	if len(got) > 512 {
		t.Errorf("sanitize kept %d bytes, want a bounded field", len(got))
	}
	if got == "" {
		t.Error("sanitize dropped the whole value rather than truncating it")
	}
}

// TestSanitizeKeepsOrdinaryText is the other half: the sanitisation must not mangle the
// message an operator is going to read.
func TestSanitizeKeepsOrdinaryText(t *testing.T) {
	const text = "draining in-flight requests and Git operations (0/5)"
	if got := sanitize(text); got != text {
		t.Errorf("sanitize(%q) = %q, want it unchanged", text, got)
	}
}

// TestSanitizeKeepsNonASCII covers a message containing a name or a path outside ASCII,
// which must not be silently reduced to nothing.
func TestSanitizeKeepsNonASCII(t *testing.T) {
	const text = "the repository ünïcödé is unavailable"
	if got := sanitize(text); got != text {
		t.Errorf("sanitize(%q) = %q, want it unchanged", text, got)
	}
}

// TestStatusAndStoppingProduceOneFieldPerLine is the shape assertion: a message with two
// values must be two lines, and a message with one must be one. The distinction matters
// because systemd reads every line as a field.
func TestStatusAndStoppingProduceOneFieldPerLine(t *testing.T) {
	// Stopping with a reason is the only two-field message.
	withReason := "STOPPING=1\nSTATUS=" + sanitize("a reason")
	if got := buildMessage("STOPPING=1", "STATUS="+sanitize("a reason")); got != withReason {
		t.Errorf("the stopping message = %q", got)
	}
}
