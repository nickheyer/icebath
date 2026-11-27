package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	contextsconfig "github.com/nickheyer/icebath/internal/contexts/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What the turn does after a pass of context management before a model call
type phase int

const (
	// Goes on to the model call
	onward phase = iota
	// The messages changed, so the step starts anew from the stored ones without counting
	anew
	// The turn ends with the reply given
	finish
)

// Fresh compaction state of a turn as it starts
func (r *run) begin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cx.attempts, r.cx.pressure, r.cx.blocked, r.cx.overflow, r.cx.timedOut = 0, 0, false, false, false
	r.cx.shrink, r.cx.stripTools, r.cx.outputCap = 0, false, 0
}

// Whether a compaction asked for the prompt of the turn to be built anew, which this call takes
func (r *run) rebuilt() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	asked := r.cx.rebuild
	r.cx.rebuild = false
	return asked
}

// The messages as the next request sends them, pictures shrunk or tool pictures left out after a provider refused them
func (r *run) outgoing(messages []*sessionsv1.Message) []*sessionsv1.Message {
	r.mu.Lock()
	shrink, strip := r.cx.shrink, r.cx.stripTools
	r.mu.Unlock()
	if strip {
		messages = withoutToolImages(messages)
	}
	if shrink > 0 {
		messages, _ = contexts.ShrinkImages(messages, shrink)
	}
	return messages
}

// Output tokens the next request asks for: the model's own, clamped after a provider stated less room
func (r *run) outputCap(own int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cx.outputCap > 0 && (own <= 0 || r.cx.outputCap < own) {
		return r.cx.outputCap
	}
	return own
}

// Size of the next request in tokens: the provider's count of the newest request with what came since estimated, else an estimate of the
// whole; whether it rests on the provider's count. The history is the session's messages as stored, which the count's anchor fingerprints
func (r *run) pressure(history, system, messages []*sessionsv1.Message, definitions []map[string]any) (int, bool) {
	r.mu.Lock()
	anchor := r.cx.usage
	r.mu.Unlock()
	if tokens, held, _ := anchor.Tokens(history); held {
		return tokens, true
	}
	return requestTokens(system, messages, definitions), false
}

// Takes what the newest model call took: the engine told, the count it gave kept with the session as the start of later ones, and the
// attempts of the turn given back once a compaction it confirms cleared the trigger
func (a *Agent) observe(r *run, engine contexts.Engine, usage *provider.Usage, history []*sessionsv1.Message) {
	var anchor *contexts.Anchor
	if usage != nil {
		anchor = contexts.Capture(int(usage.Prompt()), int(usage.Output), history)
	}
	a.anchor(r, anchor)
	if engine != nil {
		cleared := engine.Observe(usage)
		r.mu.Lock()
		r.cx.pressure = 0
		if cleared {
			r.cx.attempts = 0
		}
		r.mu.Unlock()
	}
	// The dials of the session's window follow every call: its context and what its calls cost
	a.changed(r)
}

// Sets the provider's count the session's context is measured from, nil forgetting it once the messages changed under it, kept with the
// session for the next process to start from
func (a *Agent) anchor(r *run, anchor *contexts.Anchor) {
	r.mu.Lock()
	unchanged := r.cx.usage == nil && anchor == nil
	r.cx.usage = anchor
	r.mu.Unlock()
	if unchanged {
		return
	}
	if store, err := a.store(); err == nil {
		contexts.SaveAnchor(a.ctx, store, r.sessionID(), anchor)
	}
}

// Takes up the count kept with the session when the run has none and the messages still begin with what it priced
func (a *Agent) restoreAnchor(ctx context.Context, r *run, history []*sessionsv1.Message) {
	r.mu.Lock()
	held := r.cx.usage != nil
	r.mu.Unlock()
	if held {
		return
	}
	store, err := a.store()
	if err != nil {
		return
	}
	if anchor := contexts.LoadAnchor(ctx, store, r.sessionID(), history); anchor != nil {
		r.mu.Lock()
		if r.cx.usage == nil {
			r.cx.usage = anchor
		}
		r.mu.Unlock()
	}
}

// Size of the session's context for the views of it: the provider's count with what came since estimated, else an estimate; whether
// any of it is estimated
func (a *Agent) occupancy(ctx context.Context, r *run, history []*sessionsv1.Message) (int, bool) {
	a.restoreAnchor(ctx, r, history)
	r.mu.Lock()
	anchor := r.cx.usage
	r.mu.Unlock()
	if tokens, held, estimated := anchor.Tokens(history); held {
		return tokens, estimated
	}
	return requestTokens(nil, sent(history), nil), true
}

