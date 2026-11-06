package diskcleanup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/tools"
)

const largeFileBytes = 500 * 1024 * 1024

var (
	resolvePath           = pypath.Resolve
	stdout      io.Writer = os.Stdout
	nowUTC                = func() time.Time { return time.Now().UTC() }
)

// Allowed tracking categories
var AllowedCategories = map[string]bool{
	"temp": true, "test": true, "research": true, "download": true, "chrome-profile": true, "cron-output": true, "other": true}

// Top level home dirs whose empty subdirs are never swept, user project trees included
var emptyDirProtectedTopLevel = map[string]bool{
	"logs": true, "memories": true, "sessions": true, "cron": true, "cronjobs": true,
	"cache": true, "skills": true, "plugins": true, "disk-cleanup": true, "optional-skills": true,
	"icebath-agent": true, "backups": true, "profiles": true, ".worktrees": true,
	"patches": true, "projects": true, "skins": true, "themes": true, "contributors": true,
	"workspace": true, "plans": true, "home": true, "kanban": true}

var emptyDirSweepPruneDirs = map[string]bool{
	".git": true, "node_modules": true, "venv": true, ".venv": true, "site-packages": true, "__pycache__": true}

// Top level home entries never auto tracked, state, secrets and user project trees
var neverTrackTopLevel = map[string]bool{
	"disk-cleanup": true, "logs": true, "memories": true, "sessions": true, "config.yaml": true,
	"skills": true, "plugins": true, ".env": true, "USER.md": true, "MEMORY.md": true, "SOUL.md": true,
	"auth.json": true, "icebath-agent": true,
	"patches": true, "projects": true, "skins": true, "themes": true, "contributors": true,
	"profiles": true, "backups": true, "optional-skills": true, "workspace": true, "plans": true, "home": true,
	"kanban": true}

// Stored categories revalidated before use with the note appended to the skip log line
var staleSkipNote = map[string]string{"cron-output": "", "test": " \u2014 under protected tree"}

var (
	testPatterns = []string{"test_", "tmp_"}
	testSuffixes = []string{".test.py", ".test.js", ".test.ts", ".test.md"}
)

// One tracked.json entry in its stored key order
type Entry = *pyjson.Object

// Result of a quick cleanup run
type Summary struct {
	Deleted   int
	EmptyDirs int
	Freed     float64
	Errors    []string
}

// Per category totals in first seen order
type CategoryTotal struct {
	Name  string
	Count int
	Size  float64
}

// One existing tracked path ranked by size
type TopItem struct {
	Path     string
	Size     float64
	Category string
}

// Category breakdown, ten largest existing tracked files and the tracked count
type Status struct {
	Categories   []CategoryTotal
	Top10        []TopItem
	TotalTracked int
}

// Path of a state file under the home disk-cleanup dir, outside logs
func stateFile(ctx context.Context, name string) string {
	return filepath.Join(home.Dir(ctx), "disk-cleanup", name)
}

// Components of a relative_to result, none for the root itself
func relParts(rel string) []string {
	if rel == "." || rel == "" {
		return nil
	}
	return strings.Split(rel, "/")
}

// Whether a file system entry exists, following symlinks
func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Whether p is a regular file after following symlinks
func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// Whether p is a directory after following symlinks
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Accepts only paths under the home or a top level icebath- entry of the resolved tmp root
func IsSafePath(ctx context.Context, path string) bool {
	h := home.Dir(ctx)
	resolved, err := resolvePath(path)
	if err != nil {
		return false
	}
	if _, ok := pypath.PathRelativeTo(resolved, h); ok {
		return true
	}
	tmp, err := resolvePath("/tmp")
	if err != nil {
		return false
	}
	rel, ok := pypath.PathRelativeTo(resolved, tmp)
	if !ok {
		return false
	}
	parts := relParts(rel)
	return len(parts) > 0 && strings.HasPrefix(parts[0], "icebath-")
}

// Appends a timestamped line to the audit log, dropping any OS error
func Log(ctx context.Context, message string) {
	logFile := stateFile(ctx, "cleanup.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o777); err != nil {
		return
	}
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", nowUTC().Format("2006-01-02 15:04:05"), message)
}

