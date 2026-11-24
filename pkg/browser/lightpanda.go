package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	lightpandaInstallURL      = "https://lightpanda.io/docs/run-locally/installation/one-liner"
	lightpandaInstallHint     = "Install Lightpanda from " + lightpandaInstallURL + " and make sure `lightpanda` is on PATH"
	lightpandaHTTPCacheFlag   = "--http-cache-dir"
	lightpandaStderrTailLimit = 2000
	// Environment switch that stops Lightpanda sending its usage telemetry
	lightpandaTelemetryOff = "LIGHTPANDA_DISABLE_TELEMETRY"
)

var lightpandaSanePathDirs = []string{"/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/sbin", "/usr/local/bin",
	"/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// The spawned serve child as Popen exposes it, poll reporting the return code once it exited
type lightpandaChild interface {
	pid() int
	poll() (int, bool)
	terminate() error
	kill() error
	wait(timeout time.Duration) bool
}

func (p *launchProc) terminate() error {
	if runtime.GOOS == "windows" {
		return p.cmd.Process.Kill()
	}
	return p.cmd.Process.Signal(syscall.SIGTERM)
}

func (p *launchProc) kill() error { return p.cmd.Process.Kill() }

// One lightpanda serve per browser session, its CDP discovery URL on a loopback port
type LightpandaServer struct {
	SessionName string
	Port        int
	proc        lightpandaChild
	LogPath     string
	StartTime   *int64
}

// HTTP discovery URL, the harness resolves json/version itself
func (s *LightpandaServer) CDPURL() string { return "http://127.0.0.1:" + strconv.Itoa(s.Port) }

func (s *LightpandaServer) IsAlive() bool {
	_, done := s.proc.poll()
	return !done
}

// Process wide servers by session name in insertion order
var (
	lightpandaMu    sync.Mutex
	lightpandaNames []string
	lightpandaByKey = map[string]*LightpandaServer{}
)

// Every function the tests replace, the Python monkeypatch points
var (
	lightpandaStateDir          = defaultLightpandaStateDir
	lightpandaHomeCandidates    = defaultLightpandaHomeCandidates
	lightpandaStartTime         = safeStartTime
	lightpandaSupportsHTTPCache = binarySupportsHTTPCache
	lightpandaHelpRun           = runLightpandaHelp
	lightpandaFindBinary        = findLightpandaBinary
	lightpandaMergeSanePath     = mergeSanePath
	lightpandaFreePort          = pickFreeLoopbackPort
	lightpandaCDPReady          = func(url string) bool { return IsBrowserDebugReady(context.Background(), url, 200*time.Millisecond) }
	lightpandaBrowserEnv        = func(env *tools.Env) map[string]string { return buildBrowserEnv(env) }
	lightpandaPopen             = popenLightpanda
	lightpandaTreeKill          = treeKillLightpanda
	lightpandaPidExists         = proc.Exists
	lightpandaIsServe           = isLightpandaProcess
	lightpandaProcName          = proc.Name
	lightpandaProcCmdline       = tools.ProcessCmdline
	lightpandaProcStartTime     = gateway.GetProcessStartTime
	lightpandaCloudProvider     = func(env *tools.Env) (BrowserProvider, error) { return getCloudProvider(env) }
	lightpandaReadyTimeout      = 10 * time.Second
	lightpandaPollInterval      = 100 * time.Millisecond
)

func lightpandaLogger(env *tools.Env) interface {
	Debug(string, ...any)
	Info(string, ...any)
} {
	return cfgOf(env).logger()
}

// Installer locations then the home's bin directory
func defaultLightpandaHomeCandidates(env *tools.Env) []string {
	userHome := pathutil.ExpandUser("~")
	return []string{
		filepath.Join(userHome, ".lightpanda", "lightpanda"),
		filepath.Join(userHome, ".local", "bin", "lightpanda"),
		filepath.Join(cfgOf(env).homeDir(), "bin", "lightpanda"),
	}
}

// Existing path with the standard system directories that exist appended, existing order kept
func mergeSanePath(existing string) string {
	var parts []string
	for _, p := range strings.Split(existing, string(os.PathListSeparator)) {
		if p != "" {
			parts = append(parts, p)
		}
	}
	out := slices.Clone(parts)
	for _, d := range lightpandaSanePathDirs {
		if st, err := os.Stat(d); err == nil && st.IsDir() && !slices.Contains(parts, d) {
			out = append(out, d)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}

// Lightpanda on PATH plus the system directories, then the installer locations, empty on Windows
func findLightpandaBinary(env *tools.Env) string {
	if runtime.GOOS == "windows" {
		lightpandaLogger(env).Debug("Lightpanda has no Windows build")
		return ""
	}
	search := lightpandaMergeSanePath(os.Getenv("PATH"))
	if found, err := fsutil.LookPathIn("lightpanda", search); err == nil {
		return found
	}
	for _, c := range lightpandaHomeCandidates(env) {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
			if _, err := exec.LookPath(c); err == nil {
				return c
			}
		}
	}
	return ""
}

func pickFreeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// Home cache directory holding the session records and logs, created on demand unless its home is gone or erased
func defaultLightpandaStateDir(env *tools.Env) (string, error) {
	path := filepath.Join(cfgOf(env).homeDir(), "cache", "browser-use", "lightpanda")
	if err := home.AssertProfileLive(path); err != nil {
		return path, err
	}
	return path, os.MkdirAll(path, 0o777)
}

// HTTP cache every spawned Lightpanda shares so a cached asset survives session churn
func lightpandaHTTPCacheDir(env *tools.Env) (string, error) {
	state, err := lightpandaStateDir(env)
	if err != nil {
		return "", err
	}
	path := filepath.Join(state, "http-cache")
	return path, os.MkdirAll(path, 0o777)
}

var lightpandaProbe struct {
	sync.Mutex
	binary    string
	supported bool
	cached    bool
}

// Output of binary help within three seconds, stdout and stderr joined
func runLightpandaHelp(binary string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "help")
	var out strings.Builder
	var errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return "", err
	}
	return out.String() + errOut.String(), nil
}

