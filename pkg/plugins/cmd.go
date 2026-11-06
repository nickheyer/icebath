package plugins

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	xterm "github.com/charmbracelet/x/term"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

const (
	defaultCloneTimeoutSeconds = 300
	maxCloneTimeoutSeconds     = 3600
	CloneTimeoutHint           = "On a slow connection, raise plugins.clone_timeout_seconds in config.yaml."
)

var urlSchemes = []string{"https://", "http://", "git@", "ssh://", "file://"}

// Console markup printer writing plain text, or ANSI styled text when the writer is a terminal
type Console struct {
	Out   io.Writer
	Color bool
}

// Console over a writer, styled only when the writer is a terminal file
func NewConsole(w io.Writer) *Console {
	f, ok := w.(*os.File)
	return &Console{Out: w, Color: ok && xterm.IsTerminal(f.Fd())}
}

// Prints one markup message followed by a newline
func (c *Console) Print(markup string) {
	fmt.Fprintln(c.Out, pyrich.Render(markup, c.Color))
}

// Process exit a plugin command asks for after printing its message
type CommandExit struct{ Code int }

func (e *CommandExit) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Prints the message and returns the exit 1 the command ends with
func fail(console *Console, message string) error {
	console.Print(message)
	return &CommandExit{Code: 1}
}

// Failure class of an operation error, other when the raise site named none
func (e *PluginOperationError) Class() string {
	if e.FailureClass == "" {
		return "other"
	}
	return e.FailureClass
}

func hasURLScheme(identifier string) bool {
	return slices.ContainsFunc(urlSchemes, func(p string) bool { return strings.HasPrefix(identifier, p) })
}

var gitExecutable = sync.OnceValue(func() string {
	if found, err := exec.LookPath("git"); err == nil {
		return found
	}
	var candidates []string
	if runtime.GOOS == "windows" {
		roots := []string{envOr("ProgramFiles", `C:\Program Files`), envOr("ProgramFiles(x86)", `C:\Program Files (x86)`)}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			roots = append(roots, filepath.Join(local, "Programs"))
		}
		for _, r := range roots {
			for _, sub := range []string{"cmd", "bin"} {
				candidates = append(candidates, filepath.Join(r, "Git", sub, "git.exe"))
			}
		}
	} else {
		candidates = []string{"/usr/bin/git", "/usr/local/bin/git", "/bin/git"}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
			return c
		}
	}
	return ""
})

// Git binary for plugin subprocesses, searched beyond PATH when PATH is minimal, resolved once
var resolveGitExecutable = func() string { return gitExecutable() }

func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// Config value at the key path, the default on a missing key or a config load failure
func configValue(h string, def any, keys ...string) any {
	cfg, err := loadConfig(h)
	if err != nil {
		return def
	}
	var cur any = cfg
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return def
		}
		if cur, ok = m[k]; !ok {
			return def
		}
	}
	return cur
}

// Clone and pinned fetch deadline in seconds from the profile config, defaulted and clamped
func cloneTimeoutSeconds(h string) int {
	value := configValue(h, defaultCloneTimeoutSeconds, "plugins", "clone_timeout_seconds")
	n, ok := pyIntValue(value)
	if !ok || n < 1 {
		logWarn(fmt.Sprintf("plugins.clone_timeout_seconds must be a positive integer; using %ds", defaultCloneTimeoutSeconds))
		return defaultCloneTimeoutSeconds
	}
	if n > maxCloneTimeoutSeconds {
		logWarn(fmt.Sprintf("plugins.clone_timeout_seconds exceeds %ds; clamping", maxCloneTimeoutSeconds))
		return maxCloneTimeoutSeconds
	}
	return int(n)
}

// Integer value of a decoded config scalar, refusing bools and floats like an isinstance int check
func pyIntValue(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case uint64:
		return int64(n), true
	}
	return 0, false
}

// List valued config key as a set, empty on a failure or a non list
func configNameSet(h string, keys ...string) map[string]bool {
	out := map[string]bool{}
	if list, ok := configValue(h, []any{}, keys...).([]any); ok {
		for _, v := range list {
			out[scalarString(v)] = true
		}
	}
	return out
}

// Plain words clone failure naming what to check, with the escaped git output last
func cloneFailureMessage(gitURL, gitError string) string {
	return fmt.Sprintf("Could not download the plugin from %s. Check the address (browse the catalog "+
		"with `icebath plugins search`), check your internet connection, or, if the repository "+
		"is private, sign in first with `gh auth login` (or set GITHUB_TOKEN in your .env).\n"+
		"Details: %s", gitURL, pyrich.Escape(strings.TrimSpace(gitError)))
}

// Safe target path of a plugin name inside the plugins dir, refusing traversal and the dir itself
func sanitizePluginName(name, dir string, allowSubdir bool) (string, error) {
	if allowSubdir && name != "" {
		name = strings.Trim(name, "/")
	}
	if name == "" {
		return "", fmt.Errorf("Plugin name must not be empty.")
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("Invalid plugin name '%s': must not reference the plugins directory itself.", name)
	}
	bad := []string{"/", `\`, ".."}
	if allowSubdir {
		bad = []string{`\`, ".."}
	}
	for _, b := range bad {
		if strings.Contains(name, b) {
			return "", fmt.Errorf("Invalid plugin name '%s': must not contain '%s'.", name, b)
		}
	}
	target := resolvePath(filepath.Join(dir, name))
	root := resolvePath(dir)
	if target == root {
		return "", fmt.Errorf("Invalid plugin name '%s': resolves to the plugins directory itself.", name)
	}
	if !strings.HasPrefix(target, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator)) {
		return "", fmt.Errorf("Invalid plugin name '%s': resolves outside the plugins directory.", name)
	}
	return target, nil
}

// Absolute path with symlinks resolved and missing components kept, as Path.resolve does
func resolvePath(p string) string {
	resolved, err := pypath.Realpath(p, pypath.Lax)
	if err != nil {
		abs, _ := filepath.Abs(p)
		return abs
	}
	return resolved
}
