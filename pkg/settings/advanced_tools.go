package settings

import "time"

// Tool groups to switch off for every chat
var AgentDisabledToolsets = Register(&List{Meta: Meta{Key: "agent.disabled_toolsets", Label: "Disabled toolsets", Short: "Tool groups to switch off for every chat.", Help: "Applies on top of Tool groups.", Group: GroupTools, Order: 1000, Advanced: true}})

// Start a hidden desktop on its own when a tool needs a screen
var BotDesktopAutoStart = Register(&Bool{Meta: Meta{Key: "bot_desktop.auto_start", Label: "Auto start desktop", Short: "Start a hidden desktop on its own when a tool needs a screen.", Help: "Only applies on a Linux machine with no display and the needed packages installed.", Group: GroupTools, Order: 1010, Advanced: true}, Default: false})

// Screen size of the agent's own desktop, as width x height
var BotDesktopGeometry = Register(&String{Meta: Meta{Key: "bot_desktop.geometry", Label: "Desktop size", Short: "Screen size of the agent's own desktop, as width x height.", Group: GroupTools, Order: 1020, Advanced: true}, Default: "1440x900"})

// Stop the agent's desktop after this long without use
var BotDesktopIdleStopMinutes = Register(&Duration{Meta: Meta{Key: "bot_desktop.idle_stop_minutes", Label: "Desktop idle stop", Short: "Stop the agent's desktop after this long without use.", Help: "Zero or less keeps it running.", Group: GroupTools, Order: 1030, Advanced: true}, Default: 30 * time.Minute, Unit: time.Minute})

// Refuse to start a desktop when free memory is below this
var BotDesktopMinFreeMemoryMb = Register(&Int{Meta: Meta{Key: "bot_desktop.min_free_memory_mb", Label: "Min free memory", Short: "Refuse to start a desktop when free memory is below this.", Help: "Zero turns the check off.", Group: GroupTools, Order: 1040, Advanced: true}, Default: 1536, Unit: "MB", Min: at(0)})

// Where the agent's desktop runs
var BotDesktopPlacement = Register(&Enum{Meta: Meta{Key: "bot_desktop.placement", Label: "Desktop placement", Short: "Where the agent's desktop runs.", Group: GroupTools, Order: 1050, Advanced: true}, Default: "auto", Values: []Choice{{"auto", "Automatic", "Pick based on where commands run."}, {"terminal", "With commands", "Run inside the same place as commands."}, {"gateway", "This computer", "Run on the machine running the app."}}})

// Where the browser tool runs its pages
var BrowserCloudProvider = Register(&String{Meta: Meta{Key: "browser.cloud_provider", Label: "Browser service", Short: "Where web pages open: local, or a cloud browser service.", Group: GroupTools, Order: 1005, Advanced: true}, Default: "local"})

// Let the browser open local and private network addresses
var BrowserAllowPrivateUrls = Register(&Bool{Meta: Meta{Key: "browser.allow_private_urls", Label: "Allow private addresses", Short: "Let the browser open local and private network addresses.", Group: GroupTools, Order: 1060, Advanced: true}, Default: false})

// Use the local browser for private addresses instead of a cloud one
var BrowserAutoLocalForPrivateUrls = Register(&Bool{Meta: Meta{Key: "browser.auto_local_for_private_urls", Label: "Local for private", Short: "Use the local browser for private addresses instead of a cloud one.", Group: GroupTools, Order: 1070, Advanced: true}, Default: true})

// Ask Browserbase for its advanced stealth browser
var BrowserBrowserbaseAdvancedStealth = Register(&Bool{Meta: Meta{Key: "browser.browserbase.advanced_stealth", Label: "Browserbase stealth", Short: "Ask Browserbase for its advanced stealth browser.", Help: "Needs a Browserbase plan that includes it.", Group: GroupTools, Order: 1072, Advanced: true, Env: "BROWSERBASE_ADVANCED_STEALTH"}, Default: false})

// Keep a Browserbase session alive between steps
var BrowserBrowserbaseKeepAlive = Register(&Bool{Meta: Meta{Key: "browser.browserbase.keep_alive", Label: "Browserbase keep alive", Short: "Keep a Browserbase session alive between steps.", Help: "Dropped by itself when the plan does not include it.", Group: GroupTools, Order: 1074, Advanced: true, Env: "BROWSERBASE_KEEP_ALIVE"}, Default: true})

// Route Browserbase sessions through its proxies
var BrowserBrowserbaseProxies = Register(&Bool{Meta: Meta{Key: "browser.browserbase.proxies", Label: "Browserbase proxies", Short: "Route Browserbase sessions through its proxies.", Help: "Dropped by itself when the plan does not include them.", Group: GroupTools, Order: 1076, Advanced: true, Env: "BROWSERBASE_PROXIES"}, Default: true})

// How long a Browserbase session may run before it ends
var BrowserBrowserbaseSessionTimeout = Register(&Duration{Meta: Meta{Key: "browser.browserbase.session_timeout", Label: "Browserbase session time", Short: "How long a Browserbase session may run before it ends.", Help: "Zero or unset uses the project's own limit.", Group: GroupTools, Order: 1078, Advanced: true, Env: "BROWSERBASE_SESSION_TIMEOUT"}, Null: "the project's own limit"})

// Options for the Camofox browser backend
var BrowserCamofox = Register(&Map{Meta: Meta{Key: "browser.camofox", Label: "Camofox options", Short: "Options for the Camofox browser backend.", Help: "A block of sub-settings such as managed_persistence, cookies_dir, timeout and temperature.", Group: GroupTools, Order: 1080, Advanced: true}})

// Attach to an already running browser at this address
var BrowserCdpUrl = Register(&String{Meta: Meta{Key: "browser.cdp_url", Label: "Browser attach address", Short: "Attach to an already running browser at this address.", Help: "A live /browser connect wins over this.", Group: GroupTools, Order: 1090, Advanced: true, Env: "BROWSER_CDP_URL"}, Default: ""})

// Extra command line flags added to every Chromium launch
var BrowserChromiumArgs = Register(&List{Meta: Meta{Key: "browser.chromium_args", Label: "Extra browser flags", Short: "Extra command line flags added to every Chromium launch.", Group: GroupTools, Order: 1100, Advanced: true}})

