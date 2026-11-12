package whatsapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

// SIGTERM grace before SIGKILL and the wait for a stopped bridge to release its port
var (
	sigtermGrace          = 5 * time.Second
	bridgePortFreeTimeout = 10 * time.Second
)

var (
	isWindows              = runtime.GOOS == "windows"
	runCommand             = runText
	listenerPidsOnPort     = listenerPidsOnPortLive
	pidLooksLikeNodeBridge = pidLooksLikeNodeBridgeLive
	signalPid              = signalPidLive
	ssPidPattern           = regexp.MustCompile(`pid=(\d+)`)
	procNetTCPRows         = []string{"/proc/net/tcp", "/proc/net/tcp6"}
)

// Stdout of a short command with stdin closed, exec.ErrNotFound when the binary is missing
func runText(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	err := cmd.Run()
	if errors.Is(err, exec.ErrNotFound) {
		return "", exec.ErrNotFound
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("Command '%s' timed out after %v seconds", strings.Join(append([]string{name}, args...), " "), timeout.Seconds())
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return "", err
	}
	return strings.ToValidUTF8(out.String(), "�"), nil
}

// Integers of the tokens that parse, the rest dropped
func safeInts(tokens []string) []int {
	out := []int{}
	for _, tok := range tokens {
		if n, err := strconv.Atoi(pystr.Strip(tok)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// PIDs listening on the port on POSIX, never clients, by lsof then ss then the kernel socket tables
func listenerPidsOnPortLive(port int) ([]int, error) {
	pids := []int{}
	out, err := runCommand(5*time.Second, "lsof", "-ti", fmt.Sprintf("tcp:%d", port), "-sTCP:LISTEN")
	if err != nil && !errors.Is(err, exec.ErrNotFound) {
		return nil, err
	}
	if err == nil {
		if stripped := pystr.Strip(out); stripped != "" {
			pids = safeInts(pystr.Splitlines(stripped, false))
		}
		if len(pids) > 0 {
			return pids, nil
		}
	}
	out, err = runCommand(5*time.Second, "ss", "-ltnHp", fmt.Sprintf("sport = :%d", port))
	if err != nil && !errors.Is(err, exec.ErrNotFound) {
		return nil, err
	}
	if err == nil {
		for _, m := range ssPidPattern.FindAllStringSubmatch(out, -1) {
			n, _ := strconv.Atoi(m[1])
			pids = append(pids, n)
		}
	}
	if len(pids) > 0 {
		return pids, nil
	}
	if found, err := kernelListenerPids(port); err == nil {
		pids = found
	}
	return pids, nil
}

// Listening PIDs on the port read from the kernel TCP tables and each process's socket inodes
func kernelListenerPids(port int) ([]int, error) {
	inodes := map[string]bool{}
	for _, table := range procNetTCPRows {
		data, err := os.ReadFile(table)
		if err != nil {
			if strings.HasSuffix(table, "6") && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			_, hexPort, ok := strings.Cut(fields[1], ":")
			if p, err := strconv.ParseInt(hexPort, 16, 32); ok && err == nil && int(p) == port && fields[9] != "0" {
				inodes[fields[9]] = true
			}
		}
	}
	pids := []int{}
	if len(inodes) == 0 {
		return pids, nil
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fdDir := filepath.Join("/proc", p.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err == nil && strings.HasPrefix(target, "socket:[") && inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}

// PIDs in LISTENING state on the port from netstat on Windows
func windowsListenerPids(port int) ([]int, error) {
	out, err := runCommand(5*time.Second, "netstat", "-ano", "-p", "TCP")
	if err != nil {
		return nil, err
	}
	tokens := []string{}
	suffix := fmt.Sprintf(":%d", port)
	for _, line := range pystr.Splitlines(out, false) {
		p := strings.Fields(line)
		if len(p) >= 5 && p[3] == "LISTENING" && strings.HasSuffix(p[1], suffix) {
			tokens = append(tokens, p[4])
		}
	}
	return safeInts(tokens), nil
}

// Listening PIDs on the port, netstat on Windows and lsof or ss elsewhere
func portListenerPids(port int) ([]int, error) {
	if isWindows {
		return windowsListenerPids(port)
	}
	return listenerPidsOnPort(port)
}

// Executable name of a live process, false when it cannot be read
func processName(pid int) (string, bool) {
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		name := strings.TrimRight(string(raw), "\n")
		if len(name) >= 15 {
			if argv, err := tools.ProcessCmdline(pid); err == nil && len(argv) > 0 {
				if base := filepath.Base(argv[0]); strings.HasPrefix(base, name) {
					name = base
				}
			}
		}
		return name, true
	}
	if isWindows {
		out, err := runCommand(5*time.Second, "tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
		if err != nil {
			return "", false
		}
		first, _, _ := strings.Cut(pystr.Strip(out), ",")
		name := strings.Trim(first, `"`)
		return name, name != "" && !strings.HasPrefix(name, "INFO:")
	}
	out, err := runCommand(5*time.Second, "ps", "-o", "comm=", "-p", strconv.Itoa(pid))
	if err != nil || pystr.Strip(out) == "" {
		return "", false
	}
	return filepath.Base(pystr.Strip(out)), true
}

// Fail closed check that the live process is a node executable before any kill
func pidLooksLikeNodeBridgeLive(pid int) bool {
	name, ok := processName(pid)
	if !ok {
		return false
	}
	if strings.Contains(pystr.Lower(name), "node") {
		return true
	}
	argv, err := tools.ProcessCmdline(pid)
	if err != nil {
		return false
	}
	first, _, _ := strings.Cut(pystr.Lower(strings.Join(argv, " ")), " ")
	return strings.Contains(first, "node")
}

// The pids still running, zombies counted gone
func pidsAlive(pids []int) []int {
	out := []int{}
	for _, pid := range pids {
		if gateway.PidExists(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// Polls until every pid exits or the timeout passes and returns the survivors
func waitPidsExit(pids []int, timeout time.Duration) []int {
	deadline := time.Now().Add(timeout)
	alive := pidsAlive(pids)
	for len(alive) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		alive = pidsAlive(pids)
	}
	return alive
}

// Sends a signal to a pid, the OSError of os.kill on failure
func signalPidLive(pid int, sig syscall.Signal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(sig)
}

// Kills node bridges listening on the port, SIGTERM then SIGKILL past the grace on POSIX and taskkill on Windows
func killPortProcess(port int) {
	signalled := []int{}
	func() {
		pids, err := portListenerPids(port)
		if err != nil {
			return
		}
		for _, pid := range pids {
			if pid <= 0 || !pidLooksLikeNodeBridge(pid) {
				slog.Warn(fmt.Sprintf("[whatsapp] Not killing PID %d on port %d: process is not a node bridge (or identity unverifiable)", pid, port))
				continue
			}
			if isWindows {
				if _, err := runCommand(5*time.Second, "taskkill", "/PID", strconv.Itoa(pid), "/F"); errors.Is(err, exec.ErrNotFound) {
					return
				}
				continue
			}
			if signalPid(pid, syscall.SIGTERM) == nil {
				signalled = append(signalled, pid)
			}
		}
	}()
	if len(signalled) == 0 {
		return
	}
	survivors := waitPidsExit(signalled, sigtermGrace)
	for _, pid := range survivors {
		if !pidLooksLikeNodeBridge(pid) {
			slog.Warn(fmt.Sprintf("[whatsapp] Not SIGKILLing PID %d on port %d: identity no longer verifiable", pid, port))
			continue
		}
		if signalPid(pid, syscall.SIGKILL) == nil {
			slog.Warn(fmt.Sprintf("[whatsapp] SIGKILLed node bridge PID %d on port %d after it ignored SIGTERM", pid, port))
		}
	}
	waitPidsExit(survivors, time.Second)
}

// Whether the pid is alive and still the recorded bridge host by kernel start time, false without a fingerprint
func bridgePidIsOurs(pid int, expectedStart *int64) bool {
	if !gateway.PidExists(pid) || expectedStart == nil {
		return false
	}
	switch v := gateway.GetProcessStartTime(pid).(type) {
	case int64:
		return v == *expectedStart
	case int:
		return int64(v) == *expectedStart
	case float64:
		return v == float64(*expectedStart)
	}
	return false
}

// Removes a file, errors ignored
func unlinkQuietly(path string) { _ = os.Remove(path) }

// Pid, kernel start time and port from bridge.pid, all nil when absent or unparseable
func readBridgePidfile(sessionPath string) (*int, *int64, *int) {
	raw, err := os.ReadFile(filepath.Join(sessionPath, "bridge.pid"))
	if err != nil {
		return nil, nil, nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, nil, nil
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = pystr.Strip(lines[i])
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil {
		return nil, nil, nil
	}
	var start *int64
	if len(lines) > 1 && lines[1] != "" {
		n, err := strconv.ParseInt(lines[1], 10, 64)
		if err != nil {
			return nil, nil, nil
		}
		start = &n
	}
	var port *int
	if len(lines) > 2 && lines[2] != "" {
		n, err := strconv.Atoi(lines[2])
		if err != nil {
			return nil, nil, nil
		}
		port = &n
	}
	return &pid, start, port
}

// Stops the stale bridge bridge.pid records after the identity check, then removes the pidfile
func killStaleBridgeByPidfile(sessionPath string) {
	pidFile := filepath.Join(sessionPath, "bridge.pid")
	if _, err := os.Stat(pidFile); err != nil {
		return
	}
	pid, recordedStart, _ := readBridgePidfile(sessionPath)
	if pid == nil {
		unlinkQuietly(pidFile)
		return
	}
	switch {
	case bridgePidIsOurs(*pid, recordedStart) && *pid == os.Getpid():
		stopInProcessBridge(sessionPath)
	case bridgePidIsOurs(*pid, recordedStart):
		if signalPid(*pid, syscall.SIGTERM) == nil {
			slog.Info(fmt.Sprintf("[whatsapp] Killed stale bridge PID %d from pidfile", *pid))
		}
		if !isWindows {
			if len(waitPidsExit([]int{*pid}, sigtermGrace)) > 0 && bridgePidIsOurs(*pid, recordedStart) {
				if signalPid(*pid, syscall.SIGKILL) == nil {
					slog.Warn(fmt.Sprintf("[whatsapp] SIGKILLed stale bridge PID %d after it ignored SIGTERM", *pid))
				}
			}
			waitPidsExit([]int{*pid}, time.Second)
		}
	case gateway.PidExists(*pid):
		reason := "it is no longer the bridge (recycled onto an unrelated process)"
		if recordedStart == nil {
			reason = "legacy pidfile lacks a start-time fingerprint and cmdline substring evidence can name a stranger"
		}
		slog.Warn(fmt.Sprintf("[whatsapp] Not killing pidfile PID %d: %s; skipping to avoid killing a stranger.", *pid, reason))
	}
	unlinkQuietly(pidFile)
}

// Writes the bridge host pid, its kernel start time and the port it serves for identity checked cleanup
func writeBridgePidfile(sessionPath string, pid int, port *int) {
	start := gateway.GetProcessStartTime(pid)
	text := strconv.Itoa(pid)
	if start != nil {
		text = fmt.Sprintf("%d\n%s", pid, pyrepr.Str(start))
		if port != nil {
			text += fmt.Sprintf("\n%d", *port)
		}
	}
	_ = os.WriteFile(filepath.Join(sessionPath, "bridge.pid"), []byte(text), 0o666)
}

// Image, audio, video and document cache directories of the active profile
func cacheDirs(ctx context.Context) ([4]string, error) {
	h := home.Dir(ctx)
	var out [4]string
	for i, get := range []func(context.Context, string) (string, error){mediacache.ImageCacheDir, mediacache.AudioCacheDir,
		mediacache.VideoCacheDir, mediacache.DocumentCacheDir} {
		dir, err := get(ctx, h)
		if err != nil {
			return out, err
		}
		out[i] = dir
	}
	return out, nil
}

// Whether an absolute bridge path resolves, symlinks included, inside a media cache directory
func isAllowedBridgePath(ctx context.Context, url string) bool {
	resolved, err := pypath.Resolve(url)
	if err != nil {
		return false
	}
	dirs, err := cacheDirs(ctx)
	if err != nil {
		return false
	}
	for _, root := range dirs {
		rootResolved, err := pypath.Resolve(root)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(rootResolved, resolved); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// First 16 hex characters of the SHA-256 of a file, empty when unreadable
func fileContentHash(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}
