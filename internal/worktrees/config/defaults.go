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
		About:  "Git worktrees that keep sessions apart from your checkout",
		Order:  60,
		Settings: []config.Setting{
			{Key: Own, Title: "Separate tree per launch", About: "Start each launch of icebath in a new git worktree, as icebath -w does.", Default: false,
				Apply: config.Restart},
			{Key: Sync, Title: "Fetch before branching", About: "Fetch the upstream branch before a new worktree is made from it.",
				Help: "When off, new worktrees branch from your local HEAD.", Default: true},
			{Key: ArchiveDirty, Title: "Archive unsaved work", About: "Save the uncommitted changes and unpushed commits of an idle worktree before removing it.",
				Help: "The work is kept in an archive that icebath worktree restore brings back.", Default: true},
			{Key: ArchiveRetention, Title: "Keep archives for", About: "How long the archive of a removed worktree is kept.", Default: 30 * 24 * time.Hour,
				Unit: 24 * time.Hour, Zero: "Forever"},
			{Key: ArchiveMaxFile, Title: "Largest archived file", About: "The largest untracked file that a worktree archive takes in.",
				Help: "A worktree that holds a larger file is kept instead of removed.", Default: 50, Zero: "No limit", Counts: "MB", Least: config.Num(0),
				Advanced: true},
			{Key: MaxTotal, Title: "Disk limit", About: "How much disk all worktrees may take before the oldest idle ones are archived and removed.",
				Default: 0, Zero: "No limit", Counts: "GB", Least: config.Num(0), Advanced: true},
		},
	}})
}
