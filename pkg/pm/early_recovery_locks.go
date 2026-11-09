package pm

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Answer of a lock holder probe, Unknown when no platform proof exists and Holder naming a live owner
type LockHold struct {
	Unknown bool
	Holder  string
}

// Reports a named live holder, the truthy hermes answer
func (h LockHold) Held() bool { return h.Holder != "" }

// Reports the proven free answer
func (h LockHold) Free() bool { return !h.Unknown && h.Holder == "" }

var (
	lockFree    = LockHold{}
	lockUnknown = LockHold{Unknown: true}
)

// Names a holding process like pid 4242 (git commit)
func lockHolder(pid, name string) LockHold { return LockHold{Holder: "pid " + pid + " (" + name + ")"} }

// Set of the given names
func gitNameSet(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// Git subcommands that only read and never keep index.lock behind a closed fd
var readerGit = gitNameSet(
	"annotate", "blame", "cat-file", "check-attr", "check-ignore", "check-mailmap", "check-ref-format",
	"cherry", "count-objects", "describe", "diff", "diff-files", "diff-index", "diff-tree",
	"for-each-ref", "fsmonitor--daemon", "grep", "help", "log", "ls-files", "ls-remote", "ls-tree",
	"merge-base", "name-rev", "range-diff", "rev-list", "rev-parse", "shortlog", "show", "show-branch",
	"show-ref", "status", "var", "verify-commit", "verify-tag", "version", "whatchanged",
	"credential", "credential-cache", "credential-store",
	"fetch", "fetch-pack", "http-fetch", "index-pack", "pack-objects", "remote-http", "remote-https",
	"upload-pack",
)

// Global git options whose value is the next argument
var gitValueOptions = gitNameSet("-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env", "--super-prefix", "--attr-source")

// Environment variables that point a git at a repository other than its cwd
var gitLocationEnv = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"}

// Daemons a git spawns that outlive it and never take the index lock
var neverLocks = gitNameSet("credential-cache--daemon", "fsmonitor--daemon")

// Executable lookup, subprocess runner and holder rule of the probes, swapped by tests
var (
	lockWhichFn      = func(name string) (string, bool) { return pyshutil.Which(name, nil) }
	lockRunFn        = pysubprocess.RunWith
	countsAsHolderFn = countsAsHolder
)

// Returns git or git-sub when arg0 runs git in any directory, separator, case or .exe form, else empty
func gitProgram(arg0 string) string {
	name := pystr.Strip(arg0)
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(pystr.Lower(name), ".exe")
	if name == "git" || strings.HasPrefix(name, "git-") {
		return name
	}
	return ""
}

// Exported gitProgram for the lock sweeps of other packages
func GitProgram(arg0 string) string { return gitProgram(arg0) }

// Splits a NUL separated /proc cmdline into its non empty arguments decoded with replacement
func gitArgvBytes(cmdline []byte) []string {
	var out []string
	for _, a := range bytes.Split(cmdline, []byte{0}) {
		if len(a) > 0 {
			out = append(out, pystr.DecodeUTF8Replace(a))
		}
	}
	return out
}

// Exported gitArgvBytes for the probe tests of other packages
func GitArgvBytes(cmdline []byte) []string { return gitArgvBytes(cmdline) }

// Drops the empty arguments of an argv list
func gitArgv(cmdline []string) []string {
	out := make([]string, 0, len(cmdline))
	for _, a := range cmdline {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

// Subcommand a git argv runs, the dashed program suffix or the first non option, empty when none
func gitSubcommandOf(cmdline []string) string {
	args := gitArgv(cmdline)
	if len(args) == 0 {
		return ""
	}
	program := gitProgram(args[0])
	if program == "" {
		return ""
	}
	if program != "git" {
		return program[len("git-"):]
	}
	for i := 1; i < len(args); i++ {
		if gitValueOptions[args[i]] {
			i++
		} else if !strings.HasPrefix(args[i], "-") {
			return args[i]
		}
	}
	return ""
}

// Exported gitSubcommandOf
func GitSubcommandOf(cmdline []string) string { return gitSubcommandOf(cmdline) }

// Reports whether a subcommand is a daemon that never takes a lock
func GitNeverLocks(sub string) bool { return neverLocks[sub] }

// The checkout's own paths as given and resolved, sorted, so another process's path needs no resolving
func checkoutPlaces(paths ...string) []string {
	set := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := pypath.Abspath(p)
		if err != nil {
			abs = pypath.Normpath(p)
		}
		set[pypath.Normcase(pypath.Normpath(abs))] = true
		real, err := pypath.Realpath(p, pypath.Lax)
		if err != nil {
			real = abs
		}
		set[pypath.Normcase(real)] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Exported checkoutPlaces
func CheckoutPlaces(paths ...string) []string { return checkoutPlaces(paths...) }

// Splits a normalized path into drive, root and tail like os.path.splitroot
func lockSplitRoot(p string) (drive, root, tail string) {
	if runtime.GOOS == "windows" {
		return pypath.NtSplitRoot(p)
	}
	if strings.HasPrefix(p, "/") {
		return "", "/", p[1:]
	}
	return "", "", p
}

// Non empty, non dot components of a path tail
func pathComponents(tail, sep string) []string {
	var out []string
	for _, c := range strings.Split(tail, sep) {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

// Longest common sub path of two normalized paths like os.path.commonpath, false across drives or roots
func commonPath(a, b string) (string, bool) {
	da, ra, ta := lockSplitRoot(a)
	db, rb, tb := lockSplitRoot(b)
	if da != db || (ra == "") != (rb == "") {
		return "", false
	}
	sep := string(filepath.Separator)
	ca, cb := pathComponents(ta, sep), pathComponents(tb, sep)
	n := 0
	for n < len(ca) && n < len(cb) && ca[n] == cb[n] {
		n++
	}
	return da + ra + strings.Join(ca[:n], sep), true
}

// Reports whether path is one of places or under it by path components, lexically
func pathWithin(path string, places []string) bool {
	if !pypath.IsAbs(path) {
		return false
	}
	lexical := pypath.Normcase(pypath.Normpath(path))
	for _, place := range places {
		if common, ok := commonPath(lexical, place); ok && common == place {
			return true
		}
	}
	return false
}

// Reports whether a git works in places by cwd, a path argument or a GIT_DIR style variable
func GitWorksIn(args []string, cwd string, env map[string]string, places []string) bool {
	inside := func(value string) bool {
		if value == "" {
			return false
		}
		if !pypath.IsAbs(value) {
			if cwd == "" {
				return false
			}
			value = pypath.Join(cwd, value)
		}
		return pathWithin(value, places)
	}
	if cwd != "" && pathWithin(cwd, places) {
		return true
	}
	for i := 1; i < len(args); i++ {
		value := args[i]
		if strings.HasPrefix(value, "-") {
			_, value, _ = strings.Cut(value, "=")
		}
		if inside(value) {
			return true
		}
	}
	for _, key := range gitLocationEnv {
		if inside(env[key]) {
			return true
		}
	}
	return false
}

// Reports whether uid may write gitDir, root, its owner or a group or world writable dir
func couldWrite(uid int, known bool, gitDir string) bool {
	info, err := os.Stat(gitDir)
	if err != nil {
		return true
	}
	return !known || uid == 0 || uid == fileOwnerUID(info) || info.Mode().Perm()&0o022 != 0
}

// Whether a running process may still own a git lock, a named holder, proven free or unknowable
func heldOpen(path, root string, anyGit bool) LockHold {
	if info, err := os.Stat("/proc/self/fd"); err == nil && info.IsDir() {
		return heldOpenProc(path, root, anyGit)
	}
	return heldOpenLsof(path, root, anyGit)
}

// Exported heldOpen
func HeldOpen(path, root string, anyGit bool) LockHold { return heldOpen(path, root, anyGit) }

// Whether a git argv keeps a lock, daemons never and pure readers only when anyGit
func countsAsHolder(args []string, anyGit bool) bool {
	sub := gitSubcommandOf(args)
	return !neverLocks[sub] && (anyGit || !readerGit[sub])
}

// Replaces the holder rule the probes consult and returns the previous one
func SwapCountsAsHolder(fn func(args []string, anyGit bool) bool) func(args []string, anyGit bool) bool {
	prev := countsAsHolderFn
	countsAsHolderFn = fn
	return prev
}

// Replaces the executable lookup of the lsof and ps probes and returns the previous one
func SwapLockWhich(fn func(name string) (string, bool)) func(name string) (string, bool) {
	prev := lockWhichFn
	lockWhichFn = fn
	return prev
}

// Replaces the subprocess runner of the lsof and ps probes and returns the previous one
func SwapLockRunner(fn func(context.Context, []string, pysubprocess.Options) (*pysubprocess.Completed, error)) func(context.Context, []string, pysubprocess.Options) (*pysubprocess.Completed, error) {
	prev := lockRunFn
	lockRunFn = fn
	return prev
}

// Reports whether the process of a /proc directory has released its memory or is gone
func exited(pidDir string) bool {
	data, err := os.ReadFile(filepath.Join(pidDir, "cmdline"))
	if err != nil {
		return !pathExists(pidDir)
	}
	return len(data) == 0
}

// Argv of a /proc directory
func procCmdline(pidDir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(pidDir, "cmdline"))
	if err != nil {
		return nil, err
	}
	return gitArgvBytes(raw), nil
}

// Comm of a /proc directory decoded as ascii with replacement and stripped
func procComm(pidDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(pidDir, "comm"))
	if err != nil {
		return "", err
	}
	return pystr.Strip(pystr.DecodeASCIIReplace(raw)), nil
}

// Environment of a /proc directory, later entries winning
func procEnviron(pidDir string) (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join(pidDir, "environ"))
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		key, value, _ := strings.Cut(pystr.DecodeUTF8Replace(entry), "=")
		env[key] = value
	}
	return env, nil
}

// Display name of a holder from its program and first two arguments, else its comm
func holderName(args []string, pidDir string) string {
	if len(args) > 0 {
		return argvLabel(args)
	}
	comm, err := procComm(pidDir)
	if err != nil {
		return "?"
	}
	return comm
}

// Program basename with up to two arguments
func argvLabel(args []string) string {
	return strings.Join(append([]string{pypath.Basename(args[0])}, args[1:min(3, len(args))]...), " ")
}

// Scans /proc for a process holding path open or a lock keeping git working in root
func heldOpenProc(path, root string, anyGit bool) LockHold {
	target, err := pypath.Realpath(path, pypath.Lax)
	if err != nil {
		target = path
	}
	var places []string
	if root != "" {
		places = checkoutPlaces(root, pypath.Dirname(path))
	}
	unknowable := false
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid := entry.Name()
		if pid == "" || pid[0] < '0' || pid[0] > '9' {
			continue
		}
		pidDir := filepath.Join("/proc", pid)
		if fds, err := os.ReadDir(filepath.Join(pidDir, "fd")); err == nil {
			for _, fd := range fds {
				link, err := os.Readlink(filepath.Join(pidDir, "fd", fd.Name()))
				if err != nil {
					break
				}
				if link == target {
					args, _ := procCmdline(pidDir)
					return lockHolder(pid, holderName(args, pidDir))
				}
			}
		}
		if places == nil {
			continue
		}
		uid, err := procOwnerUID(pidDir)
		if err != nil {
			continue
		}
		args, cmdErr := procCmdline(pidDir)
		comm, commErr := procComm(pidDir)
		if cmdErr != nil || commErr != nil {
			if pathExists(pidDir) && couldWrite(uid, true, pypath.Dirname(path)) {
				unknowable = true
			}
			continue
		}
		if len(args) == 0 {
			continue
		}
		if gitProgram(args[0]) == "" && gitProgram(comm) == "" {
			continue
		}
		if n, _ := strconv.Atoi(pid); n == os.Getpid() || !countsAsHolderFn(args, anyGit) {
			continue
		}
		cwd, cwdErr := os.Readlink(filepath.Join(pidDir, "cwd"))
		env, envErr := procEnviron(pidDir)
		if cwdErr != nil || envErr != nil {
			if exited(pidDir) {
				continue
			}
			if GitWorksIn(args, "", nil, places) {
				return lockHolder(pid, holderName(args, pidDir))
			}
			if pathExists(pidDir) && couldWrite(uid, true, pypath.Dirname(path)) {
				unknowable = true
			}
			continue
		}
		if GitWorksIn(args, strings.TrimSuffix(cwd, " (deleted)"), env, places) {
			return lockHolder(pid, holderName(args, pidDir))
		}
	}
	if unknowable {
		return lockUnknown
	}
	return lockFree
}

// Owner uid of a /proc directory
func procOwnerUID(pidDir string) (int, error) {
	info, err := os.Stat(pidDir)
	if err != nil {
		return 0, err
	}
	return fileOwnerUID(info), nil
}

// Lines starting with prefix, the prefix removed
func prefixed(lines []string, prefix string) []string {
	var out []string
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line[len(prefix):])
		}
	}
	return out
}

