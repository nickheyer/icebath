package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	agentprompt "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/prompt"
	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// System prompt a session sends, built on its first turn and kept, so a provider's prompt cache holds it whole from turn to turn, until
// what it was built for changes or it is refreshed
type system struct {
	// What it was built for: the profile, the provider and model, the directory, the surface, the posture and the tools
	key  string
	text string
	// Start of the text that stays the same from session to session, which a provider caching in blocks caches on its own
	static string
	// What the chosen personality or the user's own instructions add after it, as the settings say when the turn starts
	overlay string
	// Example messages sent after it
	prefill []*sessionsv1.Message
	// Count of prompt refreshes of the session it was built under; a higher one builds it again
	epoch int64
}

// What the prompt of a turn is shaped by besides the model and the tools: the surface the session is shown on and the coding focus
type shaping struct {
	surface string
	focus   bool
}

// Messages every request of the session starts with: the system prompt and what the chosen personality or the user's own instructions add
// to it, its Model and Provider lines naming the route a fallback runs on, then the prefill messages
func (s *system) messages(route, primary provider.Route) []*sessionsv1.Message {
	text := s.text
	if routeKey(route) != routeKey(primary) {
		text = renamed(text, route)
	}
	if text = strings.TrimSpace(text + "\n\n" + s.overlay); text == "" {
		return s.prefill
	}
	return append([]*sessionsv1.Message{{Role: sessionsv1.Role_ROLE_SYSTEM, Text: text}}, s.prefill...)
}

// System prompt of the session for the turn, built on its first turn, kept and stored, built again once what it was built for changes or
// a refresh asked for it; and what it was built for
func (a *Agent) system(ctx context.Context, r *run, settings *config.Config, session *sessionsv1.Session, instance provider.Instance, route provider.Route,
	definitions []map[string]any, shape shaping) (*system, *prompt.Env, error) {
	env := a.promptEnv(settings, session.GetId(), session.GetDirectory(), shape.surface, instance, route, definitions, session.GetCreated().AsTime())
	env.Focus, env.OneShot = shape.focus, r.oneshot
	r.mu.Lock()
	env.Skills = slices.Clone(r.skills)
	r.mu.Unlock()
	key := strings.Join([]string{settings.Profile(), env.Provider, env.Kind, env.Model, env.Cwd, env.Platform, fmt.Sprint(env.Focus, env.OneShot,
		env.IgnoreRules, settings.GetBool(agentconfig.PassSessionID)), strings.Join(env.Tools, ","), strings.Join(env.Skills, ",")}, "\x00")
	r.mu.Lock()
	built := r.system
	r.mu.Unlock()
	store, storeErr := a.store()
	var epoch int64
	if storeErr == nil {
		epoch, _ = store.PromptEpoch(ctx, r.sessionID())
	}
	if built != nil && built.epoch != epoch {
		built = nil
	}
	if built == nil && storeErr == nil {
		built = a.storedPrompt(ctx, store, settings, session, key, epoch, r.shell())
	}
	if built == nil || built.key != key {
		var notes []string
		env.Note = func(text string) { notes = append(notes, text) }
		env.Keep = func(name string, make func() string) string {
			r.mu.Lock()
			text, ok := r.kept[name]
			r.mu.Unlock()
			if ok {
				return text
			}
			text = make()
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.kept == nil {
				r.kept = map[string]string{}
			}
			if kept, ok := r.kept[name]; ok {
				return kept
			}
			r.kept[name] = text
			return text
		}
		sections, err := a.prompts.Sections(ctx, env)
		if err != nil {
			return nil, nil, err
		}
		prefill, err := agentprompt.Prefill(settings, r.shell())
		if err != nil {
			notes = append(notes, err.Error())
		}
		built = &system{key: key, text: prompt.Join(sections), static: prompt.Prefix(sections), prefill: prefill, epoch: epoch}
		if storeErr == nil {
			a.keepPrompt(ctx, store, r.sessionID(), built, env.Tools)
		}
		r.mu.Lock()
		r.system = built
		for _, note := range notes {
			a.notify(r, note)
		}
		r.mu.Unlock()
		// A prompt built rather than restored starts the session as its hooks know it
		a.hooks.Fire(ctx, settings, hooks.SessionStarted{Session: session.GetId(), Model: env.Model, Platform: env.Platform})
	}
	turn := *built
	turn.overlay = agentprompt.Overlay(settings, r.shell())
	if r.child != nil {
		// A subagent speaks with no personality of its own: what it adds is its task's brief
		turn.overlay = r.child.prompt
	}
	env.Note, env.Keep = nil, nil
	return &turn, env, nil
}

