package engine

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	contextsconfig "github.com/nickheyer/icebath/internal/contexts/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Aux task the summaries run as, and its setting of the context window of its model, when what icebath knows of it is wrong
const (
	Task      = "compression"
	AuxWindow = "context_length"
)

// Summaries prepared in the background at once, over every session
const preparedPasses = 2

type kinds struct {
	slots chan struct{}
}

func Register(registry *contexts.Registry) {
	registry.Register(kinds{slots: make(chan struct{}, preparedPasses)})
}

func (k kinds) Kinds() []contexts.Kind {
	return []contexts.Kind{{Name: contextsconfig.Compressor, Title: "Compressor",
		About: "Built in: prunes old tool output, keeps the start and the latest messages, and summarises the middle on a helper model",
		New:   func() contexts.Engine { return &compressor{slots: k.slots} }}}
}

// Settings the compressor follows, as the session's profile sets them
type settings struct {
	enabled         bool
	threshold       float64
	thresholdTokens int
	modelThresholds map[string]float64
	targetRatio     float64
	protectFirst    int
	protectLast     int
	minTailUsers    int
	tailMode        string
	abortOnFailure  bool
	prepareAhead    bool
	pruneTokens     int
	pruneMinChars   int
	pruneMinReclaim int
	micro           bool
	microEvery      int
	microDefrag     int
	autoraise       bool
	checkpoint      bool
}

func read(cfg *config.Config) settings {
	s := settings{
		enabled:         cfg.GetBool(contextsconfig.Enabled),
		threshold:       cast.ToFloat64(cfg.Get(contextsconfig.Threshold)),
		thresholdTokens: max(cfg.GetInt(contextsconfig.ThresholdTokens), 0),
		modelThresholds: map[string]float64{},
		targetRatio:     max(0.10, min(cast.ToFloat64(cfg.Get(contextsconfig.TargetRatio)), 0.80)),
		protectFirst:    max(cfg.GetInt(contextsconfig.ProtectFirstN), 0),
		protectLast:     max(cfg.GetInt(contextsconfig.ProtectLastN), 0),
		minTailUsers:    max(cfg.GetInt(contextsconfig.MinTailUsers), 1),
		tailMode:        cfg.GetString(contextsconfig.TailMode),
		abortOnFailure:  cfg.GetBool(contextsconfig.AbortOnFailure),
		prepareAhead:    cfg.GetBool(contextsconfig.PrepareAhead),
		pruneTokens:     max(cfg.GetInt(contextsconfig.PruneTokens), 0),
		pruneMinChars:   max(pruneMinChars, cfg.GetInt(contextsconfig.PruneMinChars)),
		pruneMinReclaim: max(cfg.GetInt(contextsconfig.PruneMinReclaim), 0),
		micro:           cfg.GetBool(contextsconfig.MicroCompact),
		microEvery:      max(cfg.GetInt(contextsconfig.MicroEveryNTurns), 1),
		microDefrag:     max(cfg.GetInt(contextsconfig.MicroDefragTokens), 1),
		autoraise:       cfg.GetBool(contextsconfig.CodexAutoraise),
		checkpoint:      cfg.GetBool(contextsconfig.CheckpointRequired),
	}
	if s.tailMode != contextsconfig.TailLegacy {
		s.tailMode = contextsconfig.TailLean
	}
	for model, value := range cast.ToStringMap(cfg.Get(contextsconfig.ModelThresholds)) {
		switch n := value.(type) {
		case int:
			s.modelThresholds[model] = float64(n)
		case int64:
			s.modelThresholds[model] = float64(n)
		case float64:
			s.modelThresholds[model] = n
		}
	}
	if s.checkpoint && s.micro {
		slog.Warn("compression.checkpoint_required is enabled: post-turn micro-compaction is disabled for this agent so every lossy rewrite passes through the checkpoint-gated compressor.")
		s.micro = false
	}
	return s
}

