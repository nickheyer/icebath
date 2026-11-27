package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	contextsconfig "github.com/nickheyer/icebath/internal/contexts/config"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Share of the known window below which a local server's refusal of the size is not explained by the request
	unexplainedShare = 0.5
	// Native compaction: the models that take it, the room left under the local trigger and the size used when nothing sets one
	nativeModel        = "gpt-5.6"
	nativeMargin       = 8192
	nativeDefaultLimit = 200000
	// Pause before a request compressed after a refusal goes again
	recoveryPause = 2 * time.Second
)

// Four or more digits in a row, which a refusal quoting the request's size carries
var quotedCount = regexp.MustCompile(`\p{Nd}{4,}`)

// What a turn does after a provider refused its request
type recovery int

const (
	// The refusal is of something else, for the rest of the turn to handle
	unrecovered recovery = iota
	// The request was made smaller, so it is sent again
	retried
	// The turn ends with the reply given
	ended
)

// Recovers a turn from a provider's refusal of its request for its size: the output cap clamped to the room the provider states, the
// window the provider names adopted, the conversation compressed, the pictures shrunk; the reply the turn ends with when nothing helps
func (a *Agent) recover(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine, failure error,
	request []*sessionsv1.Message, tokens int) (recovery, string, error) {
	refusal := contexts.Refused(failure, contexts.Load{Tokens: tokens, Window: env.Window, Messages: len(request)})
	apiErr, _ := errors.AsType[*provider.APIError](failure)
	message := ""
	if apiErr != nil {
		message = apiErr.Message
	}
	if nativeRejected(failure) {
		r.mu.Lock()
		off := r.cx.nativeOff
		r.cx.nativeOff = true
		r.mu.Unlock()
		if !off {
			a.tell(r, "⚠️  Provider rejected native compaction (context_management) — disabled for this session, local compression stays active. Retrying...")
			return retried, "", nil
		}
	}
	model := env.Route.Model
	switch refusal {
	case contexts.ImageTooLarge:
		r.mu.Lock()
		first := r.cx.shrink == 0
		if first {
			r.cx.shrink = contexts.ImageLimit(message)
		}
		r.mu.Unlock()
		if _, changed := contexts.ShrinkImages(request, contexts.ImageLimit(message)); first && changed {
			a.tell(r, "📐 Image(s) exceeded provider size limit — shrank and retrying...")
			return retried, "", nil
		}
		r.mu.Lock()
		r.cx.shrink = 0
		r.mu.Unlock()
		slog.Info("image-shrink recovery: no data-URL image parts found or shrink didn't reduce size; surfacing original error.")
		return unrecovered, "", nil
	case contexts.TooLarge:
		return a.recoverTooLarge(ctx, r, settings, env, engine, request, tokens, model)
	case contexts.Overflow, contexts.LongTier:
		return a.recoverLength(ctx, r, settings, env, engine, message, request, tokens, model, refusal)
	}
	return unrecovered, "", nil
}

// Counts one compression against the turn's attempts; the reply the turn ends with once they are spent
func (a *Agent) spend(r *run, settings *config.Config, tooLarge bool, model string) (string, bool) {
	limit := max(settings.GetInt(contextsconfig.MaxAttempts), 1)
	r.mu.Lock()
	r.cx.attempts++
	attempts := r.cx.attempts
	r.mu.Unlock()
	if attempts <= limit {
		return "", false
	}
	if tooLarge {
		a.tell(r, fmt.Sprintf("❌ Max compression attempts (%d) reached for payload-too-large error.\n%s", limit, overflowRetryHint))
		slog.Error(fmt.Sprintf("413 compression failed after %d attempts.", limit))
		return fmt.Sprintf(tooLargeCopy, model), true
	}
	a.tell(r, fmt.Sprintf("❌ Max compression attempts (%d) reached.\n%s", limit, overflowRetryHint))
	slog.Error(fmt.Sprintf("Context compression failed after %d attempts.", limit))
	return fmt.Sprintf(overflowCopy, model), true
}

// One compression of a refused request that waits for no cooldown; the reply the turn ends with when the compression was held off for a
// moment or timed out
func (a *Agent) squeeze(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine, tokens int,
	failOnTimeout bool) (compressed, string, error) {
	done, err := a.compress(ctx, r, settings, env, engine, compressing{trigger: "overflow", tokens: tokens, bypass: true})
	if err != nil {
		return done, "", err
	}
	if !done.stored {
		if reply, deferred := a.deferredReply(r); deferred {
			r.mu.Lock()
			r.cx.attempts--
			r.mu.Unlock()
			return done, reply, nil
		}
	}
	r.mu.Lock()
	timedOut := r.cx.timedOut
	r.mu.Unlock()
	if failOnTimeout && timedOut {
		return done, compressionTimeoutReply, nil
	}
	return done, "", nil
}

