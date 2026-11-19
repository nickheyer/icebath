package tuigateway

// Kind of what a tool call label names
type ToolLabelKind string

func (ToolLabelKind) Values() []string { return []string{"connector", "mcp", "tool"} }

// Display label of a tool call, the app and action it drives with its emoji, text and argument preview
type ToolLabel struct {
	Kind    ToolLabelKind `json:"kind"`
	App     string        `json:"app"`
	Action  string        `json:"action"`
	Emoji   string        `json:"emoji"`
	Text    string        `json:"text"`
	Name    string        `json:"name"`
	Preview string        `json:"preview,omitzero"`
}

// Status of one task of the planning tool
type TodoStatus string

func (TodoStatus) Values() []string {
	return []string{"pending", "in_progress", "completed", "cancelled"}
}

// One task of the planning tool, a subtask naming its parent
type TodoItem struct {
	ID      string     `json:"id"`
	Content string     `json:"content"`
	Status  TodoStatus `json:"status"`
	Parent  *string    `json:"parent,omitzero"`
}

// Task list of the planning tool with the revision that orders its snapshots
type TodoState struct {
	Todos    []TodoItem `json:"todos"`
	Revision int64      `json:"revision"`
}

// A tool call began, its id, a one line context, its arguments and labels, the verbose argument text when the session shows it
type ToolStartPayload struct {
	ToolID     string         `json:"tool_id"`
	Name       string         `json:"name"`
	Context    *string        `json:"context,omitzero"`
	Args       map[string]any `json:"args,omitzero"`
	ArgsText   *string        `json:"args_text,omitzero"`
	Preview    *string        `json:"preview,omitzero"`
	Labels     []ToolLabel    `json:"labels,omitzero"`
	ToolCallID *string        `json:"tool_call_id,omitzero"`
	ToolName   *string        `json:"tool_name,omitzero"`
}

// A tool call finished, its verdict and reason, result text, summary, duration, inline diff and the task list
type ToolCompletePayload struct {
	ToolID     string         `json:"tool_id"`
	Name       string         `json:"name"`
	Context    *string        `json:"context,omitzero"`
	Preview    *string        `json:"preview,omitzero"`
	Args       map[string]any `json:"args,omitzero"`
	Labels     []ToolLabel    `json:"labels,omitzero"`
	ToolCallID *string        `json:"tool_call_id,omitzero"`
	ToolName   *string        `json:"tool_name,omitzero"`
	IsError    *bool          `json:"is_error,omitzero"`
	Refused    *bool          `json:"refused,omitzero"`
	Error      *string        `json:"error,omitzero"`
	Result     *string        `json:"result,omitzero"`
	DurationS  *float64       `json:"duration_s,omitzero"`
	Summary    *string        `json:"summary,omitzero"`
	ResultText *string        `json:"result_text,omitzero"`
	InlineDiff *string        `json:"inline_diff,omitzero"`
	Todos      []TodoItem     `json:"todos,omitzero"`
	Revision   *int64         `json:"revision,omitzero"`
}

// The model is writing the arguments of a call to the named tool
type ToolGeneratingPayload struct {
	Name string `json:"name"`
}

// Output of a call the injection scanner flagged, its risk level and findings
type ToolOutputRiskPayload struct {
	ToolID   string   `json:"tool_id"`
	Name     string   `json:"name"`
	Risk     string   `json:"risk"`
	Findings []string `json:"findings"`
	Redacted bool     `json:"redacted"`
}

// Progress an MCP server reported for a running call, at most one every half second per call
type ToolProgressPayload struct {
	ToolID   string   `json:"tool_id"`
	Name     string   `json:"name"`
	Progress float64  `json:"progress"`
	Total    *float64 `json:"total,omitzero"`
	Percent  *int64   `json:"percent,omitzero"`
	Message  *string  `json:"message,omitzero"`
}

// Session a request or method is scoped to, the launch profile's config when absent or unknown
type SessionScopedParams struct {
	SessionID *string `json:"session_id,omitzero"`
}

// One tool of the tools listing with its description
type ToolShowRow struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Tools of one toolset
type ToolShowSection struct {
	Name  string        `json:"name"`
	Tools []ToolShowRow `json:"tools"`
}

// Tools of the session grouped by toolset, deferred tools included, and their count
type ToolsShowResult struct {
	Sections []ToolShowSection `json:"sections"`
	Total    int64             `json:"total"`
}

