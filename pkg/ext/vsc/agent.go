package vsc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

// How often the agent checks the mode, the titles and the selected session for changes no message carries
const modeTick = 500 * time.Millisecond

// Settings a caller passes when building the agent
type Options struct {
	Version string
	// Receives every line read or written so a transport can log it, nil for none
	Trace func(dir string, line []byte)
}

// Notification waiting to be written to the connected client
type outItem struct {
	method string
	params any
}

// Mirrors the file changes, run state and mode of the live sessions onto one connected editor window
type Agent struct {
	app  *harness.App
	opts Options

	mu       sync.Mutex
	peer     *Peer
	lastMode policy.Mode
	lastCur  string
	titles   map[string]string
	cancels  map[string]bool
	// Tool calls whose proposed change the client shows, until the call ends
	proposed map[string]bool

	qmu    sync.Mutex
	queue  []outItem
	signal chan struct{}
}

func NewAgent(app *harness.App, opts Options) *Agent {
	return &Agent{app: app, opts: opts, titles: map[string]string{}, cancels: map[string]bool{}, proposed: map[string]bool{}}
}

// Serves one client over conn until the connection or the context ends
func (a *Agent) Serve(ctx context.Context, conn io.ReadWriteCloser) error {
	peer := newPeer(conn)
	peer.SetTrace(a.opts.Trace)
	signal := make(chan struct{}, 1)
	a.qmu.Lock()
	a.queue = nil
	a.qmu.Unlock()
	a.mu.Lock()
	a.peer = peer
	a.signal = signal
	a.cancels = map[string]bool{}
	a.proposed = map[string]bool{}
	a.lastMode = a.app.Mode.Mode()
	a.lastCur = a.currentID()
	a.titles = map[string]string{}
	a.mu.Unlock()
	a.register(peer)
	peer.Start()
	go a.writer(peer, signal)
	go a.ticker(peer)
	a.announce()
	select {
	case <-peer.Done():
	case <-ctx.Done():
		peer.Close()
	}
	a.mu.Lock()
	a.peer = nil
	a.signal = nil
	a.mu.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	return peer.Err()
}

// Sends the state of every open session and the change of every approval the TUI is asking about
func (a *Agent) announce() {
	for _, l := range a.app.Lives() {
		a.sendState(l)
	}
	for _, p := range a.app.Approver.Pending() {
		if l := a.app.LiveOfRun(p.Run); l != nil {
			a.propose(l.Session.ID(), p.Request.Call)
		}
	}
}

func (a *Agent) writer(peer *Peer, signal chan struct{}) {
	for {
		select {
		case <-signal:
		case <-peer.Done():
			return
		}
		for {
			a.qmu.Lock()
			if len(a.queue) == 0 {
				a.qmu.Unlock()
				break
			}
			it := a.queue[0]
			a.queue = a.queue[1:]
			a.qmu.Unlock()
			peer.Notify(it.method, it.params)
		}
	}
}

func (a *Agent) ticker(peer *Peer) {
	t := time.NewTicker(modeTick)
	defer t.Stop()
	for {
		select {
		case <-peer.Done():
			return
		case <-t.C:
			a.syncMode()
			a.syncCurrent()
		}
	}
}

// Queues a notification for the writer while a client is connected
func (a *Agent) enqueue(method string, params any) {
	a.mu.Lock()
	signal := a.signal
	connected := a.peer != nil
	a.mu.Unlock()
	if !connected {
		return
	}
	a.qmu.Lock()
	a.queue = append(a.queue, outItem{method: method, params: params})
	a.qmu.Unlock()
	select {
	case signal <- struct{}{}:
	default:
	}
}

func (a *Agent) register(peer *Peer) {
	peer.Handle(MethodInitialize, a.initialize)
	peer.Handle(MethodLoadSession, a.loadSession)
	peer.Handle(MethodPrompt, a.prompt)
	peer.Handle(MethodSetMode, a.setMode)
	peer.Handle(MethodSessionsList, a.sessionsList)
	peer.HandleNotify(MethodCancel, a.cancel)
}

