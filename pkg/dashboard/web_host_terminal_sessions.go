package dashboard

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	hostTerminalRetention  = 15 * time.Minute
	hostTerminalMaxShells  = 16
	hostInputQueueFrames   = 64
	hostInputQueueBytes    = 1024 * 1024
	hostFrameOverheadBytes = 256
	hostTerminalExitsKept  = 4 * hostTerminalMaxShells
)

var (
	hostLivenessProbe   = time.Second
	hostResizeAnswer    = time.Second
	hostTerminalTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

// Refusal of a terminal that belongs to another identity or profile
type terminalDeniedError struct{}

func (*terminalDeniedError) Error() string { return "TerminalDenied" }

// Refusal of a terminal that is gone, exited when the shell ended by itself
type terminalExpiredError struct{ exited bool }

func (e *terminalExpiredError) Error() string {
	if e.exited {
		return "TerminalExited"
	}
	return "TerminalExpired"
}

func isTerminalDenied(err error) bool {
	var denied *terminalDeniedError
	return errors.As(err, &denied)
}

// Expired refusal of err, the exited one included
func asTerminalExpired(err error) (*terminalExpiredError, bool) {
	var expired *terminalExpiredError
	return expired, errors.As(err, &expired)
}

// Whether a named home exists without a deletion tombstone and any other home exists as a directory
func hostHomeLive(h string) bool {
	if named := home.NamedProfileHome(h); named != "" {
		if _, err := os.Stat(named); home.ProfileDeleted(named) || err != nil {
			return false
		}
	}
	st, err := os.Stat(h)
	return err == nil && st.IsDir()
}

// Identity, profile home, shell and cwd a host shell belongs to
type hostOwner struct {
	identity [2]string
	home     string
	shell    string
	cwd      string
}

// Whether the owner's profile home is still live, a blocking filesystem read
func (o *hostOwner) current() bool { return hostHomeLive(o.home) }

// Runs write while the owner's profile home is live, the expired refusal once it is gone
func (o *hostOwner) admit(write tools.PtyWriteAttempt) (int, bool, error) {
	if !o.current() {
		return 0, false, &terminalExpiredError{}
	}
	return write()
}

// Provider and user of the terminal ticket that admitted the socket, denied when either is missing
func hostRequestIdentity(ws *WebSocket) ([2]string, error) {
	identity := wsAuthIdentity(ws)
	user, _ := identity["user_id"].(string)
	provider, _ := identity["provider"].(string)
	if user == "" || provider == "" {
		return [2]string{}, &terminalDeniedError{}
	}
	return [2]string{provider, user}, nil
}

// Resolved home of the requested profile, the active home for none or current
func hostRequestHome(ctx context.Context, profile string) (string, error) {
	requested := pystr.Strip(profile)
	h := home.Dir(ctx)
	if requested != "" && strings.ToLower(requested) != "current" {
		dir, err := resolveProfileDir(requested)
		if err != nil {
			return "", err
		}
		h = dir
	}
	return pypath.Resolve(h)
}

// Home resolution the redial validation runs, replaced by cases that hold a validation open
var hostTerminalRequestHome = hostRequestHome

// Token, identity and reap time of a shell that exited by itself
type hostExit struct {
	token    string
	identity [2]string
	at       time.Time
}

// Process lifetime host shells keyed by token with their owners and the shells that exited by themselves
type hostTerminalRegistry struct {
	*tools.PtySessionRegistry
	mu         sync.Mutex
	owners     map[string]*hostOwner
	ownerOrder []string
	exits      []hostExit
	drainSlots chan struct{}
}

// Registry that never evicts a retained shell and gives each live shell its own reader slot
func newHostTerminalRegistry() *hostTerminalRegistry {
	r := &hostTerminalRegistry{owners: map[string]*hostOwner{}, drainSlots: make(chan struct{}, hostTerminalMaxShells)}
	r.PtySessionRegistry = tools.NewPtySessionRegistry(tools.PtySessionRegistryOptions{
		TTL: hostTerminalRetention, MaxSessions: hostTerminalMaxShells, BufferCap: 1024 * 1024,
		ReadTimeout: 200 * time.Millisecond, DrainExecutor: r.drain, RetainIdle: true, ReapIdle: r.reapIdle,
	})
	return r
}

// Runs one PTY read once one of the registry's reader slots is free
func (r *hostTerminalRegistry) drain(task func()) {
	go func() {
		r.drainSlots <- struct{}{}
		defer func() { <-r.drainSlots }()
		task()
	}()
}

func (r *hostTerminalRegistry) owner(token string) *hostOwner {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owners[token]
}

func (r *hostTerminalRegistry) setOwner(token string, owner *hostOwner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.owners[token]; !ok {
		r.ownerOrder = append(r.ownerOrder, token)
	}
	r.owners[token] = owner
}

func (r *hostTerminalRegistry) dropOwner(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.owners[token]; ok {
		delete(r.owners, token)
		r.ownerOrder = slices.DeleteFunc(r.ownerOrder, func(t string) bool { return t == token })
	}
}

// One registered owner with its token
type hostOwnerItem struct {
	token string
	owner *hostOwner
}

// Owners in registration order
func (r *hostTerminalRegistry) ownerItems() []hostOwnerItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]hostOwnerItem, 0, len(r.ownerOrder))
	for _, token := range r.ownerOrder {
		out = append(out, hostOwnerItem{token, r.owners[token]})
	}
	return out
}

