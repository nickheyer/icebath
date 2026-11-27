package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/proc"
)

const (
	devtoolsFile          = "DevToolsActivePort"
	launchLog             = "icebath-chromium.log"
	minFirstLaunch        = 120 * time.Second
	unresponsiveWait      = 10 * time.Second
	launchPoll            = 100 * time.Millisecond
	logTailChars          = 1500
	sharedProfilesDirname = "browser-shared-profiles"
)

var (
	packagedFlags = []string{"--password-store=basic"}
	macFlags      = []string{"--use-mock-keychain"}
)

// Names of throwaway profiles: digest, owner pid, its start time and the namespace tag
var throwawayName = regexp.MustCompile(`^[0-9a-f]{16}-(\d+)(?:-(\d+))?(-n[0-9a-f]{12})?$`)

// A Chromium this process launched or attached to on a shared profile
type launched struct {
	key string
	pid int
	// Start time of the process, nil when it could not be read
	start     *int64
	cdp       string
	http      string
	dir       string
	owned     bool
	ephemeral bool
	// Folder of the profile it serves
	profile string
	// Record file the holds are keyed by when this process did not name it
	recordName string
	// Held from another PID namespace, so its pid is never signalled and its endpoint says whether it lives
	foreign bool
}

// What a launch starts or attaches to, an empty program running the packaged Chromium
type launchOptions struct {
	exe       string
	dir       string
	ephemeral bool
	headed    *bool
	extra     []string
	env       map[string]string
	timeout   time.Duration
	bypass    bool
	// Readies the profile before a launch, the failure it says ending it
	prepare func() string
	// The copy of the user's own profile, which keeps its browser sign in
	userProfile bool
}

// A Chromium started here whose exit a goroutine reaps
type child struct {
	cmd  *exec.Cmd
	done chan struct{}
	code int
}

func (p *child) poll() (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	default:
		return 0, false
	}
}

func (p *child) wait(timeout time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Ordered table of browsers by key
type table struct {
	keys []string
	m    map[string]*launched
}

func (t *table) get(key string) *launched { return t.m[key] }

func (t *table) set(key string, b *launched) {
	if t.m == nil {
		t.m = map[string]*launched{}
	}
	if _, ok := t.m[key]; !ok {
		t.keys = append(t.keys, key)
	}
	t.m[key] = b
}

func (t *table) del(key string) {
	if _, ok := t.m[key]; !ok {
		return
	}
	delete(t.m, key)
	t.keys = slices.DeleteFunc(t.keys, func(k string) bool { return k == key })
}

func (t *table) values() []*launched {
	out := make([]*launched, 0, len(t.keys))
	for _, k := range t.keys {
		out = append(out, t.m[k])
	}
	return out
}

// Browsers this process launched or leased, those still starting by pid, and the profiles it served
type launcher struct {
	mu          sync.Mutex
	browsers    table
	procs       map[string]*child
	starting    table
	locks       map[string]*sync.Mutex
	autoinstall bool
	served      map[string]*config.Config
}

func newLauncher() launcher {
	return launcher{procs: map[string]*child{}, locks: map[string]*sync.Mutex{}, served: map[string]*config.Config{}}
}

// Throwaway profile folder of the key: under the profile, named by key, owner pid, its start time and the namespace
func throwawayDir(cfg *config.Config, key string) string {
	sum := sha256.Sum256([]byte(cfg.Profile() + "\x00" + key))
	pid := os.Getpid()
	owner := strconv.Itoa(pid)
	if started := startOf(pid); started != nil {
		owner = fmt.Sprintf("%d-%d", pid, *started)
	}
	return filepath.Join(ephemeralRoot(profileDir(cfg)), hex.EncodeToString(sum[:])[:16]+"-"+owner+nsTag())
}

func ephemeralRoot(dir string) string { return filepath.Join(dir, "cache", "browser", "chromium") }
func recordsDir(dir string) string    { return filepath.Join(dir, "cache", "browser", "chromium-records") }

// How long a launch waits: the one given, else the command timeout and never under the first launch minimum
func launchTimeout(cfg *config.Config, given time.Duration) time.Duration {
	if given > 0 {
		return given
	}
	return max(commandTimeout(cfg), minFirstLaunch)
}

// Headless unless the profile shows the window, and a Linux machine without a display always headless
func showWindow(cfg *config.Config, given *bool, env map[string]string) bool {
	want := headed(cfg)
	if given != nil {
		want = *given
	}
	if !want {
		return false
	}
	return env["DISPLAY"] != "" || env["WAYLAND_DISPLAY"] != "" || runtime.GOOS != "linux"
}

// Launch argv from its inputs, the sandbox facts of the host and browser.chromium_args
func chromiumArgv(cfg *config.Config, exe, dir string, packaged, windowed, bypass bool, extra []string) []string {
	argv := append([]string{exe, "--remote-debugging-port=0", "--user-data-dir=" + dir}, browser.QuietArgs()...)
	if packaged {
		argv = append(argv, packagedFlags...)
		if runtime.GOOS == "darwin" {
			argv = append(argv, macFlags...)
		}
	}
	if !windowed {
		argv = append(argv, "--headless=new")
	}
	configured := chromiumArgs(cfg)
	if bypass && browser.Bypass() {
		for _, flag := range browser.BypassArgs {
			if !slices.Contains(configured, flag) {
				argv = append(argv, flag)
			}
		}
	}
	argv = append(argv, extra...)
	argv = append(argv, configured...)
	return append(browser.MergeFeatures(argv), "about:blank")
}

func newLaunched(key string, port int, path string, pid int, start *int64, dir string, owned, ephemeral bool, profile string) *launched {
	return &launched{key: key, pid: pid, start: start, cdp: fmt.Sprintf("ws://127.0.0.1:%d%s", port, path), http: fmt.Sprintf("http://127.0.0.1:%d", port),
		dir: dir, owned: owned, ephemeral: ephemeral, profile: profile}
}

// A real folder of this user closed to group and other writes, Windows checking only that it is a folder
func isPrivateDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || ownedClosed(info)
}

