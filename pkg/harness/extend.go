package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Session label naming the kind the session runs as
const kindLabel = "kind"

// Directory under a configuration directory holding one kind per JSON file
const KindsDir = "kinds"

// Named recipe an agent is built from, a session type
type Kind struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Base prompt sent in place of the built in one
	System string `json:"system,omitempty"`
	// Text appended to the base prompt
	Append string `json:"append,omitempty"`
	// Tool names offered, every tool when empty
	Tools []string `json:"tools,omitempty"`
	// Tool names withheld
	Deny []string `json:"deny,omitempty"`
	// Profile and model sessions created with the kind start on, the default endpoint when nil
	Model *Target `json:"model,omitempty"`
	// Tool rounds one run may take, the configured limit when zero
	MaxRounds int `json:"max_rounds,omitempty"`
	// Output tokens one model turn may produce, the configured limit when zero
	MaxTokens int `json:"max_tokens,omitempty"`
	// Hooks every agent of the kind runs with
	Hooks []agent.Hook `json:"-"`
	// Extension or file the kind came from
	Source string `json:"-"`
}

// Name that stands for no kind wherever a kind is chosen
const NoKind = "none"

// Checks that the kind has a usable name and lists no tool as both offered and withheld
func (k Kind) Validate() error {
	if !profiles.ValidName(k.Name) || k.Name == NoKind {
		return fmt.Errorf("kind %q: invalid name", k.Name)
	}
	for _, name := range k.Deny {
		if slices.Contains(k.Tools, name) {
			return fmt.Errorf("kind %s: tool %s is both offered and withheld", k.Name, name)
		}
	}
	if k.Model != nil && k.Model.Profile == "" {
		return fmt.Errorf("kind %s: model needs a profile", k.Name)
	}
	if k.MaxRounds < 0 {
		return fmt.Errorf("kind %s: max_rounds must not be negative", k.Name)
	}
	if k.MaxTokens < 0 {
		return fmt.Errorf("kind %s: max_tokens must not be negative", k.Name)
	}
	return nil
}

// Reports whether the kind offers the tool
func (k Kind) Offers(name string) bool {
	if len(k.Tools) > 0 && !slices.Contains(k.Tools, name) {
		return false
	}
	return !slices.Contains(k.Deny, name)
}

// Setting an extension adds to the configuration, saved under settings in the configuration file
type Setting struct {
	Key  string
	Help string
	// Values the setting may take, a choice when set
	Choices []string
	// Floor, ceiling and step of a number, no ceiling when it sits at or under the floor
	Lo, Hi, Step float64
	Integer      bool
	// Further check of a value after the choice or range one
	Check func(value string) error
	// Shown masked wherever it is edited, for a key or token
	Secret bool
	// Extension the setting came from
	Source string
}

// Reports whether the setting takes a number
func (s Setting) Number() bool { return s.Step > 0 }

// Checks a value against the choices, the range and the extra check
func (s Setting) Validate(value string) error {
	switch {
	case len(s.Choices) > 0:
		if !slices.Contains(s.Choices, value) {
			return fmt.Errorf("%s must be one of %s, got %q", s.Key, strings.Join(s.Choices, ", "), value)
		}
	case s.Number():
		f, err := strconv.ParseFloat(value, 64)
		switch {
		case s.Hi > s.Lo && (err != nil || f < s.Lo || f > s.Hi):
			return fmt.Errorf("%s must be a number from %g to %g, got %q", s.Key, s.Lo, s.Hi, value)
		case err != nil || f < s.Lo:
			return fmt.Errorf("%s must be a number of at least %g, got %q", s.Key, s.Lo, value)
		}
		if s.Integer && f != float64(int64(f)) {
			return fmt.Errorf("%s must be an integer, got %q", s.Key, value)
		}
	}
	if s.Check != nil {
		return s.Check(value)
	}
	return nil
}

// Agent being built, as hook builders see it
type AgentInfo struct {
	Name     string
	Session  *session.Session
	Endpoint Endpoint
	Kind     Kind
	App      *App
}