// Spawns a shell for the socket's requested profile under a fresh token, expired when the home is gone
func (r *hostTerminalRegistry) create(ctx context.Context, ws *WebSocket, identity [2]string) (*tools.PtySession, *hostOwner, error) {
	token := tokenURLSafe(32)
	var owner *hostOwner
	query := ws.QueryParams()
	spawn := func() (tools.TerminalBridge, error) {
		h, err := hostRequestHome(ctx, query.Get("profile"))
		if err != nil {
			return nil, err
		}
		if !hostHomeLive(h) {
			return nil, &terminalExpiredError{}
		}
		argv, cwd, env, shell, err := resolveHostTerminalArgv(ctx, h, query.Get("cwd"))
		if err != nil {
			return nil, err
		}
		bridge, err := tools.SpawnTerminalBridge(ctx, nil, argv, tools.PtySpawnOptions{
			Cwd: cwd, Env: env, Cols: queryDimension(query.Get("cols"), 80, 2000), Rows: queryDimension(query.Get("rows"), 24, 1000),
		})
		if err != nil {
			return nil, err
		}
		owner = &hostOwner{identity: identity, home: h, shell: shell, cwd: cwd}
		return bridge, nil
	}
	session, _, err := r.AttachOrSpawn(ctx, token, spawn, "")
	if err != nil {
		return nil, nil, err
	}
	r.setOwner(token, owner)
	if !owner.current() {
		r.remove(ctx, token)
		return nil, nil, &terminalExpiredError{}
	}
	return session, owner, nil
}

// Session and owner of token for the same identity and profile, refusing a gone, exited or expired shell
func (r *hostTerminalRegistry) resolve(ctx context.Context, token string, ws *WebSocket, identity [2]string, closing bool) (*tools.PtySession, *hostOwner, error) {
	session := r.Get(token)
	owner := r.owner(token)
	if session == nil || owner == nil {
		return nil, nil, r.gone(token, identity)
	}
	if owner.identity != identity {
		return nil, nil, &terminalDeniedError{}
	}
	h, err := hostTerminalRequestHome(ctx, ws.QueryParams().Get("profile"))
	if err != nil {
		return nil, nil, err
	}
	if h != owner.home {
		return nil, nil, &terminalDeniedError{}
	}
	if _, _, err := owner.admit(func() (int, bool, error) { return 0, true, nil }); err != nil {
		return nil, nil, err
	}
	if r.Get(token) != session {
		return nil, nil, r.gone(token, identity)
	}
	if !closing && hostShellExited(session) {
		return nil, nil, &terminalExpiredError{exited: true}
	}
	if !closing && r.Expired(session, time.Time{}) {
		return nil, nil, &terminalExpiredError{}
	}
	return session, owner, nil
}

