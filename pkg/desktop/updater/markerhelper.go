package updater

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const markerHelperTimeout = 20 * time.Second

var (
	handoffProtocolRE = regexp.MustCompile(`(?m)^# hermes-handoff-protocol: (\d+)\s*$`)
	bareVerdicts      = map[string]bool{"absent": true, "reclaimed": true, "held": true, "busy": true, "withdrawn": true, "foreign": true}
	opVerdicts        = map[string]map[string]bool{
		"reclaim":  {"absent": true, "reclaimed": true, "held": true, "busy": true, "live": true},
		"withdraw": {"absent": true, "busy": true, "withdrawn": true, "taken": true, "foreign": true},
	}
)

// MarkerHelperVerdict is one answer of the hand-off script's marker helper, PID set for live and taken
type MarkerHelperVerdict struct {
	Kind string
	PID  int
}

// UpdateScriptHandoff is a repo owned hand-off script and the command that runs it
type UpdateScriptHandoff struct {
	Command    string
	Args       []string
	ScriptPath string
}

func stagedFileExists(candidate string) bool {
	info, err := os.Stat(candidate)
	return err == nil && info.Mode().IsRegular()
}

// Windows hand-off script of a checkout, nil elsewhere or when the checkout predates it
func resolveUpdateScriptHandoff(updateRoot string, isWindows bool) *UpdateScriptHandoff {
	if !isWindows {
		return nil
	}
	for _, candidate := range []string{filepath.Join(updateRoot, "scripts", "desktop-update", "windows.ps1"), filepath.Join(updateRoot, "scripts", "desktop-update.ps1")} {
		if stagedFileExists(candidate) {
			return &UpdateScriptHandoff{Command: "powershell", Args: []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", candidate}, ScriptPath: candidate}
		}
	}
	return nil
}

// POSIX hand-off script of a checkout, nil on Windows or when the checkout predates it
func resolvePosixScriptHandoff(updateRoot string, isWindows bool) *UpdateScriptHandoff {
	if isWindows {
		return nil
	}
	scriptPath := filepath.Join(updateRoot, "scripts", "desktop-update", "posix.sh")
	if !stagedFileExists(scriptPath) {
		return nil
	}
	return &UpdateScriptHandoff{Command: "/bin/bash", Args: []string{scriptPath}, ScriptPath: scriptPath}
}

// Hand-off script whose marker helper the desktop asks, empty when there is none
func handoffScriptPath(updateRoot string, isWindows bool) string {
	if isWindows {
		if handoff := resolveUpdateScriptHandoff(updateRoot, true); handoff != nil {
			return handoff.ScriptPath
		}
		return ""
	}
	posix := filepath.Join(updateRoot, "scripts", "desktop-update", "posix.sh")
	if _, err := os.Stat(posix); err == nil {
		return posix
	}
	return ""
}

// Hand-off protocol a script declares, one for none or a missing file, zero when it cannot be read
func readHandoffProtocol(scriptPath string) int {
	if scriptPath == "" {
		return 1
	}
	text, err := os.ReadFile(scriptPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 1
		}
		return 0
	}
	match := handoffProtocolRE.FindSubmatch(text)
	if match == nil {
		return 1
	}
	version, err := strconv.ParseUint(string(match[1]), 10, 53)
	if err != nil || version < 2 {
		return 1
	}
	return int(version)
}

// Parses the single verdict line the helper prints
func parseMarkerHelperVerdict(stdout string) MarkerHelperVerdict {
	var lines []string
	for _, line := range regexp.MustCompile(`\r?\n`).Split(stdout, -1) {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) != 1 {
		return MarkerHelperVerdict{Kind: "error"}
	}
	fields := regexp.MustCompile(`\s+`).Split(lines[0], -1)
	if len(fields) > 2 {
		return MarkerHelperVerdict{Kind: "error"}
	}
	word := fields[0]
	if len(fields) == 1 {
		if bareVerdicts[word] {
			return MarkerHelperVerdict{Kind: word}
		}
		return MarkerHelperVerdict{Kind: "error"}
	}
	if (word == "live" || word == "taken") && regexp.MustCompile(`^\d+$`).MatchString(fields[1]) {
		if pid, err := strconv.ParseUint(fields[1], 10, 53); err == nil && pid > 0 {
			return MarkerHelperVerdict{Kind: word, PID: int(pid)}
		}
	}
	return MarkerHelperVerdict{Kind: "error"}
}

// Command and arguments that run one marker operation of a hand-off script
func markerHelperCommand(op, scriptPath, updateRoot string, desktopPID int, runID string, isWindows bool) (string, []string) {
	if isWindows {
		args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-MarkerOp", op, "-InstallRoot", updateRoot}
		if desktopPID > 0 {
			args = append(args, "-DesktopPid", strconv.Itoa(desktopPID))
		}
		if runID != "" {
			args = append(args, "-HandoffRun", runID)
		}
		return "powershell.exe", args
	}
	args := []string{scriptPath, "--marker-op", op, "--install-root", updateRoot}
	if desktopPID > 0 {
		args = append(args, "--desktop-pid", strconv.Itoa(desktopPID))
	}
	if runID != "" {
		args = append(args, "--handoff-run", runID)
	}
	return "bash", args
}

