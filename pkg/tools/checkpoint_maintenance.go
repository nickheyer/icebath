package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Counts what one prune scanned, deleted and reclaimed
type CheckpointPruneResult struct {
	Scanned       int   `json:"scanned"`
	DeletedOrphan int   `json:"deleted_orphan"`
	DeletedStale  int   `json:"deleted_stale"`
	Errors        int   `json:"errors"`
	BytesFreed    int64 `json:"bytes_freed"`
}

// Selects the retention, orphan and size rules of one prune
type CheckpointPruneOptions struct {
	RetentionDays  int
	DeleteOrphans  bool
	Base           string
	MaxTotalSizeMB int
	// Restricts orphan deletion to these project hashes and pre v2 repo paths, nil allows every orphan
	OrphanAllowlist map[string]bool
}

// Keeps a week of history and deletes every orphan
func DefaultCheckpointPruneOptions() CheckpointPruneOptions {
	return CheckpointPruneOptions{RetentionDays: 7, DeleteOrphans: true}
}

// Error that escapes maintenance the way a Python ValueError or TypeError does
type checkpointValueError struct{ msg string }

func (e *checkpointValueError) Error() string { return e.msg }

// Deletes a ref from the store, true on success, swapped by tests
var checkpointDeleteRef = func(env *Env, store, ref string) bool {
	ok, _, _ := runCheckpointGit(env, []string{"update-ref", "-d", ref}, store, filepath.Dir(store),
		checkpointGitOpts{Allowed: map[int]bool{128: true}})
	return ok
}

// Reports the stat errors pathlib treats as a missing path, any other probe failure is not evidence of absence
func checkpointPathMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ELOOP)
}

// True only when the workdir is observably deleted from a present, identity matched and populated or mounted parent
func workdirIsObservablyGone(workdir string, parentDev, parentIno *uint64, requireParentIdentity bool) bool {
	if workdir == "" {
		return false
	}
	path := filepath.Clean(workdir)
	if _, err := os.Stat(path); err == nil || !checkpointPathMissing(err) {
		return false
	}
	parent := filepath.Dir(path)
	if parent == path {
		return false
	}
	if st, err := os.Stat(parent); err != nil || !st.IsDir() {
		return false
	}
	if parentDev != nil && parentIno != nil {
		dev, ino, err := checkpointDevIno(parent)
		if err != nil || dev != *parentDev || ino != *parentIno {
			return false
		}
	} else if requireParentIdentity {
		return false
	}
	has, err := dirHasAnyEntry(parent)
	if err != nil {
		return false
	}
	if has {
		return true
	}
	return checkpointIsMount(parent)
}

// Reports whether the directory holds at least one entry without listing all of it
func dirHasAnyEntry(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Integer metadata value the way isinstance int without bool accepts it, nil otherwise
func checkpointMetaUint(v any) *uint64 {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return nil
	}
	u, err := strconv.ParseUint(string(n), 10, 64)
	if err != nil {
		return nil
	}
	return &u
}

// String metadata value, empty for a falsy one and a type error for any other kind
func checkpointMetaString(meta map[string]any, key string) (string, error) {
	v := meta[key]
	if s, ok := v.(string); ok {
		return s, nil
	}
	if !checkpointTruthy(v) {
		return "", nil
	}
	return "", &checkpointValueError{msg: "expected str, bytes or os.PathLike object, not " + checkpointPyType(v)}
}

// Python truthiness of a decoded metadata value
func checkpointTruthy(v any) bool {
	if n, ok := v.(json.Number); ok {
		f, err := n.Float64()
		return err != nil || f != 0
	}
	return truthy(v)
}

