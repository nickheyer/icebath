package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	contextsconfig "github.com/nickheyer/icebath/internal/contexts/config"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/tools/redact"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Life of the session's compression lock, refreshed while a compression runs
	compressionLease   = 120 * time.Second
	compressionRefresh = 30 * time.Second
	// Wait for a compression the watchdog gave up on to let go before going on without it
	cancelGrace = 5 * time.Second
	// Compressions running at once over every session, beyond which one is refused for the cycle
	compressionSlots = 4
	// Recent exchanges a manual compression of the start of the chat keeps whole, and the most it keeps
	keepLast    = 2
	maxKeepLast = 100
)

// One compression the session asks for
type compressing struct {
	// What asked: preflight, pre_api, post_tool, idle, overflow, engine_preflight or manual
	trigger string
	// Size of the request in tokens as last measured
	tokens int
	focus  string
	// Asked for by the user, which no trigger, backoff or guard holds off
	force bool
	// The cooldown after a failed summary is not consulted
	bypass     bool
	aggressive bool
	// Exchanges at the end kept whole outside the compression, 0 to compress the whole chat
	keep int
}

// What a compression came to
type compressed struct {
	// The session's messages were changed, and whether in a new session
	stored  bool
	rotated bool
	// Why nothing was stored: blocked, lock, saturated, rotated, aborted, unchanged, empty, refused, failed or timeout; empty when stored
	reason string
	// Messages the model held before and holds after, the after equal to the before when nothing was stored
	before, after []*sessionsv1.Message
	result        *contexts.Result
}

