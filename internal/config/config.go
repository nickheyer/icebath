// Config file schema, loading, saving and managed scope of every icebath home, and the YAML dialect its files use
package config

import (
	"errors"
	"maps"

	"gopkg.in/yaml.v3"
)

// Current config schema version written as _config_version
const Version = 52

// Whole config file, default sections in written order, then keys outside the defaults, then unknown root keys
type Config struct {
	Model                          ModelSetting        `yaml:"model"`
	Providers                      Map[ProviderEntry]  `yaml:"providers"`
	FallbackProviders              FallbackChain       `yaml:"fallback_providers"`
	Fallback                       FallbackSettings    `yaml:"fallback"`
	CredentialPoolStrategies       Map[PoolStrategy]   `yaml:"credential_pool_strategies"`
	Toolsets                       []string            `yaml:"toolsets"`
	Database                       DatabaseConfig      `yaml:"database"`
	Runtime                        RuntimeConfig       `yaml:"runtime"`
	MaxConcurrentSessions          *int                `yaml:"max_concurrent_sessions" coerce:"whole"`
	MaxLiveSessions                *int                `yaml:"max_live_sessions" coerce:"whole"`
	Session                        SessionConfig       `yaml:"session"`
	Attachments                    AttachmentsConfig   `yaml:"attachments"`
	Agent                          AgentConfig         `yaml:"agent"`
	Terminal                       TerminalConfig      `yaml:"terminal"`
	Web                            WebConfig           `yaml:"web"`
	Browser                        BrowserConfig       `yaml:"browser"`
	Worktrees                      WorktreesConfig     `yaml:"worktrees"`
	Checkpoints                    CheckpointsConfig   `yaml:"checkpoints"`
	ContextFileMaxChars            *int                `yaml:"context_file_max_chars" coerce:"exact"`
	ContextFileReadTimeout         float64             `yaml:"context_file_read_timeout" coerce:"exact"`
	FileReadMaxChars               int                 `yaml:"file_read_max_chars" coerce:"exact"`
	MCPDiscoveryTimeout            float64             `yaml:"mcp_discovery_timeout"`
	MCPSingleQueryDiscoveryTimeout float64             `yaml:"mcp_single_query_discovery_timeout"`
	MCP                            MCPConfig           `yaml:"mcp"`
	ToolOutput                     ToolOutputConfig    `yaml:"tool_output"`
	ToolLoopGuardrails             ToolLoopGuardrails  `yaml:"tool_loop_guardrails"`
	ToolBudget                     ToolBudgetConfig    `yaml:"tool_budget"`
	Compression                    CompressionConfig   `yaml:"compression"`
	PromptCaching                  PromptCachingConfig `yaml:"prompt_caching"`
	OpenRouter                     OpenRouterConfig    `yaml:"openrouter"`
	Bedrock                        BedrockConfig       `yaml:"bedrock"`
	Auxiliary                      AuxiliaryConfig     `yaml:"auxiliary"`
	Display                        DisplayConfig       `yaml:"display"`
	Webapp                         WebappConfig        `yaml:"webapp"`
	Dashboard                      DashboardConfig     `yaml:"dashboard"`
	Privacy                        PrivacyConfig       `yaml:"privacy"`
	TTS                            TTSConfig           `yaml:"tts"`
	STT                            STTConfig           `yaml:"stt"`
	Voice                          VoiceConfig         `yaml:"voice"`
	Vision                         VisionConfig        `yaml:"vision"`
	WakeWord                       WakeWordConfig      `yaml:"wake_word"`
	HumanDelay                     HumanDelayConfig    `yaml:"human_delay"`
	Context                        ContextConfig       `yaml:"context"`
	Memory                         MemoryConfig        `yaml:"memory"`
	Delegation                     DelegationConfig    `yaml:"delegation"`
	PrefillMessagesFile            string              `yaml:"prefill_messages_file"`
	Goals                          GoalsConfig         `yaml:"goals"`
	Loops                          LoopsConfig         `yaml:"loops"`
	MoA                            MoAConfig           `yaml:"moa"`
	Skills                         SkillsConfig        `yaml:"skills"`
	Curator                        CuratorConfig       `yaml:"curator"`
	Honcho                         Map[Raw]            `yaml:"honcho"`
	Timezone                       string              `yaml:"timezone"`
	Slack                          SlackSection        `yaml:"slack"`
	Discord                        DiscordSection      `yaml:"discord"`
	WhatsApp                       WhatsAppSection     `yaml:"whatsapp"`
	Telegram                       TelegramSection     `yaml:"telegram"`
	Mattermost                     MattermostSection   `yaml:"mattermost"`
	Matrix                         MatrixSection       `yaml:"matrix"`
	Approvals                      ApprovalsConfig     `yaml:"approvals"`
	CommandAllowlist               []string            `yaml:"command_allowlist"`
	QuickCommands                  Map[QuickCommand]   `yaml:"quick_commands"`
	PlatformHints                  Map[PlatformHint]   `yaml:"platform_hints"`
	Plugins                        PluginsConfig       `yaml:"plugins"`
	Hooks                          HooksConfig         `yaml:"hooks"`
	HooksAutoAccept                bool                `yaml:"hooks_auto_accept" coerce:"truthwords"`
	Personalities                  Map[Personality]    `yaml:"personalities"`
	Auth                           AuthConfig          `yaml:"auth"`
	Security                       SecurityConfig      `yaml:"security"`
	Cron                           CronConfig          `yaml:"cron"`
	Kanban                         KanbanConfig        `yaml:"kanban"`
	BotMode                        BotModeConfig       `yaml:"bot_mode"`
	CodeExecution                  CodeExecutionConfig `yaml:"code_execution"`
	Tools                          ToolsConfig         `yaml:"tools"`
	Logging                        LoggingConfig       `yaml:"logging"`
	ModelCatalog                   ModelCatalogConfig  `yaml:"model_catalog"`
	ModelOverrides                 ModelOverrides      `yaml:"model_overrides"`
	ModelsDev                      ModelsDevConfig     `yaml:"models_dev"`
	Network                        NetworkConfig       `yaml:"network"`
	Monitoring                     MonitoringConfig    `yaml:"monitoring"`
	HostedRooms                    HostedRoomsConfig   `yaml:"hosted_rooms"`
	Gateway                        GatewaySection      `yaml:"gateway"`
	Streaming                      StreamingConfig     `yaml:"streaming"`
	Sessions                       SessionsConfig      `yaml:"sessions"`
	Onboarding                     OnboardingConfig    `yaml:"onboarding"`
	Telemetry                      TelemetryConfig     `yaml:"telemetry"`
	Doctor                         DoctorConfig        `yaml:"doctor"`
	Updates                        UpdatesConfig       `yaml:"updates"`
	LSP                            LSPConfig           `yaml:"lsp"`
	XSearch                        XSearchConfig       `yaml:"x_search"`
	Vault                          VaultConfig         `yaml:"vault"`
	Secrets                        SecretsConfig       `yaml:"secrets"`
	PasteCollapseThreshold         int                 `yaml:"paste_collapse_threshold"`
	PasteCollapseThresholdFallback int                 `yaml:"paste_collapse_threshold_fallback"`
	PasteCollapseCharThreshold     int                 `yaml:"paste_collapse_char_threshold"`
	BotDesktop                     BotDesktopConfig    `yaml:"bot_desktop"`
	ComputerUse                    ComputerUseConfig   `yaml:"computer_use"`
	Proxy                          ProxyConfig         `yaml:"proxy"`
	Desktop                        DesktopConfig       `yaml:"desktop"`
	Portal                         PortalConfig        `yaml:"portal"`
	Vertex                         VertexConfig        `yaml:"vertex"`
	LocalRuntime                   LocalRuntimeConfig  `yaml:"local_runtime"`
	ConfigVersion                  int                 `yaml:"_config_version"`
	Extra                          ExtraRootKeys       `yaml:",inline"`
	rest                           rest
	origin                         *loadOrigin
}

