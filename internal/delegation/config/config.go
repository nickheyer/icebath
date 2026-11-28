package config

const (
	Key = "delegation"
	// Backend that routes each task to where its subagent runs, and the settings of each backend by its name
	Backend  = Key + ".backend"
	Backends = Key + ".backends"
	// Where subagents run when the default backend routes them: model, provider, an endpoint of their own with its key and request format,
	// extra request fields, how hard they think and the models they fall back to
	Model            = Key + ".model"
	Provider         = Key + ".provider"
	BaseURL          = Key + ".base_url"
	APIKey           = Key + ".api_key"
	APIMode          = Key + ".api_mode"
	RequestOverrides = Key + ".request_overrides"
	ReasoningEffort  = Key + ".reasoning_effort"
	Fallbacks        = Key + ".fallback_providers"
	// How many run at once, how deep they nest, whether children may lead children, how many turns each takes and how many one one-shot
	// session may start
	MaxConcurrent       = Key + ".max_concurrent_children"
	MaxSpawnDepth       = Key + ".max_spawn_depth"
	OrchestratorEnabled = Key + ".orchestrator_enabled"
	MaxIterations       = Key + ".max_iterations"
	OneshotMaxChildren  = Key + ".oneshot_max_children"
	// Seconds a subagent may sit without any progress before it is stopped
	ChildTimeout = Key + ".child_timeout_seconds"
	// Tokens at which a subagent compresses its chat, below the share of the window its main model would compress at
	CompressionThreshold = Key + ".compression_threshold_tokens"
	// Longest report of a subagent handed back whole, the rest saved to a file
	MaxSummaryChars = Key + ".max_summary_chars"
	// Whether chat sessions wait for their subagents inside the turn, and whether background ones report one by one
	GatewayMode            = Key + ".gateway_mode"
	IndependentCompletions = Key + ".independent_completions"
	// Each subagent in a git worktree of its own
	WorktreeIsolation = Key + ".worktree_isolation"
	// Subagents narrowed to some toolsets keep the parent's MCP servers
	InheritMCPToolsets = Key + ".inherit_mcp_toolsets"
	// Risky commands of subagents run without asking, else they are refused
	SubagentAutoApprove = Key + ".subagent_auto_approve"
	// The parent hears of the background processes its subagents start
	SurfaceChildProcessNotifications = Key + ".surface_child_process_notifications"
)

// Values of gateway_mode
const (
	GatewayAuto       = "auto"
	GatewayInline     = "inline"
	GatewayBackground = "background"
)

// Kind of thing Method names, a way subagents are run, by the name the Subagents page registers its picker under: the methods that can run,
// each opening its own settings
const PickMethod = "method"
