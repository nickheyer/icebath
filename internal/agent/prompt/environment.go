package prompt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/env"
)

const wslEnvironmentHint = "You are running inside WSL (Windows Subsystem for Linux). The Windows host filesystem is mounted under /mnt/ — " +
	"/mnt/c/ is the C: drive, /mnt/d/ is D:, etc. The user's Windows files are typically at " +
	"/mnt/c/Users/<username>/Desktop/, Documents/, Downloads/, etc. When the user references Windows paths or desktop " +
	"files, translate to the /mnt/c/ equivalent. You can list /mnt/c/Users/ to discover the Windows username if needed."

var backendFallbackDescriptions = map[string]string{
	"docker":         "a Docker container (Linux)",
	"modal":          "a Modal sandbox (Linux)",
	"managed_modal":  "a managed Modal sandbox (Linux)",
	"daytona":        "a Daytona workspace (Linux)",
	"vercel_sandbox": "a Vercel sandbox (Linux)",
	"ssh":            "a remote host reached over SSH (likely Linux)",
}

const windowsBashShellHint = "Shell: on this Windows host your `terminal` tool runs commands through bash (git-bash / MSYS), NOT PowerShell or " +
	"cmd.exe. Use POSIX shell syntax (`ls`, `$HOME`, `&&`, `|`, single-quoted strings) inside terminal calls. " +
	"MSYS-style paths like `/c/Users/<user>/...` work alongside native `C:\\Users\\<user>\\...` paths. PowerShell " +
	"builtins (`Get-ChildItem`, `$env:FOO`, `Select-String`) will NOT work — use their POSIX equivalents (`ls`, " +
	"`$FOO`, `grep`). Path arguments for NATIVE Windows programs (git, rg, node, python, ...) are NOT translated: MSYS " +
	"path conversion is disabled here, so `git -C /c/Users/x` or `node /tmp/a.js` fails with 'cannot change to'/'not " +
	"found' even though `cd /c/Users/x` (a bash builtin) works. Pass `C:/Users/x`-style forward-slash native paths to " +
	"native tools, and prefer `$LOCALAPPDATA/Temp` (or `$TMPDIR`, which icebath points at its own scratch dir) for scratch files a native tool must read — never a bare `/tmp`. When " +
	"answering prompts in a pty background process, use process(submit) — never process(write) with a bare trailing " +
	"newline: Enter on a Windows PTY is a carriage return, and a lone `\\n" +
	"` is not delivered as a line terminator, so the child's prompt silently never returns. When a CLI offers a " +
	"non-interactive path (flags, `--with-token`, config files, an OAuth device flow polled with curl), prefer it over " +
	"driving prompts."

const backendProbeCommand = `printf 'os=%s\nkernel=%s\n' "$(uname -s 2>/dev/null || echo unknown)" "$(uname -r 2>/dev/null || echo unknown)"`

const backendProbeTimeout = 4 * time.Second

const (
	runtimeEnvironmentHeading = "# icebath runtime environment"
	runtimeEnvironmentEnd     = "<!-- End icebath runtime environment -->"
)

const envEnvironmentHint = "ICEBATH_ENVIRONMENT_HINT"

type BackendProber interface {
	ProbeBackend(ctx context.Context, kind, command string, timeout time.Duration) (tool.ExecResult, error)
}

type BotScreen interface {
	Screen(ctx context.Context) (display, holder string, err error)
}

func isSystemError(err error) bool {
	var pathErr *fs.PathError
	var callErr *os.SyscallError
	var linkErr *os.LinkError
	var errno syscall.Errno
	return errors.As(err, &pathErr) || errors.As(err, &callErr) || errors.As(err, &linkErr) || errors.As(err, &errno)
}

func (b *Builder) terminalBackend(ctx context.Context) (string, error) {
	v, err := b.env.Settings().TerminalEnv(ctx, "TERMINAL_ENV")
	if err != nil {
		return "", err
	}
	if v == "" {
		v = "local"
	}
	return strings.ToLower(trim(v)), nil
}

func formatBackendProbe(output string) string {
	parsed := map[string]string{}
	for _, line := range splitLines(output) {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		parsed[trim(k)] = trim(v)
	}
	var bits []string
	for _, key := range []string{"os", "kernel"} {
		if v := parsed[key]; v != "" && v != "unknown" {
			bits = append(bits, v)
		}
	}
	if len(bits) == 0 {
		return ""
	}
	return "  OS: " + strings.Join(bits, " ")
}

func (b *Builder) probeRemoteBackend(ctx context.Context, kind string) (string, error) {
	h, err := b.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	homeKey, err := b.homes.Key(h)
	if err != nil {
		return "", err
	}
	cwd, err := b.env.Settings().TerminalEnv(ctx, "TERMINAL_CWD")
	if err != nil {
		return "", err
	}
	key := [3]string{homeKey, kind, cwd}
	b.mu.Lock()
	formatted, ok := b.probes[key]
	b.mu.Unlock()
	if ok {
		return formatted, nil
	}
	if b.probe == nil {
		return "", &tool.UnfilledError{Name: "prompt.Options.Probe"}
	}
	res, err := b.probe.ProbeBackend(ctx, kind, backendProbeCommand, backendProbeTimeout)
	switch {
	case err != nil:
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		slog.Debug(fmt.Sprintf("Backend probe failed: %v", err))
	case res.ExitCode != 0:
		slog.Debug(fmt.Sprintf("Backend probe returned non-zero: %d", res.ExitCode))
	default:
		formatted = formatBackendProbe(trim(res.Output))
	}
	b.mu.Lock()
	b.probes[key] = formatted
	b.mu.Unlock()
	return formatted, nil
}

