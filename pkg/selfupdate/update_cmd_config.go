package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Snapshot id the pre update backup took for each sibling profile, by profile name
var lastSiblingSnapshots = pyjson.NewObject()

// Config calls of the migration check, the facade functions hermes tests patch
var (
	configCheckVersion     = config.CheckConfigVersion
	configMigrate          = config.MigrateConfig
	configMissingEnvVars   = config.GetMissingEnvVars
	configMissingFields    = config.GetMissingConfigFields
	siblingProfileHomes    = backup.SiblingProfileHomes
	configMigrationSibling = migrateSiblingProfileConfigs
)

// Retired hook of an old updater, stops the run for the relaunch that migrates in fresh code
func reloadConfigModules(ctx context.Context) error { return pm.StopForRelaunchIncomplete(ctx) }

// Retired hook of an old updater, stops the run for the relaunch that checks the config in fresh code
func runConfigCheckFresh(ctx context.Context) error { return pm.StopForRelaunchIncomplete(ctx) }

// Retired hook of an old updater, stops the run for the relaunch that migrates in fresh code
func runMigrateConfigFresh(ctx context.Context, interactive, quiet bool) error {
	return pm.StopForRelaunchIncomplete(ctx)
}

// One sibling profile whose config format a quiet migration raised
type siblingMigration struct {
	Name     string
	From, To int
}

// Migrates every sibling profile's config without prompting, notes receiving what quiet mode muted
func migrateSiblingProfileConfigs(ctx context.Context, notes map[string][]string) ([]siblingMigration, error) {
	migrated := []siblingMigration{}
	err := bestEffort(ctx, "Sibling profile enumeration failed: %s", func() error {
		for _, sibling := range siblingProfileHomes(home.Process()) {
			name, profileHome := sibling[0], sibling[1]
			if info, err := os.Stat(filepath.Join(profileHome, "config.yaml")); err != nil || !info.Mode().IsRegular() {
				continue
			}
			scoped := home.WithOverride(ctx, profileHome)
			err := func() error {
				currentVer, latestVer, err := configCheckVersion(scoped, profileHome, true)
				if err != nil || currentVer >= latestVer {
					return err
				}
				results, err := configMigrate(scoped, profileHome, false, true)
				if err != nil {
					return err
				}
				afterVer, _, err := configCheckVersion(scoped, profileHome, true)
				if err != nil || afterVer <= currentVer {
					return err
				}
				migrated = append(migrated, siblingMigration{Name: name, From: currentVer, To: afterVer})
				if notes == nil {
					return nil
				}
				fields, err := configMissingFields(scoped, profileHome, nil)
				if err != nil {
					return err
				}
				newKeys := map[string]bool{}
				for _, field := range fields {
					newKeys[field.Key] = true
				}
				lines := []string{}
				for _, note := range results.ConfigAdded {
					if !newKeys[note] {
						lines = append(lines, "ℹ "+note)
					}
				}
				for _, warning := range results.Warnings {
					lines = append(lines, "⚠️  "+warning)
				}
				notes[name] = lines
				return nil
			}()
			if err != nil {
				if isUpdateExit(err) {
					return err
				}
				updateLog.Debug(fmt.Sprintf("Config migration for profile %s failed: %s", name, launchErrorText(err)))
			}
		}
		return nil
	})
	return migrated, err
}

// Line reporting cron jobs a restore put back
func cronRestoreLine(r *pyjson.Object) string {
	return fmt.Sprintf("cron/jobs.json lost jobs during this update — restored %s job(s) from pre-update snapshot %s.",
		pyrepr.Str(r.Vals["job_count"]), pyrepr.Str(r.Vals["snapshot_id"]))
}

// Line reporting agent job prompts a restore put back
func cronPromptRestoreLine(r cron.CronPromptRestore) string {
	return fmt.Sprintf("cron/jobs.json had agent-job prompt(s) replaced by the job name during this "+
		"update — restored %d prompt(s) from pre-update snapshot %s.", r.Prompts, r.SnapshotID)
}

// Line reporting protected model settings a restore put back
func configRestoreLine(r *pyjson.Object) string {
	keys, _ := r.Vals["keys"].([]any)
	names := make([]string, len(keys))
	for i, key := range keys {
		names[i] = pyrepr.Str(key)
	}
	return fmt.Sprintf("config.yaml user model settings were rewritten during this update — restored "+
		"%s from pre-update snapshot %s.", strings.Join(names, ", "), pyrepr.Str(r.Vals["snapshot_id"]))
}

