package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key             = toolsconfig.Key + ".x_search"
	Model           = Key + ".model"
	ReasoningEffort = Key + ".reasoning_effort"
	Retries         = Key + ".retries"
	TimeoutSeconds  = Key + ".timeout_seconds"
)

// Model of a search when tools.x_search.model is blank
const DefaultModel = "grok-4.5"