// Reply of a turn whose compression was held off for a moment, by another compression of the session or a cooldown, which does not count
// as running out of attempts; false when none was
func (a *Agent) deferredReply(r *run) (string, bool) {
	r.mu.Lock()
	deferred := r.cx.deferred
	r.mu.Unlock()
	switch {
	case strings.HasPrefix(deferred, "lock:"):
		slog.Info(fmt.Sprintf("turn deferred: compression lock held by another path (session=%s holder=%s) — not counting as compression exhaustion",
			r.sessionID(), strings.TrimPrefix(deferred, "lock:")))
		return lockedReply, true
	case strings.HasPrefix(deferred, "transient:"):
		slog.Info(fmt.Sprintf("turn deferred: compression transiently blocked (%s) (session=%s) — not counting as compression exhaustion",
			strings.TrimPrefix(deferred, "transient:"), r.sessionID()))
		return transientReply, true
	}
	return "", false
}

// Recovery from a body too large for the provider, the progress of the compression weighed in bytes
func (a *Agent) recoverTooLarge(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine,
	request []*sessionsv1.Message, tokens int, model string) (recovery, string, error) {
	if reply, spent := a.spend(r, settings, true, model); spent {
		return ended, reply, nil
	}
	r.mu.Lock()
	attempts := r.cx.attempts
	r.mu.Unlock()
	a.tell(r, fmt.Sprintf("⚠️  Request payload too large (413) — compression attempt %d/%d...", attempts, max(settings.GetInt(contextsconfig.MaxAttempts), 1)))
	before := payloadBytes(request)
	done, reply, err := a.squeeze(ctx, r, settings, env, engine, tokens, false)
	if err != nil || reply != "" {
		return ended, reply, err
	}
	after := payloadBytes(done.after)
	if done.stored && (len(done.after) < len(done.before) || after > 0 && float64(after) < float64(before)*0.95) {
		if len(done.after) < len(done.before) {
			a.tell(r, retryMessagesStatus(len(done.before), len(done.after)))
		} else {
			a.tell(r, fmt.Sprintf("🗜️ Compressed %s → %s payload bytes, retrying...", strs.Thousands(before), strs.Thousands(after)))
		}
		pause(ctx, recoveryPause)
		return retried, "", nil
	}
	r.mu.Lock()
	stripped := r.cx.stripTools
	r.cx.stripTools = true
	r.mu.Unlock()
	if !stripped && toolImages(request) {
		a.tell(r, "📐 Compression could not reduce the request further — removed retained vision payloads and retrying...")
		return retried, "", nil
	}
	a.tell(r, "❌ Payload too large and cannot compress further.\n"+overflowRetryHint)
	slog.Error("413 payload too large. Cannot compress further.")
	return ended, fmt.Sprintf(tooLargeCopy, model), nil
}