// Whether a configure call turns its names on or off
type ToolsAction string

func (ToolsAction) Values() []string { return []string{"enable", "disable"} }

// Toolsets or server:tool MCP targets to turn on or off, the live session's profile and agent when a session is named
type ToolsConfigureParams struct {
	Action    ToolsAction `json:"action"`
	Names     []string    `json:"names"`
	SessionID *string     `json:"session_id,omitzero"`
	Profile   *string     `json:"profile,omitzero"`
}

// What a configure call changed, the toolsets now on, MCP servers and names it did not know and the session info after it
type ToolsConfigureResult struct {
	Changed         []string     `json:"changed"`
	EnabledToolsets []string     `json:"enabled_toolsets"`
	Info            *SessionInfo `json:"info,omitzero"`
	MissingServers  []string     `json:"missing_servers"`
	Reset           bool         `json:"reset"`
	Unknown         []string     `json:"unknown"`
}

// Platform whose toolsets the listing shows
type ToolsListParams struct {
	Platform *string `json:"platform,omitzero"`
}

// Every toolset and MCP tool of the platform with its status, lines in the profile's language
type ToolsListResult struct {
	Lines []string `json:"lines"`
}

// Whether a reload ran or waits for the user to confirm it
type ReloadMCPStatus string

func (ReloadMCPStatus) Values() []string { return []string{"confirm_required", "reloaded"} }

// What a reload did to the calling session's MCP servers, applied, nothing to change, or deferred while a turn runs
type ReloadMCPSession string

func (ReloadMCPSession) Values() []string { return []string{"applied", "unchanged", "busy"} }

// MCP reload of the session, confirmed or asked first, always storing the opt out, rev the config revision to load
type ReloadMCPParams struct {
	SessionID *string `json:"session_id,omitzero"`
	Confirm   bool    `json:"confirm,omitzero"`
	Always    bool    `json:"always,omitzero"`
	Rev       *string `json:"rev,omitzero"`
}

// Outcome of an MCP reload, the servers that did not connect and the servers the session's policy now runs
type ReloadMCPResult struct {
	Status         ReloadMCPStatus   `json:"status"`
	Message        *string           `json:"message,omitzero"`
	LoadedRev      *string           `json:"loaded_rev,omitzero"`
	Coalesced      *bool             `json:"coalesced,omitzero"`
	Session        *ReloadMCPSession `json:"session,omitzero"`
	Unavailable    []string          `json:"unavailable,omitzero"`
	SessionServers []string          `json:"session_servers,omitzero"`
}

// One catalog MCP server with its install state and the env keys it needs
type MCPCatalogEntry struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	ConnectorSlug *string  `json:"connector_slug,omitzero"`
	Installed     bool     `json:"installed"`
	Enabled       bool     `json:"enabled"`
	Requires      []string `json:"requires"`
	Transport     string   `json:"transport"`
}

// Catalog MCP servers of the profile
type MCPCatalogResult struct {
	Servers []MCPCatalogEntry `json:"servers"`
}

// Where a configured MCP server comes from, config.yaml or a plugin
type MCPServerSource string

func (MCPServerSource) Values() []string { return []string{"config", "plugin"} }

// A configured MCP server without its secret values, env and tools as configured
type MCPServerSummary struct {
	Name               string          `json:"name"`
	Transport          string          `json:"transport"`
	URL                *string         `json:"url,omitzero"`
	Command            *string         `json:"command,omitzero"`
	Args               []string        `json:"args"`
	Env                []string        `json:"env"`
	Auth               *string         `json:"auth,omitzero"`
	OAuthTokensPresent *bool           `json:"oauth_tokens_present,omitzero"`
	Enabled            bool            `json:"enabled"`
	Tools              any             `json:"tools,omitzero"`
	Source             MCPServerSource `json:"source"`
	Plugin             *string         `json:"plugin,omitzero"`
}

// Configured MCP servers of the profile
type MCPServersListResult struct {
	Servers []MCPServerSummary `json:"servers"`
}

// Runtime state of a configured MCP server, read from the cache and never probed
type MCPRuntimeStatus string

func (MCPRuntimeStatus) Values() []string {
	return []string{"connected", "disabled", "connecting", "failed", "lazy", "configured"}
}

