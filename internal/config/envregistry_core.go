package config

// Entry of one category with the password and advanced flags its category starts from
func categoryVar(category string, password, advanced bool, description, prompt string) EnvVar {
	return EnvVar{Description: description, Prompt: prompt, Password: &password, Category: category, Advanced: advanced}
}

// Provider credential entry, a secret hidden from the checklists
func providerVar(description, prompt string) EnvVar {
	return categoryVar("provider", true, true, description, prompt)
}

// Provider base URL override entry, hidden from the checklists and no secret
func baseURLVar(name, promptName string) EnvVar {
	if promptName == "" {
		promptName = name
	}
	return providerVar(name+" base URL override", promptName+" base URL (leave empty for default)").open()
}

// Tool credential entry, a secret shown in the checklists
func toolVar(description, prompt string) EnvVar {
	return categoryVar("tool", true, false, description, prompt)
}

// Messaging platform entry, shown in the checklists and no secret
func messagingVar(description, prompt string) EnvVar {
	return categoryVar("messaging", false, false, description, prompt)
}

// Bundled skill credential entry, a secret hidden from the checklists
func skillVar(description, prompt string) EnvVar {
	return categoryVar("skill", true, true, description, prompt)
}

// Agent setting entry, shown in the checklists and no secret
func settingVar(description, prompt string) EnvVar {
	return categoryVar("setting", false, false, description, prompt)
}

// Entry with the page its value is obtained from
func (v EnvVar) at(url string) EnvVar {
	v.URL = &url
	return v
}

// Entry whose value is typed and shown in the clear
func (v EnvVar) open() EnvVar {
	v.Password = new(false)
	return v
}

// Entry whose value is typed and displayed as a secret
func (v EnvVar) masked() EnvVar {
	v.Password = new(true)
	return v
}

// Entry that leaves the masking of its value to its reader
func (v EnvVar) unmarked() EnvVar {
	v.Password = nil
	return v
}

// Entry the setup checklists leave out
func (v EnvVar) hidden() EnvVar {
	v.Advanced = true
	return v
}

// Entry the setup checklists list
func (v EnvVar) shown() EnvVar {
	v.Advanced = false
	return v
}

// Entry with the model tools that need its value
func (v EnvVar) unlocks(tools ...string) EnvVar {
	v.Tools = tools
	return v
}

// Entry with the steps that lead to its value
func (v EnvVar) explained(help string) EnvVar {
	v.Help = help
	return v
}

// One registry entry under its variable name
type namedEnvVar struct {
	name string
	v    EnvVar
}