// Refusal of a token with no registered shell, exited when the identity's shell ended by itself
func (r *hostTerminalRegistry) gone(token string, identity [2]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.exits {
		if e.token == token {
			return &terminalExpiredError{exited: e.identity == identity}
		}
	}
	return &terminalExpiredError{}
}

// Drops the shell and owner of token and waits for the shell to close
func (r *hostTerminalRegistry) remove(ctx context.Context, token string) {
	session := r.Pop(token)
	r.dropOwner(token)
	if session != nil {
		done := r.CloseInBackground(session)
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
}

// Closes shells whose profile is gone, dead or expired, remembering the ones that exited by themselves
func (r *hostTerminalRegistry) reapIdle(ctx context.Context, now time.Time) {
	owners := r.ownerItems()
	retired := map[string]bool{}
	for _, item := range owners {
		if !item.owner.current() {
			retired[item.token] = true
		}
	}
	if now.IsZero() {
		now = time.Now()
	}
	for _, item := range owners {
		session := r.Get(item.token)
		if session != nil && (retired[item.token] || r.Reapable(session, now)) {
			if !retired[item.token] && hostShellExited(session) {
				r.rememberExit(item.token, item.owner, now)
			}
			r.remove(ctx, item.token)
		}
	}
	for _, item := range r.ownerItems() {
		if r.Get(item.token) == nil {
			r.dropOwner(item.token)
		}
	}
	r.mu.Lock()
	r.exits = slices.DeleteFunc(r.exits, func(e hostExit) bool { return now.Sub(e.at) > r.TTL() })
	r.mu.Unlock()
}

// Records a shell that exited by itself, keeping only the newest exits
func (r *hostTerminalRegistry) rememberExit(token string, owner *hostOwner, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := slices.IndexFunc(r.exits, func(e hostExit) bool { return e.token == token }); i >= 0 {
		r.exits[i].identity, r.exits[i].at = owner.identity, now
	} else {
		r.exits = append(r.exits, hostExit{token: token, identity: owner.identity, at: now})
	}
	for len(r.exits) > hostTerminalExitsKept {
		r.exits = r.exits[1:]
	}
}

// Closes every shell and forgets every owner and exit
func (r *hostTerminalRegistry) closeAll(ctx context.Context) {
	r.CloseAll(ctx)
	r.mu.Lock()
	r.owners = map[string]*hostOwner{}
	r.ownerOrder = nil
	r.exits = nil
	r.mu.Unlock()
}

// Whether a registered session lost its shell, which only the shell's own exit leaves behind
func hostShellExited(session *tools.PtySession) bool {
	return !session.Alive() || !session.Bridge.IsAlive()
}

// Serializes the overlapping lifespans' registry lists in app state
var hostTerminalStateMu sync.Mutex

// Running host terminal registry of the app, a RuntimeError before the lifespan started
func getHostTerminals(app *FastAPI) (*hostTerminalRegistry, error) {
	registry, _ := app.State.Get("host_terminals").(*hostTerminalRegistry)
	if registry == nil || registry.Closed() {
		return nil, &RuntimeError{Msg: "Host terminal service is not running; start the application lifespan first."}
	}
	return registry, nil
}

// Starts a registry and its one second reaper for one lifespan, the exit withdrawing and closing it
func hostTerminalLifespan(ctx context.Context, app *FastAPI) (func(ctx context.Context) error, error) {
	registry := newHostTerminalRegistry()
	hostTerminalStateMu.Lock()
	registries, _ := app.State.Get("_host_terminal_registries").([]*hostTerminalRegistry)
	app.State.Set("_host_terminal_registries", append(registries, registry))
	app.State.Set("host_terminals", registry)
	hostTerminalStateMu.Unlock()
	reaperCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		tools.RunPtyReaper(reaperCtx, registry.PtySessionRegistry, time.Second)
	}()
	exit := func(ctx context.Context) error {
		hostTerminalStateMu.Lock()
		registries, _ := app.State.Get("_host_terminal_registries").([]*hostTerminalRegistry)
		registries = slices.DeleteFunc(slices.Clone(registries), func(r *hostTerminalRegistry) bool { return r == registry })
		if current, _ := app.State.Get("host_terminals").(*hostTerminalRegistry); current == registry {
			if len(registries) > 0 {
				app.State.Set("host_terminals", registries[len(registries)-1])
			} else {
				app.State.Del("host_terminals")
			}
		}
		if len(registries) == 0 {
			app.State.Del("_host_terminal_registries")
		} else {
			app.State.Set("_host_terminal_registries", registries)
		}
		hostTerminalStateMu.Unlock()
		cancel()
		<-reaperDone
		registry.closeAll(ctx)
		return nil
	}
	return exit, nil
}

