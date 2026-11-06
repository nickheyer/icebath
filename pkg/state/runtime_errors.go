package state

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// State key prefix of a durable deleted session tombstone
const RetiredPrefix = "gateway.retired_session.v1."

// Refusal by the state authority, Reason being the bounded wire code callers compare
type RuntimeStoreError struct {
	Reason string
	Field  *string
	Detail *string
}

func (e *RuntimeStoreError) Error() string { return e.Reason }

// Refusal with its field and detail collapsed to one bounded printable line
func NewRuntimeStoreError(reason string, field, detail *string) *RuntimeStoreError {
	return &RuntimeStoreError{Reason: reason, Field: runtimeOneLine(field, 80), Detail: runtimeOneLine(detail, 160)}
}

// Whitespace collapsed, non printables dropped and length bounded, nil when nothing remains
func runtimeOneLine(value *string, limit int) *string {
	if value == nil {
		return nil
	}
	var b strings.Builder
	for _, r := range strings.Join(pySplitWhitespace(*value), " ") {
		if pystr.IsPrintable(r) {
			b.WriteRune(r)
		}
	}
	runes := []rune(b.String())
	if len(runes) > limit {
		runes = runes[:limit]
	}
	if len(runes) == 0 {
		return nil
	}
	text := string(runes)
	return &text
}
