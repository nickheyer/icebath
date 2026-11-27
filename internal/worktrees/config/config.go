package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key = toolsconfig.Key + ".worktrees"
	// Idle trees with uncommitted changes or unpushed commits are archived before they are removed
	ArchiveDirty = Key + ".archive_dirty"
	// Largest untracked file in MB an archive takes
	ArchiveMaxFile = Key + ".archive_max_file_mb"
	// Days the archive of a removed tree is kept
	ArchiveRetention = Key + ".archive_retention_days"
	// Days the dependency installs of a checkout that is gone are kept
	InstallStateRetention = Key + ".install_state_retention_days"
	// GB every tree together may take before the idle ones are archived
	MaxTotal = Key + ".max_total_gb"
	// Every launch of the app runs its sessions in a git worktree of its own
	Own = Key + ".own_tree"
	// A new tree is branched from the upstream branch, fetched first
	Sync = Key + ".fetch_base"
)
