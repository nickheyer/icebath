package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Layout of the shared shadow store under the checkpoint base
const (
	checkpointStoreDirname    = "store"
	checkpointRefsPrefix      = "refs/icebath"
	checkpointIndexesDirname  = "indexes"
	checkpointProjectsDirname = "projects"
	checkpointLedgersDirname  = "ledgers"
	checkpointLegacyPrefix    = "legacy-"
	checkpointPruneMarkerName = ".last_prune"
	checkpointLedgerMaxItems  = 2000
	checkpointMaxFiles        = 50000
	checkpointWorkdirMarker   = "ICEBATH_WORKDIR"
)

// Patterns every snapshot leaves out, written to the store's info exclude file
var checkpointDefaultExcludes = []string{
	"node_modules/", "dist/", "build/", "target/", "out/", ".next/", ".nuxt/",
	"__pycache__/", "*.pyc", "*.pyo", ".cache/", ".pytest_cache/", ".mypy_cache/", ".ruff_cache/", "coverage/", ".coverage",
	".venv/", "venv/", "env/",
	".git/", ".hg/", ".svn/",
	".worktrees/",
	"*.so", "*.dylib", "*.dll", "*.o", "*.a", "*.jar", "*.class", "*.exe", "*.obj",
	"*.mp4", "*.mov", "*.mkv", "*.webm", "*.zip", "*.tar", "*.tar.gz", "*.tgz", "*.7z", "*.rar", "*.iso",
	".env", ".env.*", ".env.local", ".env.*.local",
	".DS_Store", "Thumbs.db",
	"*.log",
}

// Git timeout in seconds of one checkpoint git call, set at package init from the environment
var checkpointGitTimeoutSecs = 30

// Default git timeout of one checkpoint git call
func checkpointGitTimeout() time.Duration {
	return time.Duration(checkpointGitTimeoutSecs) * time.Second
}

// Checkpoint base of the active profile at call time, swapped by tests
var checkpointBaseFn = func(env *Env) string {
	return filepath.Join(envHome(env), "checkpoints")
}

// The checkpoint root of the profile the env belongs to
func CheckpointBase(env *Env) string { return checkpointBaseFn(env) }

var checkpointCommitHashRe = regexp.MustCompile(`\A[0-9a-fA-F]{4,64}\n?\z`)

// Error text for a commit hash that is empty, flag shaped or not hex, empty when valid
func checkpointValidateCommitHash(commitHash string) string {
	if commitHash == "" || pyStrip(commitHash) == "" {
		return "Empty commit hash"
	}
	if strings.HasPrefix(commitHash, "-") {
		return "Invalid commit hash (must not start with '-'): " + pyReprStr(commitHash)
	}
	if !checkpointCommitHashRe.MatchString(commitHash) {
		return "Invalid commit hash (expected 4-64 hex characters): " + pyReprStr(commitHash)
	}
	return ""
}

// Error text for a file path that is empty, absolute or escapes the working directory, empty when valid
func checkpointValidateFilePath(filePath, workingDir string) string {
	if filePath == "" || pyStrip(filePath) == "" {
		return "Empty file path"
	}
	if filepath.IsAbs(filePath) {
		return "File path must be relative, got absolute path: " + pyReprStr(filePath)
	}
	absWorkdir := checkpointNormalizePath(workingDir)
	resolved := checkpointResolve(absWorkdir + string(filepath.Separator) + filePath)
	if _, ok := relativeTo(resolved, absWorkdir); !ok {
		return "File path escapes the working directory via traversal: " + pyReprStr(filePath)
	}
	return ""
}

// Canonical absolute path with the tilde expanded and symlinks resolved
func checkpointNormalizePath(p string) string {
	return checkpointResolve(expandTilde(p))
}

// Path resolve semantics, symlinks before parent references on POSIX and lexical parents first on Windows
func checkpointResolve(p string) string {
	if runtime.GOOS == "windows" {
		return resolveNonStrict(p)
	}
	return pyRealpath(p)
}