// Runtime row of one configured MCP server with its tool count
type MCPServerRuntimeRow struct {
	Name      string           `json:"name"`
	Transport string           `json:"transport"`
	Tools     int64            `json:"tools"`
	Connected bool             `json:"connected"`
	Disabled  bool             `json:"disabled"`
	Status    MCPRuntimeStatus `json:"status"`
	Source    MCPServerSource  `json:"source"`
	Plugin    *string          `json:"plugin,omitzero"`
}

// Runtime rows of the configured MCP servers and when they were read
type MCPServersStatusResult struct {
	Servers   []MCPServerRuntimeRow `json:"servers"`
	CheckedAt int64                 `json:"checked_at"`
}

// Server to add from a catalog preset or an explicit config, a bearer token written to the profile's .env
type MCPServersAddParams struct {
	Profile     *string        `json:"profile,omitzero"`
	Name        string         `json:"name"`
	Preset      *string        `json:"preset,omitzero"`
	Config      map[string]any `json:"config,omitzero"`
	BearerToken *string        `json:"bearer_token,omitzero"`
}

// The server an add, enable or key change wrote
type MCPServerResult struct {
	OK     bool             `json:"ok"`
	Name   string           `json:"name"`
	Server MCPServerSummary `json:"server"`
}

// Credential of a server stored in the profile's .env under env_var or the server's own key
type MCPServersSetAPIKeyParams struct {
	Profile *string `json:"profile,omitzero"`
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	EnvVar  *string `json:"env_var,omitzero"`
}

// The server a key change wrote and the env key that holds the value
type MCPServersSetAPIKeyResult struct {
	OK     bool             `json:"ok"`
	Name   string           `json:"name"`
	EnvVar string           `json:"env_var"`
	Server MCPServerSummary `json:"server"`
}

// Configured MCP server a test, removal or sign in names
type MCPServerNameParams struct {
	Profile *string `json:"profile,omitzero"`
	Name    string  `json:"name"`
}

// Server whose enabled flag a call sets
type MCPServersEnableParams struct {
	Profile *string `json:"profile,omitzero"`
	Name    string  `json:"name"`
	Enabled bool    `json:"enabled"`
}

// One tool a probed MCP server lists
type MCPProbeTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Outcome of connecting to a server once, its tools, prompt and resource counts, or why it failed
type MCPServersTestResult struct {
	OK                 bool           `json:"ok"`
	Tools              []MCPProbeTool `json:"tools"`
	Error              *string        `json:"error,omitzero"`
	Prompts            *int64         `json:"prompts,omitzero"`
	Resources          *int64         `json:"resources,omitzero"`
	OAuthNeeded        bool           `json:"oauth_needed"`
	OAuthTokensPresent *bool          `json:"oauth_tokens_present,omitzero"`
}

// Whether a removal dropped the server from config.yaml
type MCPServersRemoveResult struct {
	OK      bool `json:"ok"`
	Removed bool `json:"removed"`
}

// Browser sign in of a server, the client hosting the loopback itself when it names its redirect
type MCPOAuthStartParams struct {
	Profile           *string `json:"profile,omitzero"`
	Name              string  `json:"name"`
	ClientRedirectURI *string `json:"client_redirect_uri,omitzero"`
}

// Sign in flow that started and the link the user opens
type MCPOAuthStartResult struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id"`
	AuthURL   string `json:"auth_url"`
	Flow      string `json:"flow"`
}

// Sign in flow of a server by the flow id its start returned
type MCPOAuthFlowParams struct {
	Profile   *string `json:"profile,omitzero"`
	Name      string  `json:"name"`
	SessionID string  `json:"session_id"`
}

// Where a browser sign in stands
type MCPOAuthPollStatus string

func (MCPOAuthPollStatus) Values() []string { return []string{"pending", "approved", "error"} }

// State of a sign in flow, the tools of the server once approved or why it failed
type MCPOAuthPollResult struct {
	OK           bool               `json:"ok"`
	Status       MCPOAuthPollStatus `json:"status"`
	SessionID    *string            `json:"session_id,omitzero"`
	ErrorMessage *string            `json:"error_message,omitzero"`
	AuthURL      *string            `json:"auth_url,omitzero"`
	Tools        []MCPProbeTool     `json:"tools,omitzero"`
}

// Whether a cancel stopped the flow and its state then
type MCPOAuthCancelResult struct {
	OK           bool    `json:"ok"`
	Status       *string `json:"status,omitzero"`
	ErrorMessage *string `json:"error_message,omitzero"`
}

