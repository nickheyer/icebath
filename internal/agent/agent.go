package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/state"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/approval"
	"github.com/nickheyer/icebath/internal/tool/checkpoint"
	"github.com/nickheyer/icebath/internal/tool/env"
	"github.com/nickheyer/icebath/internal/tool/process"
	"github.com/nickheyer/icebath/internal/tool/redact"
)

type Options struct {
	SessionID         string
	ParentSessionID   string
	Source            string
	SourceEnv         SourceEnv
	Platform          string
	Origin            tool.Origin
	GatewaySessionKey string
	Cwd               string
	TurnOrigin        string
	DelegateDepth     int
	DelegateRole      string

	Target         model.Target
	Requested      string
	Mode           wire.Mode
	Command        string
	Args           []string
	Fallback       []config.FallbackEntry
	Pool           *auth.Pool
	Capabilities   map[string]bool
	Reasoning      *wire.Reasoning
	ServiceTier    string
	MaxTokens      int
	Overrides      map[string]json.RawMessage
	Routing        config.ProviderRouting
	MinCodingScore *float64
	PassSessionID  bool

	MaxIterations     int
	Budget            *IterationBudget
	RunBudget         time.Duration
	Toolsets          tool.Selection
	EphemeralSystem   string
	Prefill           []wire.Message
	SkipContextFiles  bool
	LoadSoulIdentity  bool
	SkipMemory        bool
	SkipReview        bool
	SafeWorker        bool
	SaveTrajectories  bool
	Quiet             bool
	ToolProgress      string
	Checkpoints       bool
	SideAgent         bool
	PersistDisabled   bool
	EndSessionOnClose bool
	OwnsStore         bool
	SkipToolRefresh   bool

	Config    *config.Store
	Homes     *home.Resolver
	Router    *model.Router
	Side      tool.SideModel
	Catalog   *catalog.Catalog
	Store     *state.Store
	Registry  *tool.Registry
	Executor  *tool.Executor
	Spill     *tool.Spill
	Approval  *approval.Gate
	Snapshots *checkpoint.Manager
	Envs      *env.Manager
	Processes *process.Registry
	Redactor  *redact.Redactor
	Secrets   *secret.Scope

	Events    Sink
	Surface   tool.Surface
	Stream    func(text string)
	Engine    ContextEngine
	Compactor Compactor
	Reviewer  Reviewer
	MoA       Mixture
	Memory    Memory
	Skills    Skills
	Hooks     Hooks
	LSP       LSP
	StopGates []StopGate
	Recovery  []TurnRecovery
	Observers []ToolObserver
	Metrics   Metrics
	Trace     Trace
	Text      Catalog
	Bridge    ActivityBridge
	Clock     func() time.Time
}

const (
	ToolProgressAll = "all"

	TurnOriginReview       = "review"
	TurnOriginSideQuestion = "side_question"
)

type OptionError struct {
	Field  string
	Reason string
}

func (e *OptionError) Error() string {
	return "agent: option " + e.Field + " " + e.Reason
}

func (o *Options) validate() error {
	required := []struct {
		name    string
		missing bool
	}{
		{"config", o.Config == nil},
		{"homes", o.Homes == nil},
		{"router", o.Router == nil},
		{"side model", o.Side == nil},
		{"catalog", o.Catalog == nil},
		{"store", o.Store == nil && !o.PersistDisabled},
		{"registry", o.Registry == nil},
		{"executor", o.Executor == nil},
		{"spill", o.Spill == nil},
		{"approval", o.Approval == nil},
		{"snapshots", o.Snapshots == nil && o.Checkpoints},
		{"envs", o.Envs == nil},
		{"processes", o.Processes == nil},
		{"redactor", o.Redactor == nil},
		{"secrets", o.Secrets == nil},
		{"events", o.Events == nil},
		{"engine", o.Engine == nil},
		{"compactor", o.Compactor == nil},
		{"reviewer", o.Reviewer == nil && !o.SkipReview},
		{"memory", o.Memory == nil && !o.SkipMemory},
	}
	for _, r := range required {
		if r.missing {
			return &tool.UnfilledError{Name: r.name}
		}
	}
	if o.MaxIterations < 0 {
		return &OptionError{Field: "MaxIterations", Reason: "is negative"}
	}
	if o.DelegateDepth < 0 {
		return &OptionError{Field: "DelegateDepth", Reason: "is negative"}
	}
	if o.MaxTokens < 0 {
		return &OptionError{Field: "MaxTokens", Reason: "is negative"}
	}
	return nil
}

type Agent struct {
	o Options

	clock         func() time.Time
	sessionStart  time.Time
	sessionsDir   string
	maxIterations int
	budget        *IterationBudget
	limits        limits
	events        Sink

	idMu            sync.RWMutex
	sessionID       string
	parentSessionID string
	source          string
	turnID          string

	control  control
	activity activityState
	status   statusState
	children *ChildRegistry

	muted           atomic.Bool
	mutePost        atomic.Bool
	userIntervened  atomic.Bool
	running         atomic.Bool
	itersSinceSkill atomic.Int64
}

