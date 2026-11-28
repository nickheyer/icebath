package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/auxiliary"
	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Told the model before a message that follows a spoken reply the person talked over
const speechInterrupted = "[Note: the user interrupted your previous spoken reply before it finished.]"

// Weakest effort every reasoning wire takes, sent in place of off to a route that cannot switch reasoning off
const floorEffort = "low"

// Endpoint the provider openai stands for when the profile has no provider of that name
const openAIBase = "https://api.openai.com/v1"

// Route and effort of a turn the person spoke. While the voice chat model runs it, the session's fallback state is set aside and put back as
// the turn ends, so nothing the voice chat model met outlives the turn
type voiceTurn struct {
	instance provider.Instance
	route    provider.Route
	effort   string
	bound    bool
	prior    *failover
	refused  refusals
}

// Message queued for a turn, the model told first when the person cut a spoken reply off; the lock held
func (r *run) said(text string, voice bool) queued {
	q := queued{text: text, voice: voice}
	if r.cut {
		r.cut = false
		q.text, q.display = speechInterrupted+"\n\n"+text, text
	}
	return q
}

// Task the session's model calls are kept under: the voice chat helper's while its model runs a turn the person spoke, else none
func (r *run) routeTask() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.voice != nil && r.voice.bound {
		return speechconfig.VoiceChat
	}
	return ""
}

func voiceKey(name string) string {
	return auxiliaryconfig.Key + "." + speechconfig.VoiceChat + "." + name
}

// Provider, model and endpoint the voice chat helper names for the turn, false when it follows the model the session runs on: auto or main
// with an endpoint is a custom endpoint, with a model the session's provider; a provider with no model takes its model for side jobs; the
// provider openai the profile has none of is the OpenAI endpoint
func (a *Agent) voiceTarget(settings *config.Config, aux *auxiliary.Client, live provider.Route) (string, string, string, bool, bool) {
	name := strings.ToLower(strings.TrimSpace(settings.GetString(voiceKey(auxiliaryconfig.Provider))))
	model := strings.TrimSpace(settings.GetString(voiceKey(auxiliaryconfig.Model)))
	base := strings.TrimSpace(settings.GetString(voiceKey(auxiliaryconfig.BaseURL)))
	auto := false
	switch name {
	case "", auxiliaryconfig.Auto, auxiliaryconfig.Main:
		switch {
		case base != "":
			name = auxiliaryconfig.Custom
		case model != "":
			name, auto = strings.ToLower(strings.TrimSpace(live.Provider)), true
		default:
			return "", "", "", false, false
		}
	}
	if model == "" && aux != nil {
		model = aux.Helper(name)
	}
	if name == "" || model == "" || base == "" && name == strings.ToLower(live.Provider) && model == live.Model {
		return "", "", "", false, false
	}
	if _, ok := a.provider(settings, name); !ok && name == "openai" {
		name, base = auxiliaryconfig.Custom, strings.TrimRight(cmp.Or(base, strings.TrimSpace(settings.Getenv("OPENAI_BASE_URL")), openAIBase), "/")
	}
	return name, model, base, auto, true
}

// Instance and route of the voice chat helper's entry: an endpoint with the key and wire its settings give, else the model on the provider
// named, a model shortcut landing where it says, with the helper's own key and wire over the provider's
func (a *Agent) voiceRoute(settings *config.Config, aux *auxiliary.Client, name, model, base string, auto bool, live provider.Route) (provider.Instance,
	provider.Route, error) {
	if base != "" {
		if aux == nil {
			return provider.Instance{}, provider.Route{}, errors.New("No helper models are set up.")
		}
		route, err := aux.Endpoint(speechconfig.VoiceChat, name, model, base)
		if err != nil {
			return provider.Instance{}, route, err
		}
		instance := provider.Instance{ID: route.Provider, Spec: provider.Spec{Name: route.Spec, BaseURL: route.BaseURL, Mode: route.Mode}, Known: true}
		if found, ok := a.providers.Instance(settings, route.Provider); ok {
			instance = found
		}
		return instance, route, nil
	}
	explicit := name
	if auto {
		explicit = ""
	}
	instance, route, err := a.routeModel(settings, explicit, live.Provider, model)
	if err != nil {
		return instance, route, err
	}
	key := strings.TrimSpace(settings.GetString(voiceKey(auxiliaryconfig.APIKey)))
	if env := strings.TrimSpace(settings.GetString(voiceKey(auxiliaryconfig.KeyEnv))); key == "" && env != "" {
		key = strings.TrimSpace(settings.Getenv(env))
	}
	if key != "" {
		route.APIKey, route.Auth = key, provider.AuthAPIKey
	}
	if mode := strings.TrimSpace(settings.GetString(voiceKey(auxiliaryconfig.APIMode))); provider.KnownMode(mode) {
		route.Mode = provider.APIMode(mode)
	}
	return instance, route, nil
}

