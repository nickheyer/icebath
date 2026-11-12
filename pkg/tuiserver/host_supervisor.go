package tuiserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Argv word that runs this binary as the compute host child
const ComputeHostEntry = "__icebath_compute_host__"

// Route class of every host mutator a control frame may carry
var mutatorRouteTable = map[string]string{
	"prompt.submit": "turn-path", "session.interrupt": "turn-path", "reload.mcp": "run-concurrent",
	"session.save": "run-concurrent", "session.compress": "idle-gated",
	"prompt.submit.truncate": "idle-gated", "slash.model": "idle-gated",
	"slash.personality": "idle-gated", "slash.prompt": "idle-gated", "slash.compress": "idle-gated",
	"slash.refine":  "idle-gated",
	"session.reset": "idle-gated", "session.history.reload": "idle-gated",
	"slash.retry": "idle-gated"}

// Registry file name, respawn window, shutdown and hello waits of the compute host
const (
	hostRegistryName      = "dashboard-compute-host.json"
	hostRespawnWindowSecs = 300.0
	hostShutdownTimeout   = 10 * time.Second
	hostHelloTimeoutSecs  = 10.0
)

// Late control ack registrations kept past their waiter, bounded by age and count
var (
	lateControlTTLSecs = 1800.0
	lateControlMax     = 64
)

// Host frames whose request id resolves a pending or late control waiter
var hostControlReplyTypes = setOf("control.ack", "control.error", "respond.ack", "respond.error", "interrupt.ack",
	"reload_mcp.ack", "shutdown.ack")

// Error of a control or respond wait that ran out, the queue.Empty of the Python waiter
var ErrHostReplyTimeout = errors.New("compute host reply timed out")

// Stripped stdout of a short subprocess, empty on any failure
func hostCheckOutput(argv []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(out), "�"))
}

// Revision this binary was built from, unknown when the build carries none, shared with the child hello
func hostBuildSHA() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value
			}
		}
	}
	return "unknown"
}

// Calls a host frame callback, logging instead of raising when it panics
func hostCallLogged(cb func(*pyjson.Object), frame *pyjson.Object, failure string) {
	defer func() {
		if r := recover(); r != nil {
			logger().Error(fmt.Sprintf("%s: %s", failure, failureText(r)))
		}
	}()
	cb(frame)
}

// Appends one log record with a single append write so concurrent writers never interleave a line
func appendLogRecord(path, record string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	if !strings.HasSuffix(record, "\n") {
		record += "\n"
	}
	return hostAppendWrite(path, []byte(strings.ToValidUTF8(record, "\uFFFD")))
}

// Command line of a pid, procfs first then ps, empty when neither answers
func hostPIDCommand(pid int) string {
	if pid <= 0 {
		return ""
	}
	if data := hostProcCmdline(pid); data != "" {
		return strings.ToValidUTF8(strings.ReplaceAll(data, "\x00", " "), "�")
	}
	return hostCheckOutput([]string{"ps", "-p", fmt.Sprint(pid), "-o", "command="})
}

// Whether pid runs the compute host entry of this binary
func IsComputeHostIdentity(pid int) bool {
	return strings.Contains(hostPIDCommand(pid), ComputeHostEntry)
}

// Pending turn completion of one session
type hostPendingTurn struct {
	sid        string
	onComplete func(*pyjson.Object)
}

// Late control handler with its registration time
type hostLateControl struct {
	at      float64
	handler func(*pyjson.Object)
}

