package tuiserver

import (
	"strings"

	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var sessionInterruptRegistry = &handlerRegistry{}

func init() {
	sessionInterruptRegistry.install()
}

// Transport of the current request, stdio when none is bound
func callerTransport(ctx context.Context) tuihost.Transport {
	if transport := tuihost.CurrentTransport(ctx); transport != nil {
		return transport
	}
	return tuihost.ModuleStdioTransport()
}

// Trimmed text of a param, the default for a missing or falsy value
func strParam(params map[string]any, key, def string) string {
	raw := params[key]
	if !truthy(raw) {
		raw = ""
	}
	if text := strings.TrimSpace(pyrepr.Str(raw)); text != "" {
		return text
	}
	return def
}
