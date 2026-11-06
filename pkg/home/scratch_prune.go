package home

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	reapGrace       = 3 * time.Second
	gitFileMaxDepth = 4
	stateTouched    = "touched"
	stateScanFailed = "scan-failed"
	stateIdle       = "idle"
)

// Prune log rotation size and backup count, the size lowered by the rotation race test
var (
	pruneLogMaxBytes int64 = 5 * 1024 * 1024
	pruneLogBackups        = 3
)

// Whether this host exposes procfs, which makes boot ticks the launcher lineage stamp
var hasProc = func() bool {
	fi, err := os.Stat("/proc/self")
	return err == nil && fi.IsDir()
}()

// Clock, filesystem and step seams the prune tests replace like the monkeypatched time, os and module functions
var (
	scratchNow     = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	scratchLstat   = os.Lstat
	scratchReadDir = readDirUnsorted
	scratchRename  = os.Rename
	treeBytesStep  = treeBytes
	reapStep       = reapProcessesRootedIn
	rmtreeStep     = rmtreeCollectingPermissionErrors
)

// Launcher ancestry a detached pass spares, unknown when the launcher could not compute it
var (
	launcherLineage      []lineageEntry
	launcherLineageKnown = true
)

// One ancestor of the launching command with its start stamp
type lineageEntry struct {
	pid     int
	started float64
}

// Directory entries in kernel order like os.scandir
func readDirUnsorted(dir string) ([]fs.DirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// Python repr of a filesystem path or name, undecodable bytes shown as surrogate escapes
func pathRepr(p string) string { return pyrepr.StrRepr(pystr.SurrogateEscapeDecode([]byte(p))) }

// Modification time of fi as float epoch seconds
func mtimeOf(fi fs.FileInfo) float64 { return float64(fi.ModTime().UnixNano()) / 1e9 }

// Audit sink for one prune, rotating logs/scratch-prune.log and echoing every record to slog
type pruneAudit struct {
	path string
}

// Audit writer on logFile with its directory created, slog only when logFile is empty or uncreatable
func openPruneLog(logFile string) *pruneAudit {
	if logFile == "" {
		return &pruneAudit{}
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o777); err != nil {
		slog.Warn(fmt.Sprintf("scratch prune: cannot create %s: %s", filepath.Dir(logFile), pypath.OSErrorText(err)))
		return &pruneAudit{}
	}
	return &pruneAudit{path: logFile}
}

// Appends one timestamped record, rotating first when it would reach the size cap
func (a *pruneAudit) info(msg string) {
	slog.Info(msg)
	if a.path == "" {
		return
	}
	line := time.Now().Format("2006-01-02 15:04:05,000") + " " + msg + "\n"
	if fi, err := os.Stat(a.path); err == nil && fi.Mode().IsRegular() && fi.Size()+int64(pystr.Len(line)) >= pruneLogMaxBytes {
		a.rollover()
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// Shifts the numbered backups and moves the live log to .1, a rotation another prune finished first tolerated
func (a *pruneAudit) rollover() {
	for i := pruneLogBackups - 1; i > 0; i-- {
		src, dst := fmt.Sprintf("%s.%d", a.path, i), fmt.Sprintf("%s.%d", a.path, i+1)
		if _, err := os.Stat(src); err == nil {
			if _, err := os.Stat(dst); err == nil {
				if err := os.Remove(dst); errors.Is(err, fs.ErrNotExist) {
					return
				}
			}
			if err := scratchRename(src, dst); errors.Is(err, fs.ErrNotExist) {
				return
			}
		}
	}
	dst := a.path + ".1"
	if _, err := os.Stat(dst); err == nil {
		if err := os.Remove(dst); errors.Is(err, fs.ErrNotExist) {
			return
		}
	}
	if _, err := os.Stat(a.path); err == nil {
		_ = scratchRename(a.path, dst)
	}
}

// Apparent size of path and everything under it without following symlinks
func treeBytes(path string) int64 {
	fi, err := scratchLstat(path)
	if err != nil {
		return 0
	}
	total := fi.Size()
	var stack []string
	if st, err := os.Stat(path); err == nil && st.IsDir() && fi.Mode()&fs.ModeSymlink == 0 {
		stack = append(stack, path)
	}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		children, err := scratchReadDir(current)
		if err != nil {
			continue
		}
		for _, child := range children {
			info, err := child.Info()
			if err != nil {
				continue
			}
			total += info.Size()
			if child.Type().IsDir() {
				stack = append(stack, filepath.Join(current, child.Name()))
			}
		}
	}
	return total
}

// Touched, scan-failed or idle for the subtree at path against cutoff, hardlinked files never counting as writes
func SubtreeTouchState(path string, cutoff float64) string {
	state, _ := scanSubtree(path, cutoff)
	return state
}

// Subtree state plus the directory that refused listing with a permission error when that failed the scan
func scanSubtree(path string, cutoff float64) (string, string) {
	fi, err := scratchLstat(path)
	if err != nil {
		return stateScanFailed, ""
	}
	if mtimeOf(fi) >= cutoff {
		return stateTouched, ""
	}
	st, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, syscall.ELOOP) {
		return stateScanFailed, ""
	}
	if err != nil || !st.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return stateIdle, ""
	}
	stack := []string{path}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		children, err := scratchReadDir(current)
		if err != nil {
			return stateScanFailed, deniedDir(current, err)
		}
		for _, child := range children {
			info, err := child.Info()
			if err != nil {
				return stateScanFailed, deniedDir(current, err)
			}
			if mtimeOf(info) >= cutoff && !(linkCount(info) > 1 && child.Type().IsRegular()) {
				return stateTouched, ""
			}
			if child.Type().IsDir() {
				stack = append(stack, filepath.Join(current, child.Name()))
			}
		}
	}
	return stateIdle, ""
}