// Built in context engine: prunes old tool results, keeps the head and the tail and summarises the middle
type compressor struct {
	contexts.Base
	mu sync.Mutex

	s       settings
	store   sessions.Store
	session string
	route   provider.Route
	bound   bool
	window  int
	reserve int

	// Trigger share after the per model table, then after the floor of small windows, and the trigger and budgets in tokens
	basePercent float64
	percent     float64
	threshold   int
	tailBudget  int
	maxSummary  int
	// Window of the summarising model when it is below the trigger, which then holds the trigger down
	auxCeiling int

	// Prompt and output of the newest call as the provider counted them, -1 for the prompt right after a compaction
	lastPrompt     int
	lastCompletion int
	lastReal       int
	// Rough size the transcript had right after the last compaction, and whether the provider's count of it is still awaited
	roughAfter   int
	awaitingReal bool
	omitsUsage   bool
	// The next real count is the first after a compaction, which tells whether it cleared the trigger
	verify       bool
	compressions int
	savings      float64

	// Body of the summary the next one updates, and whether the summarised region held a turn of the person
	previous string
	hasUser  *bool
	// The configured summarising model failed, so summaries run on the main model until one succeeds
	fallenBack bool
	lastError  string
	// Summarising model that failed while the main model stood in, and how
	auxFailedModel string
	auxFailure     string
	lastAuxModel   string

	timeoutFailures    int
	truncationFailures int
	overloadAborts     int
	overloadDegraded   bool
	// Failures of the summary that abort a compression whatever the settings say
	terminal map[string]bool

	cooldownUntil    time.Time
	persistFailed    bool
	ineffective      int
	recoveryDeadline time.Time
	backoffUntil     time.Time
	backoffLen       int
	prellmSkips      int
	fallbackStreak   int
	probeAt          time.Time

	// Outcome of the last attempt
	aborted         bool
	refusedGrow     bool
	feasibilitySkip bool
	fallbackUsed    bool
	dropped         int
	madeProgress    bool
	aggressive      bool

	pruneRearm int
	// Early trim runway before the attempt, put back when its result is not stored
	rearmBefore int
	reclaimed   string
	// Whether the summarising model was checked to take what the trigger lets through, and the last thing told of it
	feasible bool
	told     string
	// Trigger share the setting gave and the one a capped Codex model raised it to, zero when none was raised
	autoraised [2]float64

	env *contexts.Env
	// Slots of the summaries prepared in the background, shared by every session
	slots    chan struct{}
	micro    microState
	prepared *preparedState
	record   contexts.Record
	notify   func(string)
}

func (c *compressor) Bind(env *contexts.Env) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := read(env.Config)
	changed := c.bound && routeKey(env.Route) != routeKey(c.route)
	sessionChanged := env.Session != c.session || env.Store != c.store
	c.env, c.notify = env, env.Notify
	if changed {
		c.auxCeiling, c.feasible = 0, false
	}
	if !c.bound || changed || env.Window != c.window || env.Reserve != c.reserve || !sameSettings(s, c.s) {
		c.s, c.route, c.window, c.reserve = s, env.Route, env.Window, env.Reserve
		c.rebudget()
	}
	if changed {
		c.lastPrompt, c.lastCompletion = 0, 0
		c.resetUsagePairing()
		c.ineffective, c.prellmSkips = 0, 0
		c.fallbackStreak = 0
		c.clearCooldown()
		c.overloadAborts = 0
		c.verify, c.madeProgress = false, false
		c.pruneRearm, c.reclaimed = 0, ""
		c.persist(context.Background())
		c.setModelConfig(context.Background(), pruneRearmKey, nil)
	}
	c.bound = true
	if sessionChanged {
		c.store, c.session = env.Store, env.Session
		c.load(context.Background())
	}
	if c.s.prepareAhead && c.prepared == nil {
		c.prepared = &preparedState{}
	}
	if !c.s.prepareAhead && c.prepared != nil {
		c.prepared.discard("prepare ahead turned off")
		c.prepared = nil
	}
}

