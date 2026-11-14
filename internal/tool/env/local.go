package env

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"time"

	"github.com/nickheyer/icebath/internal/tool"
)

const defaultLocalTimeout = 60 * time.Second

type LocalOptions struct {
	Cwd        string
	Timeout    time.Duration
	Persistent bool
	Prepare    CommandPreparer
	Foreground *Foreground
	Limits     tool.OutputLimits
}

type Local struct {
	h       *Host
	tempDir string
	persist bool
}

func (h *Host) OpenLocal(ctx context.Context, o LocalOptions) (*Shell, error) {
	cwd, err := resolveLocalInitialCwd(o.Cwd)
	if err != nil {
		return nil, err
	}
	tempDir, err := h.localTempDir(ctx)
	if err != nil {
		return nil, err
	}
	homeDir, err := h.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultLocalTimeout
	}
	l := &Local{h: h, tempDir: tempDir, persist: o.Persistent}
	s, err := NewShell(l, ShellOptions{
		Cwd: cwd, Timeout: timeout, Prepare: o.Prepare, Foreground: o.Foreground,
		Passthrough: h.passthrough, Secrets: h.secrets, Homes: h.homes, Home: homeDir, Limits: o.Limits,
	})
	if err != nil {
		return nil, err
	}
	s.InitSession(ctx)
	return s, nil
}

func (l *Local) Kind() string { return "local" }

func (l *Local) Traits() Traits {
	return Traits{
		Local:                    true,
		HostFilesystem:           true,
		StdinMode:                StdinPipe,
		SnapshotTimeout:          defaultSnapshotTimeout,
		PersistEnv:               l.persist,
		PasswordlessSudoProbe:    true,
		ProfileScopedPassthrough: true,
		TempDir:                  l.tempDir,
	}
}

func (l *Local) RunBash(ctx context.Context, script string, o RunOptions) (tool.Process, error) {
	bash, err := l.h.FindBash(ctx)
	if err != nil {
		return nil, err
	}
	runEnv, err := l.h.RunEnv(ctx, nil)
	if err != nil {
		return nil, err
	}
	if o.Login {
		script = DroppedPathPrelude(script, runEnv["PATH"])
		script = prependShellInit(script, l.h.shellInitFiles(ctx))
	}
	args := []string{}
	if o.Login {
		args = append(args, "-l")
	}
	args = append(args, "-c", script)
	cmd := exec.Command(bash, args...)
	cmd.Env = EnvList(runEnv)
	cmd.Dir = o.Cwd
	return startCommand(cmd, o.Stdin, true, l.h.guard)
}

func (l *Local) Close(context.Context, bool) error { return nil }

func (l *Local) RecoverCwd(cwd string) string {
	safe := SafeCwd(cwd)
	if safe == cwd {
		return cwd
	}
	if safe != MsysToWindowsPath(cwd) {
		slog.Warn(fmt.Sprintf("Local environment cwd %s is missing on disk; falling back to %s so terminal commands keep working.",
			quotedPath(cwd), quotedPath(safe)))
	}
	return safe
}

func (l *Local) ObservedCwd(previous, observed string) (string, bool) {
	if observed == previous {
		return observed, true
	}
	normalized := MsysToWindowsPath(observed)
	if normalized != "" && isDir(normalized) {
		return normalized, true
	}
	return previous, false
}

func (l *Local) QuoteShellPath(p string) string { return quoteBashPath(p) }

func (l *Local) QuoteCwd(cwd string) string { return quoteCwdForCd(windowsToMsysPath(cwd)) }

func (l *Local) ScopedPassthroughNames(context.Context) []string {
	var out []string
	for name := range Environ() {
		if FirstPartyPrefix(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
