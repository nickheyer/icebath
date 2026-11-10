package memory

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/skills"
)

const (
	curatorBackupDefaultKeep = 2
	curatorCronJobsFilename  = "cron-jobs.json"
	curatorArchiveName       = "skills.tar.gz"
	curatorStagingPrefix     = ".rollback-staging-"
	curatorUnrestoredPrefix  = ".rollback-unrestored-"
)

var curatorExcludeTopLevel = map[string]bool{".curator_backups": true, ".hub": true, ".locks": true, ".git": true, ".archive": true, ".curator_ledger.jsonl": true}

var curatorIDRE = sync.OnceValue(func() *regexp2.Regexp {
	re, err := pyre.Compile(`^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z(-\d{2})?$`)
	if err != nil {
		panic(err)
	}
	return re
})

func curatorBackupLog() *slog.Logger { return slog.Default().With("logger", "agent.curator_backup") }

func curatorIDMatch(name string) bool {
	ok, err := curatorIDRE().MatchString(name)
	return err == nil && ok
}

// Excluded names anywhere in the member path plus transient directories among its directory parts
func curatorExcludedMember(name string, isDir bool) bool {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	dirParts := parts
	if !isDir {
		dirParts = parts[:len(parts)-1]
	}
	for _, p := range parts {
		if curatorExcludeTopLevel[p] {
			return true
		}
	}
	for _, p := range dirParts {
		if skills.TransientDirs[p] {
			return true
		}
	}
	return false
}

func curatorSkillsDir(ctx context.Context) string { return filepath.Join(home.Dir(ctx), "skills") }
func curatorBackupsDir(ctx context.Context) string {
	return filepath.Join(curatorSkillsDir(ctx), ".curator_backups")
}

// Jobs list of a jobs.json payload, the object form or a bare list
func curatorJobsList(parsed any) ([]any, bool) {
	if curatorIsDict(parsed) {
		parsed, _ = curatorGet(parsed, "jobs")
	}
	list, ok := parsed.([]any)
	return list, ok
}

// Text of a file decoded as utf-8-sig, an error for invalid UTF-8
func curatorReadUTF8Sig(p string) (string, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("'utf-8' codec can't decode %s", p)
	}
	return strings.TrimPrefix(string(raw), "\ufeff"), nil
}

// Copies the live cron jobs.json into the snapshot, reporting why when it could not
func curatorBackupCronJobsInto(ctx context.Context, dest string) (*pyjson.Object, error) {
	src := filepath.Join(home.Dir(ctx), "cron", "jobs.json")
	info := curatorEntry("backed_up", false, "jobs_count", 0)
	if _, err := os.Stat(src); err != nil {
		info.Set("reason", "no cron/jobs.json present")
		return info, nil
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		curatorBackupLog().Debug(fmt.Sprintf("Failed to read cron/jobs.json for backup: %v", err))
		info.Set("reason", fmt.Sprintf("read error: %v", err))
		return info, nil
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("'utf-8' codec can't decode %s", src)
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	if parsed, err := pyjson.Loads(text); err == nil {
		jobs, _ := curatorJobsList(parsed)
		info.Set("jobs_count", len(jobs))
	} else {
		info.Set("parse_warning", "jobs.json was not valid JSON at snapshot time")
	}
	if err := os.WriteFile(filepath.Join(dest, curatorCronJobsFilename), []byte(text), 0o666); err != nil {
		curatorBackupLog().Debug(fmt.Sprintf("Failed to write cron backup file: %v", err))
		info.Set("reason", fmt.Sprintf("write error: %v", err))
		return info, nil
	}
	info.Set("backed_up", true)
	return info, nil
}

// Filesystem safe UTC id like 2026-05-01T13-05-42Z
func curatorUTCID(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	return now.UTC().Format("2006-01-02T15-04-05") + "Z"
}

// curator.backup section of the config, replaced by tests
var curatorBackupConfig = func(ctx context.Context) map[string]any {
	return curatorConfigSection(ctx, "curator backup", curatorBackupLog(), "curator", "backup")
}

// Snapshot id clock, snapshot entry, move and rename the rollback and curator run reach, replaced by tests
var (
	curatorUTCIDNow       = curatorUTCID
	curatorSnapshotSkills = SnapshotSkills
	curatorMove           = pyfs.Move
	curatorRename         = os.Rename
)

// Snapshot before mutating passes, on unless curator.backup.enabled is false
func CuratorBackupEnabled(ctx context.Context) bool {
	v, ok := curatorBackupConfig(ctx)["enabled"]
	if !ok {
		v = true
	}
	return pyrepr.Truthy(v)
}

// Snapshots kept by the prune, at least one
func CuratorBackupKeep(ctx context.Context) int {
	v, ok := curatorBackupConfig(ctx)["keep"]
	if !ok {
		v = curatorBackupDefaultKeep
	}
	n, err := pyint.Of(v)
	if err != nil {
		return curatorBackupDefaultKeep
	}
	return max(1, curatorBigToInt(n))
}

// SKILL.md files under base that skill scanners would not skip
func curatorCountSkillFiles(base string) int {
	count := 0
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != base && d.Name() == "SKILL.md" && !skills.IsExcludedSkillPath(p, "") {
			count++
		}
		return nil
	})
	if err != nil {
		return 0
	}
	return count
}