// How long to wait for the browser to start and answer
var BrowserCommandTimeout = Register(&Duration{Meta: Meta{Key: "browser.command_timeout", Label: "Browser start timeout", Short: "How long to wait for the browser to start and answer.", Help: "Never less than 5 seconds.", Group: GroupTools, Order: 1110, Advanced: true}, Default: 30 * time.Second, Min: at(5 * time.Second)})

// How the browser handles alert, confirm and prompt pop-ups
var BrowserDialogPolicy = Register(&Enum{Meta: Meta{Key: "browser.dialog_policy", Label: "Page dialogs", Short: "How the browser handles alert, confirm and prompt pop-ups.", Group: GroupTools, Order: 1120, Advanced: true}, Default: "must_respond", Values: []Choice{{"must_respond", "Ask the agent", "Agent must answer each dialog."}, {"auto_dismiss", "Dismiss", "Close dialogs without accepting."}, {"auto_accept", "Accept", "Accept every dialog."}}})

// How long a pop-up waits for an answer before it is dealt with
var BrowserDialogTimeoutS = Register(&Duration{Meta: Meta{Key: "browser.dialog_timeout_s", Label: "Dialog wait", Short: "How long a pop-up waits for an answer before it is dealt with.", Help: "Zero or less uses the default.", Group: GroupTools, Order: 1130, Advanced: true}, Default: 5 * time.Minute})

// Which local browser engine the tools drive
var BrowserEngine = Register(&Enum{Meta: Meta{Key: "browser.engine", Label: "Browser engine", Short: "Which local browser engine the tools drive.", Group: GroupTools, Order: 1140, Advanced: true}, Default: "auto", Values: []Choice{{"auto", "Chromium", "Standard Chromium browser."}, {"lightpanda", "Lightpanda", "Lightweight headless engine."}, {"chrome", "Chrome", "Same as Chromium."}}})

// One folder browser code keeps its files in for every task
var BrowserExecWorkspace = Register(&Path{Meta: Meta{Key: "browser.exec_workspace", Label: "Browser code folder", Short: "One folder browser code keeps its files in for every task.", Help: "Empty gives each task its own folder in the icebath cache. A relative path is placed under the icebath home folder.", Group: GroupTools, Order: 1145, Advanced: true, Env: "BH_AGENT_WORKSPACE"}, Default: ""})

// Use this browser program instead of the bundled one
var BrowserExecutablePath = Register(&String{Meta: Meta{Key: "browser.executable_path", Label: "Browser program", Short: "Use this browser program instead of the bundled one.", Group: GroupTools, Order: 1150, Advanced: true}, Default: ""})

// How long a Firecrawl browser session lives
var BrowserFirecrawlTtl = Register(&Duration{Meta: Meta{Key: "browser.firecrawl.ttl", Label: "Firecrawl session life", Short: "How long a Firecrawl browser session lives.", Group: GroupTools, Order: 1155, Advanced: true, Env: "FIRECRAWL_BROWSER_TTL"}, Default: 5 * time.Minute, Min: at(time.Second)})

// Show the browser window instead of running it hidden
var BrowserHeaded = Register(&Bool{Meta: Meta{Key: "browser.headed", Label: "Show browser window", Short: "Show the browser window instead of running it hidden.", Group: GroupTools, Order: 1160, Advanced: true}, Default: false})

// Close an unused browser session after this long
var BrowserInactivityTimeout = Register(&Duration{Meta: Meta{Key: "browser.inactivity_timeout", Label: "Browser idle close", Short: "Close an unused browser session after this long.", Help: "Never less than 30 seconds.", Group: GroupTools, Order: 1170, Advanced: true, Env: "BROWSER_INACTIVITY_TIMEOUT"}, Default: 2 * time.Minute, Min: at(30 * time.Second)})

// Close your own browser by itself when the agent needs its profile
var BrowserRealProfileAutoclose = Register(&Bool{Meta: Meta{Key: "browser.real_profile_autoclose", Label: "Close real browser", Short: "Close your own browser by itself when the agent needs its profile.", Help: "Only matters when using your real browser profile is allowed.", Group: GroupTools, Order: 1180, Advanced: true}, Default: false})

// Folder name of the browser profile to borrow, such as Profile 2
var BrowserRealProfilePin = Register(&String{Meta: Meta{Key: "browser.real_profile_pin", Label: "Real profile name", Short: "Folder name of the browser profile to borrow, such as Profile 2.", Help: "Empty uses the profile you used last. A name that does not exist stops the borrow.", Group: GroupTools, Order: 1190, Advanced: true}, Default: ""})

// Mark the tab the agent drives so you can tell it apart
var BrowserTabMarker = Register(&Bool{Meta: Meta{Key: "browser.tab_marker", Label: "Mark agent tab", Short: "Mark the tab the agent drives so you can tell it apart.", Group: GroupTools, Order: 1195, Advanced: true, Env: "BH_TAB_MARKER"}, Default: true})

// Allow the agent to use a copy of your own browser profile and logins
var BrowserUseRealProfile = Register(&Bool{Meta: Meta{Key: "browser.use_real_profile", Label: "Use real profile", Short: "Allow the agent to use a copy of your own browser profile and logins.", Group: GroupTools, Order: 1200, Advanced: true}, Default: false})

// Keep one browser profile in this folder for every session
var BrowserUserDataDir = Register(&String{Meta: Meta{Key: "browser.user_data_dir", Label: "Browser profile folder", Short: "Keep one browser profile in this folder for every session.", Help: "A relative path is placed under the Icebath home folder.", Group: GroupTools, Order: 1210, Advanced: true}, Default: ""})

// Close an idle code session after this long
var CodeExecutionKernelIdleTimeout = Register(&Duration{Meta: Meta{Key: "code_execution.kernel_idle_timeout", Label: "Kernel idle close", Short: "Close an idle code session after this long.", Help: "Never less than 1 second.", Group: GroupTools, Order: 1220, Advanced: true}, Default: 30 * time.Minute, Min: at(time.Second)})

