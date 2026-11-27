// Package checkpoints keeps snapshots of the projects the agent changes, in one shadow git store per profile beside the projects
// themselves: a ref of snapshots for each project, taken before the agent writes, edits or deletes its files, restored by /rollback
// keeping the edits the user made since, and kept within a count per project and a size for the store
package checkpoints

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	checkpointsconfig "github.com/nickheyer/icebath/internal/checkpoints/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/git"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Layout of the checkpoint folder of a profile
const (
	// The shared shadow store, a bare repository
	storeDir = "store"
	// Namespace of the refs holding each project's snapshots
	refPrefix = "refs/icebath"
	// Git index, metadata and agent write ledger of each project, by its hash, inside the store
	indexDir   = "indexes"
	projectDir = "projects"
	ledgerDir  = "ledgers"
	// Archives of whatever else was found in the folder when the store was first made
	legacyPrefix = "legacy-"
	// Time of the last automatic cleanup
	pruneMarker = ".last_prune"
	// Left by a rewrite of a project's history whose garbage collection waits for the next cleanup
	gcPending = ".gc-pending"
	// Newest agent writes a project's ledger keeps
	ledgerMax = 2000
	// Projects with more files than this are not snapshotted
	maxFiles = 50000
)

// Patterns every snapshot leaves out: dependencies, build output, caches, environments, other repositories, binaries, media, archives,
// secrets and logs
var excludes = []string{
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

// Markers whose presence makes a folder a project
var markers = []string{".git", "pyproject.toml", "package.json", "Cargo.toml", "go.mod", "Makefile", "pom.xml", ".hg", "Gemfile"}

// Checkpoints of one profile: its shadow store and the limits its settings set
type Store struct {
	base      string
	enabled   bool
	snapshots int
	// Megabytes of the whole store and of one file, none for zero
	totalMB int
	fileMB  int
	// Longest one git step may take
	timeout time.Duration
}

// Folder of the profile's checkpoints
func Base(cfg *config.Config) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), "checkpoints")
}

// Checkpoints of the profile the settings are of
func Open(cfg *config.Config) *Store {
	return &Store{
		base:      Base(cfg),
		enabled:   cfg.GetBool(checkpointsconfig.Enabled),
		snapshots: max(1, cfg.GetInt(checkpointsconfig.MaxSnapshots)),
		totalMB:   max(0, cfg.GetInt(checkpointsconfig.MaxTotalSize)),
		fileMB:    max(0, cfg.GetInt(checkpointsconfig.MaxFileSize)),
		timeout:   gitTimeout(cfg),
	}
}

// Longest one git step of the profile's checkpoints may take, ten to sixty seconds
func gitTimeout(cfg *config.Config) time.Duration {
	return time.Duration(min(60, max(10, cfg.GetInt(checkpointsconfig.GitTimeout)))) * time.Second
}

// Whether the settings ask for snapshots
func (s *Store) Enabled() bool { return s.enabled }

func (s *Store) store() string { return filepath.Join(s.base, storeDir) }

// Whether the store was made
func (s *Store) made() bool { return fsx.Exists(filepath.Join(s.store(), "HEAD")) }

// Absolute path with a leading ~ expanded and its links resolved as far as it exists
func normalize(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	return resolve(abs)
}

// Path with the links of the part of it that exists resolved
func resolve(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(resolve(parent), filepath.Base(path))
}

// Key of a project's ref, index, metadata and ledger: the start of the sha256 of its folder
func hash(dir string) string {
	sum := sha256.Sum256([]byte(normalize(dir)))
	return hex.EncodeToString(sum[:])[:16]
}

func ref(project string) string { return refPrefix + "/" + project }

func (s *Store) index(project string) string { return filepath.Join(s.store(), indexDir, project) }

func (s *Store) meta(project string) string {
	return filepath.Join(s.store(), projectDir, project+".json")
}

func (s *Store) ledgerPath(project string) string {
	return filepath.Join(s.store(), ledgerDir, project+".json")
}

// The user's home folder, empty when not known
func home() string {
	if dir, err := os.UserHomeDir(); err == nil && dir != "" {
		return filepath.Clean(dir)
	}
	return ""
}

// Lock file of the whole store, beside its folder so clearing the folder never replaces it
func lockPath(base string) string {
	base = resolve(base)
	return filepath.Join(filepath.Dir(base), "."+filepath.Base(base)+".lock")
}

