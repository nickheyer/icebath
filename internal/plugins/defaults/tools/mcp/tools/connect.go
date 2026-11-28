package tools

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/strs"
)

// States of one target of a connection operation
const (
	TargetPending      = "pending"
	TargetInitiated    = "initiated"
	TargetConnected    = "connected"
	TargetSkipped      = "skipped"
	TargetFailed       = "failed"
	TargetNotConnected = "not_connected"
)

// Why an operation settled
const (
	SettleAllResolved = "all_resolved"
	SettleContinue    = "continue"
	SettleDeadline    = "deadline"
	SettleInterrupt   = "interrupt"
)

// manage_connections actions
var actions = []string{"install", "enable", "authorize"}

const (
	// Longest an operation stays open, and how often it is looked at while nothing wakes it
	operationDeadline = 600 * time.Second
	operationTick     = 5 * time.Second
	// Longest the sign ins of an authorize wait together for their links
	prepareWait = 30 * time.Second
)

const cardNote = "Settled once; do not re-ask on your own for any target the user skipped or that timed out, but a later request from the USER for that " +
	"same app is not a re-ask — run it. Connected targets' tools are callable now under the names listed in tools_listing. A target with " +
	"discovery_error is authorized but its tools are unavailable; retry discovery with manage_connections using that target's authorize or install " +
	"action without asking for consent again."

const noCardNote = "No connection card is drawn for this turn. Show any connect_url to the user so they open it in a browser, then ask them to say when " +
	"they are done. Connected targets' tools are callable now under the names listed in tools_listing. A target with discovery_error is authorized but its " +
	"tools are unavailable; retry discovery with manage_connections using that target's authorize or install action without asking for consent again. Do " +
	"not re-ask on your own for skipped or timed-out targets, but a later request from the USER for that same app is not a re-ask — run it."

// One server a connection operation works on
type Target struct {
	Name         string
	Action       string
	State        string
	Detail       string
	Instructions string
	// Link the person opens to sign in, empty when none is needed, and the sign in it belongs to, which takes the address the browser ended on
	ConnectURL string
	Signin     string
	// Values the person gives before an install, those that have none yet
	Required []mcp.EnvSpec
	// Names of the tools it connected with, and why they could not be listed though it signed in
	Tools          []string
	DiscoveryError string
}

// The servers one manage_connections call works on, with the card the app draws for them
type Operation struct {
	ID       string
	Session  string
	Profile  string
	ToolCall string
	Action   string
	Deadline time.Time
	Targets  []*Target
	// Why it settled, empty while it is open
	Settled string
	Seq     int

	ops      *operations
	cfg      *config.Config
	env      *tools.Env
	detached bool
	wake     chan struct{}
	mu       sync.Mutex
	// Sign ins and installs running, by target
	work     map[string]*work
	approved map[string]map[string]string
}

type work struct {
	signin *Signin
	done   chan struct{}
	err    error
}

// Open operations, one per session, and the apps watching them
type operations struct {
	m        *Manager
	mu       sync.Mutex
	open     map[string]*Operation
	watchers map[chan *Operation]string
}

func newOperations(m *Manager) *operations {
	return &operations{m: m, open: map[string]*Operation{}, watchers: map[chan *Operation]string{}}
}

// Watches the operations of a session, of every session for an empty one, until ctx ends; each change sends a copy of the operation
func (o *operations) Watch(ctx context.Context, session string) <-chan *Operation {
	ch := make(chan *Operation, 16)
	o.mu.Lock()
	o.watchers[ch] = session
	var current []*Operation
	for _, op := range o.open {
		if session == "" || op.Session == session {
			current = append(current, op)
		}
	}
	o.mu.Unlock()
	for _, op := range current {
		ch <- op.Snapshot()
	}
	go func() {
		<-ctx.Done()
		o.mu.Lock()
		delete(o.watchers, ch)
		o.mu.Unlock()
		close(ch)
	}()
	return ch
}

// Whether an app watches the session, so the operation draws a card
func (o *operations) watched(session string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, watching := range o.watchers {
		if watching == "" || watching == session {
			return true
		}
	}
	return false
}