// Most tool calls one code run may make
var CodeExecutionMaxToolCalls = Register(&Int{Meta: Meta{Key: "code_execution.max_tool_calls", Label: "Script tool calls", Short: "Most tool calls one code run may make.", Group: GroupTools, Order: 1225, Advanced: true}, Default: 50, Min: at(0), Unit: "calls"})

// How many code sessions can stay open at once
var CodeExecutionMaxSessionKernels = Register(&Int{Meta: Meta{Key: "code_execution.max_session_kernels", Label: "Max code sessions", Short: "How many code sessions can stay open at once.", Group: GroupTools, Order: 1230, Advanced: true}, Default: 4, Min: at(1)})

// Where scripts run: your working folder or a scratch one
var CodeExecutionMode = Register(&Enum{Meta: Meta{Key: "code_execution.mode", Label: "Code run folder", Short: "Where scripts run: your working folder or a scratch one.", Group: GroupTools, Order: 1240, Advanced: true}, Default: "project", Values: []Choice{{"project", "Project folder", "Scripts run in your working folder."}, {"strict", "Scratch folder", "Scripts run in a temporary folder."}}})

// Longest one code run may take before it is stopped
var CodeExecutionTimeout = Register(&Duration{Meta: Meta{Key: "code_execution.timeout", Label: "Code run limit", Short: "Longest one code run may take before it is stopped.", Group: GroupTools, Order: 1245, Advanced: true}, Default: 5 * time.Minute})

// Accept a driver app that lacks the expected publisher signature
var ComputerUseAllowUnsignedDriver = Register(&Bool{Meta: Meta{Key: "computer_use.allow_unsigned_driver", Label: "Allow unsigned driver", Short: "Accept a driver app that lacks the expected publisher signature.", Help: "Only for locally built drivers on macOS.", Group: GroupTools, Order: 1250, Advanced: true}, Default: false})

// Start the computer-use driver on its own when first needed
var ComputerUseAutostart = Register(&Bool{Meta: Meta{Key: "computer_use.autostart", Label: "Auto start driver", Short: "Start the computer-use driver each time you log in, on Windows.", Group: GroupTools, Order: 1260, Advanced: true}, Default: false})

// Most on-screen items listed per accessibility snapshot
var ComputerUseAxMaxElements = Register(&Int{Meta: Meta{Key: "computer_use.ax_max_elements", Label: "Max screen elements", Short: "Most on-screen items listed per accessibility snapshot.", Help: "Zero means no limit.", Group: GroupTools, Order: 1270, Advanced: true}, Default: 200, Unit: "elements", Min: at(0)})

// Which computer-use provider controls the screen
var ComputerUseBackend = Register(&String{Meta: Meta{Key: "computer_use.backend", Label: "Computer use backend", Short: "Which computer-use provider controls the screen.", Help: "Names a built-in or plugin provider. Unknown names stop the tool with a list of the available ones.", Group: GroupTools, Order: 1280, Advanced: true}, Default: "cua"})

// Path to a file limiting what the driver may do
var ComputerUseCapabilityManifest = Register(&String{Meta: Meta{Key: "computer_use.capability_manifest", Label: "Capability file", Short: "Path to a file limiting what the driver may do.", Help: "Used only outside the standard permission mode.", Group: GroupTools, Order: 1290, Advanced: true}, Default: ""})

// What each action sends back after it runs
var ComputerUseCaptureAfterMode = Register(&Enum{Meta: Meta{Key: "computer_use.capture_after_mode", Label: "Follow-up capture", Short: "What each action sends back after it runs.", Group: GroupTools, Order: 1300, Advanced: true}, Default: "som", Values: []Choice{{"som", "Labeled shot", "Screenshot with numbered elements."}, {"vision", "Screenshot", "Plain screenshot only."}, {"ax", "Element list", "Text list of on-screen elements."}}})

// Let the computer-use driver send usage data
var ComputerUseCuaTelemetry = Register(&Bool{Meta: Meta{Key: "computer_use.cua_telemetry", Label: "Driver telemetry", Short: "Let the computer-use driver send usage data.", Group: GroupTools, Order: 1310, Advanced: true}, Default: false})

// Run this computer-use driver program instead of the managed one
var ComputerUseDriverCommand = Register(&String{Meta: Meta{Key: "computer_use.driver_command", Label: "Driver program", Short: "Run this computer-use driver program instead of the managed one.", Help: "Empty uses the cua-driver icebath installs.", Group: GroupTools, Order: 1315, Advanced: true, Env: "ICEBATH_CUA_DRIVER_CMD"}, Default: ""})

// Longest side of a screenshot sent to the model, in pixels
var ComputerUseMaxImageDimension = Register(&Int{Meta: Meta{Key: "computer_use.max_image_dimension", Label: "Max screenshot edge", Short: "Longest side of a screenshot sent to the model, in pixels.", Help: "Zero or less turns the cap off.", Group: GroupTools, Order: 1320, Advanced: true}, Default: 1456, Unit: "pixels"})

// Use Wayland directly instead of the X11 compatibility layer
var ComputerUseNativeWayland = Register(&Bool{Meta: Meta{Key: "computer_use.native_wayland", Label: "Native Wayland", Short: "Use Wayland directly instead of the X11 compatibility layer.", Group: GroupTools, Order: 1330, Advanced: true}, Default: false})

// Turn the on-screen cursor overlay off
var ComputerUseNoOverlay = Register(&Bool{Meta: Meta{Key: "computer_use.no_overlay", Label: "Hide overlay", Short: "Turn the on-screen cursor overlay off.", Group: GroupTools, Order: 1340, Advanced: true}, Null: "decided automatically"})

// How much of the screen the agent may control
var ComputerUsePermissionMode = Register(&Enum{Meta: Meta{Key: "computer_use.permission_mode", Label: "Computer use access", Short: "How much of the screen the agent may control.", Help: "Any value other than bounded is treated as standard.", Group: GroupTools, Order: 1350, Advanced: true}, Default: "standard", Values: []Choice{{"standard", "Standard", "Normal driver access."}, {"bounded", "Bounded", "Limited by the capability file."}}})