// Root keys outside the defaults that users, the setup flows and the gateway write, each absent unless written
type ExtraRootKeys struct {
	CustomProviders              CustomProviders    `yaml:"custom_providers,omitempty"`
	FallbackModel                FallbackChain      `yaml:"fallback_model,omitempty"`
	MCPServers                   Map[MCPServer]     `yaml:"mcp_servers,omitempty"`
	ImageGen                     *MediaGenConfig    `yaml:"image_gen,omitempty"`
	VideoGen                     *MediaGenConfig    `yaml:"video_gen,omitempty"`
	SmartModelRouting            *SmartModelRouting `yaml:"smart_model_routing,omitempty"`
	PlatformToolsets             Map[[]string]      `yaml:"platform_toolsets,omitempty"`
	KnownPluginToolsets          Map[[]string]      `yaml:"known_plugin_toolsets,omitempty"`
	KnownBuiltinToolsets         Map[[]string]      `yaml:"known_builtin_toolsets,omitempty"`
	ToolGatewayDeclinedTools     []string           `yaml:"tool_gateway_declined_tools,omitempty"`
	ProviderRouting              *ProviderRouting   `yaml:"provider_routing,omitempty"`
	Timeouts                     TimeoutTree        `yaml:"timeouts,omitempty"`
	Worktree                     *bool              `yaml:"worktree,omitempty" coerce:"bool"`
	WorktreeSync                 *bool              `yaml:"worktree_sync,omitempty" coerce:"bool"`
	Platforms                    Map[PlatformBlock] `yaml:"platforms,omitempty"`
	Signal                       *PlatformBlock     `yaml:"signal,omitempty"`
	GroupSessionsPerUser         Scalar             `yaml:"group_sessions_per_user,omitempty"`
	ThreadSessionsPerUser        Scalar             `yaml:"thread_sessions_per_user,omitempty"`
	STTEnabled                   Scalar             `yaml:"stt_enabled,omitempty"`
	STTEchoTranscripts           Scalar             `yaml:"stt_echo_transcripts,omitempty"`
	ResetTriggers                *[]string          `yaml:"reset_triggers,omitempty"`
	AlwaysLogLocal               Scalar             `yaml:"always_log_local,omitempty"`
	FilterSilenceNarration       Scalar             `yaml:"filter_silence_narration,omitempty"`
	WriteSessionsJSON            Scalar             `yaml:"write_sessions_json,omitempty"`
	MultiplexProfiles            Scalar             `yaml:"multiplex_profiles,omitempty"`
	ProfileRoutes                *[]ProfileRoute    `yaml:"profile_routes,omitempty"`
	RequireMention               Scalar             `yaml:"require_mention,omitempty"`
	UnauthorizedDMBehavior       string             `yaml:"unauthorized_dm_behavior,omitempty"`
	UnauthorizedDMDeclineMessage string             `yaml:"unauthorized_dm_decline_message,omitempty"`
	AllowAllUsers                Scalar             `yaml:"allow_all_users,omitempty"`
	RoomLinkURL                  string             `yaml:"room_link_url,omitempty" coerce:"exact"`
	OnAllAdaptersDown            string             `yaml:"on_all_adapters_down,omitempty"`
	SessionStoreMaxAgeDays       Scalar             `yaml:"session_store_max_age_days,omitempty"`
	SessionsDir                  string             `yaml:"sessions_dir,omitempty"`
	LoopWatchdog                 Scalar             `yaml:"loop_watchdog,omitempty"`
	LoopWatchdogProbeIntervalS   Scalar             `yaml:"loop_watchdog_probe_interval_s,omitempty"`
	LoopWatchdogProbeTimeoutS    Scalar             `yaml:"loop_watchdog_probe_timeout_s,omitempty"`
	LoopWatchdogMaxStrikes       Scalar             `yaml:"loop_watchdog_max_strikes,omitempty"`
	Legacy                       LegacyRootKeys     `yaml:",inline"`
}