// Compaction as a turn starts: a session resumed after the idle limit compacted when it is over the floor the last compaction raised, then
// passes while the request is over the trigger, up to max_attempts; an engine that keeps its own budget asked for one below it
func (a *Agent) turnStart(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine, idle time.Duration,
	messages []*sessionsv1.Message, tokens int, anchored bool, measure func() int) (phase, error) {
	enabled := settings.GetBool(contextsconfig.Enabled)
	if !enabled {
		if env.Window > 0 && tokens > env.Window {
			a.warnUncompressed(r, tokens, env.Window)
		} else {
			r.rearm()
		}
		return onward, nil
	}
	engine.Turn(messages)
	status := engine.Status()
	idled := false
	if after := time.Duration(max(settings.GetInt(contextsconfig.IdleCompact), 0)) * time.Second; after > 0 && idle >= after && !status.Awaiting &&
		!strings.HasPrefix(status.Blocked, "cooldown") {
		floor := int(float64(status.Threshold) * status.Target)
		effective := floor
		if status.After > 0 {
			effective = max(effective, status.After+floor)
		}
		if tokens > effective {
			slog.Info(fmt.Sprintf("Idle compaction: %ds idle >= %ds, ~%s tokens > %s floor (last compaction produced ~%s) (session %s)", int(idle.Seconds()),
				int(after.Seconds()), strs.Thousands(tokens), strs.Thousands(floor), contextsOr(status.After), env.Session))
			if text := engine.Announce("idle", idleStatus(idle, tokens)); text != "" {
				a.tell(r, text)
			}
			done, err := a.compress(ctx, r, settings, env, engine, compressing{trigger: "idle", tokens: tokens})
			if err != nil || done.rotated {
				return anew, err
			}
			if done.stored {
				// The preflight below weighs what the idle compaction left
				idled, tokens, anchored, status = true, measure(), false, engine.Status()
			}
		}
	}
	deferred := !anchored && engine.Defer(tokens)
	cooldown := strings.HasPrefix(status.Blocked, "cooldown")
	due, why := engine.Due(tokens)
	switch {
	case deferred:
		slog.Info(fmt.Sprintf("Skipping preflight compression: rough estimate ~%s >= %s is not anchored on real usage (last real provider prompt %s); "+
			"deferring to the next response", strs.Thousands(tokens), strs.Thousands(status.Threshold), strs.Thousands(status.Prompt)))
	case cooldown:
		slog.Info(fmt.Sprintf("Skipping preflight compression: same-session cooldown active (%s, session %s)", status.Blocked, env.Session))
		if tokens >= status.Threshold {
			a.warnBlocked(r, status.Blocked, tokens, status.Threshold)
		}
	case due:
		next, err := a.preflight(ctx, r, settings, env, engine, tokens, measure)
		if idled && next == onward {
			next = anew
		}
		return next, err
	case why != "":
		a.warnBlocked(r, why, tokens, status.Threshold)
	default:
		r.rearm()
		if !idled && engine.Maintain(messages) {
			slog.Info(fmt.Sprintf("Engine-driven preflight maintenance: engine requested compress() at ~%s tokens (below %s threshold)",
				strs.Thousands(tokens), strs.Thousands(status.Threshold)))
			done, err := a.compress(ctx, r, settings, env, engine, compressing{trigger: "engine_preflight", tokens: tokens})
			if err != nil || done.stored {
				return anew, err
			}
		}
	}
	if idled {
		return anew, nil
	}
	return onward, nil
}

func contextsOr(n int) string {
	if n <= 0 {
		return "n/a"
	}
	return strs.Thousands(n)
}

