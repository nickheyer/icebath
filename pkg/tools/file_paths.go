package tools

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Working directory values that mean not configured
var terminalCwdSentinels = setOf("", ".", "./", "auto", "cwd")

// Seconds a failed environment bring up during path resolution is remembered
const sshHomeRetryAfter = 30 * time.Second

// Container task key to the time of its last failed environment bring up in path resolution
var sshHomeFailedAt = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

// Remote home an SSH environment detected at connect, detected false when it only guessed
type remoteHomeEnvironment interface {
	RemoteHome() (home string, detected bool)
}

var msysDriveRe = regexp.MustCompile(`^/(?:(?:cygdrive|mnt)/)?([a-zA-Z])(/.*)?$`)

// Translates a Git Bash drive path to its native Windows form, a no op elsewhere
func msysToWindowsPath(p string) string {
	if runtime.GOOS != "windows" || p == "" {
		return p
	}
	m := msysDriveRe.FindStringSubmatch(p)
	if m == nil {
		return p
	}
	tail := strings.ReplaceAll(m[2], "/", `\`)
	if tail == "" {
		tail = `\`
	}
	return strings.ToUpper(m[1]) + ":" + tail
}

// Windows form of an MSYS style drive path, the exported form the binary uses for chat --in
func MSYSToWindowsPath(p string) string { return msysToWindowsPath(p) }

// Home directory of the OS user, empty when unknown
func userHomeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// Joins two path parts the way os.path.join does, an absolute second part replacing the first
func pyOSPathJoin(a, b string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(a, b)
	}
	if strings.HasPrefix(b, "/") || a == "" {
		return b
	}
	if strings.HasSuffix(a, "/") {
		return a + b
	}
	return a + "/" + b
}

// Expands a leading tilde through the subprocess home of the env, else the OS user database
func expandTildeFor(env *Env, p string) string {
	if p == "" || !strings.Contains(p, "~") {
		return p
	}
	if home := subprocessHome(env, nil); home != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
		if p == "~" {
			return home
		}
		return pyOSPathJoin(home, p[2:])
	}
	if !strings.HasPrefix(p, "~") {
		return p
	}
	rest := p[1:]
	name, tail := rest, ""
	if i := strings.IndexAny(rest, `/\`); i >= 0 {
		name, tail = rest[:i], rest[i:]
	}
	if name == "" {
		h := os.Getenv("HOME")
		if h == "" {
			h = userHomeDir()
		}
		if h == "" {
			return p
		}
		if tail == "" {
			return h
		}
		return strings.TrimRight(h, "/") + tail
	}
	if u, err := user.Lookup(name); err == nil && u.HomeDir != "" {
		return strings.TrimRight(u.HomeDir, "/") + tail
	}
	return p
}

// Expands a leading tilde with the process level subprocess home
func expandTilde(p string) string { return expandTildeFor(nil, p) }

// UnicodeEncodeError posixpath.expanduser raises looking up the account named after a leading tilde
func expanduserEncodeError(p string) error {
	if !strings.HasPrefix(p, "~") {
		return nil
	}
	name := p[1:]
	if i := strings.IndexByte(name, '/'); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return nil
	}
	return fsEncodeError(name)
}

// Tilde expansion that raises the account lookup encode error the Python expansion raises
func expandTildeErr(env *Env, p string) (string, error) {
	if err := expanduserEncodeError(p); err != nil {
		return "", err
	}
	return expandTildeFor(env, p), nil
}

// Process cwd as getcwd reports it, symlink free like Python os.getcwd
func pyGetcwd() (string, error) {
	if runtime.GOOS == "windows" {
		return os.Getwd()
	}
	return syscall.Getwd()
}

// UnicodeEncodeError the CPython realpath walk raises at the lstat of the first component path it cannot encode
func realpathEncodeError(p string) error {
	if runtime.GOOS == "windows" || fsEncodeError(p) == nil {
		return nil
	}
	type part struct {
		name   string
		marker bool
	}
	var rest []part
	pushParts := func(s string) int {
		segs := strings.Split(s, "/")
		for i := len(segs) - 1; i >= 0; i-- {
			rest = append(rest, part{name: segs[i]})
		}
		return len(segs)
	}
	count := pushParts(p)
	resolved := "/"
	if !strings.HasPrefix(p, "/") {
		wd, err := pyGetcwd()
		if err != nil {
			return nil
		}
		resolved = wd
	}
	seen := map[string]*string{}
	for count > 0 {
		it := rest[len(rest)-1]
		rest = rest[:len(rest)-1]
		if it.marker {
			link := rest[len(rest)-1].name
			rest = rest[:len(rest)-1]
			done := resolved
			seen[link] = &done
			continue
		}
		count--
		name := it.name
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			if i := strings.LastIndexByte(resolved, '/'); i > 0 {
				resolved = resolved[:i]
			} else {
				resolved = "/"
			}
			continue
		}
		next := resolved + "/" + name
		if resolved == "/" {
			next = "/" + name
		}
		if err := fsEncodeError(next); err != nil {
			return err
		}
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if cached, ok := seen[next]; ok {
			if cached != nil {
				resolved = *cached
			} else {
				resolved = next
			}
			continue
		}
		target, err := os.Readlink(next)
		if err != nil {
			resolved = next
			continue
		}
		if strings.HasPrefix(target, "/") {
			resolved = "/"
		}
		seen[next] = nil
		rest = append(rest, part{name: next}, part{marker: true})
		count += pushParts(target)
	}
	return nil
}

// Absolute path with symlinks resolved along the longest existing prefix
func resolveNonStrict(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	dir, base := filepath.Split(abs)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	if dir == "" || dir == abs {
		return abs
	}
	return filepath.Join(resolveNonStrict(dir), base)
}

// Realpath of a path that may not exist, used by every write guard comparison
func realPath(p string) string {
	return resolveNonStrict(expandTilde(p))
}

// posixpath.normpath, keeping exactly two leading slashes the way POSIX allows
func posixNormpath(p string) string {
	if p == "" {
		return "."
	}
	initial := 0
	if strings.HasPrefix(p, "/") {
		initial = 1
		if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
			initial = 2
		}
	}
	var comps []string
	for _, c := range strings.Split(p, "/") {
		switch {
		case c == "" || c == ".":
		case c != ".." || (initial == 0 && len(comps) == 0) || (len(comps) > 0 && comps[len(comps)-1] == ".."):
			comps = append(comps, c)
		case len(comps) > 0:
			comps = comps[:len(comps)-1]
		}
	}
	out := strings.Repeat("/", initial) + strings.Join(comps, "/")
	if out == "" {
		return "."
	}
	return out
}

// posixpath.join of a base and a relative or absolute part
func posixJoin(a, b string) string {
	if strings.HasPrefix(b, "/") || a == "" {
		return b
	}
	if strings.HasSuffix(a, "/") {
		return a + b
	}
	return a + "/" + b
}

// Terminal backend type of a task from its live environment, else the configured type
func terminalEnvTypeForTask(env *Env, taskID string) string {
	key, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		key = taskID
	}
	termPool.Lock()
	e := termPool.active[key]
	if e == nil {
		e = termPool.active[taskID]
	}
	termPool.Unlock()
	if e != nil {
		if t := e.EnvType(); t != "" {
			return t
		}
	}
	cfg, err := getEnvConfig(env)
	if err != nil {
		v := rawSecret(env, "TERMINAL_ENV")
		if v == "" {
			v = "local"
		}
		return strings.ToLower(v)
	}
	v := ""
	if raw := cfg["env_type"]; raw != nil && raw != "" && raw != false {
		v = pyStr(raw)
	}
	if v == "" {
		v = rawSecret(env, "TERMINAL_ENV")
	}
	if v == "" {
		v = "local"
	}
	return strings.ToLower(v)
}

// Reports whether a task's file paths name a container filesystem
func usesContainerPaths(env *Env, taskID string) bool {
	return isContainerBackend(terminalEnvTypeForTask(env, taskID))
}

// Backend name of a task whose file paths belong to a container, empty otherwise
func containerBackendForTask(env *Env, taskID string) string {
	if usesContainerPaths(env, taskID) {
		return terminalEnvTypeForTask(env, taskID)
	}
	return ""
}

// Expanded absolute anchor from a raw cwd value, empty for sentinels and relative values
func sentinelFreeAbsCwd(env *Env, raw any) string {
	text := ""
	if raw != nil && raw != "" && raw != false {
		text = pyStr(raw)
	}
	text = pyStrip(text)
	if terminalCwdSentinels[strings.ToLower(text)] {
		return ""
	}
	expanded := expandTildeFor(env, text)
	if !filepath.IsAbs(expanded) {
		return ""
	}
	return expanded
}

// Scope aware TERMINAL_CWD when it names a real absolute anchor
func configuredTerminalCwd(env *Env) (string, error) {
	v, err := scopeTerminalCwd(env)
	if err != nil {
		return "", err
	}
	return sentinelFreeAbsCwd(env, v), nil
}

// Registered cwd override keyed by the raw task id
func registeredTaskCwdOverride(env *Env, taskID string) string {
	overrides, err := ResolveTaskOverrides(env, taskID)
	if err != nil {
		return ""
	}
	return sentinelFreeAbsCwd(env, overrides["cwd"])
}

// Cwd override registered for the raw task id, empty when none is registered
func RegisteredTaskCwdOverride(env *Env, taskID string) string {
	return registeredTaskCwdOverride(env, taskID)
}

// Workspace root from the session cwd record, a registered override, then TERMINAL_CWD
func authoritativeWorkspaceRootFor(env *Env, taskID string) (string, error) {
	if recorded, ok := getSessionCwdFor(env, taskID); ok && recorded != "" {
		return recorded, nil
	}
	if ov := registeredTaskCwdOverride(env, taskID); ov != "" {
		return ov, nil
	}
	return configuredTerminalCwd(env)
}

// Workspace root of the default task
func authoritativeWorkspaceRoot(env *Env) (string, error) {
	return authoritativeWorkspaceRootFor(env, "default")
}

// Workspace root the session treats as authoritative
func AuthoritativeWorkspaceRoot(env *Env) (string, error) { return authoritativeWorkspaceRoot(env) }

// Launch directory of the session, the process working directory when the session names none
func processCwd(env *Env) (string, error) {
	if env != nil && env.Cwd != "" {
		return env.Cwd, nil
	}
	return os.Getwd()
}

// Expands tilde and, on host backends, translates Git Bash drive paths
func hostText(env *Env, text string, containerPaths bool) string {
	if !containerPaths {
		text = msysToWindowsPath(text)
	}
	return expandTildeFor(env, text)
}

// Host text with the tilde account lookup encode error raised the way Python raises it
func hostTextErr(env *Env, text string, containerPaths bool) (string, error) {
	if !containerPaths {
		text = msysToWindowsPath(text)
	}
	return expandTildeErr(env, text)
}

// Absolute normalized path joined onto the base when relative, host paths resolved, container paths kept pure
func anchorPath(text string, base func() (string, error), containerPaths bool) (string, error) {
	if containerPaths {
		if !strings.HasPrefix(text, "/") {
			b, err := base()
			if err != nil {
				return "", err
			}
			text = posixJoin(b, text)
		}
		return posixNormpath(text), nil
	}
	if runtime.GOOS == "windows" {
		if !filepath.IsAbs(text) {
			b, err := base()
			if err != nil {
				return "", err
			}
			text = filepath.Join(b, text)
		}
		return filepath.Clean(text), nil
	}
	if !filepath.IsAbs(text) {
		b, err := base()
		if err != nil {
			return "", err
		}
		text = pyOSPathJoin(b, text)
	}
	if err := realpathEncodeError(text); err != nil {
		return "", err
	}
	return resolveNonStrict(text), nil
}

// Working directory on the SSH target read raw from the session record, override and TERMINAL_CWD
func sshRemoteAnchor(env *Env, taskID string) (string, error) {
	recorded, _ := getSessionCwdFor(env, taskID)
	overrides, err := ResolveTaskOverrides(env, taskID)
	if err != nil {
		return "", err
	}
	scoped, err := scopeTerminalCwd(env)
	if err != nil {
		return "", err
	}
	for _, raw := range []any{recorded, overrides["cwd"], scoped} {
		text := ""
		if raw != nil && raw != "" && raw != false {
			text = pyStr(raw)
		}
		text = pyStrip(text)
		if !terminalCwdSentinels[strings.ToLower(text)] && (strings.HasPrefix(text, "~") || strings.HasPrefix(text, "/")) {
			return coerceSSHRemoteCwd(env, text, "ssh"), nil
		}
	}
	return "~", nil
}

// Terminal environment of the task's file backend, brought up through the file tools' own creator
func fileOpsEnvironment(env *Env, taskID string) (Environment, error) {
	handle, err := getFileOps(env, taskID)
	if err != nil {
		return nil, err
	}
	return handle.terminal, nil
}

// Live terminal environment of a task, else one brought up through the file tools creator
func liveOrStartedEnv(env *Env, taskID string) (Environment, error) {
	key, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return nil, err
	}
	e, err := GetActiveEnv(env, taskID)
	if err != nil {
		return nil, err
	}
	if e == nil {
		sshHomeFailedAt.Lock()
		failed, seen := sshHomeFailedAt.m[key]
		sshHomeFailedAt.Unlock()
		if seen && time.Since(failed) < sshHomeRetryAfter {
			return nil, nil
		}
		started, err := fileOpsEnvironment(env, taskID)
		if err != nil || started == nil {
			sshHomeFailedAt.Lock()
			sshHomeFailedAt.m[key] = time.Now()
			sshHomeFailedAt.Unlock()
			return nil, nil
		}
		e = started
	}
	sshHomeFailedAt.Lock()
	delete(sshHomeFailedAt.m, key)
	sshHomeFailedAt.Unlock()
	return e, nil
}

// Home of the SSH user as detected at connect, empty when unknown or only guessed
func sshRemoteHome(env *Env, taskID string) (string, error) {
	e, err := liveOrStartedEnv(env, taskID)
	if err != nil || e == nil {
		return "", err
	}
	rh, ok := e.(remoteHomeEnvironment)
	if !ok {
		return "", nil
	}
	home, detected := rh.RemoteHome()
	if !detected || !strings.HasPrefix(home, "/") {
		return "", nil
	}
	return home, nil
}

// Resolves a path in the SSH target namespace, tilde becoming the remote home once it is known
func resolveSSHPath(env *Env, taskID, filePath string) (string, error) {
	text := pyStrip(filePath)
	if !strings.HasPrefix(text, "~") && !strings.HasPrefix(text, "/") {
		anchor, err := sshRemoteAnchor(env, taskID)
		if err != nil {
			return "", err
		}
		text = posixJoin(anchor, text)
	}
	if text == "~" || strings.HasPrefix(text, "~/") {
		home, err := sshRemoteHome(env, taskID)
		if err != nil {
			return "", err
		}
		if home != "" {
			text = home + text[1:]
		}
	}
	if !strings.HasPrefix(text, "~") {
		return posixNormpath(text), nil
	}
	head, tail, _ := strings.Cut(text, "/")
	if tail == "" {
		tail = "."
	} else {
		tail = posixNormpath(tail)
	}
	if tail == "." {
		return head, nil
	}
	return head + "/" + tail, nil
}

// Reports whether a still tilde relative SSH path climbs above the home it is relative to
func sshPathEscapesHome(resolved string) bool {
	tail := ""
	if strings.HasPrefix(resolved, "~") {
		_, tail, _ = strings.Cut(resolved, "/")
	}
	return tail == ".." || strings.HasPrefix(tail, "../")
}

// Configured working directory of a container backend, the plugin sandbox home standing in for tilde
func containerDefaultCwd(env *Env, taskID string) string {
	cfg, err := getEnvConfig(env)
	if err != nil {
		envLog(env).Debug("container default cwd unavailable", "task", taskID, "error", err)
		return ""
	}
	cwd, _ := cfg["cwd"].(string)
	if cwd == "~" {
		e, err := liveOrStartedEnv(env, taskID)
		if err != nil {
			envLog(env).Debug("container default cwd unavailable", "task", taskID, "error", err)
			return ""
		}
		cwd = ""
		if e != nil {
			cwd = pluginSandboxHome(e)
		}
	}
	if strings.HasPrefix(cwd, "/") {
		return cwd
	}
	return ""
}

// Absolute base directory for relative paths, the SSH namespace for SSH tasks
func resolveBaseDirFor(env *Env, taskID string, containerPaths *bool) (string, error) {
	var cp bool
	if containerPaths == nil {
		if terminalEnvTypeForTask(env, taskID) == "ssh" {
			return resolveSSHPath(env, taskID, ".")
		}
		cp = usesContainerPaths(env, taskID)
	} else {
		cp = *containerPaths
	}
	root, err := authoritativeWorkspaceRootFor(env, taskID)
	if err != nil {
		return "", err
	}
	if root == "" && cp {
		root = containerDefaultCwd(env, taskID)
	}
	if root == "" {
		if root, err = processCwd(env); err != nil {
			return "", err
		}
	}
	text, err := hostTextErr(env, root, cp)
	if err != nil {
		return "", err
	}
	return anchorPath(text, func() (string, error) { return processCwd(env) }, cp)
}

// Base directory of the default task
func resolveBaseDir(env *Env) (string, error) { return resolveBaseDirFor(env, "default", nil) }

// Resolves a model supplied path against the task base directory
func resolvePathForTaskID(env *Env, taskID, filePath string) (string, error) {
	if terminalEnvTypeForTask(env, taskID) == "ssh" {
		return resolveSSHPath(env, taskID, filePath)
	}
	cp := usesContainerPaths(env, taskID)
	text, err := hostTextErr(env, filePath, cp)
	if err != nil {
		return "", err
	}
	return anchorPath(text, func() (string, error) { return resolveBaseDirFor(env, taskID, &cp) }, cp)
}

// Resolves a model supplied path for the default task
func resolvePathForTask(env *Env, filePath string) (string, error) {
	return resolvePathForTaskID(env, "default", filePath)
}

// Splits a directory entry keeping the last component after dropping trailing separators and dots
func splitEntry(p string) (string, string) {
	drive, root, rest := pypath.SplitRoot(p)
	tail := root + rest
	seps := "/"
	if runtime.GOOS == "windows" {
		seps = `\/`
	}
	trimmed := strings.TrimRight(tail, seps)
	if trimmed == "" && tail != "" {
		trimmed = tail[:1]
	}
	parent, leaf := pypath.Split(drive + trimmed)
	if leaf == "." && parent != "" {
		return splitEntry(parent)
	}
	return parent, leaf
}

// Resolves the parent but keeps a symlink in the last component for delete and rename
func resolveEntryForTaskID(env *Env, taskID, filePath string) (string, error) {
	parent, name := splitEntry(filePath)
	if name == "" || name == "." || name == ".." || (parent == "" && strings.HasPrefix(name, "~")) {
		return resolvePathForTaskID(env, taskID, filePath)
	}
	if parent == "" {
		parent = "."
	}
	base, err := resolvePathForTaskID(env, taskID, parent)
	if err != nil {
		return "", err
	}
	if terminalEnvTypeForTask(env, taskID) == "ssh" || usesContainerPaths(env, taskID) {
		return posixJoin(strings.TrimSuffix(base, "/")+"/", name), nil
	}
	return filepath.Join(base, name), nil
}

// Entry resolution for the default task
func resolveEntryForTask(env *Env, filePath string) (string, error) {
	return resolveEntryForTaskID(env, "default", filePath)
}

// Reports whether a resolved path equals or sits below a resolved root
func isUnder(p, root string) bool {
	if p == root {
		return true
	}
	root = strings.TrimRight(root, string(filepath.Separator))
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// Reports whether a POSIX path equals or sits below a POSIX root component wise
func isUnderPosix(p, root string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, strings.TrimRight(root, "/")+"/")
}

// Warning when a relative path resolved outside the task workspace root, empty otherwise
func pathResolutionWarningFor(env *Env, taskID, filePath, resolved string) string {
	var root string
	posix := false
	if terminalEnvTypeForTask(env, taskID) == "ssh" {
		if strings.HasPrefix(filePath, "~") || strings.HasPrefix(filePath, "/") {
			return ""
		}
		r, err := resolveSSHPath(env, taskID, ".")
		if err != nil {
			return ""
		}
		root, posix = r, true
	} else {
		if filepath.IsAbs(expandTildeFor(env, filePath)) {
			return ""
		}
		workspaceRoot, err := authoritativeWorkspaceRootFor(env, taskID)
		if err != nil || workspaceRoot == "" {
			return ""
		}
		if usesContainerPaths(env, taskID) {
			root, posix = posixNormpath(filepath.ToSlash(expandTildeFor(env, workspaceRoot))), true
		} else {
			root = resolveNonStrict(expandTildeFor(env, workspaceRoot))
		}
	}
	if (posix && isUnderPosix(resolved, root)) || (!posix && isUnder(resolved, root)) {
		return ""
	}
	return "Relative path " + pyReprStr(filePath) + " resolved to " + pyReprStr(resolved) + ", which is " +
		"OUTSIDE the active workspace (" + pyReprStr(root) + "). The edit will land in " +
		"a different directory than the terminal's cwd. If this is not " +
		"intended (e.g. a git-worktree session writing into the main " +
		"checkout), pass an absolute path under the workspace instead."
}

// Workspace divergence warning for the default task
func pathResolutionWarning(env *Env, filePath, resolved string) string {
	return pathResolutionWarningFor(env, "default", filePath, resolved)
}

// Reports whether a path contains a literal parent directory component
func hasTraversalComponent(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

// Reports a literal parent directory component
func HasTraversalComponent(p string) bool { return hasTraversalComponent(p) }

// Reports whether a resolution error is a filesystem or value failure rather than a policy refusal
func isPathResolutionFailure(err error) bool {
	var pe *os.PathError
	var le *os.LinkError
	var se *os.SyscallError
	var ue *pystr.UnicodeEncodeError
	return errors.As(err, &pe) || errors.As(err, &le) || errors.As(err, &se) || errors.As(err, &ue)
}
