package prompt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	gitDiscoveryKeys     = `^(filter\..*\.(clean|smudge|process)|include\.path|includeif\..*\.path)$`
	gitMaxIncludeTargets = 16
	gitMaxFilterKeys     = 256
	gitDiscoveryTimeout  = 2 * time.Second
	gitSafeDirTimeout    = 5 * time.Second
)

var (
	gitFilterCommandKey = regexp.MustCompile(`(?i)^filter\..+\.(?:clean|smudge|process)$`)
	gitIncludeIfKey     = regexp.MustCompile(`(?i)^includeif\..*\.path$`)
	gitIncludeKey       = regexp.MustCompile(`(?i)^include(?:if\..*)?\.path$`)
)

type gitPair struct {
	key   string
	value string
}

func gitConfigOverrides() []gitPair {
	return []gitPair{
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

type gitResult struct {
	stdout string
	code   int
}

type gitRunner struct {
	mu   sync.Mutex
	safe map[string][]string
}

func newGitRunner() *gitRunner { return &gitRunner{safe: map[string][]string{}} }

func gitEnvironMap(list []string) map[string]string {
	out := make(map[string]string, len(list))
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		out[envKey(k)] = v
	}
	return out
}

func gitEnvironList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

func cloneGitEnv(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+32)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func gitText(b []byte) string {
	s := strings.ToValidUTF8(string(b), string(rune(0xFFFD)))
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func (g *gitRunner) run(ctx context.Context, environ map[string]string, timeout time.Duration, args ...string) (gitResult, bool) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return gitResult{}, false
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = gitEnvironList(environ)
	proc.Hide(cmd)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := proc.Run(rctx, cmd)
	if err != nil || rctx.Err() != nil {
		return gitResult{}, false
	}
	return gitResult{stdout: gitText(res.Stdout), code: res.Exit.Code}, true
}

func stripGitConfigInjection(environ map[string]string) {
	for k := range environ {
		if k == envKey("GIT_CONFIG_PARAMETERS") || strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			delete(environ, k)
		}
	}
	delete(environ, envKey("GIT_CONFIG_COUNT"))
}

func gitFileVersion(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

func (g *gitRunner) safeDirectories(ctx context.Context, base map[string]string) []string {
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
		gitFileVersion(system), gitFileVersion(global), gitFileVersion(filepath.Join(xdg, "git", "config")),
	}, "\x00")
	g.mu.Lock()
	cached, ok := g.safe[key]
	g.mu.Unlock()
	if ok {
		return slices.Clone(cached)
	}
	environ := cloneGitEnv(base)
	stripGitConfigInjection(environ)
	environ[envKey("GIT_TERMINAL_PROMPT")] = "0"
	var values []string
	for _, scope := range []string{"--system", "--global"} {
		r, ok := g.run(ctx, environ, gitSafeDirTimeout, "config", scope, "-z", "--get-all", "safe.directory")
		if !ok || r.code != 0 {
			continue
		}
		records := strings.Split(r.stdout, "\x00")
		if len(records) > 0 && records[len(records)-1] == "" {
			records = records[:len(records)-1]
		}
		values = append(values, records...)
	}
	if ctx.Err() != nil {
		return values
	}
	g.mu.Lock()
	g.safe[key] = slices.Clone(values)
	g.mu.Unlock()
	return values
}

func (g *gitRunner) noninteractiveEnv(ctx context.Context) map[string]string {
	base := gitEnvironMap(os.Environ())
	safe := g.safeDirectories(ctx, base)
	environ := cloneGitEnv(base)
	environ[envKey("GIT_TERMINAL_PROMPT")] = "0"
	environ[envKey("GCM_INTERACTIVE")] = "Never"
	stripGitConfigInjection(environ)
	environ[envKey("GIT_CONFIG_GLOBAL")] = os.DevNull
	environ[envKey("GIT_CONFIG_SYSTEM")] = os.DevNull
	environ[envKey("GIT_CONFIG_NOSYSTEM")] = "1"
	environ[envKey("GIT_PAGER")] = "cat"
	environ[envKey("PAGER")] = "cat"
	environ[envKey("GIT_EDITOR")] = "true"
	overrides := gitConfigOverrides()
	for _, v := range safe {
		overrides = append(overrides, gitPair{"safe.directory", v})
	}
	environ[envKey("GIT_CONFIG_COUNT")] = strconv.Itoa(len(overrides))
	for i, p := range overrides {
		environ[envKey("GIT_CONFIG_KEY_"+strconv.Itoa(i))] = p.key
		environ[envKey("GIT_CONFIG_VALUE_"+strconv.Itoa(i))] = p.value
	}
	return environ
}

