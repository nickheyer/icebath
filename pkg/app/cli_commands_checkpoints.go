package app

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Plain map of a decoded JSON row
func plainMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	if m, ok := pyjson.Plain(v).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