// First sixteen hex digits of the sha256 of the canonical working directory
func checkpointProjectHash(workingDir string) string {
	sum := sha256.Sum256([]byte(checkpointNormalizePath(workingDir)))
	return hex.EncodeToString(sum[:])[:16]
}

// Shared store directory under a checkpoint base
func checkpointStorePath(base string) string {
	return filepath.Join(base, checkpointStoreDirname)
}

// Reports whether the store has been initialised
func checkpointStoreHasHead(store string) bool {
	return pathExistsFollow(filepath.Join(store, "HEAD"))
}

// Per project git index inside the store
func checkpointIndexPath(store, dirHash string) string {
	return filepath.Join(store, checkpointIndexesDirname, dirHash)
}

// Per project agent write ledger inside the store
func checkpointLedgerPath(store, dirHash string) string {
	return filepath.Join(store, checkpointLedgersDirname, dirHash+".json")
}

// Per project metadata file inside the store
func checkpointProjectMetaPath(store, dirHash string) string {
	return filepath.Join(store, checkpointProjectsDirname, dirHash+".json")
}

// Ref that holds a project's checkpoint history
func checkpointRefName(dirHash string) string {
	return checkpointRefsPrefix + "/" + dirHash
}

// Streaming sha256 hex digest of a file, empty when it cannot be read
func checkpointHashFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// JSON document of a file read as UTF-8 with an optional byte order mark, integers kept exact
func readCheckpointJSON(path string) (any, error) {
	text, err := readUTF8SigText(path)
	if err != nil {
		return nil, err
	}
	return checkpointJSONDecode(text)
}

// Agent write ledger of a project, path to sha256 and timestamp, empty when missing or malformed
func loadCheckpointLedger(store, dirHash string) map[string]any {
	v, err := readCheckpointJSON(checkpointLedgerPath(store, dirHash))
	if err != nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Persists the ledger capped to its newest entries, logging write failures and returning value errors
func saveCheckpointLedger(env *Env, store, dirHash string, ledger map[string]any) error {
	if len(ledger) > checkpointLedgerMaxItems {
		type item struct {
			key string
			ts  float64
		}
		items := make([]item, 0, len(ledger))
		for k, v := range ledger {
			ts := 0.0
			if entry, ok := v.(map[string]any); ok {
				if raw, has := entry["ts"]; has {
					f, err := checkpointMetaFloat(raw)
					if err != nil {
						return err
					}
					ts = f
				}
			}
			items = append(items, item{k, ts})
		}
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].ts != items[j].ts {
				return items[i].ts > items[j].ts
			}
			return items[i].key < items[j].key
		})
		capped := make(map[string]any, checkpointLedgerMaxItems)
		for _, it := range items[:checkpointLedgerMaxItems] {
			capped[it.key] = ledger[it.key]
		}
		ledger = capped
	}
	path := checkpointLedgerPath(store, dirHash)
	data, err := checkpointJSONEncode(ledger)
	if err != nil {
		return err
	}
	if err := checkpointAtomicWrite(path, []byte(data)); err != nil {
		toolsLogger(env).Debug(fmt.Sprintf("Failed to save agent-write ledger for %s: %v", dirHash, err))
	}
	return nil
}

// Writes through a sibling temp file then replaces the target
func checkpointAtomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Commands that select the git binary, swapped by tests
var checkpointWhich = whichInPath

// Environment the package manager selects for a tool, swapped by tests
var checkpointPMEnsure = pmEnsureEnv

// Package manager git environment over the base, or the base itself when the selection fails
func selectedGitEnv(base map[string]string) map[string]string {
	env := make(map[string]string, len(base))
	for k, v := range base {
		env[k] = v
	}
	selected, err := checkpointPMEnsure("git", env)
	if err != nil || selected == nil {
		return env
	}
	return selected
}

