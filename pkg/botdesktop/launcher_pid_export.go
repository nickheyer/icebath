package botdesktop

import "github.com/nickheyer/icebath/pkg/tools"

// Live launcher pid of the profile screen, ok false when launcher.pid names no live launcher
func LauncherPid(env *tools.Env) (int, bool) { return launcherPid(env) }