// Passes at the start of a turn while the request is over the trigger, up to max_attempts; a turn whose request can't be brought under
// the window fails before its model call
func (a *Agent) preflight(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine, tokens int,
	measure func() int) (phase, error) {
	status := engine.Status()
	r.rearm()
	slog.Info(fmt.Sprintf("Preflight compression: ~%s tokens >= %s threshold (model %s, ctx %s)", strs.Thousands(tokens),
		strs.Thousands(status.Threshold), env.Route.Model, strs.Thousands(env.Window)))
	if text := engine.Announce("preflight", preflightStatus(tokens, status.Threshold)); text != "" {
		a.tell(r, text)
	}
	changed := false
	for range max(settings.GetInt(contextsconfig.MaxAttempts), 1) {
		before := tokens
		done, err := a.compress(ctx, r, settings, env, engine, compressing{trigger: "preflight", tokens: tokens})
		if err != nil {
			return anew, err
		}
		if done.rotated {
			return anew, nil
		}
		if !done.stored && done.reason == "lock" {
			slog.Info(fmt.Sprintf("Preflight compression deferred: compression lock held by another path (session %s)", env.Session))
			break
		}
		changed = changed || done.stored
		tokens = measure()
		if !done.stored || !shrank(len(done.before), len(done.after), before, tokens) {
			if err := a.failClosed(r, env, tokens); err != nil {
				return anew, err
			}
			r.mu.Lock()
			r.cx.blocked = true
			r.mu.Unlock()
			break
		}
		if due, _ := engine.Due(tokens); !due {
			break
		}
		if !another(before, tokens, status.Threshold) {
			r.mu.Lock()
			r.cx.blocked = true
			r.mu.Unlock()
			slog.Warn(fmt.Sprintf("Preflight compression made insufficient progress: ~%s -> ~%s request tokens; skipping additional passes",
				strs.Thousands(before), strs.Thousands(tokens)))
			if err := a.failClosed(r, env, tokens); err != nil {
				return anew, err
			}
			break
		}
	}
	if changed {
		return anew, nil
	}
	return onward, nil
}

// Stops a turn before its model call when the request is still over the model's window: its compression timed out, or it could not shrink
// it and nothing holds it off for a moment
func (a *Agent) failClosed(r *run, env *contexts.Env, tokens int) error {
	if env.Window <= 0 || tokens <= env.Window {
		r.mu.Lock()
		timedOut := r.cx.timedOut
		r.mu.Unlock()
		if timedOut && env.Window > 0 {
			slog.Warn(fmt.Sprintf("Preflight compression timed out but the request (~%s tokens) fits the model window (%s); sending it uncompressed this turn",
				strs.Thousands(tokens), strs.Thousands(env.Window)))
		}
		return nil
	}
	r.mu.Lock()
	timedOut, deferred := r.cx.timedOut, r.cx.deferred
	r.mu.Unlock()
	if timedOut {
		return fmt.Errorf("Summarizing older messages took too long while the session was still about %s tokens, so nothing was sent to the "+
			"model. Type /compress and wait for it to finish, then send your message again.", strs.Thousands(tokens))
	}
	if strings.HasPrefix(deferred, "transient:") {
		return nil
	}
	return fmt.Errorf("Summarizing could not bring this session, at about %s tokens, under the %s the model can read, so nothing was sent to "+
		"the model. Start a new session with /new, because this one can't be made smaller.", strs.Thousands(tokens), strs.Thousands(env.Window))
}

