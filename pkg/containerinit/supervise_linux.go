package containerinit

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Child table resolving every reaped pid to the callback of the code that started it
type reaper struct {
	mu     sync.Mutex
	owners map[int]func(syscall.WaitStatus)
}

func newReaper() *reaper { return &reaper{owners: map[int]func(syscall.WaitStatus){}} }

// Starts argv and registers onExit before the reaper can observe the pid
func (r *reaper) spawn(argv []string, attr *syscall.ProcAttr, onExit func(syscall.WaitStatus)) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pid, err := syscall.ForkExec(argv[0], argv, attr)
	if err != nil {
		return 0, err
	}
	r.owners[pid] = onExit
	return pid, nil
}

// Collects every exited child, handing owned pids to their callbacks and discarding adopted orphans
func (r *reaper) reap() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || pid <= 0 {
			return
		}
		r.mu.Lock()
		cb := r.owners[pid]
		delete(r.owners, pid)
		r.mu.Unlock()
		if cb != nil {
			cb(ws)
		}
	}
}

// Exit code and signal of a wait status, code 256 when a signal ended the process
func exitOf(ws syscall.WaitStatus) (int, int) {
	if ws.Signaled() {
		return 256, int(ws.Signal())
	}
	return ws.ExitStatus(), 0
}

// Persisted state of one supervised service the svstat and svwait verbs read
type svcStatus struct {
	Up         bool  `json:"up"`
	Pid        int   `json:"pid"`
	Pgid       int   `json:"pgid"`
	WantUp     bool  `json:"wantedup"`
	NormallyUp bool  `json:"normallyup"`
	Ready      bool  `json:"ready"`
	Paused     bool  `json:"paused"`
	Finishing  bool  `json:"finishing"`
	ExitCode   int   `json:"exitcode"`
	Signal     int   `json:"signal"`
	Since      int64 `json:"updownsince"`
	ReadySince int64 `json:"readysince"`
}

func statusPath(dir string) string { return filepath.Join(dir, "supervise", "status.json") }

// Reads the persisted state of a service directory
func readStatus(dir string) (svcStatus, error) {
	var st svcStatus
	raw, err := os.ReadFile(statusPath(dir))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(raw, &st)
}

// Go side definition of a service the image declares without a run script
type nativeSpec struct {
	command func() ([]string, *syscall.ProcAttr, error)
	enabled func() bool
	finish  func(code, sig int) int
	ready   func() bool
}

// One supervised service directory with its optional logger
type service struct {
	sv       *supervisor
	name     string
	dir      string
	native   *nativeSpec
	log      *service
	isLog    bool
	pipeR    *os.File
	pipeW    *os.File
	mu       sync.Mutex
	st       svcStatus
	started  time.Time
	once     bool
	exitDown bool
	retired  bool
	ended    bool
	control  *os.File
	timer    *time.Timer
	backoff  time.Duration
}

// A run living this long resets the restart backoff, which never grows past the cap
const (
	restartHealthyAfter = 10 * time.Second
	restartBackoffCap   = time.Minute
)

// Writes the state atomically so readers never see a partial file
func (s *service) writeStatusLocked() {
	dir := filepath.Join(s.dir, "supervise")
	raw, err := json.Marshal(s.st)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".status-")
	if err != nil {
		return
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Chmod(tmp.Name(), 0o644) != nil || os.Rename(tmp.Name(), statusPath(s.dir)) != nil {
		os.Remove(tmp.Name())
	}
	s.sv.notify()
}

// Standard streams of the service, its stdout feeding the logger pipe when it has one
func (s *service) files() []uintptr {
	stdin := uintptr(s.sv.devnull.Fd())
	if s.isLog && s.pipeR != nil {
		stdin = s.pipeR.Fd()
	}
	stdout := os.Stdout.Fd()
	if s.pipeW != nil {
		stdout = s.pipeW.Fd()
	}
	return []uintptr{stdin, stdout, os.Stderr.Fd()}
}

// Argv and attributes of the run script, executed in its own session from the service directory
func (s *service) command() ([]string, *syscall.ProcAttr, error) {
	if s.native != nil {
		argv, attr, err := s.native.command()
		if err == nil {
			attr.Files = s.files()
		}
		return argv, attr, err
	}
	return []string{filepath.Join(s.dir, "run")}, &syscall.ProcAttr{
		Dir: s.dir, Env: os.Environ(), Files: s.files(), Sys: &syscall.SysProcAttr{Setsid: true},
	}, nil
}

