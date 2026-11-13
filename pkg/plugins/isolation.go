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