// The directory when err is a permission error, empty otherwise
func deniedDir(dir string, err error) string {
	if isPermissionError(err) {
		return dir
	}
	return ""
}

// True when path or anything beneath it was written at or after cutoff, or could not be inspected
func SubtreeTouchedSince(path string, cutoff float64) bool {
	return SubtreeTouchState(path, cutoff) != stateIdle
}

// Whether path is root or lies beneath it
func under(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimRight(root, string(os.PathSeparator))+string(os.PathSeparator))
}

// Start stamp of pid a clock step cannot move, raw boot ticks where procfs exists and the epoch birth elsewhere
func birthStamp(pid int) (float64, error) {
	if !hasProc {
		return proc.Birth(pid)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, &pyint.Error{Kind: "IndexError", Msg: "list index out of range"}
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return 0, &pyint.Error{Kind: "IndexError", Msg: "list index out of range"}
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, &pyint.Error{Kind: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.BytesRepr([]byte(fields[19]))}
	}
	return float64(ticks), nil
}

// Whether pid still carries the recorded stamp, exact ticks on procfs and a two second window on epochs
func sameBirth(pid int, started float64) (bool, error) {
	now, err := birthStamp(pid)
	if err != nil {
		return false, err
	}
	if hasProc {
		return now == started, nil
	}
	return math.Abs(now-started) < 2.0, nil
}

// This process and its ancestors with start stamps, false when any link cannot be read
func LineageWithStartTimes() ([]lineageEntry, bool) {
	self := os.Getpid()
	parents, err := proc.Parents(self)
	if err != nil {
		return nil, false
	}
	var chain []lineageEntry
	seen := map[int]bool{}
	for _, pid := range append([]int{self}, parents...) {
		if seen[pid] {
			break
		}
		seen[pid] = true
		stamp, err := birthStamp(pid)
		if err != nil {
			return nil, false
		}
		chain = append(chain, lineageEntry{pid, stamp})
	}
	return chain, true
}

// Pids never signalled, this process with its ancestors plus surviving launcher ancestors, nil to spare everything
func ownLineage() map[int]bool {
	if !launcherLineageKnown {
		return nil
	}
	self := os.Getpid()
	pids := map[int]bool{self: true}
	if parents, err := proc.Parents(self); err == nil {
		for _, p := range parents {
			pids[p] = true
		}
	}
	for _, l := range launcherLineage {
		if !proc.Exists(l.pid) {
			continue
		}
		if same, err := sameBirth(l.pid, l.started); err != nil || same {
			pids[l.pid] = true
		}
	}
	return pids
}