// Move a skill to the archive after this many idle days
var CuratorArchiveAfterDays = Register(&Duration{Meta: Meta{Key: "curator.archive_after_days", Label: "Archive after", Short: "Move a skill to the archive after this many idle days.", Help: "Counted in days, at least 1.", Group: GroupTools, Order: 1360, Advanced: true}, Default: 30 * 24 * time.Hour, Unit: 24 * time.Hour, Min: at(24 * time.Hour), DefaultBelow: true})

// Delete archived skills older than this many days
var CuratorArchiveTtlDays = Register(&Duration{Meta: Meta{Key: "curator.archive_ttl_days", Label: "Delete archive after", Short: "Delete archived skills older than this many days.", Help: "Zero keeps archived skills forever.", Group: GroupTools, Order: 1370, Advanced: true}, Default: 0, Unit: 24 * time.Hour})

// Save a snapshot of your skills before each curator run
var CuratorBackupEnabled = Register(&Bool{Meta: Meta{Key: "curator.backup.enabled", Label: "Back up first", Short: "Save a snapshot of your skills before each curator run.", Group: GroupTools, Order: 1380, Advanced: true}, Default: true})

// How many skill snapshots to keep
var CuratorBackupKeep = Register(&Int{Meta: Meta{Key: "curator.backup.keep", Label: "Snapshots kept", Short: "How many skill snapshots to keep.", Help: "At least 1.", Group: GroupTools, Order: 1390, Advanced: true}, Default: 2, Min: at(1)})

// Let the curator merge related skills with a model pass
var CuratorConsolidate = Register(&Bool{Meta: Meta{Key: "curator.consolidate", Label: "Merge skills", Short: "Let the curator merge related skills with a model pass.", Help: "Off means each run only archives idle skills.", Group: GroupTools, Order: 1400, Advanced: true}, Default: false})

// Automatically tidy up unused skills in the background
var CuratorEnabled = Register(&Bool{Meta: Meta{Key: "curator.enabled", Label: "Skill curator", Short: "Automatically tidy up unused skills in the background.", Group: GroupTools, Order: 1410, Advanced: true}, Default: true})

// How long between automatic curator runs
var CuratorIntervalHours = Register(&Duration{Meta: Meta{Key: "curator.interval_hours", Label: "Curator interval", Short: "How long between automatic curator runs.", Help: "At least 1 hour.", Group: GroupTools, Order: 1420, Advanced: true}, Default: 7 * 24 * time.Hour, Unit: time.Hour, Min: at(time.Hour), DefaultBelow: true})

// How long you must be idle before an automatic run starts
var CuratorMinIdleHours = Register(&Duration{Meta: Meta{Key: "curator.min_idle_hours", Label: "Curator idle wait", Short: "How long you must be idle before an automatic run starts.", Group: GroupTools, Order: 1430, Advanced: true}, Default: 2 * time.Hour, Unit: time.Hour})

// Let the curator archive unused built-in skills too
var CuratorPruneBuiltins = Register(&Bool{Meta: Meta{Key: "curator.prune_builtins", Label: "Prune built-in skills", Short: "Let the curator archive unused built-in skills too.", Group: GroupTools, Order: 1440, Advanced: true}, Default: false})

// Mark a skill stale after this many idle days
var CuratorStaleAfterDays = Register(&Duration{Meta: Meta{Key: "curator.stale_after_days", Label: "Stale after", Short: "Mark a skill stale after this many idle days.", Help: "Counted in days, at least 1.", Group: GroupTools, Order: 1450, Advanced: true}, Default: 14 * 24 * time.Hour, Unit: 24 * time.Hour, Min: at(24 * time.Hour), DefaultBelow: true})

// Most characters returned from one file read
var FileReadMaxChars = Register(&Int{Meta: Meta{Key: "file_read_max_chars", Label: "File read limit", Short: "Most characters returned from one file read.", Help: "Longer files are cut at a whole line.", Group: GroupTools, Order: 1460, Advanced: true}, Default: 100000, Unit: "chars", Min: at(1), DefaultBelow: true})

// Read scanned PDF pages through Firecrawl when its key is set
var FileToolsHostedOcr = Register(&Bool{Meta: Meta{Key: "file_tools.hosted_ocr", Label: "Hosted PDF reading", Short: "Read scanned PDF pages through Firecrawl when its key is set.", Group: GroupTools, Order: 1465, Advanced: true}, Default: true})

// Commands to run on agent events, grouped by event name
var Hooks = Register(&Map{Meta: Meta{Key: "hooks", Label: "Shell hooks", Short: "Commands to run on agent events, grouped by event name.", Help: "Each event holds a list of hook definitions. Unknown event names are ignored with a warning.", Group: GroupTools, Order: 1470, Advanced: true}})

// Save long hook and plugin output to a file and keep a preview
var HooksOutputSpillEnabled = Register(&Bool{Meta: Meta{Key: "hooks.output_spill.enabled", Label: "Save long hook output", Short: "Save long hook and plugin output to a file and keep a preview.", Group: GroupTools, Order: 1471, Advanced: true}, Default: true})

// Length above which hook output is saved to a file
var HooksOutputSpillMaxChars = Register(&Int{Meta: Meta{Key: "hooks.output_spill.max_chars", Label: "Hook output limit", Short: "Length above which hook output is saved to a file.", Group: GroupTools, Order: 1472, Advanced: true}, Default: 10000, Unit: "chars", Min: at(1), DefaultBelow: true})

// Characters from the start of saved hook output kept in the preview
var HooksOutputSpillPreviewHead = Register(&Int{Meta: Meta{Key: "hooks.output_spill.preview_head", Label: "Preview start", Short: "Characters from the start of saved hook output kept in the preview.", Group: GroupTools, Order: 1473, Advanced: true}, Default: 500, Unit: "chars", Min: at(0), DefaultBelow: true})

// Characters from the end of saved hook output kept in the preview
var HooksOutputSpillPreviewTail = Register(&Int{Meta: Meta{Key: "hooks.output_spill.preview_tail", Label: "Preview end", Short: "Characters from the end of saved hook output kept in the preview.", Group: GroupTools, Order: 1474, Advanced: true}, Default: 500, Unit: "chars", Min: at(0), DefaultBelow: true})