// The folder made 0700 when missing, empty when one that is there is not private to this user
func privateDir(path string) string {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return ""
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return ""
	}
	if isPrivateDir(path) {
		return path
	}
	slog.Debug("browser: not using " + path + " for launcher state: not a private directory of this user")
	return ""
}

// Root of the launcher state of shared profiles of this user, across every icebath of the machine
func sharedRoot() string {
	state := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(state) {
		home, _ := os.UserHomeDir()
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "icebath", "locks", sharedProfilesDirname)
}

// One digest per profile folder however it is spelled
func profileDigest(dir string) string {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolved = dir
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:])
}

// DevTools port and browser path once Chromium wrote both lines, false before
func readDevtools(dir string) (int, string, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, devtoolsFile))
	if err != nil {
		return 0, "", false
	}
	lines := strings.SplitN(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"), "\n", 3)
	for len(lines) < 2 {
		lines = append(lines, "")
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	path := strings.TrimSpace(lines[1])
	if err != nil || port <= 0 || !strings.HasPrefix(path, "/devtools/browser/") {
		return 0, "", false
	}
	return port, path, true
}

// Last characters of the launch log
func logTail(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, launchLog))
	if err != nil {
		return ""
	}
	if len(raw) > logTailChars {
		raw = raw[len(raw)-logTailChars:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(raw), "�"))
}

// Host and pid Chromium wrote into the profile's SingletonLock link
func singletonOwner(dir string) (string, int, bool) {
	target, err := os.Readlink(filepath.Join(dir, "SingletonLock"))
	if err != nil {
		return "", 0, false
	}
	host, pidText := "", target
	if i := strings.LastIndexByte(target, '-'); i >= 0 {
		host, pidText = target[:i], target[i+1:]
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return "", 0, false
	}
	return host, pid, true
}

// Record names of one profile's browser pid from any namespace, sorted
func recordNames(records, dir string, pid int) []string {
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(fmt.Sprintf("%s-%d", profileDigest(dir)[:16], pid)) + `(-n[0-9a-f]{12})?\.json$`)
	entries, _ := os.ReadDir(records)
	var names []string
	for _, e := range entries {
		if pattern.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names
}

var devtoolsProbe = &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: loopbackProxy}}

// Whether the port answers /json/version with the browser the DevToolsActivePort file names
func devtoolsLive(port int, path string) bool {
	resp, err := devtoolsProbe.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	return json.Unmarshal(body, &version) == nil && strings.HasSuffix(version.WebSocketDebuggerURL, path)
}

// A Chromium is on disk, the package manager's pinned one installed once a process when none is and the profile allows it; else what to do
func (r *registrant) chromiumReady(ctx context.Context, cfg *config.Config) (bool, string) {
	if r.browser.Resolved(cfg) != "" {
		return true, ""
	}
	r.launcher.mu.Lock()
	tried := r.launcher.autoinstall
	r.launcher.autoinstall = true
	r.launcher.mu.Unlock()
	pinned := r.browser.Executable(cfg, true) != r.browser.Executable(cfg, false)
	if !tried && !browser.Docker() && packages.CurrentTarget() != packages.Bionic && !pinned && r.packages.LazyAllowed(cfg) {
		slog.Info("browser: installing PM's pinned Chromium")
		if _, err := r.packages.Ensure(ctx, cfg, browser.Chromium, packages.Ensuring{}); err != nil {
			slog.Warn("browser: Chromium auto-install failed: " + err.Error())
		} else if r.browser.Resolved(cfg) != "" {
			return true, ""
		}
	}
	return false, r.browser.MissingHint(cfg)
}