// Recovery from a request over the window: the output cap clamped when the provider states the room left, the window it names adopted,
// then the conversation compressed
func (a *Agent) recoverLength(ctx context.Context, r *run, settings *config.Config, env *contexts.Env, engine contexts.Engine, message string,
	request []*sessionsv1.Message, tokens int, model string, refusal contexts.Refusal) (recovery, string, error) {
	window := env.Window
	if room, ok := models.OutputBudget(message); ok {
		local := window - tokens
		safe := max(1, room-64)
		if local > 0 {
			safe = max(1, min(room, local)-64)
		}
		r.mu.Lock()
		r.cx.outputCap = safe
		r.mu.Unlock()
		a.tell(r, fmt.Sprintf("⚠️  Output cap too large for current prompt — retrying with max_tokens=%s (provider_available=%s, estimated_request_tokens=%s; "+
			"context_length unchanged at %s)", strs.Thousands(safe), strs.Thousands(room), strs.Thousands(tokens), strs.Thousands(window)))
		if reply, spent := a.spend(r, settings, false, model); spent {
			return ended, reply, nil
		}
		if _, reply, err := a.squeeze(ctx, r, settings, env, engine, tokens, false); err != nil {
			if ctx.Err() != nil {
				return ended, "", err
			}
			slog.Warn("Output-cap compression hit an error; retrying on max_tokens only.", "error", err)
		} else if reply != "" {
			return ended, reply, nil
		}
		return retried, "", nil
	}
	if models.OutputCapError(message) {
		a.tell(r, "❌ The provider rejected the request because the requested output length exceeds its output cap for this model, and the error did "+
			"not state the allowed limit.\n   💡 Icebath has no user setting for the output cap — check the endpoint's default max output (completion) "+
			"tokens for this model on the server or proxy. (This is an output-cap error, not a context overflow — compression cannot fix it.)")
		slog.Error("Output-cap error not routed into compression (max_tokens over provider cap): " + strs.Head(message, 200))
		return ended, "The requested output length exceeds the provider's output cap for this model, and the error did not state the allowed limit.", nil
	}
	adopted := 0
	if lower, ok := models.LowerContextLimit(message, window); ok && refusal == contexts.Overflow {
		a.tell(r, fmt.Sprintf("Context limit detected from API: %s tokens (was %s)", strs.Thousands(lower), strs.Thousands(window)))
		if host := a.models.Host(); host != nil {
			host.SaveContext(settings, env.Route.Model, env.Route.BaseURL, lower)
		}
		a.tell(r, fmt.Sprintf("⚠️  Context length exceeded — using provider limit: %s → %s tokens", strs.Thousands(window), strs.Thousands(lower)))
		adopted, env.Window = lower, lower
		engine.Bind(env)
		a.tell(r, retryWindowStatus(lower, window))
	} else if quoted, ok := models.ContextLimit(message); ok {
		a.tell(r, fmt.Sprintf("Provider reported a context limit of %s tokens, not below the %s assumed; keeping context_length at %s tokens.",
			strs.Thousands(quoted), strs.Thousands(window), strs.Thousands(window)))
	} else {
		a.tell(r, fmt.Sprintf("⚠️  Context length exceeded, but provider did not report a max context length; keeping context_length at %s tokens.",
			strs.Thousands(window)))
	}
	if models.Local(env.Route.BaseURL) && !quotedCount.MatchString(message) && env.Window > 0 && float64(tokens) < float64(env.Window)*unexplainedShare {
		a.tell(r, fmt.Sprintf("❌ The server rejected the request as too large, but it is only ~%s tokens against a %s-token window — not compressing.\n"+
			"   💡 Wait a moment and /retry; another request on the same server (e.g. a background review) may have been holding its context.",
			strs.Thousands(tokens), strs.Thousands(env.Window)))
		slog.Error(fmt.Sprintf("Provider context rejection not explained by request size (~%s of %s tokens); skipping compression: %s",
			strs.Thousands(tokens), strs.Thousands(env.Window), strs.Head(message, 200)))
		// The server may run the model with a smaller window than the one kept for it, so the next turn asks it again
		if host := a.models.Host(); host != nil {
			host.ForgetContext(settings, env.Route.Model, env.Route.BaseURL)
		}
		return ended, fmt.Sprintf(rejectionCopy, strs.Thousands(tokens), strs.Thousands(env.Window), model, model), nil
	}
	if !settings.GetBool(contextsconfig.Enabled) {
		return ended, fmt.Sprintf(disabledCopy, model), nil
	}
	if reply, spent := a.spend(r, settings, false, model); spent {
		return ended, reply, nil
	}
	r.mu.Lock()
	attempts := r.cx.attempts
	r.mu.Unlock()
	a.tell(r, retryTooLargeStatus(tokens, attempts, max(settings.GetInt(contextsconfig.MaxAttempts), 1)))
	done, reply, err := a.squeeze(ctx, r, settings, env, engine, tokens, true)
	if err != nil || reply != "" {
		return ended, reply, err
	}
	after := requestTokens(nil, done.after, nil)
	before := requestTokens(nil, done.before, nil)
	if done.stored && shrank(len(done.before), len(done.after), before, after) {
		if len(done.after) < len(done.before) {
			a.tell(r, retryMessagesStatus(len(done.before), len(done.after)))
		} else {
			a.tell(r, retryTokensStatus(before, after))
		}
	}
	if done.stored && shrank(len(done.before), len(done.after), before, after) || adopted > 0 && adopted < window {
		pause(ctx, recoveryPause)
		r.mu.Lock()
		r.cx.overflow = true
		r.mu.Unlock()
		return retried, "", nil
	}
	a.tell(r, fmt.Sprintf("❌ The conversation is too long for the model (%s tokens) and cannot be shrunk further.\n%s", strs.Thousands(after),
		overflowRetryHint))
	slog.Error(fmt.Sprintf("Context length exceeded: %s tokens. Cannot compress further.", strs.Thousands(after)))
	return ended, fmt.Sprintf(overflowCopy, model), nil
}

