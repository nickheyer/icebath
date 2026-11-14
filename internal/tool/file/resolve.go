package file

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/approval"
	"github.com/nickheyer/icebath/internal/tool/env"
)

const remoteHomeRetryAfter = 30 * time.Second

type Terminal interface {
	tool.EnvSource
	SessionCwd(ctx context.Context, sessionKey string) (string, bool)
	RecordSessionCwd(ctx context.Context, sessionKey, cwd string)
	ResolveTaskOverrides(ctx context.Context, taskID string) (env.TaskOverrides, error)
	ContainerKey(ctx context.Context, taskID string) (string, error)
	Settings() *env.Settings
}

var (
	_ Terminal    = (*env.Manager)(nil)
	_ env.FileOps = (*Tasks)(nil)
)

type RemoteHome interface {
	RemoteHome() (dir string, detected bool)
}

type sandboxHomer interface {
	SandboxHome() (string, bool)
}

type Paths struct {
	terminal Terminal
	homes    *home.Resolver

	mu       sync.Mutex
	failedAt map[string]time.Time
}

func NewPaths(terminal Terminal, homes *home.Resolver) (*Paths, error) {
	switch {
	case terminal == nil:
		return nil, &tool.UnfilledError{Name: "file.Paths.Terminal"}
	case terminal.Settings() == nil:
		return nil, &tool.UnfilledError{Name: "file.Paths.Terminal.Settings"}
	case homes == nil:
		return nil, &tool.UnfilledError{Name: "file.Paths.Homes"}
	}
	return &Paths{terminal: terminal, homes: homes, failedAt: make(map[string]time.Time)}, nil
}

func hostStyle() pathStyle {
	return pathStyle{windows: runtime.GOOS == "windows"}
}

func posixNorm(p string) string {
	if p == "" {
		return "."
	}
	lead := ""
	if strings.HasPrefix(p, "/") {
		lead = "/"
		if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
			lead = "//"
		}
	}
	var kept []string
	for _, part := range strings.Split(p, "/") {
		switch {
		case part == "" || part == ".":
			continue
		case part != "..":
			kept = append(kept, part)
		case len(kept) > 0 && kept[len(kept)-1] != "..":
			kept = kept[:len(kept)-1]
		case lead == "":
			kept = append(kept, part)
		}
	}
	out := lead + strings.Join(kept, "/")
	if out == "" {
		return "."
	}
	return out
}

func hostNorm(p string) string {
	if runtime.GOOS == "windows" {
		return filepath.Clean(p)
	}
	return posixNorm(p)
}

func isTerminalCwdSentinel(text string) bool {
	switch strings.ToLower(text) {
	case "", ".", "./", "auto", "cwd":
		return true
	}
	return false
}

