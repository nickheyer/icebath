package tool

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	maxToolWorkers           = 8
	defaultImageParallel     = 4
	defaultConcurrentTimeout = 420.0
	startOrderGateTimeout    = 120 * time.Second
	activityHeartbeat        = 30 * time.Second
	concurrentPoll           = 5 * time.Second
	sequentialPoll           = time.Second
	interruptGrace           = 3 * time.Second
	invalidArgsText          = `{"error": "Invalid tool arguments", "message": "Tool arguments must be a valid JSON object; tool was not executed."}`
	invalidArgsMessage       = "Tool arguments must be a valid JSON object"
	prunedArgsError          = "suspected_pruned_tool_arguments"
)

var prunedArgsMessage = "Tool was not executed because effect-capable arguments contain an icebath context-compression artifact. Recover the exact content from its durable source or re-read it, then issue a complete new call; do not retry these arguments. To remove a marker that already landed in a file, match it by its " + strings.Trim(CompressionMarkerPrefix, "⟪:") + " prefix (e.g. a terminal sed on that line) instead of quoting the full marker."

type Relay interface {
	Execute(ctx context.Context, c *Call, args Args, next func(ctx context.Context, args Args) Result) (Result, Args, error)
}

type ExecutorOptions struct {
	Config    *config.Store
	HumanWait HumanWait
	Relay     Relay
}

type Executor struct {
	d *Dispatcher
	o ExecutorOptions
}

func NewExecutor(d *Dispatcher, o ExecutorOptions) *Executor {
	return &Executor{d: d, o: o}
}

type Batch struct {
	Calls        []wire.ToolCall
	Template     Call
	Guard        *Guardrails
	Budget       Budget
	Spill        *Spill
	Cwd          string
	Interrupt    *Interrupt
	PrepareAhead bool
}

type BatchSink interface {
	Started(ctx context.Context, index int, c *Call, args Args)
	Review(ctx context.Context, index int, o Outcome) Result
	Commit(ctx context.Context, index int, o Outcome) error
}

type Outcome struct {
	Call      *Call
	Args      Args
	Result    Result
	Display   Result
	Duration  time.Duration
	Status    string
	ErrorType string
	Failed    bool
	Suffix    string
	Blocked   bool
	Skipped   bool
	Observed  bool
	Untrusted bool
	Guard     GuardDecision
	StallKind string
	Halt      *GuardDecision
}

type parsed struct {
	index    int
	tc       wire.ToolCall
	name     string
	args     Args
	parseErr string
	scope    string
	bridged  bool
	entry    *Entry
}

type run struct {
	e      *Executor
	b      Batch
	sink   BatchSink
	at     Scope
	conc   time.Duration
	concOn bool
	seq    time.Duration
	seqOn  bool
	err    error
}

func (e *Executor) Run(ctx context.Context, b Batch, sink BatchSink) error {
	switch {
	case e.d == nil:
		return &UnfilledError{Name: "tool.Dispatcher"}
	case e.o.Config == nil:
		return &UnfilledError{Name: "tool.ExecutorOptions.Config"}
	case e.o.HumanWait == nil:
		return &UnfilledError{Name: "tool.HumanWait"}
	case e.o.Relay == nil:
		return &UnfilledError{Name: "tool.Relay"}
	case sink == nil:
		return &UnfilledError{Name: "tool.BatchSink"}
	case b.Guard == nil:
		return &UnfilledError{Name: "tool.Batch.Guard"}
	case b.Spill == nil:
		return &UnfilledError{Name: "tool.Batch.Spill"}
	case b.Template.Activity == nil:
		return &UnfilledError{Name: "tool.Call.Activity"}
	case b.Template.Env == nil:
		return &UnfilledError{Name: "tool.EnvSource"}
	case b.PrepareAhead && b.Interrupt == nil:
		return &UnfilledError{Name: "tool.Batch.Interrupt"}
	}
	r := &run{e: e, b: b, sink: sink, at: b.Template.Scope}
	r.conc, r.concOn = e.o.Config.ResolveTimeout(ctx, "tools.concurrent_batch", defaultConcurrentTimeout, "ICEBATH_CONCURRENT_TOOL_TIMEOUT_S")
	seqDefault := 0.0
	if r.concOn {
		seqDefault = r.conc.Seconds()
	}
	r.seq, r.seqOn = e.o.Config.ResolveTimeout(ctx, "tools.sequential_call", seqDefault, "")
	all := make([]int, len(b.Calls))
	for i := range all {
		all[i] = i
	}
	switch len(all) {
	case 0:
		return nil
	case 1:
		r.sequential(ctx, all)
		return r.err
	}
	var deadline time.Duration
	if r.concOn {
		deadline = r.conc
	}
	segments, err := Plan(ctx, b.Calls, e.d.r, r.at, b.Cwd, deadline)
	if err != nil {
		return err
	}
	for _, s := range segments {
		if r.err != nil {
			break
		}
		if s.Parallel {
			r.parallel(ctx, s.Calls)
		} else {
			r.sequential(ctx, s.Calls)
		}
	}
	return r.err
}

