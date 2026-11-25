package home

import pathutil "github.com/nickheyer/icebath/pkg/utils/path"

// Config file of a home
func ConfigPath(h string) string { return pathutil.JoinKeepDotDot(h, "config.yaml") }

// Secrets file of a home
func EnvPath(h string) string { return pathutil.JoinKeepDotDot(h, ".env") }

// OAuth credential store of a home
func AuthPath(h string) string { return pathutil.JoinKeepDotDot(h, "auth.json") }

// Identity file of a home
func SoulPath(h string) string { return pathutil.JoinKeepDotDot(h, "SOUL.md") }

// Session database of a home
func StatePath(h string) string { return pathutil.JoinKeepDotDot(h, "state.db") }

// Memory directory of a home
func MemoriesDir(h string) string { return pathutil.JoinKeepDotDot(h, "memories") }

// Skills directory of a home
func SkillsDir(h string) string { return pathutil.JoinKeepDotDot(h, "skills") }

// Skill bundles directory of a home
func BundlesDir(h string) string { return pathutil.JoinKeepDotDot(h, "skill-bundles") }

// Plugins directory of a home
func PluginsDir(h string) string { return pathutil.JoinKeepDotDot(h, "plugins") }

// Plugin data directory of a home
func PluginDataDir(h string) string { return pathutil.JoinKeepDotDot(h, "plugin-data") }

// Cron directory of a home
func CronDir(h string) string { return pathutil.JoinKeepDotDot(h, "cron") }

// Logs directory of a home
func LogsDir(h string) string { return pathutil.JoinKeepDotDot(h, "logs") }

// Cache directory of a home
func CacheDir(h string) string { return pathutil.JoinKeepDotDot(h, "cache") }

// Scratch directory a home hands to child processes as their temp dir
func ScratchDir(h string) string { return pathutil.JoinKeepDotDot(h, "cache", "scratch") }

// Pending approval directory of a home
func PendingDir(h string) string { return pathutil.JoinKeepDotDot(h, "pending") }

// Sessions export directory of a home
func SessionsDir(h string) string { return pathutil.JoinKeepDotDot(h, "sessions") }

// Locales directory of a home
func LocalesDir(h string) string { return pathutil.JoinKeepDotDot(h, "locales") }
