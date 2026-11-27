package config

const (
	Name  = "custom"
	Title = "Custom endpoint"
)

// Names of the settings of a custom endpoint, below its name in the profile
const KeyEnv = "key_env"

// Context window an Ollama server is asked for on every request, 0 for the one it reports
const OllamaNumCtx = "ollama_num_ctx"