// Python type name of a decoded metadata or YAML value
func checkpointPyType(v any) string {
	switch x := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case json.Number:
		if strings.ContainsAny(string(x), ".eE") {
			return "float"
		}
		return "int"
	case int, int64, uint64:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

var pyFloatLiteralRe = regexp.MustCompile(`^[+-]?(?:(?:[0-9](?:_?[0-9])*)?\.[0-9](?:_?[0-9])*|[0-9](?:_?[0-9])*\.?)(?:[eE][+-]?[0-9](?:_?[0-9])*)?$`)
var pyFloatSpecialRe = regexp.MustCompile(`^[+-]?(?:inf|infinity|nan)$`)

// Float of a string the way Python float() parses it
func checkpointParsePyFloat(s string) (float64, error) {
	t := pyStrip(s)
	if pyFloatSpecialRe.MatchString(strings.ToLower(t)) {
		return strconv.ParseFloat(strings.ToLower(t), 64)
	}
	if !pyFloatLiteralRe.MatchString(t) {
		return 0, &checkpointValueError{msg: "could not convert string to float: " + pyReprStr(s)}
	}
	return strconv.ParseFloat(strings.ReplaceAll(t, "_", ""), 64)
}

// Float of a decoded metadata value the way Python float() reads it
func checkpointMetaFloat(v any) (float64, error) {
	switch x := v.(type) {
	case json.Number:
		return checkpointParsePyFloat(string(x))
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return checkpointParsePyFloat(x)
	}
	return 0, &checkpointValueError{msg: "float() argument must be a string or a real number, not '" + checkpointPyType(v) + "'"}
}

// Modification time in Unix seconds of a stat result
func statMtime(st os.FileInfo) float64 {
	return float64(st.ModTime().UnixNano()) / 1e9
}

// Child entries of a directory, follow symlinks when classifying directories
func checkpointBaseChildren(base string) ([]string, error) {
	ents, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, filepath.Join(base, e.Name()))
	}
	return out, nil
}