// What a connection target is, an MCP server, a plugin or a skill
type ConnectionTargetKind string

func (ConnectionTargetKind) Values() []string { return []string{"mcp", "plugin", "skill"} }

// What connecting a target takes
type ConnectionTargetAction string

func (ConnectionTargetAction) Values() []string { return []string{"authorize", "enable", "install"} }

// Where a connection target stands
type ConnectionTargetState string

func (ConnectionTargetState) Values() []string {
	return []string{"pending", "initiated", "connected", "skipped", "failed", "not_connected"}
}

// Trust tier of a catalog plugin
type CatalogTier string

func (CatalogTier) Values() []string { return []string{"official", "verified", "community"} }

// Whether the native app a catalog plugin drives is there and running
type CatalogAppState string

func (CatalogAppState) Values() []string {
	return []string{"present", "missing_app", "app_not_running", "unknown"}
}

// Step an install is at
type InstallPhase string

func (InstallPhase) Values() []string {
	return []string{"downloading", "python_packages", "loading_tools"}
}

// One target of a connection operation with its state, link, the credentials it asks for and its catalog facts
type ConnectionOperationTarget struct {
	Name             string                     `json:"name"`
	Kind             ConnectionTargetKind       `json:"kind"`
	Action           ConnectionTargetAction     `json:"action"`
	State            ConnectionTargetState      `json:"state"`
	Resolved         *bool                      `json:"resolved,omitzero"`
	Detail           *string                    `json:"detail,omitzero"`
	Instructions     *string                    `json:"instructions,omitzero"`
	DiscoveryError   *string                    `json:"discovery_error,omitzero"`
	ConnectURL       *string                    `json:"connect_url,omitzero"`
	Attempt          *string                    `json:"attempt,omitzero"`
	RequiredEnv      []ConnectionTargetEnvField `json:"required_env,omitzero"`
	Tools            []string                   `json:"tools,omitzero"`
	Hint             *string                    `json:"hint,omitzero"`
	Display          *string                    `json:"display,omitzero"`
	Description      *string                    `json:"description,omitzero"`
	Tier             *CatalogTier               `json:"tier,omitzero"`
	Platforms        []string                   `json:"platforms,omitzero"`
	Repo             *string                    `json:"repo,omitzero"`
	Sha              *string                    `json:"sha,omitzero"`
	Subdir           *string                    `json:"subdir,omitzero"`
	Scan             *CatalogScan               `json:"scan,omitzero"`
	RequiresIcebath  *string                    `json:"requires_icebath,omitzero"`
	TargetProfile    *string                    `json:"target_profile,omitzero"`
	AppState         *CatalogAppState           `json:"app_state,omitzero"`
	Skill            *string                    `json:"skill,omitzero"`
	Phase            *InstallPhase              `json:"phase,omitzero"`
	Approved         *CatalogApproved           `json:"approved,omitzero"`
	Enabled          *bool                      `json:"enabled,omitzero"`
	MissingEnv       []string                   `json:"missing_env,omitzero"`
	ServerErrors     []CatalogServerError       `json:"server_errors,omitzero"`
	AlreadyInstalled *bool                      `json:"already_installed,omitzero"`
}

// MCP server an installed plugin brought that did not connect, with the reason
type CatalogServerError struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// Advanced choices the user approved on a catalog row, repeated by a retry
type CatalogApproved struct {
	Force  bool    `json:"force"`
	Enable bool    `json:"enable"`
	Ref    *string `json:"ref,omitzero"`
}

// Verdict of the catalog's security scan
type CatalogScanStatus string

func (CatalogScanStatus) Values() []string { return []string{"passed", "warnings", "failed"} }

// Security scan of the pinned commit of a catalog plugin
type CatalogScan struct {
	Status  CatalogScanStatus `json:"status"`
	Summary string            `json:"summary"`
}

// One credential an install still needs, a card field sent back with the approval
type ConnectionTargetEnvField struct {
	Name     string  `json:"name"`
	Required bool    `json:"required"`
	Secret   bool    `json:"secret"`
	Default  string  `json:"default"`
	Prompt   *string `json:"prompt,omitzero"`
}

// Targets the card suggests first
type ConnectionDisplay struct {
	Suggested []string `json:"suggested"`
}