// Optional variables declared by the core in declaration order, provider and platform entries joining after them
func coreEnvVars() []namedEnvVar {
	return []namedEnvVar{
		{"ICEBATH_PORTAL_BASE_URL", baseURLVar("Icebath Portal", "")},
		{"ICEBATH_ANON_API_SECRET", providerVar(
			"Shared secret for the Icebath Portal free-tier sign-up endpoints while they are in their gated "+
				"integration phase (not needed once the gate is removed)",
			"Icebath Portal free-tier shared secret (leave empty unless given one)")},
		{"OPENROUTER_API_KEY", providerVar("OpenRouter API key (for vision, web scraping helpers, and MoA)",
			"OpenRouter API key").at("https://openrouter.ai/keys").unlocks("vision_analyze")},
		{"GOOGLE_API_KEY", providerVar("Google AI Studio API key (also recognized as GEMINI_API_KEY)",
			"Google AI Studio API key").at("https://aistudio.google.com/app/apikey")},
		{"GEMINI_API_KEY", providerVar("Google AI Studio API key (alias for GOOGLE_API_KEY)", "Gemini API key").
			at("https://aistudio.google.com/app/apikey")},
		{"GEMINI_BASE_URL", baseURLVar("Google AI Studio", "Gemini")},
		{"VERTEX_CREDENTIALS_PATH", providerVar(
			"Path to a Google Cloud service account JSON for Vertex AI (Gemini). Vertex uses "+
				"OAuth2, not a static API key — this points at the credentials icebath mints short-lived "+
				"tokens from. Falls back to GOOGLE_APPLICATION_CREDENTIALS, then to ADC (gcloud auth "+
				"application-default login). Set project/region under vertex: in config.yaml.",
			"Vertex service account JSON path (leave empty to use ADC / "+
				"GOOGLE_APPLICATION_CREDENTIALS)").at("https://cloud.google.com/iam/docs/keys-create-delete").open()},
		{"XAI_API_KEY", providerVar("xAI API key", "xAI API key").at("https://console.x.ai/")},
		{"XAI_BASE_URL", baseURLVar("xAI", "")},
		{"NVIDIA_API_KEY", providerVar("NVIDIA NIM API key (build.nvidia.com or local NIM endpoint)",
			"NVIDIA NIM API key").at("https://build.nvidia.com/")},
		{"NVIDIA_BASE_URL", providerVar(
			"NVIDIA NIM base URL override (e.g. http://localhost:8000/v1 for local NIM)",
			"NVIDIA NIM base URL (leave empty for default)").open()},
		{"LM_API_KEY", providerVar("LM Studio bearer token for auth-enabled local servers",
			"LM Studio API key / bearer token")},
		{"LM_BASE_URL", baseURLVar("LM Studio", "")},
		{"GLM_API_KEY", providerVar("Z.AI / GLM API key (also recognized as ZAI_API_KEY / Z_AI_API_KEY)",
			"Z.AI / GLM API key").at("https://z.ai/")},
		{"ZAI_API_KEY", providerVar("Z.AI API key (alias for GLM_API_KEY)", "Z.AI API key").at("https://z.ai/")},
		{"Z_AI_API_KEY", providerVar("Z.AI API key (alias for GLM_API_KEY)", "Z.AI API key").at("https://z.ai/")},
		{"GLM_BASE_URL", baseURLVar("Z.AI / GLM", "")},
		{"KIMI_API_KEY", providerVar("Kimi / Moonshot API key", "Kimi API key").at("https://platform.moonshot.cn/")},
		{"KIMI_BASE_URL", baseURLVar("Kimi / Moonshot", "Kimi")},
		{"KIMI_CN_API_KEY", providerVar("Kimi / Moonshot China API key", "Kimi (China) API key").
			at("https://platform.moonshot.cn/")},
		{"STEPFUN_API_KEY", providerVar("StepFun Step Plan API key", "StepFun Step Plan API key").
			at("https://platform.stepfun.com/")},
		{"STEPFUN_BASE_URL", baseURLVar("StepFun Step Plan", "")},
		{"ARCEEAI_API_KEY", providerVar("Arcee AI API key", "Arcee AI API key").at("https://chat.arcee.ai/")},
		{"ARCEE_BASE_URL", baseURLVar("Arcee AI", "Arcee")},
		{"GMI_API_KEY", providerVar("GMI Cloud API key", "GMI Cloud API key").at("https://www.gmicloud.ai/")},
		{"GMI_BASE_URL", baseURLVar("GMI Cloud", "")},
		{"ACTUAL_API_KEY", providerVar("Actual Computer inference key (ac_...)",
			"Actual Computer inference key").at("https://actual.inc/user/keys")},
		{"FIREWORKS_API_KEY", providerVar("Fireworks AI API key", "Fireworks AI API key").
			at("https://app.fireworks.ai/settings/users/api-keys")},
		{"MINIMAX_API_KEY", providerVar("MiniMax API key (international)", "MiniMax API key").
			at("https://www.minimax.io/")},
		{"MINIMAX_BASE_URL", baseURLVar("MiniMax", "")},
		{"MINIMAX_CN_API_KEY", providerVar("MiniMax API key (China endpoint)", "MiniMax (China) API key").
			at("https://www.minimaxi.com/")},
		{"MINIMAX_CN_BASE_URL", baseURLVar("MiniMax (China)", "")},
		{"DEEPSEEK_API_KEY", providerVar("DeepSeek API key for direct DeepSeek access", "DeepSeek API Key").
			at("https://platform.deepseek.com/api_keys").shown()},
		{"DEEPSEEK_BASE_URL", providerVar("Custom DeepSeek API base URL (advanced)", "DeepSeek Base URL").at("").
			open().shown()},
		{"DASHSCOPE_API_KEY", providerVar("Alibaba Cloud DashScope API key (Qwen + multi-provider models)",
			"DashScope API Key").at("https://modelstudio.console.alibabacloud.com/").shown()},
		{"DASHSCOPE_BASE_URL", providerVar(
			"Custom DashScope base URL (default: coding-intl OpenAI-compat endpoint)",
			"DashScope Base URL").at("").open()},
		{"ICEBATH_QWEN_BASE_URL", providerVar(
			"Qwen Portal base URL override (default: https://portal.qwen.ai/v1)",
			"Qwen Portal base URL (leave empty for default)").open()},
		{"OPENCODE_ZEN_API_KEY", providerVar("OpenCode Zen API key (pay-as-you-go access to curated models)",
			"OpenCode Zen API key").at("https://opencode.ai/auth")},
		{"COMMANDCODE_API_KEY", providerVar(
			"CommandCode API key (GOAT/Pro/Max/Provider plans — 30+ models via one key)",
			"CommandCode API key").at("https://commandcode.ai/studio/")},
		{"OPENCODE_ZEN_BASE_URL", baseURLVar("OpenCode Zen", "")},
		{"OPENCODE_GO_API_KEY", providerVar("OpenCode Go API key ($10/month subscription for open models)",
			"OpenCode Go API key").at("https://opencode.ai/auth")},
		{"OPENCODE_GO_BASE_URL", baseURLVar("OpenCode Go", "")},
		{"HF_TOKEN", providerVar(
			"Hugging Face token for Inference Providers (20+ open models via router.huggingface.co)",
			"Hugging Face Token").at("https://huggingface.co/settings/tokens").shown()},
		{"HF_BASE_URL", baseURLVar("Hugging Face Inference Providers", "HF")},
		{"OLLAMA_API_KEY", providerVar("Ollama Cloud API key (ollama.com — cloud-hosted open models)",
			"Ollama Cloud API key").at("https://ollama.com/settings")},
		{"OLLAMA_BASE_URL", providerVar("Ollama Cloud base URL override (default: https://ollama.com/v1)",
			"Ollama base URL (leave empty for default)").open()},
		{"XIAOMI_API_KEY", providerVar(
			"Xiaomi MiMo API key for MiMo models (mimo-v2.5-pro, mimo-v2.5, mimo-v2-pro, "+
				"mimo-v2-omni, mimo-v2-flash)", "Xiaomi MiMo API Key").at("https://platform.xiaomimimo.com").shown()},
		{"XIAOMI_BASE_URL", providerVar(
			"Xiaomi MiMo base URL override (default: https://api.xiaomimimo.com/v1)",
			"Xiaomi base URL (leave empty for default)").open()},
		{"UPSTAGE_API_KEY", providerVar("Upstage API key for Solar LLM models", "Upstage API Key").
			at("https://console.upstage.ai/api-keys").shown()},
		{"UPSTAGE_BASE_URL", providerVar("Upstage base URL override (default: https://api.upstage.ai/v1)",
			"Upstage base URL (leave empty for default)").open()},
		{"AWS_REGION", providerVar("AWS region for Bedrock API calls (e.g. us-east-1, eu-central-1)",
			"AWS Region").at("https://docs.aws.amazon.com/bedrock/latest/userguide/bedrock-regions.html").open()},
		{"AWS_PROFILE", providerVar("AWS named profile for Bedrock authentication (from ~/.aws/credentials)",
			"AWS Profile").open()},
		{"AZURE_FOUNDRY_API_KEY", providerVar("Azure Foundry API key for custom Azure endpoints",
			"Azure Foundry API Key").at("https://ai.azure.com/").shown()},
		{"AZURE_FOUNDRY_BASE_URL", providerVar(
			"Azure Foundry base URL (set via 'icebath model' for endpoint-specific config)",
			"Azure Foundry base URL").open()},
		{"EXA_API_KEY", toolVar("Exa API key for AI-native web search and contents", "Exa API key").
			at("https://exa.ai/").unlocks("web_search", "web_extract")},
		{"PARALLEL_API_KEY", toolVar("Parallel API key for AI-native web search and extract",
			"Parallel API key").at("https://parallel.ai/").unlocks("web_search", "web_extract")},
		{"FIRECRAWL_API_KEY", toolVar("Firecrawl API key for web search and scraping", "Firecrawl API key").
			at("https://firecrawl.dev/").unlocks("web_search", "web_extract")},
		{"FIRECRAWL_API_URL", toolVar("Firecrawl API URL for self-hosted instances (optional)",
			"Firecrawl API URL (leave empty for cloud)").open().hidden()},
		{"FIRECRAWL_GATEWAY_URL", toolVar(
			"Exact Firecrawl tool-gateway origin override for Icebath Portal Subscribers only (optional)",
			"Firecrawl gateway URL (leave empty to derive from domain)").open().hidden()},
		{"TOOL_GATEWAY_URL", toolVar(
			"Exact shared tool-gateway origin for on-origin vendors and media uploads (optional)",
			"Shared tool-gateway URL (leave empty to derive from domain)").open().hidden()},
		{"CONNECTOR_GATEWAY_URL", toolVar(
			"Exact connector-gateway origin for the connectors API (optional)",
			"Connector-gateway URL (leave empty to derive from domain)").open().hidden()},
		{"TOOL_GATEWAY_DOMAIN", toolVar(
			"Shared tool-gateway domain suffix for Icebath Portal Subscribers only, used to derive vendor "+
				"hosts, e.g. icebath.app -> firecrawl-gateway.icebath.app",
			"Tool-gateway domain suffix").open().hidden()},
		{"TOOL_GATEWAY_SCHEME", toolVar(
			"Shared tool-gateway URL scheme for Icebath Portal Subscribers only, used to derive vendor hosts "+
				"(`https` by default, set `http` for local gateway testing)", "Tool-gateway URL scheme").open().hidden()},
		{"TOOL_GATEWAY_USER_TOKEN", toolVar(
			"Explicit Icebath Portal Subscriber access token for tool-gateway requests (optional; otherwise "+
				"read from the icebath auth store)", "Tool-gateway user token").hidden()},
		{"TAVILY_API_KEY", toolVar(
			"Tavily API key for AI-native web search and extract (optional — keyless works when "+
				"Tavily is selected)", "Tavily API key").at("https://app.tavily.com/home").unlocks("web_search", "web_extract")},
		{"PERPLEXITY_API_KEY", toolVar(
			"Perplexity API key for the Search API web backend (ranked results + query-relevant page "+
				"snippets)", "Perplexity API key").at("https://www.perplexity.ai/account/api").unlocks("web_search", "web_extract")},
		{"KEENABLE_API_KEY", toolVar(
			"Keenable API key for fast independent-index web search and page fetch (optional — "+
				"keyless free tier works without it)", "Keenable API key").at("https://keenable.ai").unlocks("web_search", "web_extract")},
		{"SEARXNG_URL", toolVar("URL of your SearXNG instance for free self-hosted web search",
			"SearXNG URL (e.g. http://localhost:8080)").at("https://searxng.github.io/searxng/").unlocks("web_search").open()},
		{"BRAVE_SEARCH_API_KEY", toolVar(
			"Brave Search API subscription token (free tier: 2,000 queries/mo)",
			"Brave Search subscription token").at("https://brave.com/search/api/").unlocks("web_search")},
		{"BROWSERBASE_API_KEY", toolVar(
			"Browserbase API key for cloud browser (optional — local browser works without this)",
			"Browserbase API key").at("https://browserbase.com/").unlocks("browser_exec")},
		{"BROWSERBASE_PROJECT_ID", toolVar(
			"Browserbase project ID (optional — only needed for cloud browser)",
			"Browserbase project ID").at("https://browserbase.com/").unlocks("browser_exec").open()},
		{"BROWSER_USE_API_KEY", toolVar(
			"Browser Use API key for cloud browser (optional — local browser works without this)",
			"Browser Use API key").at("https://browser-use.com/").unlocks("browser_exec")},
		{"FIRECRAWL_BROWSER_TTL", toolVar(
			"Firecrawl browser session TTL in seconds (optional, default 300)",
			"Browser session TTL (seconds)").unlocks("browser_exec").open()},
		{"FAL_KEY", toolVar("FAL API key for image and video generation", "FAL API key").at("https://fal.ai/").
			unlocks("image_generate", "video_generate")},
		{"KREA_API_KEY", toolVar("Krea API key for Krea 3 and Krea 2 image generation",
			"Krea API key").at("https://www.krea.ai/settings/api-tokens").unlocks("image_generate")},
		{"VOICE_TOOLS_OPENAI_KEY", toolVar(
			"OpenAI API key for voice transcription (Whisper) and OpenAI TTS",
			"OpenAI API Key (for Whisper STT + TTS)").at("https://platform.openai.com/api-keys").
			unlocks("voice_transcription", "openai_tts")},
		{"ELEVENLABS_API_KEY", toolVar(
			"ElevenLabs API key for premium text-to-speech voices and Scribe transcription",
			"ElevenLabs API key").at("https://elevenlabs.io/").unlocks("elevenlabs_tts", "voice_transcription")},
		{"MISTRAL_API_KEY", toolVar("Mistral API key for Voxtral TTS and transcription (STT)",
			"Mistral API key").at("https://console.mistral.ai/")},
		{"PORCUPINE_ACCESS_KEY", toolVar(
			"Picovoice access key for the Porcupine 'Hey icebath' wake word engine (optional; "+
				"openWakeWord is the free default)", "Picovoice access key").at("https://console.picovoice.ai/")},
		{"GITHUB_TOKEN", toolVar("GitHub token for Skills Hub (higher API rate limits, skill publish)",
			"GitHub Token").at("https://github.com/settings/tokens")},
		{"NOTION_API_KEY", skillVar("Notion integration token (used by the `notion` skill)",
			"Notion API key").at("https://www.notion.so/my-integrations")},
		{"LINEAR_API_KEY", skillVar("Linear personal API key (used by the `linear` skill)",
			"Linear API key").at("https://linear.app/settings/account/security")},
		{"AIRTABLE_API_KEY", skillVar("Airtable personal access token (used by the `airtable` skill)",
			"Airtable API key").at("https://airtable.com/create/tokens")},
		{"TENOR_API_KEY", skillVar("Tenor API key for GIF search (used by the `gif-search` skill)",
			"Tenor API key").at("https://developers.google.com/tenor/guides/quickstart")},
		{"HONCHO_API_KEY", toolVar("Honcho API key for AI-native persistent memory", "Honcho API key").
			at("https://app.honcho.dev").unlocks("honcho_context")},
		{"HONCHO_BASE_URL", toolVar("Base URL for self-hosted Honcho instances (no API key needed)",
			"Honcho base URL (e.g. http://localhost:8000)").unmarked()},
		{"HINDSIGHT_API_KEY", toolVar("Hindsight API key for graph-aware persistent memory",
			"Hindsight API key").at("https://hindsight.vectorize.io").unlocks("hindsight_recall")},
		{"HINDSIGHT_API_URL", toolVar(
			"Base URL for the Hindsight API (default: https://api.hindsight.vectorize.io)",
			"Hindsight API URL").unmarked().hidden()},
		{"SUPERMEMORY_API_KEY", toolVar("Supermemory API key for conversation-scoped persistent memory",
			"Supermemory API key").at("https://supermemory.ai").unlocks("supermemory_search")},
		{"MEM0_API_KEY", toolVar("Mem0 Platform API key for semantic persistent memory", "Mem0 API key").
			at("https://app.mem0.ai").unlocks("mem0_search")},
		{"RETAINDB_API_KEY", toolVar("RetainDB API key for persistent memory", "RetainDB API key").
			at("https://retaindb.com").unlocks("retaindb_search")},
		{"RETAINDB_BASE_URL", toolVar(
			"Base URL for self-hosted RetainDB instances (default: https://api.retaindb.com)",
			"RetainDB base URL").unmarked().hidden()},
		{"BRV_API_KEY", toolVar("ByteRover API key (optional, for cloud sync — local-first by default)",
			"ByteRover API key").at("https://app.byterover.dev").unlocks("brv_query")},
		{"OPENVIKING_API_KEY", toolVar("OpenViking API key (leave blank for local dev mode)",
			"OpenViking API key").unlocks("viking_search")},
		{"OPENVIKING_ENDPOINT", toolVar("OpenViking server URL (default: http://127.0.0.1:1933)",
			"OpenViking endpoint").unmarked().hidden()},
		{"TELEGRAM_BOT_TOKEN", messagingVar(
			"Complete Telegram bot token created by @BotFather (numeric bot ID followed by a colon "+
				"and secret)", "Telegram bot token").at("https://t.me/BotFather").masked()},
		{"TELEGRAM_ALLOWED_USERS", messagingVar(
			"Optional comma-separated numeric Telegram user IDs allowed immediately; leave blank to "+
				"approve new users through DM pairing", "Allowed Telegram user IDs (comma-separated)").
			at("https://t.me/userinfobot")},
		{"TELEGRAM_PROXY", messagingVar(
			"Proxy URL for Telegram connections (overrides HTTPS_PROXY). Supports http://, "+
				"https://, socks5://", "Telegram proxy URL (optional)")},
		{"DISCORD_BOT_TOKEN", messagingVar("Discord bot token from Developer Portal", "Discord bot token").
			at("https://discord.com/developers/applications").masked()},
		{"DISCORD_ALLOWED_USERS", messagingVar("Comma-separated Discord user IDs allowed to use the bot",
			"Allowed Discord user IDs (comma-separated)")},
		{"DISCORD_REPLY_TO_MODE", messagingVar(
			"Discord reply threading mode: 'off' (no reply references), 'first' (reply on first "+
				"message only, default), 'all' (reply on every chunk)",
			"Discord reply mode (off/first/all)")},
		{"SLACK_BOT_TOKEN", messagingVar(
			"Slack bot token (xoxb-). Get from OAuth & Permissions after installing your app. "+
				"Required scopes: chat:write, app_mentions:read, channels:history, groups:history, "+
				"im:history, im:read, im:write, mpim:history, mpim:read, users:read, files:read, "+
				"files:write", "Slack Bot Token (xoxb-...)").at("https://api.slack.com/apps").
			explained("In your Slack app, add the required bot scopes, install the app to the workspace, " +
				"then copy OAuth & Permissions > Bot User OAuth Token.").masked()},
		{"SLACK_APP_TOKEN", messagingVar(
			"Slack app-level token (xapp-) for Socket Mode. Get from Basic Information → App-Level "+
				"Tokens. Also ensure Event Subscriptions include: message.im, message.channels, "+
				"message.groups, message.mpim, app_mention", "Slack App Token (xapp-...)").
			at("https://api.slack.com/apps").
			explained("In your Slack app, enable Socket Mode, then create Basic Information > App-Level " +
				"Tokens with the connections:write scope.").masked()},
		{"SLACK_ALLOWED_USERS", messagingVar(
			"Comma-separated Slack member IDs allowed to use icebath, e.g. U01ABC2DEF3. Without "+
				"this, Slack may connect but deny messages by default.", "Allowed Slack member IDs").
			at("https://api.slack.com/apps").
			explained("In Slack, open your profile, choose More or the three-dot menu, then Copy member " +
				"ID. Add multiple IDs comma-separated.")},
		{"MATTERMOST_URL", messagingVar("Mattermost server URL (e.g. https://mm.example.com)",
			"Mattermost server URL").at("https://mattermost.com/deploy/")},
		{"MATTERMOST_TOKEN", messagingVar("Mattermost bot token or personal access token",
			"Mattermost bot token").masked()},
		{"MATTERMOST_ALLOWED_USERS", messagingVar("Comma-separated Mattermost user IDs allowed to use the bot",
			"Allowed Mattermost user IDs (comma-separated)")},
		{"MATTERMOST_REQUIRE_MENTION", messagingVar(
			"Require @mention in Mattermost channels (default: true). Set to false to respond to "+
				"all messages.", "Require @mention in channels")},
		{"MATTERMOST_FREE_RESPONSE_CHANNELS", messagingVar(
			"Comma-separated Mattermost channel IDs where bot responds without @mention",
			"Free-response channel IDs (comma-separated)")},
		{"MATRIX_HOMESERVER", messagingVar("Matrix homeserver URL (e.g. https://matrix.example.org)",
			"Matrix homeserver URL").at("https://matrix.org/ecosystem/servers/")},
		{"MATRIX_ACCESS_TOKEN", messagingVar("Matrix access token (preferred over password login)",
			"Matrix access token").masked()},
		{"MATRIX_USER_ID", messagingVar("Matrix user ID (e.g. @icebath:example.org)",
			"Matrix user ID (@user:server)")},
		{"MATRIX_ALLOWED_USERS", messagingVar(
			"Comma-separated Matrix user IDs allowed to use the bot (@user:server format)",
			"Allowed Matrix user IDs (comma-separated)")},
		{"MATRIX_REQUIRE_MENTION", messagingVar(
			"Require @mention in Matrix rooms (default: true). Set to false to respond to all "+
				"messages.", "Require @mention in rooms (true/false)").hidden()},
		{"MATRIX_FREE_RESPONSE_ROOMS", messagingVar(
			"Comma-separated Matrix room IDs where bot responds without @mention",
			"Free-response room IDs (comma-separated)").hidden()},
		{"MATRIX_AUTO_THREAD", messagingVar("Auto-create threads for messages in Matrix rooms (default: true)",
			"Auto-create threads in rooms (true/false)").hidden()},
		{"MATRIX_DM_AUTO_THREAD", messagingVar("Auto-create threads for DM messages in Matrix (default: false)",
			"Auto-create threads in DMs (true/false)").hidden()},
		{"MATRIX_DEVICE_ID", messagingVar(
			"Stable Matrix device ID for E2EE persistence across restarts (e.g. ICEBATH_BOT)",
			"Matrix device ID (stable across restarts)").hidden()},
		{"MATRIX_RECOVERY_KEY", messagingVar(
			"Matrix recovery key for cross-signing verification after device key rotation (from "+
				"Element: Settings → Security → Recovery Key)", "Matrix recovery key").masked().hidden()},
		{"BLUEBUBBLES_SERVER_URL", messagingVar(
			"BlueBubbles server URL for iMessage integration (e.g. http://192.168.1.10:1234)",
			"BlueBubbles server URL").at("https://bluebubbles.app/")},
		{"BLUEBUBBLES_PASSWORD", messagingVar(
			"BlueBubbles server password (from BlueBubbles Server → Settings → API)",
			"BlueBubbles server password").masked()},
		{"BLUEBUBBLES_ALLOWED_USERS", messagingVar(
			"Comma-separated iMessage addresses (email or phone) allowed to use the bot",
			"Allowed iMessage addresses (comma-separated)")},
		{"BLUEBUBBLES_ALLOW_ALL_USERS", messagingVar("Allow all BlueBubbles users without allowlist",
			"Allow All BlueBubbles Users").unmarked()},
		{"QQ_APP_ID", messagingVar("QQ Bot App ID from QQ Open Platform (q.qq.com)", "QQ App ID").
			at("https://q.qq.com").unmarked()},
		{"QQ_CLIENT_SECRET", messagingVar("QQ Bot Client Secret from QQ Open Platform", "QQ Client Secret").masked()},
		{"QQ_ALLOWED_USERS", messagingVar("Comma-separated QQ user IDs allowed to use the bot",
			"QQ Allowed Users").unmarked()},
		{"QQ_GROUP_ALLOWED_USERS", messagingVar("Comma-separated QQ group IDs allowed to interact with the bot",
			"QQ Group Allowed Users").unmarked()},
		{"QQ_ALLOW_ALL_USERS", messagingVar("Allow all QQ users without an allowlist (true/false)",
			"Allow All QQ Users").unmarked()},
		{"QQBOT_HOME_CHANNEL", messagingVar("Default QQ channel/group for cron delivery and notifications",
			"QQ Home Channel").unmarked()},
		{"QQBOT_HOME_CHANNEL_NAME", messagingVar("Display name for the QQ home channel", "QQ Home Channel Name").
			unmarked()},
		{"QQ_SANDBOX", messagingVar("Enable QQ sandbox mode for development testing (true/false)",
			"QQ Sandbox Mode").unmarked()},
		{"IRC_SERVER", messagingVar("IRC server hostname (e.g. irc.libera.chat)", "IRC server")},
		{"IRC_CHANNEL", messagingVar("IRC channel to join (e.g. #icebath)", "IRC channel")},
		{"IRC_NICKNAME", messagingVar("Bot nickname on IRC (default: icebath-bot)", "IRC nickname")},
		{"IRC_SERVER_PASSWORD", messagingVar("IRC server password (if required)", "IRC server password").
			masked().hidden()},
		{"IRC_NICKSERV_PASSWORD", messagingVar("NickServ password for nick identification", "NickServ password").
			masked().hidden()},
		{"GATEWAY_ALLOW_ALL_USERS", messagingVar(
			"Allow all users to interact with messaging bots (true/false). Default: false.",
			"Allow all users (true/false)").hidden()},
		{"API_SERVER_ENABLED", messagingVar(
			"Enable the OpenAI-compatible API server (true/false). Allows frontends like Open "+
				"WebUI, LobeChat, etc. to connect.", "Enable API server (true/false)").hidden()},
		{"API_SERVER_KEY", messagingVar(
			"Bearer token for API server authentication. Required whenever the API server is "+
				"enabled; server refuses to start without it.", "API server auth key").masked().hidden()},
		{"API_SERVER_PORT", messagingVar("Port for the API server (default: 8642).", "API server port").hidden()},
		{"API_SERVER_HOST", messagingVar(
			"Host/bind address for the API server (default: 127.0.0.1). API_SERVER_KEY is still "+
				"required even on loopback binds.", "API server host").hidden()},
		{"API_SERVER_MODEL_NAME", messagingVar(
			"Model name advertised on /v1/models. Defaults to the profile name (or 'icebath' "+
				"for the default profile). Useful for multi-user setups with OpenWebUI.",
			"API server model name").hidden()},
		{"GATEWAY_PROXY_URL", messagingVar(
			"URL of a remote icebath API server to forward messages to (proxy mode). When set, the "+
				"gateway handles platform I/O only — all agent work is delegated to the remote server. "+
				"Use for Docker E2EE containers that relay to a host agent. Also configurable via "+
				"gateway.proxy_url in config.yaml.",
			"Remote icebath API server URL (e.g. http://192.168.1.100:8642)").hidden()},
		{"GATEWAY_PROXY_KEY", messagingVar(
			"Bearer token for authenticating with the remote icebath API server (proxy mode). Must "+
				"match the API_SERVER_KEY on the remote host.", "Remote API server auth key").masked().hidden()},
		{"WEBHOOK_ENABLED", messagingVar(
			"Enable the webhook platform adapter for receiving events from GitHub, GitLab, etc.",
			"Enable webhooks (true/false)")},
		{"WEBHOOK_PORT", messagingVar("Port for the webhook HTTP server (default: 8644).", "Webhook port")},
		{"WEBHOOK_SECRET", messagingVar(
			"Global HMAC secret for webhook signature validation (overridable per route in "+
				"config.yaml).", "Webhook secret").masked()},
		{"SUDO_PASSWORD", settingVar(
			"Sudo password for terminal commands requiring root access; set to an explicit empty "+
				"string to try empty without prompting", "Sudo password").masked()},
		{"ICEBATH_PREFILL_MESSAGES_FILE", settingVar(
			"Path to JSON file with ephemeral prefill messages for few-shot priming",
			"Prefill messages file path")},
		{"ICEBATH_EPHEMERAL_SYSTEM_PROMPT", settingVar(
			"Ephemeral system prompt injected at API-call time (never persisted to sessions)",
			"Ephemeral system prompt")},
	}
}