// Folder long hook output is saved in
var HooksOutputSpillDirectory = Register(&Path{Meta: Meta{Key: "hooks.output_spill.directory", Label: "Hook output folder", Short: "Folder long hook output is saved in.", Help: "Empty uses hook_outputs in the icebath home folder.", Group: GroupTools, Order: 1475, Advanced: true}, Default: ""})

// How many MCP servers connect at the same time during discovery
var McpDiscoveryConcurrency = Register(&Int{Meta: Meta{Key: "mcp.discovery_concurrency", Label: "MCP connect limit", Short: "How many MCP servers connect at the same time during discovery.", Help: "Zero means no limit.", Group: GroupTools, Order: 1480, Advanced: true}, Default: 4, Min: at(0), DefaultBelow: true})

// Folder of MCP catalog manifests used in place of the built-in catalog
var McpCatalogDir = Register(&Path{Meta: Meta{Key: "mcp.catalog_dir", Label: "MCP catalog folder", Short: "Folder of MCP catalog manifests used in place of the built-in catalog.", Help: "Empty uses the built-in catalog.", Group: GroupTools, Order: 1485, Advanced: true, Env: "ICEBATH_OPTIONAL_MCPS"}})

// Longest one call of an MCP tool may take
var TimeoutsMcpToolCall = Register(&Duration{Meta: Meta{Key: "timeouts.mcp.tool_call", Label: "MCP tool call limit", Short: "Longest one call of an MCP tool may take.", Help: "A server's own timeout wins over it. Zero or less uses the default.", Group: GroupTools, Order: 1487, Advanced: true, Apply: Restart}, Default: 5 * time.Minute})

// How long a new turn waits for MCP servers to be found
var McpDiscoveryTimeout = Register(&Duration{Meta: Meta{Key: "mcp_discovery_timeout", Label: "MCP startup wait", Short: "How long a new turn waits for MCP servers to be found.", Help: "Zero or less uses the default.", Group: GroupTools, Order: 1490, Advanced: true}, Default: 1500 * time.Millisecond})

// How long a one-shot query waits for MCP servers to be found
var McpSingleQueryDiscoveryTimeout = Register(&Duration{Meta: Meta{Key: "mcp_single_query_discovery_timeout", Label: "MCP one-shot wait", Short: "How long a one-shot query waits for MCP servers to be found.", Help: "Zero or less uses the default.", Group: GroupTools, Order: 1500, Advanced: true}, Default: 15 * time.Second})

// Install plugin updates without asking
var PluginsAutoApply = Register(&Bool{Meta: Meta{Key: "plugins.auto_apply", Label: "Auto update plugins", Short: "Install plugin updates without asking.", Group: GroupTools, Order: 1510, Advanced: true}, Default: false})

// How often to check plugins for updates
var PluginsAutoUpdateCheckHours = Register(&Duration{Meta: Meta{Key: "plugins.auto_update_check_hours", Label: "Plugin update check", Short: "How often to check plugins for updates.", Help: "Zero turns the check off.", Group: GroupTools, Order: 1520, Advanced: true}, Default: 24 * time.Hour, Unit: time.Hour})

// Longest a plugin download may take
var PluginsCloneTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "plugins.clone_timeout_seconds", Label: "Plugin download limit", Short: "Longest a plugin download may take.", Help: "Between 1 second and 1 hour.", Group: GroupTools, Order: 1530, Advanced: true}, Default: 5 * time.Minute, Min: at(time.Second), Max: at(time.Hour), DefaultBelow: true})

// Longest a plugin hook may take before it is cut off
var PluginsHookCallbackTimeout = Register(&Duration{Meta: Meta{Key: "plugins.hook_callback_timeout", Label: "Plugin hook limit", Short: "Longest a plugin hook may take before it is cut off.", Help: "Capped at 10 minutes.", Group: GroupTools, Order: 1540, Advanced: true}, Default: 30 * time.Second, Min: at(time.Duration(0)), Max: at(10 * time.Minute), DefaultBelow: true})

// Command placed before the plugin host process, such as a sandbox
var PluginsHostLauncher = Register(&List{Meta: Meta{Key: "plugins.host.launcher", Label: "Plugin launcher", Short: "Command placed before the plugin host process, such as a sandbox.", Help: "A single line is split like a shell command line.", Group: GroupTools, Order: 1550, Advanced: true}, Whole: true})

// Scan plugins for risky code when they are installed or updated
var PluginsScanOnInstall = Register(&Bool{Meta: Meta{Key: "plugins.scan_on_install", Label: "Scan new plugins", Short: "Check plugins for risky code when they are installed or updated.", Group: GroupTools, Order: 1555, Advanced: true}, Default: true})

// Let plugins watch the model's reasoning as it streams
var PluginsStreamReasoningDeltas = Register(&Bool{Meta: Meta{Key: "plugins.stream_reasoning_deltas", Label: "Stream reasoning", Short: "Let plugins watch the model's reasoning as it streams.", Group: GroupTools, Order: 1557, Advanced: true}, Default: false})

// Copy detailed plugin discovery and load logs to the error output
var PluginsDebug = Register(&Bool{Meta: Meta{Key: "plugins.debug", Label: "Plugin debug log", Short: "Copy detailed plugin discovery and load logs to the error output.", Help: "Plugin failure warnings then also carry the error detail.", Group: GroupTools, Order: 1559, Advanced: true, Env: "ICEBATH_PLUGINS_DEBUG"}, Default: false})

// Longest a single plugin may take to load
var PluginsLoadTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "plugins.load_timeout_seconds", Label: "Plugin load limit", Short: "Longest a single plugin may take to load.", Help: "Zero loads plugins inline with no limit. Capped at 10 minutes.", Group: GroupTools, Order: 1560, Advanced: true}, Default: 10 * time.Second, Min: at(time.Duration(0)), Max: at(10 * time.Minute), DefaultBelow: true})

// Plugins switched on by name
var PluginsEnabled = Register(&List{Meta: Meta{Key: "plugins.enabled", Label: "Enabled plugins", Short: "Plugins you switched on, by name.", Help: "The plugins commands and /plugins keep this list.", Group: GroupTools, Order: 1560, Advanced: true, Apply: Restart}})