// Compresses the session's conversation with its engine under the session's compression lock and the watchdog, storing the result in
// place or in a continuation of the session. The turn calls it with the lease of its turn held
func (a *Agent) compress(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine, ask compressing) (compressed, error) {
	store, err := a.store()
	if err != nil {
		return compressed{}, err
	}
	out := compressed{reason: "blocked"}
	r.mu.Lock()
	r.cx.deferred, r.cx.timedOut = "", false
	r.mu.Unlock()
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		slog.Warn(fmt.Sprintf("Context compression pool saturated (%d workers busy) — refusing new compression this cycle and continuing without "+
			"compression. Wedged workers are fence-cancelled and free their slot when they return; if this persists, check the summary provider "+
			"health.", compressionSlots))
		out.reason = "saturated"
		return out, nil
	}
	holder := fmt.Sprintf("pid=%d:backend=%s:session=%s:nonce=%s", os.Getpid(), a.life.backend.ID, env.Session, rand.Text()[:8])
	acquired, err := store.AcquireCompression(ctx, env.Session, holder, compressionLease)
	if err != nil {
		return out, err
	}
	if !acquired {
		other, _ := store.CompressionHolder(ctx, env.Session)
		r.mu.Lock()
		r.cx.deferred = "lock:" + cmp.Or(other, "unconfirmed")
		r.mu.Unlock()
		slog.Info(fmt.Sprintf("Compression skipped: compression lock held by another path (session=%s holder=%s)", env.Session, cmp.Or(other, "unconfirmed")))
		out.reason = "lock"
		return out, nil
	}
	refreshing, stopRefresh := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		ticker := time.NewTicker(compressionRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-refreshing.Done():
				return
			case <-ticker.C:
				if ok, err := store.RefreshCompression(refreshing, env.Session, holder, compressionLease); err != nil || !ok {
					slog.Warn("Session compression lock lost", "session", env.Session, "error", err)
				}
			}
		}
	}()
	defer func() {
		stopRefresh()
		if err := store.ReleaseCompression(context.WithoutCancel(ctx), env.Session, holder); err != nil {
			slog.Debug("Could not release the session compression lock", "session", env.Session, "error", err)
		}
	}()
	if tip, err := store.Tip(ctx, env.Session); err == nil && tip != "" && tip != env.Session {
		if err := a.adopt(ctx, r, tip); err != nil {
			return out, err
		}
		out.stored, out.rotated, out.reason = true, true, "rotated"
		return out, nil
	}
	if !ask.force && ask.trigger != "engine_preflight" {
		if due, why := engine.Due(ask.tokens); !due {
			if strings.HasPrefix(why, "cooldown") || strings.HasPrefix(why, "structural_backoff") {
				status := engine.Status()
				r.mu.Lock()
				r.cx.deferred = "transient:" + why
				r.mu.Unlock()
				slog.Info(fmt.Sprintf("Skipping automatic compression re-entry: transient guard active (%s, session=%s, last failure: %s) — will retry "+
					"after the backoff lapses; /compress forces an immediate retry", why, env.Session, cmp.Or(status.Blocked, "unknown")))
			}
			return out, nil
		}
	}
	if settings.GetBool(contextsconfig.CheckpointRequired) {
		// No memory provider here saves a checkpoint of the chat before it is compressed, so compression the settings make wait for one fails
		// closed
		return out, errors.New("BLOCKED_MISSING_PREREQUISITE: required pre-compress checkpoint unavailable: no active provider implements checkpoint " +
			"API v2. Recover by set " + contextsconfig.CheckpointRequired + ": false, or switch to a memory provider that implements checkpoint API v2")
	}
	history, err := store.Messages(ctx, env.Session, sessions.Model)
	if err != nil {
		return out, err
	}
	out.before, out.after = history, history
	if len(history) == 0 {
		out.reason = "unchanged"
		return out, nil
	}
	head, kept := history, []*sessionsv1.Message(nil)
	if ask.keep > 0 {
		head, kept = splitKeep(history, ask.keep)
	}
	var restore func()
	if snapshotter, ok := engine.(contexts.Snapshotter); ok {
		restore = snapshotter.Snapshot()
	}
	if text := engine.Announce(ask.trigger, compactionStatus); text != "" {
		a.activity(r, settings, text, true)
	}
	r.mu.Lock()
	r.cx.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.cx.running = false
		r.mu.Unlock()
		// A stored compaction leaves its done line up until the turn goes on
		if !out.stored {
			a.settle(r, settings)
		}
	}()
	r.rearm()
	request := &contexts.Request{Messages: head, Tokens: ask.tokens, Focus: ask.focus, Force: ask.force, Bypass: ask.bypass, Aggressive: ask.aggressive}
	result, err := a.watched(ctx, r, settings, env, engine, request, restore)
	if err != nil {
		if restore != nil {
			restore()
		}
		return out, err
	}
	if result == nil {
		out.reason = "timeout"
		return out, nil
	}
	out.result = result
	list := result.Messages
	if reason := a.rejected(r, engine, head, result); reason != "" {
		a.telemetry(env, ask, result, "aborted", reason, len(history), len(history))
		out.reason = reason
		return out, nil
	}
	a.fellBack(r, result)
	if len(kept) > 0 {
		list = rejoin(list, kept)
	}
	list = keepReply(history, list, env.Session)
	list, heads := a.carry(env, list)
	list = withUserTurn(history, list)
	if original, candidate := contexts.Estimate(history), contexts.Estimate(list); candidate > original {
		if salvaged := salvage(engine, history, list, heads, original); salvaged != nil {
			slog.Info(fmt.Sprintf("Compression salvage recovered a shrinking transcript (session=%s, ~%s -> ~%s tokens)", env.Session,
				strs.Thousands(original), strs.Thousands(contexts.Estimate(salvaged))))
			list = salvaged
		}
	}
	if grown := contexts.Estimate(list) > contexts.Estimate(history); grown {
		slog.Warn(fmt.Sprintf("Compression refused: compressed transcript would be larger than the original (session=%s, ~%s -> ~%s tokens); keeping the "+
			"original transcript unchanged", env.Session, strs.Thousands(contexts.Estimate(history)), strs.Thousands(contexts.Estimate(list))))
		a.tell(r, "⚠️ Compression refused: the generated summary would have GROWN the conversation instead of shrinking it. No messages were "+
			"dropped — conversation continues unchanged.")
		engine.Settled(contexts.Outcome{Refused: true})
		a.telemetry(env, ask, result, "aborted", "would_grow", len(history), len(history))
		out.reason = "refused"
		return out, nil
	}
	rotated, err := a.commit(ctx, r, settings, store, env, history, list, holder)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("Session DB compression split failed — new session will NOT be indexed: %s", err))
		engine.Settled(contexts.Outcome{Failure: err.Error()})
		a.telemetry(env, ask, result, "aborted", "session_split_failed", len(history), len(history))
		out.reason = "failed"
		return out, nil
	}
	estimate := requestTokens(nil, list, nil)
	engine.Settled(contexts.Outcome{Stored: true, Tokens: estimate})
	previous := env.Session
	next := previous
	if rotated != "" {
		next = rotated
	}
	engine.Compressed(previous, next)
	a.compacted(ctx, r, settings, store, next)
	out.stored, out.rotated, out.reason, out.after = true, rotated != "", "", list
	a.telemetry(env, ask, result, "committed", "", len(history), len(list))
	a.postCompaction(ctx, settings, next, previous, "summarize", ask.trigger, result.Record.Before, result.Record.After, len(history)-len(list))
	slog.Info(fmt.Sprintf("context compression done: session=%s messages=%d->%d rough_tokens=~%s awaiting_real_usage=true", next, len(history), len(list),
		strs.Thousands(estimate)))
	if count := engine.Status().Compressions; count >= 2 {
		a.tell(r, fmt.Sprintf("⚠️  Session compressed %d times — accuracy may degrade. Consider /new to start fresh.", count))
	}
	return out, nil
}