// Writes manifest.json describing the snapshot archive and its cron capture
func curatorWriteManifest(dest, reason, archive string, skillsCounted int, cronInfo *pyjson.Object) error {
	cron := curatorEntry("backed_up", pyrepr.Truthy(curatorGetOr(cronInfo, "backed_up", false)), "jobs_count", curatorCount(cronInfo, "jobs_count"))
	if !pyrepr.Truthy(curatorGetOr(cronInfo, "backed_up", nil)) {
		cron.Set("reason", curatorGetOr(cronInfo, "reason", "not captured"))
	}
	if w := curatorGetOr(cronInfo, "parse_warning", nil); pyrepr.Truthy(w) {
		cron.Set("parse_warning", w)
	}
	st, err := os.Stat(archive)
	if err != nil {
		return err
	}
	manifest := curatorEntry("id", filepath.Base(dest), "reason", reason, "created_at", pydatetime.ISOFormatUTC(time.Now()), "archive", filepath.Base(archive),
		"archive_bytes", st.Size(), "skill_files", skillsCounted, "cron_jobs", cron)
	text, err := pyjson.Dumps(manifest, pyjson.Indent(2), pyjson.SortKeys(true))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "manifest.json"), []byte(text), 0o666)
}

func curatorMkdir(p, what string, existOK bool) bool {
	var err error
	if existOK {
		err = os.MkdirAll(p, 0o777)
	} else if err = os.MkdirAll(filepath.Dir(p), 0o777); err == nil {
		err = os.Mkdir(p, 0o777)
	}
	if err != nil {
		curatorBackupLog().Debug(fmt.Sprintf("Failed to create %s %s: %v", what, p, err))
		return false
	}
	return true
}

// Writes skills.tar.gz of every non excluded top level entry with the nested exclude filter
func curatorWriteArchive(skillsDir, archive string) error {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return err
	}
	f, err := os.Create(archive)
	if err != nil {
		return err
	}
	gz, err := gzip.NewWriterLevel(f, 6)
	if err != nil {
		f.Close()
		return err
	}
	gz.Name = strings.TrimSuffix(filepath.Base(archive), ".gz")
	gz.ModTime = time.Now()
	tw := tar.NewWriter(gz)
	adder := &pyfs.TarAdder{W: tw}
	filter := func(h *tar.Header) *tar.Header {
		if curatorExcludedMember(h.Name, h.Typeflag == tar.TypeDir) {
			return nil
		}
		return h
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		if curatorExcludeTopLevel[n] {
			continue
		}
		if err = adder.Add(filepath.Join(skillsDir, n), n, true, filter); err != nil {
			break
		}
	}
	for _, closeErr := range []error{tw.Close(), gz.Close(), f.Close()} {
		if err == nil {
			err = closeErr
		}
	}
	return err
}

