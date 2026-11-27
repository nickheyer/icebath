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
		About:  "Snapshots of the files the agent changes, which /rollback restores, and how many of them are kept",
		Settings: []config.Setting{
			{Key: Enabled, Title: "File checkpoints", About: "Save a snapshot of the project before the agent writes, edits or deletes files, so /rollback can restore them",
				Default: false, Env: []string{"ICEBATH_TUI_CHECKPOINTS"}},
			{Key: AutoPrune, Title: "Auto-prune snapshots", About: "Delete old file snapshots automatically as the server starts, at most once per prune interval", Default: true},
			{Key: MaxFileSize, Title: "Max file size", About: "Largest single file in MB a snapshot includes; larger ones are left out of it", Default: 10},
			{Key: MaxSnapshots, Title: "Snapshots kept", About: "How many snapshots are kept per project", Default: 20},
			{Key: MaxTotalSize, Title: "Snapshot disk limit", About: "MB all snapshots may take before the oldest of each project are dropped; one per project is always kept",
				Default: 500},
			{Key: MinInterval, Title: "Prune interval", About: "Fewest hours between automatic snapshot cleanups", Default: 24 * time.Hour, Unit: time.Hour},
			{Key: Retention, Title: "Snapshot retention", About: "Days the snapshots of a project not snapshotted since are kept before cleanup removes them",
				Default: 7 * 24 * time.Hour, Unit: 24 * time.Hour},
			{Key: GitTimeout, Title: "Snapshot git timeout", About: "Seconds one git step of a snapshot may take, from 10 to 60", Default: 30 * time.Second,
				Env: []string{"ICEBATH_CHECKPOINT_TIMEOUT"}},
		},
	}})
}