// Root keys of older layouts that load normalization moves under model and agent, never overriding the new keys
type LegacyRootKeys struct {
	Provider      string     `yaml:"provider,omitempty"`
	BaseURL       string     `yaml:"base_url,omitempty"`
	ContextLength int        `yaml:"context_length,omitempty"`
	APIBase       string     `yaml:"api_base,omitempty"`
	MaxTurns      *TurnLimit `yaml:"max_turns,omitempty"`
}

type configFields Config

// Decodes the file over the receiver, keeping unknown root keys in file order
func (c *Config) UnmarshalYAML(n *yaml.Node) error {
	n = resolveAlias(n)
	if n != nil && n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	if isNull(n) {
		return nil
	}
	errs := duplicateKeys(n, "")
	errs = append(errs, decodeMapping(n, (*configFields)(c), &c.rest))
	return errors.Join(errs...)
}

// Writes the file with unknown root keys last
func (c Config) MarshalYAML() (any, error) {
	return encodeMapping(configFields(c), c.rest)
}

// Failure of the read this config stands in for, nil when it was read normally, a value saving refuses
func (c *Config) ReadError() error {
	if c.origin == nil {
		return nil
	}
	return c.origin.readErr
}

// Values of the user and managed files that could not be read into their fields, each at its dotted path
func (c *Config) FieldErrors() error {
	if c.origin == nil {
		return nil
	}
	return c.origin.fieldErrs
}

// Unknown root keys in file order, the env-style scalars the loader bridges into the environment among them
func (c Config) UnknownRootKeys() []string {
	return c.rest.names()
}

// Value of an unknown root key, nil when absent
func (c Config) UnknownRoot(key string) *yaml.Node {
	return c.rest.copyOf(key)
}