// Takes a tar.gz snapshot of the skills tree and prunes old ones, empty when skipped or failed
func SnapshotSkills(ctx context.Context, reason string, protect map[string]bool) (string, bool, error) {
	if !CuratorBackupEnabled(ctx) {
		curatorBackupLog().Debug("Curator backup disabled by config; skipping snapshot")
		return "", false, nil
	}
	skillsDir, backups := curatorSkillsDir(ctx), curatorBackupsDir(ctx)
	if _, err := os.Stat(skillsDir); err != nil {
		curatorBackupLog().Debug("No ~/.icebath/skills/ directory — nothing to back up")
		return "", false, nil
	}
	if !curatorMkdir(backups, "backups dir", true) {
		return "", false, nil
	}
	baseID := curatorUTCIDNow(time.Time{})
	snapID := baseID
	for n := 1; curatorExists(filepath.Join(backups, snapID)); n++ {
		snapID = fmt.Sprintf("%s-%02d", baseID, n)
	}
	dest := filepath.Join(backups, snapID)
	if !curatorMkdir(dest, "snapshot dir", false) {
		return "", false, nil
	}
	archive := filepath.Join(dest, curatorArchiveName)
	err := curatorWriteArchive(skillsDir, archive)
	if err == nil {
		var cronInfo *pyjson.Object
		if cronInfo, err = curatorBackupCronJobsInto(ctx, dest); err != nil {
			os.RemoveAll(dest)
			return "", false, err
		}
		err = curatorWriteManifest(dest, reason, archive, curatorCountSkillFiles(skillsDir), cronInfo)
	}
	if err != nil {
		curatorBackupLog().Debug(fmt.Sprintf("Curator snapshot failed: %v", err))
		os.RemoveAll(dest)
		return "", false, nil
	}
	keep := map[string]bool{snapID: true}
	for id := range protect {
		keep[id] = true
	}
	curatorPruneOld(ctx, CuratorBackupKeep(ctx), keep)
	curatorBackupLog().Info(fmt.Sprintf("Curator snapshot created: %s (%s)", snapID, reason))
	return dest, true, nil
}

// Applies curator.backup.keep without taking a snapshot
func PruneOldSnapshots(ctx context.Context) []string {
	return curatorPruneOld(ctx, CuratorBackupKeep(ctx), nil)
}

