package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Touch generation prompt and the token its answer must carry, the readiness proof of a router child
const (
	TouchPrompt = "Reply with exactly one word: the capital of France."
	TouchExpect = "paris"
)

// Stable managed server port, deliberately not 8080 so a user's own llama-server stack never collides
var lrDefaultPort = 18434

// Seconds a resident model may stay idle before the sweeper unloads it and frees its memory
const LlamaIdleUnloadS = 15 * 60

// Restart backoff ladder of a crashed router, the watch poll interval and the clocks tests replace
var (
	lrRestartBackoff = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second, 60 * time.Second}
	supWatchPoll     = 2 * time.Second
	supSleep         = time.Sleep
	supMonotonic     = func() float64 { return time.Since(supEpoch).Seconds() }
	supEpoch         = time.Now()
	supOpenProcess   = openPSProcess
	supSpawnServer   = SpawnServer
	supDirectIOArgs  = lrDirectIOArgs
	supSpawnFn       = (*LlamaServerSupervisor).spawn
	supWaitHealthFn  = (*LlamaServerSupervisor).waitHealth
	supReapFn        = (*LlamaServerSupervisor).reapOrphanedChildren
	supBackoffWait   = func(s *LlamaServerSupervisor, d time.Duration) bool { return s.stopEvent.Wait(d) }
	lrDirectIOMu     sync.Mutex
	lrDirectIOCache  = map[string][]string{}
)

// Endpoint state other processes read to route llamacpp alias requests at the managed server
func localRuntimeStatePath() string {
	return filepath.Join(RuntimesRoot(), "server.json")
}

// LocalRuntimeStatePath is the supervisor state file of the managed local runtime
func LocalRuntimeStatePath() string { return localRuntimeStatePath() }

// Router status values of a model holding a loaded child
func lrIsResident(status any) bool {
	s, ok := status.(string)
	return ok && (s == "loaded" || s == "ready")
}

// Ephemeral loopback port the OS hands out
func lrFreePort() (int, error) {
	l, err := lrBindLoopback(0)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Binds a loopback port without address reuse, as a plain Python socket bind does
func lrBindLoopback(port int) (net.Listener, error) {
	lc := net.ListenConfig{Control: lrBindControl}
	return lc.Listen(context.Background(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// Default port, or an ephemeral one only when something else already listens there
func lrStablePort() (int, error) {
	if l, err := lrBindLoopback(lrDefaultPort); err == nil {
		l.Close()
		return lrDefaultPort, nil
	}
	slog.Warn(fmt.Sprintf("port %d busy; managed llama-server falling back to an ephemeral port — resumed sessions follow the live endpoint", lrDefaultPort))
	return lrFreePort()
}

// One api key for the life of the install persisted beside the runtimes, err on an undecodable key file
func lrStableAPIKey() (string, error) {
	keyPath := filepath.Join(RuntimesRoot(), ".api_key")
	if raw, err := os.ReadFile(keyPath); err == nil {
		raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
		if !utf8.Valid(raw) {
			return "", fmt.Errorf("'utf-8' codec can't decode the api key file %s", keyPath)
		}
		existing := pyStripSpace(string(raw))
		if utf8.RuneCountInString(existing) >= 16 {
			return existing, nil
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	key := base64.RawURLEncoding.EncodeToString(buf)
	err := os.MkdirAll(filepath.Dir(keyPath), 0o777)
	if err == nil {
		err = os.WriteFile(keyPath, []byte(key), 0o666)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("could not persist api key (%s); sessions will need a re-pick after restart", err))
	}
	return key, nil
}

// Load mode flags this engine build supports, read once per executable from its help text
func lrDirectIOArgs(executable string) ([]string, error) {
	lrDirectIOMu.Lock()
	cached, ok := lrDirectIOCache[executable]
	lrDirectIOMu.Unlock()
	if ok {
		return cached, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--help")
	cmd.Dir = filepath.Dir(executable)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		argv := pyRepr([]any{executable, "--help"})
		var exitErr *exec.ExitError
		switch {
		case ctx.Err() != nil:
			return nil, fmt.Errorf("Command '%s' timed out after 15 seconds", argv)
		case errors.As(err, &exitErr):
			return nil, fmt.Errorf("Command '%s' returned non-zero exit status %d.", argv, lrReturnCode(exitErr.ProcessState))
		}
		return nil, err
	}
	help := stdout.String() + stderr.String()
	out := []string{}
	if strings.Contains(help, "--load-mode") {
		out = []string{"--load-mode", "dio"}
	} else if strings.Contains(help, "--direct-io") {
		out = []string{"-dio"}
	}
	lrDirectIOMu.Lock()
	lrDirectIOCache[executable] = out
	lrDirectIOMu.Unlock()
	return out, nil
}

// HTTP status outside 2xx, raised like urllib's HTTPError
type lrHTTPError struct {
	Code int
	URL  string
}

func (e *lrHTTPError) Error() string {
	return fmt.Sprintf("HTTP Error %d: %s", e.Code, http.StatusText(e.Code))
}

// Settable event with a wait that returns early when set
type lrEvent struct {
	mu  sync.Mutex
	set bool
	ch  chan struct{}
}

func newLREvent() *lrEvent { return &lrEvent{ch: make(chan struct{})} }

func (e *lrEvent) Set() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.set {
		e.set = true
		close(e.ch)
	}
}

func (e *lrEvent) Clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set {
		e.set = false
		e.ch = make(chan struct{})
	}
}

func (e *lrEvent) IsSet() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set
}

// Waits up to d and reports whether the event was set
func (e *lrEvent) Wait(d time.Duration) bool {
	e.mu.Lock()
	ch := e.ch
	e.mu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return e.IsSet()
	}
}

// Spawned router process with Popen semantics, reaped by its own waiter
type lrServerProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	rc   int
}

