package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	administratorsSID    = "S-1-5-32-544"
	accessAllowedACEType = 0
	nativePipeInstances  = 8
)

var errPipeConnection = errors.New("connection error")

// Win32 pipe failure carrying the Python exception class it stands for
type nativePipeError struct {
	msg  string
	kind error
}

func (e *nativePipeError) Error() string { return e.msg }
func (e *nativePipeError) Unwrap() error { return e.kind }

func pipePermissionError(msg string) error { return &nativePipeError{msg, fs.ErrPermission} }

func pipeDeadlineError() error {
	return &nativePipeError{"runtime control deadline exceeded", ErrControlTimeout}
}

// RevertToSelf failed so the serving goroutine may still carry the client identity and must stop
type impersonationLeakError struct{}

func (impersonationLeakError) Error() string {
	return "RevertToSelf failed after reading a named-pipe peer"
}

// One DACL entry of a pipe object as its ACE type and SID text
type pipeACE struct {
	Kind int
	SID  string
}

// Owner SID and DACL entries of a pipe object
type pipeSecurity struct {
	Owner string
	ACEs  []pipeACE
}

// Win32 facts the pipe identity check reads, real on Windows and injected in tests
type pipePeerFacts interface {
	pipeClientSID(handle uintptr) (string, error)
	namedPipeProcessID(handle uintptr, server bool) (uint32, bool)
	processSID(pid uint32) (string, error)
	pipeSecurity(handle uintptr) (*pipeSecurity, error)
	pipeServerSession(handle uintptr) *uint32
	sessionOf(pid uint32) *uint32
	processElevation(pid uint32) *bool
	processIsElevated() bool
	ownPID() uint32
}

// Facts of one pipe server as judgePipeServer weighs them
type pipeServerFacts struct {
	OwnSID         string
	ProcessSID     *string
	Pipe           *pipeSecurity
	PID            int
	OwnElevated    bool
	OwnSession     *uint32
	ServerSession  *uint32
	ServerElevated *bool
}

func pipeOptText[T any](v *T) string {
	if v == nil {
		return "None"
	}
	switch x := any(*v).(type) {
	case bool:
		if x {
			return "True"
		}
		return "False"
	case uint32:
		return strconv.FormatUint(uint64(x), 10)
	case string:
		return x
	}
	return fmt.Sprint(*v)
}

// sid of this user when the client may talk to the pipe server, a refusal naming the boundary otherwise
func judgePipeServer(f pipeServerFacts) (string, error) {
	var owner *string
	var aces []pipeACE
	if f.Pipe != nil {
		owner, aces = &f.Pipe.Owner, f.Pipe.ACEs
	}
	var ownerElevated *bool
	yes := true
	if f.ProcessSID != nil {
		if *f.ProcessSID != f.OwnSID {
			return "", pipePermissionError("named-pipe peer belongs to another user")
		}
		if f.ServerElevated == nil {
			admin := owner != nil && *owner == administratorsSID
			f.ServerElevated = &admin
		}
		if f.OwnElevated || !*f.ServerElevated {
			return "sid:" + f.OwnSID, nil
		}
		ownerElevated = &yes
	} else {
		if f.Pipe == nil {
			return "", pipePermissionError("cannot verify bootstrap peer process")
		}
		if (*owner != f.OwnSID && *owner != administratorsSID) || len(aces) != 1 ||
			aces[0] != (pipeACE{accessAllowedACEType, f.OwnSID}) {
			return "", pipePermissionError("named-pipe peer belongs to another user")
		}
		if *owner == administratorsSID {
			ownerElevated = &yes
		}
	}
	reason := ClassifyRefusedOwner(f.OwnSession, f.ServerSession, f.OwnElevated, ownerElevated)
	if reason == "" {
		return "", pipePermissionError("cannot verify bootstrap peer process")
	}
	readable := "unreadable"
	if f.ProcessSID != nil && *f.ProcessSID != "" {
		readable = "readable"
	}
	slog.Info(fmt.Sprintf("control pipe server PID %d refused as %s: pipe owner %s, server process %s, server elevated %s, server session %s, own session %s",
		f.PID, reason, pipeOptText(owner), readable, pipeOptText(f.ServerElevated), pipeOptText(f.ServerSession), pipeOptText(f.OwnSession)))
	return "", &RefusedOwner{ReasonCode: reason, Pid: f.PID, Remedy: RefusedOwnerRemedy(reason, f.PID, f.ServerSession)}
}

// Client half of the pipe identity check gathering the Win32 facts for judgePipeServer
func serverSubject(w pipePeerFacts, handle uintptr, pid uint32) (string, error) {
	var processSID *string
	if sid, err := w.processSID(pid); err == nil {
		processSID = &sid
	} else if !errors.Is(err, fs.ErrPermission) {
		return "", err
	}
	var serverElevated *bool
	if processSID != nil {
		serverElevated = w.processElevation(pid)
	}
	pipe, err := w.pipeSecurity(handle)
	if err != nil {
		if !errors.Is(err, fs.ErrPermission) {
			return "", err
		}
		pipe = nil
	}
	ownSID, err := w.processSID(w.ownPID())
	if err != nil {
		return "", err
	}
	return judgePipeServer(pipeServerFacts{
		OwnSID: ownSID, ProcessSID: processSID, Pipe: pipe, PID: int(pid), OwnElevated: w.processIsElevated(),
		OwnSession: w.sessionOf(w.ownPID()), ServerSession: w.pipeServerSession(handle), ServerElevated: serverElevated,
	})
}

