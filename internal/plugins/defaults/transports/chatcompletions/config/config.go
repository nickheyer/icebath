package config

import transportconfig "github.com/nickheyer/icebath/internal/transport/config"

const (
	Key = transportconfig.Key + ".chat_completions"
	// Sends the model's earlier reasoning back with every assistant message, which some providers need on tool call turns
	ReasoningEcho = Key + ".reasoning_echo"
)
