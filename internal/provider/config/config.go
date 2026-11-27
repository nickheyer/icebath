package config

import (
	"slices"
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

// Providers of a profile, each under its name
const Key = "providers"

// Names of the settings of one provider, below its name
const (
	// Kind of the provider, by the name of its spec
	Type         = "type"
	Title        = "title"
	BaseURL      = "base_url"
	DefaultModel = "default_model"
	Models       = "models"
	// Account in use
	Account = "account"
	// Accounts, each under its name, keeping its title and the fields of the kind
	Accounts = "accounts"
	// API key of an account
	APIKeyField = "api_key"
	// Tokens its models take in at once, over what icebath finds
	ContextLength = "context_length"
	// Provider of the models.dev registry whose details its models take, for an endpoint of one's own
	CatalogProvider = "catalog_provider"
	// Order the accounts of a rotation are taken in
	Strategy = "strategy"
	// What a provider held to one account does once that account reaches its limit
	OnExhausted = "on_exhausted"
	// Places its credential may not come from: environment variables and other programs, by name
	Ignored = "ignored"
	// Seconds one request may take, and a stream may stay silent, before it is given up; 0 leaves them to the transports' settings
	RequestTimeout = "request_timeout_seconds"
	StaleTimeout   = "stale_timeout_seconds"
	// Both by model id, each a table of timeout_seconds and stale_timeout_seconds over the provider's own
	ModelTimeouts = "model_timeouts"
	// Names of the two in a model's table
	ModelRequestTimeout = "timeout_seconds"
	ModelStaleTimeout   = "stale_timeout_seconds"
	// Certificate authorities its server's certificate is checked against besides the system's, and whether it is checked at all
	CACert    = "ssl_ca_cert"
	VerifyTLS = "ssl_verify"
	// Headers every request to it carries, over the built in ones
	ExtraHeaders = "extra_headers"
	// Header its requests carry the session id in, for a server that keeps a session on one backend; empty for none
	AffinityHeader = "session_affinity_header"
	// API its requests take, the kind's own when empty
	APIMode = "api_mode"
)

// Value of Account that rotates through the accounts marked for rotation
const Rotation = "@rotate"

// Strategies of a rotation
const (
	FillFirst  = "fill_first"
	RoundRobin = "round_robin"
	LeastUsed  = "least_used"
	Random     = "random"
)

// What a provider held to one account does at its limit
const (
	Fail   = "fail"
	Rotate = "rotate"
)

// Names of the fields every account keeps, below its name
const (
	// Takes part in the provider's rotation
	InRotation = "rotate"
	// Place in the order the provider's accounts are tried, lowest first
	Priority = "priority"
	// Environment variable the account reads its key from, for a key kept outside icebath
	KeyEnv = "key_env"
	// What icebath last found of the account's limits
	Status = "status"
	// Requests made with the account, and when the last one went out in Unix milliseconds, which least_used and round_robin read
	Requests = "requests"
	Used     = "used"
)

var strategies = []config.Option{
	{Value: FillFirst, Title: "Fill first", About: "Use the first account until it runs out"},
	{Value: RoundRobin, Title: "Round robin", About: "Take turns, one request each"},
	{Value: LeastUsed, Title: "Least used", About: "Pick the account used least"},
	{Value: Random, Title: "Random", About: "Pick any account of the rotation"},
}

// Strategies a rotation can take
func Strategies() []config.Option {
	return slices.Clone(strategies)
}

// Settings of a provider for the accounts it rotates through and the places its credential may not come from
func Rotations() []config.Setting {
	return []config.Setting{
		{Key: Strategy, Title: "Rotation order", About: "Order the accounts of the rotation are used in", Default: FillFirst, Options: Strategies},
		{Key: OnExhausted, Title: "When it runs out", About: "What a provider held to one account does once its limit is reached", Default: Fail,
			Options: config.Options(config.Option{Value: Fail, Title: "Stop", About: "Stop and say so"},
				config.Option{Value: Rotate, Title: "Rotate", About: "Move on to the other accounts"})},
		{Key: Ignored, Title: "Ignored sources", About: "Environment variables and programs its credential is not taken from", Default: []string{}, Internal: true},
	}
}

// Fields every account keeps besides those of its kind: its place in the rotation and what icebath found of its limits
func Kept(keyed bool) []config.Setting {
	settings := []config.Setting{
		{Key: InRotation, Title: "In rotation", About: "Takes turns with the provider's other accounts marked for rotation", Default: false},
		{Key: Priority, Title: "Order", About: "Place among the provider's accounts, lowest tried first", Default: 0, Internal: true},
		{Key: Status, Title: "Limits", About: "What icebath last found of the account's limits", Default: map[string]any{}, Internal: true},
		{Key: Requests, Title: "Requests", About: "Requests made with the account", Default: 0, Internal: true},
		{Key: Used, Title: "Last used", About: "When the last request went out, in Unix milliseconds", Default: 0, Internal: true},
	}
	if keyed {
		settings = append(settings, config.Setting{Key: KeyEnv, Title: "Key variable", About: "Environment variable the API key is read from, in place of one kept here", Default: ""})
	}
	return settings
}

// Settings with their keys put below the prefix
func Under(prefix string, settings []config.Setting) []config.Setting {
	out := make([]config.Setting, len(settings))
	for i, setting := range settings {
		setting.Key = prefix + "." + setting.Key
		out[i] = setting
	}
	return out
}

// Settings every provider keeps, with the defaults of its kind
func Instance(kind, title, base, baseEnv, model string, accounts []string) []config.Setting {
	var env []string
	if baseEnv != "" {
		env = []string{baseEnv}
	}
	return []config.Setting{
		{Key: Type, Title: "Kind", About: "Kind of provider", Default: kind, Internal: true},
		{Key: Title, Title: "Name", About: "Name the provider is shown by", Default: title},
		{Key: BaseURL, Title: "Base URL", About: "API base URL", Default: base, Env: env},
		{Key: DefaultModel, Title: "Default model", About: "Model used when none is picked", Default: model},
		{Key: Models, Title: "Models", About: "Models offered in /model; every model it lists when empty", Default: []string{}},
		{Key: ContextLength, Title: "Context window", About: "Tokens its models take in at once, when what icebath finds is wrong; 0 uses what icebath finds", Default: 0},
		{Key: RequestTimeout, Title: "Request time limit", About: "Seconds one request may take before it is given up; 0 uses the Transports limit", Default: time.Duration(0)},
		{Key: StaleTimeout, Title: "Silence limit", About: "Seconds a reply may stay silent before the request is dropped and asked again; 0 uses the Transports limit",
			Default: time.Duration(0)},
		{Key: ModelTimeouts, Title: "Limits by model", About: "Both limits for single models, each model id holding timeout_seconds and stale_timeout_seconds",
			Default: map[string]any{}},
		{Key: CACert, Title: "CA certificate", About: "PEM file of the authorities its certificate is checked against besides the system's", Default: ""},
		{Key: VerifyTLS, Title: "Check certificate", About: "Checks the server's TLS certificate; off for a server of your own with a certificate nothing vouches for",
			Default: true},
		{Key: ExtraHeaders, Title: "Extra headers", About: "Headers every request to it carries, header names mapped to values, over the built-in ones",
			Default: map[string]any{}},
		{Key: AffinityHeader, Title: "Session header", About: "Header that carries the session id, for a server that keeps each session on one backend; empty for none",
			Default: ""},
		{Key: APIMode, Title: "Request format", About: "API its requests take; empty picks it from the provider and the model", Default: "", Options: Modes},
		{Key: Account, Title: "Account", About: "Account requests are made with", Default: "", Options: func() []config.Option {
			options := []config.Option{{Title: "Every account in order", About: "The first that works, the next once it runs out"},
				{Value: Rotation, Title: "Rotation", About: "The accounts marked for rotation, in the rotation order"}}
			for _, name := range slices.Clone(accounts) {
				options = append(options, config.Option{Value: name, Title: name})
			}
			return options
		}},
	}
}

// APIs a provider's requests can take
func Modes() []config.Option {
	return []config.Option{
		{Title: "The provider's", About: "Picked from the provider and the model"},
		{Value: "chat_completions", Title: "OpenAI chat completions"},
		{Value: "codex_responses", Title: "OpenAI responses"},
		{Value: "anthropic_messages", Title: "Anthropic messages"},
		{Value: "gemini_native", Title: "Gemini generateContent"},
		{Value: "bedrock_converse", Title: "Bedrock Converse"},
	}
}

// Title every account keeps
func AccountTitle(name string) config.Setting {
	return config.Setting{Key: Title, Title: "Name", About: "Name the account is shown by", Default: name}
}

// API key of an account, the field an account keeps when its kind names none
func APIKey() config.Setting {
	return config.Setting{Key: APIKeyField, Title: "API key", About: "API key", Default: "", Secret: true}
}