// Linux boot id of the running kernel, empty elsewhere or when unreadable
func liveBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Boot relative birth under key, minus one for an entry from another boot, nil when either boot id is unknown
func entryBoot(entry *pyjson.Object, key string) (any, bool) {
	recorded, ok := entry.Vals["boot_id"].(string)
	live := liveBootID()
	if !ok || live == "" {
		return nil, false
	}
	if recorded == live {
		v := entry.Vals[key]
		return v, v != nil
	}
	return -1.0, true
}

// Liveness of pid against a recorded birth, known false when the process cannot be read
func pidAliveMatches(pid int, createTime any, boot any, hasBoot bool) (alive bool, known bool) {
	if !proc.Exists(pid) {
		return false, true
	}
	same := true
	if live, ok := proc.BootStart(pid); hasBoot && ok {
		want, err := pyint.Float(boot)
		if err != nil {
			return false, false
		}
		same = live == want
	} else if createTime != nil {
		want, err := pyint.Float(createTime)
		if err != nil {
			return false, false
		}
		birth, err := proc.Birth(pid)
		if errors.Is(err, proc.ErrNoSuchProcess) {
			return false, true
		}
		if err != nil {
			return false, false
		}
		same = math.Abs(birth-want) < 2.0
	}
	return same && (runtime.GOOS == "windows" || !proc.Zombie(pid)), true
}

// Python isinstance int of a decoded JSON value as an int
func jsonInt(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Whether the ledger entry's process is still the recorded incarnation or cannot be disproved
func entryStillRunning(entry *pyjson.Object) bool {
	pid, ok := jsonInt(entry.Vals["pid"])
	if !ok || pid <= 0 {
		return false
	}
	boot, hasBoot := entryBoot(entry, "boot_start")
	alive, known := pidAliveMatches(pid, entry.Vals["create_time"], boot, hasBoot)
	return alive || !known
}

// Whether the entry's recorded spawner is provably gone, known false when that cannot be proved
func spawnerIsDead(entry *pyjson.Object) (dead bool, known bool) {
	pid, ok := jsonInt(entry.Vals["spawner_pid"])
	if !ok || pid <= 0 {
		return false, false
	}
	boot, hasBoot := entryBoot(entry, "spawner_boot")
	alive, known := pidAliveMatches(pid, entry.Vals["spawner_create"], boot, hasBoot)
	return !alive, known
}

// Start fingerprint the process checkpoint records, procfs ticks or the birth in centiseconds
func processStartTime(pid int) (int64, bool) {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if end := strings.LastIndexByte(string(data), ')'); end >= 0 {
			if fields := strings.Fields(string(data[end+1:])); len(fields) > 19 {
				if n, err := strconv.ParseInt(fields[19], 10, 64); err == nil {
					return n, true
				}
			}
		}
	}
	birth, err := proc.Birth(pid)
	if err != nil {
		return 0, false
	}
	return int64(math.RoundToEven(birth * 100)), true
}

// Decoded JSON array of a utf-8-sig file, false when unreadable or not JSON
func readJSONList(path string) ([]any, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	text, ok := utf8SigText(data)
	if !ok {
		return nil, false
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, false
	}
	list, _ := v.([]any)
	return list, true
}

// Text of utf-8-sig bytes, false when they are not valid UTF-8
func utf8SigText(data []byte) (string, bool) {
	data = []byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))
	if _, ok := pystr.ValidIndex(data); !ok {
		return "", false
	}
	return string(data), true
}