// JSON text of a utf-8-sig file, a decode failure reported as corruption
func readJSONSig(path string) (any, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
	if !utf8.Valid(data) {
		return nil, false, nil
	}
	v, err := pyjson.Loads(string(data))
	if err != nil {
		return nil, false, nil
	}
	return v, true, nil
}

// Entries of a decoded tracked list, refusing anything but a list of objects
func asEntries(v any) ([]Entry, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("TypeError: tracked.json holds %s, not a list of entries", pyrepr.TypeName(v))
	}
	out := make([]Entry, 0, len(list))
	for _, item := range list {
		o, ok := item.(*pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("TypeError: tracked.json entry is %s, not an object", pyrepr.TypeName(item))
		}
		out = append(out, o)
	}
	return out, nil
}

// Tracked entries from tracked.json, restored from the backup when the file is corrupt
func LoadTracked(ctx context.Context) ([]Entry, error) {
	tf := stateFile(ctx, "tracked.json")
	if err := os.MkdirAll(filepath.Dir(tf), 0o777); err != nil {
		return nil, err
	}
	if !exists(tf) {
		return []Entry{}, nil
	}
	v, ok, err := readJSONSig(tf)
	if err != nil {
		return nil, err
	}
	if ok {
		return asEntries(v)
	}
	bak := tf + ".bak"
	if exists(bak) {
		if v, ok, err := readJSONSig(bak); err == nil && ok {
			Log(ctx, "WARN: tracked.json corrupted \u2014 restored from .bak")
			return asEntries(v)
		}
	}
	Log(ctx, "WARN: tracked.json corrupted, no backup \u2014 starting fresh")
	return []Entry{}, nil
}

// Atomic save through tracked.json.tmp, backing up the old file before the rename
func SaveTracked(ctx context.Context, tracked []Entry) error {
	tf := stateFile(ctx, "tracked.json")
	if err := os.MkdirAll(filepath.Dir(tf), 0o777); err != nil {
		return err
	}
	list := make([]any, len(tracked))
	for i, e := range tracked {
		list[i] = e
	}
	text, err := pyjson.Dumps(list, pyjson.Indent(2))
	if err != nil {
		return err
	}
	tmp := tf + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o666); err != nil {
		return err
	}
	if exists(tf) {
		if err := pyshutil.Copy2(tf, tf+".bak"); err != nil {
			return err
		}
	}
	return os.Rename(tmp, tf)
}

// Whether a tracked directory is the home itself or under a protected top level tree
func isProtectedDir(ctx context.Context, p string) bool {
	if !isDir(p) {
		return false
	}
	resolved, err := resolvePath(p)
	if err != nil {
		return false
	}
	rel, ok := pypath.PathRelativeTo(resolved, home.Dir(ctx))
	if !ok {
		return false
	}
	parts := relParts(rel)
	return len(parts) == 0 || emptyDirProtectedTopLevel[parts[0]]
}

// Exact cron control plane paths of a home that quick never deletes
func protectedCronPaths(h string) map[string]bool {
	out := map[string]bool{}
	for _, parent := range []string{"cron", "cronjobs"} {
		base := pypath.PathJoin(h, parent)
		for _, x := range []string{base, pypath.PathJoin(base, "output"), pypath.PathJoin(base, "jobs.json"), pypath.PathJoin(base, ".tick.lock")} {
			out[x] = true
		}
	}
	return out
}

// Whether p resolves to a cron control plane path of the active home
func isProtectedCronPath(ctx context.Context, p string) (bool, error) {
	resolved, err := resolvePath(p)
	if err != nil {
		return false, err
	}
	return protectedCronPaths(pypath.PathStr(home.Dir(ctx)))[resolved], nil
}

// Human size with one decimal in B, KB, MB, GB, TB or PB
func FmtSize(n float64) string {
	for _, unit := range []string{"B", "KB", "MB", "GB", "TB"} {
		if n < 1024 {
			return fmt.Sprintf("%.1f %s", n, unit)
		}
		n /= 1024
	}
	return fmt.Sprintf("%.1f PB", n)
}

// Key error raised by a missing entry field
type KeyError struct{ Key string }

func (e *KeyError) Error() string { return "KeyError: " + pyrepr.Repr(e.Key) }