// What the prompt of a session is built for
func (a *Agent) promptEnv(settings *config.Config, id, directory, surface string, instance provider.Instance, route provider.Route, definitions []map[string]any, started time.Time) *prompt.Env {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		function, _ := definition["function"].(map[string]any)
		if name, _ := function["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return &prompt.Env{
		Config:        settings,
		SessionID:     id,
		Cwd:           directory,
		Provider:      route.Provider,
		Model:         route.Model,
		Tools:         names,
		Kind:          instance.Spec.Name,
		Platform:      surface,
		Started:       started,
		Definitions:   definitions,
		IgnoreRules:   settings.GetBool(agentconfig.IgnoreRules),
		ContextLength: a.window(a.ctx, settings, route),
	}
}

// Builds the prompt of the session again on its next turn, of every session for no id: the session the id or the start of it names,
// and how many sessions held a prompt
func (a *Agent) Refresh(id string) (string, int, error) {
	if id != "" {
		store, err := a.store()
		if err != nil {
			return "", 0, err
		}
		session, err := store.Resolve(a.ctx, a.config.Profile(), id)
		if err != nil {
			return "", 0, connect.NewError(connect.CodeNotFound, "No session matches "+id+".")
		}
		id = session.GetId()
	}
	a.mu.Lock()
	runs := slices.Collect(maps.Values(a.runs))
	a.mu.Unlock()
	count := 0
	for _, r := range runs {
		if id != "" && r.sessionID() != id {
			continue
		}
		r.mu.Lock()
		if r.system != nil {
			r.system = nil
			count++
		}
		r.mu.Unlock()
	}
	return id, count, nil
}

// What a new session of the profile in use, in the directory and shown on the surface, would be sent, for prompt-size
func (a *Agent) Preview(_ context.Context, directory, surface string) (*prompt.Env, map[string]string, error) {
	settings := a.config
	instance, route, err := a.routeModel(settings, "", settings.GetString(agentconfig.Provider), settings.GetString(agentconfig.Model))
	if err != nil {
		route = provider.Route{Provider: instance.ID, Model: settings.GetString(agentconfig.Model)}
	}
	focus := agentprompt.Focused(settings, surface, directory)
	toolEnv := &tools.Env{Cwd: directory, Config: settings, Platform: surface}
	definitions := tools.Definitions(a.offered(toolEnv, focus, nil), toolEnv)
	toolsets := map[string]string{}
	for _, def := range a.tools.Tools() {
		toolsets[def.Name] = def.Toolset
	}
	env := a.promptEnv(settings, "", directory, surface, instance, route, definitions, time.Now())
	env.Focus = focus
	return env, toolsets, nil
}

// Lists the personalities, or sets the one every session of the profile speaks with from its next message on
func (a *Agent) Personality(id, name string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	settings := a.settings(r)
	if name = strings.TrimSpace(name); name == "" {
		current := strings.ToLower(strings.TrimSpace(settings.GetString(agentconfig.Personality)))
		lines := []string{"**Available Personalities**", "", "- `none` — (no personality overlay)"}
		for _, personality := range agentprompt.Personalities(settings) {
			mark := ""
			if personality.Name == current {
				mark = " ✓"
			}
			lines = append(lines, "- `"+personality.Name+mark+"` — "+personality.Describe(50))
		}
		return strings.Join(append(lines, "", "Usage: `/personality <name>`"), "\n"), nil
	}
	personality, ok := agentprompt.Find(settings, name)
	if !ok {
		var names []string
		for _, known := range agentprompt.Personalities(settings) {
			names = append(names, "`"+known.Name+"`")
		}
		return "", errors.New("Unknown personality: `" + strings.ToLower(name) + "`\n\nAvailable: `none`, " + strings.Join(names, ", "))
	}
	var stored any
	if personality.Name != "" {
		stored = personality.Name
	}
	if err := settings.Set(agentconfig.Personality, stored); err != nil {
		return "", fmt.Errorf("Failed to save personality change: %w", err)
	}
	if personality.Name == "" {
		return "Personality cleared — using base agent behavior.\n_(takes effect on next message)_", nil
	}
	return "Personality set to **" + personality.Name + "**\n_(takes effect on next message)_", nil
}
