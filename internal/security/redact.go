package security

import (
	"context"
	"log/slog"
	"strings"
)

// sensitiveKeys are attribute names whose values must never reach logs.
var sensitiveKeys = []string{
	"password", "passwd", "secret", "api_key", "apikey", "token", "authorization",
	"cookie", "session", "ticket", "document", "passport", "national_id", "tax_id", "ssn", "dob", "iban", "account_number",
}

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// RedactingHandler wraps an slog.Handler and replaces the value of any
// sensitive-looking attribute (including nested groups) with "[REDACTED]".
// It is a safety net; code should still avoid logging secrets at all.
type RedactingHandler struct{ next slog.Handler }

func NewRedactingHandler(next slog.Handler) *RedactingHandler { return &RedactingHandler{next: next} }

func (h *RedactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redact(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *RedactingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(as))
	for i, a := range as {
		red[i] = redact(a)
	}
	return &RedactingHandler{next: h.next.WithAttrs(red)}
}

func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{next: h.next.WithGroup(name)}
}

func redact(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		g := a.Value.Group()
		red := make([]any, len(g))
		for i, x := range g {
			red[i] = redact(x)
		}
		return slog.Group(a.Key, red...)
	}
	if isSensitive(a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}
	return a
}