// Plugins switched off by name
var PluginsDisabled = Register(&List{Meta: Meta{Key: "plugins.disabled", Label: "Disabled plugins", Short: "Plugins you switched off, by name.", Help: "The plugins commands and /plugins keep this list.", Group: GroupTools, Order: 1570, Advanced: true, Apply: Restart}})

// Plugins an update switched off, each with what to undo once it fits again
var PluginsDisabledByUpdate = Register(&Map{Meta: Meta{Key: "plugins.disabled_by_update", Label: "Update-disabled plugins", Short: "Plugins an update switched off, with what to undo once they fit.", Help: "Updates keep this record; enabling a plugin again clears its entry.", Group: GroupTools, Order: 1575, Advanced: true, Apply: Restart}})

// Each plugin's own state and settings, by plugin
var PluginsEntries = Register(&Map{Meta: Meta{Key: "plugins.entries", Label: "Plugin entries", Short: "Each plugin's own state and settings, by plugin name.", Group: GroupTools, Order: 1580, Advanced: true, Apply: Restart}})

// Your own slash commands, each running a shell command or alias
var QuickCommands = Register(&Map{Meta: Meta{Key: "quick_commands", Label: "Quick commands", Short: "Your own slash commands, each running a shell command or alias.", Help: "A mapping of command name to its definition.", Group: GroupTools, Order: 1570, Advanced: true}})

// Skills loaded into every new session
var SkillsAutoLoad = Register(&List{Meta: Meta{Key: "skills.auto_load", Label: "Preloaded skills", Short: "Skills loaded into every new session.", Group: GroupTools, Order: 1580, Advanced: true}})

// Values installed skills ask for, by name
var SkillsConfig = Register(&Map{Meta: Meta{Key: "skills.config", Label: "Skill settings", Short: "Values the skills you installed ask for, by name.", Group: GroupTools, Order: 1585, Advanced: true}})

// Turns between reminders to save a reusable skill
var SkillsCreationNudgeInterval = Register(&Int{Meta: Meta{Key: "skills.creation_nudge_interval", Label: "Skill reminder spacing", Short: "Tool turns between reminders to save what worked as a skill.", Help: "Zero turns the reminders off.", Group: GroupTools, Order: 1587, Advanced: true, Apply: NewSession}, Default: 10, Min: at(0), Unit: "turns"})

// Folder where new skills are created
var SkillsCreateDir = Register(&String{Meta: Meta{Key: "skills.create_dir", Label: "New skill folder", Short: "Folder where new skills are created.", Help: "Empty uses the normal skills folder.", Group: GroupTools, Order: 1590, Advanced: true}, Default: ""})

// Skills the agent cannot use
var SkillsDisabled = Register(&List{Meta: Meta{Key: "skills.disabled", Label: "Disabled skills", Short: "Skills the agent cannot use.", Group: GroupTools, Order: 1600, Advanced: true}})

// More folders to look for skills in
var SkillsExternalDirs = Register(&List{Meta: Meta{Key: "skills.external_dirs", Label: "Extra skill folders", Short: "More folders to look for skills in.", Group: GroupTools, Order: 1610, Advanced: true}})

// Security-scan skills the agent writes and block dangerous ones
var SkillsGuardAgentCreated = Register(&Bool{Meta: Meta{Key: "skills.guard_agent_created", Label: "Scan agent skills", Short: "Security-scan skills the agent writes and block dangerous ones.", Group: GroupTools, Order: 1620, Advanced: true}, Default: false})

// Run shell snippets embedded in skill text
var SkillsInlineShell = Register(&Bool{Meta: Meta{Key: "skills.inline_shell", Label: "Skill shell snippets", Short: "Run shell snippets embedded in skill text.", Help: "Never applied to skills from the community hub.", Group: GroupTools, Order: 1630, Advanced: true}, Default: false})

// Longest an embedded shell snippet may run
var SkillsInlineShellTimeout = Register(&Duration{Meta: Meta{Key: "skills.inline_shell_timeout", Label: "Snippet time limit", Short: "Longest an embedded shell snippet may run.", Group: GroupTools, Order: 1640, Advanced: true}, Default: 10 * time.Second})

// Keep a log of every skill change by you, the agent or the curator
var SkillsLedger = Register(&Bool{Meta: Meta{Key: "skills.ledger", Label: "Skill change log", Short: "Keep a log of every skill change by you, the agent or the curator.", Group: GroupTools, Order: 1650, Advanced: true}, Default: true})

// Size at which the skill change log is trimmed
var SkillsLedgerMaxBytes = Register(&Int{Meta: Meta{Key: "skills.ledger_max_bytes", Label: "Change log size", Short: "Size at which the skill change log is trimmed.", Group: GroupTools, Order: 1660, Advanced: true}, Default: 5242880, Unit: "bytes"})

// Skills turned off for a single app, by app name
var SkillsPlatformDisabled = Register(&Map{Meta: Meta{Key: "skills.platform_disabled", Label: "Per-app disabled", Short: "Skills turned off for a single app, by app name.", Help: "A mapping of app name to a list of skill names.", Group: GroupTools, Order: 1670, Advanced: true}})

// Find skills inside the current project folder
var SkillsProjectDiscovery = Register(&Bool{Meta: Meta{Key: "skills.project_discovery", Label: "Project skills", Short: "Find skills inside the current project folder.", Help: "Project skills load only when the project is trusted.", Group: GroupTools, Order: 1680, Advanced: true}, Default: true})

// Fill in placeholders such as the skill folder in skill text
var SkillsTemplateVars = Register(&Bool{Meta: Meta{Key: "skills.template_vars", Label: "Skill variables", Short: "Fill in placeholders such as the skill folder in skill text.", Group: GroupTools, Order: 1690, Advanced: true}, Default: true})

// Show a quick safety scan of a skill when you install it
var SkillsTier1Advisory = Register(&Bool{Meta: Meta{Key: "skills.tier1_advisory", Label: "Install safety check", Short: "Show a quick safety scan of a skill when you install it.", Group: GroupTools, Order: 1700, Advanced: true}, Default: true})

// Project folders whose own skills may be loaded
var SkillsTrustedProjectDirs = Register(&List{Meta: Meta{Key: "skills.trusted_project_dirs", Label: "Trusted projects", Short: "Project folders whose own skills may be loaded.", Group: GroupTools, Order: 1710, Advanced: true}})

