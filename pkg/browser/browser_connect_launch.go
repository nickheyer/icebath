package browser

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

const (
	// DefaultBrowserCDPPort is the remote debugging port /browser connect probes and launches on
	DefaultBrowserCDPPort = 9222
	// DefaultBrowserCDPURL is the local CDP endpoint /browser connect uses without an argument
	DefaultBrowserCDPURL = "http://127.0.0.1:9222"
	launchStderrLog      = "launch-stderr.log"
	stderrTailLimit      = 2000
)

var connectLog = slog.Default().With("logger", "hermes_cli.browser_connect")

// Possible debug browser binaries in launch order, empty entries for unresolved names
func debugCandidatePaths(system string) []string {
	var out []string
	installBases := []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")}
	which := func(name string) string {
		found, _ := pyshutil.Which(name, nil)
		return found
	}
	for _, b := range chromiumProducts {
		switch system {
		case "Darwin":
			out = append(out, b.macApp)
		case "Windows":
			for _, name := range b.winBins {
				out = append(out, which(name))
			}
			for _, base := range installBases {
				if base == "" {
					continue
				}
				for _, parts := range b.winInstall {
					out = append(out, filepath.Join(append([]string{base}, parts...)...))
				}
			}
		default:
			for _, name := range b.linuxBins {
				out = append(out, which(name))
			}
			out = append(out, b.linuxPaths...)
		}
	}
	if system != "Darwin" && system != "Windows" {
		for _, b := range chromiumProducts {
			for _, base := range []string{"/mnt/c/Program Files", "/mnt/c/Program Files (x86)"} {
				for _, parts := range b.winInstall {
					out = append(out, path.Join(append([]string{base}, parts...)...))
				}
			}
		}
	}
	return out
}

// Existing debug browser binaries deduplicated by normalized path, first seen kept
func GetChromeDebugCandidates(system string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range debugCandidatePaths(system) {
		if p == "" {
			continue
		}
		normalized := pypath.Normcase(pypath.Normpath(p))
		if seen[normalized] || !isFilePath(p) {
			continue
		}
		seen[normalized] = true
		out = append(out, p)
	}
	return out
}

// Profile directory of the debug browser under the icebath home
func ChromeDebugDataDir(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "chrome-debug")
}

// True when a package manager owns this install's modes
func managedInstall(ctx context.Context) bool {
	return config.ManagedSystem(ctx, home.Dir(ctx)) != ""
}

