package prompt

import (
	"context"
	"os"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/internal/botdesktop"
	terminalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/config"
	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/terminal"
	hostconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Heading and end marker of the runtime environment block
const (
	heading = "# Icebath runtime environment"
	end     = "<!-- End Icebath runtime environment -->"
)

const wslHint = "You are running inside WSL (Windows Subsystem for Linux). The Windows host filesystem is mounted under /mnt/ — /mnt/c/ is the C: " +
	"drive, /mnt/d/ is D:, etc. The user's Windows files are typically at /mnt/c/Users/<username>/Desktop/, Documents/, Downloads/, etc. When " +
	"the user references Windows paths or desktop files, translate to the /mnt/c/ equivalent. You can list /mnt/c/Users/ to discover the " +
	"Windows username if needed."

const windowsShellHint = "Shell: on this Windows host your `terminal` tool runs commands through bash (git-bash / MSYS), NOT PowerShell or " +
	"cmd.exe. Use POSIX shell syntax (`ls`, `$HOME`, `&&`, `|`, single-quoted strings) inside terminal calls. MSYS-style paths like " +
	"`/c/Users/<user>/...` work alongside native `C:\\Users\\<user>\\...` paths. PowerShell builtins (`Get-ChildItem`, `$env:FOO`, " +
	"`Select-String`) will NOT work — use their POSIX equivalents (`ls`, `$FOO`, `grep`). Path arguments for NATIVE Windows programs (git, rg, " +
	"node, python, ...) are NOT translated: MSYS path conversion is disabled here, so `git -C /c/Users/x` or `node /tmp/a.js` fails with " +
	"'cannot change to'/'not found' even though `cd /c/Users/x` (a bash builtin) works. Pass `C:/Users/x`-style forward-slash native paths to " +
	"native tools, and prefer `$LOCALAPPDATA/Temp` (or `$TMPDIR`, which Icebath points at its own scratch dir) for scratch files a native tool " +
	"must read — never a bare `/tmp`. When answering prompts in a pty background process, use process_manage(submit) — never " +
	"process_manage(write) with a bare trailing newline: Enter on a Windows PTY is a carriage return, and a lone `\\n` is not delivered as a " +
	"line terminator, so the child's prompt silently never returns. When a CLI offers a non-interactive path (flags, `--with-token`, config " +
	"files, an OAuth device flow polled with curl), prefer it over driving prompts."

// Says where the agent's commands run: the host's facts for a backend on this machine, only the backend's state for a remote one
type registrant struct {
	host *terminal.Terminal
	// The profile's own screen, which a GUI app started from the terminal opens on while it runs
	desktop *botdesktop.Desktop
}

func Register(registry *prompt.Registry, host *terminal.Terminal, desktop *botdesktop.Desktop) {
	registry.Register(registrant{host: host, desktop: desktop})
}

func (r registrant) Sections(ctx context.Context, env *prompt.Env) ([]prompt.Section, error) {
	backend, err := r.host.Configured(env.Config)
	if err != nil {
		backend, _ = r.host.Backend("local")
	}
	var hints []string
	if backend.Remote {
		hints = append(hints, r.remote(ctx, env, backend))
	} else {
		hints = append(hints, local(env)...)
		if hint := r.desktop.Hint(ctx, env.Config); hint != "" {
			hints = append(hints, hint)
		}
	}
	if wsl() {
		hints = append(hints, wslHint)
	}
	if note := strings.TrimSpace(env.Config.GetString(terminalconfig.EnvironmentHint)); note != "" {
		hints = append(hints, note)
	}
	text := strings.ReplaceAll(strings.Join(hints, "\n\n"), heading, "> "+heading)
	return []prompt.Section{{Tier: prompt.Turn, Text: heading + "\n\n" + text + "\n\n" + end}}, nil
}

// What the agent is told of a remote backend: what its probe found, else what the backend says it is
func (r registrant) remote(ctx context.Context, env *prompt.Env, backend terminal.Backend) string {
	lead := "Terminal backend: " + backend.Name + ". Your `terminal`, `read_file`, `write_file`, `patch`, and `search_files` tools all operate inside "
	if probed := r.host.Probe(ctx, env.Config, backend); probed != "" {
		return lead + "this " + backend.Name + " environment — NOT on the machine where Icebath itself is running. The host OS, home, and cwd of the " +
			"Icebath process are irrelevant; only the following backend state matters:\n" + probed + "\n  The sandbox's current user, $HOME, and " +
			"working directory are not listed here; if you need them, probe directly with a terminal call like `whoami && pwd`."
	}
	where := backend.Where
	if where == "" {
		where = "a " + backend.Name + " environment (likely Linux)"
	}
	return lead + where + " — NOT on the machine where Icebath itself runs. The backend probe didn't respond at prompt-build time, so the sandbox's " +
		"OS, current user, $HOME, and working directory are unknown from here. If you need them, probe directly with a terminal call like " +
		"`uname -a && whoami && pwd`."
}

// Host, home, working folder and scratch folder of this machine, with the shell notes of native Windows
func local(env *prompt.Env) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "~"
	}
	isWSL := wsl()
	lines := []string{"Host: " + hostLabel(isWSL), "User home directory: " + home, "Current working directory: " + cwd(env)}
	if scratch := scratch(env); scratch != "" {
		lines = append(lines, "Scratch directory: "+scratch+" (TMPDIR points here; write temporary files and probes there, never under the system "+
			"temp dir; entries idle for 24h are pruned)")
	}
	if runtime.GOOS != "windows" || isWSL {
		return []string{strings.Join(lines, "\n")}
	}
	lines = append(lines, "Note: on Windows, the machine hostname (e.g. from `hostname` or uname) is NOT the username. Use the 'User home "+
		"directory' above to construct paths under C:\\Users\\<user>\\, never the hostname.")
	return []string{strings.Join(lines, "\n"), windowsShellHint}
}

// Working folder setting when it names a folder that exists, else the session's folder
func cwd(env *prompt.Env) string {
	configured := strings.TrimSpace(env.Config.GetString(hostconfig.Cwd))
	if !terminal.Placeholder(configured) {
		path := fsx.ExpandHome(configured)
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			return path
		}
	}
	if env.Cwd != "" {
		return env.Cwd
	}
	dir, _ := os.Getwd()
	return dir
}

// Profile's scratch folder when the commands' temp variables point there, empty when the user pointed them elsewhere
func scratch(env *prompt.Env) string {
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if os.Getenv(name) != "" && os.Getenv(name) != os.Getenv("ICEBATH_SCRATCH_DIR") {
			return ""
		}
	}
	return terminal.Scratch(env.Config.ProfileDir(env.Config.Profile()))
}

// Whether icebath runs inside WSL
func wsl() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	data, err := os.ReadFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft")
}
