package tools

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
)

// Reads the import time checkpoint git timeout from ICEBATH_CHECKPOINT_TIMEOUT, clamped to ten through sixty seconds
func registerCheckpointManager() {
	n := 30
	if raw := pyStrip(os.Getenv("ICEBATH_CHECKPOINT_TIMEOUT")); raw != "" {
		if v, ok := pyIntParse(raw); ok {
			n = v
		}
	}
	checkpointGitTimeoutSecs = max(10, min(60, n))
}

// The master switch and the snapshot count, store size and file size budgets
type CheckpointManagerOptions struct {
	Enabled        bool
	MaxSnapshots   int
	MaxTotalSizeMB int
	MaxFileSizeMB  int
}

// Disabled with twenty snapshots, a 500 MB store and a 10 MB file cap
func DefaultCheckpointManagerOptions() CheckpointManagerOptions {
	return CheckpointManagerOptions{MaxSnapshots: 20, MaxTotalSizeMB: 500, MaxFileSizeMB: 10}
}

// Reads the checkpoints section of a config document, accepting the legacy boolean form
func CheckpointOptionsFromConfig(cfg map[string]any) (CheckpointManagerOptions, error) {
	var section map[string]any
	switch v := cfg["checkpoints"].(type) {
	case bool:
		section = map[string]any{"enabled": v}
	case map[string]any:
		section = v
	default:
		section = map[string]any{}
	}
	defaults := config.Defaults().Map("checkpoints")
	merged := map[string]any{}
	for k, v := range defaults {
		merged[k] = v
	}
	for k, v := range section {
		merged[k] = v
	}
	o := CheckpointManagerOptions{Enabled: truthy(merged["enabled"])}
	var err error
	if o.MaxSnapshots, err = checkpointConfigInt(merged, "max_snapshots", 20); err != nil {
		return o, err
	}
	if o.MaxTotalSizeMB, err = checkpointConfigInt(merged, "max_total_size_mb", 500); err != nil {
		return o, err
	}
	if o.MaxFileSizeMB, err = checkpointConfigInt(merged, "max_file_size_mb", 10); err != nil {
		return o, err
	}
	return o, nil
}

// Takes transparent filesystem snapshots into the shared shadow store, at most once per directory per turn
type CheckpointManager struct {
	Enabled        bool
	MaxSnapshots   int
	MaxTotalSizeMB int
	MaxFileSizeMB  int

	env              *Env
	mu               sync.Mutex
	checkpointedDirs map[string]bool
}

// Builds a manager for the env's profile with the budgets clamped to sane minimums
func NewCheckpointManager(env *Env, o CheckpointManagerOptions) *CheckpointManager {
	return &CheckpointManager{
		Enabled:          o.Enabled,
		MaxSnapshots:     max(1, o.MaxSnapshots),
		MaxTotalSizeMB:   max(0, o.MaxTotalSizeMB),
		MaxFileSizeMB:    max(0, o.MaxFileSizeMB),
		env:              env,
		checkpointedDirs: map[string]bool{},
	}
}

// Store of the manager's profile at call time
func (m *CheckpointManager) store() string {
	return checkpointStorePath(CheckpointBase(m.env))
}

// Text of an error the way Python's str of the exception reads
func checkpointExcText(err error) string {
	var pe *CheckpointPruneError
	if errors.As(err, &pe) {
		return pe.Msg
	}
	return pyOSErrorText(err)
}

// Resets the per turn dedup so the next mutation snapshots again
func (m *CheckpointManager) NewTurn() {
	m.mu.Lock()
	m.checkpointedDirs = map[string]bool{}
	m.mu.Unlock()
}

// Explains why host checkpoints are refused for a container backed task, empty otherwise
func (m *CheckpointManager) UnsupportedBackendReason(taskID string) string {
	return m.UnsupportedBackendReasonIn(m.env, taskID)
}

// Records the content hash of a file icebath just wrote so safe restores can spot later hand edits
func (m *CheckpointManager) RecordAgentWrite(filePath string) {
	if !m.Enabled {
		return
	}
	log := toolsLogger(m.env)
	fail := func(err error) {
		log.Debug(fmt.Sprintf("record_agent_write failed for %s: %s", filePath, checkpointExcText(err)))
	}
	defer func() {
		if r := recover(); r != nil {
			fail(fmt.Errorf("%v", r))
		}
	}()
	path := checkpointNormalizePath(filePath)
	digest := checkpointHashFile(path)
	if digest == "" {
		return
	}
	release, err := checkpointStoreLock(CheckpointBase(m.env))
	if err != nil {
		fail(err)
		return
	}
	defer release()
	store := m.store()
	dirHash, err := m.ledgerKey(path)
	if err != nil {
		fail(err)
		return
	}
	ledger := loadCheckpointLedger(store, dirHash)
	ledger[path] = map[string]any{"sha256": digest, "ts": checkpointNow()}
	if err := saveCheckpointLedger(m.env, store, dirHash, ledger); err != nil {
		fail(err)
	}
}

