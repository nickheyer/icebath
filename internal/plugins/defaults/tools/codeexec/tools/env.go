package tools

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	codeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/codeexec/config"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
)

const windows = runtime.GOOS == "windows"

var (
	// Variables a script keeps by the start of their name
	safePrefixes = []string{"PATH", "HOME", "USER", "LANG", "LC_", "TERM", "TMPDIR", "TMP", "TEMP", "SHELL", "LOGNAME", "XDG_", "PYTHONPATH",
		"VIRTUAL_ENV", "CONDA"}
	// Fragments of a name that mark a credential, which no script gets unless the profile passes it through
	secretFragments = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN", "WEBHOOK", "CREDS", "BEARER", "APIKEY",
		"COOKIE"}
	// Variables of icebath's own a script keeps
	icebathKept = []string{"ICEBATH_HOME", "ICEBATH_PROFILE", "ICEBATH_CONFIG", "ICEBATH_ENV", "ICEBATH_DELEGATED_CHILD_CONTEXT"}
	// Variables a Windows program can't start without
	windowsEssential = []string{"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "OS", "PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS",
		"PUBLIC", "ALLUSERSPROFILE", "PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432", "APPDATA", "LOCALAPPDATA", "USERPROFILE",
		"USERDOMAIN", "USERNAME", "HOMEDRIVE", "HOMEPATH", "COMPUTERNAME"}
)

func secret(name string) bool {
	upper := strings.ToUpper(name)
	return slices.ContainsFunc(secretFragments, func(fragment string) bool { return strings.Contains(upper, fragment) })
}

// Environment of a script run on this machine for the session: the session's own, cut to what a script needs, with the module's socket
// and token, UTF-8 Python and the staged module first on PYTHONPATH
func (r *registrant) childEnv(call *tools.Call, socket, token, dir string) map[string]string {
	cfg := call.Env.Config
	base := r.host.SessionEnv(cfg, call.SessionID)
	passed := r.host.Passthrough(cfg)
	toolchain := map[string]bool{}
	if client := call.Env.Client; client != nil && client.Local {
		for name := range client.Env {
			toolchain[name] = true
		}
	}
	env := map[string]string{}
	var dropped []string
	for name, value := range base {
		upper := strings.ToUpper(name)
		switch {
		case passed[name]:
			env[name] = value
		case secret(name):
		case slices.ContainsFunc(safePrefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) }) || toolchain[name] ||
			slices.Contains(icebathKept, name) || windows && slices.Contains(windowsEssential, upper):
			env[name] = value
		case strings.HasPrefix(name, "ICEBATH_"):
			dropped = append(dropped, name)
		}
	}
	if len(dropped) > 0 {
		slices.Sort(dropped)
		slog.Debug("execute_code: dropped non-allowlisted ICEBATH_* variables from the script's environment; pass one through the profile's "+
			"passthrough variables if a script needs it", "count", len(dropped), "names", strings.Join(dropped, ", "))
	}
	if key, ok := terminal.PathKey(env); ok {
		env[key] = r.packages.Path(env[key])
	}
	env["ICEBATH_RPC_SOCKET"], env["ICEBATH_RPC_TOKEN"] = socket, token
	if call.Env.Delegated != nil {
		env[tools.DelegatedMarker] = "1"
	}
	env["PYTHONDONTWRITEBYTECODE"], env["PYTHONIOENCODING"], env["PYTHONUTF8"] = "1", "utf-8", "1"
	if zone := timezone(cfg); zone != "" && !windows {
		env["TZ"] = zone
	}
	delete(env, "ICEBATH_TIMEZONE")
	env["PYTHONPATH"] = strings.Join(slices.DeleteFunc([]string{dir, env["PYTHONPATH"]}, func(s string) bool { return s == "" }),
		string(os.PathListSeparator))
	return env
}

// Time zone the profile names, empty for this machine's own or a name no zone has
func timezone(cfg *config.Config) string {
	zone, err := cfg.Zone()
	if err != nil {
		slog.Warn("Ignoring a timezone no zone is named for", "error", err)
	}
	if zone == nil {
		return ""
	}
	return zone.String()
}

// Mode of the profile, project for a value it does not know
func mode(cfg *config.Config) string {
	if m := strings.ToLower(strings.TrimSpace(cfg.GetString(codeconfig.Mode))); m == codeconfig.Strict {
		return m
	}
	return codeconfig.Project
}

// Interpreter scripts of the session run with: in project mode the active virtualenv's or conda's, else the first usable one on the
// session's PATH, else the package manager's own, installed when missing and the profile allows it
func (r *registrant) python(ctx context.Context, cfg *config.Config, env map[string]string) (string, error) {
	if mode(cfg) == codeconfig.Project {
		dir, names := "bin", []string{"python", "python3"}
		if windows {
			dir, names = "Scripts", []string{"python.exe", "python3.exe"}
		}
		for _, variable := range []string{"VIRTUAL_ENV", "CONDA_PREFIX"} {
			root := strings.TrimSpace(env[variable])
			if root == "" {
				continue
			}
			if found := slices.IndexFunc(names, func(name string) bool {
				candidate := filepath.Join(root, dir, name)
				return fsx.IsFile(candidate) && executable(candidate)
			}); found >= 0 {
				candidate := filepath.Join(root, dir, names[found])
				if r.packages.Usable(candidate, env) {
					return candidate, nil
				}
				slog.Info("execute_code: skipping interpreter (Python version < 3.8 or broken). Using the interpreter on PATH instead.",
					"var", variable, "python", candidate)
				break
			}
		}
	}
	return r.packages.Python(ctx, cfg, env, "execute_code runs Python scripts")
}

// Folder a script of the session runs in: the staging folder in strict mode; in project mode where the session's commands last ended,
// else the session's folder, else the terminal's configured one, else icebath's own
func (r *registrant) folder(call *tools.Call, staging string) string {
	cfg := call.Env.Config
	if mode(cfg) != codeconfig.Project {
		return staging
	}
	candidates := []string{r.host.Folder(cfg, call.SessionID), call.Env.Cwd}
	if configured := strings.TrimSpace(cfg.GetString(terminalconfig.Cwd)); !terminal.Placeholder(configured) {
		candidates = append(candidates, fsx.ExpandHome(configured))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	for _, candidate := range candidates {
		if candidate != "" && fsx.IsDir(candidate) {
			return candidate
		}
	}
	return staging
}