// Starts reaping cmd in the background so poll and wait never block
func lrWatchProcess(cmd *exec.Cmd) *lrServerProc {
	p := &lrServerProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		p.rc = lrReturnCode(cmd.ProcessState)
		close(p.done)
	}()
	return p
}

// Python returncode of an exited process, minus the signal number for a signal death
func lrReturnCode(st *os.ProcessState) int {
	if st == nil {
		return -1
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return st.ExitCode()
}

// Exit code and true once the process exited, like Popen.poll
func (p *lrServerProc) Poll() (int, bool) {
	select {
	case <-p.done:
		return p.rc, true
	default:
		return 0, false
	}
}

func (p *lrServerProc) Pid() int { return p.cmd.Process.Pid }

func (p *lrServerProc) CreateTime() (float64, error) {
	ps, err := openPSProcess(p.Pid())
	if err != nil {
		return 0, err
	}
	return ps.CreateTime()
}

func (p *lrServerProc) IsRunning() bool {
	_, exited := p.Poll()
	return !exited
}

func (p *lrServerProc) Exe() (string, error) {
	ps, err := openPSProcess(p.Pid())
	if err != nil {
		return "", err
	}
	return ps.Exe()
}

func (p *lrServerProc) PPID() (int, error) {
	ps, err := openPSProcess(p.Pid())
	if err != nil {
		return 0, err
	}
	return ps.PPID()
}

func (p *lrServerProc) Parent() (localRuntimeProc, error) {
	ps, err := openPSProcess(p.Pid())
	if err != nil {
		return nil, err
	}
	return ps.Parent()
}

func (p *lrServerProc) Cmdline() ([]string, error) {
	return append([]string(nil), p.cmd.Args...), nil
}

func (p *lrServerProc) Children(recursive bool) ([]localRuntimeProc, error) {
	ps, err := openPSProcess(p.Pid())
	if err != nil {
		return nil, err
	}
	return ps.Children(recursive)
}

// Signals the process unless it already exited, TerminateProcess on Windows and SIGTERM elsewhere
func (p *lrServerProc) Terminate() error {
	if _, exited := p.Poll(); exited {
		return nil
	}
	var err error
	if runtime.GOOS == "windows" {
		err = p.cmd.Process.Kill()
	} else {
		err = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (p *lrServerProc) Kill() error {
	if _, exited := p.Poll(); exited {
		return nil
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (p *lrServerProc) Wait(timeout time.Duration) error {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-p.done:
		return nil
	case <-t.C:
		return &psError{Class: "TimeoutExpired", Pid: p.Pid(), Msg: fmt.Sprintf("Command '%s' timed out after %v seconds", p.cmd.Path, timeout.Seconds())}
	}
}

// One llama-server router process owned for the life of an icebath session
type LlamaServerSupervisor struct {
	Binary     string
	ModelsDir  string
	ModelsMax  int
	Port       int
	APIKey     string
	ExtraArgs  []string
	LogPath    string
	PresetPath string

	lifecycle    sync.Mutex
	mu           sync.Mutex
	proc         *lrServerProc
	job          *WindowsJob
	primaryModel string
	restarts     int
	stopping     atomic.Bool
	stopEvent    *lrEvent
	state        map[string]any
	logHandle    *os.File
	idleSince    map[string]float64
	refitUsable  *int64
}

// Supervisor for binary serving modelsDir, port 0 picks the stable port and empty paths take the defaults
func NewLlamaServerSupervisor(binary, modelsDir string, modelsMax, port int, extraArgs []string, logPath, presetPath string) (*LlamaServerSupervisor, error) {
	s := &LlamaServerSupervisor{Binary: binary, ModelsDir: modelsDir, ModelsMax: modelsMax, Port: port,
		ExtraArgs: append([]string{}, extraArgs...), LogPath: logPath, PresetPath: presetPath,
		stopEvent: newLREvent(), idleSince: map[string]float64{}}
	if s.Port == 0 {
		p, err := lrStablePort()
		if err != nil {
			return nil, err
		}
		s.Port = p
	}
	key, err := lrStableAPIKey()
	if err != nil {
		return nil, err
	}
	s.APIKey = key
	if s.LogPath == "" {
		s.LogPath = filepath.Join(filepath.Dir(modelsDir), "logs", "llama-server.log")
	}
	return s, nil
}

// OpenAI compatible base URL, always the loopback IP since resolving localhost costs seconds on Windows
func (s *LlamaServerSupervisor) BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", s.Port)
}

func (s *LlamaServerSupervisor) url(route string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, route)
}

// Model whose readiness a crash restart proves again, empty for none
func (s *LlamaServerSupervisor) PrimaryModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.primaryModel
}

func (s *LlamaServerSupervisor) SetPrimaryModel(modelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.primaryModel = modelID
}

// Router process handle, nil before the first spawn
func (s *LlamaServerSupervisor) currentProc() *lrServerProc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc
}