// Feeds the agent write ledger with every landed path of a host backed task
func (m *CheckpointManager) RecordLandedWrites(paths []string, taskID string) {
	if !m.Enabled {
		return
	}
	if taskID == "" {
		taskID = "default"
	}
	if containerBackendForTask(m.env, taskID) != "" {
		return
	}
	for _, p := range paths {
		m.RecordAgentWrite(p)
	}
}

// Safe restore classification of the files changed since a checkpoint
type checkpointRestorePlan struct {
	Error       string
	Restore     []string
	Skipped     []string
	LedgerEmpty bool
}

// Splits the files changed since the checkpoint into icebath authored ones to revert and user edited ones to keep
func (m *CheckpointManager) safeRestorePlan(workingDir, commitHash string) (checkpointRestorePlan, error) {
	if hashErr := checkpointValidateCommitHash(commitHash); hashErr != "" {
		return checkpointRestorePlan{Error: hashErr}, nil
	}
	absDir := checkpointNormalizePath(workingDir)
	store := m.store()
	if !checkpointStoreHasHead(store) {
		return checkpointRestorePlan{Error: "No checkpoints exist for this directory"}, nil
	}
	dirHash := checkpointProjectHash(absDir)
	indexFile := checkpointIndexPath(store, dirHash)
	runCheckpointGit(m.env, []string{"add", "-A"}, store, absDir, checkpointGitOpts{Timeout: checkpointGitTimeout() * 2, IndexFile: indexFile})
	ok, namesOut, errText := runCheckpointGit(m.env, []string{"diff", "--name-only", "-z", commitHash, "--cached"}, store, absDir,
		checkpointGitOpts{IndexFile: indexFile})
	runCheckpointGit(m.env, []string{"read-tree", checkpointRefName(dirHash)}, store, absDir,
		checkpointGitOpts{IndexFile: indexFile, Allowed: map[int]bool{128: true}})
	if !ok {
		return checkpointRestorePlan{Error: "Could not compute changed files: " + errText}, nil
	}
	key, err := m.ledgerKey(absDir)
	if err != nil {
		return checkpointRestorePlan{}, err
	}
	ledger := loadCheckpointLedger(store, key)
	if len(ledger) == 0 {
		return checkpointRestorePlan{Restore: []string{}, Skipped: []string{}, LedgerEmpty: true}, nil
	}
	plan := checkpointRestorePlan{Restore: []string{}, Skipped: []string{}}
	for _, rel := range strings.Split(namesOut, "\x00") {
		if rel == "" {
			continue
		}
		absPath := filepath.Join(absDir, rel)
		entry, _ := ledger[absPath].(map[string]any)
		recorded, has := entry["sha256"]
		if entry == nil || !has || recorded == nil {
			plan.Skipped = append(plan.Skipped, rel)
			continue
		}
		current := checkpointHashFile(absPath)
		if s, isStr := recorded.(string); current == "" || (isStr && current == s) {
			plan.Restore = append(plan.Restore, rel)
			continue
		}
		plan.Skipped = append(plan.Skipped, rel)
	}
	return plan, nil
}

// Home directory the way Path.home reports it
func checkpointUserHome() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Clean(h)
	}
	if u, err := user.Current(); err == nil {
		return filepath.Clean(u.HomeDir)
	}
	return ""
}

// Snapshots the directory once per turn when enabled, true when a checkpoint was taken and never panicking
func (m *CheckpointManager) EnsureCheckpoint(workingDir, reason string) (taken bool) {
	if !m.Enabled {
		return false
	}
	log := toolsLogger(m.env)
	absDir := checkpointNormalizePath(workingDir)
	if absDir == "/" || absDir == checkpointUserHome() {
		log.Debug(fmt.Sprintf("Checkpoint skipped: directory too broad (%s)", absDir))
		return false
	}
	m.mu.Lock()
	if m.checkpointedDirs[absDir] {
		m.mu.Unlock()
		return false
	}
	m.checkpointedDirs[absDir] = true
	m.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			log.Debug(fmt.Sprintf("Checkpoint failed (non-fatal): %v", r))
			taken = false
		}
	}()
	release, err := checkpointStoreLock(CheckpointBase(m.env))
	if err != nil {
		log.Debug("Checkpoint failed (non-fatal): " + checkpointExcText(err))
		return false
	}
	defer release()
	ok, err := m.take(absDir, reason, true)
	if err != nil {
		log.Debug("Checkpoint failed (non-fatal): " + checkpointExcText(err))
		return false
	}
	return ok
}