// Runs the engine's compression under the watchdog: given up once the summary makes no headway for the idle limit, or the whole takes
// longer than the ceiling, then tried once on the first entry of the summary model's fallback chain and, after an earlier stall, once more
// with a summary built without a model. Nil and no error when every try was given up
func (a *Agent) watched(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine, request *contexts.Request,
	restore func()) (*contexts.Result, error) {
	idle := time.Duration(max(settings.GetInt(contextsconfig.IdleTimeout), 0)) * time.Second
	ceiling := time.Duration(max(settings.GetInt(contextsconfig.TotalCeiling), 0)) * time.Second
	if ceiling <= 0 {
		ceiling = 600 * time.Second
	}
	if idle <= 0 {
		return engine.Compress(ctx, env, request)
	}
	ceiling = max(ceiling, idle)
	if budget := time.Duration(max(settings.GetInt("auxiliary.compression.timeout"), 0)) * time.Second; budget > 0 {
		ceiling = max(ceiling, budget)
		idle = max(idle, min(budget, ceiling))
	}
	deadline := time.Now().Add(ceiling)
	result, total, observed, err := a.attempt(ctx, env, engine, request, idle, ceiling)
	if err != nil || result != nil {
		if result != nil {
			r.mu.Lock()
			r.cx.stalls = 0
			r.mu.Unlock()
		}
		return result, err
	}
	if restore != nil {
		restore()
	}
	r.mu.Lock()
	escalate := r.cx.stalls > 0
	r.cx.stalls++
	r.mu.Unlock()
	if remaining := time.Until(deadline); remaining > 0 {
		slog.Warn(fmt.Sprintf("Context compression stalled on the configured summary route — retrying once on fallback_chain[0] within %.1fs of the shared "+
			"pre-commit budget before continuing without compression", remaining.Seconds()))
		retry := *request
		retry.Entry, retry.Bypass = true, true
		got, _, _, err := a.attempt(ctx, env, engine, &retry, min(idle, remaining), remaining)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, err
			}
			slog.Warn("Context compression fallback attempt on fallback_chain[0] failed", "error", err)
		case got != nil && got.Record.Method != "none":
			slog.Info("Context compression recovered on fallback_chain[0] after the primary summary route stalled")
			return got, nil
		case got != nil:
			slog.Warn("Context compression fallback attempt on fallback_chain[0] produced no compression; continuing without compression")
		}
		if restore != nil {
			restore()
		}
	} else {
		slog.Info("Skipping compression stall fallback on fallback_chain[0]: the shared pre-commit ceiling is spent")
	}
	if escalate {
		slog.Warn("Context compression stalled on every summary route — committing the deterministic summary (no summary model) before continuing " +
			"without compression")
		retry := *request
		retry.Deterministic, retry.Bypass = true, true
		got, _, _, err := a.attempt(ctx, env, engine, &retry, ceiling, ceiling)
		if err != nil && ctx.Err() != nil {
			return nil, err
		}
		if err == nil && got != nil && got.Record.Method != "none" {
			slog.Warn("Context compression committed a deterministic fallback summary on deterministic after the primary summary route stalled (no summary " +
				"model produced output)")
			return got, nil
		}
		if restore != nil {
			restore()
		}
	}
	waited := ceiling - time.Until(deadline)
	r.mu.Lock()
	r.cx.timedOut = true
	r.mu.Unlock()
	a.waitsOn(r, "context compression timed out")
	if total {
		slog.Warn(fmt.Sprintf("Context compression reached its total ceiling after %.1fs (progress observed=%t); continuing without compression",
			waited.Seconds(), observed))
		engine.TimedOut("host compress_context total ceiling exhausted", "ceiling_exhausted", idle)
		after := ""
		if observed {
			after = " after summary output was observed"
		}
		a.tell(r, fmt.Sprintf("⚠ Context compression reached its total ceiling after %.1fs%s. No messages were dropped — continuing without compression. "+
			"Run /compress to retry or /new for a clean session.", waited.Seconds(), after))
		return nil, nil
	}
	slog.Warn(fmt.Sprintf("Context compression made no progress for %.1fs (total wait %.1fs, ceiling %.1fs); continuing without compression", idle.Seconds(),
		waited.Seconds(), ceiling.Seconds()))
	engine.TimedOut("host compress_context timeout (no summary progress)", "stalled", idle)
	a.tell(r, fmt.Sprintf("⚠ Context compression timed out after %.1fs with no output from the summary model. No messages were dropped — continuing "+
		"without compression. Run /compress to retry, /new for a clean session, or check auxiliary.compression.", idle.Seconds()))
	return nil, nil
}

