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
		Title:  "Checkpoints",
		About:  "Saved states of your files that /rollback restores",
		Order:  50,
		Settings: []config.Setting{
			{Key: Enabled, Title: "Save checkpoints", About: "Save a checkpoint of the project before the agent changes files, for /rollback to restore.",
				Help: "Starting icebath with --checkpoints turns this on for that run.", Default: false, Env: []string{"ICEBATH_TUI_CHECKPOINTS"}},
			{Key: MaxSnapshots, Title: "Kept per project", About: "How many checkpoints are kept for each project.", Default: 20, Counts: "checkpoints",
				Least: config.Num(1)},
			{Key: Retention, Title: "Keep for", About: "How long the checkpoints of a project are kept after the last one was saved.", Default: 7 * 24 * time.Hour,
				Unit: 24 * time.Hour, Zero: "Forever", Apply: config.Restart},
			{Key: AutoPrune, Title: "Clean up old checkpoints", About: "Delete old checkpoints automatically when icebath starts.", Default: true,
				Apply: config.Restart},
			{Key: MaxTotalSize, Title: "Disk limit", About: "How much disk all checkpoints may take before the oldest of each project go.",
				Help: "The newest checkpoint of each project is always kept.", Default: 500, Zero: "No limit", Counts: "MB", Least: config.Num(0),
				Advanced: true},
			{Key: MaxFileSize, Title: "Largest file", About: "The largest file that a checkpoint takes in.", Help: "Larger files are left out of checkpoints.",
				Default: 10, Zero: "No limit", Counts: "MB", Least: config.Num(0), Advanced: true},
			{Key: MinInterval, Title: "Cleanup interval", About: "How long to wait between two automatic cleanups.", Default: 24 * time.Hour, Unit: time.Hour,
				Zero: "Every start", Apply: config.Restart, Advanced: true},
			{Key: GitTimeout, Title: "Git step time limit", About: "How long one git step of a checkpoint may take.",
				Help: "Held between 10 and 60 seconds.", Default: 30 * time.Second, Env: []string{"ICEBATH_CHECKPOINT_TIMEOUT"}, Advanced: true},
		},
	}})
}