// Metadata frame naming the shell, token, cwd, retention and whether the replay was trimmed
func hostTerminalMetadata(token string, owner *hostOwner, registry *hostTerminalRegistry, reconnected, truncated bool) string {
	text, err := pyjson.Dumps(pyjson.ObjectOf("shell", owner.shell, "terminalId", token, "cwd", owner.cwd,
		"reconnected", reconnected, "retentionSeconds", int(registry.TTL()/time.Second), "truncated", truncated),
		pyjson.Separators(",", ":"))
	if err != nil {
		panic(err)
	}
	return hostTerminalMetaPrefix + text
}

// Tells the tab its terminal is closed, then closes the socket normally
func hostTerminalSendClosed(ctx context.Context, ws *WebSocket, token string) error {
	text, err := pyjson.Dumps(pyjson.ObjectOf("terminalId", token, "closed", true))
	if err != nil {
		return err
	}
	if err := ws.SendText(ctx, hostTerminalMetaPrefix+text); err != nil {
		return err
	}
	return ws.Close(ctx, 1000, "")
}

// Session and owner to attach for the request, nil once a close request has been served
func hostTerminalAdmit(ctx context.Context, ws *WebSocket, registry *hostTerminalRegistry) (*tools.PtySession, *hostOwner, error) {
	query := ws.QueryParams()
	token, hasToken := query.Lookup("attach")
	action, hasAction := query.Lookup("action")
	identity, err := hostRequestIdentity(ws)
	if err != nil {
		return nil, nil, err
	}
	closing := hasAction && action == "close"
	if (hasAction && action != "close") || (closing && !hasToken) {
		return nil, nil, &terminalDeniedError{}
	}
	if hasToken && !hostTerminalTokenRE.MatchString(token) {
		return nil, nil, &terminalExpiredError{}
	}
	if closing && registry.Get(token) == nil {
		return nil, nil, hostTerminalSendClosed(ctx, ws, token)
	}
	if !hasToken {
		return registry.create(ctx, ws, identity)
	}
	session, owner, err := registry.resolve(ctx, token, ws, identity, closing)
	if err != nil {
		return nil, nil, err
	}
	if closing {
		registry.remove(ctx, token)
		return nil, nil, hostTerminalSendClosed(ctx, ws, token)
	}
	return session, owner, nil
}

// Bytes a frame writes to the program, a binary frame's bytes or a text frame's UTF-8
func hostFramePayload(msg *Message) []byte {
	if msg.Bytes != nil {
		return msg.Bytes
	}
	if msg.Text == nil {
		return []byte{}
	}
	return []byte(*msg.Text)
}

// One inbound frame, or the reader's last item carrying the error that ended reading
type hostInputItem struct {
	msg *Message
	end error
}

