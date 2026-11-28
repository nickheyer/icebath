package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What a session keeps for itself is kept as its own settings over its profile's, under the keys of the settings of new chats: the model
// with its provider, the reasoning effort, what enter does while the agent works, and the account of a provider it is held to. Every way to
// change one of them, a row of This chat, a command or the question of an account that ran out, ends in one of the functions below

// Settings the session keeps of its own now, by key
func (r *run) own() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overrides == nil {
		return map[string]any{}
	}
	return maps.Clone(r.overrides.Values)
}

// Keeps the values as the session's own settings, a nil value letting go of the key, and tells the windows
func (a *Agent) keepOwn(ctx context.Context, r *run, values map[string]any) error {
	store, err := a.store()
	if err != nil {
		return err
	}
	r.mu.Lock()
	merged := r.overrides.Merge(values, nil)
	r.mu.Unlock()
	if err := store.Update(ctx, r.sessionID(), merged.Change()); err != nil {
		return err
	}
	r.mu.Lock()
	r.overrides = merged
	r.mu.Unlock()
	a.changed(r)
	return nil
}

// The model in effect for the session as a row says it: the model and the provider that runs it
func (a *Agent) modelWords(r *run) string {
	instance, route, err := a.route(r)
	switch {
	case err != nil && instance.ID == "":
		return "no model"
	case route.Model == "":
		return "the default model of " + instance.Label()
	}
	return route.Model + " · " + instance.Label()
}

// Holds the session to a model: one of the provider named, a model shortcut, which says where it runs, or with no provider one of the
// provider in effect; none takes the provider's default model. With clear the session takes the model of new chats again. The note the
// control gives
func (a *Agent) SetModel(ctx context.Context, id, name, model string, clear bool) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	if clear {
		if err := a.keepOwn(ctx, r, map[string]any{agentconfig.Provider: nil, agentconfig.Model: nil}); err != nil {
			return "", err
		}
		return "Model is back to " + a.modelWords(r) + " in this chat, as in new chats.", nil
	}
	settings := a.settings(r)
	model = strings.TrimSpace(model)
	if name = strings.TrimSpace(name); name != "" {
		instance, ok := a.provider(settings, name)
		if !ok {
			return "", noProvider(settings.Profile(), name)
		}
		name = instance.ID
	}
	note := ""
	values := map[string]any{agentconfig.Model: model}
	found, shortcut := lookupAlias(settings, model)
	if !shortcut && name == "" && model != "" {
		current, _, _ := a.route(r)
		found, shortcut = a.switchAlias(settings, current, model)
	}
	switch {
	case shortcut && name == "":
		// The shortcut runs where it says, whatever provider the session had
		values[agentconfig.Model], values[agentconfig.Provider] = found.name, nil
	case shortcut:
		values[agentconfig.Model], values[agentconfig.Provider] = found.model, name
	default:
		// The model is of the provider in effect, held with it so a change of the provider of new chats leaves the chat as it is
		if current, _, _ := a.route(r); name == "" && current.ID != "" {
			if instance, ok := a.providers.Instance(settings, current.ID); ok {
				name = instance.ID
			}
		}
		if name != "" {
			values[agentconfig.Provider] = name
		}
		if model != "" {
			verdict := a.checkModel(ctx, r, name, model)
			if !verdict.Accepted {
				return "", errors.New(verdict.Message)
			}
			note = verdict.Message
		}
	}
	if err := a.keepOwn(ctx, r, values); err != nil {
		return "", err
	}
	said := "Model is now " + a.modelWords(r) + " in this chat."
	if note != "" {
		said += " " + note
	}
	return said, nil
}

// Efforts a person may choose, as a list said in words
func efforts() string {
	return strs.Or(provider.Efforts)
}

// Reasoning effort in words, as the settings name it; the model's default for none
func effortWords(effort string) string {
	if effort == "" {
		return "The model's default"
	}
	for _, option := range provider.EffortOptions(config.Option{}) {
		if option.Value == effort {
			return option.Title
		}
	}
	return effort
}

// Sets how hard the session's model thinks, or with clear lets it take the effort of new chats again. The note the control gives
func (a *Agent) SetEffort(ctx context.Context, id, effort string, clear bool) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	var value any
	if !clear {
		effort = strings.ToLower(strings.TrimSpace(effort))
		if !slices.Contains(provider.Efforts, effort) {
			return "", fmt.Errorf("Reasoning effort must be %s.", efforts())
		}
		value = effort
	}
	if err := a.keepOwn(ctx, r, map[string]any{agentconfig.ReasoningEffort: value}); err != nil {
		return "", err
	}
	now := effortWords(a.ownEffort(r))
	if clear {
		return "Reasoning effort is back to " + now + " in this chat, as in new chats.", nil
	}
	return "Reasoning effort is now " + now + " in this chat.", nil
}