// Whether lightpanda serve accepts the http cache flag, probed once per binary from its help output
func binarySupportsHTTPCache(binary string) bool {
	lightpandaProbe.Lock()
	defer lightpandaProbe.Unlock()
	if lightpandaProbe.cached && lightpandaProbe.binary == binary {
		return lightpandaProbe.supported
	}
	text, err := lightpandaHelpRun(binary)
	supported := err == nil && strings.Contains(text, lightpandaHTTPCacheFlag)
	if err != nil {
		lightpandaLogger(nil).Debug(fmt.Sprintf("lightpanda http-cache probe failed (%v); assuming no", err))
	}
	lightpandaProbe.binary, lightpandaProbe.supported, lightpandaProbe.cached = binary, supported, true
	return supported
}

// Last non empty line of the child's stderr log, empty when unreadable
func readLightpandaLogTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > lightpandaStderrTailLimit {
		data = data[len(data)-lightpandaStderrTailLimit:]
	}
	lines := textutil.Lines(strings.TrimSpace(strings.ToValidUTF8(string(data), "\uFFFD")))
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// Terminates a live child, waits five seconds and kills it when it lingers
func terminateLightpandaChild(env *tools.Env, child lightpandaChild, what string) {
	if _, done := child.poll(); done {
		return
	}
	if err := child.terminate(); err != nil {
		lightpandaLogger(env).Debug(fmt.Sprintf("%s terminate failed: %v", what, err))
		return
	}
	if !child.wait(5 * time.Second) {
		if err := child.kill(); err != nil {
			lightpandaLogger(env).Debug(fmt.Sprintf("%s terminate failed: %v", what, err))
		}
	}
}

func safeStartTime(pid int) *int64 {
	if v, ok := tools.ProcessStartTime(pid); ok {
		return &v
	}
	return nil
}

// Tree kill after the start time check, a panic out of the registry returned as the error it raised
func treeKillLightpanda(pid int, expectedStart *int64) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	tools.TerminateHostPid(nil, pid, expectedStart)
	return nil
}

