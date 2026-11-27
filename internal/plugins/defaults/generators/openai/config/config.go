package config

import generationconfig "github.com/nickheyer/icebath/internal/generation/config"

const (
	Key = generationconfig.ImageKey + ".openai"
	// Tier or model id OpenAI draws with
	Model = Key + ".model"
	// Provider of the profile whose address and key the requests take
	Provider = Key + ".provider"
	BaseURL  = Key + ".base_url"
	// Variable holding the key the requests send
	KeyEnv = Key + ".key_env"

	CodexKey   = generationconfig.ImageKey + ".openai-codex"
	CodexModel = CodexKey + ".model"
)

// Names of the two services
const (
	Name  = "openai"
	Codex = "openai-codex"
)