// Environment that points git at the shared store and the project worktree with user config neutralised
func checkpointGitEnv(env *Env, store, workingDir, indexFile string) (map[string]string, error) {
	wd := checkpointNormalizePath(workingDir)
	base, err := BuildSubprocessEnv(context.Background(), env, SubprocessEnvOptions{})
	if err != nil {
		return nil, err
	}
	g := selectedGitEnv(base)
	g["GIT_DIR"] = store
	g["GIT_WORK_TREE"] = wd
	delete(g, "GIT_NAMESPACE")
	delete(g, "GIT_ALTERNATE_OBJECT_DIRECTORIES")
	if indexFile != "" {
		g["GIT_INDEX_FILE"] = indexFile
	} else {
		delete(g, "GIT_INDEX_FILE")
	}
	g["GIT_CONFIG_GLOBAL"] = os.DevNull
	g["GIT_CONFIG_SYSTEM"] = os.DevNull
	g["GIT_CONFIG_NOSYSTEM"] = "1"
	return g, nil
}

// Git runner against the shared store, swapped by tests
var runCheckpointGit = runCheckpointGitProcess

// Text mode output of a child, invalid bytes replaced and newlines translated
func checkpointTextOutput(b []byte) string {
	s := decodeUTF8Replace(b)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// Runs git against the store with the project worktree, ok with stdout and stderr, NUL output left raw
func runCheckpointGitProcess(env *Env, args []string, store, workingDir string, o checkpointGitOpts) (bool, string, string) {
	log := toolsLogger(env)
	wd := checkpointNormalizePath(workingDir)
	shown := "git " + strings.Join(args, " ")
	st, err := os.Stat(wd)
	if err != nil {
		msg := "working directory not found: " + wd
		log.Error(fmt.Sprintf("Git command skipped: %s (%s)", shown, msg))
		return false, "", msg
	}
	if !st.IsDir() {
		msg := "working directory is not a directory: " + wd
		log.Error(fmt.Sprintf("Git command skipped: %s (%s)", shown, msg))
		return false, "", msg
	}
	gitEnv, err := checkpointGitEnv(env, store, wd, o.IndexFile)
	if err != nil {
		log.Error(fmt.Sprintf("Unexpected git error running %s: %v", shown, err))
		return false, "", err.Error()
	}
	for k, v := range o.ExtraEnv {
		gitEnv[k] = v
	}
	git := checkpointWhich("git", gitEnv["PATH"])
	if git == "" {
		return false, "", "git is not installed or not on PATH"
	}
	argv := append([]string{git}, args...)
	cmdText := strings.Join(argv, " ")
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = time.Duration(checkpointGitTimeoutSecs) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = wd
	cmd.Env = mapToEnviron(gitEnv)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	hideConsoleWindow(cmd)
	proc.Group(cmd)
	cmd.Cancel = func() error { return proc.Kill(cmd) }
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			var pe *fs.PathError
			if errors.As(err, &pe) && pe.Path == "git" {
				log.Error("Git executable not found: " + cmdText)
				return false, "", "git not found"
			}
			msg := "working directory not found: " + wd
			log.Error(fmt.Sprintf("Git command failed before execution: %s (%s)", cmdText, msg))
			return false, "", msg
		}
		log.Error(fmt.Sprintf("Unexpected git error running %s: %v", cmdText, err))
		return false, "", pyOSErrorText(err)
	}
	werr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		msg := fmt.Sprintf("git timed out after %ds: %s", int(timeout/time.Second), cmdText)
		log.Error(msg)
		return false, "", msg
	}
	code := 0
	if werr != nil {
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			log.Error(fmt.Sprintf("Unexpected git error running %s: %v", cmdText, werr))
			return false, "", werr.Error()
		}
		code = ee.ExitCode()
	}
	ok := code == 0
	nul := slices.Contains(args, "-z")
	var out string
	if nul {
		out = stdout.String()
	} else {
		out = pyStrip(checkpointTextOutput(stdout.Bytes()))
	}
	var errText string
	if nul {
		errText = pyStrip(decodeUTF8Replace(stderr.Bytes()))
	} else {
		errText = pyStrip(checkpointTextOutput(stderr.Bytes()))
	}
	if !ok && !o.Allowed[code] {
		log.Error(fmt.Sprintf("Git command failed: %s (rc=%d) stderr=%s", cmdText, code, errText))
	}
	return ok, out, errText
}

