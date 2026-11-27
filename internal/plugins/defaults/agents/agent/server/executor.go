package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/tools"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Calls of one batch running at once
	maxWorkers = 8
	// How long a call stopped by the user gets to finish before it is given up
	abandonGrace = 3 * time.Second
	// Fewest seconds between two progress reports of one call shown, the first and the last always shown
	progressGap = 500 * time.Millisecond
)

// Why calls were cut short when the user stopped the reply, as the model is told
const stopWording = "Explicit stop requested"

// One call of a reply as the agent runs it: the tool it names in the turn's tools, its arguments and the paths it holds while it runs
type pending struct {
	call *sessionsv1.ToolCall
	def  tools.Def
	// The tool is one the turn offers
	known bool
	args  map[string]any
	// Why its arguments could not be read, empty when they could
	invalid string
}

// What came of one call: the result the model reads, what the user is shown in its place, the images it hands over, whether it failed
type ran struct {
	text    string
	display string
	media   []*sessionsv1.Media
	failed  bool
}

// Runs the tool calls of one reply: in turn, or several at once where they may, each under the time limit, the user's stop cutting the
// batch short, every result folded through the guardrails and stored in the order of the calls
type executor struct {
	a     *Agent
	r     *run
	env   *tools.Env
	tools map[string]tools.Def
	guard *guardrails
	// Stores one result as the session's next message
	commit func(message *sessionsv1.Message) error
	// Notes the turn adds to the last result of the batch, such as how much of its budget is spent
	tail func() string
	// Longest a batch of calls together, and one call alone, may take; zero for no limit
	batch, single time.Duration
	// Characters the results of the reply stored so far take in the chat, against the budget of all of them
	spent int
}

// Limits of tool calls the profile's settings set
func toolLimits(settings *config.Config) (batch, single time.Duration) {
	batch = time.Duration(max(settings.GetInt(agentconfig.ToolBatchTimeout), 0)) * time.Second
	single = batch
	if value := settings.Get(agentconfig.ToolCallTimeout); value != nil && strings.TrimSpace(cast.ToString(value)) != "" {
		single = time.Duration(max(cast.ToInt(value), 0)) * time.Second
	}
	return batch, single
}