// Owner of one persistent compute host child that relays its frames
type HostSupervisor struct {
	RegistryPath       string
	Argv               []string
	Cwd                string
	Env                map[string]string
	rpcSink            func(*pyjson.Object)
	RespawnMax         int
	HeartbeatSecs      int
	ExpectedBuildSHA   string
	ExpectedHermesHome string

	lock                pythread.RLock
	procMu              sync.Mutex
	proc                *exec.Cmd
	stdin               io.WriteCloser
	exited              chan struct{}
	helloEvent          pythread.Event
	hello               *pyjson.Object
	closing             bool
	stoppedRespawning   bool
	restartTimes        []float64
	pendingTurns        map[string]hostPendingTurn
	pendingControls     map[string]chan *pyjson.Object
	lateControlHandlers map[string]hostLateControl
	stderrMu            sync.Mutex
	stderrTail          []string
	lastProgressCounter int
	pidMatchesHost      func(int) bool
	sendFrameFn         func(*pyjson.Object) error
	startFn             func() error
}

// Options of a supervisor, zero values taking the defaults of the Python constructor
type HostSupervisorOptions struct {
	RegistryPath       string
	Argv               []string
	Cwd                string
	Env                map[string]string
	RPCSink            func(*pyjson.Object)
	RespawnMax         int
	HeartbeatSecs      int
	ExpectedBuildSHA   *string
	ExpectedHermesHome *string
	Autostart          bool
}

// Supervisor of the compute host, started at once when autostart is set
func NewHostSupervisor(ctx context.Context, o HostSupervisorOptions) (*HostSupervisor, error) {
	s := &HostSupervisor{RegistryPath: o.RegistryPath, Argv: o.Argv, Cwd: o.Cwd, Env: o.Env, rpcSink: o.RPCSink,
		RespawnMax: max(0, o.RespawnMax), HeartbeatSecs: max(1, o.HeartbeatSecs),
		pendingTurns: map[string]hostPendingTurn{}, pendingControls: map[string]chan *pyjson.Object{},
		lateControlHandlers: map[string]hostLateControl{}, pidMatchesHost: IsComputeHostIdentity}
	if s.RegistryPath == "" {
		s.RegistryPath = filepath.Join(home.Dir(ctx), "state", hostRegistryName)
	}
	if len(s.Argv) == 0 {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		s.Argv = []string{exe, ComputeHostEntry}
	}
	if s.Cwd == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		s.Cwd = filepath.Dir(exe)
	}
	if s.rpcSink == nil {
		s.rpcSink = func(*pyjson.Object) {}
	}
	if o.ExpectedBuildSHA == nil {
		s.ExpectedBuildSHA = hostBuildSHA()
	} else {
		s.ExpectedBuildSHA = *o.ExpectedBuildSHA
	}
	if o.ExpectedHermesHome == nil {
		s.ExpectedHermesHome = home.Dir(ctx)
	} else {
		s.ExpectedHermesHome = *o.ExpectedHermesHome
	}
	s.sendFrameFn = s.sendFrame
	s.startFn = s.start
	if o.Autostart {
		if err := s.Start(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Pid of the running child, zero when none was spawned
func (s *HostSupervisor) PID() int {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	if s.proc == nil || s.proc.Process == nil {
		return 0
	}
	return s.proc.Process.Pid
}

// Whether a spawned child still runs and respawning was not stopped
func (s *HostSupervisor) IsRunning() bool {
	proc, exited, _ := s.procState()
	return proc != nil && !channelClosed(exited) && !s.stoppedRespawning
}

// Child process, its exit signal and its stdin, read together
func (s *HostSupervisor) procState() (*exec.Cmd, chan struct{}, io.WriteCloser) {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	return s.proc, s.exited, s.stdin
}

// Replaces the child process state
func (s *HostSupervisor) setProcState(proc *exec.Cmd, exited chan struct{}, stdin io.WriteCloser) {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	s.proc, s.exited, s.stdin = proc, exited, stdin
}

// Spawns the child unless it runs, reconciling an orphan a previous process left
func (s *HostSupervisor) Start() error { return s.startFn() }

func (s *HostSupervisor) start() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.IsRunning() {
		return nil
	}
	s.closing = false
	s.ReconcileStartupOrphan()
	return s.spawnLocked("startup")
}

// Asks the child to shut down and waits, terminating it when the request fails, then drops the registry
func (s *HostSupervisor) Shutdown() {
	s.lock.Lock()
	s.closing = true
	s.lock.Unlock()
	proc, exited, stdin := s.procState()
	if proc == nil {
		return
	}
	defer s.removeRegistry()
	if !channelClosed(exited) && stdin != nil {
		err := s.sendFrameFn(pyjson.ObjectOf("type", "shutdown", "request_id", "shutdown-"+randomHex(16)))
		if err == nil {
			select {
			case <-exited:
				return
			case <-time.After(hostShutdownTimeout):
			}
		}
	}
	s.terminateProcess(proc, exited)
}

// Terminates a stale registered host unless its pid was reused, returning the outcome name
func (s *HostSupervisor) ReconcileStartupOrphan() string {
	raw, err := os.ReadFile(s.RegistryPath)
	if errors.Is(err, os.ErrNotExist) {
		return "none"
	}
	var decoded any
	valid := false
	if err == nil {
		if value, decodeErr := pyjson.LoadsBytes(raw); decodeErr == nil && value != nil {
			decoded, valid = value, true
		}
	}
	pid := 0
	if data, isDict := decoded.(*pyjson.Object); isDict {
		if value, convErr := pyIntOf(data.GetOr("host_pid", nil)); convErr == nil {
			pid = value
		}
	}
	outcome := ""
	switch {
	case !valid:
		outcome = "invalid-registry"
	case pid <= 0 || !hostPIDAlive(pid):
		outcome = "not-running"
	case !s.pidMatchesHost(pid):
		outcome = "pid-reuse-ignored"
	default:
		s.terminatePID(pid, hostShutdownTimeout)
		outcome = "terminated"
	}
	s.removeRegistry()
	return outcome
}

// Python int of a registry value, zero for a falsy one
func pyIntOf(v any) (int, error) {
	if !truthy(v) {
		return 0, nil
	}
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("Python int too large to convert to C long")
	}
	return int(n.Int64()), nil
}