// DevTools endpoint once written, or why Chromium exited or never showed one before the deadline
func waitForDevtools(p *child, dir string, deadline time.Time) (int, string, string) {
	for {
		if port, path, found := readDevtools(dir); found {
			return port, path, ""
		}
		if rc, exited := p.poll(); exited {
			tail := logTail(dir)
			hint := ""
			if strings.Contains(strings.ToLower(tail), "sandbox") {
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
		time.Sleep(launchPoll)
	}
}

// Pid of the Chromium holding the folder: its SingletonLock, or on Windows the browser whose command line names it
func singletonPid(dir string) (int, bool) {
	if _, pid, ok := singletonOwner(dir); ok {
		return pid, true
	}
	if runtime.GOOS == "windows" {
		return browser.Holder(dir)
	}
	return 0, false
}

// SingletonLock pid of a Chromium on the folder that runs where this process can't verify it, another machine or PID namespace
func foreignHolder(dir string) (int, bool) {
	host, pid, ok := singletonOwner(dir)
	if !ok {
		return 0, false
	}
	if terminal.Running(pid) && browser.NamesProfile(pid, dir) {
		return 0, false
	}
	hostname, _ := os.Hostname()
	if host != hostname || foreignRecorded(dir, pid) {
		return pid, true
	}
	return 0, false
}

// Whether a launch record of another PID namespace names the pid as the folder's current or still starting browser
func foreignRecorded(dir string, pid int) bool {
	state := sharedStateDir(dir)
	if state == "" {
		return false
	}
	_, path, found := readDevtools(dir)
	for _, name := range recordNames(state, dir, pid) {
		var rec record
		if !loadState(filepath.Join(state, name), &rec) || rec.PID != pid || sameNamespace(rec.NS) || !browser.SameProfile(rec.Dir, dir) {
			continue
		}
		if rec.Devtools == nil || found && *rec.Devtools == path {
			return true
		}
	}
	return false
}

// Port, browser path and pid of a live Chromium that serves DevTools on the folder
func runningInstance(dir string) (int, string, int, bool) {
	pid, held := singletonPid(dir)
	port, path, found := readDevtools(dir)
	if !held || !found || !terminal.Running(pid) || !devtoolsLive(port, path) {
		return 0, "", 0, false
	}
	return port, path, pid, true
}

// Removes a tree as rmtree ignoring errors, never following a link
func removeTree(path string) {
	if info, err := os.Lstat(path); err != nil || info.Mode()&fs.ModeSymlink != 0 {
		return
	}
	os.RemoveAll(path)
}

// A fresh folder for a throwaway profile, the one there for a shared one, either without a stale DevToolsActivePort
func prepareDir(dir string, ephemeral bool) error {
	if ephemeral {
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return err
		}
		removeTree(dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	os.Remove(filepath.Join(dir, devtoolsFile))
	return nil
}

// Starts argv detached, its output in the profile's launch log
func spawn(argv []string, env map[string]string, dir string) (*child, error) {
	log, err := os.Create(filepath.Join(dir, launchLog))
	if err != nil {
		return nil, err
	}
	defer log.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = terminal.Environ(env)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = detached()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &child{cmd: cmd, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		p.code = proc.ExitCode(cmd.ProcessState)
		close(p.done)
	}()
	return p, nil
}

// The live Chromium already on a shared profile, leased; the profile's lock held
func attachRunning(key, dir, profile string) (*launched, error) {
	port, path, pid, ok := runningInstance(dir)
	if !ok {
		return nil, nil
	}
	slog.Info(fmt.Sprintf("browser: attaching to the Chromium already running on %s (pid %d)", dir, pid))
	b := newLaunched(key, port, path, pid, startOf(pid), dir, false, false, profile)
	if records := stateDir(b); records != "" {
		bindRecord(records, b)
		if err := adoptRecord(b); err != nil {
			return nil, err
		}
		if err := writeHold(recordPath(b), b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Binds the browser to the record of this launch of it in any namespace, this namespace's first
func bindRecord(records string, b *launched) {
	own := recordName(b)
	names := recordNames(records, b.dir, b.pid)
	slices.SortStableFunc(names, func(x, y string) int {
		switch {
		case x == own && y != own:
			return -1
		case y == own && x != own:
			return 1
		}
		return 0
	})
	for _, name := range names {
		var rec record
		if loadState(filepath.Join(records, name), &rec) && recordIs(rec, b) {
			b.recordName = name
			if b.foreign {
				b.start = rec.Start
			}
			return
		}
	}
}

// The Chromium another environment runs on the folder, leased when its endpoint answers here, never adopted
func attachForeign(key, dir, profile string, pid int) (*launched, error) {
	port, path, found := readDevtools(dir)
	if !found || !devtoolsLive(port, path) {
		return nil, nil
	}
	slog.Info(fmt.Sprintf("browser: attaching to the Chromium another environment runs on %s (its pid %d there)", dir, pid))
	b := newLaunched(key, port, path, pid, nil, dir, false, false, profile)
	b.foreign = true
	if records := stateDir(b); records != "" {
		bindRecord(records, b)
		if err := writeHold(recordPath(b), b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Pid of this environment's live Chromium on the folder, false for none or a stale lock
func liveLocalHolder(dir string) (int, bool) {
	pid, ok := singletonPid(dir)
	if ok && terminal.Running(pid) && browser.NamesProfile(pid, dir) {
		return pid, true
	}
	return 0, false
}

// Attaches to this environment's Chromium on the folder, waiting out one that stopped answering before refusing it
func attachLocal(cfg *config.Config, key, dir string, timeout time.Duration) (*launched, string) {
	attached, err := attachRunning(key, dir, profileDir(cfg))
	if err != nil {
		return nil, err.Error()
	}
	pid, held := 0, false
	if attached == nil {
		pid, held = liveLocalHolder(dir)
	}
	if !held {
		return attached, ""
	}
	deadline := time.Now().Add(min(unresponsiveWait, launchTimeout(cfg, timeout)))
	delay := 250 * time.Millisecond
	for attached == nil {
		if now, ok := liveLocalHolder(dir); !ok || now != pid {
			break
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Sprintf("The browser profile %s is in use by Chromium pid %d, which is not responding on its DevTools endpoint (paused, "+
				"hung, or started without remote debugging). Icebath will not start a second browser on it; try again once it responds, or stop it.",
				dir, pid)
		}
		time.Sleep(min(delay, max(0, time.Until(deadline))))
		delay *= 2
		if attached, err = attachRunning(key, dir, profileDir(cfg)); err != nil {
			return nil, err.Error()
		}
	}
	return attached, ""
}

// Attaches to or launches a shared profile's one Chromium as one step, under the profile's lock across processes
func (r *registrant) startShared(ctx context.Context, cfg *config.Config, key, exe, dir string, packaged bool, opts launchOptions) (*launched, string) {
	if state := sharedStateDir(dir); state != "" {
		_, unlock, err := recordsLock(state, true)
		if err != nil {
			return nil, err.Error()
		}
		defer unlock()
	}
	var attached *launched
	if pid, ok := foreignHolder(dir); ok {
		var err error
		if attached, err = attachForeign(key, dir, profileDir(cfg), pid); err != nil {
			return nil, err.Error()
		}
		if attached == nil {
			return nil, fmt.Sprintf("The browser profile %s is in use by a Chromium in another environment (a container or the host) that this "+
				"process can neither reach nor verify, so Icebath will not start a second browser on it. Stop that browser, or set "+
				"browser.user_data_dir to another directory for this environment; if no browser runs there any more, delete %s.", dir,
				filepath.Join(dir, "SingletonLock"))
		}
	} else {
		var refused string
		if attached, refused = attachLocal(cfg, key, dir, opts.timeout); refused != "" {
			return nil, refused
		}
	}
	if attached == nil {
		if opts.prepare != nil {
			if failure := opts.prepare(); failure != "" {
				return nil, failure
			}
		}
		return r.launchOwned(ctx, cfg, key, exe, dir, false, packaged, opts)
	}
	r.launcher.mu.Lock()
	r.launcher.browsers.set(key, attached)
	r.launcher.mu.Unlock()
	return attached, ""
}

func (r *registrant) keyLock(key string) *sync.Mutex {
	r.launcher.mu.Lock()
	defer r.launcher.mu.Unlock()
	l, ok := r.launcher.locks[key]
	if !ok {
		l = &sync.Mutex{}
		r.launcher.locks[key] = l
	}
	return l
}

// Starts or reuses the key's Chromium; a shared profile's live browser is attached to and never killed by this process
func (r *registrant) launch(ctx context.Context, cfg *config.Config, key string, opts launchOptions) (*launched, string) {
	l := r.keyLock(key)
	l.Lock()
	defer l.Unlock()
	if existing := r.lookup(cfg, key); existing != nil {
		return existing, ""
	}
	r.launcher.mu.Lock()
	r.launcher.served[cfg.Profile()] = cfg
	r.launcher.mu.Unlock()
	exe := opts.exe
	packaged := exe == ""
	if packaged {
		if ok, hint := r.chromiumReady(ctx, cfg); !ok {
			return nil, hint
		}
		exe = r.browser.Resolved(cfg)
	}
	dir := opts.dir
	if dir == "" {
		dir = throwawayDir(cfg, key)
	}
	if opts.ephemeral {
		return r.launchOwned(ctx, cfg, key, exe, dir, true, packaged, opts)
	}
	return r.startShared(ctx, cfg, key, exe, dir, packaged, opts)
}

// Spawns a Chromium this process owns, recorded before its readiness wait so a crash leaves it reapable
func (r *registrant) launchOwned(ctx context.Context, cfg *config.Config, key, exe, dir string, ephemeral, packaged bool, opts launchOptions) (*launched, string) {
	env := opts.env
	if env == nil {
		env = r.childEnv(ctx, cfg)
	}
	if err := prepareDir(dir, ephemeral); err != nil {
		return nil, fmt.Sprintf("Could not create the browser profile directory %s: %s", dir, err)
	}
	if err := browser.Seed(dir, !opts.userProfile); err != nil {
		return nil, fmt.Sprintf("Could not write the preferences that keep the browser from calling home into %s: %s", dir, err)
	}
	argv := chromiumArgv(cfg, exe, dir, packaged, showWindow(cfg, opts.headed, env), opts.bypass, opts.extra)
	p, err := spawn(argv, env, dir)
	if err != nil {
		return nil, fmt.Sprintf("Failed to launch Chromium (%s): %s", exe, err)
	}
	pid := p.cmd.Process.Pid
	start := startOf(pid)
	starting := newLaunched(key, 0, "", pid, start, dir, true, ephemeral, profileDir(cfg))
	r.launcher.mu.Lock()
	r.launcher.starting.set(strconv.Itoa(pid), starting)
	r.launcher.mu.Unlock()
	writeRecord(starting)
	port, path, failure := waitForDevtools(p, dir, time.Now().Add(launchTimeout(cfg, opts.timeout)))
	if failure != "" {
		r.abandon(cfg, p, starting)
		return nil, failure
	}
	b := newLaunched(key, port, path, pid, start, dir, true, ephemeral, profileDir(cfg))
	writeRecord(b)
	r.launcher.mu.Lock()
	r.launcher.starting.del(strconv.Itoa(pid))
	r.launcher.browsers.set(key, b)
	r.launcher.procs[key] = p
	r.launcher.mu.Unlock()
	slog.Info(fmt.Sprintf("browser: launched Chromium pid %d for %s (port %d)", pid, key, port))
	return b, ""
}

func (r *registrant) abandon(cfg *config.Config, p *child, starting *launched) {
	r.launcher.mu.Lock()
	r.launcher.starting.del(strconv.Itoa(p.cmd.Process.Pid))
	r.launcher.mu.Unlock()
	if _, exited := p.poll(); !exited {
		r.terminateBrowser(cfg, starting.pid, starting.start, p)
	}
	removeRecord(starting)
	if starting.ephemeral {
		removeTree(starting.dir)
	}
}

// Whether a browser still runs, as its child or by its pid and start time; the launcher lock held
func (r *registrant) alive(b *launched) bool {
	if p := r.launcher.procs[b.key]; p != nil && b.owned {
		_, exited := p.poll()
		return !exited
	}
	if b.pid <= 0 || !terminal.Running(b.pid) {
		return false
	}
	return b.start == nil || sameStart(startOf(b.pid), b.start)
}

// The key's live browser, a dead one dropped with what it left
func (r *registrant) lookup(cfg *config.Config, key string) *launched {
	r.launcher.mu.Lock()
	b := r.launcher.browsers.get(key)
	if b == nil {
		r.launcher.mu.Unlock()
		return nil
	}
	if !b.foreign {
		defer r.launcher.mu.Unlock()
		if r.alive(b) {
			return b
		}
		slog.Info(fmt.Sprintf("browser: Chromium for %s (pid %d) is gone", key, b.pid))
		r.forget(key, b, true)
		return nil
	}
	r.launcher.mu.Unlock()
	if port, path, found := readDevtools(b.dir); found && b.cdp == fmt.Sprintf("ws://127.0.0.1:%d%s", port, path) && devtoolsLive(port, path) {
		return b
	}
	r.launcher.mu.Lock()
	defer r.launcher.mu.Unlock()
	if r.launcher.browsers.get(key) == b {
		slog.Info("browser: the Chromium another environment ran for " + key + " is gone")
		r.forget(key, b, true)
	}
	return nil
}

// Another key of this process on the same shared Chromium that inherits its hold; the launcher lock held
func (r *registrant) heir(b *launched) (string, bool) {
	if b.ephemeral {
		return "", false
	}
	for _, k := range r.launcher.browsers.keys {
		other := r.launcher.browsers.m[k]
		if other != b && !other.ephemeral && sameBrowser(other, b) {
			return k, true
		}
	}
	return "", false
}

// One launch by profile, pid, start time and record, a foreign one also by its endpoint
func sameBrowser(a, b *launched) bool {
	return a.foreign == b.foreign && a.pid == b.pid && sameStart(a.start, b.start) && recordName(a) == recordName(b) && browser.SameProfile(a.dir, b.dir) &&
		(!a.foreign || a.cdp == b.cdp)
}

// Drops the key's browser, its hold unless an heir takes it, and an owned one's record; the launcher lock held
func (r *registrant) forget(key string, b *launched, dropRecord bool) {
	var p *child
	heir, inherited := r.heir(b)
	if r.launcher.browsers.get(key) == b {
		r.launcher.browsers.del(key)
		p = r.launcher.procs[key]
		delete(r.launcher.procs, key)
	}
	if p != nil && inherited {
		if _, ok := r.launcher.procs[heir]; !ok {
			r.launcher.procs[heir] = p
		}
	}
	if !b.ephemeral && !inherited {
		if path := recordPath(b); path != "" {
			dropHold(path)
		}
	}
	if b.owned && dropRecord {
		removeRecord(b)
		if b.ephemeral {
			removeTree(b.dir)
		}
	}
}

// Stops a browser parent first, then what of its tree survived, its exit waited on
func (r *registrant) terminateBrowser(cfg *config.Config, pid int, start *int64, p *child) {
	if pid <= 0 {
		return
	}
	var started int64
	if start != nil {
		started = *start
	}
	r.terminal.Terminate(cfg, pid, started, true)
	if p != nil && !p.wait(5*time.Second) {
		p.cmd.Process.Kill()
	}
}

// Lets go of the key's browser, a shared one stopped only by its last holder across processes
func (r *registrant) stopBrowser(cfg *config.Config, key string) {
	l := r.keyLock(key)
	l.Lock()
	defer l.Unlock()
	r.launcher.mu.Lock()
	b := r.launcher.browsers.get(key)
	p := r.launcher.procs[key]
	r.launcher.mu.Unlock()
	if b == nil {
		return
	}
	path := ""
	if !b.ephemeral {
		path = recordPath(b)
	}
	if path == "" {
		if b.owned {
			r.terminateBrowser(cfg, b.pid, b.start, p)
		}
		r.launcher.mu.Lock()
		r.forget(key, b, true)
		r.launcher.mu.Unlock()
		return
	}
	_, unlock, err := recordsLock(filepath.Dir(path), true)
	if err != nil {
		slog.Error(fmt.Sprintf("browser: could not stop the Chromium for %s: %s", key, err))
		return
	}
	defer unlock()
	r.launcher.mu.Lock()
	_, inherited := r.heir(b)
	r.launcher.mu.Unlock()
	if !inherited && lastHolderOut(path, b) {
		r.terminateBrowser(cfg, b.pid, b.start, p)
		os.Remove(path)
	}
	r.launcher.mu.Lock()
	r.forget(key, b, false)
	r.launcher.mu.Unlock()
}

// Drops this process's hold, whether no other live process holds the browser and a launch record names it
func lastHolderOut(path string, b *launched) bool {
	dropHold(path)
	if b.foreign {
		return false
	}
	others := otherHolders(path, b.start)
	var rec record
	have := fsx.Lexists(path) && loadState(path, &rec) && recordIs(rec, b)
	elsewhere := have && !(rec.OwnerPID == os.Getpid() && sameNamespace(rec.NS)) && ownerAlive(rec)
	if heir := successor(others); heir != nil && have && !elsewhere {
		if err := handOver(path, rec, heir); err != nil {
			slog.Debug(fmt.Sprintf("browser: could not hand over %s: %s", path, err))
		}
	}
	if len(others) > 0 || elsewhere {
		return false
	}
	return b.owned || have
}

// Stops every browser this process launched
func (r *registrant) stopAll() {
	r.launcher.mu.Lock()
	keys := slices.Clone(r.launcher.browsers.keys)
	r.launcher.mu.Unlock()
	for _, key := range keys {
		r.stopBrowser(r.config, key)
	}
}

// DevTools browser path of the endpoint, new each launch and none before it is up
func devtoolsID(b *launched) *string {
	at := strings.Index(b.cdp, "/devtools/browser/")
	if at < 0 {
		return nil
	}
	id := b.cdp[at:]
	return &id
}

// Whether the record is of this launch: by start time here, by another namespace's DevTools id for a foreign one
func recordIs(rec record, b *launched) bool {
	if rec.PID != b.pid || !browser.SameProfile(rec.Dir, b.dir) {
		return false
	}
	if b.foreign {
		id := devtoolsID(b)
		return !sameNamespace(rec.NS) && rec.Devtools != nil && id != nil && *rec.Devtools == *id
	}
	return sameStart(rec.Start, b.start)
}

// A shared profile's own private folder of launch records, holds and lock
func sharedStateDir(dir string) string {
	root := privateDir(sharedRoot())
	if root == "" {
		return ""
	}
	return privateDir(filepath.Join(root, profileDigest(dir)[:32]))
}

func stateDir(b *launched) string {
	if b.ephemeral {
		return privateDir(recordsDir(b.profile))
	}
	return sharedStateDir(b.dir)
}

func recordPath(b *launched) string {
	records := stateDir(b)
	if records == "" {
		return ""
	}
	return filepath.Join(records, recordName(b))
}

// Record the holds are keyed by: the bound one, else this namespace's name
func recordName(b *launched) string {
	if b.recordName != "" {
		return b.recordName
	}
	return fmt.Sprintf("%s-%d%s.json", profileDigest(b.dir)[:16], b.pid, nsTag())
}

func writeRecord(b *launched) {
	me := os.Getpid()
	rec := record{OwnerPID: me, OwnerStart: startOf(me), PID: b.pid, Start: b.start, Dir: b.dir, Profile: b.profile, Ephemeral: b.ephemeral,
		NS: nsValue(), Devtools: devtoolsID(b)}
	if path := recordPath(b); path != "" {
		if err := writeState(path, rec); err != nil {
			slog.Debug(fmt.Sprintf("browser: could not write launch record for pid %d: %s", b.pid, err))
		}
	}
}

func removeRecord(b *launched) {
	if path := recordPath(b); path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Debug(fmt.Sprintf("browser: could not remove launch record for pid %d: %s", b.pid, err))
		}
	}
}

// Whether the record's owner lives, or can't be proven gone since another PID namespace wrote it
func ownerAlive(rec record) bool {
	if !sameNamespace(rec.NS) {
		return true
	}
	return processAlive(rec.OwnerPID, rec.OwnerStart)
}

// Whether the folder is strictly inside the profile's throwaway root
func insideThrowaway(dir, profile string) bool {
	if profile == "" {
		return false
	}
	resolved := func(p string) string {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return real
		}
		abs, _ := filepath.Abs(p)
		return abs
	}
	rel, err := filepath.Rel(resolved(ephemeralRoot(profile)), resolved(dir))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Whether a live pid is proven the recorded browser by its start time and command line, never by the command line alone
func recordedBrowser(rec record, profile string) bool {
	if rec.PID <= 0 || !terminal.Running(rec.PID) {
		return false
	}
	if rec.Ephemeral && !insideThrowaway(rec.Dir, profile) {
		slog.Debug(fmt.Sprintf("browser: ignoring launch record for pid %d: %s is not a launcher profile dir", rec.PID, rec.Dir))
		return false
	}
	if rec.Start == nil || !sameStart(startOf(rec.PID), rec.Start) {
		return false
	}
	if !browser.NamesProfile(rec.PID, rec.Dir) {
		slog.Warn(fmt.Sprintf("browser: refusing to reap pid %d: its command line does not name %s", rec.PID, rec.Dir))
		return false
	}
	return true
}

// A browser of this namespace in use here, launched or starting, that the record describes
func (r *registrant) tracked(rec record) *launched {
	r.launcher.mu.Lock()
	defer r.launcher.mu.Unlock()
	for _, b := range append(r.launcher.browsers.values(), r.launcher.starting.values()...) {
		if !b.foreign && recordIs(rec, b) {
			return b
		}
	}
	return nil
}

// Takes over the record of a crashed owner of a browser this process now uses, only the owner changing
func adoptRecord(b *launched) error {
	path := recordPath(b)
	if path == "" || !fsx.Lexists(path) {
		return nil
	}
	var rec record
	if !loadState(path, &rec) || !recordIs(rec, b) || ownerAlive(rec) {
		return nil
	}
	me := os.Getpid()
	rec.OwnerPID, rec.OwnerStart = me, startOf(me)
	return writeState(path, rec)
}

// Reaps one record, the records lock held: a browser whose owner and every holder are gone is stopped
func (r *registrant) reapRecord(path, profile string) bool {
	var rec record
	if !loadState(path, &rec) || !sameNamespace(rec.NS) {
		return false
	}
	if b := r.tracked(rec); b != nil {
		if err := adoptRecord(b); err != nil {
			slog.Error(fmt.Sprintf("browser: could not adopt launch record %s: %s", path, err))
		}
		return false
	}
	if rec.OwnerPID != os.Getpid() && ownerAlive(rec) {
		return false
	}
	if others := otherHolders(path, rec.Start); len(others) > 0 {
		if heir := successor(others); heir != nil {
			if err := handOver(path, rec, heir); err != nil {
				slog.Debug(fmt.Sprintf("browser: could not hand over %s: %s", path, err))
			}
		}
		return false
	}
	reaped := recordedBrowser(rec, profile)
	if reaped {
		r.terminateBrowser(r.config, rec.PID, rec.Start, nil)
		slog.Info(fmt.Sprintf("browser: reaped orphaned Chromium pid %d (%s)", rec.PID, rec.Dir))
	}
	if rec.Ephemeral && insideThrowaway(rec.Dir, profile) && (reaped || !browser.RunningOn(rec.Dir)) {
		removeTree(rec.Dir)
	}
	os.Remove(path)
	return reaped
}

// Reaps one launcher state folder, a shared profile whose lock is taken left for the next pass
func (r *registrant) reapState(records, profile string) int {
	if !fsx.Lexists(records) {
		return 0
	}
	if !isPrivateDir(records) {
		slog.Debug("browser: not reaping from " + records + ": not a private directory of this user")
		return 0
	}
	locked, unlock, err := recordsLock(records, profile != "")
	if err != nil {
		slog.Error(fmt.Sprintf("browser: could not lock %s for reaping: %s", records, err))
		return 0
	}
	defer unlock()
	if !locked {
		slog.Debug("browser: " + records + " is busy (a launch or stop holds it); reaping it next pass")
		return 0
	}
	entries, _ := os.ReadDir(records)
	reaped := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && r.reapRecord(filepath.Join(records, e.Name()), profile) {
			reaped++
		}
	}
	dropDeadHolds(records)
	return reaped
}

// Removes the profile's throwaway folders whose owner is gone, never one of a live owner or of another namespace
func reclaimThrowaway(profile string) {
	root := ephemeralRoot(profile)
	if !fsx.Lexists(root) || !isPrivateDir(root) {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		m := throwawayName.FindStringSubmatch(e.Name())
		if m == nil || m[3] != nsTag() {
			continue
		}
		owner, err := strconv.Atoi(m[1])
		if err == nil && owner == os.Getpid() {
			continue
		}
		var started *int64
		if m[2] != "" {
			if v, err := strconv.ParseInt(m[2], 10, 64); err == nil {
				started = &v
			}
		}
		if err == nil && processAlive(owner, started) {
			continue
		}
		entry := filepath.Join(root, e.Name())
		if browser.RunningOn(entry) {
			slog.Debug("browser: keeping the throwaway profile " + entry + ": a Chromium still runs on it")
			continue
		}
		removeTree(entry)
		slog.Info("browser: removed the throwaway profile " + entry + " of an Icebath process that is gone")
	}
}

// Stops the Chromium of owners that are gone in every profile served and of every shared profile, reclaiming their throwaway folders
func (r *registrant) reapBrowsers(extra []*config.Config) int {
	reaped := 0
	if root := sharedRoot(); fsx.Lexists(root) && isPrivateDir(root) {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			reaped += r.reapState(filepath.Join(root, e.Name()), "")
		}
	}
	r.launcher.mu.Lock()
	profiles := map[string]bool{profileDir(r.config): true}
	for _, cfg := range r.launcher.served {
		profiles[profileDir(cfg)] = true
	}
	r.launcher.mu.Unlock()
	for _, cfg := range extra {
		profiles[profileDir(cfg)] = true
	}
	for _, dir := range slices.Sorted(func(yield func(string) bool) {
		for dir := range profiles {
			if !yield(dir) {
				return
			}
		}
	}) {
		reaped += r.reapState(recordsDir(dir), dir)
		reclaimThrowaway(dir)
	}
	return reaped
}
