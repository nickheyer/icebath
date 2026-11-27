// Package git is icebath's own runs of git: environments that never prompt, page, open an editor, run hooks or filter drivers, or read
// the user's config beyond the directories it says are safe; the checkout a folder lies in; and the upkeep of a clone many processes
// share: what a killed git leaves behind, and maintenance one process at a time claims
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What one run of git gave: its exit status and its output, invalid text replaced
type Result struct {
	Code   int
	Stdout string
	Stderr string
}

// Run of git stopped once it outlasted its time
type TimeoutError struct {
	Args    []string
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("`git %s` did not finish within %s.", strings.Join(e.Args, " "), strs.Span(e.Timeout))
}

// Runs git in the folder with the environment, the process's own when nil, input on its stdin, stopped with every process it started
// once the timeout passes; an error when it could not start or timed out, a non zero exit being a Result
func Run(ctx context.Context, dir string, env map[string]string, timeout time.Duration, input []byte, args ...string) (*Result, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = Environ(env)
	}
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	group(cmd)
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, &TimeoutError{Args: args, Timeout: timeout}
	}
	result := &Result{Stdout: strings.ToValidUTF8(stdout.String(), "�"), Stderr: strings.ToValidUTF8(stderr.String(), "�")}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		result.Code = exit.ExitCode()
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Trimmed output of a run of git in the folder that succeeded, false for any failure
func Output(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, bool) {
	result, err := Run(ctx, dir, nil, timeout, nil, args...)
	if err != nil || result.Code != 0 {
		return "", false
	}
	return strings.TrimSpace(result.Stdout), true
}

// Lines of text with the empty ones left out, each trimmed
func Lines(text string) []string {
	var lines []string
	for line := range strings.Lines(text) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// Environment as a process takes it, sorted by name
func Environ(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, key := range slices.Sorted(maps.Keys(env)) {
		out = append(out, key+"="+env[key])
	}
	return out
}

// Environment of this process by name
func Process() map[string]string {
	env := map[string]string{}
	for _, variable := range os.Environ() {
		if key, value, ok := strings.Cut(variable, "="); ok && key != "" {
			env[key] = value
		}
	}
	return env
}

// Settings every run of git here is given in place of the user's
var overrides = [][2]string{
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

var (
	discoveryKeys = `^(filter\..*\.(clean|smudge|process)|include\.path|includeif\..*\.path)$`
	includeIfKey  = regexp.MustCompile(`(?i)^includeif\..*\.path$`)
	includeKey    = regexp.MustCompile(`(?i)^include(?:if\..*)?\.path$`)
	filterKey     = regexp.MustCompile(`(?i)^filter\..+\.(?:clean|smudge|process)$`)
)

const (
	// Included config files followed while looking for filter drivers, past which the repository is not trusted
	includeTargets = 16
	// Filter driver keys turned off, past which the repository is not trusted
	filterKeys    = 256
	configTimeout = 2 * time.Second
)

// Removes the ways an environment hands git settings of its own
func stripInjected(env map[string]string) {
	for key := range env {
		if key == "GIT_CONFIG_PARAMETERS" || key == "GIT_CONFIG_COUNT" || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			delete(env, key)
		}
	}
}

// Directories the user's own git config says are safe, in the order git reads them
func safeDirectories(ctx context.Context, base map[string]string) []string {
	env := maps.Clone(base)
	stripInjected(env)
	env["GIT_TERMINAL_PROMPT"] = "0"
	var dirs []string
	for _, scope := range []string{"--system", "--global"} {
		result, err := Run(ctx, "", env, 5*time.Second, nil, "config", scope, "-z", "--get-all", "safe.directory")
		if err != nil || result.Code != 0 {
			continue
		}
		dirs = append(dirs, strings.Split(strings.TrimSuffix(result.Stdout, "\x00"), "\x00")...)
	}
	return dirs
}

// Environment of a git run that never prompts or runs the user's hooks, pager or editor: the process environment, or base, with git's
// config replaced by the overrides and the user's safe directories
func Env(ctx context.Context, base map[string]string) map[string]string {
	if base == nil {
		base = Process()
	}
	env := maps.Clone(base)
	settings := slices.Clone(overrides)
	for _, dir := range safeDirectories(ctx, base) {
		settings = append(settings, [2]string{"safe.directory", dir})
	}
	stripInjected(env)
	maps.Copy(env, map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GCM_INTERACTIVE":     "Never",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_PAGER":           "cat",
		"PAGER":               "cat",
		"GIT_EDITOR":          "true",
		"GIT_CONFIG_COUNT":    strconv.Itoa(len(settings)),
	})
	for i, setting := range settings {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = setting[0]
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = setting[1]
	}
	return env
}