// Root platform section of any platform through the shared platform keys, ok false when neither defaults nor file hold one
func (c Config) PlatformSection(name string) (PlatformBlock, bool, error) {
	switch name {
	case "slack":
		b, err := blockView(c.Slack)
		return b, true, err
	case "discord":
		b, err := blockView(c.Discord)
		return b, true, err
	case "whatsapp":
		b, err := blockView(c.WhatsApp)
		return b, true, err
	case "telegram":
		b, err := blockView(c.Telegram)
		return b, true, err
	case "mattermost":
		b, err := blockView(c.Mattermost)
		return b, true, err
	case "matrix":
		b, err := blockView(c.Matrix)
		return b, true, err
	case "signal":
		if c.Extra.Signal == nil {
			return PlatformBlock{}, false, nil
		}
		return *c.Extra.Signal, true, nil
	}
	n := c.rest.get(name)
	if n == nil || n.Kind != yaml.MappingNode {
		return PlatformBlock{}, false, nil
	}
	var b PlatformBlock
	err := b.UnmarshalYAML(n)
	return b, true, err
}

// Fresh config holding every default, sharing nothing with any other call
func Default() *Config {
	return &Config{
		Model:                          ModelSetting{Bare: true},
		FallbackProviders:              FallbackChain{Entries: []FallbackEntry{}},
		Fallback:                       FallbackSettings{MinSwitchResetSeconds: 0},
		Toolsets:                       []string{"icebath-cli"},
		Database:                       DatabaseConfig{JournalMode: JournalWAL},
		Runtime:                        RuntimeConfig{NofileSoftLimit: new(Int(4096)), MallocArenaMax: new(Int(4))},
		MaxLiveSessions:                new(16),
		Session:                        SessionConfig{TerminalContinue: true},
		Attachments:                    AttachmentsConfig{Storage: AttachIcebathHome},
		Agent:                          defaultAgent(),
		Terminal:                       defaultTerminal(),
		Web:                            defaultWeb(),
		Browser:                        defaultBrowser(),
		Worktrees:                      defaultWorktrees(),
		Checkpoints:                    defaultCheckpoints(),
		ContextFileReadTimeout:         5.0,
		FileReadMaxChars:               100_000,
		MCPDiscoveryTimeout:            1.5,
		MCPSingleQueryDiscoveryTimeout: 15.0,
		MCP:                            MCPConfig{AutoReloadOnConfigChange: true, DiscoveryConcurrency: 4},
		ToolOutput:                     ToolOutputConfig{MaxBytes: 50000, MaxLines: 2000, MaxLineLength: 2000},
		ToolLoopGuardrails:             defaultGuardrails(),
		ToolBudget:                     ToolBudgetConfig{MCPResultSizeChars: 50000},
		Compression:                    defaultCompression(),
		PromptCaching:                  PromptCachingConfig{CacheTTL: new(Text("5m"))},
		OpenRouter:                     OpenRouterConfig{ResponseCache: true, ResponseCacheTTL: 300, MinCodingScore: Float(0.65)},
		Bedrock:                        defaultBedrock(),
		Auxiliary:                      defaultAuxiliary(),
		Display:                        defaultDisplay(),
		Webapp:                         WebappConfig{Host: "127.0.0.1", Port: 9120},
		Dashboard:                      defaultDashboard(),
		TTS:                            defaultTTS(),
		STT:                            defaultSTT(),
		Voice:                          defaultVoice(),
		Vision:                         VisionConfig{EmbedTargetBytes: 256 * 1024, ReplayRecentImages: Int(3)},
		WakeWord:                       defaultWakeWord(),
		HumanDelay:                     HumanDelayConfig{Mode: DelayOff, MinMS: 800, MaxMS: 2500},
		Context:                        ContextConfig{Engine: "compressor", MemoryTrim: MemoryTrimConfig{Enabled: true, CooldownSeconds: 60.0, LogEveryN: 1}},
		Memory:                         MemoryConfig{MemoryEnabled: true, UserProfileEnabled: true, MemoryCharLimit: 2200, UserCharLimit: 1375, NudgeInterval: 10},
		Delegation:                     defaultDelegation(),
		Goals:                          GoalsConfig{MaxTurns: 20},
		Loops:                          LoopsConfig{MinIntervalSeconds: 30, MaxTicks: 100, SelfPacedFloorSeconds: 60, SelfPacedCeilingSeconds: 900},
		MoA:                            defaultMoA(),
		Skills:                         defaultSkills(),
		Curator:                        defaultCurator(),
		Slack:                          defaultSlack(),
		Discord:                        defaultDiscord(),
		Telegram:                       TelegramSection{},
		Mattermost:                     MattermostSection{RequireMention: true},
		Matrix:                         MatrixSection{RequireMention: true},
		Approvals:                      defaultApprovals(),
		CommandAllowlist:               []string{},
		Plugins:                        defaultPlugins(),
		Auth:                           AuthConfig{AdoptExternalLogins: true, CodexLoginFlow: LoginDeviceCode},
		Security:                       defaultSecurity(),
		Cron:                           defaultCron(),
		Kanban:                         defaultKanban(),
		BotMode:                        BotModeConfig{EnvelopeTTLSeconds: 900, TurnWaitSeconds: 120},
		CodeExecution:                  CodeExecutionConfig{Mode: CodeExecProject, KernelIdleTimeout: 1800, MaxSessionKernels: 4},
		Tools:                          defaultTools(),
		Logging:                        LoggingConfig{Level: LogInfo, MaxSizeMB: 5, BackupCount: 3},
		ModelCatalog:                   ModelCatalogConfig{Enabled: true, URL: "https://icebath.app/docs/api/model-catalog.json", TTLMinutes: 20},
		Monitoring:                     defaultMonitoring(),
		HostedRooms:                    HostedRoomsConfig{Discussion: RoomDiscussion{MaxRounds: 3, MaxMessages: 10}},
		Gateway:                        defaultGateway(),
		Streaming:                      defaultStreaming(),
		Sessions:                       defaultSessions(),
		Onboarding:                     OnboardingConfig{ProfileBuild: ProfileBuildAsk},
		Telemetry:                      TelemetryConfig{SharedMetrics: SharedMetrics{Endpoint: "https://telemetry.icebath.app/v1/telemetry"}},
		Doctor:                         DoctorConfig{LiveProbeTimeout: 10},
		Updates:                        defaultUpdates(),
		LSP:                            defaultLSP(),
		XSearch:                        XSearchConfig{Model: "grok-4.5", TimeoutSeconds: 180, Retries: 2},
		Vault:                          defaultVault(),
		Secrets:                        defaultSecrets(),
		PasteCollapseThreshold:         5,
		PasteCollapseThresholdFallback: 5,
		PasteCollapseCharThreshold:     2000,
		BotDesktop:                     BotDesktopConfig{Geometry: "1440x900", MinFreeMemoryMB: 1536, IdleStopMinutes: 30, Placement: PlacementAuto},
		ComputerUse:                    ComputerUseConfig{MaxImageDimension: 1456, CaptureAfterMode: CaptureSOM, AXMaxElements: 200, PermissionMode: PermissionStandard},
		Proxy:                          ProxyConfig{TunnelPort: 9090, AutoInstall: true, CredentialSource: CredentialEnv, EnforceOnDocker: true, ExtraAllowedHosts: []string{}},
		Desktop:                        defaultDesktop(),
		Portal:                         PortalConfig{KeepaliveIntervalSeconds: 900, AnthropicWire: WireChat, Guest: true},
		Vertex:                         VertexConfig{Region: "global"},
		LocalRuntime:                   LocalRuntimeConfig{Backend: RuntimeAuto, ModelsMax: 4, DetectPorts: []int{}},
		ConfigVersion:                  Version,
	}
}