// Live pids icebath registered for home in the spawn ledger and the process checkpoints, reused pids excluded
func registeredLivePIDs(h string) map[int]bool {
	root := RootOf(h)
	type source struct{ path, stampKey string }
	sources := []source{{filepath.Join(root, "spawn-ledger.json"), "create_time"}}
	sources = append(sources, source{filepath.Join(h, "processes.json"), "host_start_time"})
	if filepath.Clean(root) != filepath.Clean(h) {
		sources = append(sources, source{filepath.Join(root, "processes.json"), "host_start_time"})
	}
	pids := map[int]bool{}
	for _, src := range sources {
		entries, _ := readJSONList(src.path)
		for _, raw := range entries {
			entry, ok := raw.(*pyjson.Object)
			if !ok {
				continue
			}
			pid, ok := jsonInt(entry.Vals["pid"])
			if !ok || pid <= 0 {
				continue
			}
			if scope, has := entry.Vals["pid_scope"]; has && scope != "host" {
				continue
			}
			if entry.Vals["purpose"] == "mcp-helper" {
				if dead, known := spawnerIsDead(entry); known && dead {
					continue
				}
			}
			var alive bool
			stamp := entry.Vals[src.stampKey]
			switch {
			case src.stampKey == "create_time":
				alive = entryStillRunning(entry)
			case stamp == nil:
				alive = proc.Exists(pid)
			default:
				start, ok := processStartTime(pid)
				alive = ok && numberEquals(stamp, start)
			}
			if alive {
				pids[pid] = true
			}
		}
	}
	return pids
}

// Python == between a decoded JSON number and an int
func numberEquals(v any, n int64) bool {
	switch t := v.(type) {
	case int64:
		return t == n
	case float64:
		return t == float64(n)
	case bool:
		return (t && n == 1) || (!t && n == 0)
	}
	if big, ok := pyrepr.Int(v); ok {
		return big.IsInt64() && big.Int64() == n
	}
	return false
}

// Registered itself or a descendant of a live registered process, pid 1 never counting as an owner
func ownedByIcebath(pid int, registered map[int]bool) bool {
	if registered[pid] {
		return true
	}
	parents, err := proc.Parents(pid)
	if err != nil {
		return false
	}
	for _, p := range parents {
		if registered[p] && p != 1 {
			return true
		}
	}
	return false
}

// os.path.realpath without strictness
func realpath(p string) string {
	if r, err := pypath.Realpath(p, pypath.Lax); err == nil {
		return r
	}
	return p
}

// One same user process whose cwd lies in scratch
type reapCandidate struct {
	pid   int
	cwd   string
	birth float64
	born  bool
}

// Still the process that was listed, a recorded birth that moved meaning a reused pid
func (c reapCandidate) current() bool {
	if !c.born {
		return proc.Exists(c.pid)
	}
	b, err := proc.Birth(c.pid)
	return err == nil && b == c.birth
}

