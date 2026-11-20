package cronconfig

import (
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Provider every cron job without its own pin resolves through, read from cron.model_provider
func DefaultProvider(cfg map[string]any) string {
	cron, ok := cfg["cron"].(map[string]any)
	if !ok {
		return ""
	}
	value := cron["model_provider"]
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(value))
}