// Bytes a queued item charges, its payload and the per frame overhead
func hostFrameCost(item hostInputItem) int {
	if item.msg == nil {
		return hostFrameOverheadBytes
	}
	return len(hostFramePayload(item.msg)) + hostFrameOverheadBytes
}

// Frames a tab sent while a batch is written, full once it holds both the frame and byte bounds
type hostInputQueue struct {
	mu      sync.Mutex
	items   []hostInputItem
	queued  int
	changed chan struct{}
}

func newHostInputQueue() *hostInputQueue { return &hostInputQueue{changed: make(chan struct{})} }

func (q *hostInputQueue) fullLocked() bool {
	return len(q.items) >= hostInputQueueFrames && q.queued >= hostInputQueueBytes
}

func (q *hostInputQueue) notifyLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// Queues item once the queue is not full, errTimeout after timeout when it is positive
func (q *hostInputQueue) put(ctx context.Context, item hostInputItem, timeout time.Duration) error {
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	for {
		q.mu.Lock()
		if !q.fullLocked() {
			q.items = append(q.items, item)
			q.queued += hostFrameCost(item)
			q.notifyLocked()
			q.mu.Unlock()
			return nil
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-changed:
		case <-expired:
			return context.DeadlineExceeded
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Oldest item, waiting while the queue is empty
func (q *hostInputQueue) get(ctx context.Context) (hostInputItem, error) {
	for {
		if item, ok := q.getNowait(); ok {
			return item, nil
		}
		q.mu.Lock()
		changed := q.changed
		empty := len(q.items) == 0
		q.mu.Unlock()
		if !empty {
			continue
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return hostInputItem{}, ctx.Err()
		}
	}
}

// Oldest item without waiting, false when the queue is empty
func (q *hostInputQueue) getNowait() (hostInputItem, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return hostInputItem{}, false
	}
	item := q.items[0]
	q.items = q.items[1:]
	q.queued -= hostFrameCost(item)
	q.notifyLocked()
	return item, true
}

// Once set result of reading, the error that ended it or nil
type hostReadEnded struct {
	once   sync.Once
	done   chan struct{}
	result error
}

func newHostReadEnded() *hostReadEnded { return &hostReadEnded{done: make(chan struct{})} }

func (e *hostReadEnded) set(result error) {
	e.once.Do(func() {
		e.result = result
		close(e.done)
	})
}

func (e *hostReadEnded) isDone() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// Queues a frame, probing the tab with an empty binary frame every second while the inbox stays full
func hostQueueFrame(ctx context.Context, ws *WebSocket, inbox *hostInputQueue, ended *hostReadEnded, item hostInputItem) error {
	for !ended.isDone() {
		err := inbox.put(ctx, item, hostLivenessProbe)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if err := tools.SendToPtyViewer(ctx, ws, []byte{}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			ended.set(nil)
		}
	}
	return inbox.put(ctx, item, 0)
}

// Queues inbound frames until the tab leaves, the last item carrying the error that ended reading
func hostReadFrames(ctx context.Context, ws *WebSocket, inbox *hostInputQueue, ended *hostReadEnded) {
	var end error
	for {
		msg, err := ws.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !wsReceiveRuntimeError(err) {
				end = err
			}
			break
		}
		if msg.Type == "websocket.disconnect" {
			break
		}
		if err := hostQueueFrame(ctx, ws, inbox, ended, hostInputItem{msg: &msg}); err != nil {
			return
		}
	}
	ended.set(end)
	_ = inbox.put(ctx, hostInputItem{end: end}, 0)
}

// Outcome of one session write
type hostWriteResult struct {
	delivered bool
	err       error
}

// Writes data with no deadline while the socket is open, nil once the socket ended and the write had to wait
func hostWriteWhileOpen(ctx context.Context, session *tools.PtySession, ws *WebSocket, data []byte, fence tools.PtyWriteFence, ended *hostReadEnded) (*bool, error) {
	var stalled pythread.Event
	watched := func(write tools.PtyWriteAttempt) (int, bool, error) {
		n, open, err := fence(write)
		if err != nil {
			return n, open, err
		}
		if open && n == 0 {
			stalled.Set()
		} else {
			stalled.Clear()
		}
		return n, open, err
	}
	writeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan hostWriteResult, 1)
	go func() {
		delivered, err := session.Write(writeCtx, ws, data, tools.PtyNoTimeout, watched)
		done <- hostWriteResult{delivered, err}
	}()
	var result hostWriteResult
	finished := false
	select {
	case result = <-done:
		finished = true
	case <-ended.done:
		select {
		case result = <-done:
			finished = true
		case <-stalled.Waiter():
		case <-ctx.Done():
		}
	case <-ctx.Done():
	}
	if !finished {
		cancel()
		result = <-done
		if errors.Is(result.err, context.Canceled) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, nil
		}
	}
	if result.err != nil {
		return nil, result.err
	}
	return &result.delivered, nil
}

