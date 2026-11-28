package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "MCP servers",
		About:  "Servers that give the agent more tools, such as GitHub",
		Area:   true,
		Group:  config.GroupAbilities,
		Order:  12,
		Settings: []config.Setting{
			{Key: Servers, Title: "Your servers", About: "Tool servers that the agent connects to, by name.", Default: map[string]any{}, Custom: true,
				Entry: &config.Entry{Noun: "server", Name: "Server name"}},
			{Key: Env, Title: "Values", About: "Values that server settings name as ${NAME}, such as API keys.",
				Help: "An environment variable of the same name wins over the value kept here.", Default: map[string]any{}, Custom: true,
				Entry: &config.Entry{Noun: "value", Name: "Value name", Fields: []config.Setting{
					{Title: "Value", About: "The text put in place of ${NAME}.", Default: "", Secret: true},
				}}},
			{Key: ReloadConfirm, Title: "Confirm tool reload", About: "Ask before the servers connect again.",
				Help: "Connect again asks first while this is on, and so does /reload-mcp unless you type /reload-mcp now.", Default: true},
			{Key: ToolTimeout, Title: "Tool call limit", About: "How long one call of an MCP tool may take.",
				Help: "A time limit that the server sets for itself wins over this one.", Default: 5 * time.Minute, Zero: "5 minutes", Advanced: true},
			{Key: DiscoveryTimeout, Title: "Wait to connect", About: "How long each reply waits for servers to connect before it starts.",
				Default: 1500 * time.Millisecond, Zero: "1.5 seconds", Advanced: true},
			{Key: SingleQueryTimeout, Title: "Single-question wait", About: "How long a single-question run waits for servers to connect.",
				Default: 15 * time.Second, Zero: "15 seconds", Advanced: true},
			{Key: DiscoveryConcurrency, Title: "Servers at once", About: "How many servers connect at the same time.", Default: 4, Zero: "No limit",
				Counts: "servers", Least: config.Num(0), Advanced: true},
			{Key: CatalogDir, Title: "Catalog folder", About: "The folder of server listings used in place of the built-in catalog.", Default: "",
				Zero: "Built-in catalog", Env: []string{"ICEBATH_OPTIONAL_MCPS"}, Advanced: true, Path: &config.Path{Kind: config.PathFolder, Exists: true}},
		},
	}})
}

// Groups of fields of a server with a page of their own on its page, each the key of a table inside the server's entry
const (
	OAuth       = "oauth"
	Sampling    = "sampling"
	Elicitation = "elicitation"
	Identity    = "identity_header"
	Filter      = "tools"
)

// Fields of a server's entry its page changes itself: whether it is on, how it signs in, and the tools the agent gets
const (
	Enabled = "enabled"
	Auth    = "auth"
	Include = "include"
	Exclude = "exclude"
	URL     = "url"
	Command = "command"
)