// Ask before the agent saves or changes a skill
var SkillsWriteApproval = Register(&Bool{Meta: Meta{Key: "skills.write_approval", Label: "Approve skill writes", Short: "Ask before the agent saves or changes a skill.", Group: GroupTools, Order: 1720, Advanced: true}, Default: false, Also: map[string]bool{"approve": true}})

// Most bytes of a tool result kept before it is cut
var ToolOutputMaxBytes = Register(&Int{Meta: Meta{Key: "tool_output.max_bytes", Label: "Tool output size", Short: "Most bytes of a tool result kept before it is cut.", Group: GroupTools, Order: 1730, Advanced: true}, Default: 50000, Unit: "bytes", Min: at(1), DefaultBelow: true})

// Most characters kept per line of a tool result
var ToolOutputMaxLineLength = Register(&Int{Meta: Meta{Key: "tool_output.max_line_length", Label: "Tool line length", Short: "Most characters kept per line of a tool result.", Group: GroupTools, Order: 1740, Advanced: true}, Default: 2000, Unit: "chars", Min: at(1), DefaultBelow: true})

// Most lines of a tool result kept before it is cut
var ToolOutputMaxLines = Register(&Int{Meta: Meta{Key: "tool_output.max_lines", Label: "Tool output lines", Short: "Most lines of a tool result kept before it is cut.", Group: GroupTools, Order: 1750, Advanced: true}, Default: 2000, Unit: "lines", Min: at(1), DefaultBelow: true})

// Largest MCP tool result kept in the chat before it is saved to a file
var ToolBudgetMcpResultSizeChars = Register(&Int{Meta: Meta{Key: "tool_budget.mcp_result_size_chars", Label: "MCP result size", Short: "Largest MCP tool result kept in the chat before it is saved to a file.", Group: GroupTools, Order: 1755, Advanced: true}, Default: 50000, Unit: "chars", Min: at(1), DefaultBelow: true})

// Tools hidden until the agent searches for them, to save context
var ToolsToolSearchDefer = Register(&List{Meta: Meta{Key: "tools.tool_search.defer", Label: "Deferred tools", Short: "Tools hidden until the agent searches for them, to save context.", Help: "An empty list keeps every tool loaded up front.", Group: GroupTools, Order: 1760, Advanced: true}, Default: []any{"computer_use", "session_search", "image_generate", "todo_list", "process_manage", "cronjob_manage", "browser_vault", "desktop_project"}})

// Whether rarely used tools are found by search instead of listed
var ToolsToolSearchEnabled = Register(&Enum{Meta: Meta{Key: "tools.tool_search.enabled", Label: "Tool search", Short: "Whether rarely used tools are found by search instead of listed.", Group: GroupTools, Order: 1770, Advanced: true}, Default: "auto", Values: []Choice{{"auto", "Automatic", "Used when it saves context."}, {"on", "On", "Always used."}, {"off", "Off", "Never used."}},
	Also: map[string]string{"true": "on", "1": "on", "yes": "on", "false": "off", "0": "off", "no": "off"}})

// Whether hidden tools are summarized in the prompt
var ToolsToolSearchListing = Register(&Enum{Meta: Meta{Key: "tools.tool_search.listing", Label: "Tool listing", Short: "Whether hidden tools are summarized in the prompt.", Group: GroupTools, Order: 1780, Advanced: true}, Default: "auto", Values: []Choice{{"auto", "Automatic", "Shown when it fits."}, {"on", "On", "Always shown."}, {"off", "Off", "Never shown."}},
	Also: map[string]string{"true": "on", "1": "on", "yes": "on", "false": "off", "0": "off", "no": "off"}})

// Most tokens spent summarizing hidden tools
var ToolsToolSearchListingMaxTokens = Register(&Int{Meta: Meta{Key: "tools.tool_search.listing_max_tokens", Label: "Listing size", Short: "Most tokens spent summarizing hidden tools.", Help: "Between 200 and 60000.", Group: GroupTools, Order: 1790, Advanced: true}, Default: 4000, Unit: "tokens", Min: at(200), Max: at(60000)})

// Most tools one search may return
var ToolsToolSearchMaxSearchLimit = Register(&Int{Meta: Meta{Key: "tools.tool_search.max_search_limit", Label: "Max search results", Short: "Most tools one search may return.", Group: GroupTools, Order: 1800, Advanced: true}, Default: 25, Min: at(1), Max: at(50)})

// How many tools a search returns when none is asked for
var ToolsToolSearchSearchDefaultLimit = Register(&Int{Meta: Meta{Key: "tools.tool_search.search_default_limit", Label: "Default search results", Short: "How many tools a search returns when none is asked for.", Help: "Never above the maximum.", Group: GroupTools, Order: 1810, Advanced: true}, Default: 5, Min: at(1), Max: at(50)})

// Share of the context window the tool summary may use
var ToolsToolSearchThresholdPct = Register(&Float{Meta: Meta{Key: "tools.tool_search.threshold_pct", Label: "Listing context share", Short: "Share of the context window the tool summary may use.", Group: GroupTools, Order: 1820, Advanced: true}, Default: 5, Unit: "%", Min: at(0.0), Max: at(100.0)})

// Only used to turn on the Kanban tools when listed
var Toolsets = Register(&List{Meta: Meta{Key: "toolsets", Label: "Toolsets", Short: "Only used to turn on the Kanban tools when listed.", Help: "Tool groups are normally chosen per platform, not here.", Group: GroupTools, Order: 1830, Advanced: true}, Default: []any{"icebath-cli"}})

// Reuse recent web search and page results
var WebCacheEnabled = Register(&Bool{Meta: Meta{Key: "web.cache_enabled", Label: "Web cache", Short: "Reuse recent web search and page results.", Group: GroupTools, Order: 1840, Advanced: true}, Default: true})

// Sites whose pages are always fetched fresh
var WebCacheExemptHosts = Register(&List{Meta: Meta{Key: "web.cache_exempt_hosts", Label: "Uncached sites", Short: "Sites whose pages are always fetched fresh.", Help: "Matches a site and its subdomains.", Group: GroupTools, Order: 1850, Advanced: true}})