// Reports whether the path is a directory once symlinks are followed
func isDirFollow(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// Reports whether the path exists once symlinks are followed
func pathExistsFollow(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Deletes orphan and stale checkpoints and reclaims store space under the store lock
func PruneCheckpoints(env *Env, o CheckpointPruneOptions) (CheckpointPruneResult, error) {
	base := o.Base
	if base == "" {
		base = CheckpointBase(env)
	}
	var result CheckpointPruneResult
	if !pathExistsFollow(base) {
		return result, nil
	}
	release, err := checkpointStoreLock(base)
	if err != nil {
		result.Errors++
		toolsLogger(env).Warn(fmt.Sprintf("Checkpoint maintenance stopped: %v", err))
		return result, nil
	}
	defer release()
	if err := pruneCheckpointsLocked(env, base, &result, o); err != nil {
		var valueErr *checkpointValueError
		if errors.As(err, &valueErr) {
			return result, err
		}
		result.Errors++
		toolsLogger(env).Warn(fmt.Sprintf("Checkpoint maintenance stopped: %v", err))
	}
	return result, nil
}

// Prune body run while the store lock is held, sweeping legacy archives, pre v2 repos and v2 project refs
func pruneCheckpointsLocked(env *Env, base string, result *CheckpointPruneResult, o CheckpointPruneOptions) error {
	log := toolsLogger(env)
	sizeBefore := checkpointDirSizeBytes(base)
	cutoff := 0.0
	if o.RetentionDays > 0 {
		cutoff = float64(time.Now().UnixNano())/1e9 - float64(o.RetentionDays)*86400
	}
	children, err := checkpointBaseChildren(base)
	if err != nil {
		return err
	}
	for _, child := range children {
		name := filepath.Base(child)
		if !isDirFollow(child) || name == checkpointStoreDirname || !strings.HasPrefix(name, checkpointLegacyPrefix) {
			continue
		}
		if o.RetentionDays <= 0 {
			continue
		}
		st, err := os.Stat(child)
		if err != nil || statMtime(st) >= cutoff {
			continue
		}
		size := checkpointDirSizeBytes(child)
		if err := rmtreeReadonly(child, false); err != nil {
			result.Errors++
			log.Warn(fmt.Sprintf("Failed to delete legacy archive %s: %v", child, err))
			continue
		}
		result.BytesFreed += size
		result.DeletedStale++
	}

	repos, err := preV2ShadowRepos(base)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		child := repo.Path
		result.Scanned++
		reason := ""
		if o.DeleteOrphans && !repo.MarkerUnreadable &&
			(repo.Workdir == nil || workdirIsObservablyGone(*repo.Workdir, nil, nil, false)) &&
			(o.OrphanAllowlist == nil || o.OrphanAllowlist[child]) {
			reason = "orphan"
		}
		if reason == "" && o.RetentionDays > 0 {
			newest := checkpointNewestMtime(child)
			if newest > 0 && newest < cutoff {
				reason = "stale"
			}
		}
		if reason == "" {
			continue
		}
		size := checkpointDirSizeBytes(child)
		if err := rmtreeReadonly(child, false); err != nil {
			result.Errors++
			log.Warn(fmt.Sprintf("Failed to prune checkpoint repo %s: %v", filepath.Base(child), err))
			continue
		}
		result.BytesFreed += size
		if reason == "orphan" {
			result.DeletedOrphan++
		} else {
			result.DeletedStale++
		}
	}

	store := checkpointStorePath(base)
	if pathExistsFollow(filepath.Join(store, "HEAD")) {
		clearStaleTmpPacks(env, store, nil)
		for _, meta := range listCheckpointProjects(store) {
			dirHash, _ := meta["_hash"].(string)
			if dirHash == "" {
				continue
			}
			result.Scanned++
			reason := ""
			parentDev := checkpointMetaUint(meta["workdir_parent_dev"])
			parentIno := checkpointMetaUint(meta["workdir_parent_ino"])
			orphan := false
			if o.DeleteOrphans {
				workdir, err := checkpointMetaString(meta, "workdir")
				if err != nil {
					return err
				}
				orphan = workdir == "" || workdirIsObservablyGone(workdir, parentDev, parentIno, true)
			}
			if orphan && (o.OrphanAllowlist == nil || o.OrphanAllowlist[dirHash]) {
				reason = "orphan"
			} else if o.RetentionDays > 0 {
				lastTouch := 0.0
				if raw, ok := meta["last_touch"]; ok && checkpointTruthy(raw) {
					lastTouch, err = checkpointMetaFloat(raw)
					if err != nil {
						return err
					}
				}
				if lastTouch > 0 && lastTouch < cutoff {
					reason = "stale"
				}
			}
			if reason == "" {
				continue
			}
			if !checkpointDeleteRef(env, store, checkpointRefName(dirHash)) {
				result.Errors++
				return nil
			}
			os.Remove(checkpointIndexPath(store, dirHash))
			os.Remove(checkpointProjectMetaPath(store, dirHash))
			if reason == "orphan" {
				result.DeletedOrphan++
			} else {
				result.DeletedStale++
			}
		}

		pruner := &CheckpointPruner{
			RunGit:     checkpointGitRunner(env),
			Store:      store,
			Cwd:        base,
			Timeout:    checkpointGitTimeoutSecs,
			DirSize:    checkpointDirSizeBytes,
			RefsPrefix: checkpointRefsPrefix,
		}
		if err := checkpointRunPruner(pruner, result.DeletedOrphan+result.DeletedStale > 0, int64(o.MaxTotalSizeMB)*1024*1024, result, log.Warn); err != nil {
			result.Errors++
			log.Warn(fmt.Sprintf("Checkpoint pruning stopped: %v", err))
		}
	}

	delta := sizeBefore - checkpointDirSizeBytes(base)
	result.BytesFreed = max(result.BytesFreed, delta)
	return nil
}

// Reclaims after deletions or a deferred rewrite, then enforces the size cap, counting a cap it cannot reach
func checkpointRunPruner(p *CheckpointPruner, deleted bool, capBytes int64, result *CheckpointPruneResult, warn func(string, ...any)) error {
	if deleted || p.GCPending() {
		if err := p.Reclaim(); err != nil {
			return err
		}
	}
	fits, err := p.EnforceSize(capBytes)
	if err != nil {
		return err
	}
	if !fits {
		result.Errors++
		warn("Checkpoint store remains over its size cap; minimum history retained")
	}
	return nil
}

// Newest modification time of anything under the directory, zero when nothing is readable
func checkpointNewestMtime(dir string) float64 {
	newest := 0.0
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != dir {
				return fs.SkipDir
			}
			return nil
		}
		if p == dir {
			return nil
		}
		if st, err := os.Stat(p); err == nil {
			newest = max(newest, statMtime(st))
		}
		return nil
	})
	return newest
}

// Reports whether an auto prune ran, its counts and any failure text
type CheckpointAutoPruneOutcome struct {
	Skipped bool
	Result  *CheckpointPruneResult
	Error   string
}

// The knobs of the idempotent startup prune
type CheckpointAutoPruneOptions struct {
	RetentionDays    int
	MinIntervalHours int
	DeleteOrphans    bool
	Base             string
	MaxTotalSizeMB   int
}