// Settings of one server, each a field of its entry under Servers, which its page shows as option rows with the control of their kind:
// those of the way it connects, a command on this computer or an address, then those of every server. Whether it is on, how it signs in
// and the tools the agent gets are its page's to change with views of their own
func Fields(prefix string, remote bool) []config.Setting {
	key := func(name string) string { return prefix + "." + name }
	var way []config.Setting
	if remote {
		way = []config.Setting{
			{Key: key(URL), Title: "Address", About: "The address that the server is reached at.", Help: "Such as https://example.com/mcp.", Default: "",
				Starts: []string{"https://", "http://"}},
			{Key: key("headers"), Title: "Headers", About: "Headers sent with every request to the server.", Default: map[string]any{},
				Entry: &config.Entry{Noun: "header", Name: "Header name", IgnoreCase: true, Fields: []config.Setting{
					{Title: "Value", About: "The text the header holds, such as Bearer ${API_KEY}.", Default: ""},
				}}},
			{Key: key("transport"), Title: "Connection kind", About: "How icebath talks to the server.", Default: "", Advanced: true,
				Options: config.Options(
					config.Option{Value: "", Title: "Automatic", About: "HTTP, which most servers use"},
					config.Option{Value: "http", Title: "HTTP", About: "Streamable HTTP"},
					config.Option{Value: "sse", Title: "SSE", About: "Server-sent events, for older servers"},
				)},
		}
	} else {
		way = []config.Setting{
			{Key: key(Command), Title: "Command", About: "The program that starts the server on this computer.", Help: "Such as npx or uvx.", Default: "",
				Path: &config.Path{Kind: config.PathProgram}},
			{Key: key("args"), Title: "Arguments", About: "The words the command is given after it.",
				Help: "Written as in a shell, such as -y @modelcontextprotocol/server-github.", Default: "", Zero: "None"},
			{Key: key("env"), Title: "Variables", About: "Variables that the command gets.", Default: map[string]any{},
				Entry: &config.Entry{Noun: "variable", Name: "Variable name", Fields: []config.Setting{
					{Title: "Value", About: "The text the variable holds, such as ${GITHUB_TOKEN}.", Default: ""},
				}}},
			{Key: key("cwd"), Title: "Folder", About: "The folder that the command starts in.", Default: "", Zero: "The folder icebath works in",
				Path: &config.Path{Kind: config.PathFolder, Exists: true}},
		}
	}
	return append(way, []config.Setting{
		{Key: key(Enabled), Title: "On", About: "Connect to this server.", Default: true, Custom: true},
		{Key: key(Auth), Title: "Sign-in", About: "How icebath signs in to the server.", Default: "", Custom: true},
		{Key: key("trust"), Title: "Trust", About: "Whether the agent asks before tools of this server that may make changes.", Default: "full",
			Options: config.Options(
				config.Option{Value: "full", Title: "Full", About: "Its tools run without asking"},
				config.Option{Value: "untrusted", Title: "Ask first", About: "Ask before each tool that may make changes"},
			)},
		{Key: key("lazy"), Title: "Start on first use", About: "Start the server only when the agent first uses one of its tools.",
			Help: "It needs the tools of the server to have been listed once before.", Default: false},
		{Key: key("timeout"), Title: "Tool call limit", About: "How long one tool call to this server may take.", Default: time.Duration(0),
			Zero: "The shared limit", Advanced: true},
		{Key: key("connect_timeout"), Title: "Connect time limit", About: "How long the first connection to the server may take.",
			Default: time.Duration(0), Zero: "60 seconds", Advanced: true},
		{Key: key("protocol"), Title: "Protocol", About: "Which version of the protocol icebath tries first.", Default: "auto", Advanced: true,
			Options: config.Options(
				config.Option{Value: "auto", Title: "Automatic", About: "The handshake first, then the newer kind if the server asks"},
				config.Option{Value: "stateless", Title: "Stateless", About: "The newer kind first, then the handshake"},
				config.Option{Value: "legacy", Title: "Legacy", About: "The handshake alone"},
			)},
		{Key: key("keepalive_interval"), Title: "Keepalive", About: "How often the connection to the server is checked.",
			Help: "At least 5 seconds. A server reached at an address is checked every 3 minutes when this is not set.", Default: time.Duration(0),
			Zero: "Automatic", Advanced: true},
		{Key: key("idle_timeout_seconds"), Title: "Stop when idle", About: "How long the server may sit unused before it stops.",
			Default: time.Duration(0), Zero: "Never", Advanced: true},
		{Key: key("max_lifetime_seconds"), Title: "Restart after", About: "How long the server runs before it is started again.",
			Default: time.Duration(0), Zero: "Never", Advanced: true},
		{Key: key("tool_costs"), Title: "Tool costs", About: "What each tool of the server costs, by the tool's name.", Default: map[string]any{},
			Advanced: true, Entry: &config.Entry{Noun: "tool cost", Name: "Tool name", Fields: []config.Setting{
				{Key: "credits", Title: "Cost", About: "What one call of the tool costs.", Default: 0.0, Need: true},
				{Key: "note", Title: "Note", About: "A few words about the cost, such as per page.", Default: ""},
			}}},
		{Key: key("spend_limit"), Title: "Spend limit", About: "The most that calls to this server may cost in one chat.", Default: 0.0,
			Zero: "No limit", Advanced: true},
		{Key: key("cost_unit"), Title: "Cost unit", About: "The name of the unit that costs are counted in.", Default: "", Zero: "Credits",
			Advanced: true},
		{Key: key("supports_parallel_tool_calls"), Title: "Calls at once", About: "Let the agent call several tools of this server at the same time.",
			Default: false, Advanced: true},
		{Key: key("skip_preflight"), Title: "Skip first check", About: "Connect without first checking how the server answers.", Default: false,
			Advanced: true},
		{Key: key("strict_redirect_headers"), Title: "Strict redirects",
			About: "Drop all your headers when the server sends a request on to another site.", Default: false, Advanced: true},
		{Key: key("client_cert"), Title: "Client certificate", About: "The certificate file that icebath shows the server.",
			Help: "Such as ~/certs/client.pem.", Default: "", Advanced: true, Path: &config.Path{Kind: config.PathFile, Exists: true}},
		{Key: key("client_key"), Title: "Client key", About: "The key file of the client certificate.", Help: "Such as ~/certs/client.key.",
			Default: "", Advanced: true, Path: &config.Path{Kind: config.PathFile, Exists: true}},
	}...)
}

