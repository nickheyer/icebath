package env

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
)

type TerminalSettings interface {
	TerminalEnv(ctx context.Context, name string) (string, error)
}

type sessionCwdKey struct{}

func WithSessionCwd(ctx context.Context, cwd string) context.Context {
	return context.WithValue(ctx, sessionCwdKey{}, strings.TrimSpace(cwd))
}

func sessionCwd(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(sessionCwdKey{}).(string)
	return v, ok
}

func ScopedSessionCwd(ctx context.Context) string {
	v, _ := sessionCwd(ctx)
	return strings.TrimSpace(v)
}

func ScopeTerminalCwd(ctx context.Context, ts TerminalSettings) (string, error) {
	if ts == nil {
		return "", &tool.UnfilledError{Name: "env.TerminalSettings"}
	}
	return ts.TerminalEnv(ctx, "TERMINAL_CWD")
}

func existingDir(raw, label string) (string, bool) {
	p := home.ExpandUser(raw)
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		return p, true
	}
	slog.Warn(label + " does not exist: " + raw)
	return "", false
}

func resolveConfiguredCwd(ctx context.Context, ts TerminalSettings, overrideFinal, includeSession bool) (string, bool, error) {
	if includeSession {
		if override := ScopedSessionCwd(ctx); override != "" {
			p, ok := existingDir(override, "configured working directory")
			if ok || overrideFinal {
				return p, ok, nil
			}
		}
	}
	raw, err := ScopeTerminalCwd(ctx, ts)
	if err != nil {
		return "", false, err
	}
	if raw = strings.TrimSpace(raw); raw == "" {
		return "", false, nil
	}
	p, ok := existingDir(raw, "TERMINAL_CWD")
	return p, ok, nil
}

func ResolveAgentCwd(ctx context.Context, ts TerminalSettings) (string, error) {
	p, ok, err := resolveConfiguredCwd(ctx, ts, false, true)
	if err != nil {
		return "", err
	}
	if ok {
		return p, nil
	}
	return os.Getwd()
}

func ResolveContextCwd(ctx context.Context, ts TerminalSettings, includeSession bool) (string, bool, error) {
	return resolveConfiguredCwd(ctx, ts, true, includeSession)
}

func IsInstallTree(p string) bool {
	root, err := home.InstallRoot()
	if err != nil {
		return false
	}
	resolved, err := resolveLoose(p)
	if err != nil {
		return false
	}
	if rootResolved, err := resolveLoose(root); err == nil {
		root = rootResolved
	}
	rel, err := filepath.Rel(root, resolved)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolveLoose(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	dir, base := filepath.Split(abs)
	if dir == abs || base == "" {
		return abs, nil
	}
	parent, err := resolveLoose(filepath.Clean(dir))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, base), nil
}