// sid of a same user peer, a refusal for another user or an unidentified peer
func peerSubject(w pipePeerFacts, handle uintptr, server bool) (string, error) {
	sid, known := "", false
	if !server {
		got, err := w.pipeClientSID(handle)
		if err == nil {
			sid, known = got, true
		} else if !errors.Is(err, fs.ErrPermission) {
			return "", err
		}
	}
	if !known {
		pid, ok := w.namedPipeProcessID(handle, server)
		if !ok {
			return "", pipePermissionError("cannot identify named-pipe peer")
		}
		if server {
			return serverSubject(w, handle, pid)
		}
		got, err := w.processSID(pid)
		if err != nil {
			return "", err
		}
		sid = got
	}
	own, err := w.processSID(w.ownPID())
	if err != nil {
		return "", err
	}
	if sid != own {
		return "", pipePermissionError("named-pipe peer belongs to another user")
	}
	return "sid:" + sid, nil
}

// Runs fn on a worker under the deadline, the thunk yields its value or error, true when abandoned
func runBoundedSync[T any](fn func() (T, error), timeout time.Duration, label string) (func() (T, error), bool) {
	bound, bounded := provider.ClampTimeout(timeout.Seconds())
	res, err := provider.RunBounded(context.Background(), func(context.Context) (any, error) { return fn() }, bound, bounded, label, nil, nil)
	if res.TimedOut {
		return nil, true
	}
	return func() (T, error) {
		v, _ := res.Value.(T)
		return v, err
	}, false
}

var windowsPipeExchangeSeam = queryRuntimeControl

// One control pipe exchange abandoned at the deadline, nil when the peer never answers in time
func queryWindowsPipe(home string, request []byte, timeout time.Duration) ([]byte, error) {
	exchange := windowsPipeExchangeSeam
	value, timedOut := runBoundedSync(func() ([]byte, error) { return exchange(home, request, timeout) }, timeout, "control-pipe")
	if timedOut {
		return nil, nil
	}
	return value()
}

// One listening control pipe instance served by its own worker
type nativePipeInstance interface {
	accept(budget time.Duration) (bool, error)
	readLine(deadline time.Time, maximum int) ([]byte, error)
	write(data []byte, deadline time.Time) error
	peerSubject() (string, error)
	disconnect() error
	close()
}

var createNativePipeSeam = createNativePipe

// Bounded native pipe workers serving control requests independent of any event loop
type NativeControlServer struct {
	home      string
	handler   func(raw []byte, peerSubject string) []byte
	stop      chan struct{}
	stopOnce  sync.Once
	ready     chan struct{}
	readyOnce sync.Once
	mu        sync.Mutex
	err       error
	done      [nativePipeInstances]chan struct{}
}

// Server over home's control pipe answering each request line through handler
func NewNativeControlServer(home string, handler func(raw []byte, peerSubject string) []byte) *NativeControlServer {
	return &NativeControlServer{home: home, handler: handler, stop: make(chan struct{}), ready: make(chan struct{})}
}

// Error the most recent worker stopped on, nil while every worker serves
func (s *NativeControlServer) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *NativeControlServer) launch(index int) {
	s.done[index] = make(chan struct{})
	go s.run(index)
}

// First instance claims the pipe name before the others start, failing when it cannot within five seconds
func (s *NativeControlServer) Start() error {
	s.launch(0)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-s.ready:
	case <-timer.C:
		_ = s.Close()
		return &nativePipeError{"runtime control pipe startup deadline exceeded", ErrControlTimeout}
	}
	if err := s.Err(); err != nil {
		return err
	}
	for i := 1; i < nativePipeInstances; i++ {
		s.launch(i)
	}
	return nil
}

// Stops every worker, failing when one has not stopped five seconds after its turn
func (s *NativeControlServer) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	alive := false
	for _, done := range s.done {
		if done == nil {
			continue
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-done:
		case <-timer.C:
			alive = true
		}
		timer.Stop()
	}
	if alive {
		return &nativePipeError{"runtime control pipe did not stop", ErrControlTimeout}
	}
	return nil
}

// Alive reports whether the worker at index has started and not yet stopped
func (s *NativeControlServer) Alive(index int) bool {
	done := s.done[index]
	if done == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

func (s *NativeControlServer) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func (s *NativeControlServer) fail(err error) {
	slog.Debug(fmt.Sprintf("runtime control pipe stopped: %v", err))
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(s.ready) })
}

// Serves one instance until stop, recycling it after every client and storing the error that ends it
func (s *NativeControlServer) run(index int) {
	defer close(s.done[index])
	pipe, err := createNativePipeSeam(s.home, index == 0)
	if err != nil {
		s.fail(err)
		return
	}
	defer pipe.close()
	s.readyOnce.Do(func() { close(s.ready) })
	for !s.stopped() {
		connected, err := pipe.accept(500 * time.Millisecond)
		if err != nil {
			connected = true
		}
		if !connected {
			continue
		}
		serveErr := s.serveOne(pipe)
		if err := pipe.disconnect(); err != nil {
			s.fail(err)
			return
		}
		if serveErr != nil {
			s.fail(serveErr)
			return
		}
	}
}

// Reads one request, identifies the peer, answers and lets the client close, only an impersonation leak escapes
func (s *NativeControlServer) serveOne(pipe nativePipeInstance) error {
	raw, err := pipe.readLine(time.Now().Add(2*time.Second), 64*1024)
	if err != nil {
		return nil
	}
	subject, err := pipe.peerSubject()
	if err != nil {
		if errors.As(err, new(impersonationLeakError)) {
			return err
		}
		return nil
	}
	response := s.handler(raw, subject)
	deadline := time.Now().Add(2 * time.Second)
	if err := pipe.write(response, deadline); err != nil {
		return nil
	}
	_, _ = pipe.readLine(deadline, 64*1024)
	return nil
}