// Groups of a server's fields, each with a page of its own inside its Advanced page, by the key of its table, its title and what it is
type Part struct {
	Key, Title, About string
	Fields            func(prefix string) []config.Setting
}

// The groups of fields of a server, those of signing in only for one reached at an address
func Parts(remote bool) []Part {
	parts := []Part{
		{Filter, "Resources and prompts", "Whether the agent may use the resources and the prompts of the server", filterFields},
		{Sampling, "Model requests", "When the server may ask the agent's model to write for it", samplingFields},
		{Elicitation, "Questions", "When the server may ask you a question", elicitationFields},
		{Identity, "Identity header", "A header that tells the server who is calling", identityFields},
	}
	if remote {
		parts = append(parts, Part{OAuth, "Sign-in details", "The client and addresses icebath signs in to the server with", oauthFields})
	}
	return parts
}

func filterFields(prefix string) []config.Setting {
	return []config.Setting{
		{Key: prefix + ".resources", Title: "Resources", About: "Let the agent read the resources the server offers.", Default: true},
		{Key: prefix + ".prompts", Title: "Prompts", About: "Let the agent use the prompts the server offers.", Default: true},
		{Key: prefix + "." + Include, Title: "Only these tools", About: "The tools of the server that the agent gets.", Default: []string{}, Custom: true},
		{Key: prefix + "." + Exclude, Title: "All tools but these", About: "The tools of the server that the agent does not get.", Default: []string{},
			Custom: true},
	}
}

func samplingFields(prefix string) []config.Setting {
	return []config.Setting{
		{Key: prefix + ".enabled", Title: "Allow model requests", About: "Let the server ask the agent's model to write for it.", Default: true},
		{Key: prefix + ".model", Title: "Model", About: "The model that writes for the server.", Zero: "The one the server asks for",
			Default: "", Picks: providerconfig.PickModel},
		{Key: prefix + ".provider", Title: "Provider", About: "The provider of the model that writes for the server.", Default: "",
			Picks: providerconfig.PickProvider},
		{Key: prefix + ".allowed_models", Title: "Models it may ask for", About: "The models that the server may ask to write with.",
			Help: "Such as claude-haiku-4-5, by the names the server uses.", Default: []string{}, Zero: "Any model", Entry: &config.Entry{Noun: "model"}},
		{Key: prefix + ".max_rpm", Title: "Requests per minute", About: "How many requests the server may make in one minute.", Default: 10,
			Counts: "requests", Least: config.Num(1)},
		{Key: prefix + ".timeout", Title: "Time limit", About: "How long the model may take to write one answer.", Default: 30 * time.Second},
		{Key: prefix + ".max_tokens_cap", Title: "Longest answer", About: "How long one answer for the server may be.", Default: 4096,
			Counts: "tokens", Least: config.Num(1)},
		{Key: prefix + ".max_tool_rounds", Title: "Tool rounds", About: "How many times in a row the model may use tools for one request.", Default: 5,
			Counts: "rounds", Least: config.Num(0)},
		{Key: prefix + ".expose_client_tools", Title: "Share the agent's tools", About: "Let the model use the agent's tools while it writes.",
			Default: false},
		{Key: prefix + ".log_level", Title: "Log detail", About: "How much of each request the log of icebath keeps.", Default: "info",
			Options: config.Options(
				config.Option{Value: "debug", Title: "Everything", About: "Every request and answer in full"},
				config.Option{Value: "info", Title: "Normal", About: "One line for each request and answer"},
				config.Option{Value: "warning", Title: "Problems", About: "Only what went wrong"},
				config.Option{Value: "error", Title: "Failures", About: "Only what failed"},
			)},
	}
}

