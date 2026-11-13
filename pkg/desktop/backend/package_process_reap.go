package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

const windowsProcessListTimeout = 10 * time.Second

// RunningProcess is one process reduced to what the package reap decides on
type RunningProcess struct {
	PID         int
	ParentPID   *int
	Path        *string
	CommandLine *string
}

// ReapPackageDeps scope the reap to the artifact roots and carry the process table and kill
type ReapPackageDeps struct {
	InstallRoots  []string
	ListProcesses func() ([]RunningProcess, error)
	KillProcess   func(pid int) error
	SelfPID       int
	ExcludePIDs   []int
	IsWindows     bool
	Log           func(string)
}

// ReapOutcome counts what the package reap matched, killed and failed to kill
type ReapOutcome struct {
	Matched int
	Killed  []int
	Failed  []int
	Skipped bool
}

var sharedRuntimeImage = regexp.MustCompile(`^(?:pythonw?(?:[0-9.]+)?|node|icebath(?:-agent|-acp)?)(?:\.exe|\.cmd)?$`)

// Whether an image is a shared interpreter or entry point no desktop window owns
func isSharedRuntime(imagePath string) bool {
	image := imagePath
	if i := strings.LastIndexAny(image, `\/`); i >= 0 {
		image = image[i+1:]
	}
	return sharedRuntimeImage.MatchString(strings.ToLower(image))
}

// Every process with its image path, parent and command line from Win32_Process, bounded by a timeout
func ListWindowsProcesses(execFile func(file string, args []string, timeout time.Duration) (string, error)) ([]RunningProcess, error) {
	script := `$ErrorActionPreference = "SilentlyContinue"; ` +
		`Get-CimInstance Win32_Process | ForEach-Object { ` +
		`"{0}|{1}|{2}|{3}" -f $_.ProcessId, $_.ParentProcessId, $_.ExecutablePath, $_.CommandLine }`
	stdout, err := execFile("powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command", script}, windowsProcessListTimeout)
	if err != nil {
		return nil, err
	}
	var processes []RunningProcess
	for _, line := range regexp.MustCompile(`\r?\n`).Split(stdout, -1) {
		first := strings.Index(line, "|")
		second, third := -1, -1
		if first >= 0 {
			if i := strings.Index(line[first+1:], "|"); i >= 0 {
				second = first + 1 + i
			}
		}
		if second >= 0 {
			if i := strings.Index(line[second+1:], "|"); i >= 0 {
				third = second + 1 + i
			}
		}
		if first <= 0 || second < 0 || third < 0 {
			continue
		}
		pid, ok := leadingInt(line[:first])
		if !ok {
			continue
		}
		entry := RunningProcess{PID: pid}
		if parent, ok := leadingInt(line[first+1 : second]); ok {
			entry.ParentPID = &parent
		}
		if image := strings.TrimSpace(line[second+1 : third]); image != "" {
			entry.Path = &image
		}
		commandLine := line[third+1:]
		entry.CommandLine = &commandLine
		processes = append(processes, entry)
	}
	return processes, nil
}

// Runs a hidden command for its whole stdout within a timeout, the Windows process listing in production
var execFileText = func(file string, args []string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, file, args...)
	hideWindow(cmd)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("spawnSync %s ETIMEDOUT", file)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return "", fmt.Errorf("Command failed: %s %s\n%s", file, strings.Join(args, " "), stderr.String())
	}
	if err != nil {
		return "", errors.New(nodeErrorString(err))
	}
	return string(out), nil
}

// Force kills one pid as process.kill with SIGKILL does, failing with the Node kill error code
func killProcessNow(pid int) error {
	p, err := os.FindProcess(pid)
	if err == nil {
		err = p.Kill()
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrProcessDone) {
		return errors.New("kill ESRCH")
	}
	return errors.New("kill " + hardening.NodeCode(err))
}