func (a *Agent) initialize(ctx context.Context, params json.RawMessage) (any, error) {
	return InitializeResponse{ProtocolVersion: ProtocolVersion, SessionID: a.currentID(), Version: a.opts.Version, Pid: os.Getpid(), Modes: a.modeState()}, nil
}

// Opens the session in the TUI, focusing it when it is open already, and answers with the modes
func (a *Agent) loadSession(ctx context.Context, params json.RawMessage) (any, error) {
	var req LoadSessionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &RPCError{Code: codeBadParams, Message: err.Error()}
	}
	if req.SessionID == "" {
		return nil, &RPCError{Code: codeBadParams, Message: "sessionId is required"}
	}
	l, err := a.app.Open(ctx, req.SessionID)
	if err != nil {
		return nil, &RPCError{Code: codeInternal, Message: err.Error()}
	}
	a.app.Announce(harness.OpenedMsg{Live: l})
	return LoadSessionResponse{Modes: a.modeState()}, nil
}

func (a *Agent) prompt(ctx context.Context, params json.RawMessage) (any, error) {
	var req PromptRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &RPCError{Code: codeBadParams, Message: err.Error()}
	}
	live := a.app.LiveByID(req.SessionID)
	if live == nil {
		return nil, &RPCError{Code: codeBadParams, Message: "no session " + req.SessionID}
	}
	parts := textParts(req.Prompt)
	if len(parts) == 0 {
		return nil, &RPCError{Code: codeBadParams, Message: "prompt is empty"}
	}
	if live.Session.Title() == "" {
		live.Session.SetTitle(derivedTitle(protocol.Response{Content: parts}.Text()))
	}
	run, err := a.startPrompt(ctx, live, parts)
	if err != nil {
		return nil, &RPCError{Code: codeInternal, Message: err.Error()}
	}
	done, ok := a.app.Control.Done(run.ID)
	if !ok {
		return PromptResponse{StopReason: StopEndTurn}, nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return PromptResponse{StopReason: StopCancelled}, nil
	}
	return PromptResponse{StopReason: a.stopReason(run)}, nil
}

// Steers the live root run with the parts or starts a fresh run on the session
func (a *Agent) startPrompt(ctx context.Context, live *harness.Live, parts []protocol.Part) (*harness.Run, error) {
	if run, ok := a.rootRun(live); ok {
		if err := a.app.Control.Steer(run.ID, parts...); err != nil {
			return nil, err
		}
		return run, nil
	}
	return a.app.RunOn(ctx, live, "", parts)
}

func (a *Agent) stopReason(run *harness.Run) string {
	a.mu.Lock()
	cancelled := a.cancels[run.ID]
	a.mu.Unlock()
	err := run.Err()
	switch {
	case cancelled || errors.Is(err, context.Canceled):
		return StopCancelled
	case isDenied(err):
		return StopRefusal
	case run.Response().StopReason == protocol.StopLength:
		return StopMaxTokens
	}
	return StopEndTurn
}

func (a *Agent) cancel(params json.RawMessage) {
	var req CancelNotification
	if err := json.Unmarshal(params, &req); err != nil {
		return
	}
	live := a.app.LiveByID(req.SessionID)
	if live == nil {
		return
	}
	run, ok := a.rootRun(live)
	if !ok {
		return
	}
	a.mu.Lock()
	a.cancels[run.ID] = true
	a.mu.Unlock()
	a.app.Control.Cancel(run.ID)
}

func (a *Agent) setMode(ctx context.Context, params json.RawMessage) (any, error) {
	var req SetModeRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &RPCError{Code: codeBadParams, Message: err.Error()}
	}
	mode, err := policy.ParseMode(req.ModeID)
	if err != nil {
		return nil, &RPCError{Code: codeBadParams, Message: err.Error()}
	}
	a.app.Mode.Set(mode)
	a.mu.Lock()
	a.lastMode = mode
	a.mu.Unlock()
	a.enqueue(MethodUpdate, SessionNotification{SessionID: req.SessionID, Update: SessionUpdate{SessionUpdate: UpdateModeChanged, CurrentModeID: string(mode)}})
	return struct{}{}, nil
}

