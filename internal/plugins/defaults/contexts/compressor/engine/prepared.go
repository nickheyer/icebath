package engine

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/contexts"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const (
	// Share below the trigger where the provider's count starts a summary in the background
	prepareBand = 0.12
	// Wait after a background summary failed before another starts
	failedPassBackoff = 60 * time.Second
	// How often a compaction waiting on a background summary looks at it
	waitPoll = 500 * time.Millisecond
)

// Message as a background summary saw it: its shape, its durable id and a digest of its text where no id vouches for it
type identity struct{ shape, uid, digest string }

func identityOf(m *sessionsv1.Message) identity {
	var shape strings.Builder
	shape.WriteString(contexts.Role(m) + "|" + m.GetToolCallId() + "|" + m.GetToolName())
	for _, call := range m.GetToolCalls() {
		shape.WriteString("|" + call.GetId() + ":" + call.GetName())
	}
	id := identity{shape: shape.String(), uid: m.GetUid()}
	if id.uid == "" {
		sum := sha256.Sum256([]byte(contexts.Text(m) + "\x00" + argumentsText(m)))
		id.digest = hex.EncodeToString(sum[:])
	}
	return id
}

// Arguments of every call of a reply, run together
func argumentsText(m *sessionsv1.Message) string {
	var b strings.Builder
	for _, c := range m.GetToolCalls() {
		b.WriteString(c.GetArguments())
	}
	return b.String()
}

// Window a background summary covers: the session, its bounds and the messages up to its end
type window struct {
	session    string
	start, end int
	identities []identity
}

// Why the window can't take the place of the middle of the messages with the head ending at start, empty when it can
func (w window) mismatch(messages []*sessionsv1.Message, session string, start int) string {
	switch {
	case w.session != session:
		return "another session"
	case w.start != start:
		return fmt.Sprintf("head moved (%d -> %d)", w.start, start)
	case w.end > len(messages):
		return "transcript shorter than the prepared window"
	}
	for i := 0; i < min(len(w.identities), len(messages)); i++ {
		was, now := w.identities[i], identityOf(messages[i])
		switch {
		case now.shape != was.shape:
			return fmt.Sprintf("message %d changed shape", i)
		case was.uid != "" && now.uid != "":
			if was.uid != now.uid {
				return fmt.Sprintf("message %d is a different message (rewound or branched)", i)
			}
		case was.digest != now.digest:
			return fmt.Sprintf("message %d changed and has no durable id to vouch for it", i)
		}
	}
	return ""
}

// Summary a background pass made, with the window it covers and what its calls left
type preparedSummary struct {
	window
	summary string
	body    string
	model   string
}

// Background pass running, cancelled when what it covers is no longer wanted
type pass struct {
	window
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	reason string
	// Progress of the compaction waiting on the pass, which its streaming feeds
	progress func()
}

func (p *pass) stop(reason string) {
	p.mu.Lock()
	if p.reason == "" {
		p.reason = reason
	}
	p.mu.Unlock()
	p.cancel()
}

func (p *pass) touch() {
	p.mu.Lock()
	progress := p.progress
	p.mu.Unlock()
	if progress != nil {
		progress()
	}
}

// Summaries prepared ahead of the next compaction of one session
type preparedState struct {
	mu         sync.Mutex
	entry      *preparedSummary
	pending    *pass
	retryAfter time.Time
	// A memory provider hands compactions context of its own, which a summary prepared without it would lack
	needsMemory bool
	boundaries  int
}

// Drops the summary prepared and stops a pass running
func (p *preparedState) discard(reason string) {
	p.mu.Lock()
	entry, pending := p.entry, p.pending
	p.entry, p.pending = nil, nil
	if entry != nil || pending != nil {
		p.boundaries++
	}
	p.mu.Unlock()
	if pending != nil {
		pending.stop("session_boundary")
	}
	var dropped *window
	switch {
	case entry != nil:
		dropped = &entry.window
	case pending != nil:
		dropped = &pending.window
	}
	if dropped != nil {
		running := " (pass running)"
		if entry != nil {
			running = ""
		}
		slog.Info(fmt.Sprintf("prepared compaction: dropped session=%s window=%d-%d reason=%s%s", dropped.session, dropped.start, dropped.end, reason, running))
	}
}