// Git runner closure bound to an env for the pruner
func checkpointGitRunner(env *Env) checkpointGitFunc {
	return func(args []string, store, cwd string, o checkpointGitOpts) (bool, string, string) {
		return runCheckpointGit(env, args, store, cwd, o)
	}
}

// Stdout of a successful git call, empty otherwise
func checkpointGitOut(env *Env, args []string, store, workingDir string, allowed map[int]bool) string {
	ok, out, _ := runCheckpointGit(env, args, store, workingDir, checkpointGitOpts{Allowed: allowed})
	if !ok {
		return ""
	}
	return out
}

// Commit at the ref, empty when the ref does not exist yet
func checkpointRefTip(env *Env, store, workingDir, ref string) string {
	return checkpointGitOut(env, []string{"rev-parse", "--verify", ref + "^{commit}"}, store, workingDir, map[int]bool{128: true})
}

// Every project ref in the store
func listCheckpointProjectRefs(env *Env, store, workingDir string) []string {
	out := checkpointGitOut(env, []string{"for-each-ref", "--format=%(refname)", checkpointRefsPrefix}, store, workingDir, map[int]bool{128: true})
	var refs []string
	for _, r := range pySplitLines(out) {
		if pyStrip(r) != "" {
			refs = append(refs, r)
		}
	}
	return refs
}

// Removes a file ignoring every failure
func checkpointUnlinkQuiet(path string) {
	os.Remove(path)
}

// Moves pre v2 per project shadow repos and strays into a legacy archive, returning its path or empty
func migrateLegacyCheckpointStore(env *Env, base string) (string, error) {
	if !pathExistsFollow(base) {
		return "", nil
	}
	log := toolsLogger(env)
	children, err := checkpointBaseChildren(base)
	if err != nil {
		return "", err
	}
	legacyRoot := ""
	for _, child := range children {
		name := filepath.Base(child)
		if name == checkpointStoreDirname || name == checkpointPruneMarkerName || strings.HasPrefix(name, checkpointLegacyPrefix) {
			continue
		}
		if legacyRoot == "" {
			legacyRoot = filepath.Join(base, checkpointLegacyPrefix+time.Now().Format("20060102-150405"))
			if err := os.MkdirAll(legacyRoot, 0o777); err != nil {
				log.Warn(fmt.Sprintf("Could not create legacy archive dir: %s", pyOSErrorText(err)))
				return "", nil
			}
		}
		if err := os.Rename(child, filepath.Join(legacyRoot, name)); err != nil {
			log.Warn(fmt.Sprintf("Could not archive legacy checkpoint %s: %s", child, pyOSErrorText(err)))
		}
	}
	if legacyRoot != "" {
		log.Info(fmt.Sprintf("Migrated pre-v2 checkpoint repos to %s. Clear with `icebath checkpoints clear-legacy` when safe.", legacyRoot))
	}
	return legacyRoot, nil
}