func skipWording(ctx context.Context) string {
	reason, ok := InterruptReason(ctx)
	if !ok {
		return "Turn interrupted"
	}
	switch reason {
	case ReasonUserInterrupt, ReasonNewMessage, ReasonHardStop:
		return strings.ToUpper(reason[:1]) + reason[1:]
	}
	return "Turn aborted — " + strings.ReplaceAll(reason, "_", " ")
}

func secondsText(d time.Duration, prec int) string {
	return strconv.FormatFloat(d.Seconds(), 'f', prec, 64)
}

func (r *run) parse(ctx context.Context, i int, flatten bool) *parsed {
	tc := r.b.Calls[i]
	p := &parsed{index: i, tc: tc, name: legacyToolAlias(tc.Name)}
	args, err := ParseArgs(tc.Arguments)
	if err != nil {
		p.parseErr = invalidArgsText
		p.args = Args{}
		return p
	}
	p.args = args
	if p.name == callToolName {
		c := r.callFor(p)
		u, err := r.e.d.Unwrap(ctx, c, args)
		switch {
		case err != nil:
			slog.Debug(fmt.Sprintf("tool_call unwrap failed for %s: %v", CanonicalCallID(tc.ID), err))
		case u.Probe != nil:
			p.scope = u.Probe.Text
			if flatten {
				p.scope = flattenProbe(u.Probe.Text)
			}
		case u.ScopeBlock != "":
			p.scope = u.ScopeBlock
		default:
			p.name, p.args, p.bridged = u.Name, u.Args, u.Name != callToolName
		}
	}
	if e, ok := r.e.d.r.Lookup(p.name, r.at); ok {
		p.entry = e
	}
	return p
}

func flattenProbe(text string) string {
	n, err := parseNode([]byte(text))
	if err != nil || !n.isObj() {
		return text
	}
	field := func(key string) string {
		if s, ok := n.get(key).text(); ok {
			return s
		}
		if v := n.get(key); v != nil {
			return v.spaced()
		}
		return ""
	}
	params := n.get("parameters")
	if params == nil {
		params = newObj()
	}
	return trimWide(field("error") + " Parameters schema: " + params.spaced() + ". " + field("hint"))
}

func (r *run) callFor(p *parsed) *Call {
	c := r.b.Template
	c.ID = CanonicalCallID(p.tc.ID)
	c.Name = p.name
	c.Bridged = p.bridged
	c.Arguments = ""
	if !p.bridged && p.name != callToolName {
		c.Arguments = p.tc.Arguments
	}
	c.Trace = nil
	return &c
}

func effectsOf(e *Entry) Effects {
	if e == nil {
		return MayHaveEffect
	}
	return e.Tool.Effects
}

func (r *run) emitPost(ctx context.Context, c *Call, args Args, res Result, d time.Duration, status, errType, errText string) {
	r.e.d.PostToolCall(context.WithoutCancel(ctx), Event{Call: c, Args: args, Result: res, Duration: d, Status: status, ErrorType: errType, ErrorText: errText})
}

func (r *run) activeEnv(ctx context.Context) (Environment, error) {
	env, ok, err := r.b.Template.Env.Active(ctx, r.b.Template.TaskID)
	if err != nil || !ok {
		return nil, err
	}
	return env, nil
}

type managed struct {
	mu         sync.Mutex
	result     Result
	args       Args
	trace      []Trace
	blocked    bool
	dispatched bool
	abandoned  bool
	halt       *GuardDecision
	err        error
	kind       string
	blockType  string
}

type pipeOpts struct {
	index     int
	call      *Call
	entry     *Entry
	scope     string
	startGate *startOnce
	authGate  *authGate
	dispatch  func(ctx context.Context, c *Call, args Args) Result
}

func (r *run) concurrentDispatch(ctx context.Context, c *Call, args Args) Result {
	return r.e.d.Dispatch(ctx, c, args, DispatchFlags{SkipPreHook: true, SkipRequestMiddleware: true, SkipExecMiddleware: true})
}

func (r *run) sequentialDispatch(ctx context.Context, c *Call, args Args) Result {
	return r.e.d.Dispatch(ctx, c, args, DispatchFlags{SkipPreHook: true, SkipRequestMiddleware: true, SkipExecMiddleware: true, SkipPostHook: true})
}

func (r *run) pipeline(ctx context.Context, args Args, o pipeOpts) *managed {
	m := &managed{args: args}
	c := o.call
	original := args.Clone()
	authorized := func(ctx context.Context, final Args) Result {
		m.mu.Lock()
		if m.dispatched {
			m.mu.Unlock()
			return Result{Text: fmt.Sprintf("Error executing tool '%s': icebath tool execution callback invoked more than once", c.Name)}
		}
		m.dispatched, m.blocked, m.args = true, false, final
		m.mu.Unlock()
		return r.authorizedOnce(ctx, m, o, final)
	}
	pipelineFn := func(ctx context.Context, relayArgs Args) Result {
		hooks := r.e.d.hooks
		if hooks == nil {
			return authorized(ctx, relayArgs)
		}
		requestArgs := relayArgs
		payload, _, trace, err := hooks.RequestMiddleware(ctx, c, relayArgs)
		if err != nil {
			slog.Debug(fmt.Sprintf("tool_request middleware error: %v", err))
		} else {
			if payload != nil {
				requestArgs = payload
			}
			m.mu.Lock()
			m.trace = slices.Clone(trace)
			m.mu.Unlock()
			c.Trace = slices.Clone(trace)
		}
		return hooks.ExecutionMiddleware(ctx, c, requestArgs, original, func(next Args) Result {
			if next == nil {
				next = requestArgs
			}
			return authorized(ctx, next)
		})
	}
	res, _, err := r.e.o.Relay.Execute(ctx, c, args, pipelineFn)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil && m.err == nil {
		m.err = err
	}
	m.result = res
	return m
}

