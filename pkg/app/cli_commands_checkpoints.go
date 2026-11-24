package app

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Plain map of a decoded JSON row
func plainMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	if m, ok := jsonutil.Plain(v).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