// One run of joined keystrokes or one resize to cols and rows
type hostInputRun struct {
	data   []byte
	resize bool
	size   [2]int
}

// Frames in order with consecutive keystrokes joined into one run and resizes kept apart
func hostInputRuns(frames []hostInputItem) []hostInputRun {
	var out []hostInputRun
	var run []byte
	for _, item := range frames {
		raw := hostFramePayload(item.msg)
		var match []int
		if len(raw) > 0 {
			if loc := ptyResizeRE.FindSubmatchIndex(raw); loc != nil && loc[1] == len(raw) {
				match = loc
			}
		}
		if match == nil {
			if len(raw) > 0 {
				run = append(run, raw...)
			}
			continue
		}
		if len(run) > 0 {
			out = append(out, hostInputRun{data: run})
			run = nil
		}
		out = append(out, hostInputRun{resize: true, size: [2]int{
			resizeDimension(raw[match[2]:match[3]]), resizeDimension(raw[match[4]:match[5]])}})
	}
	if len(run) > 0 {
		out = append(out, hostInputRun{data: run})
	}
	return out
}

// Size before a resize that changed it and the output count when it was applied
type hostResizeUndo struct {
	before  [2]int
	outputs int
}

// Session surface a resize reads and drives
type hostResizable interface {
	Size() (int, int, bool)
	Resize(ws tools.PtyViewer, cols, rows int)
	Outputs() int
	WaitForOutput(ctx context.Context, since int, timeout time.Duration) error
}

// Applies size, first waiting for the program to answer a previous resize this one undoes
func hostResize(ctx context.Context, session hostResizable, ws tools.PtyViewer, size [2]int, undo *hostResizeUndo) (*hostResizeUndo, error) {
	if undo != nil && undo.before == size {
		if err := session.WaitForOutput(ctx, undo.outputs, hostResizeAnswer); err != nil {
			return nil, err
		}
	}
	cols, rows, known := session.Size()
	session.Resize(ws, size[0], size[1])
	if known && [2]int{cols, rows} != size {
		return &hostResizeUndo{before: [2]int{cols, rows}, outputs: session.Outputs()}, nil
	}
	return nil, nil
}