// Bytes of the messages as they are sent
func payloadBytes(messages []*sessionsv1.Message) int {
	total := 0
	for _, m := range messages {
		total += len(contexts.Text(m)) + len(m.GetReasoning())
		for _, call := range m.GetToolCalls() {
			total += len(call.GetArguments()) + len(call.GetName())
		}
		for _, media := range m.GetMedia() {
			total += len(media.GetData()) * 4 / 3
		}
	}
	return total
}

// Whether tool results of the messages carry pictures
func toolImages(messages []*sessionsv1.Message) bool {
	for _, m := range messages {
		if m.GetRole() == sessionsv1.Role_ROLE_TOOL && len(m.GetMedia()) > 0 {
			return true
		}
	}
	return false
}

// Messages with the pictures of tool results taken out, for a request that stays too large with them
func withoutToolImages(messages []*sessionsv1.Message) []*sessionsv1.Message {
	out := make([]*sessionsv1.Message, len(messages))
	for i, m := range messages {
		out[i] = m
		if m.GetRole() == sessionsv1.Role_ROLE_TOOL && len(m.GetMedia()) > 0 {
			copied := contexts.Copy(m)
			copied.Media = nil
			copied.Text = strings.TrimSpace(copied.GetText() + "\n[screenshot removed to fit the request]")
			out[i] = copied
		}
	}
	return out
}

func pause(ctx context.Context, wait time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

// The context_management field that asks the provider to compact the conversation itself, nil when the route does not take it or the
// settings do not ask for it
func nativeCompaction(settings *config.Config, route provider.Route) []map[string]any {
	if !settings.GetBool(contextsconfig.CodexNative) || !settings.GetBool(contextsconfig.Enabled) || settings.GetBool(contextsconfig.CheckpointRequired) ||
		route.Mode != provider.ModeCodexResponses || !strings.Contains(strings.ToLower(route.Model), nativeModel) {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(route.BaseURL))
	host := ""
	if err == nil {
		host = strings.ToLower(parsed.Hostname())
	}
	if host != "api.openai.com" && host != "chatgpt.com" {
		return nil
	}
	return []map[string]any{{"type": "compaction", "compact_threshold": max(settings.GetInt(contextsconfig.CodexCompactThreshold), 0)}}
}

// The compact threshold of native compaction: the one set, held under the local trigger with room to spare; the room under the trigger
// when none is set, the default when no trigger is known either
func nativeThreshold(set, trigger int) int {
	upper := 0
	if trigger > 0 {
		upper = max(1024, int(float64(trigger)*0.8))
		if trigger > nativeMargin {
			upper = max(1024, trigger-nativeMargin)
		}
	}
	switch {
	case set <= 0 && upper > 0:
		return upper
	case set <= 0:
		return nativeDefaultLimit
	case upper == 0:
		return set
	}
	return max(1024, min(set, upper))
}

// Fields of the request that ask the provider to compact the conversation itself, nil when it is not to
func (a *Agent) nativeFields(r *run, settings *config.Config, route provider.Route, engine contexts.Engine) map[string]any {
	r.mu.Lock()
	off := r.cx.nativeOff
	r.mu.Unlock()
	management := nativeCompaction(settings, route)
	if off || management == nil {
		return nil
	}
	trigger := 0
	if engine != nil {
		trigger = engine.Status().Threshold
	}
	management[0]["compact_threshold"] = nativeThreshold(settings.GetInt(contextsconfig.CodexCompactThreshold), trigger)
	return map[string]any{"context_management": management}
}

// Whether the provider refused the request for its context_management field
func nativeRejected(err error) bool {
	failure, ok := errors.AsType[*provider.APIError](err)
	if !ok || failure.StatusCode != 0 && failure.StatusCode != 400 {
		return false
	}
	text := strings.ToLower(failure.Message)
	if !strings.Contains(text, "context_management") && !strings.Contains(text, "compact_threshold") {
		return false
	}
	for _, marker := range []string{"unknown", "unsupported", "invalid", "unexpected", "not permitted", "not allowed", "unrecognized", "extra field",
		"no such", "bad request", "not supported"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