// Snapshots the workspace a write_file or patch call will mutate, resolved the way the file tools resolve it
func (m *CheckpointManager) EnsureFileCheckpoint(name string, args map[string]any, taskID string) {
	if !m.Enabled {
		return
	}
	filePath, _ := args["path"].(string)
	if filePath == "" {
		return
	}
	if taskID == "" {
		taskID = "default"
	}
	if containerBackendForTask(m.env, taskID) != "" || isNTNamespacePath(filePath) {
		return
	}
	resolved, err := resolvePathForTaskID(m.env, taskID, filePath)
	if err != nil {
		return
	}
	workdir, err := m.WorkingDirForPath(resolved)
	if err != nil {
		return
	}
	m.EnsureCheckpoint(workdir, "before "+name)
}

// Snapshots the terminal working directory of a host backed task before a destructive command
func (m *CheckpointManager) EnsureTerminalCheckpoint(cwd, label, taskID string) {
	if !m.Enabled {
		return
	}
	if taskID == "" {
		taskID = "default"
	}
	if containerBackendForTask(m.env, taskID) != "" {
		return
	}
	if cwd == "" {
		scoped, err := scopeTerminalCwd(m.env)
		if err != nil {
			return
		}
		cwd = scoped
	}
	if cwd == "" {
		current, err := processCwd(m.env)
		if err != nil {
			return
		}
		cwd = current
	}
	m.EnsureCheckpoint(cwd, label)
}

// One snapshot of a project, newest first in listings
type CheckpointEntry struct {
	Hash         string `json:"hash"`
	ShortHash    string `json:"short_hash"`
	Timestamp    string `json:"timestamp"`
	Reason       string `json:"reason"`
	FilesChanged int    `json:"files_changed"`
	Insertions   int    `json:"insertions"`
	Deletions    int    `json:"deletions"`
	Workdir      string `json:"workdir,omitempty"`
}

// Lists the directory's snapshots newest first with their shortstat counts
func (m *CheckpointManager) ListCheckpoints(workingDir string) []CheckpointEntry {
	absDir := checkpointNormalizePath(workingDir)
	store := m.store()
	if !checkpointStoreHasHead(store) {
		return []CheckpointEntry{}
	}
	ref := checkpointRefName(checkpointProjectHash(absDir))
	allowed := map[int]bool{128: true, 129: true}
	ok, stdout, _ := runCheckpointGit(m.env, []string{"log", ref, "--format=%H|%h|%aI|%s", "-n", strconv.Itoa(m.MaxSnapshots)},
		store, absDir, checkpointGitOpts{Allowed: allowed})
	if !ok || stdout == "" {
		return []CheckpointEntry{}
	}
	results := []CheckpointEntry{}
	for _, line := range pySplitLines(stdout) {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		entry := CheckpointEntry{Hash: parts[0], ShortHash: parts[1], Timestamp: parts[2], Reason: parts[3]}
		statOK, statOut, _ := runCheckpointGit(m.env, []string{"diff", "--shortstat", parts[0] + "~1", parts[0]}, store, absDir,
			checkpointGitOpts{Allowed: allowed})
		if statOK && statOut != "" {
			parseCheckpointShortstat(statOut, &entry)
		}
		results = append(results, entry)
	}
	return results
}

// Lists the snapshots of every registered project newest first, each labelled with its workdir
func (m *CheckpointManager) ListAllCheckpoints() ([]CheckpointEntry, error) {
	store := m.store()
	if !checkpointStoreHasHead(store) {
		return []CheckpointEntry{}, nil
	}
	results := []CheckpointEntry{}
	for _, meta := range listCheckpointProjects(store) {
		workdir, err := checkpointMetaString(meta, "workdir")
		if err != nil {
			return nil, err
		}
		if workdir == "" {
			continue
		}
		for _, entry := range m.ListCheckpoints(workdir) {
			entry.Workdir = workdir
			results = append(results, entry)
		}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Timestamp > results[j].Timestamp })
	return results, nil
}

var (
	shortstatFilesRe      = regexp.MustCompile(`(\d+) file`)
	shortstatInsertionsRe = regexp.MustCompile(`(\d+) insertion`)
	shortstatDeletionsRe  = regexp.MustCompile(`(\d+) deletion`)
)

