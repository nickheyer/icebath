package contexts

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	contextsconfig "github.com/nickheyer/icebath/internal/contexts/config"
	launchconfig "github.com/nickheyer/icebath/internal/launch/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Kind of context engine a plugin adds: what keeps a long conversation within the model's limit. Each session runs an engine of the kind
// the context.engine setting of its profile names
type Kind struct {
	Name  string
	Title string
	About string
	// Engine of one session
	New func() Engine
	// Tools the engine gives the model, offered while the profile's engine is of this kind; a call is handled by the engine of the
	// session that makes it, which then implements Handler
	Tools []tools.Def
}

// What an engine works with, given each time it is asked anything: the session, its settings and store, the helper models of the turn
// and the main model with its window
type Env struct {
	Config  *config.Config
	Session string
	Store   sessions.Store
	// Helper models of the session, routed from its main model; nil outside a turn
	Aux   *auxiliary.Client
	Route provider.Route
	// Tokens the main model takes in at once, and those kept for its reply
	Window  int
	Reserve int
	// Tokens a model takes in at once as icebath knows them, such as the summarising model's; nil where nothing is known of models
	Measure func(ctx context.Context, route provider.Route) int
	// Time zone the dates a summary is anchored on are told in
	Location *time.Location
	// The provider compacts the conversation itself, so nothing is to be prepared for a compaction here
	Native bool
	// The session ends with its turn, such as a one-shot run or a scheduled job
	Ending bool
	// Tells the user how the work goes, such as a summary that failed and fell back; nil where nobody watches
	Notify func(text string)
	// Called as the work makes headway, such as each piece of a summary streamed, for the deadline that gives up a stalled compression
	Progress func()
}

// One compression of a session's messages
type Request struct {
	// Messages the model is sent now, oldest first, the system prompt left out
	Messages []*sessionsv1.Message
	// Size of the request in tokens as last measured, 0 when not known
	Tokens int
	// What the summary should keep above all, as the user asked
	Focus string
	// Asked for by the user: the trigger, the backoffs and the guard against repeated useless compressions are not consulted
	Force bool
	// The cooldown after a failed summary is not consulted, as for a request the provider refused for its size
	Bypass bool
	// Keeps the smallest tail and summary the engine allows, for one manual run
	Aggressive bool
	// What a memory provider hands the summariser to keep, empty for nothing
	Memory string
	// The summary runs on the first entry of the summarising model's fallback chain, for a retry after the configured one stalled
	Entry bool
	// A summary built without the summariser stands in for its own, for a retry after the summariser stalled on every route
	Deterministic bool
}

// How a compression the engine made came to an end once the agent was done with it
type Outcome struct {
	// Its messages were stored in place of the session's
	Stored bool
	// It was refused before it was stored, as it would have grown the transcript
	Refused bool
	// It came back the same as the messages it was given, so nothing was stored
	Unchanged bool
	// Storing it failed this way; empty when it did not
	Failure string
	// Rough size of the request right after it was stored
	Tokens int
}

// What a compression came to
type Result struct {
	// Messages in place of the request's; its own when nothing changed
	Messages []*sessionsv1.Message
	// The engine kept every message as it was, the summary having failed in a way that must not lose context
	Aborted bool
	// A summary built without the summariser stood in for its own, and how many messages it dropped
	Fallback bool
	Dropped  int
	// The summariser was not asked, the middle being too small or the summariser benched after repeated fallbacks
	Skipped bool
	// Why the summary failed, empty when it did not
	Failure string
	// Summarising model that failed before the main model stood in, and how; empty when none did
	AuxModel   string
	AuxFailure string
	// Tool results the pass pruned before summarising
	Pruned int
	// Seconds before the engine compresses again after a failure, 0 for none
	Cooldown float64
	// What the attempt did, for the record of compressions
	Record Record
}

// Content free record of one compression attempt
type Record struct {
	Attempt     string
	Trigger     string
	Method      string
	Failure     string
	Window      int
	Threshold   int
	Before      int
	After       int
	Middle      int
	Head        int
	Tail        int
	Messages    int
	Left        int
	Prepared    bool
	AuxModel    string
	AuxProvider string
}