// Compaction before a model call later in the turn, when the request grew over the trigger or a provider refused the last one for its
// size; the reply the turn ends with when it can't go on
func (a *Agent) preAPI(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine, messages []*sessionsv1.Message,
	tokens int, anchored bool) (phase, string, error) {
	status := engine.Status()
	threshold := status.Threshold
	limit := max(settings.GetInt(contextsconfig.MaxAttempts), 1)
	enabled := settings.GetBool(contextsconfig.Enabled)
	r.mu.Lock()
	last := r.cx.pressure
	r.cx.pressure = 0
	overflow := r.cx.overflow && (threshold <= 0 || tokens >= threshold)
	if r.cx.overflow && !overflow {
		r.cx.overflow = false
	}
	if last > 0 && tokens >= threshold && !another(last, tokens, threshold) {
		r.cx.blocked = true
		slog.Warn(fmt.Sprintf("Pre-API compression made insufficient progress: ~%s -> ~%s request tokens; skipping additional preflight passes",
			strs.Thousands(last), strs.Thousands(tokens)))
	}
	blocked, attempts := r.cx.blocked, r.cx.attempts
	r.mu.Unlock()
	deferred := !anchored && engine.Defer(tokens)
	cooldown := strings.HasPrefix(status.Blocked, "cooldown")
	eligible := enabled && len(messages) > 1 && attempts < limit
	exhausted := func() (phase, string, error) {
		slog.Error(fmt.Sprintf("Context compression failed after %d attempts; rebuilt request remains over threshold at ~%s tokens.", limit,
			strs.Thousands(tokens)))
		return finish, fmt.Sprintf(overflowCopy, env.Route.Model), nil
	}
	due := false
	if eligible && (!blocked || overflow) && (!deferred || overflow) && !cooldown {
		due, _ = engine.Due(tokens)
	}
	switch {
	case due:
		r.mu.Lock()
		r.cx.attempts++
		attempts = r.cx.attempts
		r.cx.pressure = tokens
		r.mu.Unlock()
		r.rearm()
		slog.Info(fmt.Sprintf("Pre-API compression: ~%s request tokens >= %s threshold (context=%s, attempt=%d/%d)", strs.Thousands(tokens),
			strs.Thousands(threshold), contextsOr(env.Window), attempts, limit))
		if text := engine.Announce("pre_api", preAPIStatus(tokens)); text != "" {
			a.tell(r, text)
		}
		done, err := a.compress(ctx, r, settings, env, engine, compressing{trigger: "pre_api", tokens: tokens, bypass: overflow})
		if err != nil {
			return finish, "", err
		}
		r.mu.Lock()
		timedOut := r.cx.timedOut
		r.mu.Unlock()
		if timedOut {
			return finish, compressionTimeoutReply, nil
		}
		if !done.stored {
			if _, deferred := a.deferredReply(r); deferred {
				r.mu.Lock()
				r.cx.attempts--
				r.cx.pressure = 0
				r.mu.Unlock()
				break
			}
		}
		if done.stored {
			if a.handoffOnly(ctx, r) {
				slog.Info("Skipping post-compaction model call: reference-only handoff would be the sole active user turn (#80622)")
				return finish, handoffSkipReply, nil
			}
			return anew, "", nil
		}
	case overflow && cooldown:
		return finish, transientReply, nil
	case overflow && attempts >= limit:
		return exhausted()
	case eligible && !deferred && cooldown:
		a.warnBlocked(r, status.Blocked, tokens, threshold)
	case !enabled && len(messages) > 1 && env.Window > 0 && tokens > env.Window:
		a.warnUncompressed(r, tokens, env.Window)
	}
	if overflow {
		return exhausted()
	}
	return onward, "", nil
}

// Compaction after a round of tool results pushed the request over the trigger, else the warning that it is held off and the early trim
// of old tool output; the reply the turn ends with when the compaction leaves nothing to answer
func (a *Agent) postTool(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine, tokens int) (string, error) {
	status := engine.Status()
	if status.Awaiting || !settings.GetBool(contextsconfig.Enabled) {
		return "", nil
	}
	limit := max(settings.GetInt(contextsconfig.MaxAttempts), 1)
	r.mu.Lock()
	attempts := r.cx.attempts
	r.mu.Unlock()
	due, why := engine.Due(tokens)
	if due && attempts < limit {
		r.mu.Lock()
		r.cx.attempts++
		r.mu.Unlock()
		r.rearm()
		a.activity(r, settings, "Summarizing older messages to make room.", false)
		done, err := a.compress(ctx, r, settings, env, engine, compressing{trigger: "post_tool", tokens: tokens})
		if err != nil {
			return "", err
		}
		if !done.stored && done.reason == "lock" {
			r.mu.Lock()
			r.cx.attempts--
			r.mu.Unlock()
		}
		if done.stored && a.handoffOnly(ctx, r) {
			slog.Info("Skipping post-tool compaction model call: reference-only handoff would be the sole active user turn (#80622)")
			return handoffSkipReply, nil
		}
		return "", nil
	}
	if due {
		why = fmt.Sprintf("attempts_exhausted:%d", attempts)
	}
	if why != "" {
		a.warnBlocked(r, why, tokens, status.Threshold)
	}
	return "", a.prune(ctx, r, settings, env, engine, tokens)
}