// Sets what a message sent while the session's agent works does, or with clear lets it take the choice of new chats again. The note the
// control gives
func (a *Agent) SetBusy(ctx context.Context, id, mode string, clear bool) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	var value any
	if !clear {
		switch mode = strings.ToLower(strings.TrimSpace(mode)); mode {
		case agentconfig.BusyQueue, agentconfig.BusySteer, agentconfig.BusyInterrupt:
			value = mode
		default:
			return "", errors.New("Enter while busy must be queue, steer or interrupt.")
		}
	}
	if err := a.keepOwn(ctx, r, map[string]any{agentconfig.BusyInputMode: value}); err != nil {
		return "", err
	}
	now := agentconfig.BusyTitle(a.settings(r).GetString(agentconfig.BusyInputMode))
	if clear {
		return "Enter while busy is back to " + now + " in this chat, as in new chats.", nil
	}
	return "Enter while busy is now " + now + " in this chat.", nil
}

// What the session keeps for itself, with what is in effect for it, and whether it runs on the profile the asker shows
func (a *Agent) Chat(_ context.Context, id, profile string) (*agentv1.ChatResponse, error) {
	r, err := a.run(id)
	if err != nil {
		return nil, err
	}
	own := r.own()
	settings := a.settings(r)
	out := &agentv1.ChatResponse{Profile: r.profileName(), Effort: a.ownEffort(r), Busy: settings.GetString(agentconfig.BusyInputMode)}
	out.Shown = a.config.In(profile).Profile() == out.Profile
	_, out.OwnModel = own[agentconfig.Model]
	_, out.OwnEffort = own[agentconfig.ReasoningEffort]
	_, out.OwnBusy = own[agentconfig.BusyInputMode]
	instance, route, _ := a.route(r)
	out.Provider, out.ProviderTitle, out.Model = instance.ID, instance.Label(), route.Model
	if instance.ID == "" {
		return out, nil
	}
	out.Accounts = int32(len(instance.Accounts))
	if held, ok := own[loginKey(instance.ID)].(string); ok {
		out.Account = held
		out.AccountTitle = loginName(instance, held)
		if held == providerconfig.Rotation {
			out.AccountTitle = "Take turns"
		}
	}
	return out, nil
}

// Name a person knows the provider of the profile by, its name there when it has none or is gone
func (a *Agent) providerTitle(settings *config.Config, name string) string {
	if instance, ok := a.providers.Instance(settings, name); ok {
		return instance.Label()
	}
	return name
}

// Account the session is held to in words, for the note of its control
func accountWords(instance provider.Instance, login string) string {
	if login == providerconfig.Rotation {
		return "Take turns"
	}
	return cmp.Or(loginName(instance, login), agentconfig.ProviderChoice)
}

// Error of a refusal as the app shows it, the words of the refusal alone
func refusal(err error) error {
	if _, ok := errors.AsType[*connect.Error](err); ok {
		return err
	}
	return connect.NewError(connect.CodeInvalidArgument, err.Error())
}

func (s *service) Chat(ctx context.Context, request *agentv1.ChatRequest) (*agentv1.ChatResponse, error) {
	return s.agent.Chat(ctx, request.GetSessionId(), request.GetProfile())
}

func (s *service) SetModel(ctx context.Context, request *agentv1.SetModelRequest) (*agentv1.SetModelResponse, error) {
	said, err := s.agent.SetModel(ctx, request.GetSessionId(), request.GetProvider(), request.GetModel(), request.GetClear())
	if err != nil {
		return nil, refusal(err)
	}
	return &agentv1.SetModelResponse{Said: said}, nil
}

func (s *service) SetEffort(ctx context.Context, request *agentv1.SetEffortRequest) (*agentv1.SetEffortResponse, error) {
	said, err := s.agent.SetEffort(ctx, request.GetSessionId(), request.GetEffort(), request.GetClear())
	if err != nil {
		return nil, refusal(err)
	}
	return &agentv1.SetEffortResponse{Said: said}, nil
}

func (s *service) SetBusy(ctx context.Context, request *agentv1.SetBusyRequest) (*agentv1.SetBusyResponse, error) {
	said, err := s.agent.SetBusy(ctx, request.GetSessionId(), request.GetMode(), request.GetClear())
	if err != nil {
		return nil, refusal(err)
	}
	return &agentv1.SetBusyResponse{Said: said}, nil
}