func scriptLine(text string, isWindows bool) string {
	line, _, _ := strings.Cut(strings.TrimPrefix(text, "\ufeff"), "\n")
	if isWindows {
		return strings.TrimSpace(line)
	}
	return strings.Trim(strings.TrimSuffix(line, "\r"), " \t")
}

// Checkout lock the update scripts take, inside the common git dir for a checkout
func checkoutLockPath(updateRoot string, isWindows bool) (string, error) {
	dot := filepath.Join(updateRoot, ".git")
	plain := filepath.Join(updateRoot, updaterecord.CheckoutLockName)
	gitdir := dot
	info, err := os.Stat(dot)
	switch {
	case err == nil && info.Mode().IsRegular():
		raw, err := os.ReadFile(dot)
		if err != nil {
			return "", err
		}
		line := scriptLine(string(raw), isWindows)
		prefix := strings.HasPrefix(line, "gitdir:")
		if isWindows {
			prefix = len(line) >= 7 && strings.EqualFold(line[:7], "gitdir:")
		}
		if !prefix {
			return plain, nil
		}
		named := scriptLine(line[7:], isWindows)
		if isWindows && named == "" {
			return plain, nil
		}
		gitdir = resolveFrom(updateRoot, named)
	case err == nil && info.IsDir():
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", err
	default:
		return plain, nil
	}
	commondir := filepath.Join(gitdir, "commondir")
	if info, err := os.Stat(commondir); err == nil && info.Mode().IsRegular() {
		raw, err := os.ReadFile(commondir)
		if err != nil {
			return "", err
		}
		if common := scriptLine(string(raw), isWindows); common != "" {
			gitdir = resolveFrom(gitdir, common)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Join(gitdir, updaterecord.GitCheckoutLockName), nil
}

// Absolute path of target against base as path.resolve joins them
func resolveFrom(base, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Join(base, target)
}

// False only when the script would answer not held because no checkout lock file exists
func checkoutLockMayBeHeld(updateRoot string, isWindows bool) bool {
	lock, err := checkoutLockPath(updateRoot, isWindows)
	if err == nil {
		info, statErr := os.Stat(lock)
		if statErr == nil {
			return info.Mode().IsRegular()
		}
		err = statErr
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return !errors.Is(err, syscall.ENOTDIR)
}

// Exit code and stdout of one helper run, a missing code for a spawn failure or timeout
type helperSpawnResult struct {
	code   *int
	stdout string
}

// Runs a helper detached in its own group, killing the group at the timeout
var helperSpawn = func(command string, args []string, env []string, timeout time.Duration) helperSpawnResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.Command(command, args...)
	cmd.Env = env
	var stdout cappedBuffer
	stdout.limit = 64 * 1024
	cmd.Stdout = &stdout
	detachHelper(cmd)
	if err := cmd.Start(); err != nil {
		return helperSpawnResult{}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			return helperSpawnResult{stdout: stdout.String()}
		}
		code := cmd.ProcessState.ExitCode()
		return helperSpawnResult{code: &code, stdout: stdout.String()}
	case <-ctx.Done():
		killHelper(cmd)
		<-done
		return helperSpawnResult{stdout: stdout.String()}
	}
}

// Buffer keeping the first limit bytes it is handed in whole chunks
type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.Len() < c.limit {
		c.Buffer.Write(p)
	}
	return len(p), nil
}

// Asks the checkout's hand-off script one marker question, unsupported for a script before protocol 2
func runMarkerHelper(op, updateRoot, home string, desktopPID int, runID string, isWindows bool) MarkerHelperVerdict {
	scriptPath := handoffScriptPath(updateRoot, isWindows)
	protocol := readHandoffProtocol(scriptPath)
	if protocol == 0 {
		return MarkerHelperVerdict{Kind: "error"}
	}
	if scriptPath == "" || protocol < 2 {
		return MarkerHelperVerdict{Kind: "unsupported"}
	}
	command, args := markerHelperCommand(op, scriptPath, updateRoot, desktopPID, runID, isWindows)
	result := helperSpawn(command, args, append(os.Environ(), "ICEBATH_HOME="+home), markerHelperTimeout)
	verdict := MarkerHelperVerdict{Kind: "error"}
	if result.code != nil && *result.code == 0 {
		verdict = parseMarkerHelperVerdict(result.stdout)
	}
	if opVerdicts[op][verdict.Kind] || verdict.Kind == "error" {
		return verdict
	}
	return MarkerHelperVerdict{Kind: "error"}
}