// Named procedure that drives sessions and runs, offered as a command
type Workflow struct {
	Name        string
	Title       string
	Description string
	// Placeholder of the argument, asked for when the command comes without one, none when empty
	Arg string
	Run func(ctx context.Context, f *Flow, arg string) error
	// Extension the workflow came from
	Source string
}

// Checks that the workflow has a usable name and something to run
func (w Workflow) Validate() error {
	if !profiles.ValidName(w.Name) {
		return fmt.Errorf("workflow %q: invalid name", w.Name)
	}
	if w.Run == nil {
		return fmt.Errorf("workflow %s: run is required", w.Name)
	}
	return nil
}

var (
	extMu        sync.RWMutex
	kinds        []Kind
	settings     []Setting
	workflows    []Workflow
	hookBuilders []func(AgentInfo) agent.Hook
	fragments    []func(context.Context, *App, Endpoint) string
	toolBuilders []func(*App) ([]tool.Tool, error)
)

// Makes a kind available to every app built later, refusing an invalid or duplicate one
func RegisterKind(k Kind) error {
	if err := k.Validate(); err != nil {
		return err
	}
	extMu.Lock()
	defer extMu.Unlock()
	if slices.ContainsFunc(kinds, func(have Kind) bool { return have.Name == k.Name }) {
		return fmt.Errorf("kind %s is already registered", k.Name)
	}
	kinds = append(kinds, k)
	return nil
}