func (g *gitRunner) repoEnv(ctx context.Context, cwd string) (map[string]string, bool) {
	environ := g.noninteractiveEnv(ctx)
	probe, ok := g.run(ctx, environ, gitDiscoveryTimeout, "-C", cwd, "config", "--includes", "--show-origin", "-z", "--get-regexp", gitDiscoveryKeys)
	if !ok || (probe.code != 0 && probe.code != 1) {
		return nil, false
	}
	var names []string
	targets := map[string]bool{}
	toplevel, haveTop := "", false
	fields := strings.Split(probe.stdout, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		origin, entry := fields[i], fields[i+1]
		key, value, _ := strings.Cut(entry, "\n")
		if !gitIncludeIfKey.MatchString(key) {
			names = append(names, key)
			continue
		}
		if !strings.HasPrefix(origin, "file:") {
			return nil, false
		}
		originPath := strings.TrimPrefix(origin, "file:")
		if !filepath.IsAbs(originPath) {
			if environ[envKey("GIT_DIR")] != "" || environ[envKey("GIT_WORK_TREE")] != "" {
				return nil, false
			}
			if !haveTop {
				top, ok := g.run(ctx, environ, gitDiscoveryTimeout, "-C", cwd, "rev-parse", "--show-toplevel")
				if !ok || top.code != 0 {
					return nil, false
				}
				toplevel, haveTop = strings.TrimRight(top.stdout, "\r\n"), true
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
		info, err := os.Stat(target)
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() || len(targets) >= gitMaxIncludeTargets {
			return nil, false
		}
		targets[target] = true
		found, ok := g.run(ctx, environ, gitDiscoveryTimeout, "config", "--file", target, "--name-only", "-z", "--get-regexp", gitDiscoveryKeys)
		if !ok || (found.code != 0 && found.code != 1) {
			return nil, false
		}
		list := strings.Split(found.stdout, "\x00")
		for _, name := range list {
			if gitIncludeKey.MatchString(name) {
				return nil, false
			}
		}
		names = append(names, list...)
	}
	var keys, required []string
	seen := map[string]bool{}
	for _, raw := range names {
		key := trim(raw)
		if key == "" || seen[key] || !gitFilterCommandKey.MatchString(key) {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
		if len(keys) > gitMaxFilterKeys {
			return nil, false
		}
		req := key[:strings.LastIndex(key, ".")] + ".required"
		if !seen[req] {
			seen[req] = true
			required = append(required, req)
		}
	}
	start, _ := strconv.Atoi(environ[envKey("GIT_CONFIG_COUNT")])
	pairs := make([]gitPair, 0, len(keys)+len(required))
	for _, k := range keys {
		pairs = append(pairs, gitPair{k, ""})
	}
	for _, k := range required {
		pairs = append(pairs, gitPair{k, "false"})
	}
	for i, p := range pairs {
		environ[envKey("GIT_CONFIG_KEY_"+strconv.Itoa(start+i))] = p.key
		environ[envKey("GIT_CONFIG_VALUE_"+strconv.Itoa(start+i))] = p.value
	}
	environ[envKey("GIT_CONFIG_COUNT")] = strconv.Itoa(start + len(pairs))
	return environ, true
}

func (g *gitRunner) probe(ctx context.Context, environ map[string]string, timeout time.Duration, args ...string) string {
	if environ == nil {
		environ = g.noninteractiveEnv(ctx)
	} else {
		environ = cloneGitEnv(environ)
	}
	environ[envKey("GIT_NO_LAZY_FETCH")] = "1"
	r, ok := g.run(ctx, environ, timeout, args...)
	if !ok || r.code != 0 {
		return ""
	}
	return trim(r.stdout)
}
