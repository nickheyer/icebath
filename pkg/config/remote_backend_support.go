package config

import "context"

// Schema version this build writes, the defaults stamp
func LatestConfigVersion() int { return latestConfigVersion() }

// Top level keys this build knows, the defaults plus the keys read outside them
func KnownTopLevelKeys() map[string]bool { return knownTopLevelKeys() }

// Runs every due migration quietly over the config document alone, the backend keeping the result in memory
func RunConfigOnlyMigrations(ctx context.Context, h string, current int) {
	RunMigrations(h, current, &MigrationResults{}, true, MigrationOptions{ConfigOnly: true}, migrationSources(ctx, h))
}