// String field of an entry
func strField(e Entry, key string) (string, error) {
	v, ok := e.Get(key)
	if !ok {
		return "", &KeyError{Key: key}
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("TypeError: entry %s is %s, not str", key, pyrepr.TypeName(v))
	}
	return s, nil
}

// Numeric size field of an entry
func sizeField(e Entry) (float64, error) {
	v, ok := e.Get("size")
	if !ok {
		return 0, &KeyError{Key: "size"}
	}
	switch n := v.(type) {
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case *big.Int:
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, nil
	}
	return 0, fmt.Errorf("TypeError: entry size is %s, not a number", pyrepr.TypeName(v))
}

// Sum of the size fields of entries
func sumSizes(items []Entry) (float64, error) {
	total := 0.0
	for _, i := range items {
		s, err := sizeField(i)
		if err != nil {
			return 0, err
		}
		total += s
	}
	return total, nil
}

// Registers a path for tracking and reports whether it was newly tracked
func Track(ctx context.Context, pathStr, category string, silent bool) (bool, error) {
	if !AllowedCategories[category] {
		Log(ctx, fmt.Sprintf("WARN: unknown category '%s', using 'other'", category))
		category = "other"
	}
	path, err := resolvePath(pathStr)
	if err != nil {
		Log(ctx, fmt.Sprintf("REJECT: %s (could not resolve path)", pathStr))
		return false, nil
	}
	if !exists(path) {
		Log(ctx, fmt.Sprintf("SKIP: %s (does not exist)", path))
		return false, nil
	}
	if !IsSafePath(ctx, path) {
		Log(ctx, fmt.Sprintf("REJECT: %s (outside ICEBATH_HOME)", path))
		return false, nil
	}
	var size int64
	if isFile(path) {
		fi, err := os.Stat(path)
		if err != nil {
			return false, err
		}
		size = fi.Size()
	}
	tracked, err := LoadTracked(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range tracked {
		p, err := strField(item, "path")
		if err != nil {
			return false, err
		}
		if p == path {
			return false, nil
		}
	}
	tracked = append(tracked, pyjson.ObjectOf("path", path, "timestamp", pydatetime.ISOFormatUTC(nowUTC()), "category", category, "size", size))
	if err := SaveTracked(ctx, tracked); err != nil {
		return false, err
	}
	Log(ctx, fmt.Sprintf("TRACKED: %s (%s, %s)", path, category, FmtSize(float64(size))))
	if !silent {
		fmt.Fprintf(stdout, "Tracked: %s (%s, %s)\n", path, category, FmtSize(float64(size)))
	}
	return true, nil
}

// Removes a path from tracking without deleting it and returns the number of entries dropped
func Forget(ctx context.Context, pathStr string) (int, error) {
	p, err := resolvePath(pathStr)
	if err != nil {
		return 0, err
	}
	tracked, err := LoadTracked(ctx)
	if err != nil {
		return 0, err
	}
	kept := []Entry{}
	for _, item := range tracked {
		ip, err := strField(item, "path")
		if err != nil {
			return 0, err
		}
		r, err := resolvePath(ip)
		if err != nil {
			return 0, err
		}
		if r != p {
			kept = append(kept, item)
		}
	}
	removed := len(tracked) - len(kept)
	if removed > 0 {
		if err := SaveTracked(ctx, kept); err != nil {
			return 0, err
		}
		Log(ctx, fmt.Sprintf("FORGOT: %s (%d entries)", p, removed))
	}
	return removed, nil
}

// One tracked entry whose path still exists with its age in whole days
type liveItem struct {
	item Entry
	path string
	age  int
}

// Whole days between an aware ISO timestamp and now, floored like timedelta.days
func ageDays(now time.Time, stamp string) (int, error) {
	ts, aware, err := pydatetime.ParseISOFormat(stamp)
	if err != nil {
		return 0, err
	}
	if !aware {
		return 0, errors.New("TypeError: can't subtract offset-naive and offset-aware datetimes")
	}
	return int(math.Floor(now.Sub(ts).Hours() / 24)), nil
}

// Entries whose path exists with their age, logging vanished ones when asked
func liveItems(ctx context.Context, tracked []Entry, now time.Time, logStale bool) ([]liveItem, error) {
	var out []liveItem
	for _, item := range tracked {
		raw, err := strField(item, "path")
		if err != nil {
			return nil, err
		}
		p := pypath.PathStr(raw)
		if exists(p) {
			stamp, err := strField(item, "timestamp")
			if err != nil {
				return nil, err
			}
			age, err := ageDays(now, stamp)
			if err != nil {
				return nil, err
			}
			out = append(out, liveItem{item: item, path: p, age: age})
		} else if logStale {
			Log(ctx, fmt.Sprintf("STALE: %s (removed from tracking)", p))
		}
	}
	return out, nil
}

// Whether a category at an age deletes without a prompt
func isAutoDelete(cat string, age int) bool {
	return cat == "test" || (cat == "temp" && age > 7) || (cat == "cron-output" && age > 14)
}

// Prompt only bucket research, chrome or large, empty when none applies
func promptGroup(item Entry, age int) (string, error) {
	cat, err := strField(item, "category")
	if err != nil {
		return "", err
	}
	if cat == "research" && age > 30 {
		return "research", nil
	}
	if cat == "chrome-profile" && age > 14 {
		return "chrome", nil
	}
	size, err := sizeField(item)
	if err != nil {
		return "", err
	}
	if size > largeFileBytes {
		return "large", nil
	}
	return "", nil
}

// Deletes a tracked file or tree and audit logs it, returning the error text of a failed removal
func deleteItem(ctx context.Context, item Entry) (string, error) {
	raw, err := strField(item, "path")
	if err != nil {
		return "", err
	}
	p := pypath.PathStr(raw)
	var rmErr error
	if isFile(p) {
		rmErr = os.Remove(p)
	} else if isDir(p) {
		rmErr = pyshutil.Rmtree(p)
	}
	if rmErr != nil {
		text := pypath.OSErrorText(rmErr)
		Log(ctx, fmt.Sprintf("ERROR deleting %s: %s", p, text))
		return fmt.Sprintf("%s: %s", p, text), nil
	}
	cat, err := strField(item, "category")
	if err != nil {
		return "", err
	}
	size, err := sizeField(item)
	if err != nil {
		return "", err
	}
	Log(ctx, fmt.Sprintf("DELETED: %s (%s, %s)", p, cat, FmtSize(size)))
	return "", nil
}

// Auto delete and needs prompt lists without touching any file
func DryRun(ctx context.Context) ([]Entry, []Entry, error) {
	auto, prompt := []Entry{}, []Entry{}
	tracked, err := LoadTracked(ctx)
	if err != nil {
		return nil, nil, err
	}
	live, err := liveItems(ctx, tracked, nowUTC(), false)
	if err != nil {
		return nil, nil, err
	}
	for _, li := range live {
		cat, err := strField(li.item, "category")
		if err != nil {
			return nil, nil, err
		}
		if cat == "cron-output" {
			guessed, err := GuessCategory(ctx, li.path)
			if err != nil {
				return nil, nil, err
			}
			if guessed != "cron-output" {
				continue
			}
		}
		if isProtectedDir(ctx, li.path) {
			continue
		}
		if isAutoDelete(cat, li.age) {
			auto = append(auto, li.item)
			continue
		}
		group, err := promptGroup(li.item, li.age)
		if err != nil {
			return nil, nil, err
		}
		if group != "" {
			prompt = append(prompt, li.item)
		}
	}
	return auto, prompt, nil
}

// Safe deterministic cleanup with no prompts, sweeping empty dirs afterwards
func Quick(ctx context.Context) (Summary, error) {
	s := Summary{Errors: []string{}}
	newTracked := []Entry{}
	tracked, err := LoadTracked(ctx)
	if err != nil {
		return s, err
	}
	live, err := liveItems(ctx, tracked, nowUTC(), true)
	if err != nil {
		return s, err
	}
	for _, li := range live {
		cat, err := strField(li.item, "category")
		if err != nil {
			return s, err
		}
		if note, stale := staleSkipNote[cat]; stale {
			reCat, err := GuessCategory(ctx, li.path)
			if err != nil {
				return s, err
			}
			if reCat != cat {
				var shown any
				if reCat != "" {
					shown = reCat
				}
				Log(ctx, fmt.Sprintf("SKIP stale %s entry: %s (re-classified as %s%s)", cat, li.path, pyrepr.Repr(shown), note))
				continue
			}
		}
		protected, err := isProtectedCronPath(ctx, li.path)
		if err != nil {
			return s, err
		}
		if protected {
			Log(ctx, fmt.Sprintf("SKIP protected cron path: %s", li.path))
			continue
		}
		if isProtectedDir(ctx, li.path) {
			Log(ctx, fmt.Sprintf("SKIPPED: %s (protected top-level dir)", li.path))
			continue
		}
		if !isAutoDelete(cat, li.age) {
			newTracked = append(newTracked, li.item)
			continue
		}
		failure, err := deleteItem(ctx, li.item)
		if err != nil {
			return s, err
		}
		if failure == "" {
			size, err := sizeField(li.item)
			if err != nil {
				return s, err
			}
			s.Freed += size
			s.Deleted++
		} else {
			s.Errors = append(s.Errors, failure)
			newTracked = append(newTracked, li.item)
		}
	}
	s.EmptyDirs = sweepEmptyDirs(ctx, home.Dir(ctx))
	if err := SaveTracked(ctx, newTracked); err != nil {
		return s, err
	}
	Log(ctx, fmt.Sprintf("QUICK_SUMMARY: %d files, %d dirs, %s", s.Deleted, s.EmptyDirs, FmtSize(s.Freed)))
	return s, nil
}

// Real subdirectories of a dir in listing order minus symlinks and excluded names
func subdirs(dirpath string, exclude func(string) bool) []string {
	f, err := os.Open(dirpath)
	if err != nil {
		return nil
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil
	}
	var out []string
	for _, name := range names {
		c := filepath.Join(dirpath, name)
		if fi, err := os.Lstat(c); err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		if isDir(c) && !exclude(name) {
			out = append(out, c)
		}
	}
	return out
}

// Whether a directory has at least one entry, false with an error when it cannot be listed
func hasEntries(dirpath string) (bool, error) {
	f, err := os.Open(dirpath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err == io.EOF {
		return false, nil
	}
	return len(names) > 0, err
}

// Removes empty dirs under the home in iterative post order without entering durable or heavy trees
func sweepEmptyDirs(ctx context.Context, h string) int {
	removed := 0
	type frame struct {
		dir     string
		visited bool
	}
	var stack []frame
	for _, top := range subdirs(h, func(n string) bool { return emptyDirProtectedTopLevel[n] || emptyDirSweepPruneDirs[n] }) {
		stack = append(stack, frame{top, false})
	}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.visited {
			if full, err := hasEntries(f.dir); err == nil && !full {
				if err := os.Remove(f.dir); err == nil {
					removed++
					Log(ctx, fmt.Sprintf("DELETED: %s (empty dir)", f.dir))
				}
			}
			continue
		}
		stack = append(stack, frame{f.dir, true})
		for _, child := range subdirs(f.dir, func(n string) bool { return emptyDirSweepPruneDirs[n] }) {
			stack = append(stack, frame{child, false})
		}
	}
	return removed
}

// Per category breakdown and the ten largest existing tracked files
func GetStatus(ctx context.Context) (Status, error) {
	tracked, err := LoadTracked(ctx)
	if err != nil {
		return Status{}, err
	}
	var cats []CategoryTotal
	index := map[string]int{}
	var existing []TopItem
	for _, item := range tracked {
		cat, err := strField(item, "category")
		if err != nil {
			return Status{}, err
		}
		size, err := sizeField(item)
		if err != nil {
			return Status{}, err
		}
		i, ok := index[cat]
		if !ok {
			i = len(cats)
			index[cat] = i
			cats = append(cats, CategoryTotal{Name: cat})
		}
		cats[i].Count++
		cats[i].Size += size
	}
	for _, item := range tracked {
		p, err := strField(item, "path")
		if err != nil {
			return Status{}, err
		}
		if exists(pypath.PathStr(p)) {
			size, _ := sizeField(item)
			cat, _ := strField(item, "category")
			existing = append(existing, TopItem{Path: p, Size: size, Category: cat})
		}
	}
	sort.SliceStable(existing, func(a, b int) bool { return existing[a].Size > existing[b].Size })
	if len(existing) > 10 {
		existing = existing[:10]
	}
	return Status{Categories: cats, Top10: existing, TotalTracked: len(tracked)}, nil
}

// Human readable status table for the slash command
func FormatStatus(s Status) string {
	lines := []string{fmt.Sprintf("%-20s %6s  %10s", "Category", "Files", "Size"), strings.Repeat("-", 40)}
	cats := slices.Clone(s.Categories)
	sort.SliceStable(cats, func(a, b int) bool { return cats[a].Size > cats[b].Size })
	for _, d := range cats {
		lines = append(lines, fmt.Sprintf("%-20s %6d  %10s", d.Name, d.Count, FmtSize(d.Size)))
	}
	if len(cats) == 0 {
		lines = append(lines, "(nothing tracked yet)")
	}
	lines = append(lines, "", "Top 10 largest tracked files:")
	if len(s.Top10) == 0 {
		lines = append(lines, "  (none)")
	}
	for rank, t := range s.Top10 {
		lines = append(lines, fmt.Sprintf("  %2d. %8s  [%s]  %s", rank+1, FmtSize(t.Size), t.Category, t.Path))
	}
	return strings.Join(lines, "\n")
}

// Git environment for read only probes, no prompts, no inherited targeting or pathspec modes
func sourceGitEnv() []string {
	base := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			base[k] = v
		}
	}
	env := tools.NoninteractiveGitEnv(base)
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_SHALLOW_FILE", "GIT_NAMESPACE",
		"GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS"} {
		delete(env, key)
	}
	env["GIT_OPTIONAL_LOCKS"] = "0"
	env["GIT_NO_LAZY_FETCH"] = "1"
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// Whether the repo enclosing path tracks it exactly, any git failure meaning untracked
func gitTracks(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", pypath.Dirname(path), "ls-files", "--error-unmatch", "--", ":(literal)"+pypath.Basename(path))
	cmd.Env = sourceGitEnv()
	return cmd.Run() == nil
}