// Figures of an engine for the views of a session's context
type Status struct {
	// Tokens the newest request held as the provider counted them, the window and the trigger
	Prompt    int
	Window    int
	Threshold int
	// Share of the window the trigger is set at
	Percent float64
	// Compressions of the session so far
	Compressions int
	// Share of its size the last compression saved
	Savings float64
	// The prompt size is the provider's own count rather than an estimate
	Measured bool
	// Why compression is held off now, empty when it isn't
	Blocked string
	// Trigger share the settings gave and the higher one the model raised it to, zero when it was not raised
	Raised [2]float64
	// A compaction was stored whose first real count is still awaited, and the rough size it left; 0 before any
	Awaiting bool
	After    int
	// Share of the trigger a compaction aims to leave, which a session resumed after a pause must be over to be compacted
	Target float64
	// More lines the engine shows about itself
	Lines []string
}

// Context engine of one session. Embed Base for the methods an engine leaves to their defaults
type Engine interface {
	// Takes the session's model, window and settings: as a turn starts, and as the model or what is known of its window changes
	Bind(env *Env)
	// Told a turn starts with the messages the model is sent
	Turn(messages []*sessionsv1.Message)
	// Takes what the newest call of the main model took, nil when the provider said nothing of it; whether it confirms a compression of
	// the turn cleared the trigger, so the turn may compress again
	Observe(usage *provider.Usage) bool
	// Whether a request of that many tokens is to be compressed now, and when it should be but is held off, why
	Due(tokens int) (bool, string)
	// Whether a rough estimate over the trigger waits for the provider's own count of the next request before compressing
	Defer(tokens int) bool
	// Whether the engine asks for a compression below the trigger, for an engine that keeps its own budget
	Maintain(messages []*sessionsv1.Message) bool
	// Whether the messages hold anything between what is always kept to compress
	Compressible(messages []*sessionsv1.Message) bool
	// The messages compressed
	Compress(ctx context.Context, env *Env, request *Request) (*Result, error)
	// The messages with old tool results cut down without summarising, and how many were; the same messages and 0 when none was
	Prune(ctx context.Context, env *Env, messages []*sessionsv1.Message, tokens int) ([]*sessionsv1.Message, int)
	// Messages the request is to carry in place of those given, picked from the conversation; nil keeps them
	Select(request, conversation []*sessionsv1.Message, incoming *sessionsv1.Message, budget int) []*sessionsv1.Message
	// Starts work ahead of the next compression, such as a summary prepared in the background, once the session nears the trigger
	Prepare(ctx context.Context, env *Env, messages []*sessionsv1.Message, origin string)
	// Told a turn ended with the messages it left; a result to store in their place, such as one more exchange folded into a running
	// summary, nil for none
	Finished(ctx context.Context, env *Env, messages []*sessionsv1.Message) (*Result, error)
	// Status line shown while the engine compresses in the phase, from the default given; empty shows none
	Announce(phase, text string) string
	// Told how the result of the last Compress came to an end
	Settled(outcome Outcome)
	// Told the last compression was given up for want of progress or time, the kind saying which, so the next one waits longer
	TimedOut(failure, kind string, idle time.Duration)
	// Told a compression was stored, the session going on as next, which differs from previous when it was continued in a new session
	Compressed(previous, next string)
	// Session start and end
	Start(env *Env)
	End(messages []*sessionsv1.Message)
	Status() Status
}

// Engine that handles the calls of the tools of its kind
type Handler interface {
	Handle(ctx context.Context, call *tools.Call) string
}

// Engine that can shrink a compression that came out larger than what it replaces, toward the size given; nil when it can't
type Salvager interface {
	Salvage(original, candidate []*sessionsv1.Message, budget int) []*sessionsv1.Message
}

// Engine whose attempt can be put back as it was, for a compression given up before it was stored
type Snapshotter interface {
	Snapshot() func()
}

type Registrant interface {
	Kinds() []Kind
}

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for the host's own
	host *Contexts
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

// Host the registry is nested in, through which the agent reaches the engines of its sessions
func (r *Registry) Host() *Contexts {
	return r.host
}

func (r *Registry) Kinds() []Kind {
	var kinds []Kind
	for _, registrant := range r.registrants {
		kinds = append(kinds, registrant.Kinds()...)
	}
	return kinds
}