// Deletes regular snapshots past the newest keep and stale staging dirs, returning the pruned ids
func curatorPruneOld(ctx context.Context, keep int, protect map[string]bool) []string {
	backups := curatorBackupsDir(ctx)
	entries, err := os.ReadDir(backups)
	if err != nil {
		return []string{}
	}
	var dirs, ids []string
	for _, e := range entries {
		if st, err := os.Stat(filepath.Join(backups, e.Name())); err == nil && st.IsDir() {
			dirs = append(dirs, e.Name())
			if curatorIDMatch(e.Name()) {
				ids = append(ids, e.Name())
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	type doom struct{ name, what string }
	var doomed []doom
	if keep < len(ids) {
		for _, id := range ids[max(keep, 0):] {
			if !protect[id] {
				doomed = append(doomed, doom{id, "prune"})
			}
		}
	}
	for _, d := range dirs {
		if strings.HasPrefix(d, curatorStagingPrefix) {
			doomed = append(doomed, doom{d, "clean stale staging dir"})
		}
	}
	deleted := []string{}
	for _, d := range doomed {
		p := filepath.Join(backups, d.name)
		if err := pyfs.RmTree(p, nil); err != nil {
			curatorBackupLog().Debug(fmt.Sprintf("Failed to %s %s: %v", d.what, p, err))
			continue
		}
		if d.what == "prune" {
			deleted = append(deleted, d.name)
		}
	}
	return deleted
}

// Decoded manifest.json of a snapshot, empty when missing or malformed
func curatorReadManifest(dir string) (any, error) {
	text, err := curatorReadUTF8Sig(filepath.Join(dir, "manifest.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "codec") {
			return pyjson.NewObject(), nil
		}
		return nil, err
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return pyjson.NewObject(), nil
	}
	return v, nil
}

func curatorIsRestorable(dir string) bool {
	st, err := os.Stat(dir)
	return err == nil && st.IsDir() && curatorIDMatch(filepath.Base(dir)) && curatorExists(filepath.Join(dir, curatorArchiveName))
}

// Restorable snapshot dirs, newest first
func curatorRestorableSnapshots(ctx context.Context) []string {
	backups := curatorBackupsDir(ctx)
	entries, err := os.ReadDir(backups)
	if err != nil {
		return nil
	}
	var out []string
	for i := len(entries) - 1; i >= 0; i-- {
		if p := filepath.Join(backups, entries[i].Name()); curatorIsRestorable(p) {
			out = append(out, p)
		}
	}
	return out
}

// Manifests of every restorable snapshot, newest first
func ListCuratorBackups(ctx context.Context) ([]*pyjson.Object, error) {
	out := []*pyjson.Object{}
	for _, child := range curatorRestorableSnapshots(ctx) {
		mf := curatorEntry("id", filepath.Base(child), "path", child)
		manifest, err := curatorReadManifest(child)
		if err != nil {
			return nil, err
		}
		obj, ok := manifest.(*pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("'%s' object is not a mapping", pyrepr.TypeName(manifest))
		}
		for _, k := range obj.Keys {
			mf.Set(k, obj.Vals[k])
		}
		if _, has := mf.Get("archive_bytes"); !has {
			if st, err := os.Stat(filepath.Join(child, curatorArchiveName)); err == nil {
				mf.Set("archive_bytes", st.Size())
			} else {
				mf.Set("archive_bytes", 0)
			}
		}
		out = append(out, mf)
	}
	return out, nil
}

// Requested snapshot or the newest, false when nothing matches
func curatorResolveBackup(ctx context.Context, id string) (string, bool) {
	if id != "" {
		target := filepath.Join(curatorBackupsDir(ctx), id)
		return target, curatorIDMatch(id) && curatorIsRestorable(target)
	}
	snaps := curatorRestorableSnapshots(ctx)
	if len(snaps) == 0 {
		return "", false
	}
	return snaps[0], true
}

func curatorSetOrDelete(job any, key string, value any) {
	switch m := job.(type) {
	case *pyjson.Object:
		if value == nil {
			m.Delete(key)
		} else {
			m.Set(key, value)
		}
	case map[string]any:
		if value == nil {
			delete(m, key)
		} else {
			m[key] = value
		}
	}
}

// Restores backed up skills and skill fields onto cron jobs that still exist, under the cron jobs lock
func curatorRestoreCronSkillLinks(ctx context.Context, snapDir string) *pyjson.Object {
	report := curatorEntry("attempted", false, "restored", []any{}, "skipped_missing", []any{}, "unchanged", 0, "error", nil)
	backupFile := filepath.Join(snapDir, curatorCronJobsFilename)
	if !curatorExists(backupFile) {
		report.Set("error", "snapshot has no "+curatorCronJobsFilename)
		return report
	}
	text, err := curatorReadUTF8Sig(backupFile)
	var parsed any
	if err == nil {
		parsed, err = pyjson.Loads(text)
	}
	if err != nil {
		report.Set("error", fmt.Sprintf("failed to load backed-up jobs: %v", err))
		return report
	}
	backupJobs, ok := curatorJobsList(parsed)
	if !ok {
		report.Set("error", "backed-up cron-jobs.json has no jobs list")
		return report
	}
	byID := pyjson.NewObject()
	for _, job := range backupJobs {
		id, ok := curatorGetOr(job, "id", nil).(string)
		if !curatorIsDict(job) || !ok || id == "" {
			continue
		}
		name := curatorGetOr(job, "name", nil)
		if !pyrepr.Truthy(name) {
			name = id
		}
		byID.Set(id, curatorEntry("skills", curatorGetOr(job, "skills", nil), "skill", curatorGetOr(job, "skill", nil), "name", name))
	}
	if byID.Len() == 0 {
		report.Set("attempted", true)
		return report
	}
	if CuratorHooks.UpdateJobs == nil {
		report.Set("error", fmt.Sprintf("cron module unavailable: %v", errCronNotLinked))
		return report
	}
	report.Set("attempted", true)
	restored, unchanged := []any{}, 0
	liveIDs := map[string]bool{}
	err = CuratorHooks.UpdateJobs(ctx, home.Dir(ctx), func(jobs []any) (bool, error) {
		changed := false
		for _, live := range jobs {
			jid, ok := curatorGetOr(live, "id", nil).(string)
			if !curatorIsDict(live) || !ok || jid == "" {
				continue
			}
			liveIDs[jid] = true
			b, ok := byID.Get(jid)
			if !ok {
				continue
			}
			cur := curatorEntry("skills", curatorGetOr(live, "skills", nil), "skill", curatorGetOr(live, "skill", nil))
			bkp := curatorEntry("skills", curatorGetOr(b, "skills", nil), "skill", curatorGetOr(b, "skill", nil))
			if reflect.DeepEqual(cur.Vals, bkp.Vals) {
				unchanged++
				continue
			}
			for _, k := range bkp.Keys {
				curatorSetOrDelete(live, k, bkp.Vals[k])
			}
			restored = append(restored, curatorEntry("job_id", jid, "job_name", curatorGetOr(b, "name", jid), "from", cur, "to", bkp))
			changed = true
		}
		skipped := []any{}
		for _, jid := range byID.Keys {
			if !liveIDs[jid] {
				skipped = append(skipped, curatorEntry("job_id", jid, "job_name", curatorGetOr(byID.Vals[jid], "name", jid)))
			}
		}
		report.Set("skipped_missing", skipped)
		return changed, nil
	})
	report.Set("restored", restored)
	report.Set("unchanged", unchanged)
	if err != nil {
		curatorBackupLog().Debug(fmt.Sprintf("Cron skill-link restore failed: %v", err))
		report.Set("error", fmt.Sprintf("restore failed mid-flight: %v", err))
	}
	return report
}

func curatorRemoveEntry(entry string) error {
	st, err := os.Lstat(entry)
	if err != nil {
		return nil
	}
	if st.IsDir() {
		return pyfs.RmTree(entry, nil)
	}
	return os.Remove(entry)
}

// Moves excluded entries from the staged live tree back under the restored skills, returning failed moves
func curatorRestoreExcludedSubtrees(staged, skillsDir string) []string {
	var failed []string
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		var carried, descend []string
		for _, e := range entries {
			isDir := e.IsDir()
			if !isDir && e.Type()&fs.ModeSymlink != 0 {
				st, err := os.Stat(filepath.Join(dir, e.Name()))
				isDir = err == nil && st.IsDir()
			}
			if isDir {
				if curatorExcludeTopLevel[e.Name()] || skills.TransientDirs[e.Name()] {
					carried = append(carried, e.Name())
				} else if e.IsDir() {
					descend = append(descend, e.Name())
				}
			} else if curatorExcludeTopLevel[e.Name()] {
				carried = append(carried, e.Name())
			}
		}
		for _, n := range carried {
			src := filepath.Join(dir, n)
			rel, _ := filepath.Rel(staged, src)
			dest := filepath.Join(skillsDir, rel)
			if st, err := os.Stat(filepath.Dir(dest)); err == nil && st.IsDir() && !curatorExists(dest) {
				if _, err := curatorMove(src, dest); err != nil {
					failed = append(failed, rel)
					curatorBackupLog().Debug(fmt.Sprintf("Could not restore excluded entry %s: %v", src, err))
				}
			}
		}
		for _, n := range descend {
			walk(filepath.Join(dir, n))
		}
	}
	walk(staged)
	return failed
}

// Keeps a staging dir holding the only copy of unrestored entries and builds the failure message
func curatorRetainStaging(staged string, unrestored []string, reason string) (bool, string, string) {
	base := curatorUnrestoredPrefix + strings.TrimPrefix(filepath.Base(staged), curatorStagingPrefix)
	parent := filepath.Dir(staged)
	kept := filepath.Join(parent, base)
	for n := 1; curatorExists(kept); n++ {
		kept = filepath.Join(parent, fmt.Sprintf("%s-%d", base, n))
	}
	note := ""
	if err := curatorRename(staged, kept); err != nil {
		curatorBackupLog().Warn(fmt.Sprintf("Could not retain rollback staging dir %s: %v", staged, err))
		kept, note = staged, fmt.Sprintf(" (rename failed: %v; move it before the next curator run or it will be pruned)", err)
	}
	sorted := append([]string{}, unrestored...)
	sort.Strings(sorted)
	return false, fmt.Sprintf("%s - could not restore %s; staged copies kept at %s%s", reason, strings.Join(sorted, ", "), kept, note), ""
}

type curatorMoved struct{ orig, dest string }

// Moves staged entries back to their original paths, returning names that could not be restored
func curatorUnstage(moved []curatorMoved) []string {
	var failed []string
	for _, m := range moved {
		if err := curatorRemoveEntry(m.orig); err != nil {
			failed = append(failed, filepath.Base(m.orig))
			continue
		}
		if _, err := curatorMove(m.dest, m.orig); err != nil {
			failed = append(failed, filepath.Base(m.orig))
		}
	}
	return failed
}

// One line cron reconciliation summary, empty when nothing worth saying happened
func curatorCronSummary(rep *pyjson.Object) string {
	if !pyrepr.Truthy(curatorGetOr(rep, "attempted", nil)) {
		return ""
	}
	if e := curatorGetOr(rep, "error", nil); pyrepr.Truthy(e) {
		return "cron links: error — " + pyStr(e)
	}
	lenOf := func(key string) int {
		l, _ := curatorGetOr(rep, key, nil).([]any)
		return len(l)
	}
	var parts []string
	for _, p := range []struct {
		n     int
		label string
	}{{lenOf("restored"), "job(s) had skill links restored"}, {lenOf("skipped_missing"), "backed-up job(s) no longer exist (skipped)"}, {curatorCount(rep, "unchanged"), "already matched"}} {
		if p.n != 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.label))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "cron links: " + strings.Join(parts, ", ")
}

// Refuses absolute or parent escaping member names before the data filtered extract
func curatorCheckArchiveMembers(archive string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	names, err := pyfs.TarMemberNames(gz)
	if err != nil {
		return err
	}
	for _, n := range names {
		unsafe := strings.HasPrefix(n, "/")
		for _, part := range strings.Split(n, "/") {
			if part == ".." {
				unsafe = true
			}
		}
		if unsafe {
			return fmt.Errorf("refusing to extract unsafe path: %s", pyrepr.Repr(n))
		}
	}
	return nil
}

// Restores the skills tree from a snapshot after a safety snapshot, undoing the staging on failure
func RollbackCuratorSnapshot(ctx context.Context, backupID string) (bool, string, string) {
	target, ok := curatorResolveBackup(ctx, backupID)
	if !ok {
		msg := "no matching backup found"
		if backupID != "" {
			msg += fmt.Sprintf(" for id '%s'", backupID)
		}
		return false, msg + " (use `icebath curator rollback --list` to see available snapshots)", ""
	}
	archive := filepath.Join(target, curatorArchiveName)
	if !curatorExists(archive) {
		return false, fmt.Sprintf("snapshot %s has no skills.tar.gz — corrupted?", filepath.Base(target)), ""
	}
	skillsDir, backups := curatorSkillsDir(ctx), curatorBackupsDir(ctx)
	if err := os.MkdirAll(backups, 0o777); err != nil {
		return false, fmt.Sprintf("pre-rollback safety snapshot failed: %v", err), ""
	}
	_, ok, err := curatorSnapshotSkills(ctx, "pre-rollback to "+filepath.Base(target), map[string]bool{filepath.Base(target): true})
	if err != nil {
		return false, fmt.Sprintf("pre-rollback safety snapshot failed: %v", err), ""
	}
	if !ok {
		return false, "pre-rollback safety snapshot failed; backups may be disabled or unavailable, and current skills were not changed", ""
	}
	staged := filepath.Join(backups, curatorStagingPrefix+curatorUTCIDNow(time.Time{}))
	if err := os.Mkdir(staged, 0o777); err != nil {
		return false, fmt.Sprintf("failed to create staging dir: %v", err), ""
	}
	var moved []curatorMoved
	entries, err := os.ReadDir(skillsDir)
	if err == nil {
		for _, e := range entries {
			if curatorExcludeTopLevel[e.Name()] {
				continue
			}
			orig, dest := filepath.Join(skillsDir, e.Name()), filepath.Join(staged, e.Name())
			if _, err = curatorMove(orig, dest); err != nil {
				break
			}
			moved = append(moved, curatorMoved{orig, dest})
		}
	}
	if err != nil {
		reason := fmt.Sprintf("failed to stage current skills: %v", err)
		if unrestored := curatorUnstage(moved); len(unrestored) > 0 {
			return curatorRetainStaging(staged, unrestored, reason)
		}
		os.RemoveAll(staged)
		return false, reason, ""
	}
	if err = curatorCheckArchiveMembers(archive); err == nil {
		err = curatorExtractArchive(archive, skillsDir)
	}
	if err != nil {
		keep := map[string]bool{}
		for k := range curatorExcludeTopLevel {
			keep[k] = true
		}
		for _, m := range moved {
			keep[filepath.Base(m.orig)] = true
		}
		if now, rerr := os.ReadDir(skillsDir); rerr == nil {
			for _, e := range now {
				if !keep[e.Name()] {
					_ = curatorRemoveEntry(filepath.Join(skillsDir, e.Name()))
				}
			}
		}
		if unrestored := curatorUnstage(moved); len(unrestored) > 0 {
			return curatorRetainStaging(staged, unrestored, fmt.Sprintf("snapshot extract failed: %v", err))
		}
		os.RemoveAll(staged)
		return false, fmt.Sprintf("snapshot extract failed (state restored): %v", err), ""
	}
	if unrestored := curatorRestoreExcludedSubtrees(staged, skillsDir); len(unrestored) > 0 {
		return curatorRetainStaging(staged, unrestored, "snapshot extracted but excluded entries were not carried over")
	}
	os.RemoveAll(staged)
	cronReport := curatorRestoreCronSkillLinks(ctx, target)
	curatorBackupLog().Info(fmt.Sprintf("Curator rollback: restored from %s (cron_report=%s)", filepath.Base(target), pyrepr.Repr(cronReport)))
	msg := "restored from snapshot " + filepath.Base(target)
	if s := curatorCronSummary(cronReport); s != "" {
		msg += "; " + s
	}
	return true, msg, target
}

// Extracts the gzip snapshot through the tarfile data filter, replaced by tests that fail mid extract
var curatorExtractArchive = func(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	return pyfs.ExtractTarDataReader(gz, dest)
}

// hermes_cli sizefmt format_bytes, B below 1024 then one decimal KB MB GB TB
func formatBytes(n any) string {
	size, err := pyint.Float(n)
	if err != nil {
		return "?"
	}
	if size < 1024 {
		return fmt.Sprintf("%d B", int64(size))
	}
	for _, unit := range []string{"KB", "MB", "GB"} {
		size /= 1024.0
		if size < 1024 {
			return fmt.Sprintf("%.1f %s", size, unit)
		}
	}
	return fmt.Sprintf("%.1f TB", size/1024.0)
}

func curatorPad(s string, width int, right bool) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	if right {
		return strings.Repeat(" ", width-n) + s
	}
	return s + strings.Repeat(" ", width-n)
}

// Table of curator snapshots for the CLI
func SummarizeCuratorBackups(ctx context.Context) (string, error) {
	rows, err := ListCuratorBackups(ctx)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "No curator snapshots yet.", nil
	}
	header := fmt.Sprintf("%s  %s  %s  %s", curatorPad("id", 24, false), curatorPad("reason", 40, false), curatorPad("skills", 6, true), curatorPad("size", 8, true))
	lines := []string{header, strings.Repeat("─", utf8.RuneCountInString(header))}
	for _, r := range rows {
		reason := curatorGetOr(r, "reason", "?")
		if !pyrepr.Truthy(reason) {
			reason = "?"
		}
		size, err := pyint.Of(curatorGetOr(r, "archive_bytes", 0))
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %s  %s", curatorPad(pyStr(curatorGetOr(r, "id", "?")), 24, false), curatorPad(curatorRunes(pyStr(reason), 40), 40, false),
			curatorPad(pyStr(curatorGetOr(r, "skill_files", 0)), 6, true), curatorPad(formatBytes(pyint.Native(size)), 8, true)))
	}
	return strings.Join(lines, "\n"), nil
}

func registerCuratorBackup() {}