// Keeps a week of history, runs at most daily and deletes orphans
func DefaultCheckpointAutoPruneOptions() CheckpointAutoPruneOptions {
	return CheckpointAutoPruneOptions{RetentionDays: 7, MinIntervalHours: 24, DeleteOrphans: true}
}

// Prunes at most once per interval, claiming the interval in the marker before it runs
func MaybeAutoPruneCheckpoints(env *Env, o CheckpointAutoPruneOptions) CheckpointAutoPruneOutcome {
	base := o.Base
	if base == "" {
		base = CheckpointBase(env)
	}
	log := toolsLogger(env)
	var out CheckpointAutoPruneOutcome
	if !pathExistsFollow(base) {
		out.Result = &CheckpointPruneResult{}
		return out
	}
	marker := filepath.Join(base, checkpointPruneMarkerName)
	now := float64(time.Now().UnixNano()) / 1e9
	if pathExistsFollow(marker) {
		if raw, err := os.ReadFile(marker); err == nil {
			if last, err := checkpointParsePyFloat(strings.TrimPrefix(string(raw), "\ufeff")); err == nil &&
				now-last < float64(o.MinIntervalHours)*3600 {
				out.Skipped = true
				return out
			}
		}
	}
	if err := os.WriteFile(marker, []byte(pyFloatRepr(now)), 0o666); err != nil {
		log.Debug(fmt.Sprintf("Could not write checkpoint prune marker: %v", err))
	}
	result, err := PruneCheckpoints(env, CheckpointPruneOptions{
		RetentionDays:  o.RetentionDays,
		DeleteOrphans:  o.DeleteOrphans,
		Base:           base,
		MaxTotalSizeMB: o.MaxTotalSizeMB,
	})
	if err != nil {
		log.Warn(fmt.Sprintf("checkpoint auto-maintenance failed: %v", err))
		out.Error = err.Error()
		return out
	}
	out.Result = &result
	if total := result.DeletedOrphan + result.DeletedStale; total > 0 {
		log.Info(fmt.Sprintf("checkpoint auto-maintenance: pruned %d entry(ies) (%d orphan, %d stale), reclaimed %.1f MB",
			total, result.DeletedOrphan, result.DeletedStale, float64(result.BytesFreed)/(1024*1024)))
	}
	return out
}

// Fresh checkpoints section of the profile config, a type error when it is not a mapping
func checkpointConfigSection(env *Env) (map[string]any, error) {
	doc, err := config.Load(envHome(env), configLookup(env))
	if err != nil {
		return nil, err
	}
	raw := doc.Get("checkpoints")
	if !truthy(raw) {
		return map[string]any{}, nil
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return nil, &checkpointValueError{msg: "'" + checkpointPyType(raw) + "' object has no attribute 'get'"}
	}
	return section, nil
}

// Config integer the way Python int() reads it, with the int() error text on failure
func checkpointConfigInt(section map[string]any, key string, fallback int) (int, error) {
	v, ok := section[key]
	if !ok {
		return fallback, nil
	}
	if n, ok := pyIntValue(v); ok {
		return n, nil
	}
	if s, ok := v.(string); ok {
		return 0, &checkpointValueError{msg: "invalid literal for int() with base 10: " + pyReprStr(s)}
	}
	return 0, &checkpointValueError{msg: "int() argument must be a string, a bytes-like object or a real number, not '" + checkpointPyType(v) + "'"}
}

// Runs the startup prune from the checkpoints config section, never deleting orphans unattended
func AutoPruneCheckpointsFromConfig(env *Env) CheckpointAutoPruneOutcome {
	fail := func(err error) CheckpointAutoPruneOutcome {
		toolsLogger(env).Debug(fmt.Sprintf("checkpoint auto-maintenance skipped: %v", err))
		return CheckpointAutoPruneOutcome{Skipped: true, Error: err.Error()}
	}
	cfg, err := checkpointConfigSection(env)
	if err != nil {
		return fail(err)
	}
	if !truthy(cfg["auto_prune"]) {
		return CheckpointAutoPruneOutcome{Skipped: true}
	}
	retention, err := checkpointConfigInt(cfg, "retention_days", 7)
	if err != nil {
		return fail(err)
	}
	interval, err := checkpointConfigInt(cfg, "min_interval_hours", 24)
	if err != nil {
		return fail(err)
	}
	capMB, err := checkpointConfigInt(cfg, "max_total_size_mb", 500)
	if err != nil {
		return fail(err)
	}
	return MaybeAutoPruneCheckpoints(env, CheckpointAutoPruneOptions{
		RetentionDays:    retention,
		MinIntervalHours: interval,
		DeleteOrphans:    false,
		MaxTotalSizeMB:   capMB,
	})
}

