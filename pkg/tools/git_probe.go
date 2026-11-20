package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// NoDriverDiffFlags disable attribute scoped diff drivers on rendered diffs
var NoDriverDiffFlags = []string{"--no-ext-diff", "--no-textconv"}

var gitConfigOverrides = [][2]string{
	{"credential.helper", ""},
	{"core.askPass", ""},
	{"core.fsmonitor", "false"},
	{"core.untrackedCache", "false"},
	{"core.hooksPath", os.DevNull},
	{"core.pager", "cat"},
	{"core.editor", "true"},
	{"sequence.editor", "true"},
	{"diff.external", ""},
	{"core.sshCommand", "ssh -o BatchMode=yes"},
}

var gitConfigInjectPrefixes = []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"}

var (
	discoveryKeysRegexp = `^(filter\..*\.(clean|smudge|process)|include\.path|includeif\..*\.path)$`
	includeIfKeyRe      = regexp.MustCompile(`(?i)^includeif\..*\.path$`)
	includeKeyRe        = regexp.MustCompile(`(?i)^include(?:if\..*)?\.path$`)
	filterCommandKeyRe  = regexp.MustCompile(`(?i)^filter\..+\.(?:clean|smudge|process)$`)
)

const (
	maxIncludeTargets = 16
	maxFilterKeys     = 256
)

// ProbeResult is the captured outcome of a bounded probe child
type ProbeResult struct {
	Code   int
	Stdout string
	Stderr string
}

func environToMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

func mapToEnviron(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func stripGitConfigInjection(env map[string]string) {
	for key := range env {
		if key == "GIT_CONFIG_PARAMETERS" {
			delete(env, key)
			continue
		}
		for _, p := range gitConfigInjectPrefixes {
			if strings.HasPrefix(key, p) {
				delete(env, key)
				break
			}
		}
	}
	delete(env, "GIT_CONFIG_COUNT")
}

var safeDirCache = struct {
	sync.Mutex
	entries map[string][]string
}{entries: map[string][]string{}}

func safeDirectoryCacheKey(base map[string]string) string {
	parts := []string{base["GIT_CONFIG_GLOBAL"], base["GIT_CONFIG_SYSTEM"], base["GIT_CONFIG_NOSYSTEM"], base["HOME"], base["XDG_CONFIG_HOME"], base["PATH"]}
	global := base["GIT_CONFIG_GLOBAL"]
	if global == "" {
		home := base["HOME"]
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		global = filepath.Join(home, ".gitconfig")
	}
	if st, err := os.Stat(global); err == nil {
		parts = append(parts, strconv.FormatInt(st.ModTime().UnixNano(), 10), strconv.FormatInt(st.Size(), 10))
	}
	return strings.Join(parts, "\x00")
}

// The user's safe.directory values in git's own effective order, memoised per config selection
func userSafeDirectories(base map[string]string) []string {
	key := safeDirectoryCacheKey(base)
	safeDirCache.Lock()
	if cached, ok := safeDirCache.entries[key]; ok {
		safeDirCache.Unlock()
		return append([]string{}, cached...)
	}
	safeDirCache.Unlock()
	env := make(map[string]string, len(base))
	for k, v := range base {
		env[k] = v
	}
	stripGitConfigInjection(env)
	env["GIT_TERMINAL_PROMPT"] = "0"
	var values []string
	for _, scope := range []string{"--system", "--global"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, "git", "config", scope, "-z", "--get-all", "safe.directory")
		cmd.Env = mapToEnviron(env)
		cmd.Stdin = nil
		hideConsoleWindow(cmd)
		out, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}
		records := strings.Split(string(out), "\x00")
		if len(records) > 0 && records[len(records)-1] == "" {
			records = records[:len(records)-1]
		}
		values = append(values, records...)
	}
	safeDirCache.Lock()
	safeDirCache.entries[key] = append([]string{}, values...)
	safeDirCache.Unlock()
	return values
}