// Restores cron jobs, cron prompts and model settings an update emptied or rewrote, for every profile
func restoreSnapshotSafetyNets(ctx context.Context, w io.Writer, preUpdateSnapshotID *string) error {
	h := home.Dir(ctx)
	snapshotID := ""
	if preUpdateSnapshotID != nil {
		snapshotID = *preUpdateSnapshotID
	}
	warn := func(line string) {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  ⚠️  %s\n", line)
	}
	steps := []struct {
		message string
		run     func() error
	}{
		{"Cron jobs auto-restore check failed: %s", func() error {
			restored, err := backup.RestoreCronJobsIfEmptied(snapshotID, h)
			if err == nil && restored != nil && restored.Len() > 0 {
				warn(cronRestoreLine(restored))
			}
			return err
		}},
		{"Cron prompt-field auto-restore check failed: %s", func() error {
			restored, err := cron.RestoreCronPromptFieldsIfDegraded(ctx, h, snapshotID)
			if err == nil && restored != nil && restored.Restored {
				warn(cronPromptRestoreLine(*restored))
			}
			return err
		}},
		{"Config model-settings auto-restore check failed: %s", func() error {
			restored, err := backup.RestoreConfigModelSettingsIfRewritten(ctx, snapshotID, h)
			if err == nil && restored != nil && restored.Len() > 0 {
				warn(configRestoreLine(restored))
			}
			return err
		}},
		{"Sibling cron auto-restore check failed: %s", func() error {
			for _, restored := range backup.RestoreCronJobsAllProfiles(lastSiblingSnapshots, h) {
				warn(fmt.Sprintf("Profile '%s': %s", pyrepr.Str(restored.Vals["profile"]), cronRestoreLine(restored)))
			}
			return nil
		}},
		{"Sibling cron prompt-field auto-restore check failed: %s", func() error {
			var snapshots []cron.ProfileSnapshot
			for _, name := range lastSiblingSnapshots.Keys {
				snapshots = append(snapshots, cron.ProfileSnapshot{Name: name, SnapshotID: pyrepr.Str(lastSiblingSnapshots.Vals[name])})
			}
			for _, restored := range cron.RestoreCronPromptFieldsAllProfiles(ctx, snapshots, h) {
				warn(fmt.Sprintf("Profile '%s': %s", restored.Profile, cronPromptRestoreLine(restored)))
			}
			return nil
		}},
		{"Sibling config auto-restore check failed: %s", func() error {
			for _, restored := range backup.RestoreConfigModelSettingsAllProfiles(ctx, lastSiblingSnapshots, h) {
				warn(fmt.Sprintf("Profile '%s': %s", pyrepr.Str(restored.Vals["profile"]), configRestoreLine(restored)))
			}
			return nil
		}},
	}
	for _, step := range steps {
		if err := bestEffort(ctx, step.message, step.run); err != nil {
			return err
		}
	}
	return nil
}

// The answer to configuring new options now, y under yes, the relayed reply, auto when unattended, else typed
func askConfigureNewOptions(ctx context.Context, w io.Writer, assumeYes, gatewayMode bool) (string, error) {
	if assumeYes {
		fmt.Fprintln(w, "  ℹ --yes: auto-applying config migration (skipping API-key prompts).")
		return "y", nil
	}
	if gatewayMode {
		answer, err := updateGatewayPrompt(ctx, w, "Would you like to configure new options now? [Y/n]", "n", 300.0)
		return strings.ToLower(pystr.Strip(answer)), err
	}
	if !(updateStdinIsTTY() && updateStdoutIsTTY(w)) {
		fmt.Fprintln(w, "  ℹ Non-interactive session — applying safe config migrations.")
		return "auto", nil
	}
	answer, err := updateInput("Would you like to configure them now? [Y/n]: ")
	switch {
	case err == nil:
		return strings.ToLower(pystr.Strip(answer)), nil
	case errors.Is(err, cliui.ErrEOF):
		return "n", nil
	case pm.ErrorClass(err) == "UnicodeDecodeError":
		fmt.Fprintln(w, "  ⚠ Could not read input (encoding issue). Skipping. Run 'icebath config migrate' manually to configure.")
		return "n", nil
	}
	return "", err
}