// Whether no pass may start now for the session
func (c *compressor) prepareBlocked(env *contexts.Env, origin string) bool {
	st := c.prepared
	if st == nil || !c.s.enabled || c.s.micro || env.Native {
		return true
	}
	st.mu.Lock()
	needsMemory, retryAfter := st.needsMemory, st.retryAfter
	st.mu.Unlock()
	if needsMemory || origin == "turn-end" && env.Ending {
		return true
	}
	if c.awaitingReal || c.fallbackStreak >= 2 {
		return true
	}
	floor := int(float64(c.threshold) * (1 - prepareBand))
	if c.lastPrompt <= 0 || c.threshold <= 0 || c.lastPrompt < floor {
		return true
	}
	return time.Now().Before(c.cooldownUntil) || time.Now().Before(retryAfter)
}

// Starts summarising the middle in the background once the session nears the trigger and no summary is prepared yet
func (c *compressor) Prepare(ctx context.Context, env *contexts.Env, messages []*sessionsv1.Message, origin string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prepareBlocked(env, origin) {
		return
	}
	st := c.prepared
	st.mu.Lock()
	busy := st.pending != nil || st.entry != nil
	st.mu.Unlock()
	if busy {
		return
	}
	working, _ := c.pruneResults(messages, c.s.protectLast, c.tailBudget, pruneMinChars)
	working = dropBlankEchoes(working)
	start, end := c.middle(working)
	if start >= end {
		return
	}
	worker := c.worker(env)
	scan := worker.scanHandoffs(working, start, end, slices.Clone(working[start:end]))
	if scan.tailStart != end || len(scan.turns) == 0 {
		return
	}
	select {
	case c.slots <- struct{}{}:
	default:
		return
	}
	passCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	token := &pass{window: window{session: c.session, start: start, end: end, identities: identities(working, end)}, cancel: cancel, done: make(chan struct{})}
	st.mu.Lock()
	if st.pending != nil || st.entry != nil {
		st.mu.Unlock()
		cancel()
		<-c.slots
		return
	}
	st.pending = token
	st.mu.Unlock()
	focus := deriveFocus(working)
	turns := scan.turns
	passEnv := *env
	passEnv.Progress, passEnv.Notify = token.touch, nil
	go c.run(passCtx, &passEnv, worker, turns, focus, token, origin)
}

func identities(messages []*sessionsv1.Message, end int) []identity {
	out := make([]identity, 0, end)
	for _, m := range messages[:end] {
		out = append(out, identityOf(m))
	}
	return out
}

// Copy of the summarising state that keeps nothing with the session, for a pass in the background
func (c *compressor) worker(env *contexts.Env) *compressor {
	return &compressor{s: c.s, session: c.session, route: c.route, window: c.window, reserve: c.reserve, threshold: c.threshold, tailBudget: c.tailBudget,
		maxSummary: c.maxSummary, previous: c.previous, hasUser: c.hasUser, fallenBack: c.fallenBack, lastAuxModel: c.lastAuxModel, env: env, slots: c.slots}
}

// Runs the summary of the pass and keeps it while the pass still holds the slot
func (c *compressor) run(ctx context.Context, env *contexts.Env, worker *compressor, turns []*sessionsv1.Message, focus string, token *pass, origin string) {
	st := c.prepared
	var entry *preparedSummary
	started := time.Now()
	defer func() {
		st.mu.Lock()
		if st.pending == token {
			st.pending = nil
			if entry != nil {
				st.entry = entry
			} else {
				st.retryAfter = time.Now().Add(failedPassBackoff)
				if worker.cooldownUntil.After(st.retryAfter) {
					st.retryAfter = worker.cooldownUntil
				}
			}
		}
		st.mu.Unlock()
		close(token.done)
		<-c.slots
	}()
	slog.Info(fmt.Sprintf("prepared compaction: pass started session=%s origin=%s window=%d-%d turns=%d", token.session, origin, token.start, token.end, len(turns)))
	summary, err := worker.summarize(ctx, env, turns, focus, "", false, pinned{})
	cancelled := func() {
		token.mu.Lock()
		reason := token.reason
		token.mu.Unlock()
		slog.Info(fmt.Sprintf("prepared compaction: pass cancelled session=%s after %.1fs (%s)", token.session, time.Since(started).Seconds(), reason))
	}
	switch {
	case ctx.Err() != nil:
		cancelled()
	case err != nil:
		slog.Warn(fmt.Sprintf("prepared compaction: pass crashed session=%s", token.session), "error", err)
	case summary != "":
		entry = &preparedSummary{window: token.window, summary: summary, body: worker.previous, model: worker.lastAuxModel}
		slog.Info(fmt.Sprintf("prepared compaction: pass completed session=%s window=%d-%d in %.1fs", token.session, token.start, token.end,
			time.Since(started).Seconds()))
	default:
		slog.Warn(fmt.Sprintf("prepared compaction: pass failed session=%s after %.1fs: %s", token.session, time.Since(started).Seconds(),
			cmp.Or(worker.lastError, "summariser returned nothing")))
	}
}

