package browser

import "github.com/nickheyer/icebath/pkg/tools"

// Install line the doctor prints when the Lightpanda engine is selected without a binary
const LightpandaInstallHint = lightpandaInstallHint

// One actionable line for a machine without Chromium that never suggests npm
func ChromiumMissingHint(env *tools.Env) string { return chromiumMissingHint(env) }

// Lightpanda binary found on PATH or in a known install directory, empty when absent
func FindLightpandaBinary(env *tools.Env) string { return findLightpandaBinary(env) }

// Whether browser.engine selects Lightpanda in the config of env
func UsingLightpandaEngine(env *tools.Env) bool { return usingLightpandaEngine(env) }

// Whether the Lightpanda engine is in effect and the setting shadowing it otherwise
func LightpandaEngineStatus(env *tools.Env) (bool, string) { return lightpandaEngineStatus(env) }