// Longest a call waits for another to let go of the store, such as a snapshot of another session of this or another process
const lockWait = 30 * time.Second

// Takes the lock of the whole store, waiting a while for another holder to let go: its release, or an error when it doesn't
func lock(base string) (func(), error) {
	release, err := fsx.Lock(context.Background(), lockPath(base), lockWait)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return nil, fmt.Errorf("checkpoint store is busy: %s", base)
	}
	return release, err
}

// What one git step against the store is given beyond the store and the project
type step struct {
	// Longest it may take, the store's own limit for zero
	timeout time.Duration
	// Git index of the project it reads and writes, none for the store's own
	index string
	// Exit statuses that are no failure worth logging
	allowed []int
	env     map[string]string
}

// Environment pointing git at the store and the project with the user's config and every injected setting left out
func environment(store, workdir, index string) map[string]string {
	env := git.Process()
	for key := range env {
		if key == "GIT_NAMESPACE" || key == "GIT_ALTERNATE_OBJECT_DIRECTORIES" || key == "GIT_INDEX_FILE" || key == "GIT_CONFIG_PARAMETERS" ||
			key == "GIT_CONFIG_COUNT" || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			delete(env, key)
		}
	}
	maps.Copy(env, map[string]string{"GIT_DIR": store, "GIT_WORK_TREE": workdir, "GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_CONFIG_SYSTEM": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"})
	if index != "" {
		env["GIT_INDEX_FILE"] = index
	}
	return env
}

// Text output of git, its line ends made \n
func text(out string) string {
	return strings.ReplaceAll(strings.ReplaceAll(out, "\r\n", "\n"), "\r", "\n")
}

// Runs git against the store with the project's folder as its worktree: whether it succeeded, its output trimmed, or left as it is for
// a run that separates with NUL, and its errors
func (s *Store) git(workdir string, st step, args ...string) (bool, string, string) {
	wd := normalize(workdir)
	shown := "git " + strings.Join(args, " ")
	if info, err := os.Stat(wd); err != nil || !info.IsDir() {
		reason := "working directory not found: " + wd
		if err == nil {
			reason = "working directory is not a directory: " + wd
		}
		slog.Error("Git command skipped: "+shown, "reason", reason)
		return false, "", reason
	}
	if _, err := exec.LookPath("git"); err != nil {
		return false, "", "git is not installed or not on PATH"
	}
	env := environment(s.store(), wd, st.index)
	maps.Copy(env, st.env)
	timeout := st.timeout
	if timeout <= 0 {
		timeout = s.timeout
	}
	result, err := git.Run(context.Background(), wd, env, timeout, nil, args...)
	if timed, ok := errors.AsType[*git.TimeoutError](err); ok {
		reason := fmt.Sprintf("git timed out after %ds: %s", int(timed.Timeout/time.Second), shown)
		slog.Error(reason)
		return false, "", reason
	}
	if err != nil {
		slog.Error("Unexpected git error running "+shown, "error", err)
		return false, "", err.Error()
	}
	ok := result.Code == 0
	out, errs := result.Stdout, strings.TrimSpace(text(result.Stderr))
	if !slices.Contains(args, "-z") {
		out = strings.TrimSpace(text(out))
	}
	if !ok && !slices.Contains(st.allowed, result.Code) {
		slog.Error("Git command failed: "+shown, "code", result.Code, "stderr", errs)
	}
	return ok, out, errs
}

// Output of a git step that succeeded, empty otherwise
func (s *Store) out(workdir string, st step, args ...string) string {
	ok, out, _ := s.git(workdir, st, args...)
	if !ok {
		return ""
	}
	return out
}

// Snapshot a project's ref points at, empty before its first
func (s *Store) tip(workdir, name string) string {
	return s.out(workdir, step{allowed: []int{128}}, "rev-parse", "--verify", name+"^{commit}")
}

// Makes the store once, first moving whatever else is in the folder into an archive: a failure the user reads, or an error of the disk
func (s *Store) init() (string, error) {
	store := s.store()
	if !fsx.Exists(store) {
		if err := os.MkdirAll(s.base, 0o700); err != nil {
			return "Could not create checkpoint base: " + err.Error(), nil
		}
		if err := s.archiveStrays(); err != nil {
			return "", err
		}
	}
	if s.made() {
		return "", nil
	}
	for _, dir := range []string{store, filepath.Join(store, indexDir), filepath.Join(store, projectDir)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "Shadow store init failed: git is not installed or not on PATH", nil
	}
	env := git.Process()
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_NAMESPACE", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		delete(env, key)
	}
	maps.Copy(env, map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"})
	result, err := git.Run(context.Background(), s.base, env, s.timeout, nil, "init", "--bare", store)
	if _, ok := errors.AsType[*git.TimeoutError](err); ok {
		return fmt.Sprintf("Shadow store init failed: git init --bare %s timed out after %d seconds", store, int(s.timeout/time.Second)), nil
	}
	if err != nil {
		return "Shadow store init failed: " + err.Error(), nil
	}
	if result.Code != 0 {
		return "Shadow store init failed: " + strings.TrimSpace(text(result.Stderr)), nil
	}
	for _, setting := range [][2]string{{"user.email", "icebath@local"}, {"user.name", "Icebath Checkpoint"}, {"commit.gpgsign", "false"},
		{"tag.gpgSign", "false"}, {"gc.auto", "0"}} {
		s.git(s.base, step{}, "config", setting[0], setting[1])
	}
	if err := os.MkdirAll(filepath.Join(store, "info"), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(store, "info", "exclude"), []byte(strings.Join(excludes, "\n")+"\n"), 0o600); err != nil {
		return "", err
	}
	slog.Debug("Initialised checkpoint store at " + store)
	return "", nil
}

// Moves whatever is in the checkpoint folder besides the store, the cleanup marker and earlier archives into an archive of its own
func (s *Store) archiveStrays() error {
	entries, err := os.ReadDir(s.base)
	if err != nil {
		return err
	}
	archive := ""
	for _, entry := range entries {
		name := entry.Name()
		if name == storeDir || name == pruneMarker || strings.HasPrefix(name, legacyPrefix) {
			continue
		}
		if archive == "" {
			archive = filepath.Join(s.base, legacyPrefix+time.Now().Format("20060102-150405"))
			if err := os.MkdirAll(archive, 0o700); err != nil {
				slog.Warn("Could not create legacy archive dir", "error", err)
				return nil
			}
		}
		if err := os.Rename(filepath.Join(s.base, name), filepath.Join(archive, name)); err != nil {
			slog.Warn("Could not archive "+filepath.Join(s.base, name), "error", err)
		}
	}
	if archive != "" {
		slog.Info("Moved what the checkpoint folder held into " + archive + ". Clear with `icebath checkpoints clear-legacy` when safe.")
	}
	return nil
}

// What is kept of a project snapshotted into the store
type project struct {
	Workdir string  `json:"workdir"`
	Created float64 `json:"created_at"`
	Touched float64 `json:"last_touch"`
	// Device and inode of the folder holding the project, which tells a project deleted from one moved or on a volume not mounted now
	ParentDev uint64 `json:"workdir_parent_dev,omitempty"`
	ParentIno uint64 `json:"workdir_parent_ino,omitempty"`
	// Key it is kept under
	Hash string `json:"-"`
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Records the project as snapshotted now, keeping when it was first and the identity of its parent folder when that can't be read now
func (s *Store) touch(workdir string) {
	wd := normalize(workdir)
	path := s.meta(hash(wd))
	var meta project
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &meta)
	}
	meta.Workdir, meta.Touched = wd, now()
	if meta.Created == 0 {
		meta.Created = meta.Touched
	}
	if fsx.Exists(wd) {
		if dev, ino, err := identity(filepath.Dir(filepath.Clean(wd))); err == nil && dev != 0 && ino != 0 {
			meta.ParentDev, meta.ParentIno = dev, ino
		}
	}
	data, err := json.Marshal(meta)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			err = os.WriteFile(path, data, 0o600)
		}
	}
	if err != nil {
		slog.Debug("Could not write project metadata "+path, "error", err)
	}
}

