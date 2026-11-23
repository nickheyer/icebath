package settings

// Words of every reasoning effort, off first
var reasoningChoices = []Choice{
	{"none", "Off", "No thinking before answering."},
	{"minimal", "Minimal", "The least thinking the model offers."},
	{"low", "Low", "Quick answers."},
	{"medium", "Medium", "Balanced."},
	{"high", "High", "Slower, often better."},
	{"xhigh", "Extra high", "Much slower and costlier."},
	{"max", "Max", "The most the model offers."},
	{"ultra", "Ultra", "Beyond max where the model has it."},
}

// Model that answers prompts
var Model = Register(&String{Meta: Meta{Key: "model.default", Label: "Model",
	Short: "Which model answers your prompts.",
	Help:  "/model changes it for one session or saves it here.",
	Group: GroupModel, Order: 10, Apply: NewSession, Slash: "model"}})

// Service the model is reached through
var ModelProvider = Register(&String{Meta: Meta{Key: "model.provider", Label: "Provider",
	Short: "The service the model is reached through.",
	Group: GroupModel, Order: 11, Apply: NewSession}})

// Custom endpoint of the model
var ModelBaseURL = Register(&String{Meta: Meta{Key: "model.base_url", Label: "Endpoint address",
	Short: "Custom address of the model's service.",
	Help:  "Empty uses the provider's own address.",
	Group: GroupModel, Order: 100, Advanced: true, Apply: NewSession}})

// Request format of a custom endpoint
var ModelAPIMode = Register(&String{Meta: Meta{Key: "model.api_mode", Label: "Request format",
	Short: "Request format the endpoint speaks.",
	Help:  "Empty picks it from the provider.",
	Group: GroupModel, Order: 101, Advanced: true, Apply: NewSession}})

// How long the model thinks before answering
var ReasoningEffort = Register(&Enum{Meta: Meta{Key: "agent.reasoning_effort", Label: "Reasoning effort",
	Short: "How long the model thinks before answering.",
	Help:  "More effort is slower and costs more, but is often better. Models that cannot think ignore it.",
	Group: GroupModel, Order: 20, Scopes: Global | Session, Apply: NextTurn, Slash: "reasoning"},
	Null: "the model's default", Values: reasoningChoices})

// Priority processing tier
var FastMode = Register(&Enum{Meta: Meta{Key: "agent.service_tier", Label: "Fast mode",
	Short: "Pay more for priority processing so replies come back faster.",
	Help:  "Auto is fast for the first seconds of each turn, cold only on a session's first turn. Only some models offer it.",
	Group: GroupModel, Order: 30, Scopes: Global | Session, Apply: NextTurn, Slash: "fast"},
	Default: "normal", On: "fast", Off: "normal",
	Values: []Choice{
		{"normal", "Normal", "Standard speed and price."},
		{"fast", "Fast", "Priority processing on every request."},
		{"ultrafast", "Ultrafast", "The fastest tier, where offered."},
		{"auto", "Auto", "Fast for the first seconds of each turn."},
		{"cold", "Cold", "Fast only on a session's first turn."},
	}})

// Models tried in order when the main one fails
var FallbackModels = Register(&List{Meta: Meta{Key: "fallback_providers", Label: "Fallback models",
	Short: "Models to try, in order, when the main one fails.",
	Group: GroupModel, Order: 40, Apply: NextTurn},
	Of: KindMap})

// Codex security research access request
var Daybreak = Register(&Bool{Meta: Meta{Key: "agent.daybreak", Label: "Codex research access",
	Short: "Ask Codex for its security research access in your own sessions.",
	Help:  "Only your own terminal sessions send it, and only on a Codex subscription that offers it.",
	Group: GroupModel, Order: 110, Advanced: true, Apply: NextTurn, Slash: "daybreak"},
	Default: false})

// Ways a provider picks among the logins in its rotation
var loginStrategyChoices = []Choice{
	{"fill_first", "Fill first", "Use the first login until it runs out."},
	{"round_robin", "Round robin", "Take turns, one request each."},
	{"least_used", "Least used", "Pick the login used least lately."},
	{"random", "Random", "Pick any login in the rotation."},
}

// Login each provider uses by default and what happens when it runs out, keyed by provider
var Logins = Register(&Map{Meta: Meta{Key: "logins", Label: "Logins per provider",
	Short: "Which login each provider uses and what happens when it runs out.",
	Help:  "Manage logins in /logins; a session can still pin its own.",
	Group: GroupModel, Order: 50, Advanced: true, Apply: NewSession},
	Value: &Object{Meta: Meta{Key: "logins.*"}, Fields: []Entry{
		&String{Meta: Meta{Key: "default", Label: "Default login",
			Short: "Login new sessions use, or @rotate for the rotation."}, Null: "the provider picks"},
		&Enum{Meta: Meta{Key: "on_exhausted", Label: "When it runs out",
			Short: "What a pinned login does once its limit is reached."}, Default: "fail",
			Values: []Choice{
				{"fail", "Stop", "Stop and say so."},
				{"rotate", "Rotate", "Move on to the next login in the rotation."},
			}},
		&Enum{Meta: Meta{Key: "strategy", Label: "Rotation order",
			Short: "Order the logins in the rotation are used in."}, Null: "the provider's key rotation setting",
			Values: loginStrategyChoices},
	}}})