// Starts the run program unless it is already running, finishing or retired
func (s *service) startLocked() {
	if s.st.Pid != 0 || s.st.Finishing || s.retired || s.ended || s.sv.stopping() {
		return
	}
	if s.native != nil && !s.native.enabled() {
		s.st.WantUp, s.st.ExitCode, s.st.Signal, s.st.Since = false, 0, 0, time.Now().UnixNano()
		s.writeStatusLocked()
		return
	}
	argv, attr, err := s.command()
	var pid int
	if err == nil {
		pid, err = s.sv.reaper.spawn(argv, attr, s.onExit)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "s6-supervise %s: warning: unable to spawn ./run - waiting 10 seconds: %v\n", s.name, err)
		s.scheduleLocked(10 * time.Second)
		return
	}
	now := time.Now()
	s.started = now
	s.st.Up, s.st.Pid, s.st.Pgid, s.st.Paused = true, pid, pid, false
	s.st.Since = now.UnixNano()
	s.st.Ready = s.native == nil || s.native.ready == nil
	if s.st.Ready {
		s.st.ReadySince = s.st.Since
	}
	s.writeStatusLocked()
	if !s.st.Ready {
		go s.probe(pid)
	}
}

// Polls the readiness check until it passes or the process it was started for goes away
func (s *service) probe(pid int) {
	for {
		time.Sleep(250 * time.Millisecond)
		s.mu.Lock()
		if s.st.Pid != pid {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		if !s.native.ready() {
			continue
		}
		s.mu.Lock()
		if s.st.Pid == pid {
			s.st.Ready, s.st.ReadySince = true, time.Now().UnixNano()
			s.writeStatusLocked()
		}
		s.mu.Unlock()
		return
	}
}

// Starts the service again after delay unless something else starts or retires it first
func (s *service) scheduleLocked(delay time.Duration) {
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(delay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.st.WantUp {
			s.startLocked()
		}
	})
}

// Records the exit and runs the finish program with the exit code and signal
func (s *service) onExit(ws syscall.WaitStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	code, sig := exitOf(ws)
	ran := time.Since(s.started)
	s.st.Up, s.st.Pid, s.st.Pgid, s.st.Ready, s.st.Paused = false, 0, 0, false, false
	s.st.ExitCode, s.st.Signal, s.st.Since = code, sig, time.Now().UnixNano()
	if ws.Signaled() {
		s.st.ExitCode = -1
	}
	s.st.Finishing = true
	s.writeStatusLocked()
	if s.native != nil {
		s.afterFinishLocked(s.native.finish(code, sig), ran)
		return
	}
	finish := filepath.Join(s.dir, "finish")
	if info, err := os.Stat(finish); err != nil || info.Mode()&0o111 == 0 {
		s.afterFinishLocked(0, ran)
		return
	}
	attr := &syscall.ProcAttr{Dir: s.dir, Env: os.Environ(), Files: s.files(), Sys: &syscall.SysProcAttr{Setsid: true}}
	pid, err := s.sv.reaper.spawn([]string{finish, strconv.Itoa(code), strconv.Itoa(sig)}, attr, func(fs syscall.WaitStatus) {
		s.mu.Lock()
		defer s.mu.Unlock()
		fcode, _ := exitOf(fs)
		s.afterFinishLocked(fcode, ran)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "s6-supervise %s: warning: unable to spawn ./finish: %v\n", s.name, err)
		s.afterFinishLocked(0, ran)
		return
	}
	limit := 5 * time.Second
	if raw, err := os.ReadFile(filepath.Join(s.dir, "timeout-finish")); err == nil {
		if ms, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && ms > 0 {
			limit = time.Duration(ms) * time.Millisecond
		}
	}
	time.AfterFunc(limit, func() { syscall.Kill(pid, syscall.SIGKILL) })
}

// Applies the finish verdict, exit 125 marking the service down for good, then restarts a wanted service
func (s *service) afterFinishLocked(code int, ran time.Duration) {
	s.st.Finishing = false
	if code == 125 || s.once {
		s.st.WantUp, s.once = false, false
	}
	s.writeStatusLocked()
	if s.exitDown || s.retired {
		s.endLocked()
		return
	}
	if s.st.WantUp {
		s.scheduleLocked(s.restartDelayLocked(ran))
	}
}

// Delay before the next start, at least a second after the last one and doubling up to a minute while runs keep dying young
func (s *service) restartDelayLocked(ran time.Duration) time.Duration {
	if ran >= restartHealthyAfter {
		s.backoff = 0
		return max(0, time.Second-ran)
	}
	s.backoff = min(max(time.Second, s.backoff*2), restartBackoffCap)
	return s.backoff
}