// One compression run against the idle limit and the ceiling: its result, or nil once given up with whether the ceiling gave it up and
// whether the summary made any headway
func (a *Agent) attempt(ctx context.Context, env *contexts.Env, engine contexts.Engine, request *contexts.Request, idle, ceiling time.Duration) (
	*contexts.Result, bool, bool, error) {
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	var last atomic.Int64
	var observed atomic.Bool
	last.Store(time.Now().UnixNano())
	watched := *env
	watched.Progress = func() {
		last.Store(time.Now().UnixNano())
		observed.Store(true)
	}
	type outcome struct {
		result *contexts.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := engine.Compress(running, &watched, request)
		done <- outcome{result, err}
	}()
	started := time.Now()
	total := false
	for {
		quiet := time.Since(time.Unix(0, last.Load()))
		wait := max(min(idle-quiet, ceiling-time.Since(started)), 5*time.Millisecond)
		timer := time.NewTimer(wait)
		select {
		case got := <-done:
			timer.Stop()
			if got.err != nil && errors.Is(got.err, context.DeadlineExceeded) && ctx.Err() == nil {
				slog.Info(fmt.Sprintf("Context compression worker exited with %v — taking the stall path", got.err))
				return nil, false, observed.Load(), nil
			}
			return got.result, false, observed.Load(), got.err
		case <-ctx.Done():
			timer.Stop()
			cancel()
			select {
			case <-done:
			case <-time.After(cancelGrace):
			}
			return nil, false, observed.Load(), ctx.Err()
		case <-timer.C:
		}
		waited := time.Since(started)
		quiet = time.Since(time.Unix(0, last.Load()))
		if waited >= ceiling {
			total = true
			break
		}
		if quiet >= idle {
			break
		}
		slog.Info(fmt.Sprintf("Context compression still streaming after %.0fs (last progress %.1fs ago) — extending wait (ceiling %.0fs)", waited.Seconds(),
			quiet.Seconds(), ceiling.Seconds()))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(cancelGrace):
		slog.Warn(fmt.Sprintf("Cancelled compression worker did not exit within %.1fs grace — orphaning it behind the poison fence (late result will be "+
			"discarded); retaining the session compression lease until it exits so no new attempt overlaps it", cancelGrace.Seconds()))
		<-done
	}
	return nil, total, observed.Load(), nil
}

