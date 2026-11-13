package plugins

const (
	HostProcessEnv     = "ICEBATH_PLUGIN_HOST_PROCESS"
	HostDenyNetworkEnv = "ICEBATH_PLUGIN_HOST_DENY_NETWORK"
)

// Context methods the host refuses with the reason the live object cannot cross
var HostUnsupportedCtxMethods = map[string]string{
	"register_login_backend":           "browser-vault fills hold a server-local context manager",
	"register_browser_page_backend":    "browser-vault fill scripts carry credentials that stay in the Icebath process",
	"register_platform":                "platform adapters run on the gateway's event loop with native SDK clients",
	"register_platform_handler":        "the handler factory receives the platform's native SDK client",
	"register_telegram_handler":        "the handler factory receives the native Telegram Application",
	"register_slack_action_handler":    "the callback receives native slack_bolt ack/body objects",
	"register_dashboard_auth_provider": "dashboard auth is owned by the launch process, not a profile",
	"register_approval_transport":      "approval transports hold the live approval request/response channel",
	"post_message":                     "the returned handle is bound to the gateway's event loop and live adapter",
	"apost_message":                    "the returned handle is bound to the gateway's event loop and live adapter",
}

// Context methods the host skips with a warning
var HostSkippedCtxMethods = map[string]string{
	"register_cli_command": "`icebath <command>` subcommands are wired into the local CLI parser",
}

// Context facades whose methods run on the parent side object
var HostRemoteFacades = map[string]bool{
	"state": true, "llm": true, "platform_actions": true, "subagent_lifecycle": true, "delegation_backends": true,
}

// Registrars whose argument may be a plugin object, mapped to the provider base it stands in for
var HostObjectBases = map[string]string{
	"register_image_gen_provider":            "agent.image_gen_provider:ImageGenProvider",
	"register_video_gen_provider":            "agent.video_gen_provider:VideoGenProvider",
	"register_web_search_provider":           "agent.web_search_provider:WebSearchProvider",
	"register_browser_provider":              "agent.browser_provider:BrowserProvider",
	"register_terminal_environment_provider": "agent.terminal_env_provider:TerminalEnvironmentProvider",
	"register_secret_source":                 "agent.secret_sources.base:SecretSource",
	"register_tts_provider":                  "agent.tts_provider:TTSProvider",
	"register_transcription_provider":        "agent.transcription_provider:TranscriptionProvider",
	"register_delegation_provider":           "agent.delegation_provider:DelegationProvider",
	"register_context_engine":                "agent.context_engine:ContextEngine",
	"register_context_reference":             "agent.context_references:ContextReferenceProvider",
	"register_memory_provider":               "agent.memory_provider:MemoryProvider",
}

// Unsupported registrars the parent serves for an in process plugin, mapped to the base their object subclasses
var HostInProcessObjectBases = map[string]string{
	"register_login_backend":           "agent.vault_backends.base:LoginBackend",
	"register_browser_page_backend":    "agent.browser_page_backend:BrowserPageBackend",
	"register_dashboard_auth_provider": "hermes_cli.dashboard_auth:DashboardAuthProvider",
	"register_platform":                "gateway.platforms.send_targets:StandaloneSendPolicy",
}

// Provider bases a category loader hands back as hosted objects though no ctx registration names them
var HostResultObjectBases = []string{"tools.computer_use.backend:ComputerUseProvider", "tools.computer_use.backend:ComputerUseBackend",
	"agent.terminal_env_provider:TerminalEnvironment"}

// Hooks whose payload reaches hosted callbacks only as opaque values
var HostDegradedHooks = map[string]string{
	"pre_gateway_dispatch": "receives the live gateway runner and session store",
}