// A connection operation opened on the session, its targets and server deadline, the card it shows
type ConnectionRequestPayload struct {
	OpID           string                      `json:"op_id"`
	Seq            int64                       `json:"seq"`
	DeadlineAt     float64                     `json:"deadline_at"`
	TimeoutSeconds float64                     `json:"timeout_seconds"`
	Targets        []ConnectionOperationTarget `json:"targets"`
	ToolCallID     *string                     `json:"tool_call_id,omitzero"`
	Display        *ConnectionDisplay          `json:"display,omitzero"`
}

// Why an operation settled, every target resolved, the user continued, the deadline passed or an interrupt
type ConnectionSettleReason string

func (ConnectionSettleReason) Values() []string {
	return []string{"all_resolved", "continue", "deadline", "interrupt"}
}

// Who moved a target, the user on the card or the watcher of the backend
type ConnectionActor string

func (ConnectionActor) Values() []string { return []string{"user", "backend_watcher"} }

// Kind of owner a connection verb names
type SessionOwnerType string

func (SessionOwnerType) Values() []string { return []string{"session"} }

// Session that owns a connection operation
type SessionOwner struct {
	Type      SessionOwnerType `json:"type"`
	SessionID string           `json:"session_id"`
}

// Snapshot of an operation with its settlement, one transition riding along on an update
type ConnectionOperationSnapshot struct {
	OpID           string                      `json:"op_id"`
	Seq            int64                       `json:"seq"`
	DeadlineAt     float64                     `json:"deadline_at"`
	TimeoutSeconds *float64                    `json:"timeout_seconds,omitzero"`
	Settled        bool                        `json:"settled"`
	SettledAt      *float64                    `json:"settled_at,omitzero"`
	SettledBy      *ConnectionSettleReason     `json:"settled_by,omitzero"`
	Targets        []ConnectionOperationTarget `json:"targets"`
	ToolCallID     *string                     `json:"tool_call_id,omitzero"`
}

// One target transition or the settlement of an operation, with the full snapshot and its owner
type ConnectionUpdatePayload struct {
	ConnectionOperationSnapshot
	Owner  SessionOwner           `json:"owner"`
	Target *string                `json:"target,omitzero"`
	From   *ConnectionTargetState `json:"from,omitzero"`
	To     *ConnectionTargetState `json:"to,omitzero"`
	Actor  *ConnectionActor       `json:"actor,omitzero"`
	Detail *string                `json:"detail,omitzero"`
}

// Operation of the owning session a status read or wake names
type ConnectionOperationParams struct {
	Profile *string      `json:"profile,omitzero"`
	Owner   SessionOwner `json:"owner"`
	OpID    string       `json:"op_id"`
}

// Answer a card gives to one target
type ConnectionAnswerStatus string

func (ConnectionAnswerStatus) Values() []string { return []string{"approved", "skipped"} }

// One target's answer, the credential values an install asked for riding with an approval
type ConnectionAnswerTarget struct {
	Name   string                 `json:"name"`
	Status ConnectionAnswerStatus `json:"status"`
	Detail *string                `json:"detail,omitzero"`
	Env    map[string]string      `json:"env,omitzero"`
}

// Answer of the card, per target outcomes or the continue that settles the operation
type ConnectionAnswer struct {
	Targets   []ConnectionAnswerTarget `json:"targets,omitzero"`
	SettledBy *ConnectionSettleReason  `json:"settled_by,omitzero"`
	Said      *string                  `json:"said,omitzero"`
}

// Answer of the card for an operation of the owning session
type ConnectionRespondParams struct {
	Profile *string          `json:"profile,omitzero"`
	Owner   SessionOwner     `json:"owner"`
	OpID    string           `json:"op_id"`
	Result  ConnectionAnswer `json:"result"`
}

// Acknowledgement of a connection verb
type ConnectionAckStatus string

func (ConnectionAckStatus) Values() []string { return []string{"ok"} }

// Whether an answer settled the operation
type ConnectionRespondResult struct {
	Status  ConnectionAckStatus `json:"status"`
	Settled bool                `json:"settled"`
}

// Acknowledgement of a wake
type ConnectionWakeResult struct {
	Status ConnectionAckStatus `json:"status"`
}

// Failed rows of the session's open operation to run again, reconnect required
type ConnectorsConnectParams struct {
	Profile    *string      `json:"profile,omitzero"`
	Owner      SessionOwner `json:"owner"`
	Connectors []string     `json:"connectors"`
	Reconnect  bool         `json:"reconnect,omitzero"`
}