// Endpoint state recorded at the last spawn
func (s *LlamaServerSupervisor) State() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Sends one bearer authenticated request, a POST when body is set, and fails on any non 2xx status
func (s *LlamaServerSupervisor) open(route string, body []byte, timeout time.Duration, jsonType bool) ([]byte, error) {
	method := http.MethodGet
	var reader io.Reader
	if body != nil {
		method, reader = http.MethodPost, bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, s.url(route), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	if jsonType {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &lrHTTPError{Code: resp.StatusCode, URL: req.URL.String()}
	}
	return io.ReadAll(resp.Body)
}

// Decoded JSON answer of a router route, an empty object for an empty body
func (s *LlamaServerSupervisor) request(route string, body []byte, timeout time.Duration) (any, error) {
	raw, err := s.open(route, body, timeout, true)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	text, err := hfJSONText(raw)
	if err != nil {
		return nil, err
	}
	v, ok := moaJSONLoads(text)
	if !ok {
		return nil, fmt.Errorf("Expecting value: line 1 column 1 (char 0)")
	}
	return v, nil
}

// Python json.dumps text of a request body whose keys keep their order
func lrRequestBody(pairs ...any) []byte {
	var b strings.Builder
	lrWriteJSON(&b, lrObject(pairs))
	return []byte(b.String())
}

// Ordered JSON object as alternating keys and values
type lrObject []any

func lrWriteJSON(b *strings.Builder, v any) {
	switch x := v.(type) {
	case lrObject:
		b.WriteByte('{')
		for i := 0; i+1 < len(x); i += 2 {
			if i > 0 {
				b.WriteString(", ")
			}
			pyJSONWriteString(b, x[i].(string))
			b.WriteString(": ")
			lrWriteJSON(b, x[i+1])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			lrWriteJSON(b, item)
		}
		b.WriteByte(']')
	case string:
		pyJSONWriteString(b, x)
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		b.WriteString(pyFloatRepr(x))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case nil:
		b.WriteString("null")
	}
}

// Spawns the router on the stable port and records its endpoint state at spawn time
func (s *LlamaServerSupervisor) spawn() error {
	dio, err := supDirectIOArgs(s.Binary)
	if err != nil {
		return err
	}
	cmdArgs := []string{s.Binary, "--host", "127.0.0.1", "--port", strconv.Itoa(s.Port), "--api-key", s.APIKey,
		"--models-max", strconv.Itoa(s.ModelsMax), "--models-autoload", "--metrics", "--slots", "--no-ui", "--jinja"}
	cmdArgs = append(cmdArgs, dio...)
	if s.PresetPath != "" && lrPathExists(s.PresetPath) {
		cmdArgs = append(cmdArgs, "--models-preset", s.PresetPath)
	} else {
		cmdArgs = append(cmdArgs, "--models-dir", s.ModelsDir)
	}
	cmdArgs = append(cmdArgs, s.ExtraArgs...)
	if err := os.MkdirAll(filepath.Dir(s.LogPath), 0o777); err != nil {
		return err
	}
	s.mu.Lock()
	if s.logHandle != nil {
		_ = s.logHandle.Close()
		s.logHandle = nil
	}
	s.mu.Unlock()
	logHandle, err := os.OpenFile(s.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	argv := make([]any, len(cmdArgs))
	for i, a := range cmdArgs {
		argv[i] = a
	}
	if _, err := logHandle.WriteString("\n# spawn: " + pyRepr(argv) + "\n"); err != nil {
		logHandle.Close()
		return err
	}
	s.mu.Lock()
	s.logHandle = logHandle
	s.mu.Unlock()
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdout, cmd.Stderr = logHandle, logHandle
	cmd.Dir = filepath.Dir(s.Binary)
	cmd.Env = lrEnvList(ServerChildEnv(lrEnvMap(os.Environ())))
	job, err := supSpawnServer(cmd, true)
	if err != nil {
		return err
	}
	proc := lrWatchProcess(cmd)
	s.mu.Lock()
	s.proc, s.job = proc, job
	s.mu.Unlock()
	slog.Info(fmt.Sprintf("llama-server router spawned pid=%d port=%d", proc.Pid(), s.Port))
	return s.writeState()
}

// Environment list as a map, the first assignment of a name winning like os.environ
func lrEnvMap(environ []string) map[string]string {
	out := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, seen := out[k]; !seen {
			out[k] = v
		}
	}
	return out
}

func lrEnvList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func lrPathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Python json value of an optional start time
func lrStartTimeValue(pid int) any {
	if v, ok := tools.ProcessStartTime(pid); ok {
		return v
	}
	return nil
}

// Records the router incarnation and its owner so other processes can route to and recover it
func (s *LlamaServerSupervisor) writeState() error {
	proc := s.currentProc()
	ps, err := openPSProcess(proc.Pid())
	if err != nil {
		return err
	}
	created, err := ps.CreateTime()
	if err != nil {
		return err
	}
	exe, err := ps.Exe()
	if err != nil {
		return err
	}
	owner, err := openPSProcess(os.Getpid())
	if err != nil {
		return err
	}
	ownerCreated, err := owner.CreateTime()
	if err != nil {
		return err
	}
	state := lrObject{"base_url", s.BaseURL(), "api_key", s.APIKey, "pid", ps.Pid(), "create_time", created,
		"start_time", lrStartTimeValue(ps.Pid()), "executable", exe, "owner_pid", os.Getpid(),
		"owner_create_time", ownerCreated, "owner_start_time", lrStartTimeValue(os.Getpid())}
	asMap := map[string]any{}
	for i := 0; i+1 < len(state); i += 2 {
		asMap[state[i].(string)] = state[i+1]
	}
	s.mu.Lock()
	s.state = asMap
	s.mu.Unlock()
	path := localRuntimeStatePath()
	if err := lrMkdirUnderHome(filepath.Dir(path)); err != nil {
		return err
	}
	return config.AtomicWrite(path, []byte(lrIndentedJSON(state)), 0o600)
}

// Creates dir unless it lies inside a deleted or missing named profile
func lrMkdirUnderHome(dir string) error {
	if profile := home.NamedProfileHome(dir); profile != "" && !home.ProfileLive(profile) {
		return fmt.Errorf("profile home %s was deleted or never created", profile)
	}
	return os.MkdirAll(dir, 0o700)
}

// Python json.dumps text with indent 2 of a flat ordered object
func lrIndentedJSON(obj lrObject) string {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i+1 < len(obj); i += 2 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n  ")
		pyJSONWriteString(&b, obj[i].(string))
		b.WriteString(": ")
		lrWriteJSON(&b, obj[i+1])
	}
	if len(obj) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("}")
	return b.String()
}