// Sends a turn to the host, its completion callback firing once with turn.end or turn.error
func (s *HostSupervisor) SubmitTurn(frame *pyjson.Object, onComplete func(*pyjson.Object)) (string, error) {
	if err := s.Start(); err != nil {
		return "", err
	}
	requestID := strOr(frame.GetOr("request_id", nil))
	if requestID == "" {
		requestID = randomHex(16)
	}
	sid := strOr(frame.GetOr("sid", nil))
	payload := pyjson.NewObject()
	for _, key := range frame.Keys {
		payload.Set(key, frame.Vals[key])
	}
	payload.Set("type", "turn.start")
	payload.Set("request_id", requestID)
	s.lock.Lock()
	s.pendingTurns[requestID] = hostPendingTurn{sid: sid, onComplete: onComplete}
	s.lock.Unlock()
	if err := s.sendFrameFn(payload); err != nil {
		s.lock.Lock()
		delete(s.pendingTurns, requestID)
		s.lock.Unlock()
		if onComplete != nil {
			onComplete(pyjson.ObjectOf("type", "turn.error", "sid", sid, "request_id", requestID,
				"reason", "send_failed", "message", err.Error()))
		}
		return "", err
	}
	return requestID, nil
}

// Asks the host to interrupt the session turn
func (s *HostSupervisor) Interrupt(sid, requestID string) error {
	if err := s.Start(); err != nil {
		return err
	}
	if requestID == "" {
		requestID = randomHex(16)
	}
	return s.sendFrameFn(pyjson.ObjectOf("type", "interrupt", "sid", sid, "request_id", requestID))
}

// Whether a host completion callback is still registered for the session
func (s *HostSupervisor) HasPendingTurn(sid string) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	for _, pending := range s.pendingTurns {
		if pending.sid == sid {
			return true
		}
	}
	return false
}