// Byte count as B, KB, MB, GB or TB with one decimal above bytes
func sizefmtBytes(n int64) string {
	size := float64(n)
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

// Names the opt out when checkpoints are on and the store sits at or above its cap, empty otherwise
func CheckpointFootprintNotice(env *Env) string {
	cfg, err := checkpointConfigSection(env)
	if err != nil {
		toolsLogger(env).Debug(fmt.Sprintf("checkpoint footprint notice skipped: %v", err))
		return ""
	}
	if !truthy(cfg["enabled"]) {
		return ""
	}
	capMB := 0
	if raw, ok := cfg["max_total_size_mb"]; !ok || truthy(raw) {
		capMB, err = checkpointConfigInt(cfg, "max_total_size_mb", 500)
		if err != nil {
			toolsLogger(env).Debug(fmt.Sprintf("checkpoint footprint notice skipped: %v", err))
			return ""
		}
	}
	if capMB <= 0 {
		return ""
	}
	base := CheckpointBase(env)
	size := checkpointDirSizeBytes(base)
	if size < int64(capMB)*1024*1024 {
		return ""
	}
	store := checkpointStorePath(base)
	projectCount := 0
	if checkpointStoreHasHead(store) {
		projectCount = len(listCheckpointProjects(store))
	}
	return fmt.Sprintf("Filesystem checkpoints (/rollback) are on: %s across %d project(s), above the %d MB cap (one snapshot per project is "+
		"always kept). Not using /rollback? `icebath config set checkpoints.enabled false` then "+
		"`icebath checkpoints clear`; or lower `checkpoints.retention_days`.", sizefmtBytes(size), projectCount, capMB)
}

// One v2 project of the store summary
type CheckpointProjectStatus struct {
	Hash      string `json:"hash"`
	Workdir   string `json:"workdir"`
	Exists    bool   `json:"exists"`
	CreatedAt any    `json:"created_at"`
	LastTouch any    `json:"last_touch"`
	Commits   int    `json:"commits"`
}

// One pre v2 per project shadow repo still under the base
type CheckpointPreV2Status struct {
	Path    string  `json:"path"`
	Workdir *string `json:"workdir"`
	Exists  bool    `json:"exists"`
}

// One legacy archive directory left by the v2 migration
type CheckpointLegacyArchive struct {
	Name      string  `json:"name"`
	SizeBytes int64   `json:"size_bytes"`
	Mtime     float64 `json:"mtime"`
}

// Summarises the shadow store, its projects, pre v2 repos and legacy archives
type CheckpointStatus struct {
	Base            string                    `json:"base"`
	StoreSizeBytes  int64                     `json:"store_size_bytes"`
	LegacySizeBytes int64                     `json:"legacy_size_bytes"`
	TotalSizeBytes  int64                     `json:"total_size_bytes"`
	ProjectCount    int                       `json:"project_count"`
	Projects        []CheckpointProjectStatus `json:"projects"`
	PreV2Projects   []CheckpointPreV2Status   `json:"pre_v2_projects"`
	LegacyArchives  []CheckpointLegacyArchive `json:"legacy_archives"`
}

// Reports sizes, every project with its commit count, pre v2 repos and legacy archives
func CheckpointStoreStatus(env *Env, base string) (CheckpointStatus, error) {
	if base == "" {
		base = CheckpointBase(env)
	}
	out := CheckpointStatus{
		Base:           base,
		Projects:       []CheckpointProjectStatus{},
		PreV2Projects:  []CheckpointPreV2Status{},
		LegacyArchives: []CheckpointLegacyArchive{},
	}
	if !pathExistsFollow(base) {
		return out, nil
	}
	store := checkpointStorePath(base)
	if pathExistsFollow(store) {
		out.StoreSizeBytes = checkpointDirSizeBytes(store)
		if pathExistsFollow(filepath.Join(store, "HEAD")) {
			for _, meta := range listCheckpointProjects(store) {
				dirHash, _ := meta["_hash"].(string)
				workdir, err := checkpointMetaString(meta, "workdir")
				if err != nil {
					return out, err
				}
				ok, countOut, _ := runCheckpointGit(env, []string{"rev-list", "--count", checkpointRefName(dirHash)}, store, base,
					checkpointGitOpts{Allowed: map[int]bool{128: true}})
				commits := 0
				if ok {
					if n, good := pyIntParse(countOut); good {
						commits = n
					}
				}
				out.Projects = append(out.Projects, CheckpointProjectStatus{
					Hash:      dirHash,
					Workdir:   workdir,
					Exists:    workdir != "" && pathExistsFollow(workdir),
					CreatedAt: meta["created_at"],
					LastTouch: meta["last_touch"],
					Commits:   commits,
				})
			}
		}
	}
	out.ProjectCount = len(out.Projects)
	repos, err := preV2ShadowRepos(base)
	if err != nil {
		return out, err
	}
	for _, r := range repos {
		out.PreV2Projects = append(out.PreV2Projects, CheckpointPreV2Status{Path: r.Path, Workdir: r.Workdir, Exists: r.Exists})
	}
	children, err := checkpointBaseChildren(base)
	if err != nil {
		return out, err
	}
	for _, child := range children {
		if !isDirFollow(child) || !strings.HasPrefix(filepath.Base(child), checkpointLegacyPrefix) {
			continue
		}
		size := checkpointDirSizeBytes(child)
		out.LegacySizeBytes += size
		mt := 0.0
		if st, err := os.Stat(child); err == nil {
			mt = statMtime(st)
		}
		out.LegacyArchives = append(out.LegacyArchives, CheckpointLegacyArchive{Name: filepath.Base(child), SizeBytes: size, Mtime: mt})
	}
	out.TotalSizeBytes = checkpointDirSizeBytes(base)
	return out, nil
}

// Reports what clearing the whole base freed
type CheckpointClearResult struct {
	BytesFreed int64 `json:"bytes_freed"`
	Deleted    bool  `json:"deleted"`
}

// Deletes the entire checkpoint base, store and legacy archives, under the store lock
func ClearAllCheckpoints(env *Env, base string) CheckpointClearResult {
	if base == "" {
		base = CheckpointBase(env)
	}
	var out CheckpointClearResult
	if !pathExistsFollow(base) {
		return out
	}
	size := checkpointDirSizeBytes(base)
	release, err := checkpointStoreLock(base)
	if err == nil {
		err = rmtreeReadonly(base, false)
		release()
	}
	if err != nil {
		toolsLogger(env).Warn(fmt.Sprintf("Could not clear checkpoint base %s: %v", base, err))
		return out
	}
	out.BytesFreed = size
	out.Deleted = true
	return out
}

// Reports the legacy archives deleted, bytes freed and failures
type CheckpointClearLegacyResult struct {
	BytesFreed int64 `json:"bytes_freed"`
	Deleted    int   `json:"deleted"`
	Errors     int   `json:"errors"`
}

// Deletes every legacy archive directory under the store lock and counts failures
func ClearLegacyCheckpoints(env *Env, base string) CheckpointClearLegacyResult {
	if base == "" {
		base = CheckpointBase(env)
	}
	var out CheckpointClearLegacyResult
	if !pathExistsFollow(base) {
		return out
	}
	log := toolsLogger(env)
	release, err := checkpointStoreLock(base)
	if err != nil {
		out.Errors++
		log.Warn(fmt.Sprintf("Could not clear legacy archives: %v", err))
		return out
	}
	defer release()
	children, err := checkpointBaseChildren(base)
	if err != nil {
		out.Errors++
		log.Warn(fmt.Sprintf("Could not clear legacy archives: %v", err))
		return out
	}
	for _, child := range children {
		if !isDirFollow(child) || !strings.HasPrefix(filepath.Base(child), checkpointLegacyPrefix) {
			continue
		}
		size := checkpointDirSizeBytes(child)
		if err := rmtreeReadonly(child, false); err != nil {
			out.Errors++
			log.Warn(fmt.Sprintf("Could not delete legacy archive %s: %v", child, err))
			continue
		}
		out.BytesFreed += size
		out.Deleted++
	}
	return out
}
