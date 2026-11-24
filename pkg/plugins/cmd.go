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
	"time"

	xterm "github.com/charmbracelet/x/term"
	"github.com/nickheyer/icebath/pkg/settings"
	consoleutil "github.com/nickheyer/icebath/pkg/utils/console"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Advice a clone timeout ends with
const CloneTimeoutHint = "On a slow connection, raise plugins.clone_timeout_seconds in config.yaml."

var urlSchemes = []string{"https://", "http://", "git@", "ssh://", "file://"}

// Console markup printer writing plain text, or ANSI styled text when the writer is a terminal
type Console struct {
	Out   io.Writer
	Color bool
	// Width in cells panels fit within, zero reading the terminal size the way rich does
	Width int
}

func (c *Console) width() int {
	if c.Width > 0 {
		return c.Width
	}
	return consoleutil.DetectWidth()
}

// Console over a writer, styled only when the writer is a terminal file
func NewConsole(w io.Writer) *Console {
	f, ok := w.(*os.File)
	return &Console{Out: w, Color: ok && xterm.IsTerminal(f.Fd())}
}

// Prints one markup message with repr highlighting on unwrapped lines so every command in it stays copyable
func (c *Console) Print(markup string) {
	(&consoleutil.Console{Out: c.Out, Color: c.Color, Width: c.width()}).PrintSoft(markup)
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

// Clone and pinned fetch deadline from plugins.clone_timeout_seconds of the home's profile
func cloneTimeout(h string) time.Duration {
	return settings.PluginsCloneTimeoutSeconds.Get(homeContext(h))
}

// Plain words clone failure naming what to check, with the escaped git output last
func cloneFailureMessage(gitURL, gitError string) string {
	return fmt.Sprintf("Could not download the plugin from %s. Check the address (browse the catalog "+
		"with `icebath plugins search`), check your internet connection, or, if the repository "+
		"is private, sign in first with `gh auth login` (or set GITHUB_TOKEN in your .env).\n"+
		"Details: %s", gitURL, consoleutil.Escape(strings.TrimSpace(gitError)))
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
	resolved, err := pathutil.Resolve(p)
	if err != nil {
		abs, _ := filepath.Abs(p)
		return abs
	}
	return resolved
}