// Sends a frame and waits for the host reply carrying its request id
func (s *HostSupervisor) awaitReply(frame *pyjson.Object, requestID string, timeout time.Duration) (*pyjson.Object, error) {
	q := make(chan *pyjson.Object, 1)
	s.lock.Lock()
	s.pendingControls[requestID] = q
	s.lock.Unlock()
	defer func() {
		s.lock.Lock()
		delete(s.pendingControls, requestID)
		s.lock.Unlock()
	}()
	if err := s.sendFrameFn(frame); err != nil {
		return nil, err
	}
	select {
	case reply := <-q:
		return reply, nil
	case <-time.After(timeout):
		return nil, ErrHostReplyTimeout
	}
}

// Delivers an interactive prompt response to the host that owns it
func (s *HostSupervisor) Respond(sid string, params *pyjson.Object, timeout time.Duration) (*pyjson.Object, error) {
	if err := s.Start(); err != nil {
		return nil, err
	}
	requestID := randomHex(16)
	copied := pyjson.NewObject()
	for _, key := range params.Keys {
		copied.Set(key, params.Vals[key])
	}
	return s.awaitReply(pyjson.ObjectOf("type", "respond", "sid", sid, "request_id", requestID, "params", copied), requestID, timeout)
}

// Asks the host to reload MCP for the session and waits for its ack
func (s *HostSupervisor) ReloadMCP(sid, requestID string) (*pyjson.Object, error) {
	if requestID == "" {
		requestID = randomHex(16)
	}
	return s.Control(sid, "reload.mcp", pyjson.ObjectOf("type", "reload_mcp", "sid", sid, "request_id", requestID), true, 30*time.Second, nil)
}

// Sends a control frame, waiting for its ack when asked and keeping a late ack adoptable after a timeout
func (s *HostSupervisor) Control(sid, routeName string, payload *pyjson.Object, wait bool, timeout time.Duration, onLateAck func(*pyjson.Object)) (*pyjson.Object, error) {
	if _, classified := mutatorRouteTable[routeName]; !classified {
		return nil, fmt.Errorf("unclassified host mutator route: %s", routeName)
	}
	if err := s.Start(); err != nil {
		return nil, err
	}
	if payload == nil {
		payload = pyjson.NewObject()
	}
	requestID := strOr(payload.GetOr("request_id", nil))
	if requestID == "" {
		requestID = randomHex(16)
	}
	frame := pyjson.ObjectOf("type", "control")
	for _, key := range payload.Keys {
		frame.Set(key, payload.Vals[key])
	}
	frame.Set("sid", sid)
	frame.Set("route_name", routeName)
	frame.Set("request_id", requestID)
	if !wait {
		if err := s.sendFrameFn(frame); err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("status", "sent", "request_id", requestID), nil
	}
	reply, err := s.awaitReply(frame, requestID, timeout)
	if errors.Is(err, ErrHostReplyTimeout) && onLateAck != nil {
		s.registerLateControlHandler(requestID, onLateAck)
	}
	return reply, err
}

// Keeps a handler for the late ack of a timed out control, dropping expired and the oldest beyond the cap
func (s *HostSupervisor) registerLateControlHandler(requestID string, handler func(*pyjson.Object)) {
	now := pythread.Monotonic()
	s.lock.Lock()
	defer s.lock.Unlock()
	for rid, entry := range s.lateControlHandlers {
		if now-entry.at > lateControlTTLSecs {
			delete(s.lateControlHandlers, rid)
		}
	}
	for len(s.lateControlHandlers) >= lateControlMax {
		oldest, oldestAt := "", 0.0
		for rid, entry := range s.lateControlHandlers {
			if oldest == "" || entry.at < oldestAt {
				oldest, oldestAt = rid, entry.at
			}
		}
		delete(s.lateControlHandlers, oldest)
	}
	s.lateControlHandlers[requestID] = hostLateControl{at: now, handler: handler}
}