func (r *run) authorizedOnce(ctx context.Context, m *managed, o pipeOpts, args Args) Result {
	c := o.call
	advance := func(cb func()) bool {
		if o.startGate != nil {
			return o.startGate.advance(cb)
		}
		if cb != nil {
			cb()
		}
		return true
	}
	blockMsg, blockType := o.scope, "tool_scope_block"
	if blockMsg == "" {
		blockType = "plugin_block"
		resolve := func() {
			hooks := r.e.d.hooks
			if hooks == nil || !hooks.Has("pre_tool_call") {
				return
			}
			block, modified, err := hooks.PreToolCall(ctx, c, args)
			if err != nil {
				slog.Debug(fmt.Sprintf("pre_tool_call hook error: %v", err))
				return
			}
			if modified != nil {
				args = modified
			}
			blockMsg = block
		}
		if o.authGate != nil {
			o.authGate.run(resolve)
		} else {
			resolve()
		}
		m.mu.Lock()
		m.args = args
		m.mu.Unlock()
	}
	var body *jnode
	errText := blockMsg
	if blockMsg != "" {
		body = newObj().with("error", newStr(blockMsg))
	}
	effects := effectsOf(o.entry)
	if body == nil {
		if paths := PrunedArgumentPaths(effects, args); len(paths) > 0 {
			body = newObj().with("error", newStr(prunedArgsError)).with("message", newStr(prunedArgsMessage)).with("argument_paths", nodeOf(paths))
			blockType, errText = prunedArgsError, prunedArgsMessage
		}
	}
	var guard *GuardDecision
	if body == nil {
		if d := r.b.Guard.Before(c.Name, effects, args); !d.Allows() {
			guard = &d
		}
	}
	if body != nil || guard != nil {
		if !advance(nil) {
			m.mu.Lock()
			m.abandoned = true
			m.mu.Unlock()
			return Result{}
		}
		var res Result
		if body != nil {
			res = Result{Text: body.spaced()}
		} else {
			res = Result{Text: r.b.Guard.blockResult(*guard)}
			blockType, errText = "guardrail_block", guard.Message
			if errText == "" {
				errText = "Tool blocked by guardrail policy"
			}
		}
		m.mu.Lock()
		m.blocked, m.blockType, m.halt = true, blockType, guard
		m.mu.Unlock()
		r.emitPost(ctx, c, args, res, 0, "blocked", blockType, errText)
		return res
	}
	if o.entry != nil && o.entry.Tool.Prepare != nil {
		if err := r.prepare(ctx, o, args); err != nil {
			m.mu.Lock()
			m.err = err
			m.mu.Unlock()
			return Result{}
		}
	}
	if !advance(func() { r.sink.Started(ctx, o.index, c, args) }) {
		m.mu.Lock()
		m.abandoned = true
		m.mu.Unlock()
		return Result{}
	}
	stop := r.heartbeat(ctx, c)
	res := o.dispatch(ctx, c, args)
	stop()
	return res
}

func (r *run) heartbeat(ctx context.Context, c *Call) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(activityHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				c.Activity("tool running: " + c.Name)
			}
		}
	}()
	return func() {
		close(done)
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
		}
	}
}

type commitSlot struct {
	call       *Call
	args       Args
	result     Result
	duration   time.Duration
	failed     bool
	suffix     string
	blocked    bool
	observed   bool
	effect     Effect
	status     string
	errType    string
	entry      *Entry
	halt       *GuardDecision
	preview    func(Result) string
	successLog bool
}