// Fills the change counts of an entry from git diff shortstat output
func parseCheckpointShortstat(statLine string, entry *CheckpointEntry) {
	for _, f := range []struct {
		re  *regexp.Regexp
		dst *int
	}{{shortstatFilesRe, &entry.FilesChanged}, {shortstatInsertionsRe, &entry.Insertions}, {shortstatDeletionsRe, &entry.Deletions}} {
		if mm := f.re.FindStringSubmatch(statLine); mm != nil {
			if n, err := strconv.Atoi(mm[1]); err == nil {
				*f.dst = n
			}
		}
	}
}

// Shows the stat and patch between a checkpoint and the current working tree under the store lock
func (m *CheckpointManager) Diff(workingDir, commitHash string) map[string]any {
	release, err := checkpointStoreLock(CheckpointBase(m.env))
	if err != nil {
		return map[string]any{"success": false, "error": checkpointExcText(err)}
	}
	defer release()
	return m.diff(workingDir, commitHash)
}

// Stages the current tree into the project index, diffs it against the checkpoint and resets the index
func (m *CheckpointManager) diff(workingDir, commitHash string) map[string]any {
	if hashErr := checkpointValidateCommitHash(commitHash); hashErr != "" {
		return map[string]any{"success": false, "error": hashErr}
	}
	absDir := checkpointNormalizePath(workingDir)
	store := m.store()
	if !checkpointStoreHasHead(store) {
		return map[string]any{"success": false, "error": "No checkpoints exist for this directory"}
	}
	if ok, _, _ := runCheckpointGit(m.env, []string{"cat-file", "-t", commitHash}, store, absDir, checkpointGitOpts{}); !ok {
		return map[string]any{"success": false, "error": fmt.Sprintf("Checkpoint '%s' not found", commitHash)}
	}
	dirHash := checkpointProjectHash(absDir)
	indexFile := checkpointIndexPath(store, dirHash)
	runCheckpointGit(m.env, []string{"add", "-A"}, store, absDir, checkpointGitOpts{Timeout: checkpointGitTimeout() * 2, IndexFile: indexFile})
	okStat, statOut, _ := runCheckpointGit(m.env, []string{"diff", "--stat", commitHash, "--cached"}, store, absDir,
		checkpointGitOpts{IndexFile: indexFile})
	okDiff, diffOut, _ := runCheckpointGit(m.env, []string{"diff", commitHash, "--cached", "--no-color"}, store, absDir,
		checkpointGitOpts{IndexFile: indexFile})
	runCheckpointGit(m.env, []string{"read-tree", checkpointRefName(dirHash)}, store, absDir,
		checkpointGitOpts{IndexFile: indexFile, Allowed: map[int]bool{128: true}})
	if !okStat && !okDiff {
		return map[string]any{"success": false, "error": "Could not generate diff"}
	}
	if !okStat {
		statOut = ""
	}
	if !okDiff {
		diffOut = ""
	}
	return map[string]any{"success": true, "stat": statOut, "diff": diffOut}
}

// Diffs the earliest retained checkpoint against the working tree, an empty success when none exists
func (m *CheckpointManager) SessionDiff(workingDir string) map[string]any {
	checkpoints := m.ListCheckpoints(workingDir)
	if len(checkpoints) == 0 {
		return map[string]any{"success": true, "stat": "", "diff": "", "empty": true}
	}
	baseline := checkpoints[len(checkpoints)-1].Hash
	result := m.Diff(workingDir, baseline)
	if truthy(result["success"]) {
		if _, has := result["baseline"]; !has {
			result["baseline"] = baseline
		}
		if !truthy(result["stat"]) && !truthy(result["diff"]) {
			result["empty"] = true
		}
	}
	return result
}

// Puts files back to a checkpoint under the store lock, safe mode keeping files the user edited after icebath
func (m *CheckpointManager) Restore(workingDir, commitHash, filePath string, safe bool) map[string]any {
	release, err := checkpointStoreLock(CheckpointBase(m.env))
	if err != nil {
		return map[string]any{"success": false, "error": checkpointExcText(err)}
	}
	defer release()
	result, err := m.restore(workingDir, commitHash, filePath, safe)
	if err != nil {
		return map[string]any{"success": false, "error": checkpointExcText(err)}
	}
	return result
}

// Debug value of a git error, nil when git said nothing
func checkpointDebug(errText string) any {
	if errText == "" {
		return nil
	}
	return errText
}

// First eight characters of a commit hash
func checkpointShort(commitHash string) string {
	return commitHash[:min(8, len(commitHash))]
}