// Spawns the router, waits for health and starts the crash watchdog
func (s *LlamaServerSupervisor) Start(timeout time.Duration) error {
	s.lifecycle.Lock()
	s.stopping.Store(false)
	s.stopEvent.Clear()
	err := s.spawn()
	s.lifecycle.Unlock()
	if err != nil {
		return err
	}
	if err := s.waitHealth(timeout); err != nil {
		return err
	}
	go s.watch()
	return nil
}

// Polls the unauthenticated health route until it answers 200, failing on exit, cancel or timeout
func (s *LlamaServerSupervisor) waitHealth(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		if s.stopEvent.IsSet() {
			return errors.New("llama-server startup cancelled")
		}
		if proc := s.currentProc(); proc != nil {
			if rc, exited := proc.Poll(); exited {
				return fmt.Errorf("llama-server exited rc=%d during startup (log: %s)", rc, s.LogPath)
			}
		}
		if resp, err := client.Get(s.url("/health")); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		supSleep(time.Second)
	}
	return fmt.Errorf("llama-server not healthy after %ds (log: %s)", int(timeout.Seconds()), s.LogPath)
}

// Restarts the router, never its children, with backoff after every crash
func (s *LlamaServerSupervisor) watch() {
	for !s.stopping.Load() {
		proc := s.currentProc()
		if proc == nil {
			return
		}
		rc, exited := proc.Poll()
		if !exited {
			supSleep(supWatchPoll)
			continue
		}
		if s.stopping.Load() {
			return
		}
		tools.ReleaseProcessGroupOnceEmpty(proc.Pid(), nil, 0)
		s.mu.Lock()
		restarts := s.restarts
		s.mu.Unlock()
		backoff := lrRestartBackoff[min(restarts, len(lrRestartBackoff)-1)]
		slog.Warn(fmt.Sprintf("llama-server exited rc=%d; restart #%d in %ss", rc, restarts+1, pyFloatOrInt(backoff.Seconds())))
		if supBackoffWait(s, backoff) {
			return
		}
		s.mu.Lock()
		s.restarts++
		s.mu.Unlock()
		stopped, err := s.restart(proc)
		if stopped {
			return
		}
		if err != nil {
			slog.Error(fmt.Sprintf("llama-server restart failed: %s", err))
		}
	}
}