// TERMs then KILLs same user processes living in doomed entries or vanished scratch dirs, sparing icebath owned ones
func reapProcessesRootedIn(scratchRoot string, doomed *[]string, audit *pruneAudit) (int, error) {
	if !proc.CwdAvailable {
		return 0, &pyint.Error{Kind: "NotImplementedError", Msg: "platform " + runtime.GOOS + " is not supported"}
	}
	root := realpath(scratchRoot)
	var targets []string
	for _, p := range *doomed {
		targets = append(targets, realpath(p))
	}
	skip := ownLineage()
	checkUID := runtime.GOOS != "windows"
	uid := os.Getuid()
	pids, err := proc.PIDs()
	if err != nil {
		return 0, err
	}
	var victims, lineage []reapCandidate
	for _, pid := range pids {
		if checkUID {
			if ruid, err := proc.RealUID(pid); err != nil || ruid != uid {
				continue
			}
		}
		cwd, err := proc.Cwd(pid)
		if err != nil || cwd == "" {
			continue
		}
		deleted := strings.HasSuffix(cwd, " (deleted)")
		cwdPath := strings.TrimSuffix(cwd, " (deleted)")
		if !under(cwdPath, root) {
			continue
		}
		_, statErr := os.Stat(cwdPath)
		if deleted || statErr != nil || slices.ContainsFunc(targets, func(t string) bool { return under(cwdPath, t) }) {
			c := reapCandidate{pid: pid, cwd: cwdPath}
			c.birth, err = proc.Birth(pid)
			c.born = err == nil
			if skip == nil || skip[pid] {
				lineage = append(lineage, c)
			} else {
				victims = append(victims, c)
			}
		}
	}
	var registered map[int]bool
	if len(victims) > 0 {
		abs, _ := filepath.Abs(scratchRoot)
		registered = registeredLivePIDs(filepath.Dir(filepath.Dir(abs)))
	}
	owned := slices.Clone(lineage)
	for _, c := range victims {
		if ownedByIcebath(c.pid, registered) {
			owned = append(owned, c)
		}
	}
	for _, c := range owned {
		for _, entry := range slices.Clone(*doomed) {
			if !under(c.cwd, realpath(entry)) {
				continue
			}
			*doomed = slices.DeleteFunc(*doomed, func(e string) bool { return e == entry })
			name, err := proc.Name(c.pid)
			if err != nil {
				name = "?"
			}
			audit.info(fmt.Sprintf("scratch prune: kept %s — live Icebath process pid=%d (%s) works inside", pathRepr(entry), c.pid, pathRepr(name)))
		}
	}
	var remaining []reapCandidate
	for _, c := range victims {
		if slices.Contains(owned, c) {
			continue
		}
		_, statErr := os.Stat(c.cwd)
		if statErr != nil || slices.ContainsFunc(*doomed, func(e string) bool { return under(c.cwd, realpath(e)) }) {
			remaining = append(remaining, c)
		}
	}
	if len(remaining) == 0 {
		return 0, nil
	}
	for _, c := range remaining {
		name, err := proc.Name(c.pid)
		if err != nil || !c.current() || proc.Terminate(c.pid) != nil {
			continue
		}
		audit.info(fmt.Sprintf("scratch prune: sent TERM pid=%d name=%s cwd=%s", c.pid, pathRepr(name), pathRepr(c.cwd)))
	}
	deadline := time.Now().Add(reapGrace)
	alive := remaining
	for {
		alive = slices.DeleteFunc(alive, func(c reapCandidate) bool { return !c.current() })
		if len(alive) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, c := range alive {
		if proc.Zombie(c.pid) || proc.KillPID(c.pid) != nil {
			continue
		}
		audit.info(fmt.Sprintf("scratch prune: sent KILL pid=%d (still running after TERM)", c.pid))
	}
	slog.Info(fmt.Sprintf("scratch prune: reaped %d process(es) rooted in pruned entries", len(remaining)))
	return len(remaining), nil
}

// Repos whose linked worktrees live inside entry, found through gitdir lines in .git files
func linkedWorktreeRepos(entry string) map[string]bool {
	repos := map[string]bool{}
	type frame struct {
		dir   string
		depth int
	}
	stack := []frame{{entry, 0}}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		children, err := scratchReadDir(cur.dir)
		if err != nil {
			continue
		}
		for _, child := range children {
			path := filepath.Join(cur.dir, child.Name())
			if child.Name() == ".git" && child.Type().IsRegular() {
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				line := strings.TrimSpace(pystr.DecodeUTF8Replace([]byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))))
				if rest, ok := strings.CutPrefix(line, "gitdir:"); ok {
					gitdir := filepath.Clean(strings.TrimSpace(rest))
					parent := filepath.Dir(gitdir)
					if filepath.Base(parent) == "worktrees" && filepath.Base(filepath.Dir(parent)) == ".git" {
						repos[filepath.Dir(filepath.Dir(parent))] = true
					}
				}
			} else if child.Type().IsDir() && cur.depth < gitFileMaxDepth && child.Name() != "node_modules" && child.Name() != ".venv" && child.Name() != "venv" {
				stack = append(stack, frame{path, cur.depth + 1})
			}
		}
	}
	return repos
}

// Runs git worktree prune in each repo so registrations of deleted trees are dropped
func ReleaseGitWorktrees(repos map[string]bool) {
	names := make([]string, 0, len(repos))
	for r := range repos {
		names = append(names, r)
	}
	slices.Sort(names)
	for _, repo := range names {
		if st, err := os.Stat(repo); err != nil || !st.IsDir() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmd := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "prune")
		var exitErr *exec.ExitError
		if _, err := cmd.CombinedOutput(); err != nil && (ctx.Err() != nil || !errors.As(err, &exitErr)) {
			slog.Debug(fmt.Sprintf("git worktree prune in %s failed: %s", repo, err))
		}
		cancel()
	}
}

