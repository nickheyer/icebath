package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

const workingDiffGitTimeout = 15 * time.Second

// Sanity cap so a node_modules explosion cannot hang the untracked diff
const maxUntrackedDiffFiles = 50

var workingDiffModeArgs = map[string][]string{
	"working": {"diff"},
	"staged":  {"diff", "--cached"},
	"all":     {"diff", "HEAD"},
}

// Accepted diff modes in their documented order
var WorkingDiffModes = []string{"working", "staged", "all"}

// Subcommands that accept the no driver diff flags, status and friends reject them
var diffRenderingSubcommands = setOf("diff", "show", "log", "blame")

// Global git options whose following token is a value, never the subcommand
var gitValueOpts = setOf("-C", "-c", "--git-dir", "--work-tree", "--namespace", "--exec-path")

// One collected working tree diff or the reason git could not produce it
type WorkingDiff struct {
	Success   bool
	Stat      string
	Diff      string
	Untracked []string
	Empty     bool
	Error     string
}

// Inserts the no driver diff flags right after a diff rendering subcommand, other argv unchanged
func hardenGitArgv(args []string) []string {
	out := append([]string{}, args...)
	for i := 0; i < len(out); {
		tok := out[i]
		if gitValueOpts[tok] {
			i += 2
			continue
		}
		if strings.HasPrefix(tok, "-") {
			i++
			continue
		}
		if diffRenderingSubcommands[tok] {
			hardened := append([]string{}, out[:i+1]...)
			hardened = append(hardened, NoDriverDiffFlags...)
			return append(hardened, out[i+1:]...)
		}
		return out
	}
	return out
}

// HardenGitArgv prefixes internal git arguments with the hook and fsmonitor disabling overrides
func HardenGitArgv(args []string) []string { return hardenGitArgv(args) }

// Executable path for a command searched along an explicit PATH value, empty when absent
func whichInPath(cmd, pathValue string) string {
	if strings.ContainsAny(cmd, `/`+string(os.PathSeparator)) {
		if p, err := exec.LookPath(cmd); err == nil {
			return p
		}
		return ""
	}
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			continue
		}
		if p, err := exec.LookPath(dir + string(os.PathSeparator) + cmd); err == nil {
			return p
		}
	}
	return ""
}

// Python OSError text with errno, capitalised strerror, and the quoted filename
func pyOSErrorText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		var errno syscall.Errno
		if errors.As(pe.Err, &errno) {
			return fmt.Sprintf("[Errno %d] %s: %s", int(errno), strerrorCapitalized(errno.Error()), pyReprStr(pe.Path))
		}
		return pe.Err.Error() + ": " + pyReprStr(pe.Path)
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("[Errno %d] %s", int(errno), strerrorCapitalized(errno.Error()))
	}
	return err.Error()
}