// Forwards keystrokes and resizes while ws is the attached viewer, each keystroke run admitted by the owner
func hostPumpInput(ctx context.Context, ws *WebSocket, registry *hostTerminalRegistry, token string, session *tools.PtySession, owner *hostOwner) error {
	inbox := newHostInputQueue()
	ended := newHostReadEnded()
	readerCtx, cancelReader := context.WithCancel(ctx)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		hostReadFrames(readerCtx, ws, inbox, ended)
	}()
	defer func() {
		cancelReader()
		<-readerDone
	}()
	var undo *hostResizeUndo
	for session.Viewer() == tools.PtyViewer(ws) && session.Alive() {
		first, err := inbox.get(ctx)
		if err != nil {
			return err
		}
		batch := []hostInputItem{first}
		for {
			item, ok := inbox.getNowait()
			if !ok {
				break
			}
			batch = append(batch, item)
		}
		var frames []hostInputItem
		for _, item := range batch {
			if item.msg != nil {
				frames = append(frames, item)
			}
		}
	runs:
		for _, run := range hostInputRuns(frames) {
			if run.resize {
				if undo, err = hostResize(ctx, session, ws, run.size, undo); err != nil {
					return err
				}
				continue
			}
			delivered, err := hostWriteWhileOpen(ctx, session, ws, run.data, owner.admit, ended)
			if err != nil {
				if _, expired := asTerminalExpired(err); expired || errors.Is(err, fs.ErrNotExist) {
					registry.remove(ctx, token)
					return nil
				}
				return err
			}
			if delivered == nil {
				return ended.result
			}
			if !*delivered {
				if !session.Alive() {
					registry.remove(ctx, token)
					return nil
				}
				break runs
			}
		}
		if len(frames) < len(batch) {
			return batch[len(batch)-1].end
		}
	}
	return nil
}

// Whether err stands for a Python OSError or RuntimeError, the failures the terminal reports to the tab
func hostTerminalStartFailure(err error) bool {
	var runtimeErr *RuntimeError
	var disconnected *WebSocketDisconnected
	var unavailable *tools.PtyUnavailableError
	return pypath.IsOSError(err) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &runtimeErr) ||
		errors.As(err, &disconnected) || errors.As(err, &unavailable)
}

// Host terminal socket, run after the socket gate and host policy passed, refusals mapped to close codes
func hostTerminal(ctx context.Context, ws *WebSocket) error {
	var session *tools.PtySession
	defer func() {
		if session != nil {
			session.Detach(ws)
		}
	}()
	err := hostTerminalServe(ctx, ws, &session)
	if err == nil {
		return nil
	}
	var full *tools.RegistryFullError
	var disconnect *WebSocketDisconnect
	expired, isExpired := asTerminalExpired(err)
	_, isHTTP := asHTTPException(err)
	switch {
	case isTerminalDenied(err):
		return ws.Close(ctx, 4403, "Terminal belongs to another identity or profile")
	case isExpired && expired.exited:
		return ws.Close(ctx, tools.WSCloseProcessExited, tools.WSCloseProcessExitedReason)
	case isExpired || errors.Is(err, fs.ErrNotExist) || isHTTP:
		return ws.Close(ctx, 4410, "Terminal or profile expired; open a new terminal")
	case errors.As(err, &full):
		return ws.Close(ctx, 1013, "Terminal capacity reached; close a terminal and retry")
	case errors.As(err, &disconnect):
		return nil
	case hostTerminalStartFailure(err):
		return ptyFail(ctx, ws, err, "Terminal")
	}
	return err
}

// Admits, attaches and pumps one host terminal socket, the attached session stored for the caller's detach
func hostTerminalServe(ctx context.Context, ws *WebSocket, attached **tools.PtySession) error {
	registry, err := getHostTerminals(ws.App())
	if err != nil {
		return err
	}
	session, owner, err := hostTerminalAdmit(ctx, ws, registry)
	if err != nil || session == nil {
		return err
	}
	*attached = session
	token := session.Key
	_, reconnected := ws.QueryParams().Lookup("attach")
	initialText := func(truncated bool) string {
		return hostTerminalMetadata(token, owner, registry, reconnected, truncated)
	}
	ok, err := session.Attach(ctx, ws, tools.PtyAttachOptions{InitialText: initialText})
	if err != nil {
		return err
	}
	if !ok {
		if session.Viewer() == nil {
			tools.ClosePtyViewer(ws, tools.WSCloseViewerStalled, tools.WSCloseViewerStalledReason)
		}
		return nil
	}
	return hostPumpInput(ctx, ws, registry, token, session, owner)
}
