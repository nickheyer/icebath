package tuihost

import "github.com/nickheyer/icebath/pkg/tools"

// Drops every cached root of the one process wide root cache
func InvalidateGitRoots() { tools.GitProbeInvalidate() }