// Upper cases the first rune of a strerror message the way libc spells it
func strerrorCapitalized(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// Python list repr of an argv for subprocess error text
func pyArgvRepr(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = pyReprStr(a)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Subprocess TimeoutExpired equivalent for a hardened git child
type gitTimeoutError struct {
	argv    []string
	timeout time.Duration
}

func (e *gitTimeoutError) Error() string {
	return fmt.Sprintf("Command '%s' timed out after %s seconds", pyArgvRepr(e.argv), pyTimeoutSeconds(e.timeout.Seconds()))
}

// Python number text for a float that may be integral
func pyTimeoutSeconds(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return pyFloatRepr(f)
}

// Runs hardened git returning the exit code and lossy UTF-8 stdout, 127 when git is not on PATH
func workingDiffRun(args []string, cwd string, timeout time.Duration) (int, string, error) {
	env := noninteractiveGitEnv(environToMap(os.Environ()))
	command := whichInPath("git", env["PATH"])
	if command == "" {
		return 127, "", nil
	}
	argv := append([]string{command, "-c", "core.quotePath=false"}, hardenGitArgv(args)...)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = mapToEnviron(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	hideConsoleWindow(cmd)
	proc.Group(cmd)
	cmd.Cancel = func() error { return proc.Kill(cmd) }
	if err := cmd.Start(); err != nil {
		return 0, "", err
	}
	err := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return 0, "", &gitTimeoutError{argv: argv, timeout: timeout}
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return 0, "", err
		}
		code = ee.ExitCode()
	}
	return code, decodeUTF8Replace(stdout.Bytes()), nil
}

// Untracked paths honouring the standard excludes, empty when git refuses
func untrackedFiles(cwd string) ([]string, error) {
	code, out, err := workingDiffRun([]string{"ls-files", "--others", "--exclude-standard"}, cwd, workingDiffGitTimeout)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return []string{}, nil
	}
	files := []string{}
	for _, line := range pySplitLines(out) {
		if pyStrip(line) != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// Renders untracked files as new file diffs through git diff no index
func untrackedDiff(cwd string, files []string) string {
	var chunks []string
	limit := len(files)
	if limit > maxUntrackedDiffFiles {
		limit = maxUntrackedDiffFiles
	}
	for _, rel := range files[:limit] {
		_, out, err := workingDiffRun([]string{"diff", "--no-ext-diff", "--no-index", "--", os.DevNull, rel}, cwd, workingDiffGitTimeout)
		if err != nil {
			continue
		}
		if pyStrip(out) != "" {
			chunks = append(chunks, strings.TrimRight(out, "\n"))
		}
	}
	if len(files) > maxUntrackedDiffFiles {
		chunks = append(chunks, fmt.Sprintf("... (%d more untracked files not shown)", len(files)-maxUntrackedDiffFiles))
	}
	return strings.Join(chunks, "\n")
}

// Stat, diff and untracked files for working, staged or all modes, optionally limited to pathspecs
func CollectWorkingDiff(cwd, mode string, paths []string) WorkingDiff {
	if mode == "" {
		mode = "working"
	}
	base, ok := workingDiffModeArgs[mode]
	if !ok {
		return WorkingDiff{Error: fmt.Sprintf("Unknown mode '%s'. Use: %s", mode, strings.Join(WorkingDiffModes, ", "))}
	}
	code, _, err := workingDiffRun([]string{"rev-parse", "--is-inside-work-tree"}, cwd, 5*time.Second)
	if err != nil {
		return WorkingDiff{Error: "git failed: " + pyOSErrorText(err)}
	}
	if code == 127 {
		return WorkingDiff{Error: "git is not installed or not on PATH."}
	}
	if code != 0 {
		return WorkingDiff{Error: "Not a git repository."}
	}
	baseArgs := []string{"diff", "--no-ext-diff"}
	for _, a := range base {
		if a != "diff" {
			baseArgs = append(baseArgs, a)
		}
	}
	var pathspec []string
	if len(paths) > 0 {
		pathspec = append([]string{"--"}, paths...)
	}
	fail := func(err error) WorkingDiff {
		var te *gitTimeoutError
		if errors.As(err, &te) {
			return WorkingDiff{Error: "git diff timed out."}
		}
		return WorkingDiff{Error: "git failed: " + pyOSErrorText(err)}
	}
	statArgs := append(append(append([]string{}, baseArgs...), "--stat"), pathspec...)
	_, statOut, err := workingDiffRun(statArgs, cwd, workingDiffGitTimeout)
	if err != nil {
		return fail(err)
	}
	_, diffOut, err := workingDiffRun(append(append([]string{}, baseArgs...), pathspec...), cwd, workingDiffGitTimeout*2)
	if err != nil {
		return fail(err)
	}
	untracked := []string{}
	if (mode == "working" || mode == "all") && len(paths) == 0 {
		if untracked, err = untrackedFiles(cwd); err != nil {
			return fail(err)
		}
	}
	extra := ""
	if len(untracked) > 0 {
		extra = untrackedDiff(cwd, untracked)
	}
	stat, diff := pyStrip(statOut), pyStrip(diffOut)
	if extra != "" {
		diff = pyStrip(diff + "\n" + extra)
	}
	result := WorkingDiff{Success: true, Stat: stat, Diff: diff, Untracked: untracked}
	if stat == "" && diff == "" && len(untracked) == 0 {
		result.Empty = true
	}
	return result
}