// Whether a .git entry sits on the chain strictly below the home, or a repo at or above it tracks the file
func insideGitWorktree(ctx context.Context, path string) (bool, error) {
	resolved, err := resolvePath(path)
	if err != nil {
		return false, err
	}
	var parents []string
	for p := pypath.Dirname(resolved); ; p = pypath.Dirname(p) {
		parents = append(parents, p)
		if pypath.Dirname(p) == p {
			break
		}
	}
	var above []string
	h := pypath.PathStr(home.Dir(ctx))
	if i := slices.Index(parents, h); i >= 0 {
		parents, above = parents[:i], parents[i:]
	}
	for _, parent := range parents {
		if exists(pypath.PathJoin(parent, ".git")) {
			return true, nil
		}
	}
	for _, parent := range above {
		if exists(pypath.PathJoin(parent, ".git")) {
			return gitTracks(resolved), nil
		}
	}
	return false, nil
}

// Category label for a path, empty when it must not be tracked
func GuessCategory(ctx context.Context, path string) (string, error) {
	if !IsSafePath(ctx, path) {
		return "", nil
	}
	resolved, err := resolvePath(path)
	if err != nil {
		return "", err
	}
	if rel, ok := pypath.PathRelativeTo(resolved, home.Dir(ctx)); ok {
		parts := relParts(rel)
		top := ""
		if len(parts) > 0 {
			top = parts[0]
		}
		if neverTrackTopLevel[top] || isProtectedDir(ctx, path) {
			return "", nil
		}
		if top == "cron" || top == "cronjobs" {
			if len(parts) >= 3 && parts[1] == "output" {
				return "cron-output", nil
			}
			return "", nil
		}
		if top == "cache" {
			return "temp", nil
		}
	}
	name := pypath.Basename(pypath.PathStr(path))
	if slices.ContainsFunc(testPatterns, func(x string) bool { return strings.HasPrefix(name, x) }) ||
		slices.ContainsFunc(testSuffixes, func(x string) bool { return strings.HasSuffix(name, x) }) {
		inside, err := insideGitWorktree(ctx, path)
		if err != nil {
			return "", err
		}
		if inside {
			return "", nil
		}
		return "test", nil
	}
	return "", nil
}