func writeLightpandaRecord(ctx context.Context, env *tools.Env, server *LightpandaServer) {
	var start any
	if server.StartTime != nil {
		start = *server.StartTime
	}
	record := jsonutil.ObjectOf("pid", server.proc.pid(), "port", server.Port, "owner_pid", os.Getpid(),
		"start_time", start, "started_at", float64(time.Now().UnixNano())/1e9)
	dir, err := lightpandaStateDir(env)
	if err == nil {
		err = config.AtomicJSONWriteWith(tools.WithEnvHome(ctx, env), filepath.Join(dir, server.SessionName+".json"), record)
	}
	if err != nil {
		lightpandaLogger(env).Debug(fmt.Sprintf("could not write lightpanda record for %s: %s", server.SessionName, err.Error()))
	}
}

// Detached serve child with stdin and stdout on the null device and stderr in the log
func popenLightpanda(argv []string, logFile *os.File, procEnv map[string]string) (lightpandaChild, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = make([]string, 0, len(procEnv))
	for k, v := range procEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedSpawnAttrs()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &launchProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		p.code = popenReturnCode(cmd.ProcessState)
		close(p.done)
	}()
	return p, nil
}

// Starts lightpanda serve on a free loopback port and returns it once json/version answers
func launchLightpanda(ctx context.Context, env *tools.Env, sessionName string, blockPrivateNetworks bool) (*LightpandaServer, string) {
	binary := lightpandaFindBinary(env)
	if binary == "" {
		if runtime.GOOS == "windows" {
			return nil, "browser.engine is 'lightpanda' but Lightpanda has no Windows build. Set browser.engine to auto (or run Icebath under WSL2)."
		}
		return nil, "browser.engine is 'lightpanda' but no lightpanda binary was found on PATH, ~/.lightpanda or ~/.local/bin. " +
			lightpandaInstallHint + ", or set browser.engine to auto."
	}
	port, err := lightpandaFreePort()
	if err != nil {
		return nil, err.Error()
	}
	argv := []string{binary, "serve", "--host", "127.0.0.1", "--port", strconv.Itoa(port)}
	if lightpandaSupportsHTTPCache(binary) {
		cache, err := lightpandaHTTPCacheDir(env)
		if err != nil {
			return nil, err.Error()
		}
		argv = append(argv, lightpandaHTTPCacheFlag, cache)
	}
	if blockPrivateNetworks {
		argv = append(argv, "--block-private-networks")
	}
	state, err := lightpandaStateDir(env)
	if err != nil {
		return nil, err.Error()
	}
	logPath := filepath.Join(state, sessionName+".log")
	child, err := func() (lightpandaChild, error) {
		logFile, err := os.Create(logPath)
		if err != nil {
			return nil, err
		}
		defer logFile.Close()
		procEnv := lightpandaBrowserEnv(env)
		procEnv[lightpandaTelemetryOff] = "true"
		return lightpandaPopen(argv, logFile, procEnv)
	}()
	if err != nil {
		return nil, fmt.Sprintf("Failed to launch lightpanda serve (%s): %s", binary, err.Error())
	}
	url := "http://127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(lightpandaReadyTimeout)
	for {
		if rc, done := child.poll(); done {
			msg := fmt.Sprintf("lightpanda serve exited with code %d before %s/json/version answered", rc, url)
			if tail := readLightpandaLogTail(logPath); tail != "" {
				msg += ": " + tail
			}
			return nil, msg
		}
		if lightpandaCDPReady(url) {
			break
		}
		if !time.Now().Before(deadline) {
			terminateLightpandaChild(env, child, "lightpanda")
			detail := ""
			if tail := readLightpandaLogTail(logPath); tail != "" {
				detail = " (last stderr line: " + tail + ")"
			}
			return nil, fmt.Sprintf("lightpanda serve did not expose %s/json/version within %ds%s", url, int(lightpandaReadyTimeout.Seconds()), detail)
		}
		time.Sleep(lightpandaPollInterval)
	}
	server := &LightpandaServer{SessionName: sessionName, Port: port, proc: child, LogPath: logPath, StartTime: lightpandaStartTime(child.pid())}
	writeLightpandaRecord(tools.WithEnvHome(ctx, env), env, server)
	lightpandaMu.Lock()
	if _, ok := lightpandaByKey[sessionName]; !ok {
		lightpandaNames = append(lightpandaNames, sessionName)
	}
	lightpandaByKey[sessionName] = server
	lightpandaMu.Unlock()
	lightpandaLogger(env).Info(fmt.Sprintf("Started lightpanda serve (pid %d, port %d) for session %s", child.pid(), port, sessionName))
	return server, ""
}