// Creates the debug profile directory owner only unless managed, then reconciles it through the secure dir policy
func ensureChromeDebugDataDir(ctx context.Context, dataDir string) error {
	if managedInstall(ctx) {
		return os.MkdirAll(dataDir, 0o777)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	config.SecureDir(ctx, home.Dir(ctx), dataDir)
	return nil
}

// Opens the launch stderr log truncated and owner only unless managed, reconciling an older looser file
func openLaunchStderrLog(ctx context.Context, p string) (*os.File, error) {
	managed := managedInstall(ctx)
	mode := os.FileMode(0o600)
	if managed {
		mode = 0o666
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		if f, err = os.Create(p); err != nil {
			return nil, err
		}
	}
	if !managed {
		config.SecureFile(ctx, home.Dir(ctx), p)
	}
	return f, nil
}

// Remote debugging flags of a debug browser launch
func chromeDebugArgs(ctx context.Context, port int) []string {
	return []string{fmt.Sprintf("--remote-debugging-port=%d", port), "--user-data-dir=" + ChromeDebugDataDir(ctx),
		"--no-first-run", "--no-default-browser-check"}
}

// Whether a reusable TCP socket binds the host and port
func loopbackBindable(network, host string, port int) bool {
	lc := net.ListenConfig{Control: reuseAddrControl}
	ln, err := lc.Listen(context.Background(), network, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// First port after preferred bindable on both loopbacks, preferred plus one when none binds
func FindFreeDebugPort(preferred, attempts int) int {
	for p := preferred + 1; p < preferred+1+attempts; p++ {
		if loopbackBindable("tcp4", "127.0.0.1", p) && loopbackBindable("tcp6", "::1", p) {
			return p
		}
	}
	return preferred + 1
}

// Copy paste command that starts a debug browser, empty when no binary is known
func ManualChromeDebugCommand(ctx context.Context, port int, system string) string {
	system = systemOr(system)
	if candidates := GetChromeDebugCandidates(system); len(candidates) > 0 {
		argv := append([]string{candidates[0]}, chromeDebugArgs(ctx, port)...)
		if system == "Windows" {
			return pysubprocess.List2cmdline(argv)
		}
		return pyshlex.Join(argv)
	}
	if system == "Darwin" {
		return fmt.Sprintf(`open -a "Google Chrome" --args --remote-debugging-port=%d `, port) +
			fmt.Sprintf(`--user-data-dir="%s" --no-first-run `, ChromeDebugDataDir(ctx)) +
			"--no-default-browser-check"
	}
	return ""
}

// Classifies a launched browser as ready, exited or starting within the grace window
func waitForBrowserDebugReadyOrExit(ctx context.Context, exited <-chan struct{}, port int, timeout, interval time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if DiscoverLocalCDPURL(ctx, port, min(interval, 200*time.Millisecond)) != "" {
			return "ready"
		}
		select {
		case <-exited:
			return "exited"
		default:
		}
		time.Sleep(interval)
	}
	return "starting"
}

// LaunchAttempt is the outcome of one candidate binary launch
type LaunchAttempt struct {
	Binary, State string
	ReturnCode    *int
	StderrTail    string
}

// ChromeDebugLaunch is whether a debug browser was spawned and the attempts explaining why not
type ChromeDebugLaunch struct {
	Launched bool
	Attempts []*LaunchAttempt
}

// Best user facing explanation of a failed or soft launch, empty when none applies
func (l ChromeDebugLaunch) Hint() string {
	for _, a := range l.Attempts {
		if a.State == "exited" && a.ReturnCode != nil && *a.ReturnCode == 0 {
			name := filepath.Base(a.Binary)
			return name + " exited immediately without opening the debug port — an already-running " +
				name + " instance likely absorbed the launch (Chromium's single-instance " +
				"behavior). Close ALL of its processes (including background/tray instances) " +
				"and retry /browser connect."
		}
	}
	for _, a := range l.Attempts {
		if a.State == "exited" && a.StderrTail != "" {
			lines := pystr.Splitlines(a.StderrTail, false)
			return filepath.Base(a.Binary) + " exited before the debug port opened: " + pystr.Strip(lines[len(lines)-1])
		}
	}
	return ""
}

// Last bytes of the launch stderr log decoded with replacement, empty when unreadable
func readStderrTail(p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	if len(data) > stderrTailLimit {
		data = data[len(data)-stderrTailLimit:]
	}
	return pystr.Strip(strings.ToValidUTF8(string(data), "�"))
}

// Launches a Chromium family browser with remote debugging, trying each candidate until one stays up
func LaunchChromeDebug(ctx context.Context, port int, system string) ChromeDebugLaunch {
	system = systemOr(system)
	result := ChromeDebugLaunch{}
	candidates := GetChromeDebugCandidates(system)
	if len(candidates) == 0 {
		connectLog.Info(fmt.Sprintf("browser debug launch: no Chromium-family binary found (system=%s)", system))
		return result
	}
	dataDir := ChromeDebugDataDir(ctx)
	if err := ensureChromeDebugDataDir(ctx, dataDir); err != nil {
		panic(err)
	}
	stderrPath := filepath.Join(dataDir, launchStderrLog)
	for _, candidate := range candidates {
		cmd, err := spawnDebugCandidate(ctx, candidate, port, stderrPath)
		if err != nil {
			result.Attempts = append(result.Attempts, &LaunchAttempt{Binary: candidate, State: "spawn-failed"})
			connectLog.Info(fmt.Sprintf("browser debug launch: failed to spawn %s: %s", candidate, err))
			continue
		}
		connectLog.Info(fmt.Sprintf("browser debug launch: spawned %s (pid=%d) with --remote-debugging-port=%d", candidate, cmd.Process.Pid, port))
		exited := make(chan struct{})
		var code int
		go func() {
			_ = cmd.Wait()
			code = popenReturnCode(cmd.ProcessState)
			close(exited)
		}()
		state := waitForBrowserDebugReadyOrExit(ctx, exited, port, 2*time.Second, 100*time.Millisecond)
		attempt := &LaunchAttempt{Binary: candidate, State: state}
		result.Attempts = append(result.Attempts, attempt)
		if state != "exited" {
			result.Launched = true
			return result
		}
		<-exited
		rc := code
		attempt.ReturnCode = &rc
		attempt.StderrTail = readStderrTail(stderrPath)
		tail := ""
		if attempt.StderrTail != "" {
			tail = "; stderr tail: " + attempt.StderrTail
		}
		connectLog.Warn(fmt.Sprintf("browser debug launch: %s exited (code=%d) before port %d opened%s", candidate, rc, port, tail))
	}
	return result
}

// Starts one candidate detached with stdout discarded and stderr in the launch log
func spawnDebugCandidate(ctx context.Context, candidate string, port int, stderrPath string) (*exec.Cmd, error) {
	stderrFile, err := openLaunchStderrLog(ctx, stderrPath)
	if err != nil {
		return nil, err
	}
	defer stderrFile.Close()
	cmd := exec.Command(candidate, chromeDebugArgs(ctx, port)...)
	cmd.Stdout = nil
	cmd.Stderr = stderrFile
	cmd.SysProcAttr = debugLaunchAttrs()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