// Seconds as Python prints an int or float backoff
func pyFloatOrInt(secs float64) string {
	if secs == float64(int64(secs)) {
		return strconv.FormatInt(int64(secs), 10)
	}
	return pyFloatRepr(secs)
}

// Replaces a crashed router, true when a stop arrived first and the watchdog must end
func (s *LlamaServerSupervisor) restart(crashed *lrServerProc) (bool, error) {
	s.lifecycle.Lock()
	if s.stopping.Load() {
		s.lifecycle.Unlock()
		return true, nil
	}
	tools.StopOwnedGroupSurvivors(crashed.Pid(), nil, 3*time.Second)
	err := supReapFn(s)
	if err == nil {
		err = supSpawnFn(s)
	}
	s.lifecycle.Unlock()
	if err != nil {
		return false, err
	}
	if err := supWaitHealthFn(s, 120*time.Second); err != nil {
		return false, err
	}
	if primary := s.PrimaryModel(); primary != "" {
		if _, err := s.EnsureModelReady(primary, 600*time.Second); err != nil {
			return false, err
		}
	}
	return false, nil
}

// Stops the router tree, closes the containment job and keeps the state file for a replacement to overwrite
func (s *LlamaServerSupervisor) Stop() error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.stopping.Store(true)
	s.stopEvent.Set()
	proc := s.currentProc()
	var termErr error
	if proc != nil {
		if _, exited := proc.Poll(); !exited {
			termErr = terminateLlamaServerTree(proc, false)
		}
	}
	s.mu.Lock()
	job := s.job
	s.job = nil
	s.mu.Unlock()
	if job != nil {
		if err := job.Close(); err != nil {
			return err
		}
	}
	if termErr != nil {
		return termErr
	}
	if proc != nil {
		tools.StopOwnedGroupSurvivors(proc.Pid(), nil, 3*time.Second)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logHandle != nil {
		err := s.logHandle.Close()
		s.logHandle = nil
		return err
	}
	return nil
}