func getLightpandaServer(sessionName string) *LightpandaServer {
	lightpandaMu.Lock()
	defer lightpandaMu.Unlock()
	return lightpandaByKey[sessionName]
}

func popLightpandaServer(sessionName string) *LightpandaServer {
	lightpandaMu.Lock()
	defer lightpandaMu.Unlock()
	server := lightpandaByKey[sessionName]
	if server != nil {
		delete(lightpandaByKey, sessionName)
		lightpandaNames = slices.DeleteFunc(lightpandaNames, func(n string) bool { return n == sessionName })
	}
	return server
}

// Unlink that tolerates a missing file and refuses a directory as os.unlink does
func unlinkMissingOK(path string) error {
	if err := syscall.Unlink(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &fs.PathError{Op: "unlink", Path: path, Err: err}
	}
	return nil
}

// Stops the session's server by tree kill, plain terminate when that fails, and drops its record
func stopLightpanda(env *tools.Env, sessionName string) {
	server := popLightpandaServer(sessionName)
	if server != nil && server.IsAlive() {
		if err := lightpandaTreeKill(server.proc.pid(), server.StartTime); err != nil {
			lightpandaLogger(env).Debug(fmt.Sprintf("lightpanda tree-kill failed for %s: %v", server.SessionName, err))
			terminateLightpandaChild(env, server.proc, "lightpanda")
		}
		if !server.proc.wait(5 * time.Second) {
			terminateLightpandaChild(env, server.proc, "lightpanda")
		}
	}
	dir, err := lightpandaStateDir(env)
	if err == nil {
		err = unlinkMissingOK(filepath.Join(dir, sessionName+".json"))
	}
	if err != nil {
		lightpandaLogger(env).Debug(fmt.Sprintf("could not remove lightpanda record for %s: %s", sessionName, err.Error()))
	}
	if server != nil {
		lightpandaLogger(env).Debug("Stopped lightpanda serve for session " + sessionName)
	}
}

// Stops every server this process started, idempotent and safe at exit
func stopAllLightpanda(env *tools.Env) {
	lightpandaMu.Lock()
	names := slices.Clone(lightpandaNames)
	lightpandaMu.Unlock()
	for _, name := range names {
		func() {
			defer func() {
				if r := recover(); r != nil {
					lightpandaLogger(env).Debug(fmt.Sprintf("lightpanda stop failed for %s: %v", name, r))
				}
			}()
			stopLightpanda(env, name)
		}()
	}
}

// True only when pid is verifiably the lightpanda serve recorded on that port with that start time
func isLightpandaProcess(pid int, port, startTime any) bool {
	name, err := lightpandaProcName(pid)
	if err != nil || !strings.Contains(strings.ToLower(name), "lightpanda") {
		return false
	}
	cmdline, err := lightpandaProcCmdline(pid)
	if err != nil || !slices.Contains(cmdline, "serve") || !slices.Contains(cmdline, anyutil.String(port)) {
		return false
	}
	if anyutil.IsTrue(startTime) {
		return anyutil.Equal(lightpandaProcStartTime(pid), startTime)
	}
	return true
}

// Expected start of a record value, nil for null and an unmatchable start for values no start time equals
func recordStartTime(v any) *int64 {
	if v == nil {
		return nil
	}
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		n := int64(f)
		return &n
	}
	if n, ok := anyutil.Int(v); ok {
		return &n
	}
	unmatchable := int64(-1)
	return &unmatchable
}