// Adds settings to an environment Env made, after those it holds
func Configure(env map[string]string, settings ...[2]string) {
	count, _ := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	for _, setting := range settings {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(count)], env["GIT_CONFIG_VALUE_"+strconv.Itoa(count)] = setting[0], setting[1]
		count++
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(count)
}

// Environment of a git run in one repository that also turns off every filter driver its config names, following its included files;
// nil when what it includes cannot be followed with certainty
func RepoEnv(ctx context.Context, dir string) map[string]string {
	env := Env(ctx, nil)
	listed, err := Run(ctx, "", env, configTimeout, nil, "-C", dir, "config", "--includes", "--show-origin", "-z", "--get-regexp", discoveryKeys)
	if err != nil || listed.Code != 0 && listed.Code != 1 {
		return nil
	}
	var names []string
	followed := map[string]bool{}
	toplevel := ""
	fields := strings.Split(listed.Stdout, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		origin, entry := fields[i], fields[i+1]
		key, value, _ := strings.Cut(entry, "\n")
		if !includeIfKey.MatchString(key) {
			names = append(names, key)
			continue
		}
		file, ok := strings.CutPrefix(origin, "file:")
		if !ok {
			return nil
		}
		if !filepath.IsAbs(file) {
			if env["GIT_DIR"] != "" || env["GIT_WORK_TREE"] != "" {
				return nil
			}
			if toplevel == "" {
				top, err := Run(ctx, "", env, configTimeout, nil, "-C", dir, "rev-parse", "--show-toplevel")
				if err != nil || top.Code != 0 {
					return nil
				}
				toplevel = strings.TrimRight(top.Stdout, "\r\n")
			}
			file = filepath.Join(toplevel, file)
		}
		target := filepath.Join(filepath.Dir(file), fsx.ExpandHome(value))
		if real, err := filepath.EvalSymlinks(target); err == nil {
			target = real
		}
		if abs, err := filepath.Abs(target); err == nil {
			target = abs
		}
		info, err := os.Stat(target)
		if followed[target] || err != nil {
			continue
		}
		if !info.Mode().IsRegular() || len(followed) >= includeTargets {
			return nil
		}
		followed[target] = true
		included, err := Run(ctx, "", env, configTimeout, nil, "config", "--file", target, "--name-only", "-z", "--get-regexp", discoveryKeys)
		if err != nil || included.Code != 0 && included.Code != 1 {
			return nil
		}
		found := strings.Split(included.Stdout, "\x00")
		if slices.ContainsFunc(found, includeKey.MatchString) {
			return nil
		}
		names = append(names, found...)
	}
	var off [][2]string
	var required []string
	seen := map[string]bool{}
	for _, name := range names {
		key := strings.TrimSpace(name)
		if key == "" || seen[key] || !filterKey.MatchString(key) {
			continue
		}
		seen[key] = true
		if off = append(off, [2]string{key, ""}); len(off) > filterKeys {
			return nil
		}
		need := key[:strings.LastIndex(key, ".")] + ".required"
		if !seen[need] {
			seen[need] = true
			required = append(required, need)
		}
	}
	for _, key := range required {
		off = append(off, [2]string{key, "false"})
	}
	Configure(env, off...)
	return env
}

// A drive path as Git Bash writes it on Windows: /c/x, /mnt/c/x or /cygdrive/c/x
var bashDrive = regexp.MustCompile(`^/(?:(?:cygdrive|mnt)/)?([a-zA-Z])/(.*)$`)

// Top of the checkout the folder lies in, in the form of this system, empty outside one
func Root(ctx context.Context, dir string) string {
	root, ok := Output(ctx, dir, 5*time.Second, "rev-parse", "--show-toplevel")
	if !ok {
		return ""
	}
	if runtime.GOOS == "windows" {
		if m := bashDrive.FindStringSubmatch(root); m != nil {
			root = strings.ToUpper(m[1]) + `:\` + strings.ReplaceAll(m[2], "/", `\`)
		}
	}
	return filepath.Clean(root)
}

// Folder holding the objects of the checkout the folder lies in, the one a worktree or submodule .git file leads to; empty outside one
func CommonDir(ctx context.Context, dir string) string {
	common, ok := Output(ctx, dir, 10*time.Second, "rev-parse", "--git-common-dir")
	if !ok || common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	if real, err := filepath.EvalSymlinks(common); err == nil {
		return real
	}
	return filepath.Clean(common)
}