// Kinds registered so far, sorted by name
func RegisteredKinds() []Kind {
	extMu.RLock()
	defer extMu.RUnlock()
	out := append([]Kind(nil), kinds...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Makes a setting known to the configuration, refusing a built in key, an invalid default or a duplicate
func RegisterSetting(s Setting) error {
	if !profiles.ValidName(s.Key) {
		return fmt.Errorf("setting %q: invalid key", s.Key)
	}
	if _, err := settingPath(s.Key); err == nil {
		return fmt.Errorf("setting %s is built in", s.Key)
	}
	extMu.Lock()
	defer extMu.Unlock()
	if slices.ContainsFunc(settings, func(have Setting) bool { return have.Key == s.Key }) {
		return fmt.Errorf("setting %s is already registered", s.Key)
	}
	settings = append(settings, s)
	return nil
}

// Settings registered so far, in registration order
func Settings() []Setting {
	extMu.RLock()
	defer extMu.RUnlock()
	return append([]Setting(nil), settings...)
}

// Setting registered under the key
func LookupSetting(key string) (Setting, bool) {
	extMu.RLock()
	defer extMu.RUnlock()
	for _, s := range settings {
		if s.Key == key {
			return s, true
		}
	}
	return Setting{}, false
}

// Makes a workflow available as a command, refusing an invalid or duplicate one
func RegisterWorkflow(w Workflow) error {
	if err := w.Validate(); err != nil {
		return err
	}
	extMu.Lock()
	defer extMu.Unlock()
	if slices.ContainsFunc(workflows, func(have Workflow) bool { return have.Name == w.Name }) {
		return fmt.Errorf("workflow %s is already registered", w.Name)
	}
	workflows = append(workflows, w)
	return nil
}

// Workflows registered so far, in registration order
func Workflows() []Workflow {
	extMu.RLock()
	defer extMu.RUnlock()
	return append([]Workflow(nil), workflows...)
}

// Workflow registered under the name
func LookupWorkflow(name string) (Workflow, bool) {
	extMu.RLock()
	defer extMu.RUnlock()
	for _, w := range workflows {
		if w.Name == name {
			return w, true
		}
	}
	return Workflow{}, false
}

// Adds a hook every agent built later runs with, built per agent
func RegisterHook(build func(AgentInfo) agent.Hook) error {
	if build == nil {
		return errors.New("hook: a builder is required")
	}
	extMu.Lock()
	defer extMu.Unlock()
	hookBuilders = append(hookBuilders, build)
	return nil
}

// Adds text to the system prompt of every run, composed per turn and left out when empty
func RegisterPrompt(fragment func(ctx context.Context, a *App, ep Endpoint) string) error {
	if fragment == nil {
		return errors.New("prompt: a fragment is required")
	}
	extMu.Lock()
	defer extMu.Unlock()
	fragments = append(fragments, fragment)
	return nil
}

// Adds tools every app built later offers, built once the app stands
func RegisterTools(build func(a *App) ([]tool.Tool, error)) error {
	if build == nil {
		return errors.New("tools: a builder is required")
	}
	extMu.Lock()
	defer extMu.Unlock()
	toolBuilders = append(toolBuilders, build)
	return nil
}

func hooksRegistered() []func(AgentInfo) agent.Hook {
	extMu.RLock()
	defer extMu.RUnlock()
	return append([]func(AgentInfo) agent.Hook(nil), hookBuilders...)
}

func fragmentsRegistered() []func(context.Context, *App, Endpoint) string {
	extMu.RLock()
	defer extMu.RUnlock()
	return append([]func(context.Context, *App, Endpoint) string(nil), fragments...)
}

func toolsRegistered() []func(*App) ([]tool.Tool, error) {
	extMu.RLock()
	defer extMu.RUnlock()
	return append([]func(*App) ([]tool.Tool, error)(nil), toolBuilders...)
}

// Reads every kind file under the directory, none when the directory does not exist
func loadKindDir(dir string) ([]Kind, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Kind
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var k Kind
		if err := json.Unmarshal(data, &k); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if k.Name == "" {
			k.Name = strings.TrimSuffix(e.Name(), ".json")
		}
		if err := k.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		k.Source = path
		out = append(out, k)
	}
	return out, nil
}

// Kinds from the registry and the kind directories, a file namesake replacing a registered one and the project the user
func loadKinds(dir string) ([]Kind, error) {
	out := RegisteredKinds()
	userDir, err := profiles.ConfigDir()
	if err != nil {
		return nil, err
	}
	dirs := []string{filepath.Join(userDir, KindsDir)}
	if dir != "" {
		dirs = append(dirs, filepath.Join(dir, ProjectDir, KindsDir))
	}
	for _, d := range dirs {
		list, err := loadKindDir(d)
		if err != nil {
			return nil, err
		}
		for _, k := range list {
			if i := slices.IndexFunc(out, func(have Kind) bool { return have.Name == k.Name }); i >= 0 {
				out[i] = k
				continue
			}
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Kinds the app knows, sorted by name
func (a *App) Kinds() []Kind {
	a.kindMu.RLock()
	defer a.kindMu.RUnlock()
	return append([]Kind(nil), a.kinds...)
}

// Kind by name
func (a *App) Kind(name string) (Kind, bool) {
	a.kindMu.RLock()
	defer a.kindMu.RUnlock()
	for _, k := range a.kinds {
		if k.Name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// Replaces the kind of the same name or adds it, so runs started from now on follow it, refusing an invalid one
func (a *App) PutKind(k Kind) error {
	if err := k.Validate(); err != nil {
		return err
	}
	a.kindMu.Lock()
	defer a.kindMu.Unlock()
	if i := slices.IndexFunc(a.kinds, func(have Kind) bool { return have.Name == k.Name }); i >= 0 {
		a.kinds[i] = k
		return nil
	}
	a.kinds = append(a.kinds, k)
	sort.Slice(a.kinds, func(i, j int) bool { return a.kinds[i].Name < a.kinds[j].Name })
	return nil
}

// Kind the session runs as, false while it runs as none or as one the app no longer knows
func (a *App) KindOf(sess *session.Session) (Kind, bool) {
	name := sess.Labels()[kindLabel]
	if name == "" {
		return Kind{}, false
	}
	return a.Kind(name)
}

// Kinds as the subagent tool offers them
func (a *App) agentKinds() []agent.Kind {
	kinds := a.Kinds()
	out := make([]agent.Kind, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, agent.Kind{Name: k.Name, Description: k.Description})
	}
	return out
}

// Opens a fresh session of the kind beside the ones already open, on the endpoint the kind names or the default
func (a *App) CreateKind(ctx context.Context, name string) (*Live, error) {
	if name == "" || name == NoKind {
		return a.Create()
	}
	k, ok := a.Kind(name)
	if !ok {
		return nil, fmt.Errorf("harness: no kind %q", name)
	}
	ep := a.Primary
	if k.Model != nil {
		var err error
		if ep, err = a.open(ctx, *k.Model); err != nil {
			return nil, fmt.Errorf("kind %s: %w", name, err)
		}
	}
	sess := session.New()
	sess.Label(kindLabel, name)
	return a.live(sess, ep)
}

// Opens a fresh session on an opened endpoint beside the ones already open, running as the kind when the app knows
// it and as none otherwise
func (a *App) CreateOn(ep Endpoint, kind string) (*Live, error) {
	sess := session.New()
	if _, ok := a.Kind(kind); ok && kind != NoKind {
		sess.Label(kindLabel, kind)
	}
	return a.live(sess, ep)
}

// Makes an open session run as the kind from its next run, an empty name or none returning it to the plain agent
func (a *App) SetKindOn(l *Live, name string) error {
	if name == NoKind {
		name = ""
	}
	if name != "" {
		if _, ok := a.Kind(name); !ok {
			return fmt.Errorf("harness: no kind %q", name)
		}
	}
	l.Session.Label(kindLabel, name)
	a.Saver.Touch(l.Session)
	return nil
}

// Name of the kind the session runs as, empty while it runs as none
func (l *Live) Kind() string { return l.Session.Labels()[kindLabel] }

// Hook that offers an agent of a kind only the tools the kind names and refuses calls to the rest
type kindTools struct {
	agent.Hooks
	kind Kind
}

func (h kindTools) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	kept := req.Tools[:0]
	for _, t := range req.Tools {
		if h.kind.Offers(t.Name) {
			kept = append(kept, t)
		}
	}
	req.Tools = kept
	return nil
}

func (h kindTools) BeforeTool(ctx context.Context, call protocol.ToolCall) (agent.Advice, error) {
	if !h.kind.Offers(call.Name) {
		return agent.Advice{Call: call}, fmt.Errorf("tool %s is not offered to kind %s", call.Name, h.kind.Name)
	}
	return agent.Advice{Call: call}, nil
}

// Weight of a notice
type Level int

const (
	Info Level = iota
	Success
	Warn
	Error
)

// Text a workflow or extension wants the person to see
type NoticeMsg struct {
	Text  string
	Level Level
	// Workflow that raised it, empty for none
	Flow string
}

// Session a workflow opened, so a card may show it
type OpenedMsg struct {
	Live *Live
	Flow string
}

// Workflow that reached its end
type FlowDoneMsg struct {
	ID       string
	Name     string
	Err      error
	Duration time.Duration
}

// One running or finished workflow with what it may do
type Flow struct {
	ID   string
	Name string
	Arg  string
	App  *App
	// Session the workflow was started from, nil when it started without one
	Live     *Live
	Started  time.Time
	send     Sender
	mu       sync.Mutex
	status   string
	finished time.Time
	err      error
	running  bool
	cancel   context.CancelFunc
	done     chan struct{}
}

// Reports whether the workflow still goes
func (f *Flow) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

// Error the workflow ended with
func (f *Flow) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Time the workflow ended, zero while it goes
func (f *Flow) Finished() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finished
}

// Channel closed when the workflow ends
func (f *Flow) Done() <-chan struct{} { return f.done }

// Time the workflow has taken so far or took
func (f *Flow) Elapsed() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finished.IsZero() {
		return time.Since(f.Started)
	}
	return f.finished.Sub(f.Started)
}

// Words shown beside the workflow, what it is doing now
func (f *Flow) Status() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// Sets the words shown beside the workflow
func (f *Flow) SetStatus(text string) {
	f.mu.Lock()
	f.status = text
	f.mu.Unlock()
}

// Shows the person a notice, a toast in the terminal and a line when headless
func (f *Flow) Notify(level Level, text string) {
	f.send(NoticeMsg{Text: text, Level: level, Flow: f.ID})
}

// Opens a fresh session of the kind, the default kind when empty, and announces it so a card shows it
func (f *Flow) Open(ctx context.Context, kind string) (*Live, error) {
	l, err := f.App.CreateKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	f.send(OpenedMsg{Live: l, Flow: f.ID})
	return l, nil
}

// Starts a run on the session with the input, assigning it to a mission when one is named, refusing a session with a run going
func (f *Flow) Start(ctx context.Context, l *Live, missionID string, input []protocol.Part) (*Run, error) {
	if l == nil {
		return nil, errors.New("harness: a session is required")
	}
	if f.App.Busy(l) {
		return nil, fmt.Errorf("harness: a run is going on %s", l.Session.ID())
	}
	if l.Session.Title() == "" {
		l.Session.SetTitle(title(protocol.Response{Content: input}.Text()))
	}
	return f.App.RunOn(ctx, l, missionID, input)
}

// Waits for a run to end and hands back its final response or error
func (f *Flow) Wait(ctx context.Context, r *Run) (protocol.Response, error) {
	done, ok := f.App.Control.Done(r.ID)
	if !ok {
		return protocol.Response{}, fmt.Errorf("harness: no run %s", r.ID)
	}
	select {
	case <-done:
		return r.Response(), r.Err()
	case <-ctx.Done():
		return protocol.Response{}, ctx.Err()
	}
}

// Queues input the run sees at its next turn
func (f *Flow) Steer(r *Run, parts ...protocol.Part) error {
	return f.App.Control.Steer(r.ID, parts...)
}

// Title derived from the first line of a prompt, cut to sixty runes
func title(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	runes := []rune(strings.TrimSpace(line))
	if len(runes) > 60 {
		return string(runes[:59]) + "~"
	}
	return string(runes)
}

func (f *Flow) finish(err error) {
	f.mu.Lock()
	f.running, f.finished, f.err = false, time.Now(), err
	f.mu.Unlock()
}

// Starts, lists and stops workflows and keeps a registry of them
type Flows struct {
	app   *App
	send  Sender
	mu    sync.Mutex
	flows map[string]*Flow
	order []string
	next  int
	wg    sync.WaitGroup
}

func NewFlows(app *App, send Sender) *Flows {
	return &Flows{app: app, send: send, flows: map[string]*Flow{}}
}

// Starts the workflow in the background from the session, which may be nil, reporting its end as a message
func (fs *Flows) Start(ctx context.Context, w Workflow, live *Live, arg string) (*Flow, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	arg = strings.TrimSpace(arg)
	if w.Arg != "" && arg == "" {
		return nil, fmt.Errorf("workflow %s needs %s", w.Name, w.Arg)
	}
	fctx, cancel := context.WithCancel(ctx)
	fs.mu.Lock()
	fs.next++
	id := w.Name + "-" + strconv.Itoa(fs.next)
	f := &Flow{ID: id, Name: w.Name, Arg: arg, App: fs.app, Live: live, Started: time.Now(), send: fs.send, running: true, cancel: cancel, done: make(chan struct{})}
	fs.flows[id] = f
	fs.order = append(fs.order, id)
	fs.mu.Unlock()
	fs.wg.Add(1)
	go func() {
		defer fs.wg.Done()
		defer cancel()
		err := w.Run(fctx, f, arg)
		f.finish(err)
		close(f.done)
		fs.send(FlowDoneMsg{ID: id, Name: w.Name, Err: err, Duration: time.Since(f.Started)})
	}()
	return f, nil
}

func (fs *Flows) Get(id string) (*Flow, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f, ok := fs.flows[id]
	return f, ok
}

// Every workflow started, in start order
func (fs *Flows) List() []*Flow {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]*Flow, 0, len(fs.order))
	for _, id := range fs.order {
		out = append(out, fs.flows[id])
	}
	return out
}

// Workflows still going, in start order
func (fs *Flows) Active() []*Flow {
	var out []*Flow
	for _, f := range fs.List() {
		if f.Running() {
			out = append(out, f)
		}
	}
	return out
}

// Stops a workflow and the runs it started
func (fs *Flows) Cancel(id string) error {
	f, ok := fs.Get(id)
	if !ok {
		return fmt.Errorf("harness: no workflow %s", id)
	}
	if !f.Running() {
		return fmt.Errorf("harness: workflow %s has finished", id)
	}
	f.cancel()
	return nil
}

// Stops every workflow and waits for them to end
func (fs *Flows) Shutdown() {
	for _, f := range fs.List() {
		if f.Running() {
			f.cancel()
		}
	}
	fs.wg.Wait()
}
