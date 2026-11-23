package tools

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Hide secrets switch recorded per profile home key from that profile's settings
var redactPolicy = struct {
	sync.Mutex
	cfg map[string]bool
}{cfg: map[string]bool{}}

// Set by a process that must hide secrets whatever its profile says, such as the MCP tool server
var redactionForced atomic.Bool

func init() {
	config.RegisterRedactionSwitch(RedactEnabled)
	config.RegisterPolicyNote(func(setter, homeKey string, raw any) {
		if setter == "note_config_policy" {
			NoteConfigPolicy(homeKey, raw)
		}
	})
}

// ForceSecretRedaction hides secrets in this process whatever the profile setting says
func ForceSecretRedaction() { redactionForced.Store(true) }

// Records the hide secrets value a profile's config holds, an unset or unreadable value keeping it on
func NoteConfigPolicy(homeKey string, raw any) {
	on := true
	if b, ok := config.AsBool(raw); ok && raw != nil {
		on = b
	}
	redactPolicy.Lock()
	redactPolicy.cfg[homeKey] = on
	redactPolicy.Unlock()
}

// Key of the process home, empty when the process has no home
func processHomeKey() (key string) {
	defer func() {
		if recover() != nil {
			key = ""
		}
	}()
	return home.Key(home.Process())
}

// Effective redaction switch of the context profile from its recorded setting, on until one is recorded
func redactEnabledFor(ctx context.Context) bool {
	if redactionForced.Load() {
		return true
	}
	key := processHomeKey()
	if override := home.Override(ctx); override != "" {
		key = home.Key(override)
	}
	redactPolicy.Lock()
	defer redactPolicy.Unlock()
	on, recorded := redactPolicy.cfg[key]
	return on || !recorded
}

// Whether secret redaction is on for the context profile
func RedactEnabled(ctx context.Context) bool {
	return redactEnabledFor(ctx)
}

// Log handler that redacts secrets from every record message and attribute
type RedactingHandler struct {
	next slog.Handler
	ops  []redactHandlerOp
}

// One WithAttrs or WithGroup call replayed per record under that record's policy
type redactHandlerOp struct {
	group string
	attrs []slog.Attr
}

// Wraps next so records are redacted under the policy of their context profile
func NewRedactingHandler(next slog.Handler) *RedactingHandler {
	return &RedactingHandler{next: next}
}

// Defers the level decision to the wrapped handler
func (h *RedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Redacts the message and every bound or record attribute when the context profile redacts
func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	enabled := redactEnabledFor(ctx)
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
	return &RedactingHandler{next: h.next, ops: ops}
}

// Opens a group replayed on the wrapped handler per record
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	ops := append(append([]redactHandlerOp(nil), h.ops...), redactHandlerOp{group: name})
	return &RedactingHandler{next: h.next, ops: ops}
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
