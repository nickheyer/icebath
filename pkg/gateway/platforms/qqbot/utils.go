package qqbot

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Stamped icebath version, dev for an unstamped build
func icebathVersion() string {
	if v := updaterecord.BuildVersion(); v != "" {
		return v
	}
	return "dev"
}

// BuildUserAgent names the adapter version, the runtime, the OS and the icebath version
func BuildUserAgent() string {
	return fmt.Sprintf("QQBotAdapter/%s (Go/%s; %s; Icebath/%s)", QQBotVersion,
		strings.TrimPrefix(runtime.Version(), "go"), strings.ToLower(pyplatform.System()), icebathVersion())
}

// GetAPIHeaders returns the JSON content headers q.qq.com needs to skip its anti bot page
func GetAPIHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json", "Accept": "application/json", "User-Agent": BuildUserAgent()}
}

// CoerceList turns a comma separated string, a list or a scalar into trimmed non empty strings
func CoerceList(value any) []string {
	var items []any
	switch v := value.(type) {
	case nil:
		return []string{}
	case string:
		for _, s := range strings.Split(v, ",") {
			items = append(items, s)
		}
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	default:
		items = []any{v}
	}
	out := []string{}
	for _, item := range items {
		if s := pystr.Strip(pyrepr.Str(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}
