package server

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	contextsconfig "github.com/nickheyer/icebath/internal/contexts/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/tools"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Lines the session shows while its context is compacted and once it is done
const (
	compactionMarker  = "Compacting context"
	compactionStatus  = "🗜️ " + compactionMarker + " — summarizing earlier conversation so I can continue..."
	compactionDone    = "✓ Context compaction complete — continuing turn..."
	overflowRetryHint = "   💡 Try /new to start a fresh conversation, or /compress to retry compression."
	// Reply of a turn whose compression timed out while the request was over the window
	compressionTimeoutReply = "Context compression timed out without reducing this conversation. No messages were dropped. Start a fresh session " +
		"with /new, or check auxiliary.compression before retrying /compress."
	// Reply of a turn whose compaction left only the reference handoff to answer
	handoffSkipReply = "Context was compacted. The previous response is complete — awaiting your next message."
	// Failure texts a turn ends with when the conversation can't be made to fit
	overflowCopy = "This conversation has grown too long for %s to read, and Icebath couldn't shrink it enough automatically. Start a new " +
		"session with /new (your history is kept), or try /compress once more. Switching to a model with a bigger context window also works."
	tooLargeCopy = "This conversation (including attachments) has grown too large to send to %s, and Icebath couldn't shrink it enough " +
		"automatically. Start a new session with /new (your history is kept), or try /compress once more."
	disabledCopy = "This conversation is too long for %s and automatic shrinking is turned off in your settings (context.compression.enabled). " +
		"Run /compress to shrink it now, /new to start fresh, or pick a model with a bigger context window."
	rejectionCopy = "The model server rejected this request as too large, but this conversation is only about %s tokens — well under the " +
		"%s-token window Icebath knows for %s — so shrinking it would not help. Another request on the same server (for example a background " +
		"memory review from an earlier session) was probably holding its capacity, or the server runs %s with a smaller window than Icebath " +
		"assumes. Wait a moment and send /retry; if it keeps happening, check the server's context setting."
	// Replies of a turn whose compression was held off for a moment, which does not count against its attempts
	transientReply = "Context compression is temporarily paused after a recent failed attempt. Please retry in a moment — compression will " +
		"resume automatically (or run /compress to force a retry now)."
	lockedReply = "Context compression is already running for this session. Please retry in a moment — your next message will be processed " +
		"once the concurrent compression finishes."
)

func preflightStatus(tokens, threshold int) string {
	return fmt.Sprintf("📦 Preflight compression: ~%s tokens >= %s threshold. This may take a moment.", strs.Thousands(tokens), strs.Thousands(threshold))
}

func preAPIStatus(tokens int) string {
	return fmt.Sprintf("📦 Pre-API compression: ~%s tokens near the context/output limit. Compacting before the next model call.", strs.Thousands(tokens))
}

func idleStatus(idle time.Duration, tokens int) string {
	return fmt.Sprintf("💤 Resumed after %ds idle — compacting ~%s tokens before continuing.", int(idle.Seconds()), strs.Thousands(tokens))
}

func retryTooLargeStatus(tokens, attempt, limit int) string {
	return fmt.Sprintf("🗜️ Context too large (~%s tokens) — compressing (%d/%d)...", strs.Thousands(tokens), attempt, limit)
}

func retryMessagesStatus(before, after int) string {
	return fmt.Sprintf("🗜️ Compressed %d → %d messages, retrying...", before, after)
}

func retryTokensStatus(before, after int) string {
	return "🗜️ Compressed ~" + strs.Thousands(before) + " → ~" + strs.Thousands(after) + " tokens, retrying..."
}

func retryWindowStatus(window, old int) string {
	return "🗜️ Context reduced to " + strs.Thousands(window) + " tokens (was " + strs.Thousands(old) + "), retrying..."
}

// Notice of a session over the trigger whose compression is held off
func blockedWarning(tokens, threshold int, reason string) string {
	return "⚠ Context is over the compression threshold (~" + strs.Thousands(tokens) + " tokens >= " + strs.Thousands(threshold) + ") " +
		"but compression is currently blocked (" + reason + "). The model may stop responding. Run /new to start a fresh " +
		"session or /compress to retry immediately."
}