// Why a compression's result is not to be stored: the engine aborted it, it changed nothing or left nothing; empty when it may be stored
func (a *Agent) rejected(r *run, engine contexts.Engine, input []*sessionsv1.Message, result *contexts.Result) string {
	switch {
	case result.Aborted:
		failure := cmp.Or(result.Failure, "unknown error")
		r.mu.Lock()
		repeat := r.cx.told == failure
		r.cx.told = failure
		r.mu.Unlock()
		if !repeat {
			a.tell(r, fmt.Sprintf("⚠ Compression aborted: %s. No messages were dropped — conversation continues unchanged. Run /compress to retry, or /new "+
				"to start a fresh session.", failure))
		}
		engine.Settled(contexts.Outcome{})
		return "aborted"
	case same(input, result.Messages):
		slog.Info(fmt.Sprintf("Compression made no progress (session=%s) — skipping boundary rewrite.", r.sessionID()))
		engine.Settled(contexts.Outcome{Unchanged: true})
		return "unchanged"
	case len(result.Messages) == 0:
		slog.Error(fmt.Sprintf("context compression returned an empty transcript; refusing to rotate session=%s so the parent remains resumable", r.sessionID()))
		a.tell(r, "⚠ Compression returned an empty transcript. No session split was performed; conversation continues unchanged.")
		engine.Settled(contexts.Outcome{})
		return "empty"
	}
	return ""
}

// Tells the session once that the summary fell back to a built one, or that the summary model failed and the main model stood in
func (a *Agent) fellBack(r *run, result *contexts.Result) {
	var text string
	switch {
	case result.Failure != "":
		text = fmt.Sprintf("⚠ Compression summary failed: %s. Inserted a fallback context marker.", result.Failure)
	case result.AuxModel != "":
		slog.Warn(fmt.Sprintf("Configured compression model %s failed (%s); recovered using the main model.", result.AuxModel,
			cmp.Or(result.AuxFailure, "unknown error")))
		text = fmt.Sprintf("ℹ Configured compression model '%s' failed, so Icebath summarised with your main model instead. Check "+
			"auxiliary.compression.model in your config.", result.AuxModel)
	default:
		return
	}
	r.mu.Lock()
	repeat := r.cx.told == text
	r.cx.told = text
	r.mu.Unlock()
	if !repeat {
		a.tell(r, text)
	}
}

// Whether two lists hold the same messages
func same(a, b []*sessionsv1.Message) bool {
	return slices.EqualFunc(a, b, func(x, y *sessionsv1.Message) bool { return proto.Equal(x, y) })
}

// Start of the chat to compress and the end kept whole: from the keep-th last message of the person on; the whole chat and nothing kept
// when that would leave nothing to compress
func splitKeep(history []*sessionsv1.Message, keep int) ([]*sessionsv1.Message, []*sessionsv1.Message) {
	boundary, seen := 0, 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].GetRole() == sessionsv1.Role_ROLE_USER {
			boundary, seen = i, seen+1
			if seen >= max(keep, 1) {
				break
			}
		}
	}
	if boundary == 0 {
		return history, nil
	}
	return history[:boundary], history[boundary:]
}

// Compressed start followed by the end kept whole, a seam of two messages of the same side merged or bridged
func rejoin(head, tail []*sessionsv1.Message) []*sessionsv1.Message {
	if len(head) == 0 {
		return slices.Clone(tail)
	}
	out := slices.Clone(head)
	last, first := out[len(out)-1], tail[0]
	rest := tail
	if role := last.GetRole(); (role == sessionsv1.Role_ROLE_USER || role == sessionsv1.Role_ROLE_ASSISTANT) && role == first.GetRole() {
		if len(last.GetToolCalls()) == 0 && len(first.GetToolCalls()) == 0 && len(last.GetMedia()) == 0 && len(first.GetMedia()) == 0 {
			merged := contexts.Rewritten(last, contexts.Text(last)+"\n\n"+contexts.Text(first))
			merged.Absorbed = append(merged.Absorbed, first.GetUid())
			out[len(out)-1] = merged
			rest = tail[1:]
		} else {
			bridge := sessionsv1.Role_ROLE_USER
			if first.GetRole() == sessionsv1.Role_ROLE_USER {
				bridge = sessionsv1.Role_ROLE_ASSISTANT
			}
			out = append(out, &sessionsv1.Message{Role: bridge, ModelOnly: true, Display: "hidden"})
		}
	}
	return append(out, rest...)
}