// Every project of the store
func (s *Store) projects() []project {
	dir := filepath.Join(s.store(), projectDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []project
	for _, entry := range entries {
		stem, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || stem == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var meta project
		if json.Unmarshal(data, &meta) != nil {
			continue
		}
		meta.Hash = stem
		out = append(out, meta)
	}
	return out
}

// One file the agent wrote: the sha256 of what it wrote, empty for a file it deleted, and when
type write struct {
	SHA256 string  `json:"sha256"`
	TS     float64 `json:"ts"`
}

// What the agent wrote in the project, by path
func (s *Store) ledger(project string) map[string]write {
	ledger := map[string]write{}
	if data, err := os.ReadFile(s.ledgerPath(project)); err == nil {
		json.Unmarshal(data, &ledger)
	}
	return ledger
}

// Keeps the ledger, its newest entries alone when it holds more than it may
func (s *Store) keep(project string, ledger map[string]write) {
	if len(ledger) > ledgerMax {
		paths := slices.SortedFunc(maps.Keys(ledger), func(a, b string) int {
			if ledger[a].TS != ledger[b].TS {
				if ledger[a].TS > ledger[b].TS {
					return -1
				}
				return 1
			}
			return strings.Compare(a, b)
		})
		capped := make(map[string]write, ledgerMax)
		for _, path := range paths[:ledgerMax] {
			capped[path] = ledger[path]
		}
		ledger = capped
	}
	path := s.ledgerPath(project)
	data, err := json.Marshal(ledger)
	if err == nil {
		err = fsx.Write(path, data, 0o600)
	}
	if err != nil {
		slog.Debug("Failed to save agent-write ledger for "+project, "error", err)
	}
}

// Entries under a folder, counted up to one past the most a snapshot takes
func count(dir string) int {
	n := 0
	stop := errors.New("stop")
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if path == dir || err != nil && d == nil {
			return nil
		}
		if n++; n > maxFiles {
			return stop
		}
		if err != nil && d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
	return n
}

// Bytes of every regular file under a folder, linked files counted at the size of their target
func size(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if path == dir {
			return nil
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Pathspecs keeping the checkpoint folder and its lock out of a git step over a project that holds them, none when it doesn't
func (s *Store) outside(workdir string) []string {
	base, wd := normalize(s.base), normalize(workdir)
	rel, err := filepath.Rel(wd, base)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return nil
	}
	lock := filepath.Join(filepath.Dir(rel), "."+filepath.Base(rel)+".lock")
	return []string{"--", ":(exclude)" + filepath.ToSlash(rel), ":(exclude)" + filepath.ToSlash(lock)}
}

// Removes a tree, giving its owner write and search rights on a path refused once before trying it again, refusing a link at its root
func removeTree(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &os.PathError{Op: "remove", Path: path, Err: errors.New("Cannot remove a tree through a symbolic link")}
	}
	return removeDir(path)
}

func removeDir(dir string) error {
	var names []string
	if err := retry(dir, func() error {
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		names, err = f.Readdirnames(-1)
		return err
	}); err != nil {
		return err
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if info, err := os.Lstat(path); err == nil && info.IsDir() {
			if err := removeDir(path); err != nil {
				return err
			}
			continue
		}
		if err := retry(path, func() error { return os.Remove(path) }); err != nil {
			return err
		}
	}
	return retry(dir, func() error { return os.Remove(dir) })
}

// Runs the operation again once after granting the owner write and search rights on the path and its parent, for a refusal only
func retry(path string, op func() error) error {
	err := op()
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	for _, target := range []string{filepath.Dir(path), path} {
		if info, err := os.Stat(target); err == nil {
			os.Chmod(target, info.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)|0o300)
		}
	}
	return op()
}

// Said when snapshots are off for a session
const NotEnabled = "Checkpoints are not enabled.\nTurn on File checkpoints in /settings under Tools › Checkpoints, or start icebath with --checkpoints."

// Checkpoints as one session sees them: its profile's store, the project its folder lies in, and why snapshots taken on this machine
// don't serve its terminal backend, empty when they do
type Scope struct {
	*Store
	Directory string
	Refusal   string
}

// Checkpoints of a session of the profile working in the folder, with the terminal backend the profile picked
func For(cfg *config.Config, directory string, host *terminal.Terminal) *Scope {
	store := Open(cfg)
	scope := &Scope{Store: store, Directory: store.Project(directory)}
	if backend, err := host.Configured(cfg); err == nil {
		scope.Refusal = Refusal(backend)
	}
	return scope
}