// Binds the voice chat route and effort for the turn: the helper's entry when it names one other than the model the session runs on, it
// resolves and the conversation stays under the point its context engine compresses at on it, else the session's own with the person told
// once why; the helper's effort, off lifted to the weakest effort where the route cannot switch reasoning off
func (a *Agent) speakTurn(ctx context.Context, r *run, settings *config.Config, aux *auxiliary.Client, live provider.Route,
	measure func(provider.Route) int) {
	turn := &voiceTurn{}
	if name, model, base, auto, ok := a.voiceTarget(settings, aux, live); ok {
		instance, route, err := a.voiceRoute(settings, aux, name, model, base, auto, live)
		reason := ""
		switch {
		case err != nil:
			reason = strings.SplitN(err.Error(), "\n", 2)[0]
		case !a.voiceFits(ctx, r, settings, instance, route, aux, measure):
			reason = "this session is too long for it"
		}
		if reason != "" {
			a.warnVoice(r, name, model, live, reason)
		} else {
			turn.instance, turn.route, turn.bound = instance, route, true
			slog.Info(fmt.Sprintf("Voice turn routed to %s (%s)", route.Model, route.Provider))
		}
	}
	effort := strings.ToLower(strings.TrimSpace(settings.GetString(voiceKey(auxiliaryconfig.ReasoningEffort))))
	if task, ok := a.auxiliary.Task(speechconfig.VoiceChat); ok && effort == "" {
		effort = task.Effort
	}
	if provider.Rung(effort) < 0 {
		effort = ""
	}
	if effort == "none" {
		active := live
		if turn.bound {
			active = turn.route
		}
		effort = a.voiceFloor(ctx, settings, active)
	}
	turn.effort = effort
	if !turn.bound && turn.effort == "" {
		return
	}
	r.mu.Lock()
	if turn.bound {
		// A failure of the voice chat model walks the chain from where the session stands, and what the session's route turned down of
		// reasoning is not the voice chat model's
		turn.prior, turn.refused = r.failover, r.refused
		if prior := r.failover; prior != nil {
			r.failover = &failover{next: prior.next, backoff: prior.backoff}
		}
		r.refused = refusals{}
	}
	r.voice = turn
	r.mu.Unlock()
}

// Effort a turn asking for no reasoning sends on the route: the weakest effort where its model cannot switch reasoning off, as calls learned
// or its listing says, or a Responses route's weakest one; off where it can
func (a *Agent) voiceFloor(ctx context.Context, settings *config.Config, route provider.Route) string {
	thinking, known := a.models.Host().Thinking(ctx, settings, route)
	host := provider.Hostname(route.BaseURL)
	switch {
	case a.learned.Floored(settings.Profile(), route), known && thinking.Mandatory,
		route.Mode == provider.ModeAnthropicMessages && provider.MandatoryThinking(route.Model):
		return floorEffort
	case route.Mode == provider.ModeCodexResponses && host != "api.x.ai":
		return provider.ClampEffort("none", provider.ResponsesEfforts(route.Model, host == "api.openai.com"), nil)
	}
	return "none"
}

// Whether the conversation stays under the point the session's context engine compresses at once it is bound to the route
func (a *Agent) voiceFits(ctx context.Context, r *run, settings *config.Config, instance provider.Instance, route provider.Route, aux *auxiliary.Client,
	measure func(provider.Route) int) bool {
	engine := a.engine(r, a.contextEnv(ctx, r, settings, instance, route, aux))
	if engine == nil {
		return true
	}
	limit := engine.Status().Threshold
	return limit <= 0 || measure(route) < limit
}

// Logs a voice chat route that went unused, and tells the person once a session
func (a *Agent) warnVoice(r *run, name, model string, live provider.Route, reason string) {
	slog.Warn(fmt.Sprintf("auxiliary.%s route %s/%s not used: %s; the voice turn runs on %s/%s", speechconfig.VoiceChat, name, model, reason, live.Provider,
		live.Model))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warnedVoice {
		return
	}
	r.warnedVoice = true
	a.notify(r, fmt.Sprintf("⚠ The voice chat model %s on %s can't be used, so what you say is answered by %s. %s Check its model "+
		"with /settings voice chat.", model, name, live.Model, strs.Full(reason)))
}

// Puts the session's own route, fallback state, effort and what its route refused of reasoning back after a turn the person spoke; the
// voice chat model found unable to switch reasoning off is remembered, so later calls to it send the weakest effort at once
func (a *Agent) endVoice(r *run) {
	settings := a.settings(r)
	r.mu.Lock()
	v := r.voice
	r.voice = nil
	floored := false
	if v != nil && v.bound {
		floored = r.refused.floor
		r.failover, r.refused = v.prior, v.refused
	}
	r.mu.Unlock()
	if floored {
		a.learned.Floor(settings.Profile(), v.route)
	}
	if v != nil {
		a.changed(r)
	}
}

// Route and effort the session reports and keeps as its own: those it runs on, and through a turn the voice chat model runs, the route and
// effort it had before
func (a *Agent) own(r *run) (provider.Route, string) {
	effort := a.ownEffort(r)
	r.mu.Lock()
	v := r.voice
	var prior provider.Route
	if v != nil && v.bound && v.prior != nil {
		prior = v.prior.active
	}
	r.mu.Unlock()
	switch {
	case v == nil || !v.bound:
		_, route, _ := a.current(r)
		return route, effort
	case prior.Model != "":
		return prior, effort
	}
	_, route, _ := a.route(r)
	return route, effort
}