func (b *Builder) backendDescription(ctx context.Context, kind string) (string, error) {
	if d := backendFallbackDescriptions[kind]; d != "" {
		return d, nil
	}
	if b.providers == nil {
		return "", &tool.UnfilledError{Name: "prompt.Options.Providers"}
	}
	if p, ok, err := b.providers.Get(ctx, kind, ""); err == nil && ok {
		if d := p.EnvDescription(); d != "" {
			return d, nil
		}
	}
	return "a " + kind + " environment (likely Linux)", nil
}

func (b *Builder) remoteBackendHint(ctx context.Context, kind string) (string, error) {
	lead := "Terminal backend: " + kind + ". Your `terminal`, `read_file`, `write_file`, `patch`, and " +
		"`search_files` tools all operate inside "
	probe, err := b.probeRemoteBackend(ctx, kind)
	if err != nil {
		return "", err
	}
	if probe != "" {
		return lead + "this " + kind + " environment — NOT on the machine where icebath itself is running. The host OS, " +
			"home, and cwd of the icebath process are irrelevant; only the following backend state matters:\n" + probe + "\n" +
			"  The sandbox's current user, $HOME, and working directory are not listed here; if you need them, " +
			"probe directly with a terminal call like `whoami && pwd`.", nil
	}
	description, err := b.backendDescription(ctx, kind)
	if err != nil {
		return "", err
	}
	return lead + description + " — NOT on the machine where icebath itself runs. The backend probe didn't respond at " +
		"prompt-build time, so the sandbox's OS, current user, $HOME, and working directory are unknown from here. " +
		"If you need them, probe directly with a terminal call like `uname -a && whoami && pwd`.", nil
}

func (b *Builder) localHostHints(ctx context.Context) ([]string, error) {
	label, err := b.host.Label()
	if err != nil {
		return nil, err
	}
	lines := []string{"Host: " + label, "User home directory: " + home.ExpandUser("~")}
	cwd, err := env.ResolveAgentCwd(ctx, b.env.Settings())
	switch {
	case err == nil:
		lines = append(lines, "Current working directory: "+cwd)
	case !isSystemError(err):
		return nil, err
	}
	h, err := b.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	lines = append(lines, "Scratch directory: "+b.homes.ScratchDir(h)+" (TMPDIR points here; write temporary files "+
		"and probes there, never under the system temp dir; entries idle for 24h are pruned)")
	if runtime.GOOS != "windows" || b.host.WSL() {
		return []string{strings.Join(lines, "\n")}, nil
	}
	lines = append(lines, "Note: on Windows, the machine hostname (e.g. from `hostname` or uname) is NOT the username. "+
		"Use the 'User home directory' above to construct paths under C:\\Users\\<user>\\, never the hostname.")
	return []string{strings.Join(lines, "\n"), windowsBashShellHint}, nil
}

func BotScreenNote(running bool, display, holder string) string {
	if !running {
		return "Bot Screen: this profile's own headless desktop is no longer running. Do not refer to " +
			"'the bot screen' or route GUI launches at it; GUI apps from the terminal open on the " +
			"user's own display again."
	}
	if display == "" {
		return ""
	}
	held := ""
	switch holder {
	case "human":
		held = "a human holds it — do not drive the screen; ask them or wait"
	case "agent":
		held = "you hold it"
	}
	if held == "" {
		return ""
	}
	return "Bot Screen: this profile's own headless desktop is RUNNING on display " + display + " " +
		"(" + held + "). 'screen N' / ':N' / 'the bot screen' / 'your screen' means THIS screen: " +
		"GUI apps you launch from the terminal already open there (their DISPLAY is routed " +
		"to it), and display introspection (xrandr/xdotool/wmctrl) targets it. It is NOT " +
		"the user's own display."
}

func (b *Builder) botScreenHint(ctx context.Context) (string, error) {
	if b.screen == nil {
		return "", &tool.UnfilledError{Name: "prompt.Options.BotScreen"}
	}
	display, holder, err := b.screen.Screen(ctx)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		slog.Debug("bot screen state unavailable", "error", err)
		return "", nil
	}
	return BotScreenNote(true, display, holder), nil
}

func (b *Builder) embedderEnvironmentHint(ctx context.Context) string {
	if v := trim(os.Getenv(envEnvironmentHint)); v != "" {
		return v
	}
	cfg, err := b.cfg.LoadReadonly(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read agent.environment_hint from config: %v", err))
		return ""
	}
	return trim(cfg.Agent.EnvironmentHint)
}

func (b *Builder) environmentHints(ctx context.Context) (string, error) {
	backend, err := b.terminalBackend(ctx)
	if err != nil {
		return "", err
	}
	remote := b.env.Settings().IsRemoteBackend(ctx, backend)
	var hints []string
	if remote {
		hint, err := b.remoteBackendHint(ctx, backend)
		if err != nil {
			return "", err
		}
		hints = append(hints, hint)
	} else {
		hints, err = b.localHostHints(ctx)
		if err != nil {
			return "", err
		}
		screen, err := b.botScreenHint(ctx)
		if err != nil {
			return "", err
		}
		hints = append(hints, screen)
	}
	if b.host.WSL() {
		hints = append(hints, wslEnvironmentHint)
	}
	hints = append(hints, b.embedderEnvironmentHint(ctx))
	var kept []string
	for _, h := range hints {
		if h != "" {
			kept = append(kept, h)
		}
	}
	return strings.Join(kept, "\n\n"), nil
}