// Deletes entries of root idle for maxIdleHours after reaping processes and worktrees rooted in them, returns the count
func PruneIdleEntries(root string, maxIdleHours float64, skipNames map[string]bool, logFile string) (int, error) {
	return pruneIdleEntries(root, maxIdleHours, skipNames, openPruneLog(logFile))
}

// Python exception class and message for an error raised inside a prune pass
func excInfo(err error) (string, string) {
	var pe *pyint.Error
	if errors.As(err, &pe) {
		return pe.Kind, pe.Msg
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return pypath.OSErrorClass(err), pypath.OSErrorText(err)
	}
	return "OSError", err.Error()
}

func pruneIdleEntries(root string, maxIdleHours float64, skipNames map[string]bool, audit *pruneAudit) (int, error) {
	cutoff := scratchNow() - maxIdleHours*3600
	stuckSkips, err := LoadStuckSkips(root, true)
	if err != nil {
		return 0, err
	}
	skip := map[string]bool{StuckStateName: true, StuckLockName: true}
	for _, set := range []map[string]bool{skipNames, stuckSkips} {
		for n := range set {
			skip[n] = true
		}
	}
	children, err := scratchReadDir(root)
	if err != nil {
		return 0, nil
	}
	var doomed []string
	for _, child := range children {
		if skip[child.Name()] {
			continue
		}
		e := filepath.Join(root, child.Name())
		state, denied := scanSubtree(e, cutoff)
		if state == stateIdle {
			doomed = append(doomed, e)
		} else if denied != "" && stuckTrackingSupported() {
			recordStuck(root, e, []string{denied}, audit)
		}
	}
	if _, err := reapStep(root, &doomed, audit); err != nil {
		class, msg := excInfo(err)
		audit.info(fmt.Sprintf("scratch prune: process check failed (%s: %s), nothing deleted this pass", class, pyrepr.StrRepr(msg)))
		return 0, nil
	}
	if len(doomed) == 0 {
		return 0, nil
	}
	repos := map[string]bool{}
	removed := 0
	for _, entry := range doomed {
		realDir := isRealDir(entry)
		if realDir {
			for r := range linkedWorktreeRepos(entry) {
				repos[r] = true
			}
		}
		size := treeBytesStep(entry)
		if state := SubtreeTouchState(entry, cutoff); state != stateIdle {
			_, err := scratchLstat(entry)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				audit.info(fmt.Sprintf("scratch prune: entry %s vanished since selection", pathRepr(entry)))
			case state == stateTouched:
				audit.info(fmt.Sprintf("scratch prune: rescued %s — touched since selection, kept", pathRepr(entry)))
			default:
				audit.info(fmt.Sprintf("scratch prune: kept %s — activity re-scan failed, left untouched", pathRepr(entry)))
			}
			continue
		}
		var denied []string
		if isRealDir(entry) {
			denied = rmtreeStep(entry)
		} else if err := os.Remove(entry); err != nil {
			audit.info(fmt.Sprintf("scratch prune: could not remove %s: %s", pathRepr(entry), pypath.OSErrorText(err)))
			continue
		}
		if _, err := scratchLstat(entry); err == nil {
			if len(denied) > 0 && stuckTrackingSupported() {
				recordStuck(root, entry, denied, audit)
			} else {
				audit.info(fmt.Sprintf("scratch prune: could not fully remove %s", pathRepr(entry)))
			}
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			audit.info(fmt.Sprintf("scratch prune: could not confirm removal of %s", pathRepr(entry)))
			continue
		}
		audit.info(fmt.Sprintf("scratch prune: removed %s (%d bytes)", pathRepr(entry), size))
		removed++
	}
	ReleaseGitWorktrees(repos)
	return removed, nil
}

// Path.is_dir and not Path.is_symlink, the symlink probe going through the lstat seam
func isRealDir(p string) bool {
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return false
	}
	fi, err := scratchLstat(p)
	return err != nil || fi.Mode()&fs.ModeSymlink == 0
}
