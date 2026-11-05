package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Version reader and migration pass the Docker boot step runs, swapped by tests simulating failures
var (
	dockerReadStamp     = ReadConfigVersionStamp
	dockerMigrateConfig = MigrateConfig
)

const dockerBackupReason = "pre-docker-migrate"

type dockerBackup struct{ original, backup string }

// Snapshots each file into backups/config, reusing an identical existing snapshot
func dockerBackupExisting(paths ...string) ([]dockerBackup, error) {
	var out []dockerBackup
	for _, path := range paths {
		dest, err := BackupConfig(path, dockerBackupReason, DefaultBackupKeep)
		if err != nil {
			return nil, err
		}
		if dest == "" {
			if existing := ListConfigBackups(path, dockerBackupReason); len(existing) > 0 {
				dest = existing[0]
			}
		}
		if dest != "" {
			out = append(out, dockerBackup{path, dest})
		}
	}
	return out, nil
}

// Copies every backup still on disk over its original with mode and times, returning the restored paths
func dockerRestoreBackups(backups []dockerBackup) ([]string, error) {
	var restored []string
	for _, b := range backups {
		st, err := os.Stat(b.backup)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(b.backup)
		if err != nil {
			return restored, err
		}
		if err := os.WriteFile(b.original, data, st.Mode().Perm()); err != nil {
			return restored, err
		}
		if err := os.Chmod(b.original, st.Mode().Perm()); err != nil {
			return restored, err
		}
		if err := os.Chtimes(b.original, st.ModTime(), st.ModTime()); err != nil {
			return restored, err
		}
		restored = append(restored, b.original)
	}
	return restored, nil
}

func envVarEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Migrates a container volume config at boot, backing up first and restoring when the pass fails or stalls
func DockerConfigMigrate(ctx context.Context, h string) error {
	if envVarEnabled("ICEBATH_SKIP_CONFIG_MIGRATION") {
		fmt.Fprintln(FacadeStdout, "[config-migrate] ICEBATH_SKIP_CONFIG_MIGRATION is set; skipping config migration")
		return nil
	}
	if _, err := StampMissingConfig(h, facadeStampHooks()); err != nil {
		return err
	}
	stamp, latest, err := dockerReadStamp(h, true)
	var invalid *InvalidUserConfigError
	if errors.As(err, &invalid) {
		fmt.Fprintf(FacadeNoticeOut, "[config-migrate] WARNING: %v; leaving config.yaml untouched\n", err)
		return nil
	}
	if err != nil {
		return err
	}
	current := 0
	if stamp != nil {
		current = *stamp
	}
	if current >= latest {
		return nil
	}
	if stamp != nil && current < SupportFloorVersion {
		fmt.Fprintf(FacadeNoticeOut, "[config-migrate] WARNING: %s\n", SupportFloorMessage(h))
		return nil
	}
	backups, err := dockerBackupExisting(home.ConfigPath(h), home.EnvPath(h))
	if err != nil {
		return err
	}
	backupText := "none"
	if len(backups) > 0 {
		names := make([]string, len(backups))
		for i, b := range backups {
			names[i] = b.backup
		}
		backupText = strings.Join(names, ", ")
	}
	fmt.Fprintf(FacadeStdout, "[config-migrate] Migrating config schema %d -> %d; backups: %s\n", current, latest, backupText)
	if _, err := dockerMigrateConfig(ctx, h, false, false); err != nil {
		restored, rerr := dockerRestoreBackups(backups)
		if len(restored) > 0 {
			fmt.Fprintln(FacadeStdout, "[config-migrate] Migration failed; restored "+strings.Join(restored, ", "))
		}
		return errors.Join(err, rerr)
	}
	post, _, err := CheckConfigVersion(h, false)
	if err != nil {
		return err
	}
	if post < latest {
		restored, rerr := dockerRestoreBackups(backups)
		restoredText := "none"
		if len(restored) > 0 {
			restoredText = strings.Join(restored, ", ")
		}
		return errors.Join(fmt.Errorf("migration did not advance config version to %d (still %d); restored: %s", latest, post, restoredText), rerr)
	}
	return nil
}

// Runs the boot migration as the container entrypoint step, an error printed and returned as status 1
func DockerConfigMigrateMain(ctx context.Context, h string) int {
	if err := DockerConfigMigrate(ctx, h); err != nil {
		fmt.Fprintf(FacadeNoticeOut, "[config-migrate] ERROR: %v\n", err)
		return 1
	}
	return 0
}