func New(ctx context.Context, o Options) (*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	if o.SafeWorker {
		o.SkipContextFiles, o.SkipMemory, o.SkipReview = true, true, true
		o.LoadSoulIdentity = false
		o.Prefill = nil
		o.Fallback = []config.FallbackEntry{}
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	o.Target.Provider = strings.ToLower(trimText(o.Target.Provider))
	if o.Requested = strings.ToLower(trimText(o.Requested)); o.Requested == "" {
		o.Requested = o.Target.Provider
	}
	o.Args = slices.Clone(o.Args)
	if o.ToolProgress == "" {
		o.ToolProgress = ToolProgressAll
	}
	if o.RunBudget < 0 {
		o.RunBudget = 0
	}
	clock := o.Clock
	if clock == nil {
		clock = time.Now
	}
	start := clock()
	sessionID := o.SessionID
	if sessionID == "" {
		minted, err := state.NewSessionID(start, state.DefaultSessionIDHexLen)
		if err != nil {
			return nil, fmt.Errorf("agent: mint a session id: %w", err)
		}
		sessionID = minted
	}
	source := o.Source
	if source == "" {
		source = SessionSource(o.Platform, o.SourceEnv)
	}
	maxIterations := o.MaxIterations
	if maxIterations == 0 {
		maxIterations = tool.Unlimited
	}
	budget := o.Budget
	if budget == nil {
		budget = NewIterationBudget(maxIterations)
	}
	homeDir, err := o.Homes.Home(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: resolve the home: %w", err)
	}
	sessionsDir := home.Sessions.In(homeDir)
	if err := o.Homes.Mkdir(ctx, sessionsDir); err != nil {
		return nil, fmt.Errorf("agent: create %s: %w", sessionsDir, err)
	}
	a := &Agent{
		o:               o,
		clock:           clock,
		sessionStart:    start,
		sessionsDir:     sessionsDir,
		maxIterations:   maxIterations,
		budget:          budget,
		limits:          limitsFrom(nil, o.RunBudget),
		events:          o.Events,
		sessionID:       sessionID,
		parentSessionID: o.ParentSessionID,
		source:          source,
		children:        &ChildRegistry{},
	}
	a.activity.lastAt = start
	a.activity.description = "initializing"
	a.activity.provenance = state.ProvenanceUnknown
	return a, nil
}

func (a *Agent) now() time.Time {
	return a.clock()
}

func (a *Agent) SessionID() string {
	a.idMu.RLock()
	defer a.idMu.RUnlock()
	return a.sessionID
}

func (a *Agent) ParentSessionID() string {
	a.idMu.RLock()
	defer a.idMu.RUnlock()
	return a.parentSessionID
}

func (a *Agent) Source() string {
	a.idMu.RLock()
	defer a.idMu.RUnlock()
	return a.source
}

func (a *Agent) Platform() string {
	return a.o.Platform
}

func (a *Agent) currentTurnID() string {
	a.idMu.RLock()
	defer a.idMu.RUnlock()
	return a.turnID
}

func (a *Agent) setTurnID(id string) {
	a.idMu.Lock()
	a.turnID = id
	a.idMu.Unlock()
}

func (a *Agent) rebindSession(sessionID, parentSessionID string) {
	a.idMu.Lock()
	a.sessionID, a.parentSessionID = sessionID, parentSessionID
	a.idMu.Unlock()
}

func (a *Agent) beginTurn() error {
	if !a.running.CompareAndSwap(false, true) {
		return &BusyError{SessionID: a.SessionID()}
	}
	a.setRunning(true)
	return nil
}

func (a *Agent) endTurn() {
	a.setRunning(false)
	a.running.Store(false)
}

func (a *Agent) Budget() *IterationBudget {
	return a.budget
}

func (a *Agent) Engine() ContextEngine {
	return a.o.Engine
}

func (a *Agent) Store() *state.Store {
	return a.o.Store
}

func (a *Agent) Hooks() Hooks {
	return a.o.Hooks
}

func (a *Agent) Metrics() Metrics {
	return a.o.Metrics
}

func safeSessionFilenameComponent(sessionID string) string {
	raw := trimText(sessionID)
	var b strings.Builder
	for _, r := range raw {
		if r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	runes := []rune(strings.Trim(b.String(), "._"))
	if len(runes) > 96 {
		runes = runes[:96]
	}
	sanitized := string(runes)
	if sanitized == "" {
		sanitized = "session"
	}
	if raw != "" && sanitized == raw {
		return sanitized
	}
	sum := sha256.Sum256([]byte(raw))
	return sanitized + "_" + hex.EncodeToString(sum[:])[:12]
}

func (a *Agent) Children() *ChildRegistry {
	return a.children
}

func (a *Agent) Attended(finiteTurn bool) bool {
	return Attended(a.o.Platform, a.o.SourceEnv, finiteTurn)
}

type ChildRegistry struct {
	mu       sync.Mutex
	children []*Agent
}

func (r *ChildRegistry) Attach(child *Agent) (detach func()) {
	r.mu.Lock()
	r.children = append(r.children, child)
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if i := slices.Index(r.children, child); i >= 0 {
			r.children = slices.Delete(r.children, i, i+1)
		}
		r.mu.Unlock()
	}
}

func (r *ChildRegistry) Active() []*Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.children)
}

func (r *ChildRegistry) detachAll() []*Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	children := r.children
	r.children = nil
	return children
}
