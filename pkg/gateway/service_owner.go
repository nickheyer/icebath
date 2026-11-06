package gateway

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyplist"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Final component suffix the way pathlib computes it, empty for dot files and trailing dots
func pathSuffix(p string) string {
	name := filepath.Base(p)
	i := strings.LastIndexByte(name, '.')
	if i > 0 && i < len(name)-1 {
		return name[i:]
	}
	return ""
}

// ICEBATH_HOME pinned by the systemd unit or launchd plist at definition, empty when it pins none
func ServicePinnedHome(definition string) (string, error) {
	if pathSuffix(definition) != ".plist" {
		return hermesHomePinnedByUnit(definition)
	}
	raw, err := os.ReadFile(definition)
	if err != nil {
		return "", nil
	}
	data, err := pyplist.Load(raw)
	if err != nil {
		return "", nil
	}
	d, ok := data.(*pyplist.Dict)
	if !ok {
		return "", nil
	}
	envValue, _ := d.Get("EnvironmentVariables")
	env, ok := envValue.(*pyplist.Dict)
	if !ok {
		return "", nil
	}
	value, present := env.Get("ICEBATH_HOME")
	if !present || value == nil {
		return "", nil
	}
	return pystr.Strip(pyplist.Str(value)), nil
}