// Terminates the router and its model children, children enumerated first while the parent can still be walked
func terminateLlamaServerTree(proc localRuntimeProc, verifiedRoot bool) (err error) {
	var children []localRuntimeProc
	if verifiedRoot {
		children, err = proc.Children(true)
		if err != nil {
			return err
		}
		if !proc.IsRunning() {
			return &psError{Class: "NoSuchProcess", Pid: proc.Pid(), Msg: "process no longer exists"}
		}
	} else if ps, openErr := supOpenProcess(proc.Pid()); openErr == nil {
		if found, walkErr := ps.Children(true); walkErr == nil {
			children = found
		}
	}
	defer func() {
		for _, child := range children {
			if child.IsRunning() {
				_ = child.Kill()
			}
		}
	}()
	for _, child := range children {
		_ = child.Terminate()
	}
	if err := proc.Terminate(); err != nil {
		return err
	}
	if err := proc.Wait(15 * time.Second); err != nil {
		var pe *psError
		if !errors.As(err, &pe) || pe.Class != "TimeoutExpired" {
			return err
		}
		return proc.Kill()
	}
	return nil
}

// Kills model children a crashed router orphaned before the respawn loads models beside them
func (s *LlamaServerSupervisor) reapOrphanedChildren() error {
	s.mu.Lock()
	job := s.job
	s.job = nil
	s.mu.Unlock()
	if job != nil {
		return job.Close()
	}
	ownPid := -1
	if proc := s.currentProc(); proc != nil {
		ownPid = proc.Pid()
	}
	lrReapOrphans(s.Binary, ownPid)
	return nil
}

// Router models in listing order with their status values
type RouterModels struct {
	IDs    []string
	Status map[string]any
}

// Status value of one model and whether the router lists it
func (m RouterModels) Get(modelID string) (any, bool) {
	v, ok := m.Status[modelID]
	return v, ok
}

// Model id to status value from GET /models
func (s *LlamaServerSupervisor) Models(timeout time.Duration) (RouterModels, error) {
	out := RouterModels{Status: map[string]any{}}
	body, err := s.request("/models", nil, timeout)
	if err != nil {
		return out, err
	}
	obj, ok := body.(map[string]any)
	if !ok {
		return out, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(body))
	}
	data, present := obj["data"]
	if !present {
		return out, nil
	}
	items, ok := data.([]any)
	if !ok {
		if m, isMap := data.(map[string]any); isMap && len(m) == 0 {
			return out, nil
		}
		return out, fmt.Errorf("'%s' object is not iterable as model entries", pyTypeName(data))
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return out, fmt.Errorf("'%s' object is not subscriptable", pyTypeName(item))
		}
		rawID, ok := m["id"]
		if !ok {
			return out, errors.New("KeyError: 'id'")
		}
		id, ok := rawID.(string)
		if !ok {
			return out, fmt.Errorf("model id %s is not a string", pyRepr(rawID))
		}
		var status any = "unknown"
		if st, has := m["status"]; has {
			stMap, isMap := st.(map[string]any)
			if !isMap {
				return out, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(st))
			}
			if v, has := stMap["value"]; has {
				status = v
			}
		}
		if _, seen := out.Status[id]; !seen {
			out.IDs = append(out.IDs, id)
		}
		out.Status[id] = status
	}
	return out, nil
}

// Asks the router to load one model child
func (s *LlamaServerSupervisor) LoadModel(modelID string, timeout time.Duration) error {
	_, err := s.request("/models/load", lrRequestBody("model", modelID), timeout)
	return err
}

// Has the router re-read the preset file, unloading loaded models whose flags changed
func (s *LlamaServerSupervisor) ReloadPresets() error {
	_, err := s.request("/models?reload=1", nil, 10*time.Second)
	return err
}

// Frees a child's memory now and waits up to 15 s for it to leave the resident states
func (s *LlamaServerSupervisor) UnloadModel(modelID string) error {
	if _, err := s.request("/models/unload", lrRequestBody("model", modelID), 120*time.Second); err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		models, err := s.Models(30 * time.Second)
		if err != nil {
			return nil
		}
		status, _ := models.Get(modelID)
		if !lrIsResident(status) && status != "unloading" {
			return nil
		}
		supSleep(300 * time.Millisecond)
	}
	return nil
}

