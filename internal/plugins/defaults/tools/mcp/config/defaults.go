package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "MCP servers",
		About:  "Tool servers that the agent connects to over MCP",
		Order:  10,
		Settings: []config.Setting{
			{Key: Servers, Title: "Your servers", About: "Tool servers that the agent connects to, by name.",
				Help: "A server on this computer needs a command, a remote one an address. " +
					"/mcp and icebath mcp add servers for you.", Default: map[string]any{}, Slash: "mcp",
				Entry: &config.Entry{Noun: "server", Name: "Server name", Fields: server}},
			{Key: Env, Title: "Values", About: "Values that server settings name as ${NAME}, such as API keys.",
				Help: "An environment variable of the same name wins over the value kept here.", Default: map[string]any{}, Slash: "mcp",
				Entry: &config.Entry{Noun: "value", Name: "Value name", Capitals: true, Fields: []config.Setting{
					{Title: "Value", About: "The text put in place of ${NAME}.", Default: "", Secret: true},
				}}},
			{Key: ReloadConfirm, Title: "Confirm tool reload", About: "Ask before /reload-mcp reconnects the servers.", Help: "/reload-mcp now never asks.",
				Default: true},
			{Key: ToolTimeout, Title: "Tool call limit", About: "How long one call of an MCP tool may take.",
				Help: "A time limit that the server sets for itself wins over this one.", Default: 5 * time.Minute, Zero: "5 minutes", Advanced: true},
			{Key: DiscoveryTimeout, Title: "Wait to connect", About: "How long each reply waits for servers to connect before it starts.",
				Default: 1500 * time.Millisecond, Zero: "1.5 seconds", Advanced: true},
			{Key: SingleQueryTimeout, Title: "Single-question wait", About: "How long a single-question run waits for servers to connect.",
				Default: 15 * time.Second, Zero: "15 seconds", Advanced: true},
			{Key: DiscoveryConcurrency, Title: "Servers at once", About: "How many servers connect at the same time.", Default: 4, Zero: "No limit",
				Counts: "servers", Least: config.Num(0), Advanced: true},
			{Key: CatalogDir, Title: "Catalog folder", About: "The folder of server listings used in place of the built-in catalog.", Default: "",
				Zero: "Built-in catalog", Env: []string{"ICEBATH_OPTIONAL_MCPS"}, Advanced: true},
		},
	}})
}