// Runs every call of the reply, storing a result for each
func (x *executor) run(ctx context.Context, calls []*sessionsv1.ToolCall) error {
	batch := make([]*pending, len(calls))
	for i, call := range calls {
		def, known := x.tools[call.GetName()]
		p := &pending{call: call, def: def, known: known}
		if err := json.Unmarshal([]byte(cmp.Or(call.GetArguments(), "{}")), &p.args); err != nil {
			p.invalid = err.Error()
		}
		batch[i] = p
	}
	segments := x.plan(batch)
	for i, segment := range segments {
		last := i == len(segments)-1
		var err error
		if segment.parallel {
			err = x.together(ctx, segment.calls, last)
		} else {
			err = x.inTurn(ctx, segment.calls, last)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Run of calls of a batch: several at once, or one after another
type segment struct {
	parallel bool
	calls    []*pending
}

type reserved struct {
	path   string
	writes bool
}

// Splits the batch into runs that keep the order of its calls: calls that may run alongside others gathered together until one touches a
// path another of them writes, every other call running on its own
func (x *executor) plan(batch []*pending) []segment {
	var segments []segment
	var current []*pending
	var held []reserved
	alone := func(calls ...*pending) {
		if n := len(segments); n > 0 && !segments[n-1].parallel {
			segments[n-1].calls = append(segments[n-1].calls, calls...)
			return
		}
		segments = append(segments, segment{calls: calls})
	}
	flush := func() {
		switch {
		case len(current) >= 2:
			segments = append(segments, segment{parallel: true, calls: current})
		case len(current) == 1:
			alone(current...)
		}
		current, held = nil, nil
	}
	for _, p := range batch {
		paths, writes, ok := x.admit(p)
		if !ok {
			flush()
			alone(p)
			continue
		}
		for _, path := range paths {
			if clash := func() bool {
				for _, h := range held {
					if (writes || h.writes) && overlap(path, h.path) {
						return true
					}
				}
				return false
			}(); clash {
				flush()
				break
			}
		}
		for _, path := range paths {
			held = append(held, reserved{path: path, writes: writes})
		}
		current = append(current, p)
	}
	flush()
	return segments
}

// Whether the call may run alongside others, and the paths it holds while it does
func (x *executor) admit(p *pending) ([]string, bool, bool) {
	def := p.def
	if !p.known || p.invalid != "" || p.args == nil || def.Timeout < 0 || x.batch > 0 && def.Timeout > x.batch {
		return nil, false, false
	}
	if def.Paths != nil {
		raw, writes := def.Paths(x.env, p.args)
		var paths []string
		for _, path := range raw {
			if strings.TrimSpace(path) != "" {
				paths = append(paths, canonicalPath(path, x.env.Cwd))
			}
		}
		return paths, writes, len(paths) > 0
	}
	return nil, false, def.Parallel
}

// Path made absolute against the folder, ~ expanded and links resolved where they exist
func canonicalPath(path, cwd string) string {
	if rest, ok := strings.CutPrefix(path, "~"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + rest
		}
	}
	if !filepath.IsAbs(path) {
		base := cwd
		if base == "" {
			base, _ = os.Getwd()
		}
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	// The part that exists resolved, the rest joined on, so a file about to be made is held where it will be
	dir, rest := path, ""
	for dir != filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		dir, rest = filepath.Dir(dir), filepath.Join(filepath.Base(dir), rest)
	}
	return path
}

// Whether two paths may be the same file or one holds the other
func overlap(a, b string) bool {
	if a == b {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(a, strings.TrimSuffix(b, sep)+sep) || strings.HasPrefix(b, strings.TrimSuffix(a, sep)+sep)
}

// Runs the calls one after another, each under the single call limit; once the user stops the reply, the calls not started are skipped
func (x *executor) inTurn(ctx context.Context, calls []*pending, last bool) error {
	for i, p := range calls {
		final := last && i == len(calls)-1
		if ctx.Err() != nil {
			x.a.tell(x.r, fmt.Sprintf("The reply was stopped, so %s did not run.", strs.Count(len(calls)-i, "tool call")))
			for j, rest := range calls[i:] {
				// Calls cut off before any ran were cancelled; those after one that ran were never started
				text := fmt.Sprintf("[Tool execution cancelled — %s was skipped. %s]", toolName(rest), stopWording)
				if i > 0 {
					text = fmt.Sprintf("[Tool execution skipped — %s was not started. %s]", toolName(rest), stopWording)
				}
				if err := x.store(rest, ran{text: text, failed: true}, last && i+j == len(calls)-1); err != nil {
					return err
				}
			}
			return nil
		}
		result := x.alone(ctx, p)
		x.fit([]*pending{p}, []*ran{&result})
		if err := x.store(p, result, final); err != nil {
			return err
		}
	}
	return nil
}

// Runs one call on its own under its deadline, the time spent waiting on the user left out of it; a call the user stops is given a
// little while to end, then given up
func (x *executor) alone(ctx context.Context, p *pending) ran {
	limit := x.limit(p, x.single)
	callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	waits := &waiting{}
	done := make(chan ran, 1)
	go func() { done <- x.dispatch(callCtx, p, waits) }()
	stopped := ctx.Done()
	started := time.Now()
	for {
		var expire <-chan time.Time
		if limit > 0 {
			expire = time.After(max(time.Until(started.Add(limit+waits.total())), 0))
		}
		select {
		case result := <-done:
			return result
		case <-stopped:
			cancel()
			select {
			case result := <-done:
				return result
			case <-time.After(abandonGrace):
				return ran{text: fmt.Sprintf("[Tool execution cancelled — %s was abandoned: %s]", toolName(p), "explicit stop requested"), failed: true}
			}
		case <-expire:
			if waits.busy() {
				// The call waits on the user, which counts for nothing: the deadline moves on by what they took once they answer
				time.Sleep(time.Second)
				continue
			}
			cancel()
			return ran{text: fmt.Sprintf("Error executing tool '%s': timed out after %.1fs", toolName(p), limit.Seconds()), failed: true}
		}
	}
}

// Runs the calls at once, at most maxWorkers of them or fewer as a tool of the batch asks, starting in their order; their results are
// stored in that order once every one finished, the batch limit ran out or the user stopped the reply
func (x *executor) together(ctx context.Context, calls []*pending, last bool) error {
	results := make([]*ran, len(calls))
	var mu sync.Mutex
	callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	waits := &waiting{}
	var wg sync.WaitGroup
	slots := make(chan struct{}, x.workers(calls))
	launched := make(chan struct{})
	go func() {
		defer close(launched)
		for i, p := range calls {
			select {
			case slots <- struct{}{}:
			case <-callCtx.Done():
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				result := x.dispatch(callCtx, p, waits)
				mu.Lock()
				results[i] = &result
				mu.Unlock()
			}()
		}
	}()
	finished := make(chan struct{})
	go func() {
		<-launched
		wg.Wait()
		close(finished)
	}()
	limit := x.batch
	started := time.Now()
	timedOut, stopped := false, false
	for waiting := true; waiting; {
		var expire <-chan time.Time
		if limit > 0 {
			expire = time.After(max(time.Until(started.Add(limit+waits.total())), 0))
		}
		select {
		case <-finished:
			waiting = false
		case <-ctx.Done():
			stopped = true
			cancel()
			select {
			case <-finished:
			case <-time.After(abandonGrace):
			}
			waiting = false
		case <-expire:
			if waits.busy() {
				time.Sleep(time.Second)
				continue
			}
			timedOut = true
			cancel()
			waiting = false
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, p := range calls {
		result := results[i]
		switch {
		case result != nil:
		case timedOut:
			result = &ran{text: fmt.Sprintf("Error executing tool '%s': timed out after %.1fs", toolName(p), limit.Seconds()), failed: true}
		case stopped:
			result = &ran{text: fmt.Sprintf("[Tool execution cancelled — %s was skipped. %s]", toolName(p), stopWording), failed: true}
		default:
			result = &ran{text: fmt.Sprintf("Error executing tool '%s': thread did not return a result", toolName(p)), failed: true}
		}
		results[i] = result
	}
	x.fit(calls, results)
	for i, p := range calls {
		if err := x.store(p, *results[i], last && i == len(calls)-1); err != nil {
			return err
		}
	}
	return nil
}

// Calls of the batch that run at once: every one up to maxWorkers, fewer when a tool of the batch allows fewer alongside its calls
func (x *executor) workers(calls []*pending) int {
	n := min(len(calls), maxWorkers)
	for _, p := range calls {
		if p.def.Workers != nil {
			if most := p.def.Workers(x.env); most > 0 {
				n = min(n, most)
			}
		}
	}
	return n
}

// Saves the largest of the results to files, their start kept, until all the results of the reply fit the turn's budget; those stored
// already are as they were
func (x *executor) fit(calls []*pending, results []*ran) {
	budget := tools.BudgetFor(x.env.Config, x.env.Window)
	sizes := make([]int, len(results))
	total := x.spent
	for i, result := range results {
		sizes[i] = utf8.RuneCountInString(result.text)
		total += sizes[i]
	}
	if total > budget.Turn {
		order := make([]int, 0, len(results))
		for i, result := range results {
			if !strings.Contains(result.text, tools.SpillTag) {
				order = append(order, i)
			}
		}
		slices.SortStableFunc(order, func(a, b int) int { return sizes[b] - sizes[a] })
		for _, i := range order {
			if total <= budget.Turn {
				break
			}
			id := cmp.Or(calls[i].call.GetId(), fmt.Sprintf("budget_%d", i))
			replacement := tools.Spill(x.env, results[i].text, "__budget_enforcement__", id, 0, budget.Preview)
			if replacement == results[i].text {
				continue
			}
			slog.Info(fmt.Sprintf("Budget enforcement: persisted tool result %s (%d chars)", id, sizes[i]))
			total += utf8.RuneCountInString(replacement) - sizes[i]
			results[i].text = replacement
		}
	}
	x.spent = total
}

// Deadline of a call: the tool's own budget when longer, none for a tool watching its own liveness
func (x *executor) limit(p *pending, base time.Duration) time.Duration {
	switch {
	case p.def.Timeout < 0:
		return 0
	case base > 0 && p.def.Timeout > base:
		return p.def.Timeout
	}
	return base
}

// Time calls of a batch spent waiting on the user, which no deadline counts
type waiting struct {
	mu    sync.Mutex
	spent time.Duration
	open  int
}

func (w *waiting) total() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.spent
}

func (w *waiting) busy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.open > 0
}

// Runs fn, the time it takes counted as waiting on the user
func waitFor[T any](w *waiting, fn func() T) T {
	w.mu.Lock()
	w.open++
	w.mu.Unlock()
	started := time.Now()
	defer func() {
		w.mu.Lock()
		w.open--
		w.spent += time.Since(started)
		w.mu.Unlock()
	}()
	return fn()
}

// Runs one call: refused by the guardrails or a bad call, else its tool on an environment of its own whose questions to the user count
// as waiting, its progress shown as it goes; the result folded through the guardrails and, when it comes from outside, handed over as data
func (x *executor) dispatch(ctx context.Context, p *pending, waits *waiting) ran {
	name := p.call.GetName()
	switch {
	case !p.known:
		return ran{text: x.a.unoffered(x.r, name, x.tools), failed: true}
	case p.invalid != "":
		return ran{text: fmt.Sprintf("Error: Invalid JSON arguments. %s. For tools with no required parameters, use an empty object: {}. "+
			"Please retry with valid JSON.", p.invalid), failed: true}
	}
	if paths := prunedArguments(p.def, p.args); len(paths) > 0 {
		blocked, _ := json.Marshal(map[string]any{"error": prunedArgumentsError, "message": prunedArgumentsMessage, "argument_paths": paths})
		return ran{text: string(blocked), failed: true}
	}
	if v := x.guard.before(name, p.args); v.halts() {
		return ran{text: blockedResult(v), failed: true}
	}
	env := *x.env
	if approve := env.Approve; approve != nil {
		env.Approve = func(ctx context.Context, action, reason string) error {
			return waitFor(waits, func() error { return approve(ctx, action, reason) })
		}
	}
	if gate := env.Gate; gate != nil {
		env.Gate = func(ctx context.Context, g tools.Gate) error {
			return waitFor(waits, func() error { return gate(ctx, g) })
		}
	}
	if command := env.Command; command != nil {
		env.Command = func(ctx context.Context, line string, isolated bool) tools.Verdict {
			return waitFor(waits, func() tools.Verdict { return command(ctx, line, isolated) })
		}
	}
	if clarify := env.Clarify; clarify != nil {
		env.Clarify = func(ctx context.Context, questions []tools.Question) (tools.Reply, error) {
			type answered struct {
				reply tools.Reply
				err   error
			}
			got := waitFor(waits, func() answered {
				reply, err := clarify(ctx, questions)
				return answered{reply, err}
			})
			return got.reply, got.err
		}
	}
	call := &tools.Call{Name: name, RawArgs: p.call.GetArguments(), ToolCallID: p.call.GetId(), SessionID: x.r.sessionID(), Env: &env,
		Progress: x.progress(p.call)}
	c := x.r.child
	c.toolStarted(name, p.call.GetArguments())
	began := time.Now()
	text := x.a.tools.Run(ctx, p.def, call)
	broke, _ := tools.Broke(name, text)
	broke = broke || tools.Failed(text)
	c.toolFinished(name, text, broke, time.Since(began))
	args := call.Args
	if args == nil {
		args = p.args
	}
	text = x.guard.fold(name, args, text, p.call.GetId(), broke)
	if p.def.Untrusted {
		text = untrusted(name, text, func(note string) {
			x.r.mu.Lock()
			defer x.r.mu.Unlock()
			x.a.notify(x.r, note)
		})
	}
	return ran{text: text, display: call.Display, media: call.Media, failed: broke}
}

// Refusal of a call whose arguments carry what a compression left in place of the text it cut, which written anywhere would destroy it
const (
	prunedArgumentsError   = "suspected_pruned_tool_arguments"
	prunedArgumentsMessage = "Tool was not executed because effect-capable arguments contain an Icebath context-compression artifact. Recover the " +
		"exact content from its durable source or re-read it, then issue a complete new call; do not retry these arguments. To remove a marker " +
		"that already landed in a file, match it by its ICEBATH-CONTEXT-COMPRESSION prefix (e.g. a terminal sed on that line) instead of " +
		"quoting the full marker."
)

// Paths of the arguments of a call of a tool that may change something that carry a compression marker, in sorted key order
func prunedArguments(def tools.Def, args map[string]any) []string {
	if def.Effect == tools.ReadOnly || args == nil {
		return nil
	}
	var found []string
	var walk func(value any, path string)
	walk = func(value any, path string) {
		switch v := value.(type) {
		case string:
			if contexts.MarkerArtifact.MatchString(v) {
				found = append(found, path)
			}
		case map[string]any:
			for _, key := range slices.Sorted(maps.Keys(v)) {
				child := path + "[" + strconv.Quote(key) + "]"
				if identifier.MatchString(key) {
					child = path + "." + key
				}
				walk(v[key], child)
			}
		case []any:
			for i, item := range v {
				walk(item, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(any(args), "$")
	return found
}

// Key that a path names after a dot
var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Reports the call's progress to the session's windows, at most one report each progressGap besides the first and the last
func (x *executor) progress(call *sessionsv1.ToolCall) func(float64, *float64, string) {
	var mu sync.Mutex
	var last time.Time
	return func(progress float64, total *float64, message string) {
		done := total != nil && *total > 0 && progress >= *total
		mu.Lock()
		if !last.IsZero() && !done && time.Since(last) < progressGap {
			mu.Unlock()
			return
		}
		last = time.Now()
		mu.Unlock()
		event := &agentv1.Progress{ToolCallId: call.GetId(), Name: call.GetName(), Progress: progress, Message: message}
		if total != nil && !math.IsNaN(*total) {
			event.Total = new(*total)
		}
		x.r.mu.Lock()
		defer x.r.mu.Unlock()
		x.r.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Progress{Progress: event}})
	}
}

// Stores a call's result, the turn's notes added to the last of the batch
func (x *executor) store(p *pending, result ran, final bool) error {
	text := result.text
	if final && x.tail != nil {
		if note := x.tail(); note != "" {
			text += "\n\n" + note
		}
	}
	return x.commit(&sessionsv1.Message{Role: sessionsv1.Role_ROLE_TOOL, ToolCallId: p.call.GetId(), ToolName: p.call.GetName(), Text: text,
		Failed: result.failed, Media: result.media, Shown: result.display})
}

func toolName(p *pending) string {
	return cmp.Or(p.call.GetName(), "tool")
}
