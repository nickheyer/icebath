package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Worktrees",
		About:  "Git worktrees sessions work in apart from your checkout, and how the idle ones are archived and reclaimed",
		Settings: []config.Setting{
			{Key: Own, Title: "Own work tree", About: "Start each launch of icebath in a git worktree of its own, as icebath -w does", Default: false},
			{Key: Sync, Title: "Fetch before branching", About: "Fetch the upstream branch before making a new worktree; off branches from your local HEAD", Default: true},
			{Key: ArchiveDirty, Title: "Archive dirty trees", About: "Save the uncommitted changes and unpushed commits of an idle worktree to an archive ref before removing it",
				Default: true},
			{Key: ArchiveMaxFile, Title: "Archive file limit", About: "Largest untracked file in MB a worktree archive includes; a tree holding a larger one is kept",
				Default: 50},
			{Key: ArchiveRetention, Title: "Archive retention", About: "Days a removed worktree's archive is kept; 0 keeps them for good", Default: 30 * 24 * time.Hour,
				Unit: 24 * time.Hour},
			{Key: InstallStateRetention, Title: "Install cache retention", About: "Days the dependency installs of a checkout no longer there are kept; zero or less turns cleanup off",
				Default: 14 * 24 * time.Hour, Unit: 24 * time.Hour},
			{Key: MaxTotal, Title: "Worktree disk limit", About: "GB all worktrees may take before the oldest idle ones are archived and removed; 0 means no limit",
				Default: 0},
		},
	}})
}