// What one MCP server holds, as the servers setting keeps it
var server = []config.Setting{
	{Key: "url", Title: "Address", About: "The address of a remote server.", Default: ""},
	{Key: "command", Title: "Command", About: "The program that starts the server on this computer.", Default: ""},
	{Key: "args", Title: "Arguments", About: "The arguments that the command is given.", Default: []string{}},
	{Key: "env", Title: "Variables", About: "Variables that the command gets, each written as NAME=value.", Default: []string{}},
	{Key: "cwd", Title: "Folder", About: "The folder that the command starts in.", Default: ""},
	{Key: "headers", Title: "Headers", About: "Headers sent with every request to a remote server.", Default: map[string]any{}},
	{Key: "auth", Title: "Sign-in", About: "How icebath signs in to a remote server.", Default: "", Options: config.Options(
		config.Option{Value: "", Title: "Headers only", About: "Only the headers you set, or no sign-in at all"},
		config.Option{Value: "oauth", Title: "OAuth", About: "Sign in through a page of the server"},
	)},
	{Key: "oauth", Title: "OAuth details", About: "The client and address details for OAuth sign-in.", Default: map[string]any{}, Advanced: true},
	{Key: "enabled", Title: "Turned on", About: "Connect to this server.", Default: true},
	{Key: "trust", Title: "Trust", About: "Whether the agent asks before tools of this server that may make changes.", Default: "full",
		Options: config.Options(
			config.Option{Value: "full", Title: "Full", About: "The server's tools run without asking"},
			config.Option{Value: "untrusted", Title: "Untrusted", About: "Ask before each tool that may make changes"},
		)},
	{Key: "tools", Title: "Tool filter", About: "The tools of the server that the agent gets.",
		Help: "Holds include or exclude, a list of tool names, and resources and prompts, each on or off.", Default: map[string]any{}},
	{Key: "lazy", Title: "Start on first use", About: "Start the server only when the agent first uses one of its tools.",
		Help: "Needs the tools of the server to have been listed once before.", Default: false},
	{Key: "timeout", Title: "Tool call limit", About: "How long one tool call to this server may take.",
		Help: "When not set, Tool call limit of MCP servers applies.", Default: time.Duration(0), Zero: "The shared limit"},
	{Key: "connect_timeout", Title: "Connect time limit", About: "How long the first connection to the server may take.", Default: time.Duration(0),
		Zero: "60 seconds", Advanced: true},
	{Key: "transport", Title: "Connection kind", About: "How icebath talks to a remote server.", Default: "", Options: config.Options(
		config.Option{Value: "", Title: "Automatic", About: "HTTP, which most servers use"},
		config.Option{Value: "http", Title: "HTTP", About: "Streamable HTTP"},
		config.Option{Value: "sse", Title: "SSE", About: "Server-sent events, for older servers"},
	), Advanced: true},
	{Key: "protocol", Title: "Protocol", About: "Which version of the protocol icebath tries first.", Default: "auto", Options: config.Options(
		config.Option{Value: "auto", Title: "Automatic", About: "The handshake first, then the newer kind if the server asks"},
		config.Option{Value: "stateless", Title: "Stateless", About: "The newer kind first, then the handshake"},
		config.Option{Value: "legacy", Title: "Legacy", About: "The handshake alone"},
	), Advanced: true},
	{Key: "keepalive_interval", Title: "Keepalive", About: "How often the connection to the server is checked.",
		Help: "At least 5 seconds. A remote server is checked every 3 minutes when this is not set.", Default: time.Duration(0), Advanced: true},
	{Key: "idle_timeout_seconds", Title: "Stop when idle", About: "How long the server may sit unused before it stops.", Default: time.Duration(0),
		Zero: "Never", Advanced: true},
	{Key: "max_lifetime_seconds", Title: "Restart after", About: "How long the server runs before it is started again.", Default: time.Duration(0),
		Zero: "Never", Advanced: true},
	{Key: "sampling", Title: "Model requests", About: "The rules for when the server asks the agent's model to write for it.",
		Help: "Holds enabled, model, allowed_models, max_rpm, timeout, max_tokens_cap and max_tool_rounds.", Default: map[string]any{}, Advanced: true},
	{Key: "elicitation", Title: "Questions", About: "The rules for when the server asks you a question.", Help: "Holds enabled and timeout.",
		Default: map[string]any{}, Advanced: true},
	{Key: "tool_costs", Title: "Tool costs", About: "The cost of each tool of the server, by tool name.", Default: map[string]any{}, Advanced: true},
	{Key: "spend_limit", Title: "Spend limit", About: "The most that calls to this server may cost in one chat.", Default: 0.0, Zero: "No limit",
		Advanced: true},
	{Key: "cost_unit", Title: "Cost unit", About: "The name of the unit that costs are counted in.", Default: "", Zero: "Credits", Advanced: true},
	{Key: "identity_header", Title: "Identity header", About: "A header that tells the server who is calling.",
		Help: "Holds name, and value or value_from: profile.", Default: map[string]any{}, Advanced: true},
	{Key: "supports_parallel_tool_calls", Title: "Calls at once", About: "Let the agent call several tools of this server at the same time.",
		Default: false, Advanced: true},
	{Key: "skip_preflight", Title: "Skip first check", About: "Connect without first checking how the server answers.", Default: false, Advanced: true},
	{Key: "strict_redirect_headers", Title: "Strict redirects", About: "Drop all your headers when the server sends a request to another site.",
		Default: false, Advanced: true},
	{Key: "client_cert", Title: "Client certificate", About: "The certificate file that icebath shows the server.", Default: "", Advanced: true},
	{Key: "client_key", Title: "Client key", About: "The key file of the client certificate.", Default: "", Advanced: true},
}