// Unloads models idle past the threshold and returns their ids, a failed probe keeps the idle clock
func (s *LlamaServerSupervisor) SweepIdle(now *float64) []string {
	t := supMonotonic()
	if now != nil {
		t = *now
	}
	unloaded := []string{}
	statuses, err := s.Models(30 * time.Second)
	if err != nil {
		return unloaded
	}
	for _, modelID := range statuses.IDs {
		if !lrIsResident(statuses.Status[modelID]) {
			s.dropIdle(modelID)
			continue
		}
		probe := s.probeIdle(&modelID)
		if probe == nil {
			s.mu.Lock()
			since, ok := s.idleSince[modelID]
			s.mu.Unlock()
			if !ok {
				since = t
			}
			slog.Info(fmt.Sprintf("idle probe for %s failed; keeping idle clock (idle %ds)", modelID, int64(t-since)))
			continue
		}
		if !*probe {
			s.dropIdle(modelID)
			continue
		}
		s.mu.Lock()
		firstIdle, ok := s.idleSince[modelID]
		if !ok {
			firstIdle = t
			s.idleSince[modelID] = t
		}
		s.mu.Unlock()
		if t-firstIdle < LlamaIdleUnloadS {
			continue
		}
		if err := s.UnloadModel(modelID); err != nil {
			slog.Warn(fmt.Sprintf("idle unload of %s failed: %s", modelID, err))
			continue
		}
		s.dropIdle(modelID)
		unloaded = append(unloaded, modelID)
		slog.Info(fmt.Sprintf("idle-unloaded %s (idle %ds)", modelID, int64(t-firstIdle)))
	}
	return unloaded
}

func (s *LlamaServerSupervisor) dropIdle(modelID string) {
	s.mu.Lock()
	delete(s.idleSince, modelID)
	s.mu.Unlock()
}

// Readiness proof by a temperature zero generation whose content or reasoning names the expected token
func (s *LlamaServerSupervisor) TouchGenerate(modelID string, timeout time.Duration) bool {
	ok, err := s.touch(modelID, timeout)
	if err != nil {
		slog.Warn(fmt.Sprintf("touch generation failed for %s: %s", modelID, err))
		return false
	}
	return ok
}

func (s *LlamaServerSupervisor) touch(modelID string, timeout time.Duration) (bool, error) {
	body := lrRequestBody("model", modelID, "messages", []any{lrObject{"role", "user", "content", TouchPrompt}},
		"max_tokens", 512, "temperature", 0)
	resp, err := s.request("/v1/chat/completions", body, timeout)
	if err != nil {
		return false, err
	}
	obj, ok := resp.(map[string]any)
	if !ok {
		return false, fmt.Errorf("%s indices must be integers", pyTypeName(resp))
	}
	choices, ok := obj["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false, errors.New("list index out of range")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return false, errors.New("'choices' entry is not an object")
	}
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		return false, errors.New("KeyError: 'message'")
	}
	content, err := lrTextOrEmpty(msg["content"])
	if err != nil {
		return false, err
	}
	reasoning, err := lrTextOrEmpty(msg["reasoning_content"])
	if err != nil {
		return false, err
	}
	return strings.Contains(pyLower(content+" "+reasoning), TouchExpect), nil
}

// Python value or "" for a falsy value, a TypeError for a truthy non string
func lrTextOrEmpty(v any) (string, error) {
	if !pyTruthy(v) {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("can only concatenate str (not \"%s\") to str", pyTypeName(v))
	}
	return s, nil
}

// Python str.lower, which keeps the combining dot of a dotted capital I
func pyLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

// Missing model error whose text is the quoted key like a Python KeyError
type lrModelMissingError struct{ ModelID string }

func (e *lrModelMissingError) Error() string {
	return pyStringRepr("model " + e.ModelID + " not present in models dir")
}

// Loads a listed model when needed and proves readiness with a touch generation
func (s *LlamaServerSupervisor) EnsureModelReady(modelID string, timeout time.Duration) (bool, error) {
	models, err := s.Models(30 * time.Second)
	if err != nil {
		return false, err
	}
	status, ok := models.Get(modelID)
	if !ok {
		return false, &lrModelMissingError{ModelID: modelID}
	}
	if !lrIsResident(status) {
		if err := s.LoadModel(modelID, timeout); err != nil {
			return false, err
		}
	}
	return s.TouchGenerate(modelID, 300*time.Second), nil
}