// Checks and applies config migrations for the active profile, then every sibling, then the snapshot nets
func checkAndApplyConfigMigration(ctx context.Context, w io.Writer, assumeYes, gatewayMode bool, preUpdateSnapshotID *string) error {
	h := home.Dir(ctx)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "→ Checking configuration for new options...")
	missingEnv, err := configMissingEnvVars(ctx, h, true)
	var missingConfig []config.MissingConfigField
	var currentVer, latestVer int
	if err == nil {
		missingConfig, err = configMissingFields(ctx, h, nil)
	}
	if err == nil {
		currentVer, latestVer, err = configCheckVersion(ctx, h, true)
	}
	if err != nil {
		if isUpdateExit(err) {
			return err
		}
		updateLog.Debug(fmt.Sprintf("Config check during update failed: %s", launchErrorText(err)))
		fmt.Fprintln(w, "  ⚠️  Could not check config version.")
		fmt.Fprintln(w, "     Run 'icebath config migrate' to check manually.")
		return nil
	}
	var owedError error
	hasNewOptions := len(missingEnv) > 0 || len(missingConfig) > 0
	switch {
	case !hasNewOptions && currentVer < latestVer:
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  ℹ Updating config format (v%d → v%d)…\n", currentVer, latestVer)
		results, err := configMigrate(ctx, h, false, true)
		if err != nil {
			if isUpdateExit(err) {
				return err
			}
			fmt.Fprintf(w, "  ⚠️  Config format update failed: %s\n", launchErrorText(err))
			fmt.Fprintln(w, "     Run 'icebath config migrate' to retry.")
			owedError = runtimeErr(fmt.Sprintf("config format v%d → v%d was not written: %s", currentVer, latestVer, launchErrorText(err)))
			break
		}
		fmt.Fprintln(w, "  ✓ Config format updated (no new settings to configure)")
		for _, note := range results.ConfigAdded {
			fmt.Fprintf(w, "  ℹ %s\n", note)
		}
		for _, warning := range results.Warnings {
			fmt.Fprintf(w, "  ⚠️  %s\n", warning)
		}
	case hasNewOptions || currentVer < latestVer:
		fmt.Fprintln(w)
		if len(missingEnv) > 0 {
			fmt.Fprintf(w, "  ⚠️  %d new required setting(s) need configuration\n", len(missingEnv))
			items := make([]any, len(missingEnv))
			for i, item := range missingEnv {
				items[i] = map[string]any{"name": item.Name, "description": item.Description}
			}
			printItems(w, items, "New settings", "name", "")
		}
		if len(missingConfig) > 0 {
			fmt.Fprintf(w, "  ℹ️  %d new config option(s) available\n", len(missingConfig))
			items := make([]any, len(missingConfig))
			for i, item := range missingConfig {
				items[i] = map[string]any{"key": item.Key, "description": item.Description}
			}
			printItems(w, items, "New options", "key", "")
		}
		fmt.Fprintln(w)
		response, err := askConfigureNewOptions(ctx, w, assumeYes, gatewayMode)
		if err != nil {
			return err
		}
		if response == "" || response == "y" || response == "yes" || response == "auto" {
			fmt.Fprintln(w)
			unattended := gatewayMode || assumeYes || response == "auto"
			results, err := configMigrate(ctx, h, !unattended, false)
			if err != nil {
				return err
			}
			if len(results.EnvAdded) > 0 || len(results.ConfigAdded) > 0 {
				fmt.Fprintln(w)
				fmt.Fprintln(w, "✓ Configuration updated!")
			}
			if unattended && len(missingEnv) > 0 {
				fmt.Fprintln(w, "  ℹ API keys require manual entry: icebath config migrate")
			}
		} else {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Skipped. Run 'icebath config migrate' later to configure.")
		}
	default:
		fmt.Fprintln(w, "  ✓ Configuration is up to date")
	}
	if err := bestEffort(ctx, "Sibling config migration failed: %s", func() error {
		siblingNotes := map[string][]string{}
		migrated, err := configMigrationSibling(ctx, siblingNotes)
		if err != nil {
			return err
		}
		for _, sibling := range migrated {
			fmt.Fprintf(w, "  ✓ Profile '%s': config format updated (v%d → v%d)\n", sibling.Name, sibling.From, sibling.To)
			for _, note := range siblingNotes[sibling.Name] {
				fmt.Fprintf(w, "      %s\n", note)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := restoreSnapshotSafetyNets(ctx, w, preUpdateSnapshotID); err != nil {
		return err
	}
	return owedError
}

// Prints up to eight named items under a label with their descriptions, then how many more there are
func printItems(w io.Writer, items []any, label, key, fallbackKey string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s:\n", label)
	shown := items
	if len(shown) > 8 {
		shown = shown[:8]
	}
	for _, it := range shown {
		name, desc := pyrepr.Str(it), ""
		if dict, ok := it.(map[string]any); ok {
			name = "?"
			if pyrepr.Truthy(dict[key]) {
				name = pyrepr.Str(dict[key])
			} else if fallbackKey != "" && pyrepr.Truthy(dict[fallbackKey]) {
				name = pyrepr.Str(dict[fallbackKey])
			}
			if pyrepr.Truthy(dict["description"]) {
				desc = pystr.Strip(pyrepr.Str(dict["description"]))
			}
		}
		if desc != "" {
			fmt.Fprintf(w, "      • %s — %s\n", name, desc)
		} else {
			fmt.Fprintf(w, "      • %s\n", name)
		}
	}
	if len(items) > len(shown) {
		fmt.Fprintf(w, "      … and %d more\n", len(items)-len(shown))
	}
}
