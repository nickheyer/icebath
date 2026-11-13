package pty

import (
	"sync"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/connections"
)

// Terminal name every shell is started under
const terminalName = "xterm-256color"

// Renderer window a terminal was started from
type Sender interface {
	// Window id the routes and session ownership are keyed by
	ID() string
	// Delivers an event to this window while it is alive
	Send(event string, payload any)
	// Runs f once when the window is destroyed
	OnDestroyed(f func())
}

// Host facts, routing and resolvers the terminal group reads
type Options struct {
	IsWindows      bool
	Platform       string
	HomeDir        func() string
	UserData       string
	IcebathHome    string
	AppVersion     func() string
	Log            func(string)
	Routes         Routes
	WindowRoutes   *backend.WindowConnectionRoutes
	PrimaryProfile func() string
	Resolve        func(args []string) (backend.ResolvedBackend, error)
	SanitizeCwd    func(cwd any) string
}

type session struct {
	gate     *OutputGate
	pty      ptyProcess
	windowID string
	remote   bool
	sshScope string
}

// Embedded terminal PTY host behind the terminal preload functions
type Service struct {
	opts  Options
	isWsl bool

	mu       sync.Mutex
	sessions map[string]*session
}

// Terminal host with no sessions
func NewService(opts Options) *Service {
	return &Service{opts: opts, isWsl: isWslEnvironment(opts.Platform), sessions: map[string]*session{}}
}

var _ connections.TerminalSessions = (*Service)(nil)

// Started session, or the unavailable gateway a tab recorded
type StartResult struct {
	ConnectionID *string `json:"connectionId"`
	Cwd          *string `json:"cwd"`
	ID           string  `json:"id"`
	Shell        string  `json:"shell"`
	Unavailable  string  `json:"unavailable,omitempty"`
}

func terminalChannel(id, suffix string) string { return "terminal:" + id + ":" + suffix }

func (s *Service) lookup(id any) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[jsOr(id, "")]
}

// Spawns a shell on the host the tab gateway or the window route names, buffering output until attach
func (s *Service) TerminalStart(sender Sender, payload any) (StartResult, error) {
	id := uuid.NewString()
	spec := s.terminalShellCommand()
	cwd := s.safeTerminalCwd(get(payload, "cwd"))
	cols := terminalDimension(get(payload, "cols"), 80)
	rows := terminalDimension(get(payload, "rows"), 24)
	owner := RequestedTerminalGateway(payload)
	sshTarget, unavailable, err := s.resolveStartTarget(owner, sender.ID())
	if err != nil {
		return StartResult{}, err
	}
	if unavailable != "" {
		var connectionID *string
		if owner != nil {
			cid := owner.ConnectionID
			connectionID = &cid
		}
		return StartResult{ConnectionID: connectionID, Unavailable: unavailable}, nil
	}
	remote := sshTarget != nil
	requestedCwd := jsTrim(jsOr(get(payload, "cwd"), ""))
	gate := NewOutputGate(func() { s.disposeTerminalSession(id) },
		func(data string) { sender.Send(terminalChannel(id, "data"), data) },
		func(exit ExitPayload) { sender.Send(terminalChannel(id, "exit"), exit) })
	var proc ptyProcess
	if remote {
		remoteCommand := ""
		if state, ok := s.opts.Routes.SSHConnectionStateOf(sshTarget.Scope); ok && state.RemotePlatform == "Windows" {
			remoteCommand = connections.BuildWindowsInteractiveCommand(requestedCwd)
		}
		args, err := connections.BuildInteractiveSSHArgs(sshTarget.SSH.SSHEndpoint, requestedCwd, 0, remoteCommand)
		if err != nil {
			return StartResult{}, err
		}
		proc, err = spawnPty(s.opts.Routes.DesktopSSHBinary(), args, spawnOptions{Cols: cols, Rows: rows, Cwd: s.opts.HomeDir(), Env: s.terminalShellEnv(), Name: terminalName}, gate.Data, gate.Exit)
		if err != nil {
			return StartResult{}, err
		}
	} else {
		proc, err = spawnPty(spec.Command, spec.Args, spawnOptions{Cols: cols, Rows: rows, Cwd: cwd, Env: s.terminalShellEnv(), Name: terminalName}, gate.Data, gate.Exit)
		if err != nil {
			return StartResult{}, err
		}
	}
	entry := &session{gate: gate, pty: proc, windowID: sender.ID(), remote: remote}
	if remote {
		entry.sshScope = sshTarget.Scope
	}
	s.mu.Lock()
	s.sessions[id] = entry
	s.mu.Unlock()
	sender.OnDestroyed(func() { s.disposeTerminalSession(id) })
	result := StartResult{ConnectionID: s.spawnedOn(owner, sshTarget), ID: id, Shell: spec.Name}
	if remote {
		result.Shell = "ssh"
	} else {
		result.Cwd = &cwd
	}
	return result, nil
}

// Attaches the calling window to its own session, flushing buffered output and exit, false for another window
func (s *Service) TerminalAttach(windowID string, id any) bool {
	entry := s.lookup(id)
	if entry == nil || entry.windowID != windowID {
		return false
	}
	entry.gate.Attach()
	return true
}

// Writes input to a session shell, false when the session is gone
func (s *Service) TerminalWrite(id any, data any) bool {
	entry := s.lookup(id)
	if entry == nil {
		return false
	}
	entry.pty.Write(jsOr(data, ""))
	return true
}

// Resizes a session terminal to the clamped columns and rows, false when the session is gone
func (s *Service) TerminalResize(id any, size any) (bool, error) {
	entry := s.lookup(id)
	if entry == nil {
		return false, nil
	}
	if err := entry.pty.Resize(terminalDimension(get(size, "cols"), 80), terminalDimension(get(size, "rows"), 24)); err != nil {
		return false, err
	}
	return true, nil
}

// Live working directory of a local session shell, nil for a remote or missing session
func (s *Service) TerminalCwd(id any) *string {
	entry := s.lookup(id)
	if entry == nil || entry.remote {
		return nil
	}
	return readProcessCwd(s.opts.Platform, entry.pty.Pid())
}

// Kills a session shell and forgets the session, false when it was already gone
func (s *Service) TerminalDispose(id any) bool {
	return s.disposeTerminalSession(jsOr(id, ""))
}

func (s *Service) disposeTerminalSession(id string) bool {
	s.mu.Lock()
	entry := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if entry == nil {
		return false
	}
	entry.pty.Kill()
	return true
}

func (s *Service) snapshot() map[string]*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*session, len(s.sessions))
	for id, entry := range s.sessions {
		out[id] = entry
	}
	return out
}

// Marks every session riding an SSH scope disconnected and stops its shell, an unattached tab keeping the exit for attach
func (s *Service) DisposeTerminalSessionsForSSHScope(scope string) {
	disconnected := "disconnected"
	for id, entry := range s.snapshot() {
		if !entry.remote || entry.sshScope != scope {
			continue
		}
		entry.gate.Exit(ExitPayload{Signal: &disconnected})
		s.mu.Lock()
		_, live := s.sessions[id]
		s.mu.Unlock()
		if live {
			entry.pty.Kill()
		}
	}
}

// Kills every shell on app quit without telling the renderer the shells exited
func (s *Service) DisposeAllTerminalSessions() {
	for id, entry := range s.snapshot() {
		entry.gate.Silence()
		s.disposeTerminalSession(id)
	}
}