// Early trim of old tool output once the session passes its size, stored in place of the messages it shortens
func (a *Agent) prune(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine, tokens int) error {
	store, err := a.store()
	if err != nil {
		return err
	}
	history, err := store.Messages(ctx, env.Session, sessions.Model)
	if err != nil || len(history) == 0 {
		return err
	}
	pruned, count := engine.Prune(ctx, env, history, tokens)
	if count == 0 || same(history, pruned) {
		return nil
	}
	if err := a.rewrite(ctx, r, store, env, history, pruned); err != nil {
		slog.Debug(fmt.Sprintf("proactive tool-result prune failed; skipping: %v", err))
		return nil
	}
	slog.Info(fmt.Sprintf("Proactive prune: %d old tool result(s) cut to one-line summaries (session=%s, ~%s -> ~%s tokens)", count, env.Session,
		strs.Thousands(contexts.Estimate(history)), strs.Thousands(contexts.Estimate(pruned))))
	a.anchor(r, nil)
	a.tools.Compacted(settings, env.Session)
	a.postCompaction(ctx, settings, env.Session, env.Session, "prune_tool_results", "proactive_prune", contexts.Estimate(history), contexts.Estimate(pruned), 0)
	return nil
}

// Stores messages an engine rewrote without summarising in place of the session's, its originals kept for the person
func (a *Agent) rewrite(ctx context.Context, r *run, store sessions.Store, env *contexts.Env, history, rewritten []*sessionsv1.Message) error {
	return store.Compact(ctx, env.Session, history[len(history)-1].GetId(), storable(history, rewritten, true), sessions.Guard{Turn: a.holder(r)})
}

// Work at the end of a turn: an engine that folds each finished exchange into a running summary told, its result stored, and a summary
// prepared ahead of the next compaction once the session nears the trigger
func (a *Agent) finished(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine) error {
	store, err := a.store()
	if err != nil {
		return err
	}
	history, err := store.Messages(ctx, env.Session, sessions.Model)
	if err != nil || len(history) == 0 {
		return err
	}
	result, err := engine.Finished(ctx, env, history)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		slog.Debug("context engine turn-end work failed", "session", env.Session, "error", err)
	}
	if result != nil && len(result.Messages) > 0 && !same(history, result.Messages) {
		if err := a.rewrite(ctx, r, store, env, history, result.Messages); err != nil {
			slog.Warn("Could not store the context the engine rewrote at the end of the turn", "session", env.Session, "error", err)
			engine.Settled(contexts.Outcome{Failure: err.Error()})
		} else {
			engine.Settled(contexts.Outcome{Stored: true, Tokens: contexts.Estimate(result.Messages)})
			a.anchor(r, nil)
			a.tools.Compacted(settings, env.Session)
			a.postCompaction(ctx, settings, env.Session, env.Session, "micro_summarize", "between_turns", contexts.Estimate(history),
				contexts.Estimate(result.Messages), len(history)-len(result.Messages))
			history = result.Messages
		}
	}
	engine.Prepare(a.ctx, env, history, "turn-end")
	return nil
}

// Whether the newest message the model is sent is a summary carrying no request of the person, which the model would answer as one
func (a *Agent) handoffOnly(ctx context.Context, r *run) bool {
	store, err := a.store()
	if err != nil {
		return false
	}
	history, err := store.Messages(ctx, r.sessionID(), sessions.Model)
	if err != nil {
		return false
	}
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.GetRole() == sessionsv1.Role_ROLE_TOOL || len(m.GetToolCalls()) > 0 {
			return false
		}
		if m.GetRole() != sessionsv1.Role_ROLE_USER {
			return false
		}
		if contexts.RealUser(m) {
			return false
		}
		if contexts.IsSummary(m) {
			text := strings.TrimSpace(contexts.Text(m))
			return text == strings.TrimSpace(contexts.HandoffOnly(text))
		}
	}
	return false
}

// End of a turn with a reply about its context: shown as the turn's failure, or as a notice when the turn has nothing left to answer
func (a *Agent) ending(r *run, reply string) error {
	if reply == handoffSkipReply {
		a.tell(r, reply)
		return nil
	}
	return errors.New(reply)
}

// Messages the request carries as the engine picks them from the conversation for the person's newest message; those given when it
// picks none or what it picks is no list of messages
func selected(r *run, engine contexts.Engine, request, conversation []*sessionsv1.Message, window int) []*sessionsv1.Message {
	var incoming *sessionsv1.Message
	for i := len(conversation) - 1; i >= 0; i-- {
		if contexts.RealUser(conversation[i]) {
			incoming = conversation[i]
			break
		}
	}
	picked := engine.Select(request, conversation, incoming, window)
	if picked == nil {
		return request
	}
	if len(picked) == 0 || slices.Contains(picked, nil) {
		slog.Warn(fmt.Sprintf("Context engine select_context returned an invalid value (not a non-empty list of dicts); ignoring (session=%s)", r.sessionID()))
		return request
	}
	return picked
}