// Pid a record holds, -1 for a number past the int64 range, the error of a value that is not a number ending the sweep
func recordPid(v any) (int, error) {
	n, err := anyutil.ToInt(v)
	if errors.Is(err, anyutil.ErrRange) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Kills lightpanda serve processes whose owner is gone and returns the count, a live owner never touched
func reapOrphanedLightpanda(env *tools.Env) (int, error) {
	log := lightpandaLogger(env)
	stateDir, err := lightpandaStateDir(env)
	if err != nil {
		log.Debug("lightpanda state dir unavailable: " + err.Error())
		return 0, nil
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return 0, nil
	}
	reaped := 0
	for _, entry := range entries {
		if ok, _ := filepath.Match("*.json", entry.Name()); !ok {
			continue
		}
		recordPath := filepath.Join(stateDir, entry.Name())
		sessionName := strings.TrimSuffix(entry.Name(), ".json")
		record, ok := readLightpandaRecord(recordPath)
		if !ok {
			if err := unlinkMissingOK(recordPath); err != nil {
				return reaped, err
			}
			continue
		}
		ownerPid, _ := record.Get("owner_pid")
		if anyutil.Equal(ownerPid, int64(os.Getpid())) {
			if getLightpandaServer(sessionName) != nil {
				continue
			}
		} else if anyutil.IsTrue(ownerPid) {
			owner, err := recordPid(ownerPid)
			if err != nil {
				return reaped, err
			}
			if owner >= 0 && lightpandaPidExists(owner) {
				continue
			}
		}
		pidValue, _ := record.Get("pid")
		port, _ := record.Get("port")
		startTime, _ := record.Get("start_time")
		if anyutil.IsTrue(pidValue) {
			pid, err := recordPid(pidValue)
			if err != nil {
				return reaped, err
			}
			if pid >= 0 && lightpandaIsServe(pid, port, startTime) {
				if err := lightpandaTreeKill(pid, recordStartTime(startTime)); err != nil {
					log.Debug(fmt.Sprintf("orphan lightpanda kill failed for pid %d: %v", pid, err))
				} else {
					reaped++
					log.Info(fmt.Sprintf("Reaped orphaned lightpanda serve pid %d (session %s)", pid, sessionName))
				}
			}
		}
		if err := unlinkMissingOK(recordPath); err != nil {
			return reaped, err
		}
	}
	return reaped, nil
}

// Record object of a state file, false for unreadable, undecodable or non object content
func readLightpandaRecord(path string) (*jsonutil.Object, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return nil, false
	}
	value, err := jsonutil.Decode(string(data))
	if err != nil {
		return nil, false
	}
	return jsonutil.AsObject(value)
}

func usingLightpandaEngine(env *tools.Env) bool { return cfgOf(env).usingLightpanda() }

// Whether browser.engine lightpanda is in effect and the setting shadowing it, config reads only
func lightpandaEngineStatus(env *tools.Env) (bool, string) {
	if !usingLightpandaEngine(env) {
		return false, ""
	}
	s := cfgOf(env)
	if s.cdpOverrideRaw() != "" {
		return false, "a CDP override is active (/browser connect or browser.cdp_url)"
	}
	if s.useRealProfile() {
		return false, "browser.use_real_profile is on (Lightpanda cannot load a Chromium profile)"
	}
	provider, err := lightpandaCloudProvider(env)
	if err != nil {
		provider = nil
	}
	if provider != nil {
		name := provider.DisplayName()
		if name == "" {
			t := reflect.TypeOf(provider)
			for t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			name = t.Name()
		}
		return false, "cloud provider " + name + " is selected (browser.cloud_provider, or auto-detected from credentials)"
	}
	if browserUseCloudByKey(env) {
		return false, "Browser Use cloud (BROWSER_USE_API_KEY) is selected"
	}
	return true, "Icebath spawns `lightpanda serve` per browser session"
}