// Hands a control reply to its waiter, else fires and drops its late handler
func (s *HostSupervisor) deliverControlFrame(requestID string, frame *pyjson.Object) {
	s.lock.Lock()
	q := s.pendingControls[requestID]
	var late *hostLateControl
	if q == nil {
		if entry, found := s.lateControlHandlers[requestID]; found {
			delete(s.lateControlHandlers, requestID)
			late = &entry
		}
	}
	s.lock.Unlock()
	if q != nil {
		select {
		case q <- frame:
		default:
		}
	} else if late != nil {
		hostCallLogged(late.handler, frame, fmt.Sprintf("compute host late control ack handler failed (request_id=%s)", requestID))
	}
}

// Child environment of the host, the sanitized subprocess env under the live env and the overrides
func (s *HostSupervisor) childEnv() []string {
	env := tools.IcebathSubprocessEnv(nil, true, nil)
	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		env[key] = value
	}
	maps.Copy(env, s.Env)
	env["ICEBATH_COMPUTE_HOST_HEARTBEAT_SECS"] = fmt.Sprint(s.HeartbeatSecs)
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	return out
}

// Spawns the child, waits for its hello, validates it and records the registry
func (s *HostSupervisor) spawnLocked(reason string) error {
	if s.stoppedRespawning {
		return errors.New("compute host respawn disabled after crash loop")
	}
	s.helloEvent.Clear()
	s.hello = pyjson.NewObject()
	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.Dir = s.Cwd
	cmd.Env = s.childEnv()
	cmd.SysProcAttr = hostNewSessionAttr()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	s.setProcState(cmd, exited, stdin)
	go s.drainStdout(stdout)
	go s.drainStderr(stderr)
	go s.waitForExit(cmd, exited)
	if !s.helloEvent.Wait(hostHelloTimeoutSecs) {
		s.terminateProcess(cmd, exited)
		s.stderrMu.Lock()
		tail := append([]string(nil), s.stderrTail...)
		s.stderrMu.Unlock()
		if len(tail) > 5 {
			tail = tail[len(tail)-5:]
		}
		parts := make([]string, len(tail))
		for i, line := range tail {
			parts[i] = pyrepr.Repr(line)
		}
		return fmt.Errorf("compute host did not send hello; stderr=[%s]", strings.Join(parts, ", "))
	}
	if err := s.validateHello(); err != nil {
		return err
	}
	if err := s.persistRegistry(); err != nil {
		return err
	}
	logger().Info(fmt.Sprintf("compute host started pid=%d reason=%s", cmd.Process.Pid, reason))
	return nil
}

// Refuses a hello from another home or another build
func (s *HostSupervisor) validateHello() error {
	hello := s.hello
	if hello == nil || hello.Len() == 0 {
		return errors.New("compute host missing hello")
	}
	gotHome := strOr(hello.GetOr("hermes_home", nil))
	if gotHome != "" && gotHome != s.ExpectedHermesHome {
		return fmt.Errorf("compute host ICEBATH_HOME mismatch: %s != %s", gotHome, s.ExpectedHermesHome)
	}
	gotSHA := strOr(hello.GetOr("build_sha", nil))
	expected := s.ExpectedBuildSHA
	if expected != "unknown" && gotSHA != "" && gotSHA != "unknown" && gotSHA != expected {
		return fmt.Errorf("compute host build mismatch: %s != %s", gotSHA, expected)
	}
	return nil
}

// Writes the registry of the running child atomically
func (s *HostSupervisor) persistRegistry() error {
	if err := os.MkdirAll(filepath.Dir(s.RegistryPath), 0o777); err != nil {
		return err
	}
	argv := make([]any, len(s.Argv))
	for i, arg := range s.Argv {
		argv[i] = arg
	}
	pid := s.PID()
	payload := pyjson.ObjectOf("host_pid", pid, "boot_id", orText(s.hello.GetOr("boot_id", nil)),
		"build_sha", orText(s.hello.GetOr("build_sha", nil)), "started_at", float64(time.Now().UnixNano())/1e9, "argv", argv)
	text, err := pyjson.Dumps(payload, pyjson.SortKeys(true))
	if err != nil {
		return err
	}
	tmp := s.RegistryPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, s.RegistryPath)
}

