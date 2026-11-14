package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	discoveryKeysRegexp = `^(filter\..*\.(clean|smudge|process)|include\.path|includeif\..*\.path)$`
	maxIncludeTargets   = 16
	maxFilterKeys       = 256
	filterDiscoveryText = "git filter discovery failed"
)

var (
	filterCommandKey = regexp.MustCompile(`(?i)^filter\..+\.(?:clean|smudge|process)$`)
	includeIfKey     = regexp.MustCompile(`(?i)^includeif\..*\.path$`)
	includeKey       = regexp.MustCompile(`(?i)^include(?:if\..*)?\.path$`)
)

type configPair struct {
	key   string
	value string
}

func gitConfigOverrides() []configPair {
	return []configPair{
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
}

type result struct {
	args   []string
	stdout []byte
	stderr []byte
	code   int
}

func (r *result) ok() bool { return r.code == 0 }

func (r *result) text() string { return lineText(r.stdout) }

func (r *result) out() string { return strings.TrimSpace(r.text()) }

func (r *result) errText() string {
	return strings.TrimSpace(strings.ToValidUTF8(lineText(r.stderr), string(rune(0xFFFD))))
}

func lineText(b []byte) string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

type call struct {
	dir     string
	env     map[string]string
	timeout time.Duration
	input   []byte
}

func environMap(list []string) map[string]string {
	env := make(map[string]string, len(list))
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		env[envKey(k)] = v
	}
	return env
}

func environList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

func cloneEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+8)
	for k, v := range env {
		out[k] = v
	}
	return out
}

func (m *Manager) processEnv(ctx context.Context) map[string]string {
	return m.selectGit(ctx, environMap(os.Environ()))
}

func (m *Manager) run(ctx context.Context, name string, args []string, c call) (*result, error) {
	env := c.env
	if env == nil {
		env = m.processEnv(ctx)
	}
	bin := lookPath(env[envKey("PATH")], name)
	if bin == "" {
		return nil, &RunError{Command: name, Err: exec.ErrNotFound}
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = environList(env)
	cmd.Dir = c.dir
	if c.input != nil {
		cmd.Stdin = bytes.NewReader(c.input)
	}
	proc.Hide(cmd)
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	res, err := proc.Run(rctx, cmd)
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	if rctx.Err() != nil {
		return nil, &DeadlineError{Command: name, Args: args, After: c.timeout}
	}
	if err != nil {
		return nil, &RunError{Command: name, Err: err}
	}
	return &result{args: args, stdout: res.Stdout, stderr: res.Stderr, code: res.Exit.Code}, nil
}

func (m *Manager) git(ctx context.Context, dir string, timeout time.Duration, env map[string]string, args ...string) (*result, error) {
	return m.run(ctx, "git", args, call{dir: dir, env: env, timeout: timeout})
}

func (m *Manager) gitOut(ctx context.Context, dir string, timeout time.Duration, env map[string]string, args ...string) (string, bool, error) {
	r, err := m.git(ctx, dir, timeout, env, args...)
	if err != nil {
		return "", false, err
	}
	if !r.ok() {
		return "", false, nil
	}
	return r.out(), true, nil
}

func (m *Manager) gitQuiet(ctx context.Context, dir string, timeout time.Duration, env map[string]string, logMsg string, args ...string) error {
	if _, err := m.git(ctx, dir, timeout, env, args...); err != nil {
		if fatal(ctx, err) {
			return err
		}
		if logMsg != "" {
			slog.Debug(logMsg, "error", err)
		}
	}
	return nil
}

func (m *Manager) noninteractiveEnv(ctx context.Context) map[string]string {
	base := m.processEnv(ctx)
	return m.harden(ctx, base)
}

func (m *Manager) harden(ctx context.Context, base map[string]string) map[string]string {
	safe := m.safeDirectories(ctx, base)
	env := cloneEnv(base)
	env[envKey("GIT_TERMINAL_PROMPT")] = "0"
	env[envKey("GCM_INTERACTIVE")] = "Never"
	stripConfigInjection(env)
	env[envKey("GIT_CONFIG_GLOBAL")] = os.DevNull
	env[envKey("GIT_CONFIG_SYSTEM")] = os.DevNull
	env[envKey("GIT_CONFIG_NOSYSTEM")] = "1"
	env[envKey("GIT_PAGER")] = "cat"
	env[envKey("PAGER")] = "cat"
	env[envKey("GIT_EDITOR")] = "true"
	overrides := gitConfigOverrides()
	for _, v := range safe {
		overrides = append(overrides, configPair{"safe.directory", v})
	}
	env[envKey("GIT_CONFIG_COUNT")] = strconv.Itoa(len(overrides))
	for i, p := range overrides {
		env[envKey(fmt.Sprintf("GIT_CONFIG_KEY_%d", i))] = p.key
		env[envKey(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))] = p.value
	}
	return env
}

func stripConfigInjection(env map[string]string) {
	for k := range env {
		if k == envKey("GIT_CONFIG_PARAMETERS") || strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			delete(env, k)
		}
	}
	delete(env, envKey("GIT_CONFIG_COUNT"))
}

func appendConfig(env map[string]string, pairs []configPair) {
	start, _ := strconv.Atoi(env[envKey("GIT_CONFIG_COUNT")])
	for i, p := range pairs {
		env[envKey(fmt.Sprintf("GIT_CONFIG_KEY_%d", start+i))] = p.key
		env[envKey(fmt.Sprintf("GIT_CONFIG_VALUE_%d", start+i))] = p.value
	}
	env[envKey("GIT_CONFIG_COUNT")] = strconv.Itoa(start + len(pairs))
}