// How long a cached web result is reused
var WebCacheTtlMinutes = Register(&Duration{Meta: Meta{Key: "web.cache_ttl_minutes", Label: "Web cache time", Short: "How long a cached web result is reused.", Help: "Between 1 minute and 1 day.", Group: GroupTools, Order: 1860, Advanced: true}, Default: 20 * time.Minute, Unit: time.Minute, Min: at(time.Minute), Max: at(24 * time.Hour)})

// Provider used to read web pages
var WebExtractBackend = Register(&String{Meta: Meta{Key: "web.extract_backend", Label: "Page reader provider", Short: "Provider used to read web pages.", Help: "Empty follows the main web provider.", Group: GroupTools, Order: 1870, Advanced: true}, Default: ""})

// Most characters kept from one web page
var WebExtractCharLimit = Register(&Int{Meta: Meta{Key: "web.extract_char_limit", Label: "Page text limit", Short: "Most characters kept from one web page.", Help: "Between 2000 and 500000.", Group: GroupTools, Order: 1880, Advanced: true}, Default: 15000, Unit: "chars", Min: at(2000), Max: at(500000)})

// Longest one page read may take before it is given up
var WebExtractTimeout = Register(&Duration{Meta: Meta{Key: "web.extract_timeout", Label: "Page read timeout", Short: "Longest one page read may take before it is given up.", Help: "Zero waits without a limit.", Group: GroupTools, Order: 1885, Advanced: true}, Default: 2 * time.Minute})

// Use free providers when no web search key is set
var WebKeylessFallback = Register(&Bool{Meta: Meta{Key: "web.keyless_fallback", Label: "Free web providers", Short: "Use free providers when no web search key is set.", Group: GroupTools, Order: 1890, Advanced: true}, Default: true})

// Retry with a free provider when the chosen one fails
var WebKeylessRescue = Register(&Bool{Meta: Meta{Key: "web.keyless_rescue", Label: "Free provider retry", Short: "Retry with a free provider when the chosen one fails.", Help: "Needs free web providers to be on.", Group: GroupTools, Order: 1900, Advanced: true}, Default: true})

// Per provider: use its free plan, a paid key, or decide automatically
var WebProviderTier = Register(&Map{Meta: Meta{Key: "web.provider_tier", Label: "Provider plan", Short: "Per provider: use its free plan, a paid key, or decide automatically.", Group: GroupTools, Order: 1910, Advanced: true}})

// Provider used for web search
var WebSearchBackend = Register(&String{Meta: Meta{Key: "web.search_backend", Label: "Web search provider", Short: "Provider used for web search.", Help: "Empty follows the main web provider.", Group: GroupTools, Order: 1920, Advanced: true}, Default: ""})

// Model that runs xAI web searches
var WebXaiModel = Register(&String{Meta: Meta{Key: "web.xai.model", Label: "xAI search model", Short: "Model that runs xAI web searches.", Group: GroupTools, Order: 1925, Advanced: true}, Default: "grok-build-0.1"})

// Longest to wait for an xAI web search
var WebXaiTimeout = Register(&Duration{Meta: Meta{Key: "web.xai.timeout", Label: "xAI search timeout", Short: "Longest to wait for an xAI web search.", Group: GroupTools, Order: 1926, Advanced: true}, Default: 90 * time.Second})

// Only sites xAI web searches may use
var WebXaiAllowedDomains = Register(&List{Meta: Meta{Key: "web.xai.allowed_domains", Label: "xAI allowed sites", Short: "Only sites xAI web searches may use, at most five.", Help: "Cannot be set together with xAI blocked sites.", Group: GroupTools, Order: 1927, Advanced: true}})

// Sites xAI web searches leave out
var WebXaiExcludedDomains = Register(&List{Meta: Meta{Key: "web.xai.excluded_domains", Label: "xAI blocked sites", Short: "Sites xAI web searches leave out, at most five.", Help: "Cannot be set together with xAI allowed sites.", Group: GroupTools, Order: 1928, Advanced: true}})

// Model that runs X searches
var XSearchModel = Register(&String{Meta: Meta{Key: "x_search.model", Label: "X search model", Short: "Model that runs X searches.", Group: GroupTools, Order: 1930, Advanced: true}, Default: "grok-4.5"})

// How hard the model thinks while searching X
var XSearchReasoningEffort = Register(&Enum{Meta: Meta{Key: "x_search.reasoning_effort", Label: "X search effort", Short: "How hard the model thinks while searching X.", Group: GroupTools, Order: 1940, Advanced: true}, Values: []Choice{{"low", "Low", "Fastest, least thinking."}, {"medium", "Medium", "Balanced."}, {"high", "High", "More thinking."}, {"xhigh", "Extra high", "Most thinking."}}, Null: "the model's default"})

// How many times a failed X search is retried
var XSearchRetries = Register(&Int{Meta: Meta{Key: "x_search.retries", Label: "X search retries", Short: "How many times a failed X search is retried.", Group: GroupTools, Order: 1950, Advanced: true}, Default: 2, Min: at(0)})

// Longest to wait for an X search
var XSearchTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "x_search.timeout_seconds", Label: "X search timeout", Short: "Longest to wait for an X search.", Help: "Never less than 30 seconds.", Group: GroupTools, Order: 1960, Advanced: true}, Default: 3 * time.Minute, Min: at(30 * time.Second)})

// Connected MCP servers by name
var McpServers = Register(&Map{Meta: Meta{Key: "mcp_servers", Label: "MCP servers", Short: "Tool servers the agent connects to, by name.", Group: GroupTools, Order: 2990, Advanced: true, Apply: NewSession}})

// Built-in toolsets each chat app has already been offered
var KnownBuiltinToolsets = Register(&Map{Meta: Meta{Key: "known_builtin_toolsets", Label: "Offered toolsets", Short: "Built-in toolsets each app has seen; new ones turn on.", Group: GroupTools, Order: 2991, Advanced: true}})

// Plugin toolsets each chat app has already been offered
var KnownPluginToolsets = Register(&Map{Meta: Meta{Key: "known_plugin_toolsets", Label: "Offered plugin tools", Short: "Plugin toolsets each app has seen; new ones turn on.", Group: GroupTools, Order: 2992, Advanced: true}})
