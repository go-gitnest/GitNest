// Package logging configures the process logger and owns the single redaction
// boundary every log record passes through.
//
// Redaction happens on values, not on a rendered string: the handler rewrites
// each attribute before the JSON encoder sees it, so a secret cannot survive by
// being embedded in a message, an attribute, or a nested group. Two mechanisms
// apply, in this order:
//
//  1. A registered-secret list holds the exact values that must never appear,
//     including their base64 and JSON-escaped forms. Every secret the process
//     handles is registered here as it is read.
//  2. A pattern rule strips the password from any URL that carries userinfo, so
//     a credentialed connection string cannot leak through a message nobody
//     thought to register.
package logging

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Placeholder replaces a redacted value. It is fixed so an operator can grep
// for it and so a test can assert its absence of the original.
const Placeholder = "[REDACTED]"

// urlUserinfo matches the credential part of a URL: scheme://user:password@.
var urlUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^/@\s:]+):[^/@\s]*@`)

// Redactor holds the values that must never appear in log output and rewrites
// them wherever they occur.
//
// It is safe for concurrent use: secrets are registered during startup and from
// request paths (a webhook secret read for a delivery, for example) while other
// goroutines are logging.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
	known   map[string]struct{}
}

// NewRedactor returns a redactor with the given values registered.
func NewRedactor(values ...string) *Redactor {
	redactor := &Redactor{known: map[string]struct{}{}}
	redactor.Register(values...)
	return redactor
}

// defaultRedactor is the process-wide registry. Code that handles a new secret
// registers it here as soon as it is read.
var defaultRedactor = NewRedactor()

// DefaultRedactor returns the process-wide registry.
func DefaultRedactor() *Redactor { return defaultRedactor }

// Register adds values that must never appear in log output. Empty values are
// ignored, because an absent secret must not turn the whole line into a
// placeholder.
//
// Each value is stored in the forms it can appear in: as written, base64
// standard, base64 URL-safe, and JSON-escaped. A secret shorter than four
// characters is still registered: masking unrelated text is a smaller problem
// than leaking a credential.
func (r *Redactor) Register(values ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.known == nil {
		r.known = map[string]struct{}{}
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		for _, variant := range variants(value) {
			if _, seen := r.known[variant]; seen {
				continue
			}
			r.known[variant] = struct{}{}
			r.secrets = append(r.secrets, variant)
		}
	}
	// Longest first, so a secret that contains another secret is replaced
	// before its own substring is.
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

// Redact returns text with every registered secret and every URL credential
// replaced. A nil redactor still applies the pattern rule, so a caller that
// forgot to construct one cannot leak a credentialed URL.
func (r *Redactor) Redact(text string) string {
	if text == "" {
		return text
	}
	text = urlUserinfo.ReplaceAllString(text, "$1:"+Placeholder+"@")
	if r == nil {
		return text
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, secret := range r.secrets {
		text = strings.ReplaceAll(text, secret, Placeholder)
	}
	return text
}

// variants lists the forms a secret can appear in.
func variants(secret string) []string {
	forms := []string{
		secret,
		base64.StdEncoding.EncodeToString([]byte(secret)),
		base64.RawURLEncoding.EncodeToString([]byte(secret)),
	}
	// A JSON-encoded string escapes newlines, quotes, and non-ASCII characters,
	// which is how a secret appears inside a structured payload.
	if encoded, err := json.Marshal(secret); err == nil {
		if inner := strings.Trim(string(encoded), `"`); inner != "" {
			forms = append(forms, inner)
		}
	}
	return forms
}

// ParseLevel maps a configuration level name to a slog level.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q; want debug, info, warn, or error", name)
	}
}

// Configure builds a logger that writes one JSON object per line to out, drops
// records below level, and passes every record through redact.
func Configure(out io.Writer, level slog.Level, redact *Redactor) *slog.Logger {
	return slog.New(RedactHandler(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level}), redact))
}

// ConfigureDefault builds the process logger from the default redactor.
func ConfigureDefault(out io.Writer, level slog.Level) *slog.Logger {
	return Configure(out, level, DefaultRedactor())
}

// RedactHandler wraps a handler so that every record is redacted before it is
// formatted. Wrapping the handler rather than the output keeps redaction on
// values: an attribute added by a later With() call is redacted too, and a
// secret cannot be reconstructed by the encoder escaping it differently.
func RedactHandler(next slog.Handler, redact *Redactor) slog.Handler {
	return &redactingHandler{next: next, redact: redact}
}

type redactingHandler struct {
	next   slog.Handler
	redact *Redactor
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, h.redact.Redact(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(h.redactAttr(attr))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		clean = append(clean, h.redactAttr(attr))
	}
	return &redactingHandler{next: h.next.WithAttrs(clean), redact: h.redact}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name), redact: h.redact}
}

// redactAttr redacts a string value and recurses into groups and slices, so a
// secret cannot hide one level down.
func (h *redactingHandler) redactAttr(attr slog.Attr) slog.Attr {
	switch attr.Value.Kind() {
	case slog.KindString:
		return slog.String(attr.Key, h.redact.Redact(attr.Value.String()))
	case slog.KindGroup:
		group := attr.Value.Group()
		clean := make([]slog.Attr, 0, len(group))
		for _, child := range group {
			clean = append(clean, h.redactAttr(child))
		}
		return slog.Group(attr.Key, attrsToAny(clean)...)
	case slog.KindAny:
		// An arbitrary value is only redacted when it is a string or an error,
		// which are the forms this codebase logs. Anything else is passed
		// through unchanged rather than being rendered and re-parsed.
		switch typed := attr.Value.Any().(type) {
		case string:
			return slog.String(attr.Key, h.redact.Redact(typed))
		case error:
			return slog.String(attr.Key, h.redact.Redact(typed.Error()))
		}
	}
	return attr
}

func attrsToAny(attrs []slog.Attr) []any {
	values := make([]any, 0, len(attrs))
	for _, attr := range attrs {
		values = append(values, attr)
	}
	return values
}

// correlationKey is the context key for the correlation identifier.
type correlationKey struct{}

// WithCorrelationID attaches the identifier that ties every log line of one
// request or job together.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

// CorrelationID returns the identifier attached to ctx, or "" when there is
// none.
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// Logger returns the process logger annotated with the context's correlation
// identifier, so a caller does not have to repeat it on every line.
func Logger(ctx context.Context) *slog.Logger {
	logger := slog.Default()
	if id := CorrelationID(ctx); id != "" {
		return logger.With(slog.String("correlation_id", id))
	}
	return logger
}

// NewCorrelationID returns a random identifier for one request or job.
func NewCorrelationID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// A failure here would leave lines uncorrelated, which is worse than a
		// predictable identifier but not worth failing a request over.
		return "unavailable"
	}
	return hex.EncodeToString(buf)
}