func sameSettings(a, b settings) bool {
	if len(a.modelThresholds) != len(b.modelThresholds) {
		return false
	}
	for k, v := range a.modelThresholds {
		if b.modelThresholds[k] != v {
			return false
		}
	}
	a.modelThresholds, b.modelThresholds = nil, nil
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func routeKey(route provider.Route) string {
	return strings.Join([]string{route.Provider, route.Model, strings.TrimRight(route.BaseURL, "/"), string(route.Mode)}, "\x00")
}

// Derives the trigger and the budgets from the window, the model's share and the caps
func (c *compressor) rebudget() {
	c.autoraised = [2]float64{}
	c.basePercent = c.modelPercent()
	c.percent = effectivePercent(c.window, c.basePercent)
	c.threshold = thresholdTokens(c.window, c.percent, c.reserve)
	if limit := c.cap(); limit > 0 && limit < c.threshold {
		c.threshold = limit
	}
	if c.auxCeiling > 0 && c.auxCeiling < c.threshold {
		c.threshold = c.auxCeiling
	}
	c.tailBudget = c.computeTailBudget()
	c.maxSummary = min(int(float64(c.window)*0.05), summaryTokensCeiling)
	c.logInit()
}

// Configured cap of the trigger held to the window, 0 for none
func (c *compressor) cap() int {
	if c.s.thresholdTokens <= 0 {
		return 0
	}
	return min(c.s.thresholdTokens, c.window)
}

// Share of the window the session's model triggers at: the per model table, else the model's own when it has one, else the setting
func (c *compressor) modelPercent() float64 {
	percent := c.s.threshold
	if own, ok := modelThreshold(c.route, c.s.autoraise); ok {
		raising := codexAutoraised(c.route) || codexSpark(c.route)
		if !raising || own > percent+1e-9 {
			percent = own
		}
		if raising && own > c.s.threshold+1e-9 {
			c.autoraised = [2]float64{c.s.threshold, own}
		}
	}
	return resolveModelThreshold(c.route.Model, c.s.modelThresholds, percent, c.route.Provider)
}

func (c *compressor) logInit() {
	if c.bound {
		return
	}
	slog.Info(fmt.Sprintf("Context compressor initialized: model=%s context_length=%d threshold=%d (%.0f%%) target_ratio=%.0f%% tail_budget=%d provider=%s base_url=%s",
		c.route.Model, c.window, c.threshold, c.percent*100, c.s.targetRatio*100, c.tailBudget, orNone(c.route.Provider), orNone(c.route.BaseURL)))
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Protected tail budget: a clamped window of the recency in lean mode, a share of the trigger otherwise
func (c *compressor) computeTailBudget() int {
	budget := int(float64(c.threshold) * c.s.targetRatio)
	if c.s.tailMode == contextsconfig.TailLean {
		budget = leanTailBudget(c.window, c.threshold)
	}
	if c.window > 0 {
		budget = min(budget, int(float64(c.window)*tailMaxContextFraction))
	}
	return max(1, budget)
}

func (c *compressor) Observe(usage *provider.Usage) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if usage == nil {
		if c.awaitingReal {
			c.lastPrompt, c.lastCompletion = 0, 0
			c.verify, c.awaitingReal = false, false
		}
		c.omitsUsage = true
		return false
	}
	pending := c.verify
	c.lastPrompt, c.lastCompletion = int(usage.Prompt()), int(usage.Output)
	c.realVerdict()
	c.verify, c.awaitingReal = false, false
	return pending && 0 < c.lastPrompt && c.lastPrompt < c.threshold
}

// Pairs the provider's count with the verdict of the last compaction: one that left the request over the trigger strikes the guard
// against compressions that can't help, one under it clears the strikes
func (c *compressor) realVerdict() {
	if c.lastPrompt <= 0 {
		return
	}
	c.lastReal, c.omitsUsage = c.lastPrompt, false
	if c.lastPrompt < c.threshold {
		c.setIneffective(0)
	}
	if !c.verify {
		return
	}
	if c.lastPrompt >= c.threshold {
		c.setIneffective(c.ineffective + 1)
		slog.Warn(fmt.Sprintf("Compaction did not clear the threshold: %d real tokens still >= %d. The incompressible prompt (system prompt + tool schemas) may "+
			"already exceed it, in which case shrinking messages cannot help. ineffective_compression_count=%d", c.lastPrompt, c.threshold, c.ineffective))
		return
	}
	c.setIneffective(0)
}

func (c *compressor) resetUsagePairing() {
	c.lastReal, c.roughAfter = 0, 0
	c.awaitingReal, c.omitsUsage = false, false
}

func (c *compressor) Defer(tokens int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if tokens < c.threshold {
		return false
	}
	if c.awaitingReal {
		return true
	}
	if c.lastReal >= c.threshold {
		return false
	}
	return !c.omitsUsage
}

func (c *compressor) Due(tokens int) (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if tokens >= minimumWindow {
		c.feasibility(context.Background(), c.env)
	}
	if tokens < c.threshold {
		return false, ""
	}
	if c.blocked(context.Background(), false) {
		return false, c.blockReason(false)
	}
	return true, ""
}

func (c *compressor) Compressible(messages []*sessionsv1.Message) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	start := alignForward(messages, c.headSize(messages))
	return start < c.tailCut(messages, start, c.tailBudget, true)
}