func (r *run) commit(ctx context.Context, index int, s commitSlot) bool {
	ctx = context.WithoutCancel(ctx)
	c := s.call
	status, errType := s.status, s.errType
	if status == "" {
		status = "ok"
		if s.failed {
			status, errType = "error", "tool_error"
		}
	}
	o := Outcome{
		Call: c, Args: s.args, Result: s.result, Duration: s.duration, Status: status, ErrorType: errType,
		Failed: s.failed, Suffix: s.suffix, Blocked: s.blocked, Observed: s.observed, Halt: s.halt,
		Untrusted: s.entry != nil && s.entry.Tool.Untrusted,
	}
	res := r.sink.Review(ctx, index, o)
	if s.observed {
		if !s.blocked {
			g := r.b.Guard.observe(c.Name, effectsOf(s.entry), s.args, res, s.failed, c.ID)
			res, o.Guard, o.StallKind = g.result, g.decision, g.stallKind
			if g.halt != nil {
				o.Halt = g.halt
			}
		}
		switch {
		case s.failed:
			slog.Warn(fmt.Sprintf("Tool %s returned error (%ss): %s", c.Name, secondsText(s.duration, 2), s.preview(res)))
		case s.successLog:
			slog.Info(fmt.Sprintf("tool %s completed (%ss, %d chars)", c.Name, secondsText(s.duration, 2), len([]rune(res.Plain()))))
		}
	}
	suffix := ""
	if s.failed {
		suffix = " (error)"
	}
	c.Activity(fmt.Sprintf("tool completed: %s (%ss)%s", c.Name, secondsText(s.duration, 1), suffix))
	display := res
	threshold := r.b.Budget.Threshold(r.e.d.r, r.at, c.Name)
	env, err := r.activeEnv(ctx)
	if err != nil {
		if r.err == nil {
			r.err = err
		}
		return false
	}
	persisted := r.b.Spill.Persist(ctx, res, c.Name, c.ID, threshold, env, r.b.Budget)
	candidates := []string{persisted.Text}
	if persisted.Parts != nil {
		candidates = []string{persisted.Summary}
		for _, p := range persisted.Parts {
			if p.Kind == wire.PartText {
				candidates = append(candidates, p.Text)
			}
		}
	}
	for _, text := range candidates {
		if path, ok := PersistedPath(text); ok {
			r.b.Guard.RecordPersisted(c.ID, path)
			break
		}
	}
	if s.effect != EffectUnset {
		persisted.Effect = s.effect
	}
	persisted.DataUse = slices.Compact(slices.Sorted(slices.Values(persisted.DataUse)))
	o.Result, o.Display = persisted, display
	if err := r.sink.Commit(ctx, index, o); err != nil {
		if r.err == nil {
			r.err = err
		}
		return false
	}
	return true
}

func (r *run) commitPlain(ctx context.Context, index int, o Outcome) bool {
	if err := r.sink.Commit(context.WithoutCancel(ctx), index, o); err != nil {
		if r.err == nil {
			r.err = err
		}
		return false
	}
	return true
}

func (r *run) skipCalls(ctx context.Context, idx []int, content, hookType string, rawHookID, stopOnFailure bool) bool {
	why := skipWording(ctx)
	ok := true
	for _, i := range idx {
		tc := r.b.Calls[i]
		name := tc.Name
		if name == "" {
			name = "tool"
		}
		text := strings.ReplaceAll(content, "{name}", name)
		c := r.b.Template
		c.ID, c.Name, c.Trace = CanonicalCallID(tc.ID), name, nil
		res := Result{Text: text, Effect: EffectNone}
		if hookType != "" {
			hc := c
			if rawHookID {
				hc.ID = tc.ID
			}
			r.emitPost(ctx, &hc, Args{}, Result{Text: text}, 0, "cancelled", hookType, "Tool execution skipped. "+why)
		}
		if !r.commitPlain(ctx, i, Outcome{Call: &c, Args: Args{}, Result: res, Display: res, Status: "cancelled", ErrorType: hookType, Skipped: true}) {
			ok = false
			if stopOnFailure {
				return false
			}
		}
	}
	return ok
}

type startGate struct {
	mu        sync.Mutex
	next      int
	abandoned bool
	changed   chan struct{}
	timeout   time.Duration
}

func newStartGate(timeout time.Duration) *startGate {
	return &startGate{changed: make(chan struct{}), timeout: timeout}
}

func (g *startGate) notifyLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

func (g *startGate) abandon() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.abandoned = true
	g.notifyLocked()
}

func (g *startGate) begin(order int, cb func(), name string) bool {
	deadline := time.Now().Add(g.timeout)
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.next < order && !g.abandoned {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		ch := g.changed
		g.mu.Unlock()
		t := time.NewTimer(left)
		select {
		case <-ch:
		case <-t.C:
		}
		t.Stop()
		g.mu.Lock()
	}
	if g.abandoned {
		return false
	}
	if g.next < order {
		if name == "" {
			name = "tool"
		}
		slog.Warn(fmt.Sprintf("start-order gate timed out for %s (order=%d next=%d); proceeding out of order", name, order, g.next))
	}
	defer func() {
		g.next = max(g.next, order+1)
		g.notifyLocked()
	}()
	if cb != nil {
		cb()
	}
	return true
}

type startOnce struct {
	mu       sync.Mutex
	gate     *startGate
	order    int
	name     string
	advanced bool
}

func (s *startOnce) advance(cb func()) bool {
	s.mu.Lock()
	if s.advanced {
		s.mu.Unlock()
		return true
	}
	s.advanced = true
	s.mu.Unlock()
	return s.gate.begin(s.order, cb, s.name)
}

type authGate struct {
	sem      chan struct{}
	timeout  time.Duration
	hw       HumanWait
	key      string
	baseline float64
}

