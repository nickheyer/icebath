//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Snapshot id the pre update backup took for each sibling profile, by profile name
var lastSiblingSnapshots = pyjson.NewObject()

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