// Value when truthy, else the empty string
func orText(v any) any {
	if truthy(v) {
		return v
	}
	return ""
}

// Deletes the registry file, logging a failure
func (s *HostSupervisor) removeRegistry() {
	if err := os.Remove(s.RegistryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger().Debug(fmt.Sprintf("failed to remove compute host registry: %s", err))
	}
}

// Writes one compact JSON line to the child, an error when it is not running
func (s *HostSupervisor) sendFrame(frame *pyjson.Object) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	proc, exited, stdin := s.procState()
	if proc == nil || channelClosed(exited) || stdin == nil {
		return errors.New("compute host is not running")
	}
	text, err := pyjson.Dumps(frame, pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdin, text+"\n")
	return err
}

// Reads JSON lines from the child and handles every object frame
func (s *HostSupervisor) drainStdout(stdout io.Reader) {
	reader := bufio.NewReader(stdout)
	for {
		raw, err := reader.ReadString('\n')
		if raw != "" {
			line := strings.ToValidUTF8(raw, "�")
			decoded, decodeErr := pyjson.Loads(line)
			if decodeErr != nil {
				preview := []rune(line)
				if len(preview) > 200 {
					preview = preview[:200]
				}
				logger().Warn(fmt.Sprintf("compute host emitted invalid json: %s", pyrepr.Repr(string(preview))))
			} else if frame, isDict := decoded.(*pyjson.Object); isDict {
				s.handleHostFrame(frame)
			}
		}
		if err != nil {
			return
		}
	}
}

// Keeps the last eighty stderr lines of the child and logs each one
func (s *HostSupervisor) drainStderr(stderr io.Reader) {
	reader := bufio.NewReader(stderr)
	for {
		raw, err := reader.ReadString('\n')
		if text := strings.TrimRight(strings.ToValidUTF8(raw, "�"), "\n"); text != "" {
			s.stderrMu.Lock()
			s.stderrTail = append(s.stderrTail, text)
			if len(s.stderrTail) > 80 {
				s.stderrTail = s.stderrTail[len(s.stderrTail)-80:]
			}
			s.stderrMu.Unlock()
			logger().Warn(fmt.Sprintf("compute host stderr: %s", text))
		}
		if err != nil {
			return
		}
	}
}

// Routes one child frame to its waiter, the hello, the heartbeat, the RPC sink or a turn completion
func (s *HostSupervisor) handleHostFrame(frame *pyjson.Object) {
	ftype := strOr(frame.GetOr("type", nil))
	requestID := strOr(frame.GetOr("request_id", nil))
	switch {
	case hostControlReplyTypes[ftype] || (ftype == "error" && requestID != ""):
		s.deliverControlFrame(requestID, frame)
	case ftype == "hello":
		copied := pyjson.NewObject()
		for _, key := range frame.Keys {
			copied.Set(key, frame.Vals[key])
		}
		s.hello = copied
		s.helloEvent.Set()
	case ftype == "hb":
		if counter, err := pyIntOf(frame.GetOr("progress_counter", nil)); err == nil && counter != 0 {
			s.lastProgressCounter = counter
		}
		logger().Debug(fmt.Sprintf("compute host heartbeat: %s", pyrepr.Repr(frame)))
	case ftype == "rpc":
		if message, isDict := frame.GetOr("message", nil).(*pyjson.Object); isDict {
			s.rpcSink(message)
		}
	case ftype == "turn.end" || ftype == "turn.error":
		s.lock.Lock()
		pending, found := s.pendingTurns[requestID]
		delete(s.pendingTurns, requestID)
		s.lock.Unlock()
		if found && pending.onComplete != nil {
			hostCallLogged(pending.onComplete, frame, "compute host turn completion callback failed")
		}
	}
}