func (r *run) newAuthGate() *authGate {
	hw := r.e.o.HumanWait
	key := r.b.Template.SessionKey
	return &authGate{sem: make(chan struct{}, 1), timeout: hw.Ceiling(), hw: hw, key: key, baseline: hw.Seconds(key)}
}

func (a *authGate) run(fn func()) {
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
		fn()
		return
	default:
	}
	t := time.NewTimer(max(a.timeout, 0))
	defer t.Stop()
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
		fn()
	case <-t.C:
		slog.Warn(fmt.Sprintf("authorization gate lock not acquired after %ss (holder wedged in a pre_tool_call plugin or approval round-trip?); running prompt unserialized", secondsText(a.timeout, 1)))
		fn()
	}
}

func (a *authGate) excluded() time.Duration {
	s := a.hw.Seconds(a.key) - a.baseline
	if s < 0 {
		s = 0
	}
	return time.Duration(s * float64(time.Second))
}

type concSlot struct {
	call     *Call
	args     Args
	result   Result
	duration time.Duration
	failed   bool
	suffix   string
	blocked  bool
	halt     *GuardDecision
	status   string
	errType  string
}

type concBatch struct {
	r        *run
	parsed   []*parsed
	gate     *startGate
	auth     *authGate
	mu       sync.Mutex
	slots    map[int]*concSlot
	closed   map[int]bool
	finished map[int]bool
	cancels  map[int]context.CancelCauseFunc
	timedOut map[int]bool
}

func (cb *concBatch) fill(i int, s *concSlot) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if !cb.closed[i] {
		cb.slots[i] = s
	}
}

func (r *run) imageParallelLimit(ctx context.Context) int {
	limit := defaultImageParallel
	cfg, err := r.e.o.Config.Load(ctx)
	if err == nil && cfg.Extra.ImageGen != nil {
		if n := cfg.Extra.ImageGen.Setting("max_parallel_requests"); n != nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(n.Value), 64); err == nil && n.Tag != "!!str" {
				limit = int(v)
			} else if v, err := strconv.Atoi(strings.TrimSpace(n.Value)); err == nil {
				limit = v
			}
		}
	}
	return max(1, min(limit, maxToolWorkers))
}

func (r *run) parallel(ctx context.Context, idx []int) {
	if ctx.Err() != nil {
		slog.Info(fmt.Sprintf("⚡ Interrupt: skipping %d tool call(s)", len(idx)))
		r.skipCalls(ctx, idx, "[Tool execution cancelled — {name} was skipped. "+skipWording(ctx)+"]", "user_interrupt", false, false)
		return
	}
	cb := &concBatch{r: r, slots: map[int]*concSlot{}, closed: map[int]bool{}, finished: map[int]bool{}, cancels: map[int]context.CancelCauseFunc{}, timedOut: map[int]bool{}}
	names := make([]string, 0, len(idx))
	for _, i := range idx {
		p := r.parse(ctx, i, false)
		cb.parsed = append(cb.parsed, p)
		names = append(names, p.name)
		if p.parseErr != "" {
			cb.slots[i] = &concSlot{call: r.callFor(p), args: p.args, result: Result{Text: p.parseErr}, failed: true, blocked: true}
		}
	}
	joined := strings.Join(names, ", ")
	r.b.Template.Activity(fmt.Sprintf("executing %d tools concurrently: %s", len(idx), joined))
	gateTimeout := startOrderGateTimeout
	if r.concOn {
		gateTimeout = min(startOrderGateTimeout, r.conc/2)
	}
	cb.gate = newStartGate(gateTimeout)
	cb.auth = r.newAuthGate()
	var runnable []*parsed
	imageIn := false
	for _, p := range cb.parsed {
		if p.parseErr == "" {
			runnable = append(runnable, p)
			imageIn = imageIn || p.name == "image_generate"
		}
	}
	if len(runnable) > 0 {
		cb.execute(ctx, runnable, imageIn)
	}
	for k, i := range idx {
		if r.err != nil {
			return
		}
		p := cb.parsed[k]
		cb.mu.Lock()
		s := cb.slots[i]
		cb.closed[i] = true
		timedOut := cb.timedOut[i]
		cb.mu.Unlock()
		if s == nil {
			if !r.commitUnfinished(ctx, p, timedOut) {
				return
			}
			continue
		}
		effect := EffectUnset
		if s.blocked {
			effect = EffectNone
		}
		status, errType := s.status, s.errType
		if p.parseErr != "" {
			ic := *s.call
			ic.Trace = nil
			r.emitPost(ctx, &ic, p.args, s.result, 0, "error", "invalid_tool_arguments", invalidArgsMessage)
			status, errType = "error", "invalid_tool_arguments"
		}
		if !r.commit(ctx, i, commitSlot{
			call: s.call, args: s.args, result: s.result, duration: s.duration, failed: s.failed, suffix: s.suffix,
			blocked: s.blocked, observed: true, effect: effect, status: status, errType: errType, entry: p.entry, halt: s.halt,
			preview: func(res Result) string { return runePrefix(res.Plain(), 200) },
		}) {
			return
		}
	}
}