func (o *operations) publish(op *Operation) {
	if op.detached {
		return
	}
	snapshot := op.Snapshot()
	o.mu.Lock()
	defer o.mu.Unlock()
	for ch, session := range o.watchers {
		if session == "" || session == op.Session {
			select {
			case ch <- snapshot:
			default:
			}
		}
	}
}

// Open operation of the id
func (o *operations) get(id string) (*Operation, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, op := range o.open {
		if op.ID == id {
			return op, true
		}
	}
	return nil, false
}

// Copy of the operation as it stands
func (op *Operation) Snapshot() *Operation {
	op.mu.Lock()
	defer op.mu.Unlock()
	out := &Operation{ID: op.ID, Session: op.Session, Profile: op.Profile, ToolCall: op.ToolCall, Action: op.Action, Deadline: op.Deadline, Settled: op.Settled,
		Seq: op.Seq}
	for _, t := range op.Targets {
		c := *t
		c.Required, c.Tools = slices.Clone(t.Required), slices.Clone(t.Tools)
		out.Targets = append(out.Targets, &c)
	}
	return out
}

func (op *Operation) target(name string) *Target {
	for _, t := range op.Targets {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func resolved(state string) bool { return state == TargetConnected || state == TargetSkipped }

// Moves a target, refusing a move the operation's rules do not allow; false once the operation settled
func (op *Operation) move(name, to string, update func(t *Target)) bool {
	op.mu.Lock()
	t := op.target(name)
	if t == nil || op.Settled != "" {
		op.mu.Unlock()
		return false
	}
	allowed := map[string][]string{TargetPending: {TargetInitiated, TargetFailed, TargetSkipped}, TargetInitiated: {TargetConnected, TargetFailed, TargetSkipped},
		TargetFailed: {TargetInitiated, TargetSkipped, TargetFailed}}
	if t.State != to && !slices.Contains(allowed[t.State], to) {
		op.mu.Unlock()
		slog.Debug("connection operation refused a move", "op", op.ID, "target", name, "from", t.State, "to", to)
		return false
	}
	from := t.State
	t.State = to
	if update != nil {
		update(t)
	}
	op.Seq++
	op.mu.Unlock()
	if from != to {
		slog.Info("connection operation target moved", "op", op.ID, "target", name, "from", from, "to", to)
	}
	op.wakeUp()
	op.ops.publish(op)
	return true
}

// Changes a target's fields with no move
func (op *Operation) update(name string, update func(t *Target)) {
	op.mu.Lock()
	if t := op.target(name); t != nil && op.Settled == "" {
		update(t)
		op.Seq++
	}
	op.mu.Unlock()
	op.ops.publish(op)
}

func (op *Operation) wakeUp() {
	select {
	case op.wake <- struct{}{}:
	default:
	}
}

func (op *Operation) settled() bool {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.Settled != ""
}

func (op *Operation) allResolved() bool {
	op.mu.Lock()
	defer op.mu.Unlock()
	for _, t := range op.Targets {
		if !resolved(t.State) {
			return false
		}
	}
	return len(op.Targets) > 0
}

// Freezes the operation once, every target not resolved stamped not connected
func (op *Operation) settle(by string) bool {
	op.mu.Lock()
	if op.Settled != "" {
		op.mu.Unlock()
		return false
	}
	op.Settled = by
	var outcomes []string
	for _, t := range op.Targets {
		if !resolved(t.State) {
			t.State = TargetNotConnected
		}
		outcomes = append(outcomes, t.Name+"="+t.State)
	}
	op.Seq++
	op.mu.Unlock()
	slog.Info("connection operation settled", "op", op.ID, "by", by, "targets", strings.Join(outcomes, ", "))
	op.wakeUp()
	op.ops.publish(op)
	return true
}

// Error a manage_connections call returns
func connectionError(message string) string {
	return tools.JSON(map[string]any{"error": message})
}

// Server names of the connectors argument, or why it is malformed
func targetNames(raw any) ([]string, string) {
	var items []any
	switch v := raw.(type) {
	case nil:
		return nil, ""
	case string, map[string]any:
		items = []any{v}
	case []any:
		items = v
	default:
		return nil, "'connectors' must be a list of MCP server names or {name} objects."
	}
	names := []string{}
	for _, item := range items {
		var name string
		if obj, ok := item.(map[string]any); ok {
			var unknown []string
			for k := range obj {
				if k != "name" {
					unknown = append(unknown, k)
				}
			}
			if len(unknown) > 0 {
				slices.Sort(unknown)
				return nil, fmt.Sprintf("unknown target field(s) %s: a target is a server name or {\"name\": \"<server>\"}. Transport, URLs and credentials "+
					"come from the catalog manifest, never from the call.", strings.Join(unknown, ", "))
			}
			name = fmt.Sprint(orNil(obj["name"]))
		} else {
			name = fmt.Sprint(orNil(item))
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			return nil, "every target needs a non-empty 'name'."
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names, ""
}

// Why the names do not fit the action: install takes catalog entries, the others servers the profile has
func (m *Manager) validNames(cfg *config.Config, action string, names []string) string {
	var catalog []string
	for _, e := range m.catalog.Installable(m.config.In(cfg.Profile())) {
		catalog = append(catalog, e.Name)
	}
	configured := slices.Sorted(maps.Keys(m.pool(cfg).specs()))
	for _, name := range m.StoredNames(cfg) {
		if !slices.Contains(configured, name) {
			configured = append(configured, name)
		}
	}
	slices.Sort(configured)
	allowed := configured
	if action == "install" {
		allowed = catalog
	}
	var unknown []string
	for _, n := range names {
		if !slices.Contains(allowed, n) {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	or := func(s, alt string) string { return cmp.Or(s, alt) }
	if action == "install" {
		out := fmt.Sprintf("unknown MCP server(s) for install: %s. Install works for catalog entries only: %s.", strings.Join(unknown, ", "),
			or(strings.Join(catalog, ", "), "(empty catalog)"))
		if len(configured) > 0 {
			out += fmt.Sprintf(" Already configured (use enable/authorize): %s.", strings.Join(configured, ", "))
		}
		return out
	}
	out := fmt.Sprintf("unknown MCP server(s) for %s: %s. %s works for servers already in %s: %s.", action, strings.Join(unknown, ", "), action,
		"the profile's MCP servers", or(strings.Join(configured, ", "), "(none configured)"))
	if len(catalog) > 0 {
		out += fmt.Sprintf(" Catalog entries you can install: %s.", strings.Join(catalog, ", "))
	}
	return out
}

// Runs one manage_connections call: with an app watching, a card the person works through until every server is connected or skipped, or
// the deadline; without, every target at once, the result carrying the links the person opens
func (m *Manager) manage(ctx context.Context, call *tools.Call) string {
	action := strings.ToLower(strings.TrimSpace(fmt.Sprint(orNil(call.Args["action"]))))
	names, problem := targetNames(call.Args["connectors"])
	if problem != "" {
		return connectionError(problem)
	}
	if !slices.Contains(actions, action) {
		return connectionError(fmt.Sprintf("action must be one of %s.", strings.Join(actions, ", ")))
	}
	if len(names) == 0 {
		return connectionError(fmt.Sprintf("'%s' requires 'connectors': the MCP server name(s), e.g. [\"linear\"].", action))
	}
	cfg := call.Env.Config
	if problem := m.validNames(cfg, action, names); problem != "" {
		return connectionError(problem)
	}
	op := &Operation{ID: strs.Random(6), Session: call.SessionID, Profile: cfg.Profile(), ToolCall: call.ToolCallID, Action: action,
		Deadline: time.Now().Add(operationDeadline), ops: m.ops, cfg: cfg, env: call.Env, wake: make(chan struct{}, 1), work: map[string]*work{},
		approved: map[string]map[string]string{}}
	for _, name := range names {
		op.Targets = append(op.Targets, &Target{Name: name, Action: action, State: TargetPending})
	}
	if !m.ops.watched(call.SessionID) {
		op.detached = true
		return m.detached(ctx, op)
	}
	m.ops.mu.Lock()
	if existing := m.ops.open[call.SessionID]; existing != nil && !existing.settled() {
		m.ops.mu.Unlock()
		return connectionError(fmt.Sprintf("a connection operation is already open in this session (%s); it settles when the user finishes with the card, on "+
			"Continue, or at its deadline. Do not start another.", existing.ID))
	}
	m.ops.open[call.SessionID] = op
	m.ops.mu.Unlock()
	defer func() {
		m.ops.mu.Lock()
		if m.ops.open[call.SessionID] == op {
			delete(m.ops.open, call.SessionID)
		}
		m.ops.mu.Unlock()
		m.close(op)
	}()
	m.prepare(ctx, op)
	if op.allResolved() {
		op.settle(SettleAllResolved)
	}
	m.ops.publish(op)
	m.drive(ctx, op)
	return m.result(op, false, cardNote, "settled")
}

// Looks at the operation each tick, or when something wakes it, until it settles: every target resolved, its deadline, or the call ended
func (m *Manager) drive(ctx context.Context, op *Operation) {
	for !op.settled() {
		if ctx.Err() != nil {
			op.settle(SettleInterrupt)
			return
		}
		if !time.Now().Before(op.Deadline) {
			op.settle(SettleDeadline)
			return
		}
		m.observe(ctx, op)
		if op.allResolved() {
			op.settle(SettleAllResolved)
			return
		}
		t := time.NewTimer(min(operationTick, time.Until(op.Deadline)))
		select {
		case <-op.wake:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
	}
}

// Ends an operation: sign ins it started are given up when it was interrupted, and finish on their own otherwise
func (m *Manager) close(op *Operation) {
	op.mu.Lock()
	works := maps.Clone(op.work)
	settled := op.Settled
	op.mu.Unlock()
	for _, w := range works {
		if w.signin != nil && settled == SettleInterrupt {
			w.signin.Login.Fail(errors.New("The sign-in was canceled."))
		}
	}
}

// First step of each target: an authorize starts its sign in, an install asks for the values it lacks; authorize targets start together and
// their links are waited for once
func (m *Manager) prepare(ctx context.Context, op *Operation) {
	var wg sync.WaitGroup
	for _, t := range op.Snapshot().Targets {
		switch op.Action {
		case "authorize":
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.startSignin(ctx, op, t.Name, nil)
			}()
		case "install":
			entry, ok := m.Entry(op.cfg, t.Name)
			if !ok {
				m.fail(op, t.Name, fmt.Sprintf("“%s” is not in the catalog.", t.Name))
				continue
			}
			op.update(t.Name, func(t *Target) { t.Instructions, t.Required = entry.PostInstall, m.Missing(op.cfg, entry) })
		}
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(prepareWait):
	case <-ctx.Done():
	}
}

// Moves a target to failed whatever it was doing, a second failure refreshing its text
func (m *Manager) fail(op *Operation, name, detail string) {
	op.move(name, TargetFailed, func(t *Target) {
		t.Detail, t.ConnectURL, t.Signin = cmp.Or(mcp.Redact(detail), "It failed without saying why."), "", ""
	})
}

// Starts the sign in of a target, for an install with the settings it installs as, saved with its values once the sign in succeeded
func (m *Manager) startSignin(ctx context.Context, op *Operation, name string, install *installing) {
	instructions := ""
	if entry, ok := m.Entry(op.cfg, name); ok {
		instructions = entry.PostInstall
	}
	op.update(name, func(t *Target) { t.Instructions = instructions })
	var signin *Signin
	var err error
	if install != nil {
		signin, err = m.loginWith(op.cfg, name, install.settings, "browser", install.commit)
	} else {
		signin, err = m.Login(op.cfg, name, "")
	}
	if err != nil {
		m.fail(op, name, err.Error())
		return
	}
	linkCtx, cancel := context.WithTimeout(ctx, prepareWait)
	link, code, err := signin.Login.Link(linkCtx)
	cancel()
	op.mu.Lock()
	op.work[name] = &work{signin: signin}
	op.mu.Unlock()
	if err != nil {
		select {
		case <-signin.Done:
			m.finishSignin(op, name, signin)
		default:
			m.fail(op, name, fmt.Sprintf("The sign-in link did not appear within %s.", strs.Span(prepareWait)))
		}
		return
	}
	detail := ""
	if code != "" {
		detail = "Enter the code " + code + "."
	}
	op.move(name, TargetInitiated, func(t *Target) { t.ConnectURL, t.Signin, t.Detail = link, signin.ID, detail })
}

// What an install saves once its sign in or probe succeeded
type installing struct {
	settings map[string]any
	commit   func() error
}

// Looks at every target still in play once
func (m *Manager) observe(ctx context.Context, op *Operation) {
	for _, t := range op.Snapshot().Targets {
		if t.State != TargetPending && t.State != TargetInitiated {
			continue
		}
		op.mu.Lock()
		w := op.work[t.Name]
		op.mu.Unlock()
		if w == nil {
			continue
		}
		if w.signin != nil {
			select {
			case <-w.signin.Done:
				m.finishSignin(op, t.Name, w.signin)
			default:
			}
			continue
		}
		select {
		case <-w.done:
			op.mu.Lock()
			delete(op.work, t.Name)
			op.mu.Unlock()
			if w.err != nil {
				m.failInstall(op, t.Name, w.err.Error())
				continue
			}
			m.connected(ctx, op, t.Name, "")
		default:
		}
	}
}

func (m *Manager) finishSignin(op *Operation, name string, signin *Signin) {
	op.mu.Lock()
	delete(op.work, name)
	op.mu.Unlock()
	if signin.Err != nil {
		if op.Action == "install" {
			m.failInstall(op, name, signin.Err.Error())
			return
		}
		m.fail(op, name, signin.Err.Error())
		return
	}
	m.connected(context.Background(), op, name, "")
}

// A failed install asks for every declared value again, so the card shows its form once more
func (m *Manager) failInstall(op *Operation, name, detail string) {
	if entry, ok := m.Entry(op.cfg, name); ok {
		op.update(name, func(t *Target) { t.Required = entry.Auth.Env })
	}
	m.fail(op, name, detail)
}

// Connects a server whose settings were just committed and moves its target to connected with the names of its tools
func (m *Manager) connected(ctx context.Context, op *Operation, name, discovery string) {
	p := m.pool(op.cfg)
	if s, ok := p.spec(name); ok && s.Enabled {
		p.mu.Lock()
		srv := p.servers[name]
		p.mu.Unlock()
		if srv == nil || !srv.available() {
			if _, err := p.connect(ctx, s); err != nil {
				discovery = describe(err)
			}
		}
	}
	names := m.ServerTools(op.cfg, name)
	op.move(name, TargetConnected, func(t *Target) {
		t.Tools, t.DiscoveryError, t.ConnectURL, t.Signin, t.Required = names, discovery, "", "", nil
	})
}

// Takes the card's answer: a skip, an approval that starts the target's work with the values given, and Continue, which settles it
func (m *Manager) Respond(id string, answers []Answer, settle bool) error {
	op, ok := m.ops.get(id)
	if !ok {
		return errors.New("This connection card is no longer open.")
	}
	for _, a := range answers {
		t := op.Snapshot().target(strings.ToLower(strings.TrimSpace(a.Name)))
		if t == nil {
			continue
		}
		switch a.Status {
		case "skipped":
			op.mu.Lock()
			w := op.work[t.Name]
			delete(op.work, t.Name)
			op.mu.Unlock()
			if w != nil && w.signin != nil {
				w.signin.Login.Fail(ErrSkippedTarget)
			}
			op.move(t.Name, TargetSkipped, nil)
		case "approved":
			switch t.State {
			case TargetPending, TargetFailed:
				m.approve(op, t, a.Env)
			}
		}
	}
	if settle && !op.allResolved() {
		op.settle(SettleContinue)
	}
	return nil
}

// A target the person skipped
var ErrSkippedTarget = errors.New("You skipped this server.")

// One target's part of the card's answer
type Answer struct {
	Name   string
	Status string
	Env    map[string]string
}

// Starts a target's work once the person approved it: an authorize signs in again, an install installs with the values given, an enable
// turns the server on and connects it
func (m *Manager) approve(op *Operation, t *Target, values map[string]string) {
	ctx := context.Background()
	switch op.Action {
	case "authorize":
		op.move(t.Name, TargetInitiated, func(t *Target) { t.Detail = "" })
		go m.startSignin(ctx, op, t.Name, nil)
	case "enable":
		if !op.move(t.Name, TargetInitiated, func(t *Target) { t.Detail = "" }) {
			return
		}
		go func() {
			if err := m.Enable(op.cfg, t.Name, true); err != nil {
				m.fail(op, t.Name, err.Error())
				return
			}
			m.connected(ctx, op, t.Name, "")
		}()
	case "install":
		entry, ok := m.Entry(op.cfg, t.Name)
		if !ok {
			m.fail(op, t.Name, fmt.Sprintf("“%s” is not in the catalog.", t.Name))
			return
		}
		op.mu.Lock()
		approved := maps.Clone(op.approved[t.Name])
		if approved == nil {
			approved = map[string]string{}
		}
		for k, v := range values {
			if v = strings.TrimSpace(v); v != "" {
				approved[k] = v
			}
		}
		op.approved[t.Name] = approved
		op.mu.Unlock()
		var missing []mcp.EnvSpec
		for _, spec := range m.Missing(op.cfg, entry) {
			if spec.Required && approved[spec.Name] == "" {
				missing = append(missing, spec)
			}
		}
		if len(missing) > 0 {
			names := make([]string, len(missing))
			for i, spec := range missing {
				names[i] = spec.Name
			}
			op.update(t.Name, func(t *Target) {
				t.Required, t.Detail = missing, "Waiting for you to give "+strings.Join(names, ", ")+"."
			})
			return
		}
		if !op.move(t.Name, TargetInitiated, func(t *Target) { t.Required, t.Detail = nil, "" }) {
			return
		}
		go m.install(ctx, op, entry, approved)
	}
}

// Installs an entry for a card: with its own OAuth through a sign in, the settings saved once a token came; else probed with the values
// given and saved only once it answered
func (m *Manager) install(ctx context.Context, op *Operation, entry mcp.Entry, values map[string]string) {
	for k := range values {
		if !slices.ContainsFunc(entry.Auth.Env, func(spec mcp.EnvSpec) bool { return spec.Name == k }) {
			m.failInstall(op, entry.Name, fmt.Sprintf("The catalog entry %s does not ask for %s, so it was not installed.", entry.Name, k))
			return
		}
	}
	var log bytes.Buffer
	dir, err := m.GitInstall(ctx, op.cfg, entry, &log)
	if err != nil {
		m.failInstall(op, entry.Name, err.Error())
		return
	}
	settings := m.InstallConfig(op.cfg, entry, dir, values)
	commit := func() error {
		if issues, err := m.Save(op.cfg, entry.Name, settings); err != nil || len(issues) > 0 {
			return cmp.Or(err, fmt.Errorf("The settings of %s look like an attack, so it was not installed.", entry.Name))
		}
		for _, spec := range entry.Auth.Env {
			if v := values[spec.Name]; v != "" && spec.Secret {
				if err := m.SetValue(op.cfg, spec.Name, v); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if entry.Auth.Type == "oauth" && entry.Auth.Provider == "" {
		m.startSignin(ctx, op, entry.Name, &installing{settings: settings, commit: commit})
		return
	}
	w := &work{done: make(chan struct{})}
	op.mu.Lock()
	op.work[entry.Name] = w
	op.mu.Unlock()
	go func() {
		defer func() {
			close(w.done)
			op.wakeUp()
		}()
		if _, w.err = m.Probe(ctx, op.cfg, entry.Name, settings, 0, values); w.err == nil {
			w.err = commit()
		}
	}()
}

// Runs every target at once with no card, the result carrying the links the person opens
func (m *Manager) detached(ctx context.Context, op *Operation) string {
	for _, t := range op.Snapshot().Targets {
		switch op.Action {
		case "authorize":
			m.startSignin(ctx, op, t.Name, nil)
		case "enable":
			op.move(t.Name, TargetInitiated, nil)
			if err := m.Enable(op.cfg, t.Name, true); err != nil {
				m.fail(op, t.Name, err.Error())
				continue
			}
			m.connected(ctx, op, t.Name, "")
		case "install":
			entry, ok := m.Entry(op.cfg, t.Name)
			if !ok {
				m.fail(op, t.Name, fmt.Sprintf("“%s” is not in the catalog.", t.Name))
				continue
			}
			var missing []string
			for _, spec := range m.Missing(op.cfg, entry) {
				if spec.Required {
					missing = append(missing, spec.Name)
				}
			}
			if len(missing) > 0 {
				m.fail(op, t.Name, fmt.Sprintf("%s has no value yet. Set it in your environment or as a value of MCP servers, then install the "+
					"server again.", strings.Join(missing, ", ")))
				continue
			}
			op.move(t.Name, TargetInitiated, nil)
			m.install(ctx, op, entry, map[string]string{})
			op.mu.Lock()
			w := op.work[t.Name]
			op.mu.Unlock()
			if w != nil && w.done != nil {
				select {
				case <-w.done:
				case <-ctx.Done():
				}
				m.observe(ctx, op)
			}
		}
	}
	status := "settled"
	for _, t := range op.Snapshot().Targets {
		if t.State == TargetInitiated {
			status = "initiated"
		}
	}
	return m.result(op, true, noCardNote, status)
}

// Result the model reads: each target with its state and what it needs or connected with, the tools of a connected one listed
func (m *Manager) result(op *Operation, links bool, note, status string) string {
	snap := op.Snapshot()
	var targets []any
	for _, t := range snap.Targets {
		row := map[string]any{"name": t.Name, "kind": "mcp", "action": t.Action, "state": t.State}
		if t.Detail != "" {
			row["detail"] = t.Detail
		}
		if links && t.ConnectURL != "" {
			row["connect_url"] = t.ConnectURL
		}
		if snap.Settled == "" && t.Instructions != "" {
			row["instructions"] = t.Instructions
		}
		if len(t.Required) > 0 && snap.Settled == "" {
			var required []any
			for _, spec := range t.Required {
				required = append(required, map[string]any{"name": spec.Name, "prompt": spec.Prompt, "required": spec.Required, "secret": spec.Secret})
			}
			row["required_env"] = required
		}
		if t.State == TargetConnected {
			row["tools"] = t.Tools
			if listing := m.listing(op.cfg, t.Tools); listing != "" {
				row["tools_listing"] = listing
			}
		}
		if t.DiscoveryError != "" {
			row["discovery_error"] = t.DiscoveryError
		}
		targets = append(targets, row)
	}
	out := map[string]any{"op_id": snap.ID, "seq": snap.Seq, "deadline_at": float64(snap.Deadline.UnixNano()) / 1e9, "targets": targets, "status": status, "note": note}
	if snap.Settled != "" {
		out["settled_by"] = snap.Settled
	}
	return tools.JSON(out)
}

// Names and first lines of the tools given, cut to 1500 characters
func (m *Manager) listing(cfg *config.Config, names []string) string {
	var b strings.Builder
	for _, def := range m.pool(cfg).definitions() {
		if !slices.Contains(names, def.Name) {
			continue
		}
		description, _ := def.Schema["description"].(string)
		description, _, _ = strings.Cut(description, "\n")
		line := def.Name
		if description != "" {
			line += ": " + description
		}
		if b.Len()+len(line)+1 > 1500 {
			b.WriteString("…")
			break
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Wakes the operation of the id to look again at once
func (m *Manager) Wake(id string) {
	if op, ok := m.ops.get(id); ok {
		op.wakeUp()
	}
}

// Runs the named targets of an open operation again
func (m *Manager) Retry(id string, names []string) error {
	op, ok := m.ops.get(id)
	if !ok {
		return errors.New("This connection card is no longer open.")
	}
	if op.settled() {
		return errors.New("This connection card is finished and can no longer change.")
	}
	for _, name := range names {
		if t := op.Snapshot().target(name); t != nil {
			op.mu.Lock()
			values := op.approved[name]
			op.mu.Unlock()
			m.approve(op, t, values)
		}
	}
	return nil
}

// Watches the connection operations of a session, every session for an empty one
func (m *Manager) WatchOperations(ctx context.Context, session string) <-chan *Operation {
	return m.ops.Watch(ctx, session)
}