func (c *compressor) Announce(_, text string) string {
	return text
}

func (c *compressor) Compressed(previous, next string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if previous == next {
		return
	}
	streak, ineffective, overload, probe := c.fallbackStreak, c.ineffective, c.overloadAborts, c.probeAt
	c.session = next
	c.load(context.Background())
	c.fallbackStreak, c.ineffective, c.overloadAborts = streak, ineffective, overload
	c.persist(context.Background())
	c.setProbeAt(context.Background(), probe)
}

func (c *compressor) Turn(messages []*sessionsv1.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.liftBackoff(len(messages))
}

// Wait after a compression could not be stored, the first rung of the timeout ladder
const splitFailureCooldown = 60 * time.Second

func (c *compressor) Settled(outcome contexts.Outcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx := context.Background()
	switch {
	case outcome.Unchanged:
		c.structuralNoOp("compaction returned the transcript unchanged (no_progress)", 0)
	case outcome.Refused:
		c.refusedGrow = true
		c.rejected()
		c.pruneRearm = c.rearmBefore
		c.setModelConfig(ctx, pruneRearmKey, c.pruneRearm)
	case outcome.Failure != "":
		c.madeProgress = false
		c.pruneRearm = c.rearmBefore
		c.setModelConfig(ctx, pruneRearmKey, c.pruneRearm)
		c.recordCooldown(splitFailureCooldown, "session_split_failed: "+outcome.Failure)
	case outcome.Stored:
		c.roughAfter = outcome.Tokens
		c.lastPrompt, c.lastCompletion = -1, 0
		c.awaitingReal = true
		c.setModelConfig(ctx, pruneRearmKey, nil)
		if c.madeProgress {
			c.completed(c.fallbackUsed, c.feasibilitySkip)
		}
	}
}

func (c *compressor) TimedOut(failure, kind string, idle time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordTimeout(failure, kind, idle)
}

func (c *compressor) Start(env *contexts.Env) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notify = env.Notify
}

func (c *compressor) End([]*sessionsv1.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetSession()
}

// Forgets what belongs to the session's conversation: the summary it builds on, the failures, streaks and the prepared summary
func (c *compressor) resetSession() {
	if c.prepared != nil {
		c.prepared.discard("session boundary")
	}
	c.previous, c.hasUser, c.lastError = "", nil, ""
	c.auxFailedModel, c.auxFailure, c.lastAuxModel = "", "", ""
	c.timeoutFailures, c.truncationFailures, c.overloadAborts = 0, 0, 0
	c.overloadDegraded = false
	c.terminal = nil
	c.dropped, c.fallbackUsed, c.feasibilitySkip = 0, false, false
	c.savings = 100
	c.ineffective, c.prellmSkips, c.fallbackStreak = 0, 0, 0
	c.recoveryDeadline, c.backoffUntil, c.probeAt, c.cooldownUntil = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	c.backoffLen = 0
	c.verify, c.madeProgress, c.persistFailed = false, false, false
	c.aborted, c.refusedGrow = false, false
	c.resetUsagePairing()
	c.pruneRearm, c.reclaimed = 0, ""
}