// How one session's context is kept within its model's window, beside its engine: what the turn spent on it and what it was told
type compaction struct {
	// Compressions the turn ran, which max_attempts bounds
	attempts int
	// Request size the last compression before a model call left, 0 when none ran since that call
	pressure int
	// The passes before a model call stopped shrinking the request, so the turn makes no more of them
	blocked bool
	// The provider refused the request for its size and it was compressed, so the next one is checked before it goes
	overflow bool
	// The last compression gave up for want of progress or time
	timedOut bool
	// Compressions in a row the watchdog gave up on, which no stored compression has cleared since
	stalls int
	// Why the last automatic compression did not run: lock:<holder> while another one runs, transient:<reason> while a cooldown or backoff
	// holds it off; empty when it ran
	deferred string
	// Kind of the last overflow warning, given once until compression runs again
	warned string
	// Last summary failure the session was told of
	told string
	// When the session last did anything, for the compaction of a session resumed after a long pause
	active time.Time
	// The provider refused its own compaction of the conversation, so this session no longer asks for it
	nativeOff bool
	// Output cap a refusal of too large an output clamped the next request to, 0 for none
	outputCap int
	// A compression of the session runs now, and the compaction status it shows
	running bool
	status  string
	// The prompt of the turn is to be built again, a compaction having changed what the model holds
	rebuild bool
	// Longest side the pictures of the turn's requests are shrunk to after a provider refused one as too large, 0 for none
	shrink int
	// The pictures of tool results are left out of the turn's requests, the request staying too large with them
	stripTools bool
	// The provider's count of the newest request with the messages it priced, which later sizes start from; nil when none holds since the
	// last change of the messages under it
	usage *contexts.Anchor
}

// Window of the route's model: the profile's own setting, else what icebath knows of the model
func (a *Agent) window(ctx context.Context, settings *config.Config, route provider.Route) int {
	if n := settings.GetInt(agentconfig.ContextLength); n > 0 {
		return n
	}
	if host := a.models.Host(); host != nil && route.Model != "" {
		return host.ContextLength(ctx, settings, route)
	}
	return 0
}

// What the session's engine works with for a step of its turn on the route
func (a *Agent) contextEnv(ctx context.Context, r *run, settings *config.Config, instance provider.Instance, route provider.Route,
	aux *auxiliary.Client) *contexts.Env {
	store, _ := a.store()
	location := time.Local
	if zone, _ := settings.Zone(); zone != nil {
		location = zone
	}
	r.mu.Lock()
	id, oneshot, native := r.sessionID(), r.oneshot, !r.cx.nativeOff
	r.mu.Unlock()
	return &contexts.Env{
		Config:  settings,
		Session: id,
		Store:   store,
		Aux:     aux,
		Route:   route,
		Window:  a.window(ctx, settings, route),
		Reserve: instance.Spec.DefaultMaxTokens,
		Measure: func(ctx context.Context, route provider.Route) int {
			if host := a.models.Host(); host != nil {
				return host.ContextLength(ctx, settings, route)
			}
			return 0
		},
		Location: location,
		Native:   native && nativeCompaction(settings, route) != nil,
		Ending:   oneshot,
		Notify:   func(text string) { a.tell(r, text) },
	}
}

// Engine of the session bound to the env, opened the first time; nil when no engine is registered
func (a *Agent) engine(r *run, env *contexts.Env) contexts.Engine {
	if a.contexts == nil {
		return nil
	}
	engine := a.contexts.Engine(env.Session)
	opened := engine == nil
	if opened {
		engine = a.contexts.Open(env)
		if env.Config.GetBool(contextsconfig.CheckpointRequired) {
			slog.Warn(fmt.Sprintf("%s is enabled but the active memory provider (no active provider) does not implement checkpoint API v2. Compress "+
				"will fail closed. Recover by set %s: false, or switch to a memory provider that implements checkpoint API v2.",
				contextsconfig.CheckpointRequired, contextsconfig.CheckpointRequired))
		}
	} else {
		engine.Bind(env)
	}
	if engine != nil {
		a.raisedNotice(r, env, engine)
	}
	return engine
}

// Tells the profile once that a capped Codex model raised the trigger, and how to opt out
func (a *Agent) raisedNotice(r *run, env *contexts.Env, engine contexts.Engine) {
	raised := engine.Status().Raised
	if raised[1] == 0 || !env.Config.GetBool(contextsconfig.Enabled) || !env.Config.GetBool(contextsconfig.CodexAutoraiseNotice) {
		return
	}
	model := env.Route.Model[strings.LastIndex(env.Route.Model, "/")+1:]
	state := fmt.Sprintf("%s:%d:%d", model, int(math.RoundToEven(raised[0]*100)), int(math.RoundToEven(raised[1]*100)))
	marker := filepath.Join(env.Config.ProfileDir(env.Config.Profile()), ".codex_gpt55_autoraise_notice")
	if seen, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(seen)) == state {
		return
	}
	limit := "272K"
	switch {
	case env.Window > 0:
		limit = fmt.Sprintf("%dK", int(math.RoundToEven(float64(env.Window)/1000)))
	case strings.HasPrefix(model, "gpt-5.3-codex-spark"):
		limit = "128K"
	}
	a.tell(r, fmt.Sprintf("ℹ Codex %s caps context at %s, so auto-compaction was raised to %d%% (from %d%%) to use more of the window before "+
		"summarizing.\n  Opt back out: icebath config set %s false", model, limit, int(math.RoundToEven(raised[1]*100)),
		int(math.RoundToEven(raised[0]*100)), contextsconfig.CodexAutoraise))
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err == nil {
		if err := os.WriteFile(marker, []byte(state), 0o644); err != nil {
			slog.Debug("Could not record the Codex autoraise notice", "error", err)
		}
	}
}