func (a *Agent) sessionsList(ctx context.Context, params json.RawMessage) (any, error) {
	infos, err := a.app.Store.ListProject(ctx, a.app.Root)
	if err != nil {
		return nil, &RPCError{Code: codeInternal, Message: err.Error()}
	}
	session.SortInfos(infos)
	out := SessionsListResponse{Sessions: make([]SessionEntry, 0, len(infos))}
	for _, in := range infos {
		out.Sessions = append(out.Sessions, SessionEntry{SessionID: in.ID, Title: in.Title, Updated: in.Updated.Format(time.RFC3339), Busy: in.Busy})
	}
	return out, nil
}

// Modes the agent offers with the one in force marked current
func (a *Agent) modeState() ModeState {
	modes := []Mode{
		{ID: string(policy.ModeDefault), Name: "Default", Description: "Rules decide, anything else asks"},
		{ID: string(policy.ModeAcceptEdits), Name: "Accept edits", Description: "File edits run without asking"},
		{ID: string(policy.ModePlan), Name: "Plan", Description: "Read only tools, nothing that changes anything"},
		{ID: string(policy.ModeBypass), Name: "Bypass permissions", Description: "Every tool runs with nothing consulted"},
	}
	return ModeState{CurrentModeID: string(a.app.Mode.Mode()), AvailableModes: modes}
}

func (a *Agent) rootRun(live *harness.Live) (*harness.Run, bool) {
	for _, r := range a.app.Control.Runs() {
		if r.Running() && r.Parent == "" && r.Session == live.Session {
			return r, true
		}
	}
	return nil, false
}

func (a *Agent) currentID() string {
	if cur := a.app.Current(); cur != nil {
		return cur.Session.ID()
	}
	return ""
}

// Sends the mode when it differs from the last one sent
func (a *Agent) syncMode() {
	mode := a.app.Mode.Mode()
	a.mu.Lock()
	changed := mode != a.lastMode
	a.lastMode = mode
	a.mu.Unlock()
	if !changed {
		return
	}
	a.enqueue(MethodUpdate, SessionNotification{SessionID: a.currentID(), Update: SessionUpdate{SessionUpdate: UpdateModeChanged, CurrentModeID: string(mode)}})
}

// Sends the state of sessions whose selection or title changed since the last check
func (a *Agent) syncCurrent() {
	cur := a.currentID()
	a.mu.Lock()
	prev := a.lastCur
	a.lastCur = cur
	a.mu.Unlock()
	if prev != cur {
		if l := a.app.LiveByID(prev); l != nil {
			a.sendState(l)
		}
		if l := a.app.LiveByID(cur); l != nil {
			a.sendState(l)
		}
	}
	for _, l := range a.app.Lives() {
		id := l.Session.ID()
		title := l.Session.Title()
		a.mu.Lock()
		changed := a.titles[id] != title
		a.titles[id] = title
		a.mu.Unlock()
		if changed && prev == cur {
			a.sendState(l)
		}
	}
}

func (a *Agent) sendState(live *harness.Live) {
	a.enqueue(MethodState, a.stateOf(live))
}

func (a *Agent) stateOf(live *harness.Live) StateNotification {
	waiting := false
	for _, p := range a.app.Approver.Pending() {
		if l := a.app.LiveOfRun(p.Run); l == live {
			waiting = true
			break
		}
	}
	return StateNotification{
		SessionID: live.Session.ID(),
		Title:     live.Session.Title(),
		Running:   a.app.Busy(live),
		Waiting:   waiting,
		Current:   live == a.app.Current(),
	}
}

// Text blocks of a prompt as protocol parts, blanks left out
func textParts(blocks []ContentBlock) []protocol.Part {
	var out []protocol.Part
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			out = append(out, protocol.Text{Text: b.Text})
		}
	}
	return out
}

// Title from the first line of a prompt cut to sixty runes
func derivedTitle(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	runes := []rune(strings.TrimSpace(line))
	if len(runes) > 60 {
		return string(runes[:59]) + "~"
	}
	return string(runes)
}

func isDenied(err error) bool {
	var d *policy.Denied
	return errors.As(err, &d)
}