// The compacted list with the newest reply the person was shown put back where the engine folded it away, so the answer just given stays
// in the context; as it is when the reply is there, its place can't be found or it would stop the compaction shrinking the transcript
func keepReply(original, list []*sessionsv1.Message, session string) []*sessionsv1.Message {
	at := -1
	for i := len(original) - 1; i >= 0; i-- {
		m := original[i]
		if m.GetRole() == sessionsv1.Role_ROLE_ASSISTANT && len(m.GetToolCalls()) == 0 && !contexts.IsSummary(m) && strings.TrimSpace(contexts.Text(m)) != "" {
			at = i
			break
		}
	}
	if at < 0 {
		return list
	}
	reply := original[at]
	text := strings.TrimSpace(contexts.Text(reply))
	twin := func(m *sessionsv1.Message) bool {
		return m.GetRole() == sessionsv1.Role_ROLE_ASSISTANT && len(m.GetToolCalls()) == 0 && strings.TrimSpace(contexts.Text(m)) == text
	}
	if slices.ContainsFunc(list, twin) {
		return list
	}
	// The reply goes before the first of the messages after it in the original that survived
	survived := map[string]int{}
	for i, m := range list {
		if uid := m.GetUid(); uid != "" {
			if _, seen := survived[uid]; !seen {
				survived[uid] = i
			}
		}
	}
	index := len(list)
	for _, m := range original[at+1:] {
		if i, ok := survived[m.GetUid()]; ok && m.GetUid() != "" {
			index = i
			break
		}
	}
	for _, i := range []int{index - 1, index} {
		if i >= 0 && i < len(list) && list[i].GetRole() == sessionsv1.Role_ROLE_ASSISTANT {
			slog.Warn("Compression: engine folded away the just-delivered assistant reply and left a non-matching assistant row at its slot; not " +
				"reinserting it to keep strict role alternation (session=" + session + ").")
			return list
		}
	}
	anchor := contexts.Copy(reply)
	in, out := contexts.Estimate(original), contexts.Estimate(list)
	if out < in && in < out+contexts.Estimate([]*sessionsv1.Message{anchor}) {
		slog.Info("Compression: not reinserting the just-delivered assistant reply; it would stop this compaction from shrinking the transcript " +
			"(session=" + session + ").")
		return list
	}
	slog.Info(fmt.Sprintf("Compression: engine folded away the just-delivered assistant reply; reinserted it into the active set (session=%s).", session))
	return slices.Insert(slices.Clone(list), index, anchor)
}

// A compaction that came out larger than what it replaces shrunk under that by its engine, the state carried after it cut down to its
// skill reload notice when that is not enough; nil when it can't be brought under it or would lose the person's turn
func salvage(engine contexts.Engine, original, candidate []*sessionsv1.Message, heads []string, budget int) []*sessionsv1.Message {
	salvager, ok := engine.(contexts.Salvager)
	if !ok {
		return nil
	}
	out := salvager.Salvage(original, candidate, budget)
	if out == nil {
		return nil
	}
	if contexts.Estimate(out) >= budget && len(heads) > 0 {
		for i := len(out) - 1; i >= 0; i-- {
			m := out[i]
			if m.GetRole() != sessionsv1.Role_ROLE_USER || !slices.ContainsFunc(heads, func(head string) bool { return strings.Contains(contexts.Text(m), head) }) {
				continue
			}
			text := contexts.Text(m)
			cut := len(text)
			for _, head := range heads {
				if at := strings.Index(text, head); at >= 0 {
					cut = min(cut, at)
				}
			}
			kept := strings.TrimSpace(text[:cut])
			if at := strings.Index(text, contexts.SkillReloadHeader); at >= 0 {
				kept = strings.TrimSpace(kept + "\n\n" + text[at:])
			}
			out = slices.Clone(out)
			if kept == "" && len(m.GetMedia()) == 0 {
				out = slices.Delete(out, i, i+1)
			} else {
				out[i] = contexts.Rewritten(m, kept)
			}
			break
		}
	}
	if !slices.ContainsFunc(out, func(m *sessionsv1.Message) bool { return m.GetRole() == sessionsv1.Role_ROLE_USER }) || contexts.Estimate(out) >= budget {
		return nil
	}
	return out
}

