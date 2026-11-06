package browser

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Docker image build output naming the staged pm Chromium binary for its architecture
var bakedExecutableFile = "/etc/icebath/chromium-executable-path"

// Distribution command names searched only where pm builds no Chromium for the target
var systemChromiumCommands = []string{"chromium", "chromium-browser"}

// Seams the runtime reads pm, host and PATH facts through
var (
	runtimePMExecutable = func() string {
		installed, err := pm.Installed(context.Background(), "chromium", false)
		if err != nil || installed == nil {
			return ""
		}
		return installed.Binary
	}
	runtimePMHasNoChromium = func() bool {
		p, err := pm.GetPackage("chromium")
		if err != nil {
			return false
		}
		target, err := runtimeCurrentTarget()
		return err == nil && p.MissingReason(target) != ""
	}
	runtimeCurrentTarget = pm.CurrentTarget
	runtimeLocateCommand = func(name string) []string { return tools.LocateCommand(name) }
	runtimeIsTermux      = hostinfo.IsTermux
)

// The browser.executable_path pin of the active profile, tilde expanded
func configuredExecutable(env *tools.Env) string {
	return expandTilde(cfgOf(env).executablePath())
}

func bakedExecutable() string {
	raw, err := os.ReadFile(bakedExecutableFile)
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	if value == "" || !isRegularFile(value) {
		return ""
	}
	return value
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func systemExecutable() string {
	for _, name := range systemChromiumCommands {
		if found := runtimeLocateCommand(name); len(found) > 0 {
			return found[0]
		}
	}
	return ""
}

// Chromium to run, the pin then pm's install then the baked path then a system one where pm builds none
func chromiumExecutable(env *tools.Env, allowOverride bool) string {
	if allowOverride {
		if override := configuredExecutable(env); override != "" {
			return override
		}
	}
	if found := runtimePMExecutable(); found != "" {
		return found
	}
	if found := bakedExecutable(); found != "" {
		return found
	}
	if runtimePMHasNoChromium() {
		return systemExecutable()
	}
	return ""
}

// One actionable line for a machine without Chromium that never suggests npm
func chromiumMissingHint(env *tools.Env) string {
	if pinned := configuredExecutable(env); pinned != "" {
		return "browser.executable_path is set to " + pinned + ", which is not a browser on this machine: fix the " +
			"path, or clear it to use Icebath's Chromium."
	}
	if runtimeIsTermux() {
		return "No Chromium found: pm builds none for Termux. Install one with " +
			"`pkg install x11-repo && pkg install chromium`, or set browser.executable_path."
	}
	if _, err := os.Stat(bakedExecutableFile); err == nil {
		return "The image's Chromium is missing: pull the current Icebath image and recreate the " +
			"container (a volume mounted over the image's tool store hides it)."
	}
	if runtimePMHasNoChromium() {
		return "No Chromium found: pm builds none for this platform. Install your distribution's " +
			"chromium package, or set browser.executable_path."
	}
	return "Chromium is not installed: run `icebath pm install chromium`."
}