// First candidate that is a regular file
func firstRegularFile(paths ...string) (string, bool) {
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p, true
		}
	}
	return "", false
}

// Runs a probe command with the 20 second budget of the lsof and ps calls
func lockProbe(argv []string) (*pysubprocess.Completed, error) {
	return lockRunFn(context.Background(), argv, pysubprocess.Options{Timeout: 20 * time.Second})
}

// Asks lsof who holds path open, then ps for a lock keeping git in root when nothing does
func heldOpenLsof(path, root string, anyGit bool) LockHold {
	lsof, ok := lockWhichFn("lsof")
	if !ok {
		lsof, ok = firstRegularFile("/usr/sbin/lsof", "/usr/bin/lsof")
	}
	if !ok {
		return lockUnknown
	}
	found, err := lockProbe([]string{lsof, "-F", "pc", "--", path})
	if err != nil {
		return lockUnknown
	}
	lines := pystr.Splitlines(pystr.DecodeUTF8Replace(found.Stdout), false)
	if pids := prefixed(lines, "p"); len(pids) > 0 {
		name := "?"
		if names := prefixed(lines, "c"); len(names) > 0 {
			name = names[0]
		}
		return lockHolder(pids[0], name)
	}
	if found.Returncode != 0 && found.Returncode != 1 {
		return lockUnknown
	}
	if root == "" {
		return lockFree
	}
	return psGitHolder(lsof, pypath.Dirname(path), root, anyGit)
}

