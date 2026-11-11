package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Python equality of two decoded values
func pyjsonEqual(a, b any) bool { return pyjson.Equal(a, b) }

// Session key of a record, else the fallback id
func sessionKeyOr(session *tuigateway.Session, fallback string) string {
	if key := strOr(session.Get("session_key")); key != "" {
		return key
	}
	return fallback
}

// Python int of an integer slot, zero when absent
func intOr(v any) int64 {
	if v == nil {
		return 0
	}
	n, err := pyint.Int64(v)
	if err != nil {
		return 0
	}
	return n
}

// Copy of a list slot of strings
func stringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return append([]string{}, x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, pyrepr.Str(item))
		}
		return out
	}
	return []string{}
}

// Value when truthy, else integer zero like a Python or 0 chain
func orZero(v any) any {
	if truthy(v) {
		return v
	}
	return int64(0)
}

// Session history slot as a list copy
func historyList(v any) []any {
	switch x := v.(type) {
	case []any:
		return append([]any{}, x...)
	case []map[string]any:
		return messagesAsHistory(x)
	}
	return []any{}
}

// Agent messages as session history entries
func messagesAsHistory(messages []map[string]any) []any {
	out := make([]any, len(messages))
	for i, m := range messages {
		out[i] = m
	}
	return out
}

// Session history entries as agent messages, non dict entries dropped
func historyAsMessages(history []any) []map[string]any {
	out := make([]map[string]any, 0, len(history))
	for _, entry := range history {
		if m, ok := entry.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Runs body and returns what it panicked with, nil when it returned
func catchPanic(body func()) (failure any) {
	defer func() {
		if r := recover(); r != nil {
			failure = r
		}
	}()
	body()
	return nil
}

// Raises a returned error like the Python call that would have raised it
func panicIf(err error) {
	if err != nil {
		panic(err)
	}
}