// Sends sig to the running process, false when nothing runs
func (s *service) signalLocked(sig syscall.Signal) bool {
	if s.st.Pid == 0 {
		return false
	}
	return syscall.Kill(s.st.Pid, sig) == nil
}

// Applies one s6-svc command letter
func (s *service) command1(letter byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	signals := map[byte]syscall.Signal{'t': syscall.SIGTERM, 'r': syscall.SIGTERM, 'h': syscall.SIGHUP, 'i': syscall.SIGINT,
		'q': syscall.SIGQUIT, '1': syscall.SIGUSR1, '2': syscall.SIGUSR2, 'a': syscall.SIGALRM, 'k': syscall.SIGKILL,
		'y': syscall.SIGWINCH}
	switch letter {
	case 'u':
		s.st.WantUp, s.once, s.backoff = true, false, 0
		s.startLocked()
	case 'o':
		s.st.WantUp, s.once = true, true
		s.startLocked()
	case 'O':
		s.once = true
	case 'd':
		s.st.WantUp, s.once = false, false
		if s.timer != nil {
			s.timer.Stop()
		}
		s.signalLocked(syscall.SIGTERM)
		s.signalLocked(syscall.SIGCONT)
	case 'x':
		s.exitDown = true
		if s.st.Pid == 0 && !s.st.Finishing {
			s.endLocked()
			return
		}
	case 'p':
		if s.signalLocked(syscall.SIGSTOP) {
			s.st.Paused = true
		}
	case 'c':
		if s.signalLocked(syscall.SIGCONT) {
			s.st.Paused = false
		}
	default:
		if sig, ok := signals[letter]; ok {
			s.signalLocked(sig)
		}
	}
	s.writeStatusLocked()
}

// Opens the control fifo, creating it account owned when the directory did not seed one
func (s *service) openControl() error {
	sup := filepath.Join(s.dir, "supervise")
	if err := os.MkdirAll(sup, 0o755); err != nil {
		return err
	}
	ctl := filepath.Join(sup, "control")
	if _, err := os.Stat(ctl); errors.Is(err, os.ErrNotExist) {
		if err := syscall.Mkfifo(ctl, 0o660); err != nil {
			return err
		}
		os.Chmod(ctl, 0o660)
		if a, err := lookupAccount(accountName); err == nil {
			os.Chown(ctl, a.uid, a.gid)
		}
	}
	f, err := os.OpenFile(ctl, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	s.control = f
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := f.Read(buf)
			if err != nil {
				return
			}
			for _, b := range buf[:n] {
				s.command1(b)
			}
		}
	}()
	return nil
}

// Begins supervision, the logger first so the pipe has a reader before the service writes
func (s *service) begin(normallyUp bool) {
	if s.log != nil {
		s.log.begin(true)
	}
	if err := s.openControl(); err != nil {
		fmt.Fprintf(os.Stderr, "s6-supervise %s: warning: unable to open control fifo: %v\n", s.name, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.NormallyUp, s.st.WantUp, s.st.ExitCode = normallyUp, normallyUp, -1
	s.st.Since = time.Now().UnixNano()
	s.writeStatusLocked()
	if normallyUp {
		s.startLocked()
	}
}

// Ends supervision of a stopped service, closing its fifo and handing its logger an end of file
func (s *service) endLocked() {
	if s.ended {
		return
	}
	s.ended = true
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.control != nil {
		s.control.Close()
	}
	if s.pipeW != nil {
		s.pipeW.Close()
	}
	if s.isLog && s.pipeR != nil {
		s.pipeR.Close()
	}
	if s.log != nil {
		go func(l *service) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.retired = true
			l.st.WantUp = false
			if l.st.Pid == 0 && !l.st.Finishing {
				l.endLocked()
			}
		}(s.log)
	}
	go s.sv.forget(s)
}

// Down or unwanted state for the stop waiters, true once nothing runs or finishes
func (s *service) settled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Pid == 0 && !s.st.Finishing
}

// Scan directory supervisor holding every service and the shared reaper
type supervisor struct {
	dir      string
	reaper   *reaper
	devnull  *os.File
	mu       sync.Mutex
	services map[string]*service
	changed  chan struct{}
	halt     chan struct{}
	haltOnce sync.Once
	down     bool
}