// Whether an image path lies under one of the roots, case insensitively on a separator boundary
func IsUnderInstallRoot(imagePath string, roots []string) bool {
	if imagePath == "" {
		return false
	}
	normalize := func(value string) string {
		return strings.ToLower(strings.ReplaceAll(strings.TrimRight(value, `\/`), "/", `\`))
	}
	path := normalize(imagePath)
	for _, root := range roots {
		if root == "" {
			continue
		}
		normalizedRoot := normalize(root)
		if normalizedRoot == "" {
			continue
		}
		if path == normalizedRoot || strings.HasPrefix(path, normalizedRoot+`\`) {
			return true
		}
	}
	return false
}

// Pids the reap never touches, every shared runtime or unidentified process and all of its descendants
func ProtectedRuntimePIDs(running []RunningProcess) map[int]bool {
	children := map[int][]int{}
	for _, entry := range running {
		if entry.ParentPID != nil {
			children[*entry.ParentPID] = append(children[*entry.ParentPID], entry.PID)
		}
	}
	protected := map[int]bool{}
	var queue []int
	for _, entry := range running {
		if entry.Path == nil || entry.CommandLine == nil || strings.TrimSpace(*entry.CommandLine) == "" || isSharedRuntime(*entry.Path) {
			if !protected[entry.PID] {
				queue = append(queue, entry.PID)
			}
			protected[entry.PID] = true
		}
	}
	for len(queue) > 0 {
		parent := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, child := range children[parent] {
			if !protected[child] {
				protected[child] = true
				queue = append(queue, child)
			}
		}
	}
	return protected
}

// Kills package rooted tools outside protected runtime trees, logging failures instead of breaking quit
func ReapPackageRootedProcesses(d ReapPackageDeps) ReapOutcome {
	log := d.Log
	if log == nil {
		log = func(string) {}
	}
	var roots []string
	for _, root := range d.InstallRoots {
		if root != "" {
			roots = append(roots, root)
		}
	}
	if !d.IsWindows || len(roots) == 0 {
		return ReapOutcome{Killed: []int{}, Failed: []int{}, Skipped: true}
	}
	running, err := d.ListProcesses()
	if err != nil {
		log("[package-reap] process enumeration failed: " + err.Error())
		return ReapOutcome{Killed: []int{}, Failed: []int{}, Skipped: true}
	}
	excluded := map[int]bool{}
	for _, pid := range d.ExcludePIDs {
		excluded[pid] = true
	}
	protected := ProtectedRuntimePIDs(running)
	out := ReapOutcome{Killed: []int{}, Failed: []int{}}
	for _, candidate := range running {
		if candidate.PID == d.SelfPID || excluded[candidate.PID] || protected[candidate.PID] {
			continue
		}
		if candidate.CommandLine == nil || strings.TrimSpace(*candidate.CommandLine) == "" || candidate.Path == nil {
			continue
		}
		if !IsUnderInstallRoot(*candidate.Path, roots) {
			continue
		}
		out.Matched++
		if err := d.KillProcess(candidate.PID); err != nil {
			out.Failed = append(out.Failed, candidate.PID)
			log(fmt.Sprintf("[package-reap] kill pid=%d failed: %s", candidate.PID, err.Error()))
			continue
		}
		out.Killed = append(out.Killed, candidate.PID)
	}
	if out.Matched > 0 {
		log(fmt.Sprintf("[package-reap] matched=%d killed=%d failed=%d", out.Matched, len(out.Killed), len(out.Failed)))
	}
	return out
}

// Sweeps detached tools rooted in a bundled artifact on Windows after the backend teardown
func (s *Service) reapInstallRootedStragglers(excludePIDs []int) {
	if runtime.GOOS != "windows" {
		return
	}
	ReapPackageRootedProcesses(ReapPackageDeps{
		InstallRoots:  []string{s.opts.PackageRoot},
		ListProcesses: func() ([]RunningProcess, error) { return ListWindowsProcesses(execFileText) },
		KillProcess:   killProcessNow,
		SelfPID:       os.Getpid(),
		ExcludePIDs:   excludePIDs,
		IsWindows:     true,
		Log:           s.Log,
	})
}