// Initialises the shared store once, migrating pre v2 repos first, returning an error text or an escaped I/O error
func initCheckpointStore(env *Env, store, workingDir string) (string, error) {
	base := filepath.Dir(store)
	if !pathExistsFollow(store) {
		if err := os.MkdirAll(base, 0o777); err != nil {
			return "Could not create checkpoint base: " + pyOSErrorText(err), nil
		}
		if _, err := migrateLegacyCheckpointStore(env, base); err != nil {
			return "", err
		}
	}
	if pathExistsFollow(filepath.Join(store, "HEAD")) {
		return "", nil
	}
	if err := os.MkdirAll(store, 0o777); err != nil {
		return "", err
	}
	for _, d := range []string{checkpointIndexesDirname, checkpointProjectsDirname} {
		if err := os.Mkdir(filepath.Join(store, d), 0o777); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	base0, err := BuildSubprocessEnv(context.Background(), env, SubprocessEnvOptions{})
	if err != nil {
		return "", err
	}
	initEnv := selectedGitEnv(base0)
	initEnv["GIT_CONFIG_GLOBAL"] = os.DevNull
	initEnv["GIT_CONFIG_SYSTEM"] = os.DevNull
	initEnv["GIT_CONFIG_NOSYSTEM"] = "1"
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_NAMESPACE", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		delete(initEnv, k)
	}
	git := checkpointWhich("git", initEnv["PATH"])
	if git == "" {
		return "Shadow store init failed: git is not installed or not on PATH", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(checkpointGitTimeoutSecs)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "init", "--bare", store)
	cmd.Env = mapToEnviron(initEnv)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	hideConsoleWindow(cmd)
	proc.Group(cmd)
	cmd.Cancel = func() error { return proc.Kill(cmd) }
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return "Shadow store init failed: " + pyOSErrorText(err), nil
		}
		return "", err
	}
	werr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("Shadow store init failed: Command '%s' timed out after %d seconds",
			pyArgvRepr([]string{git, "init", "--bare", store}), checkpointGitTimeoutSecs), nil
	}
	if werr != nil {
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			return "", werr
		}
		return "Shadow store init failed: " + pyStrip(checkpointTextOutput(stderr.Bytes())), nil
	}
	for _, kv := range [][2]string{
		{"user.email", "icebath@local"},
		{"user.name", "Icebath Checkpoint"},
		{"commit.gpgsign", "false"},
		{"tag.gpgSign", "false"},
		{"gc.auto", "0"},
	} {
		runCheckpointGit(env, []string{"config", kv[0], kv[1]}, store, base, checkpointGitOpts{})
	}
	infoDir := filepath.Join(store, "info")
	if err := os.Mkdir(infoDir, 0o777); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(infoDir, "exclude"), []byte(strings.Join(checkpointDefaultExcludes, "\n")+"\n"), 0o666); err != nil {
		return "", err
	}
	toolsLogger(env).Debug("Initialised checkpoint store at " + store)
	return "", nil
}

// Device and inode probe of the workdir parent, swapped by tests
var checkpointParentIdentity = checkpointDevIno

// Parent directory identity of a reachable workdir, empty when unreachable or without a usable identity
func checkpointVolumeEvidence(workdir string) map[string]any {
	if !pathExistsFollow(workdir) {
		return map[string]any{}
	}
	dev, ino, err := checkpointParentIdentity(filepath.Dir(filepath.Clean(workdir)))
	if err != nil || dev == 0 || ino == 0 {
		return map[string]any{}
	}
	return map[string]any{"workdir_parent_dev": dev, "workdir_parent_ino": ino}
}

// Current time in Unix seconds with sub second precision
func checkpointNow() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// Writes project metadata, logging a failure instead of returning it
func writeCheckpointMeta(env *Env, path string, meta map[string]any, verb string) {
	data, err := checkpointJSONEncode(meta)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o777); err == nil {
			err = os.WriteFile(path, []byte(data), 0o666)
		}
	}
	if err != nil {
		toolsLogger(env).Debug(fmt.Sprintf("Could not %s project metadata %s: %v", verb, path, err))
	}
}

// Creates or rewrites the project metadata keeping created_at and any recorded parent identity
func registerCheckpointProject(env *Env, store, workingDir string) {
	dirHash := checkpointProjectHash(workingDir)
	metaPath := checkpointProjectMetaPath(store, dirHash)
	now := checkpointNow()
	wd := checkpointNormalizePath(workingDir)
	meta := map[string]any{"workdir": wd, "created_at": now, "last_touch": now}
	evidence := checkpointVolumeEvidence(wd)
	for k, v := range evidence {
		meta[k] = v
	}
	if pathExistsFollow(metaPath) {
		if v, err := readCheckpointJSON(metaPath); err == nil {
			if existing, ok := v.(map[string]any); ok {
				if created, has := existing["created_at"]; has {
					meta["created_at"] = created
				}
				if len(evidence) == 0 {
					for _, key := range []string{"workdir_parent_dev", "workdir_parent_ino"} {
						if val, has := existing[key]; has {
							meta[key] = val
						}
					}
				}
			}
		}
	}
	writeCheckpointMeta(env, metaPath, meta, "write")
}