func (r *run) commitUnfinished(ctx context.Context, p *parsed, timedOut bool) bool {
	c := r.callFor(p)
	var text, status, errType, errText string
	var d time.Duration
	effect := EffectUnset
	switch {
	case timedOut:
		suffix := "the configured timeout"
		if r.concOn {
			suffix = secondsText(r.conc, 1) + "s"
			d = r.conc
		}
		text = fmt.Sprintf("Error executing tool '%s': timed out after %s", c.Name, suffix)
		status, errType, errText, effect = "timeout", "tool_timeout", text, EffectUnknown
	case ctx.Err() != nil:
		why := skipWording(ctx)
		text = fmt.Sprintf("[Tool execution cancelled — %s was skipped. %s]", c.Name, why)
		status, errType, errText = "cancelled", "keyboard_interrupt", "Tool execution cancelled. "+why
	default:
		text = fmt.Sprintf("Error executing tool '%s': thread did not return a result", c.Name)
		status, errType, errText = "error", "thread_missing_result", text
	}
	res := Result{Text: text}
	r.emitPost(ctx, c, p.args, res, d, status, errType, errText)
	return r.commit(ctx, p.index, commitSlot{
		call: c, args: p.args, result: res, duration: d, failed: true, blocked: false, observed: false,
		effect: effect, status: status, errType: errType, entry: p.entry,
		preview: func(res Result) string { return runePrefix(res.Plain(), 200) },
	})
}

func (cb *concBatch) execute(ctx context.Context, runnable []*parsed, imageIn bool) {
	r := cb.r
	workers := min(len(runnable), maxToolWorkers)
	if imageIn {
		workers = min(workers, r.imageParallelLimit(ctx))
	}
	type job struct {
		p     *parsed
		order int
		ctx   context.Context
	}
	jobs := make(chan job, len(runnable))
	for order, p := range runnable {
		wctx, cancel := context.WithCancelCause(ctx)
		cb.cancels[p.index] = cancel
		jobs <- job{p: p, order: order, ctx: wctx}
	}
	close(jobs)
	defer func() {
		for _, cancel := range cb.cancels {
			cancel(context.Canceled)
		}
	}()
	var abandoned atomic.Bool
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if !abandoned.Load() {
					cb.worker(j.ctx, j.p, j.order)
				}
				cb.mu.Lock()
				cb.finished[j.p.index] = true
				cb.mu.Unlock()
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	notDone := func() []*parsed {
		cb.mu.Lock()
		defer cb.mu.Unlock()
		var out []*parsed
		for _, p := range runnable {
			if !cb.finished[p.index] {
				out = append(out, p)
			}
		}
		return out
	}
	nameList := func(ps []*parsed, n int) string {
		var names []string
		for _, p := range ps {
			if len(names) == n {
				break
			}
			names = append(names, p.name)
		}
		return strings.Join(names, ", ")
	}
	var deadline time.Time
	if r.concOn {
		deadline = time.Now().Add(r.conc)
	}
	started := time.Now()
	for {
		wait := concurrentPoll
		expired := false
		if r.concOn {
			remaining := time.Until(deadline.Add(cb.auth.excluded()))
			if remaining <= 0 {
				expired = true
			} else {
				wait = min(wait, remaining)
			}
		}
		if !expired {
			t := time.NewTimer(wait)
			select {
			case <-done:
				t.Stop()
				return
			case <-ctx.Done():
				t.Stop()
			case <-t.C:
			}
			select {
			case <-done:
				return
			default:
			}
			expired = r.concOn && !time.Now().Before(deadline.Add(cb.auth.excluded()))
		}
		pending := notDone()
		if len(pending) == 0 {
			return
		}
		switch {
		case expired:
			cb.mu.Lock()
			for _, p := range pending {
				cb.timedOut[p.index] = true
			}
			cb.mu.Unlock()
			slog.Warn(fmt.Sprintf("concurrent tool batch timed out after %ss; %d tool(s) still running: %s", secondsText(r.conc, 1), len(pending), nameList(pending, 5)))
			abandoned.Store(true)
			cb.gate.abandon()
			for _, p := range runnable {
				cb.cancels[p.index](&Interrupted{})
			}
			return
		case ctx.Err() != nil:
			slog.Info(fmt.Sprintf("⚡ Interrupt: cancelling %d pending concurrent tool(s)", len(pending)))
			abandoned.Store(true)
			cb.gate.abandon()
			t := time.NewTimer(interruptGrace)
			select {
			case <-done:
			case <-t.C:
			}
			t.Stop()
			return
		}
		if elapsed := int(time.Since(started).Seconds()); elapsed > 0 && elapsed%30 < 6 {
			r.b.Template.Activity(fmt.Sprintf("concurrent tools running (%ds, %d remaining: %s)", elapsed, len(pending), nameList(pending, 3)))
		}
	}
}