// Exported heldOpenLsof for the injected process metadata tests of other packages
func HeldOpenLsof(path, root string, anyGit bool) LockHold { return heldOpenLsof(path, root, anyGit) }

// Each pid's cwd from lsof, false when lsof cannot answer
func lsofCwds(lsof string, pids []string) (map[string]string, bool) {
	out, err := lockProbe([]string{lsof, "-a", "-d", "cwd", "-F", "pn", "-p", strings.Join(pids, ",")})
	if err != nil || (out.Returncode != 0 && out.Returncode != 1) {
		return nil, false
	}
	cwds := map[string]string{}
	pid, havePid := "", false
	for _, line := range pystr.Splitlines(pystr.DecodeUTF8Replace(out.Stdout), false) {
		if strings.HasPrefix(line, "p") {
			pid, havePid = line[1:], true
		} else if strings.HasPrefix(line, "n") && havePid {
			cwds[pid] = line[1:]
		}
	}
	return cwds, true
}

// Reports whether every rune is a digit and the text parses as an int
func digitsInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if !pystr.IsDigit(r) {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Holder among the gits ps lists that could write gitDir and work in root, unknown when unplaceable
func psGitHolder(lsof, gitDir, root string, anyGit bool) LockHold {
	psBin, ok := lockWhichFn("ps")
	if !ok {
		psBin, ok = firstRegularFile("/bin/ps", "/usr/bin/ps")
	}
	if !ok {
		return lockUnknown
	}
	ps := func(columns ...string) ([][]string, bool) {
		argv := []string{psBin, "-A"}
		for _, c := range columns {
			argv = append(argv, "-o"+c+"=")
		}
		out, err := lockProbe(argv)
		if err != nil || out.Returncode != 0 {
			return nil, false
		}
		rows := [][]string{}
		for _, line := range pystr.Splitlines(pystr.DecodeUTF8Replace(out.Stdout), false) {
			if pystr.Strip(line) != "" {
				rows = append(rows, pystr.SplitWhitespace(line, len(columns)-1))
			}
		}
		return rows, true
	}
	named, ok1 := ps("pid", "uid", "comm")
	commands, ok2 := ps("pid", "command")
	if !ok1 || !ok2 {
		return lockUnknown
	}
	argv := map[string][]string{}
	for _, row := range commands {
		if len(row) == 2 {
			argv[row[0]] = pystr.SplitWhitespace(row[1], -1)
		}
	}
	var order []string
	candidates := map[string][]string{}
	for _, row := range named {
		if len(row) != 3 {
			continue
		}
		pid, pidOK := digitsInt(row[0])
		uid, uidOK := digitsInt(row[1])
		if !pidOK || !uidOK || pid == os.Getpid() {
			continue
		}
		if gitProgram(row[2]) == "" && !(len(argv[row[0]]) > 0 && gitProgram(argv[row[0]][0]) != "") {
			continue
		}
		args := argv[row[0]]
		if len(args) == 0 || gitProgram(args[0]) == "" {
			args = []string{row[2]}
		}
		if couldWrite(uid, true, gitDir) && countsAsHolderFn(args, anyGit) {
			if _, seen := candidates[row[0]]; !seen {
				order = append(order, row[0])
			}
			candidates[row[0]] = args
		}
	}
	if len(candidates) == 0 {
		return lockFree
	}
	sorted := append([]string(nil), order...)
	sort.Strings(sorted)
	cwds, ok := lsofCwds(lsof, sorted)
	if !ok {
		return lockUnknown
	}
	places := checkoutPlaces(root, gitDir)
	for _, pid := range order {
		args := candidates[pid]
		cwd, has := cwds[pid]
		if !has {
			if n, _ := strconv.Atoi(pid); stillRunning(n) {
				return lockUnknown
			}
		}
		if GitWorksIn(args, cwd, nil, places) {
			return lockHolder(pid, argvLabel(args))
		}
	}
	return lockFree
}

// Who keeps index.lock for a user facing message, pid N (name) when it can be named
func indexLockHolder(gitDir, root string) string {
	if runtime.GOOS != "windows" {
		if held := heldOpen(filepath.Join(gitDir, "index.lock"), root, false); held.Held() {
			return "a running git (" + held.Holder + ")"
		}
	}
	return "a running git"
}

// Drops a killed git's lock once its owner is proven gone, false without proof or while a holder may live
func releaseDeadLock(lock, root string, anyGit bool) (bool, error) {
	deadline := time.Now().Add(5 * time.Second)
	for pathExists(lock) {
		held := lockUnknown
		if runtime.GOOS != "windows" {
			held = heldOpen(lock, root, anyGit)
		}
		if held.Free() || runtime.GOOS == "windows" {
			err := os.Remove(lock)
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				return true, nil
			}
			if !unlinkPermissionDenied(err) {
				return false, err
			}
		} else if held.Unknown {
			return false, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true, nil
}

// Exported releaseDeadLock
func ReleaseDeadLock(lock, root string, anyGit bool) (bool, error) {
	return releaseDeadLock(lock, root, anyGit)
}

// Exported checkoutGitDir
func CheckoutGitDir(root string) (string, error) { return checkoutGitDir(root) }

// Exported indexLockHolder
func IndexLockHolder(gitDir, root string) string { return indexLockHolder(gitDir, root) }