// Documentation of every key by dotted path, a fresh map on each call
func Docs() map[string]string {
	out := map[string]string{}
	for _, part := range []map[string]string{
		modelDocs(), coreDocs(), agentDocs(), terminalDocs(), webDocs(), workspaceDocs(), compressionDocs(),
		auxiliaryDocs(), displayDocs(), dashboardDocs(), ttsDocs(), speechDocs(), memoryDocs(), moaDocs(),
		skillsDocs(), platformDocs(), approvalDocs(), pluginDocs(), cronDocs(), toolsDocs(), gatewayDocs(),
		maintenanceDocs(), secretsDocs(), desktopDocs(),
	} {
		maps.Copy(out, part)
	}
	return out
}

// Child key order of every mapping a new config file writes, by dotted path, the root under the empty path
func KeyOrder() (map[string][]string, error) {
	var n yaml.Node
	if err := n.Encode(Default()); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	var walk func(path string, m *yaml.Node)
	walk = func(path string, m *yaml.Node) {
		if m.Kind != yaml.MappingNode {
			return
		}
		keys := make([]string, 0, len(m.Content)/2)
		for i := 0; i+1 < len(m.Content); i += 2 {
			k := m.Content[i].Value
			keys = append(keys, k)
			child := k
			if path != "" {
				child = path + "." + k
			}
			walk(child, m.Content[i+1])
		}
		out[path] = keys
	}
	walk("", &n)
	return out, nil
}
