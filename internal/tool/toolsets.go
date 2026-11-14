package tool

import (
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type Toolset struct {
	Name        string
	Description string
	Tools       []string
	Includes    []string
	Platforms   []string
	Posture     bool
}

func (t Toolset) clone() Toolset {
	t.Tools = slices.Clone(t.Tools)
	t.Includes = slices.Clone(t.Includes)
	t.Platforms = slices.Clone(t.Platforms)
	return t
}

type ToolsetStatus struct {
	Name         string
	Available    bool
	Tools        []string
	Requirements []string
	LeadEnv      []string
	HasCheck     bool
}

type Selection struct {
	Enabled      []string
	Disabled     []string
	Platform     string
	Oneshot      bool
	KanbanWorker bool
	Drops        []string
	Scope        Scope
}

type DefinitionOptions struct {
	ContextLength  int
	SkipToolSearch bool
}

type Definitions struct {
	Tools  []wire.Tool
	Names  []string
	Search Assembly
}

type Assembly struct {
	Activated      bool
	Tier           int
	Deferred       int
	DeferredTokens int
	Listing        string
}

const bundlePrefix = "icebath-"

func CoreTools() []string {
	return []string{
		"web_search", "web_extract",
		"terminal", "process_manage",
		"read_file", "write_file", "patch", "search_files",
		"vision_analyze", "image_generate",
		"skills_list", "skill_view", "skill_manage",
		"browser_exec",
		"browser_cdp", "browser_dialog",
		"browser_vault",
		"text_to_speech",
		"todo_list", "memory",
		"session_search",
		"clarify",
		"execute_code", "delegate_task",
		"cronjob_manage",
		"kanban_show", "kanban_list",
		"kanban_complete", "kanban_block", "kanban_request_review",
		"kanban_request_changes",
		"kanban_heartbeat",
		"kanban_comment", "kanban_create", "kanban_link",
		"kanban_unblock",
		"kanban_attach", "kanban_attach_url", "kanban_attachments",
		"computer_use",
		"manage_connections",
	}
}

func coreWithout(kanban bool, excluded ...string) []string {
	var out []string
	for _, t := range CoreTools() {
		if slices.Contains(excluded, t) || (!kanban && strings.HasPrefix(t, "kanban_")) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func corePrefixed(prefix string) []string {
	var out []string
	for _, t := range CoreTools() {
		if strings.HasPrefix(t, prefix) {
			out = append(out, t)
		}
	}
	return out
}

func StaticToolsets() []Toolset {
	feishu := []string{"feishu_doc_read", "feishu_drive_list_comments", "feishu_drive_list_comment_replies",
		"feishu_drive_reply_comment", "feishu_drive_add_comment"}
	yuanbao := []string{"yb_query_group_info", "yb_query_group_members", "yb_send_dm", "yb_search_sticker", "yb_send_sticker"}
	coding := coreWithout(false, "image_generate", "text_to_speech", "cronjob_manage", "computer_use")
	ts := func(name, description string, tools []string, includes ...string) Toolset {
		return Toolset{Name: name, Description: description, Tools: slices.Clone(tools), Includes: includes}
	}
	bundle := func(name, description string, extras ...string) Toolset {
		return ts(name, description, append(CoreTools(), extras...))
	}
	onboarding := ts("onboarding", "First-run onboarding cards; folded in only for the onboarding chat", []string{"setup_beat"})
	onboarding.Platforms = []string{"desktop"}
	catalog := ts("catalog", "Desktop catalog plugin/skill install requests through the approval card (GUI sessions only)", []string{"manage_catalog"})
	catalog.Platforms = []string{"desktop"}
	codingSet := ts("coding", "Coding-focused toolset: files, terminal, search, web docs, skills, todo, delegate, vision, browser", coding)
	codingSet.Posture = true
	acp := slices.DeleteFunc(slices.Clone(coding), func(t string) bool { return t == "clarify" })
	return []Toolset{
		ts("web", "Web research and content extraction tools", []string{"web_search", "web_extract"}),
		ts("search", "Web search only (no content extraction/scraping)", []string{"web_search"}),
		ts("x_search", "Search X (Twitter) posts and threads via xAI's built-in x_search Responses tool. Read-only public X discovery; use the xurl skill for authenticated X API reads and account actions. Available when xAI credentials are configured (SuperGrok OAuth or XAI_API_KEY). Off by default; enable in `icebath tools` → X (Twitter) Search.", []string{"x_search"}),
		ts("vision", "Image analysis and vision tools", []string{"vision_analyze"}),
		ts("video", "Video analysis and understanding tools (opt-in, not in default toolset)", []string{"video_analyze"}),
		ts("image_gen", "Creative generation tools (images)", []string{"image_generate"}),
		ts("video_gen", "Video generation tools. Single ``video_generate`` tool covers text-to-video (prompt only) and image-to-video (prompt + image_url), plus reference-to-video. Provider-specific edit/extend workflows may appear as separate tools. Configure via ``icebath tools`` → Video Generation.", []string{"video_generate", "xai_video_edit", "xai_video_extend"}),
		ts("computer_use", "Background desktop control via cua-driver (macOS/Windows/Linux) — screenshots, mouse, keyboard, scroll, drag. Does NOT steal the user's cursor or keyboard focus. Works with any tool-capable model.", []string{"computer_use"}),
		ts("terminal", "Terminal/command execution and process management tools", []string{"terminal", "process_manage"}),
		ts("skills", "Access, create, edit, and manage skill documents with specialized instructions and knowledge", []string{"skills_list", "skill_view", "skill_manage"}),
		ts("browser", "Browser automation: drive a real browser with scripts over CDP (navigate, click, type, extract, screenshots), raw CDP, dialogs and vault logins", corePrefixed("browser_")),
		ts("cronjob", "Cronjob management tool - create, list, update, pause, resume, remove, and trigger scheduled tasks", []string{"cronjob_manage"}),
		ts("file", "File manipulation tools: read, write, patch (with fuzzy matching), and search (content + files)", []string{"read_file", "write_file", "patch", "search_files"}),
		ts("tts", "Text-to-speech: convert text to audio with Edge TTS (free), ElevenLabs, OpenAI, or xAI", []string{"text_to_speech"}),
		ts("todo", "Task planning and tracking for multi-step work", []string{"todo_list"}),
		ts("memory", "Persistent memory across sessions (personal notes + user profile)", []string{"memory"}),
		ts("context_engine", "Runtime tools exposed by the active context engine", nil),
		ts("session_search", "Search and recall past conversations with summarization", []string{"session_search"}),
		ts("connections", "Remote connector discovery, execution, and account authorization", []string{"manage_connections"}),
		ts("project", "Desktop Projects — create/switch named workspaces (GUI sessions only)", []string{"desktop_project"}),
		ts("bot_room", "Verified text-only Group Chat turn capabilities", nil),
		ts("desktop_ui", "Desktop GUI affordances — in-app terminal/browser panes, pane focus, reactions, appearance (GUI sessions only)",
			[]string{"read_terminal", "close_terminal", "desktop_preview", "drive_preview", "annotate_preview", "read_window_below",
				"focus_pane", "react_to_message", "gui_tour", "show_tip", "set_theme"}),
		onboarding,
		catalog,
		ts("clarify", "Ask the user clarifying questions (multiple-choice or open-ended)", []string{"clarify"}),
		ts("code_execution", "Run scripts that call tools programmatically (reduces LLM round trips)", []string{"execute_code"}),
		ts("delegation", "Spawn subagents with isolated context for complex subtasks", []string{"delegate_task"}),
		ts("kanban", "Kanban multi-agent coordination — only active when the agent is spawned by the kanban dispatcher (ICEBATH_KANBAN_TASK env set). The dispatcher runs inside the gateway by default; see `kanban.dispatch_in_gateway` in config.yaml. Lets workers mark tasks done with structured handoffs, enter first-class review (request_review — not a block), return review changes, block for human input, heartbeat during long ops, comment on threads, attach files, and (for orchestrators) list, unblock, and fan out tasks.", corePrefixed("kanban_")),
		ts("discord", "Discord read and participate tools (fetch messages, search members, create threads)", []string{"discord"}),
		ts("discord_admin", "Discord server management (list channels/roles, pin messages, assign roles)", []string{"discord_admin"}),
		ts("yuanbao", "Yuanbao platform tools - group info, member queries, DM, stickers", yuanbao),
		ts("feishu_doc", "Read Feishu/Lark document content", []string{"feishu_doc_read"}),
		ts("feishu_drive", "Feishu/Lark document comment operations (list, reply, add)", feishu[1:]),
		ts("debugging", "Debugging and troubleshooting toolkit", []string{"terminal", "process_manage"}, "web", "file"),
		ts("safe", "Safe toolkit without terminal access", nil, "web", "vision", "image_gen"),
		codingSet,
		ts("icebath-acp", "Editor integration (VS Code, Zed, JetBrains) — coding-focused tools without messaging, audio, or clarify UI", acp),
		ts("icebath-api-server", "OpenAI-compatible API server — full agent tools accessible via HTTP (no interactive UI tools like clarify or send_message)",
			coreWithout(false, "text_to_speech", "clarify", "computer_use")),
		bundle("icebath-cli", "Full interactive CLI toolset - all default tools plus cronjob management"),
		bundle("icebath-cron", "Default cron toolset - same core tools as icebath-cli; gated by `icebath tools`"),
		bundle("icebath-telegram", "Telegram bot toolset - full access for personal use (terminal has safety checks)"),
		bundle("icebath-discord", "Discord bot toolset - full access (terminal has safety checks via dangerous command approval)", "discord", "discord_admin"),
		bundle("icebath-whatsapp", "WhatsApp bot toolset - similar to Telegram (personal messaging, more trusted)"),
		bundle("icebath-slack", "Slack bot toolset - full access for workspace use (terminal has safety checks)"),
		bundle("icebath-signal", "Signal bot toolset - encrypted messaging platform (full access)"),
		bundle("icebath-bluebubbles", "BlueBubbles iMessage bot toolset - Apple iMessage via local BlueBubbles server"),
		bundle("icebath-email", "Email bot toolset - interact with icebath via email (IMAP/SMTP)"),
		bundle("icebath-mattermost", "Mattermost bot toolset - self-hosted team messaging (full access)"),
		bundle("icebath-matrix", "Matrix bot toolset - decentralized encrypted messaging (full access)"),
		bundle("icebath-dingtalk", "DingTalk bot toolset - enterprise messaging platform (full access)"),
		bundle("icebath-feishu", "Feishu/Lark bot toolset - enterprise messaging via Feishu/Lark (full access)", feishu...),
		bundle("icebath-weixin", "Weixin bot toolset - personal WeChat messaging via iLink (full access)"),
		bundle("icebath-qqbot", "QQBot toolset - QQ messaging via Official Bot API v2 (full access)"),
		bundle("icebath-wecom", "WeCom bot toolset - enterprise WeChat messaging (full access)"),
		bundle("icebath-wecom-callback", "WeCom callback toolset - enterprise self-built app messaging (full access)"),
		bundle("icebath-yuanbao", "Yuanbao Bot 元宝消息平台工具集 - 群信息、成员查询、私聊、贴纸表情", yuanbao...),
		bundle("icebath-sms", "SMS bot toolset - interact with icebath via SMS (Twilio)"),
		ts("icebath-webhook", "Webhook toolset - receive and process external webhook events", []string{"web_search", "web_extract", "vision_analyze", "clarify"}),
		ts("icebath-gateway", "Gateway toolset - union of all messaging platform tools", nil,
			"icebath-telegram", "icebath-discord", "icebath-whatsapp", "icebath-slack",
			"icebath-signal", "icebath-bluebubbles", "icebath-email",
			"icebath-sms", "icebath-mattermost", "icebath-matrix", "icebath-dingtalk",
			"icebath-feishu", "icebath-wecom", "icebath-wecom-callback", "icebath-weixin",
			"icebath-qqbot", "icebath-webhook", "icebath-yuanbao"),
	}
}

func BuiltinToolNames() map[string]bool {
	out := map[string]bool{}
	for _, t := range StaticToolsets() {
		for _, name := range t.Tools {
			out[name] = true
		}
	}
	return out
}

func ClientSurfaceToolsets() []string {
	return []string{"catalog", "desktop_ui", "project"}
}

func oneshotDisabledToolsets() []string {
	return []string{"clarify"}
}

func SessionDisabled(disabled []string, platform string, oneshot bool) []string {
	var gated []string
	for _, t := range StaticToolsets() {
		if len(t.Platforms) > 0 && !slices.Contains(t.Platforms, platform) {
			gated = append(gated, t.Name)
		}
	}
	if oneshot {
		gated = append(gated, oneshotDisabledToolsets()...)
	}
	gated = slices.DeleteFunc(gated, func(name string) bool { return slices.Contains(disabled, name) })
	if len(gated) == 0 {
		return disabled
	}
	return append(slices.Clone(disabled), gated...)
}

func SelectionKey(s Selection) string {
	var b strings.Builder
	if s.Enabled == nil {
		b.WriteString("*")
	} else {
		b.WriteString("[")
		b.WriteString(strings.Join(sortedUnique(s.Enabled), "\x00"))
		b.WriteString("]")
	}
	b.WriteString("|")
	b.WriteString(strings.Join(sortedUnique(SessionDisabled(s.Disabled, s.Platform, s.Oneshot)), "\x00"))
	return b.String()
}

func sortedUnique(items []string) []string {
	out := slices.Clone(items)
	slices.Sort(out)
	return slices.Compact(out)
}

func legacyToolsets() map[string][]string {
	return map[string][]string{
		"web_tools":      {"web_search", "web_extract"},
		"terminal_tools": {"terminal"},
		"vision_tools":   {"vision_analyze"},
		"image_tools":    {"image_generate"},
		"skills_tools":   {"skills_list", "skill_view", "skill_manage"},
		"cronjob_tools":  {"cronjob_manage"},
		"file_tools":     {"read_file", "write_file", "patch", "search_files"},
		"tts_tools":      {"text_to_speech"},
	}
}

type Configurable struct {
	Key         string
	Label       string
	Description string
}

func ConfigurableToolsets() []Configurable {
	return []Configurable{
		{"web", "🔍 Web Search & Scraping", "web_search, web_extract"},
		{"browser", "🌐 Browser Automation", "navigate, click, type, scroll"},
		{"terminal", "💻 Terminal & Processes", "terminal, process"},
		{"file", "📁 File Operations", "read, write, patch, search"},
		{"code_execution", "⚡ Code Execution", "execute_code"},
		{"vision", "👁️  Vision / Image Analysis", "vision_analyze"},
		{"video", "🎬 Video Analysis", "video_analyze (requires video-capable model)"},
		{"image_gen", "🎨 Image Generation", "image_generate"},
		{"video_gen", "🎬 Video Generation", "video_generate (text/image/reference)"},
		{"x_search", "🐦 X (Twitter) Search", "x_search (requires xAI OAuth or XAI_API_KEY)"},
		{"tts", "🔊 Text-to-Speech", "text_to_speech"},
		{"stt", "🎙️ Speech-to-Text", "voice transcription (gateway voice messages + voice mode)"},
		{"skills", "📚 Skills", "list, view, manage"},
		{"todo", "📋 Task Planning", "todo_list"},
		{"kanban", "📌 Kanban", "opt-in task board tools for this platform"},
		{"memory", "💾 Memory", "persistent memory across sessions"},
		{"context_engine", "🧩 Context Engine", "runtime tools from the active context engine"},
		{"session_search", "🔎 Session Search", "search past conversations"},
		{"connections", "🔌 Connections", "remote connector tools and account authorization"},
		{"clarify", "❓ Clarifying Questions", "clarify"},
		{"delegation", "👥 Task Delegation", "delegate_task"},
		{"cronjob", "⏰ Cron Jobs", "create/list/update/pause/resume/run, with optional attached skills"},
		{"discord", "💬 Discord (read/participate)", "fetch messages, search members, create thread"},
		{"discord_admin", "🛡️  Discord Server Admin", "list channels/roles, pin, assign roles"},
		{"yuanbao", "🤖 Yuanbao", "group info, member queries, DM"},
		{"computer_use", "🖱️  Computer Use (macOS/Windows/Linux)", "background desktop control via cua-driver"},
	}
}

func configurableKeys() map[string]bool {
	out := map[string]bool{}
	for _, c := range ConfigurableToolsets() {
		out[c.Key] = true
	}
	return out
}

func DefaultOffToolsets() []string {
	return []string{"a2a", "discord", "discord_admin", "kanban", "spotify", "video", "video_gen", "x_search"}
}

func ConfigOnlyToolsets() []string {
	return []string{"stt"}
}

func toolsetRestrictions() map[string][]string {
	return map[string][]string{"discord": {"discord"}, "discord_admin": {"discord"}}
}

func ToolsetAllowed(toolset, platform string) bool {
	allowed, ok := toolsetRestrictions()[toolset]
	return !ok || slices.Contains(allowed, platform)
}

func ConfigurationPlatform(toolset, def string) string {
	allowed, ok := toolsetRestrictions()[toolset]
	if !ok || slices.Contains(allowed, def) {
		return def
	}
	return slices.Min(allowed)
}

func GUIToolsetLabel(label string) string {
	text := strings.TrimFunc(label, unicode.IsSpace)
	i := strings.IndexFunc(text, unicode.IsSpace)
	if i < 0 {
		return text
	}
	head, rest := text[:i], strings.TrimLeftFunc(text[i:], unicode.IsSpace)
	if rest == "" {
		return text
	}
	for _, r := range head {
		if r < unicode.MaxASCII+1 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return text
		}
	}
	return strings.TrimFunc(rest, unicode.IsSpace)
}

func knownPlatforms() []string {
	return []string{"cli", "telegram", "discord", "slack", "whatsapp", "whatsapp_cloud", "signal", "bluebubbles",
		"email", "mattermost", "matrix", "dingtalk", "feishu", "wecom", "wecom_callback", "weixin", "qqbot",
		"yuanbao", "webhook", "api_server", "cron"}
}

func PlatformDefaultToolset(platform string) string {
	switch platform {
	case "whatsapp_cloud":
		return "icebath-whatsapp"
	case "wecom_callback":
		return "icebath-wecom-callback"
	case "api_server":
		return "icebath-api-server"
	}
	return bundlePrefix + platform
}

func platformDefaultKeys() map[string]bool {
	out := map[string]bool{}
	for _, p := range knownPlatforms() {
		out[PlatformDefaultToolset(p)] = true
	}
	return out
}

func leftCorePlatforms() []string {
	return []string{"homeassistant"}
}