// No processing requests and no busy slots on the given child, or on every loaded child for ""
func (s *LlamaServerSupervisor) IsIdle(modelID string) bool {
	var id *string
	if modelID != "" {
		id = &modelID
	}
	probe := s.probeIdle(id)
	return probe != nil && *probe
}

// Tri state idle probe, nil when the probe itself failed, an unloaded child answering 400 counts idle
func (s *LlamaServerSupervisor) probeIdle(modelID *string) *bool {
	idle, err := s.probeIdleErr(modelID)
	if err != nil {
		return nil
	}
	return &idle
}

func (s *LlamaServerSupervisor) probeIdleErr(modelID *string) (bool, error) {
	var loaded []string
	if modelID != nil {
		loaded = []string{*modelID}
	} else {
		models, err := s.Models(30 * time.Second)
		if err != nil {
			return false, err
		}
		for _, id := range models.IDs {
			if lrIsResident(models.Status[id]) {
				loaded = append(loaded, id)
			}
		}
	}
	for _, mid := range loaded {
		slots, err := s.request("/slots?"+childQuery(mid), nil, 30*time.Second)
		if err != nil {
			var he *lrHTTPError
			if errors.As(err, &he) && he.Code == 400 {
				continue
			}
			return false, err
		}
		busy, err := lrAnySlotProcessing(slots)
		if err != nil {
			return false, err
		}
		if busy {
			return false, nil
		}
		raw, err := s.open("/metrics?"+childQuery(mid), nil, 10*time.Second, false)
		if err != nil {
			return false, err
		}
		if !utf8.Valid(raw) {
			return false, errors.New("'utf-8' codec can't decode the metrics body")
		}
		for _, line := range pySplitLines(string(raw)) {
			if !strings.HasPrefix(line, "llamacpp:requests_processing") {
				continue
			}
			fields := strings.FieldsFunc(line, pyIsSpace)
			value, ok := lrPyFloat(fields[len(fields)-1])
			if !ok {
				return false, fmt.Errorf("could not convert string to float: %s", pyStringRepr(fields[len(fields)-1]))
			}
			if value != 0 {
				return false, nil
			}
		}
	}
	return true, nil
}

// Whether any slot of a /slots answer is processing, erroring where Python iteration or .get would raise
func lrAnySlotProcessing(slots any) (bool, error) {
	switch x := slots.(type) {
	case []any:
		for _, slot := range x {
			m, ok := slot.(map[string]any)
			if !ok {
				return false, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(slot))
			}
			if pyTruthy(m["is_processing"]) {
				return true, nil
			}
		}
		return false, nil
	case map[string]any:
		if len(x) == 0 {
			return false, nil
		}
		return false, errors.New("'str' object has no attribute 'get'")
	case string:
		if x == "" {
			return false, nil
		}
		return false, errors.New("'str' object has no attribute 'get'")
	default:
		return false, fmt.Errorf("'%s' object is not iterable", pyTypeName(slots))
	}
}

// Python float of a numeric string, underscores between digits and any script's decimal digits allowed
func lrPyFloat(text string) (float64, bool) {
	var b strings.Builder
	for _, r := range pyStripSpace(text) {
		if r > 0x7f && unicode.IsDigit(r) {
			b.WriteRune(rune('0' + lrDigitValue(r)))
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	lower := strings.ToLower(strings.TrimLeft(s, "+-"))
	if len(s)-len(strings.TrimLeft(s, "+-")) <= 1 && (lower == "inf" || lower == "infinity" || lower == "nan") {
		v, err := strconv.ParseFloat(s, 64)
		return v, err == nil
	}
	if !lrPyFloatSyntax.MatchString(s) {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil {
		var ne *strconv.NumError
		if errors.As(err, &ne) && ne.Err == strconv.ErrRange {
			return v, true
		}
		return 0, false
	}
	return v, true
}

// Decimal value of a Unicode decimal digit, every Nd range starting at a zero
func lrDigitValue(r rune) int {
	for _, rg := range unicode.Nd.R16 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10
		}
	}
	for _, rg := range unicode.Nd.R32 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10
		}
	}
	return 0
}

// Python float() literal grammar after digits are mapped to ASCII
var lrPyFloatSyntax = regexp.MustCompile(`^[+-]?(?:(?:[0-9](?:_?[0-9])*)?\.[0-9](?:_?[0-9])*|[0-9](?:_?[0-9])*\.?)(?:[eE][+-]?[0-9](?:_?[0-9])*)?$`)