// The compacted list with what the session's tools keep put back after it, such as its task list, and a note naming the skills it
// pruned; older copies of those texts replaced. The first lines of the texts carried, which mark them
func (a *Agent) carry(env *contexts.Env, list []*sessionsv1.Message) ([]*sessionsv1.Message, []string) {
	carried := a.tools.Carried(&tools.Env{Config: env.Config, Window: env.Window}, env.Session)
	if len(carried) == 0 {
		return list, nil
	}
	heads := make([]string, len(carried))
	for i, text := range carried {
		heads[i], _, _ = strings.Cut(text, "\n")
	}
	list = slices.Clone(list)
	for i := len(list) - 1; i >= 0; i-- {
		m := list[i]
		if m.GetRole() != sessionsv1.Role_ROLE_USER {
			continue
		}
		text := contexts.Text(m)
		stripped := text
		for _, head := range heads {
			if at := strings.Index(stripped, head); at >= 0 {
				stripped = strings.TrimSpace(stripped[:at])
			}
		}
		if stripped == text {
			continue
		}
		if stripped == "" && len(m.GetMedia()) == 0 {
			list = slices.Delete(list, i, i+1)
		} else {
			list[i] = contexts.Rewritten(m, stripped)
		}
	}
	block := strings.Join(carried, "\n\n")
	if notice := contexts.SkillReloadNotice(list); notice != "" {
		block += "\n\n" + notice
	}
	if n := len(list); n > 0 && contexts.RealUser(list[n-1]) {
		list[n-1] = contexts.Rewritten(list[n-1], contexts.Text(list[n-1])+"\n\n"+block)
		return list, heads
	}
	return append(list, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: block, ModelOnly: true, Display: "hidden"}), heads
}

// The compacted list keeping a turn of the person: as it is when it holds one, else with the newest one of the original put back without
// breaking the turns' alternation, else with the marker that tells the model to go on
func withUserTurn(original, list []*sessionsv1.Message) []*sessionsv1.Message {
	for _, m := range list {
		if contexts.RealUser(m) || contexts.IsSummary(m) && strings.Contains(contexts.Text(m), "[Restating the in-flight user request") {
			return list
		}
	}
	var anchor *sessionsv1.Message
	for i := len(original) - 1; i >= 0; i-- {
		if contexts.RealUser(original[i]) {
			anchor = contexts.Copy(original[i])
			break
		}
	}
	if anchor == nil {
		return append(slices.Clone(list), &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: contexts.Continuation, ModelOnly: true, Display: "hidden"})
	}
	list = slices.Clone(list)
	for i, m := range list {
		if m.GetRole() == sessionsv1.Role_ROLE_ASSISTANT && (i == 0 || list[i-1].GetRole() != sessionsv1.Role_ROLE_USER) {
			return slices.Insert(list, i, anchor)
		}
	}
	n := len(list)
	if n == 0 || list[n-1].GetRole() != sessionsv1.Role_ROLE_USER || contexts.IsSummary(list[n-1]) {
		return append(list, anchor)
	}
	merged := contexts.Rewritten(list[n-1], strings.TrimSpace(contexts.Text(anchor)+"\n\n"+contexts.Text(list[n-1])))
	merged.Absorbed = append(merged.Absorbed, anchor.GetUid())
	list[n-1] = merged
	return list
}

// Stores the compacted list: in place of the session's messages up to the one given, or in a new session continuing it; the id of the
// new session, empty for in place
func (a *Agent) commit(ctx context.Context, r *run, settings *config.Config, store sessions.Store, env *contexts.Env, history,
	list []*sessionsv1.Message, holder string) (string, error) {
	through := history[len(history)-1].GetId()
	inPlace := settings.GetBool(contextsconfig.InPlace)
	stored := storable(history, list, inPlace)
	guard := sessions.Guard{Compression: holder}
	r.mu.Lock()
	turn := r.busy
	r.mu.Unlock()
	if turn {
		guard.Turn = a.holder(r)
	}
	if inPlace {
		return "", store.Compact(ctx, env.Session, through, stored, guard)
	}
	child, err := store.Continue(ctx, env.Session, &sessionsv1.Session{Model: env.Route.Model}, stored)
	if err != nil {
		return "", err
	}
	if err := a.adopt(ctx, r, child.GetId()); err != nil {
		return child.GetId(), err
	}
	return child.GetId(), nil
}

