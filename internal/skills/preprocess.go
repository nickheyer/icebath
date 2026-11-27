package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/proc"
)

var (
	templateVariable = regexp.MustCompile(`\$\{((?:ICEBATH|HERMES)_(?:SKILL_DIR|SESSION_ID))\}`)
	inlineShell      = regexp.MustCompile("!`([^`\n]+)`")
)

// Most characters of one snippet's output put in place
const snippetOutput = 4000

// The skill's folder and the session id in place of their variables, a variable with no value left as it is
func substitute(content, dir, session string) string {
	return templateVariable.ReplaceAllStringFunc(content, func(m string) string {
		value := session
		if strings.HasSuffix(templateVariable.FindStringSubmatch(m)[1], "_SKILL_DIR") {
			value = dir
		}
		if value == "" {
			return m
		}
		return value
	})
}

// Applies the profile's skill variables and shell snippets settings to a skill's content as it loads
func (s *Scope) preprocess(ctx context.Context, content, dir string) string {
	if content == "" {
		return content
	}
	if s.Config.GetBool(skillsconfig.TemplateVars) {
		content = substitute(content, dir, s.Session)
	}
	if s.Config.GetBool(skillsconfig.InlineShell) && !s.community(dir) {
		timeout := s.Config.GetInt(skillsconfig.InlineShellTimeout)
		if timeout <= 0 {
			timeout = 10
		}
		content = s.expand(ctx, content, dir, timeout)
	}
	return content
}

// Every shell snippet replaced by its output, run in the skill's folder
func (s *Scope) expand(ctx context.Context, content, dir string, timeout int) string {
	if !strings.Contains(content, "!`") {
		return content
	}
	return inlineShell.ReplaceAllStringFunc(content, func(m string) string {
		command := strings.TrimSpace(inlineShell.FindStringSubmatch(m)[1])
		if command == "" {
			return ""
		}
		return s.snippet(ctx, command, dir, timeout)
	})
}

// Output of one snippet: its trimmed output, its errors when it printed nothing else, or a marker saying why there is none
func (s *Scope) snippet(ctx context.Context, command, dir string, timeout int) string {
	bash := "bash"
	if runtime.GOOS == "windows" {
		found, err := gitBash()
		if err != nil {
			return fmt.Sprintf("[inline-shell error: %v]", err)
		}
		bash = found
	}
	if dir != "" {
		if marker := badFolder(dir); marker != "" {
			return marker
		}
	}
	if _, err := exec.LookPath(bash); err != nil {
		return "[inline-shell error: bash not found]"
	}
	run, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(run, bash, "-c", command)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	variables := s.environment()
	cmd.Env = make([]string, 0, len(variables))
	for name, value := range variables {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	proc.HideWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(run.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("[inline-shell timeout after %ds: %s]", timeout, command)
	}
	if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
		return fmt.Sprintf("[inline-shell error: %v]", err)
	}
	output := strings.TrimRight(newlines(stdout.String()), "\n")
	if output == "" {
		output = strings.TrimRight(newlines(stderr.String()), "\n")
	}
	if code := proc.ExitCode(cmd.ProcessState); code != 0 && output == "" {
		return fmt.Sprintf("[inline-shell exit %d with no output: %s]", code, command)
	}
	return tools.Bound(output, snippetOutput)
}

// Variables a command run for a skill gets: those the session gives it, else the profile's environment
func (s *Scope) environment() map[string]string {
	if s.Env != nil {
		return s.Env()
	}
	return s.Config.Environ()
}

// Output as text with every line ending a newline
func newlines(output string) string {
	output = strings.ToValidUTF8(output, "\uFFFD")
	return strings.ReplaceAll(strings.ReplaceAll(output, "\r\n", "\n"), "\r", "\n")
}

// bash of Git for Windows
func gitBash() (string, error) {
	if path, err := exec.LookPath("bash"); err == nil {
		return path, nil
	}
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
		if base == "" {
			continue
		}
		for _, sub := range []string{`Git\bin\bash.exe`, `Programs\Git\bin\bash.exe`} {
			if p := filepath.Join(base, sub); fsx.IsFile(p) {
				return p, nil
			}
		}
	}
	return "", errors.New("bash not found; install Git for Windows")
}

// Whether a skill folder was installed from the hub at community trust, which is never trusted to run snippets; true when the lock of
// its skills folder can't be read
func (s *Scope) community(dir string) bool {
	if dir == "" {
		return false
	}
	target := resolve(dir)
	roots := []string{resolve(s.Dir())}
	for parent := filepath.Dir(target); ; parent = filepath.Dir(parent) {
		if filepath.Base(parent) == "skills" && fsx.IsFile(lockPath(parent)) && !slices.Contains(roots, resolve(parent)) {
			roots = append(roots, resolve(parent))
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	for _, root := range roots {
		if !within(target, root) || !fsx.IsFile(lockPath(root)) {
			continue
		}
		lock, err := ReadLock(root)
		if err != nil {
			return true
		}
		for _, entry := range lock.Installed {
			if entry.Trust == "community" && entry.InstallPath != "" && within(target, resolve(filepath.Join(root, filepath.FromSlash(entry.InstallPath)))) {
				return true
			}
		}
	}
	return false
}
