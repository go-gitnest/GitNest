package logging

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// loggerFor returns a logger writing JSON into the buffer, with redaction.
func loggerFor(buf *bytes.Buffer, level slog.Level, redactor *Redactor) *slog.Logger {
	return Configure(buf, level, redactor)
}

func decodeLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("nothing was logged")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, line)
	}
	return decoded
}

func TestConfiguredLoggerWritesOneJSONObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	logger := loggerFor(&buf, slog.LevelInfo, NewRedactor())

	logger.Info("serving http", "address", "127.0.0.1:8080", "tls", false)

	line := strings.TrimSpace(buf.String())
	if strings.Count(line, "\n") != 0 {
		t.Errorf("a record spans several lines: %q", line)
	}
	decoded := decodeLine(t, &buf)
	for _, field := range []string{"time", "level", "msg"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("record has no %q field: %v", field, decoded)
		}
	}
	if decoded["msg"] != "serving http" {
		t.Errorf("msg = %v, want the message", decoded["msg"])
	}
	if decoded["address"] != "127.0.0.1:8080" {
		t.Errorf("address = %v, want the attribute", decoded["address"])
	}
}

func TestLevelIsHonoured(t *testing.T) {
	for _, test := range []struct {
		name      string
		level     slog.Level
		log       func(*slog.Logger)
		wantLines int
	}{
		{name: "info drops debug", level: slog.LevelInfo, log: func(l *slog.Logger) { l.Debug("noise") }, wantLines: 0},
		{name: "info keeps info", level: slog.LevelInfo, log: func(l *slog.Logger) { l.Info("kept") }, wantLines: 1},
		{name: "warn drops info", level: slog.LevelWarn, log: func(l *slog.Logger) { l.Info("noise") }, wantLines: 0},
		{name: "warn keeps error", level: slog.LevelWarn, log: func(l *slog.Logger) { l.Error("kept") }, wantLines: 1},
		{name: "debug keeps debug", level: slog.LevelDebug, log: func(l *slog.Logger) { l.Debug("kept") }, wantLines: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			test.log(loggerFor(&buf, test.level, NewRedactor()))

			lines := 0
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				if strings.TrimSpace(line) != "" {
					lines++
				}
			}
			if lines != test.wantLines {
				t.Errorf("wrote %d line(s), want %d: %q", lines, test.wantLines, buf.String())
			}
		})
	}
}

// TestRegisteredSecretIsRedactedEverywhere is the acceptance criterion: a
// registered value must not appear in the message, an attribute, a group, or an
// error, in any encoding.
func TestRegisteredSecretIsRedactedEverywhere(t *testing.T) {
	const secret = "sup3rsecret"
	var buf bytes.Buffer
	logger := loggerFor(&buf, slog.LevelDebug, NewRedactor(secret))

	logger.Info("connecting to postgres://gitnest:" + secret + "@db:5432/gitnest")
	logger.Info("attribute", "dsn", "postgres://gitnest:"+secret+"@db:5432/gitnest")
	logger.Info("group", slog.Group("connection", slog.String("password", secret)))
	logger.Info("error", "cause", errors.New("password authentication failed for "+secret))
	logger.Info("base64", "value", base64.StdEncoding.EncodeToString([]byte(secret)))
	logger.Info("url-safe base64", "value", base64.RawURLEncoding.EncodeToString([]byte(secret)))

	output := buf.String()
	if strings.Contains(output, secret) {
		t.Fatalf("the secret survived redaction: %s", output)
	}
	for _, encoded := range []string{
		base64.StdEncoding.EncodeToString([]byte(secret)),
		base64.RawURLEncoding.EncodeToString([]byte(secret)),
	} {
		if strings.Contains(output, encoded) {
			t.Errorf("an encoded form of the secret survived: %s", output)
		}
	}
	if !strings.Contains(output, Placeholder) {
		t.Errorf("no placeholder appeared, so nothing was redacted: %s", output)
	}
}