// Kind registered under the name
func (r *Registry) Kind(name string) (Kind, bool) {
	for _, kind := range r.Kinds() {
		if kind.Name == name {
			return kind, true
		}
	}
	return Kind{}, false
}

type Contexts struct {
	Registry
	mu sync.Mutex
	// Engines of the sessions running here, by session
	live map[string]live
}

// Engine of a session with the kind it was made from
type live struct {
	kind   string
	engine Engine
}

func New(cfg *config.Config, toolset *tools.Registry) *Contexts {
	c := &Contexts{live: map[string]live{}}
	contextsconfig.Register(cfg, func() []config.Option {
		var options []config.Option
		for _, kind := range c.Kinds() {
			options = append(options, config.Option{Value: kind.Name, Title: kind.Title, About: kind.About})
		}
		return options
	})
	toolset.Register(engineTools{c})
	return c
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (c *Contexts) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = c
	}
	c.Registry.Register(registrant)
}

// Kind the settings name for new sessions, the built-in one when they name none registered or the session runs in safe mode, which leaves
// the engines of plugins out; whether it was the one named
func (c *Contexts) Selected(cfg *config.Config) (Kind, bool) {
	name := cfg.GetString(contextsconfig.Engine)
	if cfg.GetBool(launchconfig.SafeMode) {
		name = contextsconfig.Compressor
	}
	if kind, ok := c.Kind(name); ok {
		return kind, true
	}
	kind, ok := c.Kind(contextsconfig.Compressor)
	if !ok && len(c.Kinds()) > 0 {
		kind = c.Kinds()[0]
	}
	return kind, false
}

// Engine of the session, made of the kind its settings name the first time it is asked for; nil when no kind is registered
func (c *Contexts) Open(env *Env) Engine {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.live[env.Session]; ok {
		return held.engine
	}
	kind, _ := c.Selected(env.Config)
	if kind.New == nil {
		return nil
	}
	engine := kind.New()
	c.live[env.Session] = live{kind: kind.Name, engine: engine}
	engine.Bind(env)
	engine.Start(env)
	return engine
}

// Engine of a session running here, nil when none runs
func (c *Contexts) Engine(session string) Engine {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live[session].engine
}

// Moves a session's engine to the session it goes on as, after its compression continued it in a new one
func (c *Contexts) Move(previous, next string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.live[previous]; ok {
		delete(c.live, previous)
		c.live[next] = held
	}
}

// Ends the session's engine with the messages it was left with, once the session is closed here
func (c *Contexts) Close(session string, messages []*sessionsv1.Message) {
	c.mu.Lock()
	held, ok := c.live[session]
	delete(c.live, session)
	c.mu.Unlock()
	if ok {
		held.engine.End(messages)
	}
}

// Kind a session's engine was made from, empty when none runs
func (c *Contexts) kindOf(session string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live[session].kind
}

// Base is the behaviour an engine leaves to the defaults, embedded by engines
type Base struct{}

func (Base) Defer(int) bool                          { return false }
func (Base) Maintain([]*sessionsv1.Message) bool     { return false }
func (Base) Compressible([]*sessionsv1.Message) bool { return true }
func (Base) Prune(_ context.Context, _ *Env, messages []*sessionsv1.Message, _ int) ([]*sessionsv1.Message, int) {
	return messages, 0
}
func (Base) Select(_, _ []*sessionsv1.Message, _ *sessionsv1.Message, _ int) []*sessionsv1.Message {
	return nil
}
func (Base) Prepare(context.Context, *Env, []*sessionsv1.Message, string)           {}
func (Base) Finished(context.Context, *Env, []*sessionsv1.Message) (*Result, error) { return nil, nil }
func (Base) Announce(_, text string) string                                         { return text }
func (Base) Turn([]*sessionsv1.Message)                                             {}
func (Base) Settled(Outcome)                                                        {}
func (Base) TimedOut(string, string, time.Duration)                                 {}
func (Base) Compressed(string, string)                                              {}
func (Base) Start(*Env)                                                             {}
func (Base) End([]*sessionsv1.Message)                                              {}

// Whether the kind offers a tool of the name
func (k Kind) offers(name string) bool {
	return slices.ContainsFunc(k.Tools, func(def tools.Def) bool { return def.Name == name })
}
