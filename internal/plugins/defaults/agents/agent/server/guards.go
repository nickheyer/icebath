package server

import (
	"fmt"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	agentprompt "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/prompt"
)

// Takes every write a person asks for of the settings of the agent that must be kept whole: the provider of new chats with its model, a
// model shortcut, which is nothing without its model, and a personality, which is nothing without what it tells the agent. The pages, the
// settings search and icebath config all write through it
func (a *Agent) guard(cfg *config.Config) {
	cfg.Guard(agentconfig.Provider, config.Guard{Check: a.checkProvider, Write: a.writeProvider})
	cfg.Guard(agentconfig.Aliases, config.Guard{Check: a.checkShortcut})
	cfg.Guard(agentconfig.Personalities, config.Guard{Check: checkPersonality})
}

// Refusal of a provider the profile has not got
func (a *Agent) checkProvider(cfg *config.Config, _ string, value any) error {
	name, _ := value.(string)
	if name = strings.TrimSpace(name); name == "" {
		return nil
	}
	if _, ok := a.providers.Instance(cfg, name); !ok {
		return fmt.Errorf("The profile %s has no provider named %s.", cfg.Profile(), name)
	}
	return nil
}

// A provider changed alone takes its own default model: the model kept beside it was one of the provider before
func (a *Agent) writeProvider(cfg *config.Config, key string, value any) (bool, error) {
	if err := a.checkProvider(cfg, key, value); err != nil {
		return false, err
	}
	was, _ := cfg.Stored(key).(string)
	now, _ := value.(string)
	if strings.TrimSpace(was) == strings.TrimSpace(now) {
		return false, nil
	}
	if err := cfg.Set(key, value); err != nil {
		return true, err
	}
	if cfg.Stored(agentconfig.Model) == nil || cfg.Lock(agentconfig.Model) != nil {
		return true, nil
	}
	return true, cfg.Set(agentconfig.Model, nil)
}

// Refusal of a model shortcut a write would leave without its model, with a name a key can't hold, a provider the profile has not got or
// an address that is no address
func (a *Agent) checkShortcut(cfg *config.Config, key string, value any) error {
	for name, entry := range written(cfg, agentconfig.Aliases, key, value) {
		if entry == nil {
			continue
		}
		fields, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("The shortcut %s must be a set of named values: a model, and for a server of your own its address.", name)
		}
		if !config.Named(name) {
			return fmt.Errorf("A shortcut's name holds only lower case letters, digits, - and _, such as fast, not %s.", name)
		}
		if aliasField(fields["model"]) == "" {
			return fmt.Errorf("The shortcut %s needs a model.", name)
		}
		if provider := aliasField(fields["provider"]); provider != "" {
			if _, ok := a.provider(cfg, provider); !ok {
				return fmt.Errorf("The profile %s has no provider named %s.", cfg.Profile(), provider)
			}
		}
		if base := strings.ToLower(aliasField(fields["base_url"])); base != "" && !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			return fmt.Errorf("The address of the shortcut %s starts with http:// or https://.", name)
		}
	}
	return nil
}

// Refusal of a personality a write would leave telling the agent nothing, or with a name that means no personality or that a key can't hold
func checkPersonality(cfg *config.Config, key string, value any) error {
	for name, entry := range written(cfg, agentconfig.Personalities, key, value) {
		if entry == nil {
			continue
		}
		if agentprompt.Neutral(name) {
			return fmt.Errorf("%s means no personality, so a personality of your own can't have that name.", name)
		}
		if !config.Named(name) {
			return fmt.Errorf("A personality's name holds only lower case letters, digits, - and _, such as calm, not %s.", name)
		}
		if (agentprompt.Personality{Name: name, Definition: entry}).Instructions() == "" {
			return fmt.Errorf("The personality %s needs instructions, a tone or a style.", name)
		}
	}
	return nil
}

// Entries of a table that a write of the key leaves changed, by name, nil for one removed: every entry of the whole table written, the one
// entry written, or the entry one field of it is written into
func written(cfg *config.Config, table, key string, value any) map[string]any {
	rest, _ := strings.CutPrefix(key, table)
	rest = strings.TrimPrefix(rest, ".")
	if rest == "" {
		entries, _ := value.(map[string]any)
		out := map[string]any{}
		held, _ := cfg.Stored(table).(map[string]any)
		for name := range held {
			out[name] = nil
		}
		maps.Copy(out, entries)
		return out
	}
	name, field, ok := strings.Cut(rest, ".")
	if !ok {
		return map[string]any{name: value}
	}
	entry, _ := cfg.Stored(table + "." + name).(map[string]any)
	entry = maps.Clone(entry)
	if entry == nil {
		entry = map[string]any{}
	}
	if value == nil {
		delete(entry, field)
	} else {
		entry[field] = value
	}
	return map[string]any{name: entry}
}
