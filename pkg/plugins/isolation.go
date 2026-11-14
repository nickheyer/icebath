package plugins

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const (
	IsolationInProcess = "in_process"
	IsolationHost      = "host"
)

// plugins.isolation of the active profile, an unknown value warning and meaning in process
func IsolationMode(ctx context.Context, cfg map[string]any) string {
	if os.Getenv(HostProcessEnv) == "1" {
		return IsolationInProcess
	}
	if cfg == nil {
		cfg = loadConfigOrEmpty(home.Dir(ctx))
	}
	raw, ok := pluginsSection(cfg)["isolation"]
	if !ok {
		raw = IsolationInProcess
	}
	text := IsolationInProcess
	if config.PyTruthy(raw) {
		text = config.PyStr(raw)
	}
	mode := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(text)), "-", "_")
	if mode != IsolationInProcess && mode != IsolationHost {
		logWarn(fmt.Sprintf("plugins.isolation=%s is not one of %s; loading plugins in-process", config.PyRepr(raw),
			IsolationHost+", "+IsolationInProcess))
		return IsolationInProcess
	}
	return mode
}

// Reason an in process import of third party code must not happen, empty when allowed
func InProcessImportRefusal(ctx context.Context, what, source string, cfg map[string]any) string {
	if source == "bundled" || IsolationMode(ctx, cfg) != IsolationHost {
		return ""
	}
	return what + " runs only in-process, and plugins.isolation is 'host' (third-party plugin code " +
		"never runs inside the Icebath process); set plugins.isolation: in_process to load it"
}

// Plugin host of the active profile when plugins.isolation is host, else nil
func UserPluginHost(ctx context.Context) (*PluginHost, error) {
	if IsolationMode(ctx, nil) != IsolationHost {
		return nil, nil
	}
	return GetPluginManager(ctx).pluginHost()
}
