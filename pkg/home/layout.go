package home

import "path/filepath"

// Config file of a home
func ConfigPath(h string) string { return filepath.Join(h, "config.yaml") }

// Secrets file of a home
func EnvPath(h string) string { return filepath.Join(h, ".env") }

// OAuth credential store of a home
func AuthPath(h string) string { return filepath.Join(h, "auth.json") }

// Identity file of a home
func SoulPath(h string) string { return filepath.Join(h, "SOUL.md") }

// Session database of a home
func StatePath(h string) string { return filepath.Join(h, "state.db") }

// Memory directory of a home
func MemoriesDir(h string) string { return filepath.Join(h, "memories") }

// Skills directory of a home
func SkillsDir(h string) string { return filepath.Join(h, "skills") }

// Skill bundles directory of a home
func BundlesDir(h string) string { return filepath.Join(h, "skill-bundles") }

// Plugins directory of a home
func PluginsDir(h string) string { return filepath.Join(h, "plugins") }

// Plugin data directory of a home
func PluginDataDir(h string) string { return filepath.Join(h, "plugin-data") }

// Cron directory of a home
func CronDir(h string) string { return filepath.Join(h, "cron") }

// Logs directory of a home
func LogsDir(h string) string { return filepath.Join(h, "logs") }

// Cache directory of a home
func CacheDir(h string) string { return filepath.Join(h, "cache") }

// Scratch directory a home hands to child processes as their temp dir
func ScratchDir(h string) string { return filepath.Join(h, "cache", "scratch") }

// Hook output spill directory of a home
func HookOutputsDir(h string) string { return filepath.Join(h, "hook_outputs") }

// Pending approval directory of a home
func PendingDir(h string) string { return filepath.Join(h, "pending") }

// Scripts directory cron jobs run from
func ScriptsDir(h string) string { return filepath.Join(h, "scripts") }

// Sessions export directory of a home
func SessionsDir(h string) string { return filepath.Join(h, "sessions") }

// Trajectories directory of a home
func TrajectoriesDir(h string) string { return filepath.Join(h, "trajectories") }

// Locales directory of a home
func LocalesDir(h string) string { return filepath.Join(h, "locales") }

// Skins directory of a home
func SkinsDir(h string) string { return filepath.Join(h, "skins") }

// Shell hook allowlist file of a home
func ShellHooksAllowlistPath(h string) string { return filepath.Join(h, "shell-hooks-allowlist.json") }

// Marker file turning bundled skill seeding off for a home
func NoBundledSkillsMarker(h string) string { return filepath.Join(h, ".no-bundled-skills") }