// Summary prepared for the window of the messages starting at start, waiting for a pass still running that covers it; nil for a fresh
// compaction, one a memory provider adds context to, or none that fits
func (c *compressor) takePrepared(ctx context.Context, messages []*sessionsv1.Message, start int, fresh, memory bool) *preparedSummary {
	st := c.prepared
	if st == nil {
		return nil
	}
	if memory {
		st.mu.Lock()
		first := !st.needsMemory
		st.needsMemory = true
		st.mu.Unlock()
		if first {
			slog.Info("prepared compaction: off for this agent (memory provider supplies compaction context)")
		}
	}
	st.mu.Lock()
	entry, pending := st.entry, st.pending
	st.entry = nil
	boundaries := st.boundaries
	st.mu.Unlock()
	reason := ""
	if fresh || memory {
		reason = "fresh summary required"
	}
	if reason == "" && entry != nil {
		reason = entry.mismatch(messages, c.session, start)
	}
	if reason == "" || entry == nil {
		switch {
		case pending != nil && pending.mismatch(messages, c.session, start) == "" && reason == "" && entry == nil:
			entry, reason = c.await(ctx, pending)
			st.mu.Lock()
			crossed := st.boundaries != boundaries
			st.mu.Unlock()
			if entry == nil && crossed {
				reason = "running pass discarded"
			}
		case entry == nil && reason == "":
			reason = "no candidate"
			if pending != nil {
				reason = "running pass does not fit"
			}
		}
	}
	st.mu.Lock()
	if st.pending == pending {
		st.pending = nil
	}
	st.entry = nil
	st.mu.Unlock()
	if pending != nil {
		pending.stop("superseded")
	}
	if reason != "" || entry == nil {
		if entry != nil || pending != nil {
			slog.Info(fmt.Sprintf("prepared compaction: splice miss session=%s reason=%s", c.session, cmp.Or(reason, "none")))
		}
		return nil
	}
	slog.Info(fmt.Sprintf("prepared compaction: splice hit session=%s window=%d-%d", c.session, entry.start, entry.end))
	return entry
}

// Waits for the running pass, its streaming counted as this compaction's progress, until it ends or the compression is given up
func (c *compressor) await(ctx context.Context, pending *pass) (*preparedSummary, string) {
	progress := func() {}
	if c.env != nil && c.env.Progress != nil {
		progress = c.env.Progress
	}
	pending.mu.Lock()
	pending.progress = progress
	pending.mu.Unlock()
	defer func() {
		pending.mu.Lock()
		pending.progress = nil
		pending.mu.Unlock()
	}()
	slog.Info(fmt.Sprintf("prepared compaction: waiting for the running pass session=%s window=%d-%d", pending.session, pending.start, pending.end))
	c.mu.Unlock()
	defer c.mu.Lock()
	ticker := time.NewTicker(waitPoll)
	defer ticker.Stop()
	deadline := time.After(failureCooldown)
	for {
		select {
		case <-pending.done:
			st := c.prepared
			st.mu.Lock()
			entry := st.entry
			st.entry = nil
			st.mu.Unlock()
			if entry == nil || !slices.Equal(entry.identities, pending.identities) {
				return nil, "running pass failed"
			}
			return entry, ""
		case <-deadline:
			return nil, "running pass did not finish in time"
		case <-ctx.Done():
			return nil, "host cancelled the wait"
		case <-ticker.C:
		}
	}
}

// Leaves the compressor as an inline summary would have: the prepared summary the base of the next one
func (c *compressor) adopt(prepared *preparedSummary) {
	c.succeeded(prepared.body)
	c.lastAuxModel = cmp.Or(prepared.model, c.lastAuxModel)
	c.record.Prepared = true
}
