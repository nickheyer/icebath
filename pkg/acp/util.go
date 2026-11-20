package acp

import "github.com/nickheyer/icebath/pkg/internal/pyrepr"

// Python truthiness of a decoded JSON value
func truthy(v any) bool { return pyrepr.Truthy(v) }

// Python str of a value
func pyStr(v any) string { return pyrepr.Str(v) }