func (cb *concBatch) worker(ctx context.Context, p *parsed, order int) {
	r := cb.r
	start := time.Now()
	once := &startOnce{gate: cb.gate, order: order, name: p.name}
	defer once.advance(nil)
	c := r.callFor(p)
	m := r.pipeline(ctx, p.args, pipeOpts{index: p.index, call: c, entry: p.entry, scope: p.scope, startGate: once, authGate: cb.auth, dispatch: r.concurrentDispatch})
	m.mu.Lock()
	abandoned, res, args, blocked, dispatched, halt, blockType, runErr := m.abandoned, m.result, m.args, m.blocked, m.dispatched, m.halt, m.blockType, m.err
	m.mu.Unlock()
	if abandoned {
		slog.Info(fmt.Sprintf("tool %s abandoned at start-order gate; skipping dispatch", c.Name))
		return
	}
	if runErr != nil {
		res = Result{Text: fmt.Sprintf("Error executing tool '%s': %v", c.Name, runErr)}
		slog.Error(fmt.Sprintf("_invoke_tool raised for %s: %v", c.Name, runErr))
	}
	d := time.Since(start)
	if !blocked && !dispatched {
		r.emitPost(ctx, c, args, res, d, "", "", "")
	}
	failed, suffix := DetectFailure(c.Name, res, r.e.d.translate)
	if failed {
		slog.Info(fmt.Sprintf("tool %s failed (%ss): %s", c.Name, secondsText(d, 2), runePrefix(res.Plain(), 200)))
	} else {
		slog.Info(fmt.Sprintf("tool %s completed (%ss, %d chars)", c.Name, secondsText(d, 2), len([]rune(res.Plain()))))
	}
	s := &concSlot{call: c, args: args, result: res, duration: d, failed: failed, suffix: suffix, blocked: blocked, halt: halt}
	if blocked {
		s.status, s.errType = "blocked", blockType
	}
	cb.fill(p.index, s)
}

func capable(p *parsed) bool {
	return p.parseErr == "" && p.entry != nil && p.entry.Tool.Prepare != nil
}

func (r *run) sequential(ctx context.Context, idx []int) {
	var runs [][]*parsed
	for _, i := range idx {
		p := r.parse(ctx, i, true)
		if n := len(runs); n > 0 && capable(runs[n-1][0]) == capable(p) {
			runs[n-1] = append(runs[n-1], p)
			continue
		}
		runs = append(runs, []*parsed{p})
	}
	for _, ps := range runs {
		if r.err != nil {
			return
		}
		pb := r.preparedBatch(ctx, ps)
		r.sequentialRun(ctx, ps, pb)
		if pb != nil {
			pb.close()
		}
	}
}

func (r *run) sequentialRun(ctx context.Context, ps []*parsed, pb *prepBatch) {
	rest := func(from int) []int {
		var out []int
		for _, p := range ps[from:] {
			out = append(out, p.index)
		}
		return out
	}
	for k := range ps {
		if r.err != nil {
			return
		}
		if ctx.Err() != nil {
			slog.Info(fmt.Sprintf("⚡ Interrupt: skipping %d tool call(s)", len(ps)-k))
			r.skipCalls(ctx, rest(k), "[Tool execution cancelled — {name} was skipped. "+skipWording(ctx)+"]", "user_interrupt", true, true)
			return
		}
		p := r.parse(ctx, ps[k].index, true)
		c := r.callFor(p)
		if p.parseErr != "" {
			res := Result{Text: p.parseErr}
			r.emitPost(ctx, c, p.args, res, 0, "error", "invalid_tool_arguments", invalidArgsMessage)
			if !r.commitPlain(ctx, p.index, Outcome{Call: c, Args: p.args, Result: res, Display: res, Status: "error", ErrorType: "invalid_tool_arguments", Failed: true, Skipped: true}) {
				return
			}
			continue
		}
		m, d := r.sequentialCall(ctx, p, c, pb)
		if !r.publishSequential(ctx, p, c, m, d, pb) {
			return
		}
		if ctx.Err() != nil && k < len(ps)-1 {
			slog.Info(fmt.Sprintf("⚡ Interrupt: skipping %d remaining tool call(s)", len(ps)-k-1))
			r.skipCalls(ctx, rest(k+1), "[Tool execution skipped — {name} was not started. "+skipWording(ctx)+"]", "", false, true)
			return
		}
	}
}

