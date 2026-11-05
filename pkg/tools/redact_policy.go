package tools

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Raw redaction switches recorded per profile home key, nil when the source left the switch unset
var redactPolicy = struct {
	sync.Mutex
	env map[string]*string
	cfg map[string]*string
}{env: map[string]*string{}, cfg: map[string]*string{}}

func init() {
	config.RegisterPolicyNote(func(setter, homeKey string, raw any) {
		switch setter {
		case "note_env_policy":
			NoteEnvPolicy(homeKey, raw)
		case "note_config_policy":
			NoteConfigPolicy(homeKey, raw)
		}
	})
}

// Launch profile redaction switch read once from the process environment
var launchRedactEnabled = sync.OnceValue(func() bool {
	v, ok := os.LookupEnv("ICEBATH_REDACT_SECRETS")
	if !ok {
		v = "true"
	}
	return redactSwitchOn(v)
})

// Reports whether a raw switch value turns redaction on
func redactSwitchOn(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Raw value as Python str gives it, nil for None
func policyRaw(raw any) *string {
	if raw == nil {
		return nil
	}
	s := pyrepr.Str(raw)
	return &s
}

// Records the ICEBATH_REDACT_SECRETS value a profile dotenv holds
func NoteEnvPolicy(homeKey string, raw any) {
	redactPolicy.Lock()
	redactPolicy.env[homeKey] = policyRaw(raw)
	redactPolicy.Unlock()
}

// Records the security.redact_secrets value a profile config holds
func NoteConfigPolicy(homeKey string, raw any) {
	redactPolicy.Lock()
	redactPolicy.cfg[homeKey] = policyRaw(raw)
	redactPolicy.Unlock()
}

// Effective redaction switch of the context profile from the live secret scope and recorded policy only
func redactEnabledFor(ctx context.Context, scopeOf func(context.Context) map[string]string) bool {
	override := home.Override(ctx)
	if override == "" {
		return launchRedactEnabled()
	}
	key := home.Key(override)
	var scope map[string]string
	if scopeOf != nil {
		scope = scopeOf(ctx)
	}
	var raw *string
	if v, ok := scope["ICEBATH_REDACT_SECRETS"]; ok {
		raw = &v
	}
	redactPolicy.Lock()
	defer redactPolicy.Unlock()
	if raw == nil && scope == nil {
		recorded, ok := redactPolicy.env[key]
		if !ok {
			return true
		}
		raw = recorded
	}
	if raw == nil {
		raw = redactPolicy.cfg[key]
	}
	if raw == nil {
		return true
	}
	return redactSwitchOn(*raw)
}

// Log handler that redacts secrets from every record message and attribute
type RedactingHandler struct {
	next    slog.Handler
	scopeOf func(context.Context) map[string]string
	ops     []redactHandlerOp
}

// One WithAttrs or WithGroup call replayed per record under that record's policy
type redactHandlerOp struct {
	group string
	attrs []slog.Attr
}

// Wraps next so records are redacted under the profile policy, scopeOf reading the live secret scope
func NewRedactingHandler(next slog.Handler, scopeOf func(context.Context) map[string]string) *RedactingHandler {
	return &RedactingHandler{next: next, scopeOf: scopeOf}
}

// Defers the level decision to the wrapped handler
func (h *RedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Redacts the message and every bound or record attribute when the context profile redacts
func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	enabled := redactEnabledFor(ctx, h.scopeOf)
	next := h.next
	for _, op := range h.ops {
		if op.attrs == nil {
			next = next.WithGroup(op.group)
			continue
		}
		attrs := op.attrs
		if enabled {
			attrs = make([]slog.Attr, len(op.attrs))
			for i, a := range op.attrs {
				attrs[i] = redactLogAttr(a)
			}
		}
		next = next.WithAttrs(attrs)
	}
	if !enabled {
		return next.Handle(ctx, r)
	}
	out := slog.NewRecord(r.Time, r.Level, redactLogText(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactLogAttr(a))
		return true
	})
	return next.Handle(ctx, out)
}

// Binds attributes that each record redacts under its own policy
func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	ops := append(append([]redactHandlerOp(nil), h.ops...), redactHandlerOp{attrs: append([]slog.Attr(nil), attrs...)})
	return &RedactingHandler{next: h.next, scopeOf: h.scopeOf, ops: ops}
}

// Opens a group replayed on the wrapped handler per record
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	ops := append(append([]redactHandlerOp(nil), h.ops...), redactHandlerOp{group: name})
	return &RedactingHandler{next: h.next, scopeOf: h.scopeOf, ops: ops}
}

// Default redaction passes applied unconditionally to formatted log text
func redactLogText(text string) string {
	return RedactSensitiveTextWith(nil, text, RedactOptions{Force: true})
}

// Attribute with its rendered value redacted, groups redacted member by member
func redactLogAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		members := v.Group()
		out := make([]any, len(members))
		for i, m := range members {
			out[i] = redactLogAttr(m)
		}
		return slog.Group(a.Key, out...)
	}
	text := v.String()
	if redacted := redactLogText(text); redacted != text {
		return slog.String(a.Key, redacted)
	}
	return slog.Attr{Key: a.Key, Value: v}
}
