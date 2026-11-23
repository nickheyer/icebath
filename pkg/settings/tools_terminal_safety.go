package settings

// Tool groups each surface may use, keyed by surface
var ToolGroups = Register(&Map{Meta: Meta{Key: "platform_toolsets", Label: "Tool groups",
	Short: "Which groups of tools the agent may call, per app.",
	Help:  "The terminal UI uses the cli entry and each chat app its own. /tools changes the current one.",
	Group: GroupTools, Order: 10, Apply: NewSession},
	Value: &List{Meta: Meta{Key: "platform_toolsets.*"}}})

// Web search service
var WebSearch = Register(&Enum{Meta: Meta{Key: "web.backend", Label: "Web search",
	Short: "Which service answers the agent's web searches.",
	Help:  "Automatic picks the first service with a key, else a free one.",
	Group: GroupTools, Order: 20},
	Open: true,
	Values: []Choice{
		{"", "Automatic", "The first service with a key, else a free one."},
		{"exa", "Exa", ""}, {"parallel", "Parallel", ""}, {"keenable", "Keenable", ""}, {"firecrawl", "Firecrawl", ""},
		{"tavily", "Tavily", ""}, {"perplexity", "Perplexity", ""}, {"searxng", "SearXNG", "Your own SearXNG server."},
		{"brave-free", "Brave", "Free Brave search."}, {"ddgs", "DuckDuckGo", "Free, no key."}, {"xai", "xAI", ""},
	}})

// Machine or sandbox shell commands run in
var WhereCommandsRun = Register(&Enum{Meta: Meta{Key: "terminal.backend", Label: "Where commands run",
	Short: "The machine or sandbox where the agent's shell commands run.",
	Group: GroupTerminal, Order: 10, Apply: NewSession},
	Default: "local", Open: true,
	Values: []Choice{
		{"local", "This machine", "Runs directly on your computer."},
		{"docker", "Docker", "A container on this machine."},
		{"ssh", "SSH", "Another machine over SSH."},
		{"modal", "Modal", "A Modal cloud sandbox."},
		{"daytona", "Daytona", "A Daytona cloud sandbox."},
		{"vercel_sandbox", "Vercel", "A Vercel cloud sandbox."},
	}})

// Folder shell commands start in
var WorkingFolder = Register(&Path{Meta: Meta{Key: "terminal.cwd", Label: "Working folder",
	Short: "Folder the agent's shell commands start in.",
	Group: GroupTerminal, Order: 20, Slash: "cwd"},
	Default: "."})

// Container image of the Docker sandbox
var ContainerImage = Register(&String{Meta: Meta{Key: "terminal.docker_image", Label: "Container image",
	Short: "Image the Docker sandbox starts from.",
	Group: GroupTerminal, Order: 30, Apply: NewSession},
	Default: "localhost/icebath-sandbox:desktop"})

// Whether the agent asks before risky commands
var ApprovalMode = Register(&Enum{Meta: Meta{Key: "approvals.mode", Label: "Approvals",
	Short: "Whether the agent asks before running risky commands.",
	Help:  "/yolo skips asking for one session without changing this.",
	Group: GroupSafety, Order: 10, Slash: "approvals"},
	Default: "smart",
	Values: []Choice{
		{"manual", "Always ask", "Ask before every risky command."},
		{"smart", "Smart", "A quick check lets clearly safe ones through."},
		{"off", "Never ask", "Run everything without asking."},
	}})

// Commands that run without asking
var AllowedCommands = Register(&List{Meta: Meta{Key: "command_allowlist", Label: "Allowed commands",
	Short: "Commands you chose to always allow, which run without asking.",
	Group: GroupSafety, Order: 20}})

// Confirmation before destructive slash commands
var ConfirmDestructive = Register(&Bool{Meta: Meta{Key: "approvals.destructive_slash_confirm", Label: "Confirm destructive",
	Short: "Ask before slash commands that delete or reset things.",
	Group: GroupSafety, Order: 30},
	Default: true})

// Personal details hidden from the model in chat apps
var RedactPII = Register(&Bool{Meta: Meta{Key: "privacy.redact_pii", Label: "Hide personal info",
	Short: "Hide who wrote a chat app message and where from the model.",
	Group: GroupSafety, Order: 40, Apply: NextTurn},
	Default: false})

// Secrets hidden from replies, session files and logs
var RedactSecrets = Register(&Bool{Meta: Meta{Key: "security.redact_secrets", Label: "Hide secrets",
	Short: "Mask API keys and tokens in replies, session files and logs.",
	Group: GroupSafety, Order: 50, Apply: Restart},
	Default: true})