func (r *run) sequentialCall(ctx context.Context, p *parsed, c *Call, pb *prepBatch) (*managed, time.Duration) {
	start := time.Now()
	timeout, limited := r.seq, r.seqOn
	if p.name == "delegate_task" || p.name == "manage_connections" {
		limited = false
	}
	if limited && p.entry != nil && p.entry.Tool.ExecTimeout > timeout {
		timeout = p.entry.Tool.ExecTimeout
	}
	var slot *prepSlot
	if pb != nil {
		slot = pb.take(c.ID)
		if slot != nil && slot.takeErr != nil {
			m := &managed{args: p.args, err: slot.takeErr}
			return r.settle(c, m), time.Since(start)
		}
	}
	opts := pipeOpts{index: p.index, call: c, entry: p.entry, scope: p.scope, dispatch: r.sequentialDispatch}
	if slot == nil && p.entry != nil && p.entry.Tool.Concurrency == Never {
		return r.settle(c, r.pipeline(ctx, p.args, opts)), time.Since(start)
	}
	var auth *authGate
	var done <-chan struct{}
	var result func() *managed
	var cancel context.CancelCauseFunc
	if slot != nil {
		auth, done, cancel = pb.auth, slot.done, slot.cancel
		c = slot.call
		result = func() *managed { return slot.m }
	} else {
		auth = r.newAuthGate()
		opts.authGate = auth
		wctx, wcancel := context.WithCancelCause(ctx)
		cancel = wcancel
		ch := make(chan struct{})
		var m *managed
		go func() {
			defer close(ch)
			m = r.pipeline(wctx, p.args, opts)
		}()
		done = ch
		result = func() *managed { return m }
	}
	if cancel != nil {
		defer cancel(context.Canceled)
	}
	state := r.pollSequential(ctx, done, start, timeout, limited, auth, p.name)
	if state == "done" {
		return r.settle(c, result()), time.Since(start)
	}
	var text, kind string
	if state == "interrupted" {
		t := time.NewTimer(interruptGrace)
		select {
		case <-done:
			t.Stop()
			return r.settle(c, result()), time.Since(start)
		case <-t.C:
		}
		reason, ok := InterruptReason(ctx)
		if !ok {
			reason = "interrupt requested"
		}
		text, kind = fmt.Sprintf("[Tool execution cancelled — %s was abandoned: %s]", p.name, reason), "cancelled"
		slog.Info(fmt.Sprintf("sequential tool %s abandoned due to %s (%ss elapsed)", p.name, reason, secondsText(time.Since(start), 1)))
		r.emitPost(ctx, c, p.args, Result{Text: text}, time.Since(start), "cancelled", "tool_interrupted", "Tool execution cancelled: "+reason)
	} else {
		text, kind = fmt.Sprintf("Error executing tool '%s': timed out after %ss", p.name, secondsText(timeout, 1)), "timeout"
		slog.Warn(fmt.Sprintf("sequential tool %s timed out after %ss", p.name, secondsText(timeout, 1)))
		r.emitPost(ctx, c, p.args, Result{Text: text}, timeout, "timeout", "tool_timeout", text)
	}
	if slot != nil {
		pb.close()
		if state == "timeout" {
			r.b.Interrupt.Stop("terminal batch timeout")
		}
	}
	if state == "timeout" && cancel != nil {
		cancel(&Interrupted{})
	}
	return &managed{result: Result{Text: text}, args: p.args, dispatched: true, kind: kind}, time.Since(start)
}

func (r *run) settle(c *Call, m *managed) *managed {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		m.result = Result{Text: fmt.Sprintf("Error executing tool '%s': %v", c.Name, m.err)}
		slog.Error(fmt.Sprintf("handle_function_call raised for %s: %v", c.Name, m.err))
		m.blocked, m.dispatched = false, false
	}
	return m
}

func (r *run) pollSequential(ctx context.Context, done <-chan struct{}, started time.Time, timeout time.Duration, limited bool, auth *authGate, name string) string {
	deadline := started.Add(timeout)
	lastBeat := 0
	for {
		slice := sequentialPoll
		if limited {
			remaining := time.Until(deadline.Add(auth.excluded()))
			if remaining <= 0 {
				return "timeout"
			}
			slice = min(slice, remaining)
		}
		t := time.NewTimer(slice)
		select {
		case <-done:
			t.Stop()
			return "done"
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
		select {
		case <-done:
			return "done"
		default:
		}
		if ctx.Err() != nil {
			return "interrupted"
		}
		if elapsed := int(time.Since(started).Seconds()); elapsed-lastBeat >= 30 {
			lastBeat = elapsed
			r.b.Template.Activity(fmt.Sprintf("sequential tool running (%ds): %s", elapsed, name))
		}
	}
}

func (r *run) publishSequential(ctx context.Context, p *parsed, c *Call, m *managed, d time.Duration, pb *prepBatch) bool {
	m.mu.Lock()
	res, args, blocked, kind, halt, blockType, trace := m.result, m.args, m.blocked, m.kind, m.halt, m.blockType, slices.Clone(m.trace)
	m.mu.Unlock()
	c.Trace = trace
	if !blocked && kind == "" {
		r.emitPost(ctx, c, args, res, d, "", "", "")
	}
	failed, suffix := DetectFailure(c.Name, res, r.e.d.translate)
	effect := EffectUnset
	var status, errType string
	switch {
	case kind == "timeout":
		effect, status, errType = EffectUnknown, "timeout", "tool_timeout"
	case kind == "cancelled":
		effect, status, errType = EffectUnknown, "cancelled", "tool_interrupted"
	case blocked:
		status, errType = "blocked", blockType
	}
	if !r.commit(ctx, p.index, commitSlot{
		call: c, args: args, result: res, duration: d, failed: failed, suffix: suffix, blocked: blocked,
		observed: true, effect: effect, status: status, errType: errType, entry: p.entry, halt: halt, successLog: true,
		preview: func(res Result) string { return runePrefix(res.Plain(), 200) },
	}) {
		return false
	}
	if pb != nil && (failed || blocked) {
		pb.markFailure()
	}
	return true
}