func fileVersion(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

func (m *Manager) safeDirectories(ctx context.Context, base map[string]string) []string {
	userHome := base[envKey("HOME")]
	xdg := base[envKey("XDG_CONFIG_HOME")]
	if xdg == "" {
		xdg = filepath.Join(userHome, ".config")
	}
	system := base[envKey("GIT_CONFIG_SYSTEM")]
	if system == "" {
		system = "/etc/gitconfig"
	}
	global := base[envKey("GIT_CONFIG_GLOBAL")]
	if global == "" {
		global = filepath.Join(userHome, ".gitconfig")
	}
	key := strings.Join([]string{
		base[envKey("GIT_CONFIG_GLOBAL")], base[envKey("GIT_CONFIG_SYSTEM")], base[envKey("GIT_CONFIG_NOSYSTEM")],
		userHome, base[envKey("XDG_CONFIG_HOME")], base[envKey("PATH")],
		fileVersion(system), fileVersion(global), fileVersion(filepath.Join(xdg, "git", "config")),
	}, "\x00")
	m.safeMu.Lock()
	cached, ok := m.safeCache[key]
	m.safeMu.Unlock()
	if ok {
		return slices.Clone(cached)
	}
	env := cloneEnv(base)
	stripConfigInjection(env)
	env[envKey("GIT_TERMINAL_PROMPT")] = "0"
	var values []string
	for _, scope := range []string{"--system", "--global"} {
		r, err := m.run(ctx, "git", []string{"config", scope, "-z", "--get-all", "safe.directory"}, call{env: env, timeout: 5 * time.Second})
		if err != nil || !r.ok() {
			continue
		}
		records := strings.Split(string(r.stdout), "\x00")
		if len(records) > 0 && records[len(records)-1] == "" {
			records = records[:len(records)-1]
		}
		values = append(values, records...)
	}
	if ctx.Err() != nil {
		return values
	}
	m.safeMu.Lock()
	m.safeCache[key] = slices.Clone(values)
	m.safeMu.Unlock()
	return values
}

func (m *Manager) repoEnv(ctx context.Context, cwd string) (map[string]string, bool, error) {
	env := m.noninteractiveEnv(ctx)
	probe, err := m.run(ctx, "git", []string{"-C", cwd, "config", "--includes", "--show-origin", "-z", "--get-regexp", discoveryKeysRegexp}, call{env: env, timeout: 2 * time.Second})
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, nil
	}
	if probe.code != 0 && probe.code != 1 {
		return nil, false, nil
	}
	var names []string
	targets := map[string]bool{}
	toplevel := ""
	fields := strings.Split(lineText(probe.stdout), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		origin, entry := fields[i], fields[i+1]
		key, value, _ := strings.Cut(entry, "\n")
		if !includeIfKey.MatchString(key) {
			names = append(names, key)
			continue
		}
		if !strings.HasPrefix(origin, "file:") {
			return nil, false, nil
		}
		originPath := strings.TrimPrefix(origin, "file:")
		if !filepath.IsAbs(originPath) {
			if env[envKey("GIT_DIR")] != "" || env[envKey("GIT_WORK_TREE")] != "" {
				return nil, false, nil
			}
			if toplevel == "" {
				top, err := m.run(ctx, "git", []string{"-C", cwd, "rev-parse", "--show-toplevel"}, call{env: env, timeout: 2 * time.Second})
				if err != nil {
					if ctx.Err() != nil {
						return nil, false, ctx.Err()
					}
					return nil, false, nil
				}
				if !top.ok() {
					return nil, false, nil
				}
				toplevel = strings.TrimRight(lineText(top.stdout), "\r\n")
			}
			originPath = filepath.Join(toplevel, originPath)
		}
		raw := home.ExpandUser(value)
		if !filepath.IsAbs(raw) {
			raw = filepath.Join(filepath.Dir(originPath), raw)
		}
		target := resolvePath(raw)
		if targets[target] {
			continue
		}
		info, statErr := os.Stat(target)
		if statErr != nil {
			continue
		}
		if !info.Mode().IsRegular() || len(targets) >= maxIncludeTargets {
			return nil, false, nil
		}
		targets[target] = true
		found, err := m.run(ctx, "git", []string{"config", "--file", target, "--name-only", "-z", "--get-regexp", discoveryKeysRegexp}, call{env: env, timeout: 2 * time.Second})
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			return nil, false, nil
		}
		if found.code != 0 && found.code != 1 {
			return nil, false, nil
		}
		list := strings.Split(lineText(found.stdout), "\x00")
		for _, name := range list {
			if includeKey.MatchString(name) {
				return nil, false, nil
			}
		}
		names = append(names, list...)
	}
	var keys, required []string
	seen := map[string]bool{}
	for _, raw := range names {
		key := strings.TrimSpace(raw)
		if key == "" || seen[key] || !filterCommandKey.MatchString(key) {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
		if len(keys) > maxFilterKeys {
			return nil, false, nil
		}
		req := key[:strings.LastIndex(key, ".")] + ".required"
		if !seen[req] {
			seen[req] = true
			required = append(required, req)
		}
	}
	pairs := make([]configPair, 0, len(keys)+len(required))
	for _, k := range keys {
		pairs = append(pairs, configPair{k, ""})
	}
	for _, k := range required {
		pairs = append(pairs, configPair{k, "false"})
	}
	appendConfig(env, pairs)
	return env, true, nil
}

func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	dir, rest := abs, ""
	for {
		parent := filepath.Dir(dir)
		if rest == "" {
			rest = filepath.Base(dir)
		} else {
			rest = filepath.Join(filepath.Base(dir), rest)
		}
		if parent == dir {
			return abs
		}
		if real, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(real, rest)
		}
		dir = parent
	}
}

func joinPath(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
