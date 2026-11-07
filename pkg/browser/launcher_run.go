package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// What a launch starts or attaches, a zero Executable resolving the packaged Chromium
type LaunchOptions struct {
	Executable    string
	UserDataDir   string
	Ephemeral     bool
	Headed        *bool
	ExtraArgs     []string
	ProcEnv       map[string]string
	Timeout       *time.Duration
	SandboxBypass bool
	Prepare       func() string
}

// A spawned Chromium whose exit a goroutine reaps, its code negative for a signal as Popen reports it
type launchProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	code int
}

func (p *launchProc) pid() int { return p.cmd.Process.Pid }

func (p *launchProc) poll() (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	default:
		return 0, false
	}
}

func (p *launchProc) wait(timeout time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Insertion ordered table of browsers by launcher key
type browserTable struct {
	keys []string
	m    map[string]*LaunchedBrowser
}

func (t *browserTable) get(key string) *LaunchedBrowser { return t.m[key] }

func (t *browserTable) set(key string, b *LaunchedBrowser) {
	if _, ok := t.m[key]; !ok {
		t.keys = append(t.keys, key)
	}
	t.m[key] = b
}

func (t *browserTable) del(key string) {
	if _, ok := t.m[key]; !ok {
		return
	}
	delete(t.m, key)
	for i, k := range t.keys {
		if k == key {
			t.keys = append(t.keys[:i:i], t.keys[i+1:]...)
			break
		}
	}
}

func (t *browserTable) values() []*LaunchedBrowser {
	out := make([]*LaunchedBrowser, 0, len(t.keys))
	for _, k := range t.keys {
		out = append(out, t.m[k])
	}
	return out
}

// Process wide launcher registry guarded by launcherMu, launching keyed by the pid of a Chromium still starting
var (
	launcherMu           sync.Mutex
	browsers             = &browserTable{m: map[string]*LaunchedBrowser{}}
	procs                = map[string]*launchProc{}
	launching            = &browserTable{m: map[string]*LaunchedBrowser{}}
	keyLocks             = map[string]*sync.Mutex{}
	autoinstallAttempted bool
	servedHomes          = map[string]bool{}
)

// Seams tests replace the way hermes tests monkeypatch the launcher module
var (
	launcherResolveExecutable   = resolveExecutable
	launcherIsTermux            = hostinfo.IsTermux
	launcherLazyInstallsAllowed = pm.LazyInstallsAllowed
	launcherEnsureChromium      = func() error {
		_, err := pm.Ensure(context.Background(), "chromium", pm.EnsureOptions{})
		return err
	}
	launcherCmdline   = tools.ProcessCmdline
	launcherHostname  = func() string { h, _ := os.Hostname(); return h }
	launcherProcesses = func() []int {
		table, err := proc.Snapshot()
		if err != nil {
			return nil
		}
		pids := make([]int, 0, len(table))
		for _, p := range table {
			pids = append(pids, p.PID)
		}
		sort.Ints(pids)
		return pids
	}
	launcherSpawn            = spawnChromium
	launcherTerminate        = terminateBrowser
	launcherRemoveAll        = rmtree
	launcherUnresponsiveWait = unresponsiveWait
	launcherEndpointLive     = devtoolsEndpointLive
	launcherNamesDir         = cmdlineNamesDir
)

const isWindowsHost = runtime.GOOS == "windows"

func realpathLax(path string) (string, error) { return pypath.Realpath(path, pypath.Lax) }

func int64p(v int) *int64 { n := int64(v); return &n }

func startTimeOf(pid int) *int64 {
	if v, ok := launcherStartTime(pid); ok {
		return &v
	}
	return nil
}

func samePtr(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func pidText(pid *int64) string { return pyrepr.Str(optInt(pid)) }

func jsonIntPtr(v any) *int64 {
	if n, ok := jsonInt(v); ok {
		return &n
	}
	return nil
}

// Removes a tree like shutil.rmtree with ignore_errors, refusing a symlink instead of following it
func rmtree(path string) {
	if info, err := os.Lstat(path); err != nil || info.Mode()&fs.ModeSymlink != 0 {
		return
	}
	os.RemoveAll(path)
}

func lexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Creates the leaf with mode and its missing parents with the default mode, an existing dir accepted
func mkdirLeaf(path string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	err := os.Mkdir(path, mode)
	if err != nil && errors.Is(err, fs.ErrExist) {
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			return nil
		}
	}
	return err
}

func resolveExecutable(env *tools.Env) string {
	exe := chromiumExecutable(env, true)
	if exe == "" {
		return ""
	}
	if isRegularFile(exe) {
		return exe
	}
	if found := runtimeLocateCommand(exe); len(found) > 0 {
		return found[0]
	}
	return ""
}

// An operator pinned binary, even a missing one, is their choice and never downloaded over
func explicitExecutableConfigured(env *tools.Env) bool {
	return chromiumExecutable(env, true) != chromiumExecutable(env, false)
}

// One pm chromium install per process, never in Docker, on Termux or over a pinned binary
func maybeAutoinstallChromium(env *tools.Env) bool {
	launcherMu.Lock()
	attempted := autoinstallAttempted
	autoinstallAttempted = true
	launcherMu.Unlock()
	if attempted {
		return false
	}
	if launcherInDocker() || launcherIsTermux() || explicitExecutableConfigured(env) {
		return false
	}
	if !launcherLazyInstallsAllowed() {
		return false
	}
	launcherLogger().Info("browser: installing PM's pinned Chromium")
	if err := launcherEnsureChromium(); err != nil {
		launcherLogger().Warn(fmt.Sprintf("browser: Chromium auto-install failed: %s", err))
		return false
	}
	return true
}

// Ready with an empty hint when a Chromium is on disk after at most one auto install, else the hint
func chromiumReady(env *tools.Env) (bool, string) {
	if launcherResolveExecutable(env) != "" {
		return true, ""
	}
	if maybeAutoinstallChromium(env) && launcherResolveExecutable(env) != "" {
		return true, ""
	}
	return false, chromiumMissingHint(env)
}

// The DevTools endpoint once written, or why Chromium exited or never exposed one before the deadline
func waitForDevtools(p *launchProc, userDataDir string, deadline time.Time) (int, string, string) {
	for {
		port, path, found, err := readDevtoolsFile(userDataDir)
		if err != nil {
			return 0, "", err.Error()
		}
		if found {
			return port, path, ""
		}
		if rc, exited := p.poll(); exited {
			tail := logTail(userDataDir)
			hint := ""
			if strings.Contains(pystr.Lower(tail), "sandbox") {
				hint = " Chromium's sandbox could not start; add --no-sandbox to browser.chromium_args."
			}
			msg := fmt.Sprintf("Chromium exited with code %d during startup.%s", rc, hint)
			if tail != "" {
				msg += "\n" + tail
			}
			return 0, "", msg
		}
		if !time.Now().Before(deadline) {
			return 0, "", "Chromium did not expose a DevTools endpoint in time."
		}
		time.Sleep(launcherPollInterval)
	}
}

// Pid of the Chromium holding the dir, its SingletonLock on POSIX and the browser naming it on Windows
func singletonPid(userDataDir string) (int, bool) {
	if _, pid, ok := singletonOwner(userDataDir); ok {
		return pid, true
	}
	if isWindowsHost {
		return pidNamingDir(userDataDir)
	}
	return 0, false
}

// SingletonLock pid of a Chromium on the dir that runs in an environment this process cannot verify
func foreignHolder(userDataDir string) (int, bool) {
	host, pid, ok := singletonOwner(userDataDir)
	if !ok {
		return 0, false
	}
	if launcherPidAlive(pid) && launcherNamesDir(pid, userDataDir) {
		return 0, false
	}
	if host != launcherHostname() || foreignRecordNames(userDataDir, pid) {
		return pid, true
	}
	return 0, false
}

// A launch record of another PID namespace names pid as the profile's current or still starting browser
func foreignRecordNames(userDataDir string, pid int) bool {
	state := sharedStateDir(userDataDir)
	if state == "" {
		return false
	}
	_, path, found, _ := readDevtoolsFile(userDataDir)
	for _, name := range recordNames(state, userDataDir, int64(pid)) {
		record := loadStateFile(filepath.Join(state, name))
		if record == nil || !pyrepr.Equal(objGet(record, "pid"), int64(pid)) || sameNamespace(record) ||
			!sameProfile(objGet(record, "user_data_dir"), userDataDir) {
			continue
		}
		devtools := objGet(record, "devtools")
		if devtools == nil || (found && pyrepr.Equal(devtools, path)) {
			return true
		}
	}
	return false
}

// The browser process, never a --type helper, whose argv carries the dir
func pidNamingDir(userDataDir string) (int, bool) {
	for _, pid := range launcherProcesses() {
		argv, err := launcherCmdline(pid)
		if err != nil {
			continue
		}
		helper := false
		for _, tok := range argv {
			if strings.HasPrefix(tok, "--type=") {
				helper = true
				break
			}
		}
		if !helper && launcherNamesDir(pid, userDataDir) {
			return pid, true
		}
	}
	return 0, false
}

func browserRunningOn(userDataDir string) bool {
	_, ok := pidNamingDir(userDataDir)
	return ok
}

// Port, browser path and pid of a live Chromium already serving DevTools on the dir
var runningInstance = func(userDataDir string) (port int, browserPath string, pid int, ok bool) {
	pid, held := singletonPid(userDataDir)
	port, browserPath, found, _ := readDevtoolsFile(userDataDir)
	if !held || !found || !launcherPidAlive(pid) {
		return 0, "", 0, false
	}
	if !launcherEndpointLive(port, browserPath) {
		return 0, "", 0, false
	}
	return port, browserPath, pid, true
}

// Port, browser path and pid of the live Chromium already serving DevTools on the dir, for other packages
func RunningInstance(userDataDir string) (int, string, int, bool) {
	return runningInstance(userDataDir)
}

// A fresh dir for a throwaway profile, any dir for a shared one, without a stale DevToolsActivePort
func prepareDir(userDataDir string, ephemeral bool) error {
	if ephemeral {
		if err := mkdirLeaf(filepath.Dir(userDataDir), 0o700); err != nil {
			return err
		}
		launcherRemoveAll(userDataDir)
	}
	if err := mkdirLeaf(userDataDir, 0o700); err != nil {
		return err
	}
	os.Remove(filepath.Join(userDataDir, devtoolsFile))
	return nil
}

// Starts argv detached with its output in the profile's launch log
func spawnChromium(argv []string, procEnv map[string]string, userDataDir string) (*launchProc, error) {
	log, err := os.Create(filepath.Join(userDataDir, launchLog))
	if err != nil {
		return nil, err
	}
	defer log.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = make([]string, 0, len(procEnv))
	for k, v := range procEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = log, log
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

// The live Chromium already on a shared profile, leased, caller holding the profile's lock
func attachRunning(key, userDataDir, homeKey string) (*LaunchedBrowser, error) {
	port, path, pid, ok := runningInstance(userDataDir)
	if !ok {
		return nil, nil
	}
	launcherLogger().Info(fmt.Sprintf("browser: attaching to the Chromium already running on %s (pid %d)", userDataDir, pid))
	b := makeBrowser(key, port, path, int64p(pid), startTimeOf(pid), userDataDir, false, false, homeKey)
	if records := stateDir(b); records != "" {
		bindLaunchRecord(records, b)
		if err := adoptRecord(b); err != nil {
			return nil, err
		}
		if err := writeLease(recordPath(b), b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Binds the browser to the launch record of this launch of it in any namespace, this namespace's first
func bindLaunchRecord(records string, b *LaunchedBrowser) {
	own := recordName(b)
	names := recordNames(records, b.UserDataDir, optInt(b.PID))
	sort.SliceStable(names, func(i, j int) bool { return names[i] == own && names[j] != own })
	for _, name := range names {
		record := loadStateFile(filepath.Join(records, name))
		if record != nil && recordIs(record, b) {
			b.RecordName = name
			if b.Foreign {
				b.StartTime = jsonIntPtr(objGet(record, "start_time"))
			}
			return
		}
	}
}

// The Chromium another environment holds on the dir, leased when its endpoint answers here, never adopted
func attachForeign(key, userDataDir, homeKey string, pid int) (*LaunchedBrowser, error) {
	port, path, found, _ := readDevtoolsFile(userDataDir)
	if !found || !launcherEndpointLive(port, path) {
		return nil, nil
	}
	launcherLogger().Info(fmt.Sprintf("browser: attaching to the Chromium another environment runs on %s (its pid %d there)",
		userDataDir, pid))
	b := makeBrowser(key, port, path, int64p(pid), nil, userDataDir, false, false, homeKey)
	b.Foreign = true
	if records := stateDir(b); records != "" {
		bindLaunchRecord(records, b)
		if err := writeLease(recordPath(b), b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Pid of this environment's live Chromium on the dir, none for no browser or a stale lock
func liveLocalHolder(userDataDir string) (int, bool) {
	pid, ok := singletonPid(userDataDir)
	if ok && launcherPidAlive(pid) && launcherNamesDir(pid, userDataDir) {
		return pid, true
	}
	return 0, false
}

// Attaches to this environment's Chromium on the dir, waiting out then refusing one that stopped answering
func attachLocal(env *tools.Env, key, userDataDir, homeKey string, timeout *time.Duration) (*LaunchedBrowser, string) {
	attached, err := attachRunning(key, userDataDir, homeKey)
	if err != nil {
		return nil, err.Error()
	}
	pid, held := 0, false
	if attached == nil {
		pid, held = liveLocalHolder(userDataDir)
	}
	if !held {
		return attached, ""
	}
	deadline := time.Now().Add(min(launcherUnresponsiveWait, launchTimeout(env, timeout)))
	delay := 250 * time.Millisecond
	for attached == nil {
		if now, ok := liveLocalHolder(userDataDir); !ok || now != pid {
			break
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Sprintf("The browser profile %s is in use by Chromium pid %d, which is not "+
				"responding on its DevTools endpoint (paused, hung, or started without remote debugging). "+
				"Icebath will not start a second browser on it; try again once it responds, or stop it.", userDataDir, pid)
		}
		time.Sleep(min(delay, max(0, time.Until(deadline))))
		delay *= 2
		if attached, err = attachRunning(key, userDataDir, homeKey); err != nil {
			return nil, err.Error()
		}
	}
	return attached, ""
}

func foreignUnreachable(userDataDir string) string {
	return fmt.Sprintf("The browser profile %s is in use by a Chromium in another environment (a container or "+
		"the host) that this process can neither reach nor verify, so Icebath will not start a second browser "+
		"on it. Stop that browser, or set browser.user_data_dir to another directory for this environment; if "+
		"no browser runs there any more, delete %s.", userDataDir, filepath.Join(userDataDir, "SingletonLock"))
}

// Attaches to or launches a shared profile's one Chromium as one step under the profile's cross process lock
func startShared(env *tools.Env, key, executable, udd, homeKey string, packaged bool, opts LaunchOptions) (*LaunchedBrowser, string) {
	if state := sharedStateDir(udd); state != "" {
		_, unlock, err := recordsLock(state, true)
		if err != nil {
			return nil, err.Error()
		}
		defer unlock()
	}
	var attached *LaunchedBrowser
	if pid, ok := foreignHolder(udd); ok {
		var err error
		if attached, err = attachForeign(key, udd, homeKey, pid); err != nil {
			return nil, err.Error()
		}
		if attached == nil {
			return nil, foreignUnreachable(udd)
		}
	} else {
		var refused string
		if attached, refused = attachLocal(env, key, udd, homeKey, opts.Timeout); refused != "" {
			return nil, refused
		}
	}
	if attached == nil {
		if opts.Prepare != nil {
			if failure := opts.Prepare(); failure != "" {
				return nil, failure
			}
		}
		return launchOwned(env, key, executable, udd, false, packaged, homeKey, opts)
	}
	launcherMu.Lock()
	browsers.set(key, attached)
	launcherMu.Unlock()
	return attached, ""
}

func keyLock(key string) *sync.Mutex {
	launcherMu.Lock()
	defer launcherMu.Unlock()
	l, ok := keyLocks[key]
	if !ok {
		l = &sync.Mutex{}
		keyLocks[key] = l
	}
	return l
}

// Starts or reuses the key's Chromium, a shared profile with a live browser attached and never killed
func launchChromium(env *tools.Env, key string, opts LaunchOptions) (*LaunchedBrowser, string) {
	l := keyLock(key)
	l.Lock()
	defer l.Unlock()
	if existing := getBrowser(key); existing != nil {
		return existing, ""
	}
	homeKey := cfgOf(env).homeKey()
	launcherMu.Lock()
	servedHomes[homeKey] = true
	launcherMu.Unlock()
	executable := opts.Executable
	packaged := executable == ""
	if packaged {
		if ok, hint := chromiumReady(env); !ok {
			return nil, hint
		}
		executable = launcherResolveExecutable(env)
	}
	udd := opts.UserDataDir
	if udd == "" {
		udd = defaultUserDataDir(key, homeKey)
	}
	if opts.Ephemeral {
		return launchOwned(env, key, executable, udd, true, packaged, homeKey, opts)
	}
	return startShared(env, key, executable, udd, homeKey, packaged, opts)
}

// Spawns a Chromium this process owns, recorded before its readiness wait so a crash leaves it reapable
func launchOwned(env *tools.Env, key, executable, udd string, ephemeral, packaged bool, homeKey string, opts LaunchOptions) (b *LaunchedBrowser, failure string) {
	procEnv := opts.ProcEnv
	if procEnv == nil {
		procEnv = buildBrowserEnv(env)
	}
	if err := prepareDir(udd, ephemeral); err != nil {
		return nil, fmt.Sprintf("Could not create the browser profile directory %s: %s", udd, err)
	}
	argv := buildChromiumArgv(env, executable, udd, packaged, resolveHeaded(env, opts.Headed, procEnv), opts.SandboxBypass, opts.ExtraArgs)
	p, err := launcherSpawn(argv, procEnv, udd)
	if err != nil {
		return nil, fmt.Sprintf("Failed to launch Chromium (%s): %s", executable, err)
	}
	pid := p.pid()
	startTime := startTimeOf(pid)
	starting := makeBrowser(key, 0, "", int64p(pid), startTime, udd, true, ephemeral, homeKey)
	launcherMu.Lock()
	launching.set(strconv.Itoa(pid), starting)
	launcherMu.Unlock()
	writeRecord(starting)
	settled := false
	defer func() {
		if !settled {
			abandonLaunch(p, starting)
		}
	}()
	port, path, failure := waitForDevtools(p, udd, time.Now().Add(launchTimeout(env, opts.Timeout)))
	if failure != "" {
		return nil, failure
	}
	settled = true
	b = makeBrowser(key, port, path, int64p(pid), startTime, udd, true, ephemeral, homeKey)
	writeRecord(b)
	launcherMu.Lock()
	launching.del(strconv.Itoa(pid))
	browsers.set(key, b)
	procs[key] = p
	launcherMu.Unlock()
	launcherLogger().Info(fmt.Sprintf("browser: launched Chromium pid %d for %s (port %d)", pid, key, port))
	return b, ""
}

func abandonLaunch(p *launchProc, starting *LaunchedBrowser) {
	launcherMu.Lock()
	launching.del(strconv.Itoa(p.pid()))
	launcherMu.Unlock()
	if _, exited := p.poll(); !exited {
		launcherTerminate(starting.PID, starting.StartTime, p)
	}
	removeRecord(starting)
	if starting.Ephemeral {
		launcherRemoveAll(starting.UserDataDir)
	}
}

// Caller holds launcherMu
func alive(b *LaunchedBrowser) bool {
	if p := procs[b.Key]; p != nil && b.Owned {
		_, exited := p.poll()
		return !exited
	}
	if b.PID == nil || !launcherPidAlive(int(*b.PID)) {
		return false
	}
	return b.StartTime == nil || samePtr(startTimeOf(int(*b.PID)), b.StartTime)
}

// The key's live browser, a dead one dropped with its leftovers
func getBrowser(key string) *LaunchedBrowser {
	launcherMu.Lock()
	b := browsers.get(key)
	if b == nil {
		launcherMu.Unlock()
		return nil
	}
	if !b.Foreign {
		defer launcherMu.Unlock()
		if alive(b) {
			return b
		}
		launcherLogger().Info(fmt.Sprintf("browser: Chromium for %s (pid %s) is gone", key, pidText(b.PID)))
		forgetLocked(key, b, true)
		return nil
	}
	launcherMu.Unlock()
	if foreignAlive(b) {
		return b
	}
	launcherMu.Lock()
	defer launcherMu.Unlock()
	if browsers.get(key) == b {
		launcherLogger().Info(fmt.Sprintf("browser: the Chromium another environment ran for %s is gone", key))
		forgetLocked(key, b, true)
	}
	return nil
}

// Alive while its profile still names this endpoint and the endpoint answers
func foreignAlive(b *LaunchedBrowser) bool {
	port, path, found, _ := readDevtoolsFile(b.UserDataDir)
	return found && b.CDPURL == fmt.Sprintf("ws://127.0.0.1:%d%s", port, path) && launcherEndpointLive(port, path)
}

// Another key of this process on the same shared Chromium that inherits its hold, caller holds launcherMu
func heirKeyLocked(b *LaunchedBrowser) (string, bool) {
	if b.Ephemeral {
		return "", false
	}
	for _, k := range browsers.keys {
		other := browsers.m[k]
		if other != b && !other.Ephemeral && sameBrowser(other, b) {
			return k, true
		}
	}
	return "", false
}

// One launch by profile, pid, start time and launch record, a foreign one also by its endpoint
func sameBrowser(a, b *LaunchedBrowser) bool {
	return a.Foreign == b.Foreign && samePtr(a.PID, b.PID) && samePtr(a.StartTime, b.StartTime) &&
		recordName(a) == recordName(b) && sameProfile(a.UserDataDir, b.UserDataDir) && (!a.Foreign || a.CDPURL == b.CDPURL)
}

// Drops the key's browser, its lease unless an heir inherits it, and an owned one's record, caller holds launcherMu
func forgetLocked(key string, b *LaunchedBrowser, dropRecord bool) {
	var p *launchProc
	heir, hasHeir := heirKeyLocked(b)
	if browsers.get(key) == b {
		browsers.del(key)
		p = procs[key]
		delete(procs, key)
	}
	if p != nil && hasHeir {
		if _, ok := procs[heir]; !ok {
			procs[heir] = p
		}
	}
	if !b.Ephemeral && !hasHeir {
		if record := recordPath(b); record != "" {
			dropLease(record)
		}
	}
	if b.Owned && dropRecord {
		removeRecord(b)
		if b.Ephemeral {
			launcherRemoveAll(b.UserDataDir)
		}
	}
}

// Parent first teardown, the browser alone then its snapshot survivors, then its exit awaited
func terminateBrowser(pid, startTime *int64, p *launchProc) {
	if pid == nil || *pid == 0 {
		return
	}
	tools.TerminateHostPid(nil, int(*pid), startTime)
	if p != nil && !p.wait(5*time.Second) {
		p.cmd.Process.Kill()
	}
}

// Lets go of the key's browser, a shared one stopped only by its last holder across processes
func stopBrowser(key string) {
	l := keyLock(key)
	l.Lock()
	defer l.Unlock()
	stopLocked(key)
}

func stopLocked(key string) {
	launcherMu.Lock()
	b := browsers.get(key)
	p := procs[key]
	launcherMu.Unlock()
	if b == nil {
		return
	}
	record := ""
	if !b.Ephemeral {
		record = recordPath(b)
	}
	if record == "" {
		if b.Owned {
			launcherTerminate(b.PID, b.StartTime, p)
		}
		launcherMu.Lock()
		forgetLocked(key, b, true)
		launcherMu.Unlock()
		return
	}
	_, unlock, err := recordsLock(filepath.Dir(record), true)
	if err != nil {
		launcherLogger().Error(fmt.Sprintf("browser: could not stop the Chromium for %s: %s", key, err))
		return
	}
	defer unlock()
	launcherMu.Lock()
	_, hasHeir := heirKeyLocked(b)
	launcherMu.Unlock()
	if !hasHeir && lastHolderOut(record, b) {
		launcherTerminate(b.PID, b.StartTime, p)
		os.Remove(record)
	}
	launcherMu.Lock()
	forgetLocked(key, b, false)
	launcherMu.Unlock()
}

// Drops this process's hold, true when no other live process holds it and a launch record names it
func lastHolderOut(record string, b *LaunchedBrowser) bool {
	dropLease(record)
	if b.Foreign {
		return false
	}
	others := otherLiveHolders(record, optInt(b.StartTime))
	var data *pyjson.Object
	if lexists(record) {
		data = loadStateFile(record)
	}
	if data != nil && !recordIs(data, b) {
		data = nil
	}
	heldElsewhere := data != nil && !heldHere(data, "owner_pid") && ownerAlive(data)
	if heir := successor(others); heir != nil && data != nil && !heldElsewhere {
		if err := handOver(record, data, heir); err != nil {
			launcherLogger().Debug(fmt.Sprintf("browser: could not hand over %s: %s", record, err))
		}
	}
	if len(others) > 0 || heldElsewhere {
		return false
	}
	return b.Owned || data != nil
}

// Stops every browser this process launched, idempotent
func stopAllBrowsers() {
	launcherMu.Lock()
	keys := append([]string(nil), browsers.keys...)
	launcherMu.Unlock()
	for _, key := range keys {
		stopBrowser(key)
	}
}

// The devtools browser path of the endpoint, new per launch and absent before it is up
func devtoolsID(b *LaunchedBrowser) (string, bool) {
	at := strings.Index(b.CDPURL, "/devtools/browser/")
	if at < 0 {
		return "", false
	}
	return b.CDPURL[at:], true
}

func devtoolsIDValue(b *LaunchedBrowser) any {
	if id, ok := devtoolsID(b); ok {
		return id
	}
	return nil
}

// The record describes this launch, by start time here or by another namespace's DevTools id for a foreign one
func recordIs(record *pyjson.Object, b *LaunchedBrowser) bool {
	if !pyrepr.Equal(objGet(record, "pid"), optInt(b.PID)) || !sameProfile(objGet(record, "user_data_dir"), b.UserDataDir) {
		return false
	}
	if b.Foreign {
		devtools := objGet(record, "devtools")
		return !sameNamespace(record) && devtools != nil && pyrepr.Equal(devtools, devtoolsIDValue(b))
	}
	return pyrepr.Equal(objGet(record, "start_time"), optInt(b.StartTime))
}

// A shared profile's own private dir of launch record, leases and lock
func sharedStateDir(userDataDir string) string {
	root := privateDir(sharedProfilesRoot())
	if root == "" {
		return ""
	}
	return privateDir(filepath.Join(root, profileDigest(userDataDir)[:32]))
}

func stateDir(b *LaunchedBrowser) string {
	if b.Ephemeral {
		return privateDir(recordsDir(b.HomeKey))
	}
	return sharedStateDir(b.UserDataDir)
}

func recordPath(b *LaunchedBrowser) string {
	records := stateDir(b)
	if records == "" {
		return ""
	}
	return filepath.Join(records, recordName(b))
}

// The record the leases are keyed by, the bound one or this namespace's name
func recordName(b *LaunchedBrowser) string {
	if b.RecordName != "" {
		return b.RecordName
	}
	return fmt.Sprintf("%s-%s%s.json", profileDigest(b.UserDataDir)[:16], pidText(b.PID), nsTag())
}

func writeRecord(b *LaunchedBrowser) {
	me := os.Getpid()
	record := pyjson.NewObject()
	record.Set("owner_pid", int64(me))
	record.Set("owner_start_time", optStartTime(launcherStartTime(me)))
	record.Set("pid", optInt(b.PID))
	record.Set("start_time", optInt(b.StartTime))
	record.Set("user_data_dir", b.UserDataDir)
	record.Set("home_key", b.HomeKey)
	record.Set("ephemeral", b.Ephemeral)
	record.Set("ns", nsValue())
	record.Set("devtools", devtoolsIDValue(b))
	if path := recordPath(b); path != "" {
		if err := config.AtomicJSONWriteWith(path, record); err != nil {
			launcherLogger().Debug(fmt.Sprintf("browser: could not write launch record for pid %s: %s", pidText(b.PID), err))
		}
	}
}

func removeRecord(b *LaunchedBrowser) {
	if path := recordPath(b); path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			launcherLogger().Debug(fmt.Sprintf("browser: could not remove launch record for pid %s: %s", pidText(b.PID), err))
		}
	}
}

// The record's owner is alive or cannot be proven gone because another PID namespace wrote it
func ownerAlive(record *pyjson.Object) bool {
	if !sameNamespace(record) {
		return true
	}
	owner, ok := jsonInt(objGet(record, "owner_pid"))
	if !ok || !launcherPidAlive(int(owner)) {
		return false
	}
	expected := objGet(record, "owner_start_time")
	return expected == nil || pyrepr.Equal(optStartTime(launcherStartTime(int(owner))), expected)
}

// The pid's argv carries a --user-data-dir naming the dir however either is spelled
func cmdlineNamesDir(pid int, userDataDir string) bool {
	argv, err := launcherCmdline(pid)
	if err != nil {
		return false
	}
	for _, tok := range argv {
		if strings.HasPrefix(tok, "--user-data-dir=") && sameProfile(strings.SplitN(tok, "=", 2)[1], userDataDir) {
			return true
		}
	}
	return false
}

// Strictly inside the home's throwaway profile root, never for a shared profile state dir
func insideEphemeralRoot(userDataDir, homeDir string) bool {
	if homeDir == "" {
		return false
	}
	dir, err := realpathLax(userDataDir)
	if err != nil {
		return false
	}
	root, err := realpathLax(ephemeralRoot(homeDir))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// A live pid proven to be the recorded browser by start time and argv, never by argv alone
func isRecordedBrowser(record *pyjson.Object, homeDir string) bool {
	pid, ok := jsonInt(objGet(record, "pid"))
	udd, isStr := objGet(record, "user_data_dir").(string)
	if !ok || !isStr || !launcherPidAlive(int(pid)) {
		return false
	}
	if pyrepr.Truthy(objGet(record, "ephemeral")) && !insideEphemeralRoot(udd, homeDir) {
		launcherLogger().Debug(fmt.Sprintf("browser: ignoring launch record for pid %d: %s is not a launcher profile dir", pid, udd))
		return false
	}
	expected := objGet(record, "start_time")
	if expected == nil || !pyrepr.Equal(optStartTime(launcherStartTime(int(pid))), expected) {
		return false
	}
	if !launcherNamesDir(int(pid), udd) {
		launcherLogger().Warn(fmt.Sprintf("browser: refusing to reap pid %d: its command line does not name %s", pid, udd))
		return false
	}
	return true
}

// A browser of this namespace in use here, launched or still starting, that the record describes
func trackedBrowser(record *pyjson.Object) *LaunchedBrowser {
	launcherMu.Lock()
	defer launcherMu.Unlock()
	for _, b := range append(browsers.values(), launching.values()...) {
		if !b.Foreign && recordIs(record, b) {
			return b
		}
	}
	return nil
}

// Takes over a crashed owner's record of a browser this process now uses, only the owner changing
func adoptRecord(b *LaunchedBrowser) error {
	path := recordPath(b)
	if path == "" || !lexists(path) {
		return nil
	}
	record := loadStateFile(path)
	if record == nil || !recordIs(record, b) || ownerAlive(record) {
		return nil
	}
	me := os.Getpid()
	adopted := record.Copy()
	adopted.Set("owner_pid", int64(me))
	adopted.Set("owner_start_time", optStartTime(launcherStartTime(me)))
	return config.AtomicJSONWriteWith(path, adopted)
}

// Reaps one record, caller holding the records lock
func reapRecord(path, homeDir string) bool {
	record := loadStateFile(path)
	if record == nil || !sameNamespace(record) {
		return false
	}
	if tracked := trackedBrowser(record); tracked != nil {
		if err := adoptRecord(tracked); err != nil {
			launcherLogger().Error(fmt.Sprintf("browser: could not adopt launch record %s: %s", path, err))
		}
		return false
	}
	if !pyrepr.Equal(objGet(record, "owner_pid"), int64(os.Getpid())) && ownerAlive(record) {
		return false
	}
	if others := otherLiveHolders(path, objGet(record, "start_time")); len(others) > 0 {
		if heir := successor(others); heir != nil {
			if err := handOver(path, record, heir); err != nil {
				launcherLogger().Debug(fmt.Sprintf("browser: could not hand over %s: %s", path, err))
			}
		}
		return false
	}
	reaped := isRecordedBrowser(record, homeDir)
	if reaped {
		launcherTerminate(jsonIntPtr(objGet(record, "pid")), jsonIntPtr(objGet(record, "start_time")), nil)
		launcherLogger().Info(fmt.Sprintf("browser: reaped orphaned Chromium pid %s (%s)", pyrepr.Str(objGet(record, "pid")),
			pyrepr.Str(objGet(record, "user_data_dir"))))
	}
	udd, isStr := objGet(record, "user_data_dir").(string)
	if pyrepr.Truthy(objGet(record, "ephemeral")) && isStr && insideEphemeralRoot(udd, homeDir) && (reaped || !browserRunningOn(udd)) {
		launcherRemoveAll(udd)
	}
	os.Remove(path)
	return reaped
}

func reapHome(homeDir string) int { return reapStateDir(recordsDir(homeDir), homeDir) }

// Every shared profile's records whichever home launched its browser
func reapSharedProfiles() int {
	root := sharedProfilesRoot()
	if !lexists(root) || !isPrivateDir(root) {
		return 0
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		launcherLogger().Debug(fmt.Sprintf("browser: shared-profile launcher state unavailable in %s: %s", root, err))
		return 0
	}
	reaped := 0
	for _, e := range entries {
		reaped += reapStateDir(filepath.Join(root, e.Name()), "")
	}
	return reaped
}

// Reaps one launcher state dir, a shared profile whose lock is taken left to the next pass
func reapStateDir(records, homeDir string) int {
	if !lexists(records) {
		return 0
	}
	if !isPrivateDir(records) {
		launcherLogger().Debug(fmt.Sprintf("browser: not reaping from %s: not a private directory of this user", records))
		return 0
	}
	locked, unlock, err := recordsLock(records, homeDir != "")
	if err != nil {
		launcherLogger().Error(fmt.Sprintf("browser: could not lock %s for reaping: %s", records, err))
		return 0
	}
	if !locked {
		launcherLogger().Debug(fmt.Sprintf("browser: %s is busy (a launch or stop holds it); reaping it next pass", records))
		return 0
	}
	defer unlock()
	paths, err := filepath.Glob(filepath.Join(globEscape(records), "*.json"))
	if err != nil {
		launcherLogger().Debug(fmt.Sprintf("browser: launch records unavailable in %s: %s", records, err))
		return 0
	}
	sort.Strings(paths)
	reaped := 0
	for _, path := range paths {
		if reapRecord(path, homeDir) {
			reaped++
		}
	}
	dropDeadLeases(records)
	return reaped
}

func globEscape(path string) string {
	r := strings.NewReplacer(`*`, `[*]`, `?`, `[?]`, `[`, `[[]`)
	if isWindowsHost {
		return r.Replace(path)
	}
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(path)
}

// Removes the home's throwaway profiles whose owner is gone, never one of a live owner or another namespace
func reclaimThrowawayDirs(homeDir string) {
	root := ephemeralRoot(homeDir)
	if !lexists(root) || !isPrivateDir(root) {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		launcherLogger().Debug(fmt.Sprintf("browser: throwaway profiles unavailable in %s: %s", root, err))
		return
	}
	for _, e := range entries {
		m := throwawayDirRE.FindStringSubmatch(e.Name())
		if m == nil || m[3] != nsTag() {
			continue
		}
		owner, err := strconv.Atoi(m[1])
		if err == nil && owner == os.Getpid() {
			continue
		}
		var started any
		if m[2] != "" {
			if v, err := strconv.ParseInt(m[2], 10, 64); err == nil {
				started = v
			}
		}
		if err == nil && processAlive(int64(owner), started) {
			continue
		}
		entry := filepath.Join(root, e.Name())
		if browserRunningOn(entry) {
			launcherLogger().Debug(fmt.Sprintf("browser: keeping the throwaway profile %s: a Chromium still runs on it", entry))
			continue
		}
		launcherRemoveAll(entry)
		launcherLogger().Info(fmt.Sprintf("browser: removed the throwaway profile %s of an Icebath process that is gone", entry))
	}
}

// The launch profile, every profile this process launched a browser for, and the given homes, sorted
func servedHomeKeys(homes []string) []string {
	launcherMu.Lock()
	keys := map[string]bool{}
	for k := range servedHomes {
		keys[k] = true
	}
	launcherMu.Unlock()
	keys[home.Key(home.Process())] = true
	for _, h := range homes {
		keys[home.Key(h)] = true
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Stops Chromium of owners that are gone in every served profile and reclaims their throwaway profiles
func reapOrphanedBrowsers(homes []string) int {
	reaped := reapSharedProfiles()
	for _, h := range servedHomeKeys(homes) {
		reaped += reapHome(h)
		reclaimThrowawayDirs(h)
	}
	return reaped
}