// Updates last_touch and the parent identity of a project, registering it when unknown
func touchCheckpointProject(env *Env, store, workingDir string) {
	dirHash := checkpointProjectHash(workingDir)
	metaPath := checkpointProjectMetaPath(store, dirHash)
	if !pathExistsFollow(metaPath) {
		registerCheckpointProject(env, store, workingDir)
		return
	}
	meta := map[string]any{}
	if v, err := readCheckpointJSON(metaPath); err == nil {
		if m, ok := v.(map[string]any); ok {
			meta = m
		}
	}
	wd := checkpointNormalizePath(workingDir)
	meta["workdir"] = wd
	meta["last_touch"] = checkpointNow()
	if _, has := meta["created_at"]; !has {
		meta["created_at"] = meta["last_touch"]
	}
	for k, v := range checkpointVolumeEvidence(wd) {
		meta[k] = v
	}
	data, err := checkpointJSONEncode(meta)
	if err == nil {
		err = os.WriteFile(metaPath, []byte(data), 0o666)
	}
	if err != nil {
		toolsLogger(env).Debug(fmt.Sprintf("Could not update project metadata %s: %v", metaPath, err))
	}
}

// Every registered project's metadata with its hash under the _hash key
func listCheckpointProjects(store string) []map[string]any {
	dir := filepath.Join(store, checkpointProjectsDirname)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return []map[string]any{}
	}
	out := []map[string]any{}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		stem := strings.TrimSuffix(name, ".json")
		if stem == "" {
			stem = name
		}
		v, err := readCheckpointJSON(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		meta, ok := v.(map[string]any)
		if !ok {
			continue
		}
		meta["_hash"] = stem
		out = append(out, meta)
	}
	return out
}

// One pre v2 per project shadow repo found directly under the base
type checkpointPreV2Repo struct {
	Path             string
	Workdir          *string
	Exists           bool
	MarkerUnreadable bool
}

// Pre v2 shadow repos under the base, the single scan shared by status previews and pruning
func preV2ShadowRepos(base string) ([]checkpointPreV2Repo, error) {
	out := []checkpointPreV2Repo{}
	if !pathExistsFollow(base) {
		return out, nil
	}
	children, err := checkpointBaseChildren(base)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		name := filepath.Base(child)
		if !isDirFollow(child) || name == checkpointStoreDirname || strings.HasPrefix(name, checkpointLegacyPrefix) {
			continue
		}
		if !pathExistsFollow(filepath.Join(child, "HEAD")) {
			continue
		}
		repo := checkpointPreV2Repo{Path: child}
		marker := filepath.Join(child, checkpointWorkdirMarker)
		if pathExistsFollow(marker) {
			if text, err := readUTF8SigText(marker); err == nil {
				wd := pyStrip(text)
				repo.Workdir = &wd
			} else {
				repo.MarkerUnreadable = true
			}
		}
		repo.Exists = repo.Workdir != nil && *repo.Workdir != "" && pathExistsFollow(*repo.Workdir)
		out = append(out, repo)
	}
	return out, nil
}

// Entry count under a directory, stopping once it passes the snapshot file limit
func checkpointDirFileCount(path string) int {
	count := 0
	errStop := errors.New("stop")
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if p == path {
			return nil
		}
		if err != nil && d == nil {
			return nil
		}
		count++
		if count > checkpointMaxFiles {
			return errStop
		}
		if err != nil && d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
	return count
}

// Recursive size in bytes of every regular file under the path, symlinked files counted at their target size
func checkpointDirSizeBytes(path string) int64 {
	var total int64
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if p == path {
			return nil
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			total += st.Size()
		}
		return nil
	})
	return total
}
