package cron

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const (
	quickSnapshotsDir = "state-snapshots"
	cronJobsRel       = "cron/jobs.json"
)

// CronPromptRestore reports the degraded prompts restored from one pre update snapshot
type CronPromptRestore struct {
	Restored   bool
	Prompts    int
	JobIDs     []string
	SnapshotID string
	Profile    string
}

// ProfileSnapshot names the pre update snapshot taken for one profile
type ProfileSnapshot struct {
	Name, SnapshotID string
}

// Reader of a jobs.json document, the module attribute tests patch
var loadCronJobsDoc = loadCronJobsDocFile

// Parsed jobs.json document, nil when missing, unreadable, malformed or of the wrong shape
func loadCronJobsDocFile(path string) (any, error) {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("'utf-8' codec can't decode jobs document %s", path)
	}
	data, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return nil, nil
	}
	switch d := data.(type) {
	case *pyjson.Object:
		if _, ok := field(d, "jobs", []any{}).([]any); ok {
			return d, nil
		}
	case []any:
		return d, nil
	}
	return nil, nil
}

// Job entries of a jobs document, empty for any other shape
func cronJobsList(doc any) []any {
	switch d := doc.(type) {
	case []any:
		return d
	case *pyjson.Object:
		jobs, _ := field(d, "jobs", []any{}).([]any)
		return jobs
	}
	return nil
}

// Agent job whose prompt is empty or replaced by its name
func promptDegraded(job *pyjson.Object) bool {
	if pyrepr.Truthy(field(job, "no_agent", nil)) {
		return false
	}
	prompt, ok := field(job, "prompt", nil).(string)
	if !ok || pystr.Strip(prompt) == "" {
		return true
	}
	return pystr.Strip(prompt) == pystr.Strip(pyrepr.Str(field(job, "name", "")))
}

// Non blank snapshot prompts keyed by job id
func snapshotPromptsByID(snapDoc any) map[string]string {
	prompts := map[string]string{}
	for _, item := range cronJobsList(snapDoc) {
		job, ok := item.(*pyjson.Object)
		if !ok {
			continue
		}
		if prompt, ok := field(job, "prompt", nil).(string); ok && pystr.Strip(prompt) != "" {
			prompts[pyrepr.Str(field(job, "id", ""))] = prompt
		}
	}
	return prompts
}

// Ids of degraded jobs a snapshot prompt can restore, writing the prompts when apply is set
func restoreDegradedPrompts(jobs []any, snapPrompts map[string]string, apply bool) []string {
	var restored []string
	for _, item := range jobs {
		job, ok := item.(*pyjson.Object)
		if !ok || !promptDegraded(job) {
			continue
		}
		jobID := pyrepr.Str(field(job, "id", ""))
		snapPrompt, ok := snapPrompts[jobID]
		if !ok {
			continue
		}
		if apply {
			job.Set("prompt", snapPrompt)
		}
		restored = append(restored, jobID)
	}
	return restored
}

// Restores degraded prompts against the live jobs read under the cron jobs lock
func restoreUnderCronLock(h string, snapPrompts map[string]string) ([]string, error) {
	var restored []string
	err := openCronStore(h, nil).updateRawJobs(func(jobs []any) (bool, error) {
		restored = restoreDegradedPrompts(jobs, snapPrompts, true)
		return len(restored) > 0, nil
	})
	return restored, err
}

// Restores job prompts an update replaced with the job name from the pre update snapshot, nil when none
func RestoreCronPromptFieldsIfDegraded(ctx context.Context, h, snapshotID string) (*CronPromptRestore, error) {
	if snapshotID == "" {
		return nil, nil
	}
	if h == "" {
		h = home.Dir(ctx)
	}
	livePath := filepath.Join(h, filepath.FromSlash(cronJobsRel))
	snapPath := filepath.Join(h, quickSnapshotsDir, snapshotID, filepath.FromSlash(cronJobsRel))
	snapDoc, err := loadCronJobsDoc(snapPath)
	if err != nil || snapDoc == nil {
		return nil, err
	}
	snapPrompts := snapshotPromptsByID(snapDoc)
	liveDoc, err := loadCronJobsDoc(livePath)
	if err != nil {
		return nil, err
	}
	if liveDoc == nil || len(restoreDegradedPrompts(cronJobsList(liveDoc), snapPrompts, false)) == 0 {
		return nil, nil
	}
	restoredIDs, err := restoreUnderCronLock(h, snapPrompts)
	if err != nil {
		slog.Error(fmt.Sprintf("Cron job prompts were degraded during update but auto-restore failed: %s", err))
		return nil, nil
	}
	if len(restoredIDs) == 0 {
		return nil, nil
	}
	slog.Warn(fmt.Sprintf("Restored %d cron job prompt(s) from pre-update snapshot %s — job(s) %s had their prompt replaced by the job name (#82990)",
		len(restoredIDs), snapshotID, strings.Join(restoredIDs, ", ")))
	return &CronPromptRestore{Restored: true, Prompts: len(restoredIDs), JobIDs: restoredIDs, SnapshotID: snapshotID}, nil
}

// Other profiles of this install as name and home pairs, the invoking home excluded
func siblingProfileHomes(invoking string) [][2]string {
	var homes [][2]string
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			if abs, err := filepath.Abs(r); err == nil {
				return abs
			}
		}
		abs, _ := filepath.Abs(p)
		return abs
	}
	inv := resolve(invoking)
	if def := profiles.DefaultHome(); isDir(def) && resolve(def) != inv {
		homes = append(homes, [2]string{"default", def})
	}
	root := profiles.ProfilesRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug(fmt.Sprintf("Sibling profile enumeration failed: %s", err))
		}
		return homes
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if isDir(p) && e.Name() != "default" && home.ProfileIDRe.MatchString(e.Name()) && resolve(p) != inv {
			homes = append(homes, [2]string{e.Name(), p})
		}
	}
	return homes
}

// Restores degraded prompts in every sibling profile from its own pre update snapshot
func RestoreCronPromptFieldsAllProfiles(ctx context.Context, snapshots []ProfileSnapshot, invokingHome string) []CronPromptRestore {
	restored := []CronPromptRestore{}
	if len(snapshots) == 0 {
		return restored
	}
	if invokingHome == "" {
		invokingHome = home.Dir(ctx)
	}
	byName := map[string]string{}
	for _, pair := range siblingProfileHomes(invokingHome) {
		byName[pair[0]] = pair[1]
	}
	for _, snap := range snapshots {
		profileHome, ok := byName[snap.Name]
		if !ok {
			continue
		}
		result, err := RestoreCronPromptFieldsIfDegraded(ctx, profileHome, snap.SnapshotID)
		if err != nil {
			slog.Debug(fmt.Sprintf("Cron prompt-field restore check for profile %s failed: %s", snap.Name, err))
			continue
		}
		if result != nil {
			result.Profile = snap.Name
			restored = append(restored, *result)
		}
	}
	return restored
}