// Restore body run under the store lock, refusing uncaptured nested repositories before touching anything
func (m *CheckpointManager) restore(workingDir, commitHash, filePath string, safe bool) (map[string]any, error) {
	if hashErr := checkpointValidateCommitHash(commitHash); hashErr != "" {
		return map[string]any{"success": false, "error": hashErr}, nil
	}
	absDir := checkpointNormalizePath(workingDir)
	if filePath != "" {
		if pathErr := checkpointValidateFilePath(filePath, absDir); pathErr != "" {
			return map[string]any{"success": false, "error": pathErr}, nil
		}
	}
	store := m.store()
	if !checkpointStoreHasHead(store) {
		return map[string]any{"success": false, "error": "No checkpoints exist for this directory"}, nil
	}
	if ok, _, errText := runCheckpointGit(m.env, []string{"cat-file", "-t", commitHash}, store, absDir, checkpointGitOpts{}); !ok {
		return map[string]any{"success": false, "error": fmt.Sprintf("Checkpoint '%s' not found", commitHash), "debug": checkpointDebug(errText)}, nil
	}
	ok, treeOut, errText := runCheckpointGit(m.env, []string{"ls-tree", "-r", "-z", commitHash}, store, absDir, checkpointGitOpts{})
	if !ok {
		return map[string]any{"success": false, "error": "Could not inspect checkpoint: " + errText}, nil
	}
	var selectedPaths []string
	hasSelected := false
	if nested := checkpointGitlinkPaths(treeOut); len(nested) > 0 {
		sel, err := checkpointResolveSelection(m.env, store, absDir, commitHash, filePath, nested)
		if err != nil {
			return nil, err
		}
		if sel.Failure != nil {
			return sel.Failure, nil
		}
		selectedPaths, hasSelected = sel.Selected, sel.HasSelected
		if len(sel.Blocked) > 0 {
			return map[string]any{
				"success":             false,
				"error":               "Checkpoint contains nested git repositories that were not captured (" + strings.Join(sel.Blocked, ", ") + "); rollback was not performed",
				"nested_repositories": sel.Blocked,
			}, nil
		}
	}

	skippedUserEdits := []string{}
	keptOversize := []string{}
	failedDeletes := []string{}
	var restorePaths []string
	hasRestorePaths := false
	if safe && filePath == "" {
		plan, err := m.safeRestorePlan(absDir, commitHash)
		if err != nil {
			return nil, err
		}
		if plan.Error != "" {
			return map[string]any{"success": false, "error": plan.Error}, nil
		}
		if !plan.LedgerEmpty {
			restorePaths, hasRestorePaths = plan.Restore, true
			skippedUserEdits = plan.Skipped
			if len(restorePaths) == 0 {
				return map[string]any{
					"success":            true,
					"restored_to":        checkpointShort(commitHash),
					"reason":             "nothing to restore (all changed files were user-edited)",
					"directory":          absDir,
					"restored_files":     []string{},
					"skipped_user_edits": skippedUserEdits,
					"skipped_oversize":   []string{},
				}, nil
			}
		}
	}

	if _, err := m.take(absDir, "pre-rollback snapshot (restoring to "+checkpointShort(commitHash)+")", false); err != nil {
		return nil, err
	}
	dirHash := checkpointProjectHash(absDir)
	indexFile := checkpointIndexPath(store, dirHash)
	slow := checkpointGitOpts{Timeout: checkpointGitTimeout() * 2, IndexFile: indexFile}
	switch {
	case hasRestorePaths:
		var checkoutTargets, deleteTargets []string
		for _, rel := range restorePaths {
			inCommit, _, _ := runCheckpointGit(m.env, []string{"cat-file", "-e", commitHash + ":" + rel}, store, absDir,
				checkpointGitOpts{Allowed: map[int]bool{1: true, 128: true}})
			switch {
			case inCommit:
				checkoutTargets = append(checkoutTargets, rel)
			case m.exceedsSizeCap(filepath.Join(absDir, rel)):
				keptOversize = append(keptOversize, rel)
			default:
				deleteTargets = append(deleteTargets, rel)
			}
		}
		for _, rel := range deleteTargets {
			target := filepath.Join(absDir, rel)
			st, err := os.Lstat(target)
			if err != nil {
				continue
			}
			isFile := st.Mode()&os.ModeSymlink != 0 || st.Mode().IsRegular()
			if !isFile {
				continue
			}
			if err := checkpointRemove(target); err != nil {
				toolsLogger(m.env).Warn(fmt.Sprintf("Safe restore: could not remove %s: %s", rel, pyOSErrorText(err)))
				failedDeletes = append(failedDeletes, rel)
			}
		}
		if len(checkoutTargets) == 0 {
			ok, errText = true, ""
		} else {
			ok, _, errText = runCheckpointGit(m.env, append([]string{"checkout", commitHash, "--"}, checkoutTargets...), store, absDir, slow)
		}
	case hasSelected:
		var err error
		ok, errText, err = checkpointCheckoutSelected(m.env, store, absDir, commitHash, indexFile, selectedPaths)
		if err != nil {
			return nil, err
		}
	default:
		target := filePath
		if target == "" {
			target = "."
		}
		ok, _, errText = runCheckpointGit(m.env, []string{"checkout", commitHash, "--", target}, store, absDir, slow)
	}
	if !ok {
		return map[string]any{"success": false, "error": "Restore failed: " + errText, "debug": checkpointDebug(errText)}, nil
	}
	reason := "unknown"
	if ok2, reasonOut, _ := runCheckpointGit(m.env, []string{"log", "--format=%s", "-1", commitHash}, store, absDir, checkpointGitOpts{}); ok2 {
		reason = reasonOut
	}
	result := map[string]any{
		"success":     true,
		"restored_to": checkpointShort(commitHash),
		"reason":      reason,
		"directory":   absDir,
	}
	if filePath != "" {
		result["file"] = filePath
	}
	if hasRestorePaths {
		notRestored := map[string]bool{}
		for _, rel := range keptOversize {
			notRestored[rel] = true
		}
		for _, rel := range failedDeletes {
			notRestored[rel] = true
		}
		restored := []string{}
		for _, rel := range restorePaths {
			if !notRestored[rel] {
				restored = append(restored, rel)
			}
		}
		result["restored_files"] = restored
		result["skipped_user_edits"] = skippedUserEdits
		result["skipped_oversize"] = keptOversize
		if len(failedDeletes) > 0 {
			result["failed_deletes"] = failedDeletes
		}
	}
	m.prune(store, absDir, checkpointRefName(dirHash))
	return result, nil
}