// Shows what the session does besides the reply, empty once it is done; a line marked as a row, and with progress notices on any line, is
// kept in the transcript as well
func (a *Agent) activity(r *run, settings *config.Config, text string, row bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if text == r.cx.status {
		return
	}
	r.cx.status = text
	r.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Activity{Activity: text}})
	if text != "" && (row || settings.GetBool(contextsconfig.ProgressNotices)) {
		a.notify(r, text)
	}
}

// Clears what the session showed it does besides the reply, once the turn goes on
func (a *Agent) settle(r *run, settings *config.Config) {
	a.activity(r, settings, "", false)
}

// Warns once per kind that the session is over the trigger while its compression is held off
func (a *Agent) warnBlocked(r *run, reason string, tokens, threshold int) {
	kind, _, _ := strings.Cut(cmp.Or(reason, "unknown"), ":")
	r.mu.Lock()
	if r.cx.warned == "blocked:"+kind {
		r.mu.Unlock()
		return
	}
	r.cx.warned = "blocked:" + kind
	r.mu.Unlock()
	if kind == "cooldown" || kind == "ineffective" {
		a.waitsOn(r, fmt.Sprintf("compression blocked (%s)", reason))
	}
	a.tell(r, blockedWarning(tokens, threshold, reason))
}

// Warns once per window that a session with compression off is over the model's window
func (a *Agent) warnUncompressed(r *run, tokens, window int) {
	key := fmt.Sprint("uncompressed:", window)
	r.mu.Lock()
	if r.cx.warned == key {
		r.mu.Unlock()
		return
	}
	r.cx.warned = key
	r.mu.Unlock()
	a.tell(r, fmt.Sprintf("⚠ This conversation (~%s tokens) is over the model's %s-token context window and automatic compression is off. Run "+
		"/compress to shrink it, or /new to start fresh.", strs.Thousands(tokens), strs.Thousands(window)))
}

// Lets the overflow warnings be given again, once compression runs
func (r *run) rearm() {
	r.mu.Lock()
	r.cx.warned = ""
	r.mu.Unlock()
}

// Records what the session waits on, which the session list shows
func (a *Agent) waitsOn(r *run, text string) {
	store, err := a.store()
	if err != nil {
		return
	}
	if err := store.Update(a.ctx, r.sessionID(), sessions.Change{Activity: &text}); err != nil {
		slog.Debug("Could not record what the session waits on", "session", r.sessionID(), "error", err)
	}
}

// Rough size of a request in tokens: the system prompt, the messages and the tool definitions
func requestTokens(system []*sessionsv1.Message, messages []*sessionsv1.Message, definitions []map[string]any) int {
	text := ""
	for _, m := range system {
		text += contexts.Text(m) + "\n"
	}
	return contexts.Size(text, messages, definitions)
}

// System messages of the request with the note that earlier turns were compacted, when the history holds a summary
func noted(system, history []*sessionsv1.Message) []*sessionsv1.Message {
	summarised := false
	for _, m := range history {
		if contexts.IsSummary(m) {
			summarised = true
			break
		}
	}
	if !summarised || len(system) == 0 || system[0].GetRole() != sessionsv1.Role_ROLE_SYSTEM || strings.Contains(system[0].GetText(), contexts.CompressionNote) {
		return system
	}
	out := append([]*sessionsv1.Message{{Role: sessionsv1.Role_ROLE_SYSTEM, Text: strings.TrimSpace(system[0].GetText() + "\n\n" + contexts.CompressionNote)}},
		system[1:]...)
	return out
}

// Whether a compression cut the request enough to count: fewer messages, or a twentieth fewer tokens
func shrank(messagesBefore, messagesAfter, tokensBefore, tokensAfter int) bool {
	return messagesAfter < messagesBefore || tokensBefore > 0 && float64(tokensAfter) < float64(tokensBefore)*0.95
}

// Whether another pass before a model call is worth it: still over the trigger after a pass that cut a twentieth of the request
func another(before, after, threshold int) bool {
	return after >= threshold && before > 0 && float64(after) < float64(before)*0.95
}

// Deletes the saved tool results of the profile older than its sessions are kept, and at least a day, that no transcript names any more:
// once for each profile a turn runs on in this process
func (a *Agent) sweep(settings *config.Config) {
	profile := settings.Profile()
	a.mu.Lock()
	done := a.swept[profile]
	if a.swept == nil {
		a.swept = map[string]bool{}
	}
	a.swept[profile] = true
	a.mu.Unlock()
	if done {
		return
	}
	store, err := a.store()
	if err != nil {
		return
	}
	retention := time.Duration(max(settings.GetInt(sessionsconfig.RetentionDays), 0)) * 24 * time.Hour
	a.working.Add(1)
	go func() {
		defer a.working.Done()
		tools.Sweep(settings, retention, func(name string) (bool, error) {
			hits, err := store.Search(a.ctx, &sessionsv1.SearchRequest{Query: `"` + name + `"`, Profile: profile, Limit: 1})
			return len(hits) > 0, err
		})
	}()
}
