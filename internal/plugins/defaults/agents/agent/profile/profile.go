package profile

import (
	"fmt"

	"github.com/nickheyer/icebath/internal/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/profile"
)

// What the agent renames of its own as a profile is renamed: the runs of its sessions; and why the profile can't move or go now, a reply
// being written on it
type Agent interface {
	Rename(old, renamed string) error
	Holds(profile string) string
	Pinnable(settings *config.Config, provider string) string
}

// The agent as profiles come and go: it tells the model a profile runs, a fresh profile takes the model of the one it is made from, and
// the runs of a renamed profile's sessions follow it
type registrant struct {
	config *config.Config
	agent  Agent
}

func Register(registry *profile.Registry, cfg *config.Config, agent Agent) {
	registry.Register(registrant{config: cfg, agent: agent})
}

// The agent keeps nothing in a profile's folder or under its name that a deleted profile leaves; its sessions go with the session store's
func (registrant) Delete(string) error {
	return nil
}

func (r registrant) Rename(old, renamed string) error {
	return r.agent.Rename(old, renamed)
}

func (r registrant) Holds(profile string) string {
	return r.agent.Holds(profile)
}

func (registrant) Summarize(cfg *config.Config) profile.Summary {
	return profile.Summary{Model: cfg.GetString(agentconfig.Model), Provider: cfg.GetString(agentconfig.Provider)}
}

// Sets the model a new profile was asked to run with, else has one that names no model and provider of its own, a clone's included, take
// those of the profile it was made from
func (r registrant) Made(making profile.Making) ([]string, error) {
	target := r.config.In(making.Profile)
	if making.Model != "" && making.Provider != "" {
		if why := r.agent.Pinnable(target, making.Provider); why != "" {
			return []string{fmt.Sprintf("⚠ Model not set: %s.", why)}, nil
		}
		if err := target.Set(agentconfig.Provider, making.Provider); err != nil {
			return nil, err
		}
		if err := target.Set(agentconfig.Model, making.Model); err != nil {
			return nil, err
		}
		return []string{fmt.Sprintf("The model is set to %s on %s.", making.Model, making.Provider)}, nil
	}
	if making.Bare || making.Launch == "" {
		return nil, nil
	}
	if target.Stored(agentconfig.Model) != nil && target.Stored(agentconfig.Provider) != nil {
		return nil, nil
	}
	launch := r.config.In(making.Launch)
	model, provider := launch.GetString(agentconfig.Model), launch.GetString(agentconfig.Provider)
	if model == "" || provider == "" {
		return nil, nil
	}
	if err := target.Set(agentconfig.Provider, provider); err != nil {
		return nil, err
	}
	if err := target.Set(agentconfig.Model, model); err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("The model and provider are taken from the profile %s.", making.Launch)}, nil
}