func elicitationFields(prefix string) []config.Setting {
	return []config.Setting{
		{Key: prefix + ".enabled", Title: "Allow questions", About: "Let the server ask you a question while a tool of it runs.", Default: true},
		{Key: prefix + ".timeout", Title: "Time to answer", About: "How long the server waits for your answer.", Default: 5 * time.Minute},
	}
}

func identityFields(prefix string) []config.Setting {
	return []config.Setting{
		{Key: prefix + ".name", Title: "Header name", About: "The name of the header, such as X-Icebath-Profile.", Default: ""},
		{Key: prefix + ".value_from", Title: "What it holds", About: "Which value the header holds.", Default: "", Options: config.Options(
			config.Option{Value: "", Title: "The value below", About: "The text you give it"},
			config.Option{Value: "profile", Title: "The profile's name", About: "The name of the profile in use"},
		)},
		{Key: prefix + ".value", Title: "Value", About: "The text the header holds.", Default: ""},
	}
}

func oauthFields(prefix string) []config.Setting {
	return []config.Setting{
		{Key: prefix + ".client_id", Title: "Client ID", About: "The client that icebath signs in as, when the server gave you one.", Default: ""},
		{Key: prefix + ".client_secret", Title: "Client secret", About: "Your client secret for the server.", Default: "", Secret: true},
		{Key: prefix + ".scope", Title: "Scopes", About: "The permissions icebath asks for, separated by spaces.", Default: ""},
		{Key: prefix + ".flow", Title: "How you sign in", About: "Where you approve icebath.", Default: "browser", Options: config.Options(
			config.Option{Value: "browser", Title: "Browser", About: "In the browser on this computer"},
			config.Option{Value: "device", Title: "Code", About: "With a code, on another device"},
		)},
		{Key: prefix + ".client_name", Title: "Client name", About: "The name icebath gives itself when it signs up with the server.", Default: "",
			Zero: "icebath"},
		{Key: prefix + ".redirect_uri", Title: "Return address", About: "The address the browser comes back to after you approve.", Default: "",
			Zero: "Automatic"},
		{Key: prefix + ".redirect_host", Title: "Return host", About: "The host the browser comes back to.", Default: "127.0.0.1"},
		{Key: prefix + ".redirect_port", Title: "Return port", About: "The port the browser comes back to.", Default: 0, Zero: "Any free port",
			Counts: config.Plain, Least: config.Num(0), Most: config.Num(65535)},
		{Key: prefix + ".token_endpoint_auth_method", Title: "Client sign-in", About: "How icebath proves which client it is.", Default: "",
			Options: config.Options(
				config.Option{Value: "", Title: "Automatic", About: "With the secret when there is one, else without"},
				config.Option{Value: "none", Title: "None", About: "Without a secret"},
				config.Option{Value: "client_secret_post", Title: "Secret in the request", About: "The secret sent in the body"},
				config.Option{Value: "client_secret_basic", Title: "Secret in a header", About: "The secret sent as a password"},
			)},
		{Key: prefix + ".application_type", Title: "Client kind", About: "What kind of client icebath signs up as.", Default: "native",
			Options: config.Options(
				config.Option{Value: "native", Title: "This computer", About: "A program on this computer"},
				config.Option{Value: "web", Title: "Web", About: "A site on the web"},
			)},
		{Key: prefix + ".user_agent", Title: "User agent", About: "The name icebath sends with its sign-in requests.", Default: "", Zero: "icebath"},
		{Key: prefix + ".client_metadata_url", Title: "Client details address", About: "The address of the document that describes icebath.",
			Default: "", Zero: "icebath's own"},
		{Key: prefix + ".cimd", Title: "Use client details", About: "Sign up with the document that describes icebath.", Default: true},
		{Key: prefix + ".timeout", Title: "Time to approve", About: "How long icebath waits for you to approve.", Default: 5 * time.Minute},
	}
}
