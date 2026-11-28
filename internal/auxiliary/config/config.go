package config

import (
	"encoding/json"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
)

const (
	Key              = "auxiliary"
	FreeOnly         = Key + ".free_only"
	OpenRouterModel  = Key + ".openrouter_model"
	TransientRetries = Key + ".transient_retries"
)

// Names of the settings every task keeps below its own key
const (
	Provider          = "provider"
	Model             = "model"
	BaseURL           = "base_url"
	APIKey            = "api_key"
	KeyEnv            = "key_env"
	APIMode           = "api_mode"
	Timeout           = "timeout"
	NoProgressTimeout = "no_progress_timeout"
	ReasoningEffort   = "reasoning_effort"
	ExtraBody         = "extra_body"
	FallbackChain     = "fallback_chain"
)

// Providers a task's provider setting names besides those of the profile
const (
	// Picks one: the main model of the session first
	Auto = "auto"
	// The session's main provider
	Main = "main"
	// The endpoint the task's base URL names
	Custom = "custom"
)

// Task that looks at images, the one every tool that hands the helper an image calls, and the setting of how varied its descriptions are
const (
	Vision      = "vision"
	Temperature = "temperature"
)

// Kinds of thing a setting names that the pages of the models the agent uses offer pickers for, by the name a setting gives in Picks
const (
	// How hard the model kept with the setting thinks, picked from the efforts that model has: the model at the same place as the setting,
	// else the agent's
	PickEffort = "effort"
)

// The value of one extra request field, as a list of names and values asks for it: text, a number, true or false, or JSON for a list or a
// group of values
var RequestField = config.Setting{Title: "Value", About: "The value sent for this field.",
	Help: "Text, a number, true or false, or JSON such as {\"order\": [\"groq\"]}.", Default: ""}

// A value of an extra request field as it is sent: text that reads as JSON, such as 0.2, true or {"order": ["groq"]}, taken as that JSON,
// any other value as it is
func FieldValue(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	var decoded any
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &decoded) == nil {
		return decoded
	}
	return value
}

// Write of the extra request fields kept under the table, the whole set or one field, each value kept as it is sent
func WriteFields(table string) func(cfg *config.Config, key string, value any) (bool, error) {
	return func(cfg *config.Config, key string, value any) (bool, error) {
		if fields, ok := value.(map[string]any); ok && key == table {
			kept := maps.Clone(fields)
			for name, field := range kept {
				kept[name] = FieldValue(field)
			}
			return true, cfg.Set(key, kept)
		}
		return true, cfg.Set(key, FieldValue(value))
	}
}
