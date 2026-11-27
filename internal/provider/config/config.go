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
	{Value: FillFirst, Title: "Fill first", About: "Use one account until it runs out, then the next"},
	{Value: RoundRobin, Title: "Round robin", About: "Take turns, one request each"},
	{Value: LeastUsed, Title: "Least used", About: "Pick the account used least"},
	{Value: Random, Title: "Random", About: "Pick any account in the rotation"},
}

// Strategies a rotation can take
func Strategies() []config.Option {
	return slices.Clone(strategies)
}

// Settings of a provider for the accounts it rotates through and the places its credential may not come from
func Rotations() []config.Setting {
	return []config.Setting{
		{Key: Strategy, Title: "Rotation order", About: "Which account is used next when several take turns.", Default: FillFirst, Options: Strategies},
		{Key: OnExhausted, Title: "When accounts run out", About: "What happens when the account in use reaches its limit.", Default: Fail,
			Options: config.Options(config.Option{Value: Fail, Title: "Stop", About: "Stop and say so"},
				config.Option{Value: Rotate, Title: "Move on", About: "Go on with the other accounts"})},
		{Key: Ignored, Title: "Ignored sources", About: "Variables and programs that the provider never takes a key or sign-in from.", Default: []string{},
			Internal: true},
	}
}

// Fields every account keeps besides those of its kind: its place in the rotation and what icebath found of its limits
func Kept(keyed bool) []config.Setting {
	settings := []config.Setting{
		{Key: InRotation, Title: "In rotation", About: "Take turns with the other accounts of this provider that are in rotation.", Default: false},
		{Key: Priority, Title: "Order", About: "The place among the accounts of this provider, lowest tried first.", Default: 0, Internal: true},
		{Key: Status, Title: "Limits", About: "What icebath last found out about the limits of this account.", Default: map[string]any{}, Internal: true},
		{Key: Requests, Title: "Requests", About: "How many requests were made with this account.", Default: 0, Counts: "requests", Internal: true},
		{Key: Used, Title: "Last used", About: "When the last request with this account went out.", Help: "A time in Unix milliseconds.", Default: 0,
			Internal: true},
	}
	if keyed {
		settings = append(settings, config.Setting{Key: KeyEnv, Title: "Key variable", About: "The environment variable that holds the API key.",
			Help: "Read in place of an API key kept here.", Default: "", Advanced: true})
	}
	return settings
}

// Settings in the order a section lists them: everyday ones first, then those behind Advanced, each in the order given
func Arranged(settings []config.Setting) []config.Setting {
	arranged := slices.Clone(settings)
	slices.SortStableFunc(arranged, func(a, b config.Setting) int {
		switch {
		case a.Advanced == b.Advanced:
			return 0
		case b.Advanced:
			return -1
		}
		return 1
	})
	return arranged
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
		{Key: Type, Title: "Kind", About: "The kind of provider this is.", Default: kind, Internal: true},
		{Key: Title, Title: "Name", About: "The name this provider is shown by.", Default: title},
		{Key: DefaultModel, Title: "Default model", About: "The model used when no other is picked.", Default: model},
		{Key: Models, Title: "Models", About: "The models offered when you pick one with /model.", Default: []string{}, Zero: "Every listed model"},
		{Key: Account, Title: "Account", About: "Which account requests are made with.", Default: "", Options: func() []config.Option {
			options := []config.Option{{Title: "Every account in order", About: "The first that works, then the next when one runs out"},
				{Value: Rotation, Title: "Take turns", About: "The accounts in rotation, in the rotation order"}}
			for _, name := range slices.Clone(accounts) {
				options = append(options, config.Option{Value: name, Title: name})
			}
			return options
		}},
		{Key: BaseURL, Title: "Base URL", About: "The address that requests to this provider are sent to.", Default: base, Env: env, Advanced: true},
		{Key: APIMode, Title: "Request format", About: "Which API the requests to this provider use.", Default: "", Options: Modes, Advanced: true},
		{Key: ContextLength, Title: "Context window", About: "How much text the models of this provider can read at once.",
			Help: "Set this only when icebath gets the size wrong.", Default: 0, Zero: "Automatic", Counts: "tokens", Least: config.Num(0), Advanced: true},
		{Key: RequestTimeout, Title: "Request time limit", About: "How long one request may take before it is given up.", Default: time.Duration(0),
			Zero: "As in Connections", Advanced: true},
		{Key: StaleTimeout, Title: "Silence limit", About: "How long a reply may stay silent before the request is sent again.", Default: time.Duration(0),
			Zero: "As in Connections", Advanced: true},
		{Key: ModelTimeouts, Title: "Limits by model", About: "Time limits for single models of this provider.", Default: map[string]any{}, Advanced: true,
			Entry: &config.Entry{Noun: "model", Name: "Model ID", Fields: []config.Setting{
				{Key: ModelRequestTimeout, Title: "Request time limit", About: "How long one request to this model may take.", Default: 0,
					Zero: "As the provider", Counts: "seconds"},
				{Key: ModelStaleTimeout, Title: "Silence limit", About: "How long a reply of this model may stay silent.", Default: 0,
					Zero: "As the provider", Counts: "seconds"},
			}}},
		{Key: CACert, Title: "CA certificate", About: "A PEM file of authorities that the server's certificate is checked against.",
			Help: "Used besides the authorities this computer trusts.", Default: "", Advanced: true},
		{Key: VerifyTLS, Title: "Check certificate", About: "Check the TLS certificate of the server.",
			Help: "Turn this off only for a server of your own whose certificate nothing vouches for.", Default: true, Advanced: true},
		{Key: ExtraHeaders, Title: "Extra headers", About: "Headers sent with every request to this provider.",
			Help: "A header set here goes over the built-in one of the same name.", Default: map[string]any{}, Advanced: true,
			Entry: &config.Entry{Noun: "header", Name: "Header name", Fields: []config.Setting{
				{Title: "Value", About: "The text the header carries.", Default: ""}}}},
		{Key: AffinityHeader, Title: "Session header", About: "The header that tells the session ID to a server that keeps each session in one place.",
			Default: "", Zero: "Not sent", Advanced: true},
	}
}

// APIs a provider's requests can take
func Modes() []config.Option {
	return []config.Option{
		{Title: "Automatic", About: "Picked from the provider and the model"},
		{Value: "chat_completions", Title: "OpenAI chat completions"},
		{Value: "codex_responses", Title: "OpenAI responses"},
		{Value: "anthropic_messages", Title: "Anthropic messages"},
		{Value: "gemini_native", Title: "Gemini generateContent"},
		{Value: "bedrock_converse", Title: "Bedrock Converse"},
	}
}

// Title every account keeps
func AccountTitle(name string) config.Setting {
	return config.Setting{Key: Title, Title: "Name", About: "The name this account is shown by.", Default: name}
}

// API key of an account, the field an account keeps when its kind names none
func APIKey() config.Setting {
	return config.Setting{Key: APIKeyField, Title: "API key", About: "Your API key for this account.", Default: "", Secret: true}
}