func newSupervisor(dir string, r *reaper) (*supervisor, error) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	return &supervisor{dir: dir, reaper: r, devnull: devnull, services: map[string]*service{},
		changed: make(chan struct{}, 1), halt: make(chan struct{})}, nil
}

func (sv *supervisor) notify() {
	select {
	case sv.changed <- struct{}{}:
	default:
	}
}

func (sv *supervisor) stopping() bool {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return sv.down
}

func (sv *supervisor) forget(s *service) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if sv.services[s.name] == s {
		delete(sv.services, s.name)
	}
}

// Registers a service directory and its logger, starting it unless a down file asks otherwise
func (sv *supervisor) adopt(name, dir string, native *nativeSpec) *service {
	s := &service{sv: sv, name: name, dir: dir, native: native}
	if info, err := os.Stat(filepath.Join(dir, "log", "run")); err == nil && info.Mode()&0o111 != 0 && native == nil {
		r, w, err := os.Pipe()
		if err == nil {
			s.pipeW = w
			s.log = &service{sv: sv, name: name + "/log", dir: filepath.Join(dir, "log"), isLog: true, pipeR: r}
		}
	}
	sv.services[name] = s
	_, downErr := os.Stat(filepath.Join(dir, "down"))
	go s.begin(errors.Is(downErr, os.ErrNotExist))
	return s
}

// Picks up new service directories and, when prune is set, retires services whose directory vanished
func (sv *supervisor) scan(prune bool) {
	entries, err := os.ReadDir(sv.dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "s6-svscan: warning: unable to scan %s: %v\n", sv.dir, err)
		return
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if sv.down {
		return
	}
	present := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(sv.dir, name)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		present[name] = true
		if _, ok := sv.services[name]; ok {
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, "run")); err != nil || info.Mode()&0o111 == 0 {
			continue
		}
		sv.adopt(name, dir, nil)
	}
	if !prune {
		return
	}
	for name, s := range sv.services {
		if present[name] {
			continue
		}
		go func(s *service) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.retired, s.st.WantUp = true, false
			if !s.signalLocked(syscall.SIGTERM) && !s.st.Finishing {
				s.endLocked()
				return
			}
			s.signalLocked(syscall.SIGCONT)
		}(s)
	}
}

// Serves the scan directory control fifo, a and h rescanning, n pruning and the halt letters stopping the container
func (sv *supervisor) listen() error {
	ctlDir := filepath.Dir(svscanControl(sv.dir))
	if err := os.MkdirAll(ctlDir, 0o755); err != nil {
		return err
	}
	ctl := svscanControl(sv.dir)
	if _, err := os.Stat(ctl); errors.Is(err, os.ErrNotExist) {
		if err := syscall.Mkfifo(ctl, 0o660); err != nil {
			return err
		}
	}
	os.Chmod(ctl, 0o660)
	f, err := os.OpenFile(ctl, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := f.Read(buf)
			if err != nil {
				return
			}
			cmd := string(buf[:n])
			if strings.ContainsAny(cmd, "tsqpi0678") {
				sv.haltOnce.Do(func() { close(sv.halt) })
			}
			if strings.ContainsAny(cmd, "ahnN") {
				sv.scan(strings.ContainsAny(cmd, "nN"))
			}
		}
	}()
	return nil
}

// Stops every service, loggers last, killing what outlives grace
func (sv *supervisor) stopAll(grace time.Duration) {
	sv.mu.Lock()
	sv.down = true
	var mains, logs []*service
	for _, s := range sv.services {
		mains = append(mains, s)
		if s.log != nil {
			logs = append(logs, s.log)
		}
	}
	sv.mu.Unlock()
	sort.Slice(mains, func(i, j int) bool { return mains[i].name < mains[j].name })
	sv.stopGroup(mains, grace)
	for _, s := range mains {
		s.mu.Lock()
		s.endLocked()
		s.mu.Unlock()
	}
	sv.stopGroup(logs, grace)
}

// Sends down to each service and waits for them to settle, then kills any that did not
func (sv *supervisor) stopGroup(group []*service, grace time.Duration) {
	for _, s := range group {
		s.command1('d')
	}
	deadline := time.After(grace)
	for {
		all := true
		for _, s := range group {
			if !s.settled() {
				all = false
				break
			}
		}
		if all {
			return
		}
		select {
		case <-sv.changed:
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			for _, s := range group {
				s.command1('k')
			}
			return
		}
	}
}

// TCP probe target for a listen host, the wildcard addresses mapped to loopback
func probeAddr(host, port string) string {
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), port)
}