// Reaps the child and, unless closing or replaced, fails its turns and respawns within the crash budget
func (s *HostSupervisor) waitForExit(cmd *exec.Cmd, exited chan struct{}) {
	err := cmd.Wait()
	close(exited)
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	} else if err != nil {
		code = -1
	}
	if s.closing {
		return
	}
	s.lock.Lock()
	if proc, _, _ := s.procState(); proc != cmd {
		s.lock.Unlock()
		return
	}
	s.setProcState(nil, nil, nil)
	s.lock.Unlock()
	s.removeRegistry()
	s.failPendingTurns("crash", fmt.Sprintf("compute host exited with code %d", code))
	s.maybeRespawnAfterCrash()
}

// Emits an error for every pending turn, fires their callbacks and fails every late control handler
func (s *HostSupervisor) failPendingTurns(reason, message string) {
	s.lock.Lock()
	pending := s.pendingTurns
	s.pendingTurns = map[string]hostPendingTurn{}
	s.lock.Unlock()
	for requestID, turn := range pending {
		s.rpcSink(pyjson.ObjectOf("jsonrpc", "2.0", "method", "event",
			"params", pyjson.ObjectOf("type", "error", "session_id", turn.sid, "payload", pyjson.ObjectOf("reason", reason, "message", message))))
		if turn.onComplete != nil {
			hostCallLogged(turn.onComplete, pyjson.ObjectOf("type", "turn.error", "sid", turn.sid, "request_id", requestID,
				"reason", reason, "message", message), "compute host error callback failed")
		}
	}
	s.lock.Lock()
	late := s.lateControlHandlers
	s.lateControlHandlers = map[string]hostLateControl{}
	s.lock.Unlock()
	for requestID, entry := range late {
		hostCallLogged(entry.handler, pyjson.ObjectOf("type", "control.error", "request_id", requestID, "reason", reason, "message", message),
			"compute host late control error handler failed")
	}
}

// Respawns after a short backoff unless the restart budget of the window is spent
func (s *HostSupervisor) maybeRespawnAfterCrash() {
	now := pythread.Monotonic()
	s.lock.Lock()
	kept := s.restartTimes[:0]
	for _, at := range s.restartTimes {
		if now-at <= hostRespawnWindowSecs {
			kept = append(kept, at)
		}
	}
	s.restartTimes = kept
	if len(s.restartTimes) >= s.RespawnMax {
		s.stoppedRespawning = true
		s.lock.Unlock()
		logger().Error(fmt.Sprintf("compute host crash loop: max %d restarts per 5min reached; not respawning", s.RespawnMax))
		return
	}
	s.restartTimes = append(s.restartTimes, now)
	delay := min(5.0, 0.25*float64(int(1)<<max(0, len(s.restartTimes)-1)))
	s.lock.Unlock()
	go func() {
		defer threadExceptHook("compute-host-respawn")
		time.Sleep(time.Duration(delay * float64(time.Second)))
		s.lock.Lock()
		defer s.lock.Unlock()
		if proc, _, _ := s.procState(); s.closing || s.stoppedRespawning || proc != nil {
			return
		}
		if err := s.spawnLocked("crash"); err != nil {
			logger().Error(fmt.Sprintf("compute host respawn failed: %s", err))
		}
	}()
}

// Sends SIGTERM and escalates to SIGKILL when the pid outlives the timeout
func (s *HostSupervisor) terminatePID(pid int, timeout time.Duration) {
	if !hostSignalPID(pid, false, "SIGTERM") {
		return
	}
	deadline := time.Now().Add(timeout)
	for hostPIDAlive(pid) {
		if !time.Now().Before(deadline) {
			hostSignalPID(pid, true, "SIGKILL")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Terminates a running child and waits for it, killing it when it does not exit in time
func (s *HostSupervisor) terminateProcess(cmd *exec.Cmd, exited chan struct{}) {
	if channelClosed(exited) {
		return
	}
	if cmd.Process != nil && hostSignalPID(cmd.Process.Pid, false, "SIGTERM") {
		select {
		case <-exited:
			return
		case <-time.After(hostShutdownTimeout):
		}
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
	}
}