// TestMultilineSecretIsRedactedInJSONForm covers the escaping case: inside a
// structured payload a newline becomes \n, which is a different string.
func TestMultilineSecretIsRedactedInJSONForm(t *testing.T) {
	secret := "-----BEGIN KEY-----\nline two\n-----END KEY-----"
	var buf bytes.Buffer
	logger := loggerFor(&buf, slog.LevelInfo, NewRedactor(secret))

	logger.Info("loaded", "key", secret)
	logger.Info("escaped", "raw", strings.ReplaceAll(secret, "\n", `\n`))

	output := buf.String()
	if strings.Contains(output, "line two") {
		t.Errorf("the multi-line secret survived redaction: %s", output)
	}
}

// TestURLCredentialsAreRedactedWithoutRegistration covers the pattern rule: a
// connection string nobody registered must still lose its password.
func TestURLCredentialsAreRedactedWithoutRegistration(t *testing.T) {
	var buf bytes.Buffer
	logger := loggerFor(&buf, slog.LevelInfo, NewRedactor())

	logger.Info("clone failed", "url", "https://user:hunter2@git.example.com/team/repo.git")
	logger.Info("connect", "dsn", "postgres://gitnest:hunter2@db:5432/gitnest?sslmode=disable")

	output := buf.String()
	if strings.Contains(output, "hunter2") {
		t.Fatalf("a URL password survived redaction: %s", output)
	}
	if !strings.Contains(output, "git.example.com/team/repo.git") {
		t.Errorf("redaction removed more than the credential: %s", output)
	}
}

func TestEmptySecretIsIgnored(t *testing.T) {
	var buf bytes.Buffer
	logger := loggerFor(&buf, slog.LevelInfo, NewRedactor(""))

	logger.Info("a normal message with words")

	output := buf.String()
	if strings.Contains(output, Placeholder) {
		t.Errorf("an empty secret erased unrelated text: %s", output)
	}
	if !strings.Contains(output, "a normal message with words") {
		t.Errorf("the message was altered: %s", output)
	}
}

func TestRedactAppliesTheURLRuleWithoutARedactor(t *testing.T) {
	var redactor *Redactor
	got := redactor.Redact("postgres://user:secret@db/gitnest")
	if strings.Contains(got, "secret") {
		t.Errorf("Redact on a nil redactor left the password: %q", got)
	}
}

func TestParseLevel(t *testing.T) {
	for _, test := range []struct {
		name    string
		input   string
		want    slog.Level
		wantErr bool
	}{
		{name: "debug", input: "debug", want: slog.LevelDebug},
		{name: "info", input: "info", want: slog.LevelInfo},
		{name: "warn", input: "warn", want: slog.LevelWarn},
		{name: "error", input: "error", want: slog.LevelError},
		{name: "mixed case and padding", input: " WARN ", want: slog.LevelWarn},
		{name: "unknown", input: "verbose", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseLevel(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseLevel(%q) = %v, want an error", test.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLevel(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", test.input, got, test.want)
			}
		})
	}
}

func TestCorrelationIDTravelsInTheContext(t *testing.T) {
	ctx := context.Background()
	if got := CorrelationID(ctx); got != "" {
		t.Errorf("CorrelationID on a bare context = %q, want empty", got)
	}

	ctx = WithCorrelationID(ctx, "abc123")
	if got := CorrelationID(ctx); got != "abc123" {
		t.Errorf("CorrelationID = %q, want abc123", got)
	}

	var buf bytes.Buffer
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	slog.SetDefault(loggerFor(&buf, slog.LevelInfo, NewRedactor()))
	Logger(ctx).Info("with context")

	decoded := decodeLine(t, &buf)
	if decoded["correlation_id"] != "abc123" {
		t.Errorf("correlation_id = %v, want it attached by Logger", decoded["correlation_id"])
	}
}

func TestNewCorrelationIDIsRandomAndBounded(t *testing.T) {
	first := NewCorrelationID()
	second := NewCorrelationID()

	if len(first) != 32 {
		t.Errorf("identifier %q is not a 16-byte hex string", first)
	}
	if first == second {
		t.Error("two identifiers were identical")
	}
}

func TestRegisterIsIdempotent(t *testing.T) {
	redactor := NewRedactor()
	redactor.Register("secret-one")
	redactor.Register("secret-one")

	redactor.mu.RLock()
	count := len(redactor.secrets)
	redactor.mu.RUnlock()

	// One value produces the plain, two base64, and one JSON form at most.
	if count > 4 {
		t.Errorf("registering the same secret twice stored %d variants", count)
	}
}
