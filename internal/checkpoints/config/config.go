package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key = toolsconfig.Key + ".checkpoints"
	// Snapshots of the files the agent changes, which /rollback restores
	Enabled = Key + ".enabled"
	// Old snapshots cleaned up as the server starts, at most once per interval
	AutoPrune = Key + ".auto_prune"
	// Largest file in MB a snapshot includes
	MaxFileSize = Key + ".max_file_size_mb"
	// Snapshots kept for each project
	MaxSnapshots = Key + ".max_snapshots"
	// MB every snapshot together may take before the oldest of each project go
	MaxTotalSize = Key + ".max_total_size_mb"
	// Fewest hours between two automatic cleanups
	MinInterval = Key + ".min_interval_hours"
	// Days a project's snapshots are kept since it was last snapshotted
	Retention = Key + ".retention_days"
	// Seconds one git step of a snapshot may take
	GitTimeout = Key + ".git_timeout"
)