// Tools of the session grouped by toolset, deferred tools included
var ToolsShow = NewMethod[SessionScopedParams, ToolsShowResult]("tools.show")

// Turns toolsets or MCP tools on or off and rebuilds the session's agent so the change applies now
var ToolsConfigure = NewMethod[ToolsConfigureParams, ToolsConfigureResult]("tools.configure")

// Every toolset and MCP tool of a platform with its status
var ToolsList = NewMethod[ToolsListParams, ToolsListResult]("tools.list")

// Reconnects the MCP servers of the live sessions after a config change
var ReloadMCP = NewMethod[ReloadMCPParams, ReloadMCPResult]("reload.mcp")

// Catalog MCP servers with their install state and the env keys each needs
var MCPCatalog = NewMethod[ProfileParams, MCPCatalogResult]("mcp.catalog")

// Configured MCP servers of the profile, secrets named by their env keys
var MCPServersList = NewMethod[ProfileParams, MCPServersListResult]("mcp.servers.list")

// Cached runtime state of every configured MCP server, never connecting
var MCPServersStatus = NewMethod[ProfileParams, MCPServersStatusResult]("mcp.servers.status")

// Adds an MCP server from a catalog preset or an explicit config
var MCPServersAdd = NewMethod[MCPServersAddParams, MCPServerResult]("mcp.servers.add")

// Stores a credential of an MCP server in the profile's .env and references it from the server
var MCPServersSetAPIKey = NewMethod[MCPServersSetAPIKeyParams, MCPServersSetAPIKeyResult]("mcp.servers.set_api_key")

// Connects to an MCP server once and lists its tools
var MCPServersTest = NewMethod[MCPServerNameParams, MCPServersTestResult]("mcp.servers.test")

// Drops an MCP server from config.yaml
var MCPServersRemove = NewMethod[MCPServerNameParams, MCPServersRemoveResult]("mcp.servers.remove")

// Sets the enabled flag of a configured MCP server
var MCPServersEnable = NewMethod[MCPServersEnableParams, MCPServerResult]("mcp.servers.enable")

// Starts the browser sign in of an MCP server
var MCPServersOAuthStart = NewMethod[MCPOAuthStartParams, MCPOAuthStartResult]("mcp.servers.oauth.start")

// Reads a sign in flow, an approval storing the tokens for the profile
var MCPServersOAuthPoll = NewMethod[MCPOAuthFlowParams, MCPOAuthPollResult]("mcp.servers.oauth.poll")

// Stops a sign in flow
var MCPServersOAuthCancel = NewMethod[MCPOAuthFlowParams, MCPOAuthCancelResult]("mcp.servers.oauth.cancel")

// Runs failed rows of the session's open connection operation again
var ConnectorsConnect = NewMethod[ConnectorsConnectParams, ConnectionOperationSnapshot]("connectors.connect")

// Current snapshot of an open connection operation
var ConnectorsOperationStatus = NewMethod[ConnectionOperationParams, ConnectionOperationSnapshot]("connectors.operation.status")

// Makes the watcher of an open operation check the accounts now
var ConnectorsOperationWake = NewMethod[ConnectionOperationParams, ConnectionWakeResult]("connectors.operation.wake")

// Per target answers of the connection card, or its continue
var ConnectionRespond = NewMethod[ConnectionRespondParams, ConnectionRespondResult]("connection.respond")

// A tool call began
var ToolStartEvent = NewEvent[ToolStartPayload]("tool.start")

// A tool call finished
var ToolCompleteEvent = NewEvent[ToolCompletePayload]("tool.complete")

// The model is writing a tool call's arguments
var ToolGeneratingEvent = NewEvent[ToolGeneratingPayload]("tool.generating")

// A tool's output carries injection findings
var ToolOutputRiskEvent = NewEvent[ToolOutputRiskPayload]("tool.output_risk")

// Progress of a running MCP tool call
var ToolProgressEvent = NewEvent[ToolProgressPayload]("tool.progress")

// Task list after a planning tool ran
var TodoUpdatedEvent = NewEvent[TodoState]("todo.updated")

// A connection operation opened on the session
var ConnectionRequestEvent = NewEvent[ConnectionRequestPayload]("connection.request")

// A target of an open connection operation moved or the operation settled
var ConnectionUpdateEvent = NewEvent[ConnectionUpdatePayload]("connection.update")