func environmentPart[T any](e tool.Environment) (T, bool) {
	if v, ok := e.(T); ok {
		return v, true
	}
	if s, ok := e.(interface{ Backend() env.Backend }); ok {
		if v, ok := s.Backend().(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

func (p *Paths) ExpandTilde(ctx context.Context, path string) string {
	if path == "" || !strings.Contains(path, "~") {
		return path
	}
	if h := p.homes.SubprocessHome(ctx, nil); h != "" && (path == "~" || strings.HasPrefix(path, "~/")) {
		if path == "~" {
			return h
		}
		return hostStyle().join(h, path[2:])
	}
	return home.ExpandUser(path)
}

func (p *Paths) EnvKind(ctx context.Context, taskID string) string {
	kind := strings.ToLower(p.terminal.Kind(ctx, taskID))
	if kind == "" {
		return "local"
	}
	return kind
}

func (p *Paths) usesContainerPaths(ctx context.Context, taskID string) bool {
	return p.terminal.Settings().IsContainerBackend(ctx, p.EnvKind(ctx, taskID))
}

func (p *Paths) ContainerBackend(ctx context.Context, taskID string) (string, bool) {
	if !p.usesContainerPaths(ctx, taskID) {
		return "", false
	}
	return p.EnvKind(ctx, taskID), true
}

func (p *Paths) sentinelFreeAbsCwd(ctx context.Context, raw string) string {
	raw = trimSpace(raw)
	if isTerminalCwdSentinel(raw) {
		return ""
	}
	expanded := p.ExpandTilde(ctx, raw)
	if filepath.IsAbs(expanded) {
		return expanded
	}
	return ""
}

func (p *Paths) TaskCwdOverride(ctx context.Context, taskID string) string {
	overrides, err := p.terminal.ResolveTaskOverrides(ctx, taskID)
	if err != nil {
		return ""
	}
	return p.sentinelFreeAbsCwd(ctx, overrides["cwd"])
}

func (p *Paths) WorkspaceRoot(ctx context.Context, taskID string) (string, error) {
	if recorded, ok := p.terminal.SessionCwd(ctx, taskID); ok && recorded != "" {
		return recorded, nil
	}
	if override := p.TaskCwdOverride(ctx, taskID); override != "" {
		return override, nil
	}
	configured, err := env.ScopeTerminalCwd(ctx, p.terminal.Settings())
	if err != nil {
		return "", err
	}
	return p.sentinelFreeAbsCwd(ctx, configured), nil
}

func (p *Paths) hostText(ctx context.Context, text string, containerPaths bool) string {
	if !containerPaths {
		text = env.MsysToWindowsPath(text)
	}
	return p.ExpandTilde(ctx, text)
}

func (p *Paths) anchor(text string, base func() (string, error), containerPaths bool) (string, error) {
	style := pathStyle{}
	if !containerPaths {
		style = hostStyle()
	}
	if !style.isAbs(text) {
		root, err := base()
		if err != nil {
			return "", err
		}
		text = style.join(root, text)
	}
	switch {
	case containerPaths:
		return posixNorm(text), nil
	case runtime.GOOS == "windows":
		return filepath.Clean(text), nil
	}
	return realPath(text), nil
}

func (p *Paths) sshRemoteAnchor(ctx context.Context, taskID string) (string, error) {
	recorded, _ := p.terminal.SessionCwd(ctx, taskID)
	overrides, err := p.terminal.ResolveTaskOverrides(ctx, taskID)
	if err != nil {
		return "", err
	}
	configured, err := env.ScopeTerminalCwd(ctx, p.terminal.Settings())
	if err != nil {
		return "", err
	}
	for _, raw := range [...]string{recorded, overrides["cwd"], configured} {
		text := trimSpace(raw)
		if !isTerminalCwdSentinel(text) && (strings.HasPrefix(text, "~") || strings.HasPrefix(text, "/")) {
			return p.terminal.Settings().CoerceSSHRemoteCwd(ctx, text, "ssh"), nil
		}
	}
	return "~", nil
}

func (p *Paths) liveOrStartedEnv(ctx context.Context, taskID string) (tool.Environment, error) {
	key, err := p.terminal.ContainerKey(ctx, taskID)
	if err != nil {
		return nil, err
	}
	live, ok, err := p.terminal.Active(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if !ok {
		p.mu.Lock()
		failed, seen := p.failedAt[key]
		p.mu.Unlock()
		if seen && time.Since(failed) < remoteHomeRetryAfter {
			return nil, nil
		}
		started, err := p.terminal.Acquire(ctx, &tool.Call{TaskID: taskID})
		if err != nil {
			p.mu.Lock()
			p.failedAt[key] = time.Now()
			p.mu.Unlock()
			return nil, nil
		}
		live = started
	}
	p.mu.Lock()
	delete(p.failedAt, key)
	p.mu.Unlock()
	return live, nil
}

func (p *Paths) sshRemoteHome(ctx context.Context, taskID string) (string, error) {
	live, err := p.liveOrStartedEnv(ctx, taskID)
	if err != nil || live == nil {
		return "", err
	}
	remote, ok := environmentPart[RemoteHome](live)
	if !ok {
		return "", nil
	}
	dir, detected := remote.RemoteHome()
	if !detected || !strings.HasPrefix(dir, "/") {
		return "", nil
	}
	return dir, nil
}

func (p *Paths) resolveSSH(ctx context.Context, path, taskID string) (string, error) {
	text := trimSpace(path)
	if !strings.HasPrefix(text, "~") && !strings.HasPrefix(text, "/") {
		anchor, err := p.sshRemoteAnchor(ctx, taskID)
		if err != nil {
			return "", err
		}
		text = pathStyle{}.join(anchor, text)
	}
	if text == "~" || strings.HasPrefix(text, "~/") {
		remoteHome, err := p.sshRemoteHome(ctx, taskID)
		if err != nil {
			return "", err
		}
		if remoteHome != "" {
			text = remoteHome + text[1:]
		}
	}
	if !strings.HasPrefix(text, "~") {
		return posixNorm(text), nil
	}
	head, tail, _ := strings.Cut(text, "/")
	if tail == "" {
		return head, nil
	}
	tail = strings.TrimLeft(posixNorm(tail), "/")
	if tail == "." || tail == "" {
		return head, nil
	}
	return head + "/" + tail, nil
}

func sshPathEscapesHome(resolved string) bool {
	if !strings.HasPrefix(resolved, "~") {
		return false
	}
	_, tail, _ := strings.Cut(resolved, "/")
	return tail == ".." || strings.HasPrefix(tail, "../")
}

func (p *Paths) containerDefaultCwd(ctx context.Context, taskID string) string {
	cfg, err := p.terminal.Settings().EnvConfig(ctx)
	if err != nil {
		return ""
	}
	cwd := cfg.Cwd
	if cwd == "~" {
		live, err := p.liveOrStartedEnv(ctx, taskID)
		if err != nil || live == nil {
			return ""
		}
		sandbox, ok := environmentPart[sandboxHomer](live)
		if !ok {
			return ""
		}
		cwd, ok = sandbox.SandboxHome()
		if !ok {
			return ""
		}
	}
	if strings.HasPrefix(cwd, "/") {
		return cwd
	}
	return ""
}

func (p *Paths) baseDirFor(ctx context.Context, taskID string, containerPaths bool) (string, error) {
	root, err := p.WorkspaceRoot(ctx, taskID)
	if err != nil {
		return "", err
	}
	if root == "" && containerPaths {
		root = p.containerDefaultCwd(ctx, taskID)
	}
	if root == "" {
		root, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	return p.anchor(p.hostText(ctx, root, containerPaths), os.Getwd, containerPaths)
}

func (p *Paths) BaseDir(ctx context.Context, taskID string) (string, error) {
	if p.EnvKind(ctx, taskID) == "ssh" {
		return p.resolveSSH(ctx, ".", taskID)
	}
	return p.baseDirFor(ctx, taskID, p.usesContainerPaths(ctx, taskID))
}

func (p *Paths) Resolve(ctx context.Context, path, taskID string) (string, error) {
	if p.EnvKind(ctx, taskID) == "ssh" {
		return p.resolveSSH(ctx, path, taskID)
	}
	containerPaths := p.usesContainerPaths(ctx, taskID)
	return p.anchor(p.hostText(ctx, path, containerPaths), func() (string, error) {
		return p.baseDirFor(ctx, taskID, containerPaths)
	}, containerPaths)
}

func (p *Paths) ResolveEntry(ctx context.Context, path, taskID string) (string, error) {
	parent, name := approval.SplitEntry(path)
	if name == "" || name == "." || name == ".." || (parent == "" && strings.HasPrefix(name, "~")) {
		return p.Resolve(ctx, path, taskID)
	}
	if parent == "" {
		parent = "."
	}
	dir, err := p.Resolve(ctx, parent, taskID)
	if err != nil {
		return "", err
	}
	sep := "/"
	if runtime.GOOS == "windows" && p.EnvKind(ctx, taskID) != "ssh" && !p.usesContainerPaths(ctx, taskID) {
		sep = `\`
	}
	if strings.HasSuffix(dir, sep) {
		return dir + name, nil
	}
	return dir + sep + name, nil
}

func underRoot(resolved, root string, windows bool) bool {
	if windows {
		return isUnder(resolved, root)
	}
	if resolved == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return strings.HasPrefix(resolved, prefix)
}

func (p *Paths) ResolutionWarning(ctx context.Context, path, resolved, taskID string) string {
	root := ""
	windows := false
	if p.EnvKind(ctx, taskID) == "ssh" {
		if strings.HasPrefix(path, "~") || strings.HasPrefix(path, "/") {
			return ""
		}
		anchor, err := p.resolveSSH(ctx, ".", taskID)
		if err != nil {
			return ""
		}
		root = anchor
	} else {
		if filepath.IsAbs(p.ExpandTilde(ctx, path)) {
			return ""
		}
		workspace, err := p.WorkspaceRoot(ctx, taskID)
		if err != nil || workspace == "" {
			return ""
		}
		if p.usesContainerPaths(ctx, taskID) {
			root = posixNorm(p.ExpandTilde(ctx, workspace))
		} else {
			root = realPath(p.ExpandTilde(ctx, workspace))
			windows = runtime.GOOS == "windows"
		}
	}
	if underRoot(resolved, root, windows) {
		return ""
	}
	return "Relative path " + quote(path) + " resolved to " + quote(resolved) + ", which is " +
		"OUTSIDE the active workspace (" + quote(root) + "). The edit will land in " +
		"a different directory than the terminal's cwd. If this is not " +
		"intended (e.g. a git-worktree session writing into the main " +
		"checkout), pass an absolute path under the workspace instead."
}