// Messages to store in place of the session's: those at the start the same as stored kept by their ids, copies of the rest for the model
// alone when the person still sees the originals; copies of all of them for a new session
func storable(history, list []*sessionsv1.Message, inPlace bool) []*sessionsv1.Message {
	out := make([]*sessionsv1.Message, len(list))
	same := inPlace
	for i, m := range list {
		if same = same && i < len(history) && proto.Equal(m, history[i]); same {
			out[i] = m
			continue
		}
		copied := contexts.Copy(m)
		copied.Id, copied.Time = 0, nil
		if inPlace {
			copied.ModelOnly = true
		}
		out[i] = copied
	}
	return out
}

// Moves the run onto the session its conversation goes on in after a compression continued it there: the turn lease, the approvals and
// the windows watching it go with it
func (a *Agent) adopt(ctx context.Context, r *run, next string) error {
	store, err := a.store()
	if err != nil {
		return err
	}
	r.mu.Lock()
	previous, turn := r.sessionID(), r.busy
	r.mu.Unlock()
	if previous == next {
		return nil
	}
	if turn {
		if acquired, err := store.AcquireTurn(ctx, next, a.holderOf(next), leaseTTL); err != nil {
			return err
		} else if !acquired {
			return fmt.Errorf("Another Icebath process holds session %s that this conversation continued in.", next)
		}
	}
	yolo := a.approvals.Yolo(previous)
	a.mu.Lock()
	delete(a.runs, previous)
	a.runs[next] = r
	if started, ok := a.life.turns[previous]; ok {
		delete(a.life.turns, previous)
		a.life.turns[next] = started
	}
	a.mu.Unlock()
	r.mu.Lock()
	r.session.Store(&next)
	r.system, r.kept = nil, nil
	r.mu.Unlock()
	a.approvals.Restore(next, yolo)
	a.approvals.Watch(next, func() {
		event := approvalsEvent(a.approvals.Pending(next))
		r.mu.Lock()
		defer r.mu.Unlock()
		r.publish(event)
	})
	a.approvals.Unwatch(previous, "The conversation went on in a new session after its context was compressed.")
	if a.contexts != nil {
		a.contexts.Move(previous, next)
	}
	if turn {
		if err := store.ReleaseTurn(context.WithoutCancel(ctx), previous, a.holderOf(previous)); err != nil {
			slog.Debug("Could not release the turn lease of the compressed session", "session", previous, "error", err)
		}
	}
	slog.Info(fmt.Sprintf("Compression continued session %s in %s", previous, next))
	r.mu.Lock()
	r.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Continued{Continued: next}})
	r.mu.Unlock()
	return nil
}

// What follows a stored compaction: the prompt built again, the tools told the model no longer holds what they showed, and the person
// shown where the context was compacted
func (a *Agent) compacted(ctx context.Context, r *run, settings *config.Config, store sessions.Store, session string) {
	if _, err := store.RefreshPrompts(ctx, session); err != nil {
		slog.Debug("Could not ask for the session's prompt to be built again", "session", session, "error", err)
	}
	r.mu.Lock()
	r.system, r.kept = nil, nil
	r.cx.rebuild = true
	r.mu.Unlock()
	a.anchor(r, nil)
	a.tools.Compacted(settings, session)
	a.changed(r)
	a.activity(r, settings, compactionDone, false)
}

// Content free record of one compression attempt, written to the log
func (a *Agent) telemetry(env *contexts.Env, ask compressing, result *contexts.Result, status, failure string, before, after int) {
	record := result.Record
	payload := map[string]any{
		"attempt_id": record.Attempt, "session_id": env.Session, "trigger": ask.trigger, "manual": ask.force, "commit_status": status,
		"failure_class": cmp.Or(failure, record.Failure, "none"), "method": record.Method, "context_length": record.Window,
		"threshold_tokens": record.Threshold, "tokens_before": record.Before, "tokens_after": record.After, "middle_tokens": record.Middle,
		"head_tokens": record.Head, "tail_tokens": record.Tail, "messages_before": before, "messages_after": after, "prepared": record.Prepared,
		"aux_model": record.AuxModel, "aux_provider": record.AuxProvider, "tool_results_pruned": result.Pruned, "model": env.Route.Model,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	slog.Info("compression telemetry " + string(encoded))
}

// Failure of a summary with what may be secret in it masked, for the person to read
func maskedFailure(text string) string {
	return redact.Text(strings.TrimSpace(text), false, redact.Ends)
}