// Environment for internal git invocations that must never prompt or run user hooks
func noninteractiveGitEnv(base map[string]string) map[string]string {
	if base == nil {
		base = environToMap(os.Environ())
	}
	env := make(map[string]string, len(base)+16)
	for k, v := range base {
		env[k] = v
	}
	safeDirs := userSafeDirectories(base)
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GCM_INTERACTIVE"] = "Never"
	stripGitConfigInjection(env)
	env["GIT_CONFIG_GLOBAL"] = os.DevNull
	env["GIT_CONFIG_SYSTEM"] = os.DevNull
	env["GIT_CONFIG_NOSYSTEM"] = "1"
	env["GIT_PAGER"] = "cat"
	env["PAGER"] = "cat"
	env["GIT_EDITOR"] = "true"
	overrides := append([][2]string{}, gitConfigOverrides...)
	for _, v := range safeDirs {
		overrides = append(overrides, [2]string{"safe.directory", v})
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(len(overrides))
	for i, kv := range overrides {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = kv[0]
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = kv[1]
	}
	return env
}

// Environment for internal git and gh runs that never prompt, page or read user git config
func NoninteractiveGitEnv(base map[string]string) map[string]string {
	return noninteractiveGitEnv(base)
}

// Runs a probe child with captured pipes, nil on spawn failure or timeout
func boundedProbeRun(argv []string, timeout time.Duration, env map[string]string, cwd string, input *string) *ProbeResult {
	cmd := exec.Command(argv[0], argv[1:]...)
	if env != nil {
		cmd.Env = mapToEnviron(env)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	if input != nil {
		cmd.Stdin = strings.NewReader(*input)
	}
	hideConsoleWindow(cmd)
	proc.Group(cmd)
	child, err := StartPiped(cmd)
	if err != nil {
		return nil
	}
	stdout, stderr, ok := child.Communicate(timeout)
	if !ok {
		KillAndDrain(child, time.Second)
		return nil
	}
	code := 0
	if err := child.Err(); err != nil {
		ee, isExit := err.(*exec.ExitError)
		if !isExit {
			return nil
		}
		code = pysubprocess.Returncode(ee)
	}
	return &ProbeResult{Code: code, Stdout: string(stdout), Stderr: string(stderr)}
}

// Runs a short git probe and returns stripped stdout, empty on any failure
func boundedGitProbe(argv []string, timeout time.Duration, env map[string]string) string {
	if env == nil {
		env = noninteractiveGitEnv(nil)
	}
	merged := make(map[string]string, len(env)+1)
	for k, v := range env {
		merged[k] = v
	}
	merged["GIT_NO_LAZY_FETCH"] = "1"
	r := boundedProbeRun(argv, timeout, merged, "", nil)
	if r == nil || r.Code != 0 {
		return ""
	}
	return strings.TrimSpace(r.Stdout)
}

// Hardens internal git for one repository including its named filter drivers, nil when discovery cannot be trusted
func noninteractiveRepoGitEnv(cwd string, base map[string]string) map[string]string {
	env := noninteractiveGitEnv(base)
	p := boundedProbeRun([]string{"git", "-C", cwd, "config", "--includes", "--show-origin", "-z", "--get-regexp", discoveryKeysRegexp}, 2*time.Second, env, "", nil)
	if p == nil || (p.Code != 0 && p.Code != 1) {
		return nil
	}
	var names []string
	targets := map[string]bool{}
	toplevel := ""
	fields := strings.Split(p.Stdout, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		origin, entry := fields[i], fields[i+1]
		key, value, _ := strings.Cut(entry, "\n")
		if !includeIfKeyRe.MatchString(key) {
			names = append(names, key)
			continue
		}
		if !strings.HasPrefix(origin, "file:") {
			return nil
		}
		originPath := origin[len("file:"):]
		if !filepath.IsAbs(originPath) {
			if env["GIT_DIR"] != "" || env["GIT_WORK_TREE"] != "" {
				return nil
			}
			if toplevel == "" {
				top := boundedProbeRun([]string{"git", "-C", cwd, "rev-parse", "--show-toplevel"}, 2*time.Second, env, "", nil)
				if top == nil || top.Code != 0 {
					return nil
				}
				toplevel = strings.TrimRight(top.Stdout, "\r\n")
			}
			originPath = filepath.Join(toplevel, originPath)
		}
		target := filepath.Join(filepath.Dir(originPath), expandUser(value))
		if resolved, err := filepath.EvalSymlinks(target); err == nil {
			target = resolved
		}
		if abs, err := filepath.Abs(target); err == nil {
			target = abs
		}
		st, err := os.Stat(target)
		if targets[target] || err != nil {
			continue
		}
		if !st.Mode().IsRegular() || len(targets) >= maxIncludeTargets {
			return nil
		}
		targets[target] = true
		probe := boundedProbeRun([]string{"git", "config", "--file", target, "--name-only", "-z", "--get-regexp", discoveryKeysRegexp}, 2*time.Second, env, "", nil)
		if probe == nil || (probe.Code != 0 && probe.Code != 1) {
			return nil
		}
		found := strings.Split(probe.Stdout, "\x00")
		for _, name := range found {
			if includeKeyRe.MatchString(name) {
				return nil
			}
		}
		names = append(names, found...)
	}
	var keys, required []string
	seen := map[string]bool{}
	for _, raw := range names {
		key := strings.TrimSpace(raw)
		if key == "" || seen[key] || !filterCommandKeyRe.MatchString(key) {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
		if len(keys) > maxFilterKeys {
			return nil
		}
		idx := strings.LastIndex(key, ".")
		requiredKey := key[:idx] + ".required"
		if !seen[requiredKey] {
			seen[requiredKey] = true
			required = append(required, requiredKey)
		}
	}
	start, _ := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	offset := 0
	for _, key := range keys {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(start+offset)] = key
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(start+offset)] = ""
		offset++
	}
	for _, key := range required {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(start+offset)] = key
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(start+offset)] = "false"
		offset++
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(start + offset)
	return env
}

// NoninteractiveRepoGitEnv hardens git for one repository, nil when filter discovery cannot be trusted
func NoninteractiveRepoGitEnv(cwd string, base map[string]string) map[string]string {
	return noninteractiveRepoGitEnv(cwd, base)
}