// File removal used by safe restore, swapped by tests
var checkpointRemove = os.Remove

// Ledger key of a path, the hash of its marker walked project directory for writer and reader alike
func (m *CheckpointManager) ledgerKey(path string) (string, error) {
	workdir, err := m.WorkingDirForPath(path)
	if err != nil {
		return "", err
	}
	return checkpointProjectHash(workdir), nil
}

// Comparison key of a path, case folded where the platform folds case
func checkpointPathKey(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(filepath.Clean(p))
	}
	return filepath.Clean(p)
}

// Markers whose presence makes a directory a project root
var checkpointProjectMarkers = []string{".git", "pyproject.toml", "package.json", "Cargo.toml", "go.mod", "Makefile", "pom.xml", ".hg", "Gemfile"}

// Resolves a file to the directory that owns its checkpoints, the nearest registered root or marked project
func (m *CheckpointManager) WorkingDirForPath(filePath string) (string, error) {
	path := checkpointNormalizePath(filePath)
	candidate := path
	if !isDirFollow(path) {
		candidate = filepath.Dir(path)
	}
	owner, ownerParts := "", -1
	for _, meta := range listCheckpointProjects(m.store()) {
		root, err := checkpointMetaString(meta, "workdir")
		if err != nil {
			return "", err
		}
		if root == "" {
			continue
		}
		if _, ok := relativeTo(candidate, root); !ok {
			continue
		}
		if n := len(pathParts(filepath.Clean(root))); n > ownerParts {
			owner, ownerParts = filepath.Clean(root), n
		}
	}
	if owner != "" {
		return owner, nil
	}
	broad := map[string]bool{}
	if h := checkpointUserHome(); h != "" {
		for p := checkpointResolve(h); ; p = filepath.Dir(p) {
			broad[checkpointPathKey(p)] = true
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	for check := candidate; check != filepath.Dir(check) && !broad[checkpointPathKey(check)]; check = filepath.Dir(check) {
		for _, marker := range checkpointProjectMarkers {
			if pathExistsFollow(filepath.Join(check, marker)) {
				return check, nil
			}
		}
	}
	return candidate, nil
}

// Takes one snapshot into the project ref, false when skipped or failed and an error for escaped I/O failures
func (m *CheckpointManager) take(workingDir, reason string, prune bool) (bool, error) {
	log := toolsLogger(m.env)
	store := m.store()
	initErr, err := initCheckpointStore(m.env, store, workingDir)
	if err != nil {
		return false, err
	}
	if initErr != "" {
		log.Debug("Checkpoint store init failed: " + initErr)
		return false, nil
	}
	touchCheckpointProject(m.env, store, workingDir)
	if checkpointDirFileCount(workingDir) > checkpointMaxFiles {
		log.Debug(fmt.Sprintf("Checkpoint skipped: >%d files in %s", checkpointMaxFiles, workingDir))
		return false, nil
	}
	dirHash := checkpointProjectHash(workingDir)
	indexFile := checkpointIndexPath(store, dirHash)
	ref := checkpointRefName(dirHash)
	revParse := []string{"rev-parse", "--verify", ref + "^{commit}"}
	if pathExistsFollow(indexFile) {
		okRef, refCommit, _ := runCheckpointGit(m.env, revParse, store, workingDir, checkpointGitOpts{Allowed: map[int]bool{128: true}})
		if okRef && refCommit != "" {
			runCheckpointGit(m.env, []string{"read-tree", refCommit}, store, workingDir,
				checkpointGitOpts{IndexFile: indexFile, Allowed: map[int]bool{128: true}})
		} else {
			os.Remove(indexFile)
		}
	} else if err := os.MkdirAll(filepath.Dir(indexFile), 0o777); err != nil {
		return false, err
	}
	if ok, _, errText := runCheckpointGit(m.env, []string{"add", "-A"}, store, workingDir,
		checkpointGitOpts{Timeout: checkpointGitTimeout() * 2, IndexFile: indexFile}); !ok {
		log.Debug("Checkpoint git-add failed: " + errText)
		return false, nil
	}
	if m.MaxFileSizeMB > 0 {
		m.dropOversizeFromIndex(store, workingDir, indexFile)
	}
	okRef, refCommit, _ := runCheckpointGit(m.env, revParse, store, workingDir, checkpointGitOpts{Allowed: map[int]bool{128: true}})
	hasRef := okRef && refCommit != ""
	if hasRef {
		if same, _, _ := runCheckpointGit(m.env, []string{"diff-index", "--cached", "--quiet", refCommit}, store, workingDir,
			checkpointGitOpts{Allowed: map[int]bool{1: true}, IndexFile: indexFile}); same {
			log.Debug("Checkpoint skipped: no changes in " + workingDir)
			return false, nil
		}
	} else {
		okLs, lsOut, _ := runCheckpointGit(m.env, []string{"ls-files", "--cached"}, store, workingDir, checkpointGitOpts{IndexFile: indexFile})
		if okLs && pyStrip(lsOut) == "" {
			log.Debug("Checkpoint skipped: empty tree in " + workingDir)
			return false, nil
		}
	}
	okTree, treeSHA, errText := runCheckpointGit(m.env, []string{"write-tree"}, store, workingDir, checkpointGitOpts{IndexFile: indexFile})
	if !okTree || treeSHA == "" {
		log.Debug("Checkpoint write-tree failed: " + errText)
		return false, nil
	}
	okLs, treeOut, _ := runCheckpointGit(m.env, []string{"ls-tree", "-r", "-z", treeSHA}, store, workingDir, checkpointGitOpts{})
	if okLs {
		if nested := checkpointGitlinkPaths(treeOut); len(nested) > 0 {
			log.Info(fmt.Sprintf("Checkpoint of %s does not capture nested git repositories: %s", workingDir, strings.Join(nested, ", ")))
			var lines []string
			for _, line := range pySplitLines(reason) {
				if s := pyStrip(line); s != "" {
					lines = append(lines, s)
				}
			}
			reason = strings.Join(lines, " ") + checkpointUncapturedNote(nested)
		}
	}
	commitArgs := []string{"commit-tree", treeSHA, "-m", reason, "--no-gpg-sign"}
	if hasRef {
		commitArgs = []string{"commit-tree", treeSHA, "-p", refCommit, "-m", reason, "--no-gpg-sign"}
	}
	okCommit, newSHA, errText := runCheckpointGit(m.env, commitArgs, store, workingDir, checkpointGitOpts{IndexFile: indexFile})
	if !okCommit || newSHA == "" {
		log.Debug("Checkpoint commit-tree failed: " + errText)
		return false, nil
	}
	updateArgs := []string{"update-ref", ref, newSHA}
	if hasRef {
		updateArgs = append(updateArgs, refCommit)
	}
	if ok, _, errText := runCheckpointGit(m.env, updateArgs, store, workingDir, checkpointGitOpts{}); !ok {
		log.Debug("Checkpoint update-ref failed: " + errText)
		return false, nil
	}
	log.Debug(fmt.Sprintf("Checkpoint taken in %s: %s (%s)", workingDir, reason, checkpointShort(newSHA)))
	if prune {
		m.prune(store, workingDir, ref)
	}
	return true, nil
}

// Reports whether the path is larger than the per file cap, the one test shared by snapshot and safe restore
func (m *CheckpointManager) exceedsSizeCap(path string) bool {
	capBytes := int64(m.MaxFileSizeMB) * 1024 * 1024
	if capBytes <= 0 {
		return false
	}
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.Size() > capBytes
}

// Removes every staged file above the per file cap from the project index in batches of two hundred
func (m *CheckpointManager) dropOversizeFromIndex(store, workingDir, indexFile string) {
	if m.MaxFileSizeMB <= 0 {
		return
	}
	ok, stdout, _ := runCheckpointGit(m.env, []string{"ls-files", "--cached", "-z"}, store, workingDir, checkpointGitOpts{IndexFile: indexFile})
	if !ok || stdout == "" {
		return
	}
	absWorkdir := checkpointNormalizePath(workingDir)
	var oversize []string
	for _, rel := range strings.Split(stdout, "\x00") {
		if rel != "" && m.exceedsSizeCap(filepath.Join(absWorkdir, rel)) {
			oversize = append(oversize, rel)
		}
	}
	if len(oversize) == 0 {
		return
	}
	toolsLogger(m.env).Debug(fmt.Sprintf("Checkpoint: dropping %d oversize file(s) (>%d MB) from index", len(oversize), m.MaxFileSizeMB))
	const batch = 200
	for i := 0; i < len(oversize); i += batch {
		chunk := oversize[i:min(i+batch, len(oversize))]
		runCheckpointGit(m.env, append([]string{"rm", "--cached", "--quiet", "--"}, chunk...), store, workingDir,
			checkpointGitOpts{IndexFile: indexFile, Allowed: map[int]bool{128: true}})
	}
}

// Applies the snapshot count budget and one size round on the take path, deferring the gc to the periodic prune
func (m *CheckpointManager) prune(store, workingDir, ref string) {
	log := toolsLogger(m.env)
	pruner := &CheckpointPruner{
		RunGit:     checkpointGitRunner(m.env),
		Store:      store,
		Cwd:        workingDir,
		Timeout:    checkpointGitTimeoutSecs,
		DirSize:    checkpointDirSizeBytes,
		RefsPrefix: checkpointRefsPrefix,
	}
	if err := pruner.Trim(ref, m.MaxSnapshots); err != nil {
		log.Warn("Checkpoint pruning stopped: " + checkpointExcText(err))
		return
	}
	dropped, err := pruner.DropOneRound(int64(m.MaxTotalSizeMB) * 1024 * 1024)
	if err != nil {
		log.Warn("Checkpoint pruning stopped: " + checkpointExcText(err))
		return
	}
	if dropped {
		log.Info(fmt.Sprintf("Checkpoint store exceeded %d MB — dropped the oldest snapshot per project; space is reclaimed by the next prune", m.MaxTotalSizeMB))
	}
}

// Renders checkpoints as the numbered rollback listing with its usage footer
func FormatCheckpointList(checkpoints []CheckpointEntry, directory string) string {
	if len(checkpoints) == 0 {
		return "No checkpoints found for " + directory
	}
	lines := []string{"\U0001F4F8 Checkpoints for " + directory + ":\n"}
	for i, cp := range checkpoints {
		ts := cp.Timestamp
		if strings.Contains(ts, "T") {
			clock := strings.SplitN(ts, "T", 3)[1]
			clock = strings.Split(clock, "+")[0]
			clock = strings.Split(clock, "-")[0]
			clock = string([]rune(clock)[:min(5, len([]rune(clock)))])
			ts = strings.Split(cp.Timestamp, "T")[0] + " " + clock
		}
		stat := ""
		if cp.FilesChanged != 0 {
			plural := "s"
			if cp.FilesChanged == 1 {
				plural = ""
			}
			stat = fmt.Sprintf("  (%d file%s, +%d/-%d)", cp.FilesChanged, plural, cp.Insertions, cp.Deletions)
		}
		if cp.Workdir != "" && directory == "all directories" {
			short := filepath.Base(cp.Workdir)
			if short == "" || short == "." || short == string(filepath.Separator) {
				short = cp.Workdir
			}
			lines = append(lines, fmt.Sprintf("  %d. %s  %s  [%s]  %s%s", i+1, cp.ShortHash, ts, short, cp.Reason, stat))
		} else {
			lines = append(lines, fmt.Sprintf("  %d. %s  %s  %s%s", i+1, cp.ShortHash, ts, cp.Reason, stat))
		}
	}
	lines = append(lines,
		"\n  /rollback <N>             restore to checkpoint N",
		"  /rollback diff <N>        preview changes since checkpoint N",
		"  /rollback <N> <file>      restore a single file from checkpoint N")
	return strings.Join(lines, "\n")
}