func (c *compressor) Status() contexts.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := contexts.Status{Prompt: max(c.lastPrompt, 0), Window: c.window, Threshold: c.threshold, Percent: c.percent, Compressions: c.compressions,
		Savings: c.savings, Measured: c.lastPrompt > 0, Blocked: c.blockReasonQuiet(), Raised: c.autoraised, Awaiting: c.awaitingReal, After: c.roughAfter,
		Target: c.s.targetRatio}
	if c.s.micro && c.micro.passes > 0 {
		status.Lines = append(status.Lines, fmt.Sprintf("Micro-compaction: %d passes, ~%s tokens saved", c.micro.passes, strs.Thousands(c.micro.saved)))
	}
	if c.fallbackStreak >= 2 {
		status.Lines = append(status.Lines, fmt.Sprintf("Summary model benched after %d fallback summaries in a row", c.fallbackStreak))
	}
	return status
}

// Bookkeeping of the compressor an attempt changes, kept to put back when the attempt is given up before it is stored
type kept struct {
	previous                          string
	hasUser                           *bool
	compressions                      int
	savings                           float64
	ineffective, fallbackStreak       int
	recoveryDeadline, cooldownUntil   time.Time
	verify, madeProgress              bool
	persistFailed                     bool
	lastError                         string
	timeoutFailures, truncationFailed int
	dropped                           int
	fallbackUsed, aborted             bool
	terminal                          map[string]bool
	overloadAborts                    int
	overloadDegraded                  bool
	auxFailedModel, auxFailure        string
	lastAuxModel                      string
	fallenBack                        bool
	pruneRearm                        int
	micro                             microState
}

// Puts the attempt's bookkeeping back as it was before, for a compression given up before it was stored
func (c *compressor) Snapshot() func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	saved := kept{previous: c.previous, hasUser: c.hasUser, compressions: c.compressions, savings: c.savings, ineffective: c.ineffective,
		fallbackStreak: c.fallbackStreak, recoveryDeadline: c.recoveryDeadline, cooldownUntil: c.cooldownUntil, verify: c.verify,
		madeProgress: c.madeProgress, persistFailed: c.persistFailed, lastError: c.lastError, timeoutFailures: c.timeoutFailures,
		truncationFailed: c.truncationFailures, dropped: c.dropped, fallbackUsed: c.fallbackUsed, aborted: c.aborted, terminal: maps.Clone(c.terminal),
		overloadAborts: c.overloadAborts, overloadDegraded: c.overloadDegraded, auxFailedModel: c.auxFailedModel, auxFailure: c.auxFailure,
		lastAuxModel: c.lastAuxModel, fallenBack: c.fallenBack, pruneRearm: c.pruneRearm, micro: c.micro}
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.previous, c.hasUser, c.compressions, c.savings = saved.previous, saved.hasUser, saved.compressions, saved.savings
		c.ineffective, c.recoveryDeadline, c.fallbackStreak = saved.ineffective, saved.recoveryDeadline, saved.fallbackStreak
		c.verify, c.madeProgress, c.cooldownUntil, c.persistFailed = saved.verify, saved.madeProgress, saved.cooldownUntil, saved.persistFailed
		c.lastError, c.timeoutFailures, c.truncationFailures = saved.lastError, saved.timeoutFailures, saved.truncationFailed
		c.dropped, c.fallbackUsed, c.aborted, c.terminal = saved.dropped, saved.fallbackUsed, saved.aborted, saved.terminal
		c.overloadAborts, c.overloadDegraded = saved.overloadAborts, saved.overloadDegraded
		c.auxFailedModel, c.auxFailure, c.lastAuxModel, c.fallenBack = saved.auxFailedModel, saved.auxFailure, saved.lastAuxModel, saved.fallenBack
		c.pruneRearm, c.micro = saved.pruneRearm, saved.micro
		c.setModelConfig(context.Background(), pruneRearmKey, c.pruneRearm)
		c.persistCooldown(context.Background())
		c.persist(context.Background())
	}
}
