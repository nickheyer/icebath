package tools

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// CoreTools is the shared tool list every platform bundle inherits
var CoreTools = []string{
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

// WebhookSafeTools are the tools untrusted webhook payloads may reach
var WebhookSafeTools = []string{"web_search", "web_extract", "vision_analyze", "clarify"}

var feishuTools = []string{
	"feishu_doc_read", "feishu_drive_list_comments", "feishu_drive_list_comment_replies",
	"feishu_drive_reply_comment", "feishu_drive_add_comment",
}

var yuanbaoTools = []string{"yb_query_group_info", "yb_query_group_members", "yb_send_dm", "yb_search_sticker", "yb_send_sticker"}

// ClientSurfaceToolsets are folded in by a GUI client for its own sessions, never by config
var ClientSurfaceToolsets = map[string]bool{"project": true, "desktop_ui": true, "catalog": true}

// OneshotDisabledToolsets are the toolsets nobody is present to use in a one shot session
var OneshotDisabledToolsets = []string{"clarify"}

// Toolset is one named tool group with its includes and session gating
type Toolset struct {
	Description string
	Tools       []string
	Includes    []string
	Posture     bool
	Platforms   map[string]bool
	Module      string
}

func coreWithout(kanban bool, excluded ...string) []string {
	skip := map[string]bool{}
	for _, e := range excluded {
		skip[e] = true
	}
	var out []string
	for _, t := range CoreTools {
		if skip[t] || (!kanban && strings.HasPrefix(t, "kanban_")) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func corePrefixed(prefix string) []string {
	var out []string
	for _, t := range CoreTools {
		if strings.HasPrefix(t, prefix) {
			out = append(out, t)
		}
	}
	return out
}

func concat(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}

// CodingTools is the coding posture, core tools minus messaging, audio, cron, kanban, and computer use
var CodingTools = coreWithout(false, "image_generate", "text_to_speech", "cronjob_manage", "computer_use")

func ts(description string, tools []string, includes ...string) Toolset {
	return Toolset{Description: description, Tools: append([]string{}, tools...), Includes: includes}
}

func bundle(description string, extras ...string) Toolset {
	return ts(description, concat(CoreTools, extras))
}

func platformGated(t Toolset, platforms ...string) Toolset {
	t.Platforms = map[string]bool{}
	for _, p := range platforms {
		t.Platforms[p] = true
	}
	return t
}

var toolsetsMu sync.RWMutex

func withoutName(list []string, name string) []string {
	var out []string
	for _, t := range list {
		if t != name {
			out = append(out, t)
		}
	}
	return out
}

// Toolsets is the static toolset table, extended at runtime by CreateCustomToolset
var Toolsets = map[string]Toolset{
	"web":    ts("Web research and content extraction tools", []string{"web_search", "web_extract"}),
	"search": ts("Web search only (no content extraction/scraping)", []string{"web_search"}),
	"x_search": ts("Search X (Twitter) posts and threads via xAI's built-in x_search Responses tool. Read-only public X discovery; use the xurl skill for authenticated X API reads and account actions. Available when xAI credentials are configured (SuperGrok OAuth or XAI_API_KEY). Off by default; enable in `icebath tools` -> X (Twitter) Search.",
		[]string{"x_search"}),
	"vision":    ts("Image analysis and vision tools", []string{"vision_analyze"}),
	"video":     ts("Video analysis and understanding tools (opt-in, not in default toolset)", []string{"video_analyze"}),
	"image_gen": ts("Creative generation tools (images)", []string{"image_generate"}),
	"video_gen": ts("Video generation tools. Single ``video_generate`` tool covers text-to-video (prompt only) and image-to-video (prompt + image_url), plus reference-to-video. Provider-specific edit/extend workflows may appear as separate tools. Configure via ``icebath tools`` -> Video Generation.",
		[]string{"video_generate", "xai_video_edit", "xai_video_extend"}),
	"computer_use": ts("Background desktop control via cua-driver (macOS/Windows/Linux) - screenshots, mouse, keyboard, scroll, drag. Does NOT steal the user's cursor or keyboard focus. Works with any tool-capable model.",
		[]string{"computer_use"}),
	"terminal": ts("Terminal/command execution and process management tools", []string{"terminal", "process_manage"}),
	"skills":   ts("Access, create, edit, and manage skill documents with specialized instructions and knowledge", []string{"skills_list", "skill_view", "skill_manage"}),
	"browser": ts("Browser automation: drive a real browser with Python over CDP (navigate, click, type, extract, screenshots), raw CDP, dialogs and vault logins",
		corePrefixed("browser_")),
	"cronjob":        ts("Cronjob management tool - create, list, update, pause, resume, remove, and trigger scheduled tasks", []string{"cronjob_manage"}),
	"file":           ts("File manipulation tools: read, write, patch (with fuzzy matching), and search (content + files)", []string{"read_file", "write_file", "patch", "search_files"}),
	"tts":            ts("Text-to-speech: convert text to audio with Edge TTS (free), ElevenLabs, OpenAI, or xAI", []string{"text_to_speech"}),
	"todo":           ts("Task planning and tracking for multi-step work", []string{"todo_list"}),
	"memory":         ts("Persistent memory across sessions (personal notes + user profile)", []string{"memory"}),
	"context_engine": ts("Runtime tools exposed by the active context engine", nil),
	"session_search": ts("Search and recall past conversations with summarization", []string{"session_search"}),
	"connections":    ts("Remote connector discovery, execution, and account authorization", []string{"manage_connections"}),
	"project":        ts("Desktop Projects - create/switch named workspaces (GUI sessions only)", []string{"desktop_project"}),
	"bot_room":       ts("Verified text-only Group Chat turn capabilities", nil),
	"desktop_ui": ts("Desktop GUI affordances - in-app terminal/browser panes, pane focus, reactions, appearance (GUI sessions only)",
		[]string{"read_terminal", "close_terminal", "desktop_preview", "drive_preview", "annotate_preview", "read_window_below", "focus_pane", "react_to_message", "gui_tour", "show_tip", "set_theme"}),
	"onboarding":     platformGated(ts("First-run onboarding cards; folded in only for the onboarding chat", []string{"setup_beat"}), "desktop"),
	"catalog":        platformGated(ts("Desktop catalog plugin/skill install requests through the approval card (GUI sessions only)", []string{"manage_catalog"}), "desktop"),
	"clarify":        ts("Ask the user clarifying questions (multiple-choice or open-ended)", []string{"clarify"}),
	"code_execution": ts("Run Python scripts that call tools programmatically (reduces LLM round trips)", []string{"execute_code"}),
	"delegation":     ts("Spawn subagents with isolated context for complex subtasks", []string{"delegate_task"}),
	"kanban": ts("Kanban multi-agent coordination - only active when the agent is spawned by the kanban dispatcher (ICEBATH_KANBAN_TASK env set). The dispatcher runs inside the gateway by default; see `kanban.dispatch_in_gateway` in config.yaml. Lets workers mark tasks done with structured handoffs, enter first-class review (request_review - not a block), return review changes, block for human input, heartbeat during long ops, comment on threads, attach files, and (for orchestrators) list, unblock, and fan out tasks.",
		corePrefixed("kanban_")),
	"discord":       ts("Discord read and participate tools (fetch messages, search members, create threads)", []string{"discord"}),
	"discord_admin": ts("Discord server management (list channels/roles, pin messages, assign roles)", []string{"discord_admin"}),
	"yuanbao":       ts("Yuanbao platform tools - group info, member queries, DM, stickers", yuanbaoTools),
	"feishu_doc":    ts("Read Feishu/Lark document content", []string{"feishu_doc_read"}),
	"feishu_drive":  ts("Feishu/Lark document comment operations (list, reply, add)", feishuTools[1:]),
	"debugging":     ts("Debugging and troubleshooting toolkit", []string{"terminal", "process_manage"}, "web", "file"),
	"safe":          ts("Safe toolkit without terminal access", nil, "web", "vision", "image_gen"),
	"coding": {Description: "Coding-focused toolset: files, terminal, search, web docs, skills, todo, delegate, vision, browser",
		Tools: append([]string{}, CodingTools...), Posture: true},
	"icebath-acp":            ts("Editor integration (VS Code, Zed, JetBrains) - coding-focused tools without messaging, audio, or clarify UI", withoutName(CodingTools, "clarify")),
	"icebath-api-server":     ts("OpenAI-compatible API server - full agent tools accessible via HTTP (no interactive UI tools like clarify or send_message)", coreWithout(false, "text_to_speech", "clarify", "computer_use")),
	"icebath-cli":            bundle("Full interactive CLI toolset - all default tools plus cronjob management"),
	"icebath-cron":           bundle("Default cron toolset - same core tools as icebath-cli; gated by `icebath tools`"),
	"icebath-telegram":       bundle("Telegram bot toolset - full access for personal use (terminal has safety checks)"),
	"icebath-discord":        bundle("Discord bot toolset - full access (terminal has safety checks via dangerous command approval)", "discord", "discord_admin"),
	"icebath-whatsapp":       bundle("WhatsApp bot toolset - similar to Telegram (personal messaging, more trusted)"),
	"icebath-slack":          bundle("Slack bot toolset - full access for workspace use (terminal has safety checks)"),
	"icebath-signal":         bundle("Signal bot toolset - encrypted messaging platform (full access)"),
	"icebath-bluebubbles":    bundle("BlueBubbles iMessage bot toolset - Apple iMessage via local BlueBubbles server"),
	"icebath-email":          bundle("Email bot toolset - interact with Icebath via email (IMAP/SMTP)"),
	"icebath-mattermost":     bundle("Mattermost bot toolset - self-hosted team messaging (full access)"),
	"icebath-matrix":         bundle("Matrix bot toolset - decentralized encrypted messaging (full access)"),
	"icebath-dingtalk":       bundle("DingTalk bot toolset - enterprise messaging platform (full access)"),
	"icebath-feishu":         bundle("Feishu/Lark bot toolset - enterprise messaging via Feishu/Lark (full access)", feishuTools...),
	"icebath-weixin":         bundle("Weixin bot toolset - personal WeChat messaging via iLink (full access)"),
	"icebath-qqbot":          bundle("QQBot toolset - QQ messaging via Official Bot API v2 (full access)"),
	"icebath-wecom":          bundle("WeCom bot toolset - enterprise WeChat messaging (full access)"),
	"icebath-wecom-callback": bundle("WeCom callback toolset - enterprise self-built app messaging (full access)"),
	"icebath-yuanbao": {Description: "Yuanbao Bot messaging platform toolset - group info, member queries, DM, stickers",
		Tools: concat(CoreTools, yuanbaoTools), Module: "tools.yuanbao_tools"},
	"icebath-sms":     bundle("SMS bot toolset - interact with Icebath via SMS (Twilio)"),
	"icebath-webhook": ts("Webhook toolset - receive and process external webhook events", WebhookSafeTools),
	"icebath-gateway": ts("Gateway toolset - union of all messaging platform tools", nil,
		"icebath-telegram", "icebath-discord", "icebath-whatsapp", "icebath-slack",
		"icebath-signal", "icebath-bluebubbles", "icebath-email",
		"icebath-sms", "icebath-mattermost", "icebath-matrix", "icebath-dingtalk",
		"icebath-feishu", "icebath-wecom", "icebath-wecom-callback", "icebath-weixin",
		"icebath-qqbot", "icebath-webhook", "icebath-yuanbao"),
}

// BundlePrefix is the prefix every platform bundle name carries
const BundlePrefix = "icebath-"

// legacyBundlePrefix is the pre rename bundle prefix still accepted from old configs
const legacyBundlePrefix = "hermes-"

// CanonicalToolsetName maps a pre rename bundle spelling onto its icebath name
func CanonicalToolsetName(name string) string {
	if strings.HasPrefix(name, legacyBundlePrefix) {
		return BundlePrefix + strings.TrimPrefix(name, legacyBundlePrefix)
	}
	return name
}

// ToolsetSessionPlatforms maps every platform gated toolset to the session platforms that get it
var ToolsetSessionPlatforms = func() map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for name, spec := range Toolsets {
		if spec.Platforms != nil {
			out[name] = spec.Platforms
		}
	}
	return out
}()

// BuiltinToolNames is every tool the static table names before runtime additions
var BuiltinToolNames = func() map[string]bool {
	out := map[string]bool{}
	for _, spec := range Toolsets {
		for _, t := range spec.Tools {
			out[t] = true
		}
	}
	return out
}()

// BuiltinToolsetNames is every static toolset name before runtime additions
var BuiltinToolsetNames = func() map[string]bool {
	out := map[string]bool{}
	for name := range Toolsets {
		out[name] = true
	}
	return out
}()

// PlatformRegistered reports whether a plugin platform of this name is loaded through the gateway names hook
var PlatformRegistered = func(name string) bool {
	return RegisteredPlatformNames != nil && containsString(RegisteredPlatformNames(), name)
}

// Toolset table keys in upstream TOOLSETS definition order, runtime custom toolsets appended
var toolsetOrder = []string{
	"web", "search", "x_search", "vision", "video", "image_gen", "video_gen", "computer_use", "terminal", "skills",
	"browser", "cronjob", "file", "tts", "todo", "memory", "context_engine", "session_search", "connections",
	"project", "bot_room", "desktop_ui", "onboarding", "catalog", "clarify", "code_execution", "delegation", "kanban",
	"discord", "discord_admin", "yuanbao", "feishu_doc", "feishu_drive", "debugging", "safe", "coding",
	"icebath-acp", "icebath-api-server", "icebath-cli", "icebath-cron", "icebath-telegram", "icebath-discord",
	"icebath-whatsapp", "icebath-slack", "icebath-signal", "icebath-bluebubbles", "icebath-email",
	"icebath-mattermost", "icebath-matrix", "icebath-dingtalk", "icebath-feishu", "icebath-weixin", "icebath-qqbot",
	"icebath-wecom", "icebath-wecom-callback", "icebath-yuanbao", "icebath-sms", "icebath-webhook", "icebath-gateway",
}

// Static toolset names in table definition order like iterating the upstream TOOLSETS dict
func orderedToolsetNames() []string {
	toolsetsMu.RLock()
	defer toolsetsMu.RUnlock()
	out := make([]string, 0, len(Toolsets))
	for _, n := range toolsetOrder {
		if _, ok := Toolsets[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// OrderedStaticToolsetNames lists the static toolset table in definition order
func OrderedStaticToolsetNames() []string { return orderedToolsetNames() }

func staticToolset(name string) (Toolset, bool) {
	toolsetsMu.RLock()
	defer toolsetsMu.RUnlock()
	spec, ok := Toolsets[CanonicalToolsetName(name)]
	if !ok {
		return Toolset{}, false
	}
	spec.Tools = append([]string{}, spec.Tools...)
	spec.Includes = append([]string{}, spec.Includes...)
	return spec, true
}

// StaticToolset is the static table entry of a toolset with copied tool and include lists
func StaticToolset(name string) (Toolset, bool) { return staticToolset(name) }

func staticToolsetNames() []string {
	toolsetsMu.RLock()
	defer toolsetsMu.RUnlock()
	out := make([]string, 0, len(Toolsets))
	for n := range Toolsets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pluginToolsetNames lists registry toolsets absent from the static table
func (r *Registry) pluginToolsetNames(ctx context.Context, scope string) map[string]bool {
	out := map[string]bool{}
	for _, n := range r.RegisteredToolsetNames(ctx, scope) {
		if _, ok := staticToolset(n); !ok {
			out[n] = true
		}
	}
	return out
}

func displayAlias(name string, aliases map[string]string) string {
	keys := make([]string, 0, len(aliases))
	for a := range aliases {
		keys = append(keys, a)
	}
	sort.Strings(keys)
	for _, a := range keys {
		if aliases[a] == name {
			if _, static := staticToolset(a); !static {
				return a
			}
		}
	}
	return ""
}

func (r *Registry) pluginDisplayNames(ctx context.Context, scope string) []string {
	aliases := r.ToolsetAliases()
	var out []string
	for n := range r.pluginToolsetNames(ctx, scope) {
		if a := displayAlias(n, aliases); a != "" {
			out = append(out, a)
		} else {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// GetToolset returns a toolset definition merged with registry additions, or false when unknown
func (r *Registry) GetToolset(ctx context.Context, scope, name string, includeRegistry bool) (Toolset, bool) {
	name = CanonicalToolsetName(name)
	spec, ok := staticToolset(name)
	if !includeRegistry {
		return spec, ok
	}
	if ok {
		merged := map[string]bool{}
		for _, t := range spec.Tools {
			merged[t] = true
		}
		for _, t := range r.ToolNamesForToolset(ctx, scope, name) {
			merged[t] = true
		}
		if target := r.ToolsetAliasTarget(name); target != "" && target != name {
			for _, t := range r.ToolNamesForToolset(ctx, scope, target) {
				merged[t] = true
			}
		}
		spec.Tools = sortedSet(merged)
		return spec, true
	}
	var registryToolset, description string
	if r.pluginToolsetNames(ctx, scope)[name] {
		registryToolset = name
		if alias := displayAlias(name, r.ToolsetAliases()); alias != "" {
			description = fmt.Sprintf("MCP server '%s' tools", alias)
		} else {
			description = "Plugin toolset: " + name
		}
	} else {
		registryToolset = r.ToolsetAliasTarget(name)
		if registryToolset == "" {
			return Toolset{}, false
		}
		description = fmt.Sprintf("MCP server '%s' tools", name)
	}
	return Toolset{Description: description, Tools: r.ToolNamesForToolset(ctx, scope, registryToolset)}, true
}

// BundleNonCoreTools returns a bundle's tools minus the core tools every toolset shares
func (r *Registry) BundleNonCoreTools(ctx context.Context, scope, name string) map[string]bool {
	core := map[string]bool{}
	for _, t := range CoreTools {
		core[t] = true
	}
	out := map[string]bool{}
	spec, ok := r.GetToolset(ctx, scope, name, true)
	if !ok {
		for _, t := range r.ResolveToolset(ctx, scope, name) {
			if !core[t] {
				out[t] = true
			}
		}
		return out
	}
	for _, t := range spec.Tools {
		if !core[t] {
			out[t] = true
		}
	}
	for _, inc := range spec.Includes {
		if incSpec, ok := r.GetToolset(ctx, scope, inc, true); ok {
			for _, t := range incSpec.Tools {
				if !core[t] {
					out[t] = true
				}
			}
		}
	}
	return out
}

type resolveKey struct {
	name       string
	registry   bool
	generation int64
	scope      string
}

var (
	resolveMemoMu sync.Mutex
	resolveMemo   = map[resolveKey][]string{}
)

// pluginPlatformBundle synthesizes the bundle of a registered plugin platform
func (r *Registry) pluginPlatformBundle(ctx context.Context, scope, name string) []string {
	if !strings.HasPrefix(name, BundlePrefix) {
		return nil
	}
	platform := strings.TrimPrefix(name, BundlePrefix)
	if !PlatformRegistered(platform) {
		return nil
	}
	tools := map[string]bool{}
	for _, t := range CoreTools {
		tools[t] = true
	}
	for _, d := range r.Snapshot(ctx, scope) {
		if d.Toolset == platform {
			tools[d.Name] = true
		}
	}
	return sortedSet(tools)
}

// ResolveToolset expands a toolset and its includes into a sorted tool name list
func (r *Registry) ResolveToolset(ctx context.Context, scope, name string) []string {
	return r.resolveToolset(ctx, scope, name, true)
}

// ResolveStaticToolset expands a toolset using the static table only
func (r *Registry) ResolveStaticToolset(ctx context.Context, scope, name string) []string {
	return r.resolveToolset(ctx, scope, name, false)
}

func (r *Registry) resolveToolset(ctx context.Context, scope, name string, includeRegistry bool) []string {
	name = CanonicalToolsetName(name)
	key := resolveKey{name, includeRegistry, r.Generation(), scope + "\x00" + SessionToolScope(ctx)}
	resolveMemoMu.Lock()
	if cached, ok := resolveMemo[key]; ok {
		resolveMemoMu.Unlock()
		return append([]string{}, cached...)
	}
	resolveMemoMu.Unlock()
	result := r.resolveVisited(ctx, scope, name, includeRegistry, map[string]bool{})
	resolveMemoMu.Lock()
	if len(resolveMemo) >= 256 {
		resolveMemo = map[resolveKey][]string{}
	}
	resolveMemo[key] = append([]string{}, result...)
	resolveMemoMu.Unlock()
	return result
}

func (r *Registry) resolveVisited(ctx context.Context, scope, name string, includeRegistry bool, visited map[string]bool) []string {
	name = CanonicalToolsetName(name)
	if name == "all" || name == "*" {
		all := map[string]bool{}
		for _, tsName := range r.ToolsetNames(ctx, scope) {
			if _, gated := ToolsetSessionPlatforms[tsName]; gated {
				continue
			}
			copyVisited := map[string]bool{}
			for k, v := range visited {
				copyVisited[k] = v
			}
			for _, t := range r.resolveVisited(ctx, scope, tsName, includeRegistry, copyVisited) {
				all[t] = true
			}
		}
		return sortedSet(all)
	}
	if visited[name] {
		return nil
	}
	visited[name] = true
	spec, ok := r.GetToolset(ctx, scope, name, includeRegistry)
	if !ok {
		if includeRegistry {
			return r.pluginPlatformBundle(ctx, scope, name)
		}
		return nil
	}
	tools := map[string]bool{}
	for _, t := range spec.Tools {
		tools[t] = true
	}
	for _, inc := range spec.Includes {
		for _, t := range r.resolveVisited(ctx, scope, inc, includeRegistry, visited) {
			tools[t] = true
		}
	}
	return sortedSet(tools)
}

// AllToolsets returns every static and plugin toolset keyed by display name
func (r *Registry) AllToolsets(ctx context.Context, scope string) map[string]Toolset {
	out := map[string]Toolset{}
	for _, n := range staticToolsetNames() {
		spec, _ := staticToolset(n)
		out[n] = spec
	}
	aliases := r.ToolsetAliases()
	for _, display := range r.pluginDisplayNames(ctx, scope) {
		if _, exists := out[display]; exists {
			continue
		}
		if spec, ok := r.GetToolset(ctx, scope, display, true); ok {
			out[display] = spec
		}
	}
	for name := range aliases {
		if _, static := staticToolset(name); static {
			if spec, ok := r.GetToolset(ctx, scope, name, true); ok {
				out[name] = spec
			}
		}
	}
	return out
}

// OrderedToolsetNames lists static toolsets in table order then plugin display names like the all toolsets dict keys
func (r *Registry) OrderedToolsetNames(ctx context.Context, scope string) []string {
	out := orderedToolsetNames()
	have := make(map[string]bool, len(out))
	for _, n := range out {
		have[n] = true
	}
	for _, display := range r.pluginDisplayNames(ctx, scope) {
		if have[display] {
			continue
		}
		if _, ok := r.GetToolset(ctx, scope, display, true); ok {
			have[display] = true
			out = append(out, display)
		}
	}
	return out
}

// ToolsetNames lists every static and plugin toolset name sorted, aliases excluded
func (r *Registry) ToolsetNames(ctx context.Context, scope string) []string {
	names := map[string]bool{}
	for _, n := range staticToolsetNames() {
		names[n] = true
	}
	for _, n := range r.pluginDisplayNames(ctx, scope) {
		names[n] = true
	}
	return sortedSet(names)
}

// SessionPlatformToolDrops returns the tools of every platform gated toolset this platform lacks
func (r *Registry) SessionPlatformToolDrops(ctx context.Context, scope, platform string) map[string]bool {
	out := map[string]bool{}
	for name, platforms := range ToolsetSessionPlatforms {
		if platforms[platform] {
			continue
		}
		for _, t := range r.ResolveToolset(ctx, scope, name) {
			out[t] = true
		}
	}
	return out
}

// SessionPlatformToolDrops returns the default registry tools of platform gated toolsets the platform lacks
func SessionPlatformToolDrops(platform string) map[string]bool {
	return Default.SessionPlatformToolDrops(context.Background(), ScopeKey(home.Dir(context.Background())), platform)
}

// SessionDisabledToolsets extends a disabled list with platform gated and one shot toolsets
func SessionDisabledToolsets(disabled []string, platform string, oneshot bool) []string {
	have := map[string]bool{}
	for _, d := range disabled {
		have[d] = true
	}
	var gated []string
	names := make([]string, 0, len(ToolsetSessionPlatforms))
	for n := range ToolsetSessionPlatforms {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !ToolsetSessionPlatforms[n][platform] {
			gated = append(gated, n)
		}
	}
	if oneshot {
		gated = append(gated, OneshotDisabledToolsets...)
	}
	var extra []string
	for _, g := range gated {
		if !have[g] {
			extra = append(extra, g)
		}
	}
	if len(extra) == 0 {
		return disabled
	}
	return concat(disabled, extra)
}

// ToolSelectionKey is the order free identity of one enabled and disabled toolset pair
func ToolSelectionKey(enabled, disabled []string) string {
	e := "nil"
	if enabled != nil {
		set := map[string]bool{}
		for _, n := range enabled {
			set[n] = true
		}
		e = strings.Join(sortedSet(set), ",")
	}
	set := map[string]bool{}
	for _, n := range disabled {
		set[n] = true
	}
	return e + "|" + strings.Join(sortedSet(set), ",")
}

// ValidateToolset reports whether a name is a known static, plugin, alias, or wildcard toolset
func (r *Registry) ValidateToolset(ctx context.Context, scope, name string) bool {
	name = CanonicalToolsetName(name)
	if name == "all" || name == "*" {
		return true
	}
	if _, ok := staticToolset(name); ok {
		return true
	}
	if r.pluginToolsetNames(ctx, scope)[name] {
		return true
	}
	_, ok := r.ToolsetAliases()[name]
	return ok
}

// CreateCustomToolset adds or replaces a runtime toolset in the table
func CreateCustomToolset(name, description string, tools, includes []string) {
	toolsetsMu.Lock()
	defer toolsetsMu.Unlock()
	if _, exists := Toolsets[name]; !exists {
		toolsetOrder = append(toolsetOrder, name)
	}
	Toolsets[name] = ts(description, tools, includes...)
	resolveMemoMu.Lock()
	resolveMemo = map[resolveKey][]string{}
	resolveMemoMu.Unlock()
}

// ToolsetDetail is a toolset with its resolved members for display
type ToolsetDetail struct {
	Name          string
	Description   string
	DirectTools   []string
	Includes      []string
	ResolvedTools []string
	ToolCount     int
	IsComposite   bool
}

// ToolsetInfoFor returns a toolset's definition plus its resolved tools, or false when unknown
func (r *Registry) ToolsetInfoFor(ctx context.Context, scope, name string) (ToolsetDetail, bool) {
	spec, ok := r.GetToolset(ctx, scope, name, true)
	if !ok {
		return ToolsetDetail{}, false
	}
	resolved := r.ResolveToolset(ctx, scope, name)
	return ToolsetDetail{
		Name: CanonicalToolsetName(name), Description: spec.Description, DirectTools: spec.Tools, Includes: spec.Includes,
		ResolvedTools: resolved, ToolCount: len(resolved), IsComposite: len(spec.Includes) > 0,
	}, true
}

// LegacyToolsetMap maps retired tools suffixed names onto their tool lists
var LegacyToolsetMap = map[string][]string{
	"web_tools":      {"web_search", "web_extract"},
	"terminal_tools": {"terminal"},
	"vision_tools":   {"vision_analyze"},
	"image_tools":    {"image_generate"},
	"skills_tools":   {"skills_list", "skill_view", "skill_manage"},
	"cronjob_tools":  {"cronjob_manage"},
	"file_tools":     {"read_file", "write_file", "patch", "search_files"},
	"tts_tools":      {"text_to_speech"},
}

var (
	warnedBundlesMu sync.Mutex
	warnedBundles   = map[string]bool{}
)

// SelectionNotice is one line the non quiet selection pass prints
type SelectionNotice struct {
	Text string
}

// applyToolsetSelection adds or subtracts every named toolset, returning the printed notices
func (r *Registry) applyToolsetSelection(ctx context.Context, env *Env, tools map[string]bool, names []string, disable bool) []SelectionNotice {
	scope := scopeOf(env)
	var notices []SelectionNotice
	verb, icon := "Enabled", "[+]"
	if disable {
		verb, icon = "Disabled", "[-]"
	}
	for _, raw := range names {
		name := CanonicalToolsetName(raw)
		var resolved []string
		var label string
		switch {
		case r.ValidateToolset(ctx, scope, name):
			label = verb + " toolset"
			spec, _ := r.GetToolset(ctx, scope, name, true)
			if disable && (strings.HasPrefix(name, BundlePrefix) || spec.Posture) {
				resolved = sortedSet(r.BundleNonCoreTools(ctx, scope, name))
				if strings.HasPrefix(name, BundlePrefix) {
					warnedBundlesMu.Lock()
					first := !warnedBundles[name]
					warnedBundles[name] = true
					warnedBundlesMu.Unlock()
					if first {
						checkLogger(env).Info("agent.disabled_toolsets contains a platform bundle name, core tools are preserved and only its platform specific tools are removed",
							"bundle", name, "removed", strings.Join(resolved, ", "))
					}
				}
			} else {
				resolved = r.ResolveToolset(ctx, scope, name)
			}
		case LegacyToolsetMap[name] != nil:
			label = verb + " legacy toolset"
			resolved = LegacyToolsetMap[name]
		default:
			notices = append(notices, SelectionNotice{Text: "Unknown toolset: " + raw})
			continue
		}
		for _, t := range resolved {
			if disable {
				delete(tools, t)
			} else {
				tools[t] = true
			}
		}
		list := "no tools"
		if len(resolved) > 0 {
			list = strings.Join(resolved, ", ")
		}
		notices = append(notices, SelectionNotice{Text: fmt.Sprintf("%s %s '%s': %s", icon, label, raw, list)})
	}
	return notices
}

// SelectionContext carries the kanban worker facts that widen a toolset selection
type SelectionContext struct {
	KanbanTask            bool
	DelegatedChild        bool
	DispatcherOwnedWorker bool
}

// SelectToolNames expands enabled toolsets then subtracts disabled ones at tool granularity
func (r *Registry) SelectToolNames(ctx context.Context, env *Env, enabled, disabled []string, sel SelectionContext) (map[string]bool, []SelectionNotice) {
	tools := map[string]bool{}
	var notices []SelectionNotice
	scope := scopeOf(env)
	if enabled != nil {
		list := append([]string{}, enabled...)
		if sel.KanbanTask && !sel.DelegatedChild && sel.DispatcherOwnedWorker && !containsString(list, "kanban") {
			list = append(list, "kanban")
		}
		notices = append(notices, r.applyToolsetSelection(ctx, env, tools, list, false)...)
	} else {
		for name := range r.AllToolsets(ctx, scope) {
			if _, gated := ToolsetSessionPlatforms[name]; gated {
				continue
			}
			for _, t := range r.ResolveToolset(ctx, scope, name) {
				tools[t] = true
			}
		}
	}
	if len(disabled) > 0 {
		notices = append(notices, r.applyToolsetSelection(ctx, env, tools, disabled, true)...)
	}
	return tools, notices
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Distribution maps toolset entries to the percent chance each is enabled per prompt, Order keeping declaration order
type Distribution struct {
	Description string
	Toolsets    map[string]int
	Order       []string
}

// Distribution from alternating entry and percent arguments in declaration order
func dist(description string, pairs ...any) Distribution {
	d := Distribution{Description: description, Toolsets: map[string]int{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		entry := pairs[i].(string)
		d.Toolsets[entry] = pairs[i+1].(int)
		d.Order = append(d.Order, entry)
	}
	return d
}

// Distributions are the batch data generation toolset distributions
var Distributions = map[string]Distribution{
	"default":        dist("All available tools, all the time", "web", 100, "vision", 100, "image_gen", 100, "terminal", 100, "file", 100, "browser", 100),
	"image_gen":      dist("Heavy focus on image generation with vision and web support", "image_gen", 90, "vision", 90, "web", 55, "terminal", 45),
	"research":       dist("Web research with vision analysis and reasoning", "web", 90, "browser", 70, "vision", 50, "terminal", 10),
	"science":        dist("Scientific research with web, terminal, file, and browser capabilities", "web", 94, "terminal", 94, "file", 94, "vision", 65, "browser", 50, "image_gen", 15),
	"development":    dist("Terminal, file tools, and reasoning with occasional web lookup", "terminal", 80, "file", 80, "web", 30, "vision", 10),
	"safe":           dist("All tools except terminal for safety", "web", 80, "browser", 70, "vision", 60, "image_gen", 60),
	"balanced":       dist("Equal probability of all toolsets", "web", 50, "vision", 50, "image_gen", 50, "terminal", 50, "file", 50, "browser", 50),
	"minimal":        dist("Only web tools for basic research", "web", 100),
	"terminal_only":  dist("Terminal and file tools for code execution tasks", "terminal", 100, "file", 100),
	"terminal_web":   dist("Terminal and file tools with web search for documentation lookup", "terminal", 100, "file", 100, "web", 100),
	"creative":       dist("Image generation and vision analysis focus", "image_gen", 90, "vision", 90, "web", 30),
	"reasoning":      dist("Heavy research/reasoning distribution with minimal other tools", "web", 90, "file", 60, "terminal", 20),
	"browser_use":    dist("Full browser-based web interaction with search, vision, and page control", "browser", 100, "web", 80, "vision", 70),
	"browser_only":   dist("Only browser automation tools for pure web interaction tasks", "browser", 100),
	"browser_tasks":  dist("Browser-focused distribution with web_search for finding URLs (Google blocks direct browser searches)", "browser+search", 97, "vision", 12, "terminal", 15),
	"terminal_tasks": dist("Terminal-focused distribution with high terminal/file availability, occasional other tools", "terminal", 97, "file", 97, "web", 97, "browser", 75, "vision", 50, "image_gen", 10),
	"mixed_tasks":    dist("Mixed distribution with high browser, terminal, and file availability for complex tasks", "browser", 92, "terminal", 92, "file", 92, "web", 35, "vision", 15, "image_gen", 15),
}

// GetDistribution returns the named distribution
func GetDistribution(name string) (Distribution, bool) {
	d, ok := Distributions[name]
	return d, ok
}

// ListDistributions returns a copy of every distribution keyed by name
func ListDistributions() map[string]Distribution {
	out := make(map[string]Distribution, len(Distributions))
	for k, v := range Distributions {
		out[k] = v
	}
	return out
}

// PrintDistributionInfo writes a distribution's description and toolset chances highest first
func PrintDistributionInfo(w io.Writer, name string) {
	d, ok := Distributions[name]
	if !ok {
		fmt.Fprintf(w, "❌ Unknown distribution: %s\n", name)
		return
	}
	fmt.Fprintf(w, "\n📊 Distribution: %s\n", name)
	fmt.Fprintf(w, "   Description: %s\n", d.Description)
	fmt.Fprintln(w, "   Toolsets:")
	order := append([]string{}, d.Order...)
	sort.SliceStable(order, func(i, j int) bool { return d.Toolsets[order[i]] > d.Toolsets[order[j]] })
	for _, entry := range order {
		fmt.Fprintf(w, "     • %-15s : %3d%% chance\n", entry, d.Toolsets[entry])
	}
}

// ValidateDistribution reports whether a distribution name exists
func ValidateDistribution(name string) bool {
	_, ok := Distributions[name]
	return ok
}

func distributionMembers(entry string) []string {
	parts := strings.Split(entry, "+")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// SampleToolsetsFromDistribution rolls each entry independently, grouped entries roll once together
func (r *Registry) SampleToolsetsFromDistribution(ctx context.Context, scope, name string, rng *rand.Rand) ([]string, []string, error) {
	d, ok := Distributions[name]
	if !ok {
		return nil, nil, fmt.Errorf("unknown distribution: %s", name)
	}
	entries := d.Order
	var selected, warnings []string
	for _, entry := range entries {
		members := distributionMembers(entry)
		var invalid []string
		for _, m := range members {
			if !r.ValidateToolset(ctx, scope, m) {
				invalid = append(invalid, m)
			}
		}
		if len(invalid) > 0 {
			warnings = append(warnings, fmt.Sprintf("Toolset '%s' in distribution '%s' is not valid", strings.Join(invalid, "+"), name))
			continue
		}
		if rng.Float64()*100 < float64(d.Toolsets[entry]) {
			selected = append(selected, members...)
		}
	}
	if len(selected) == 0 && len(d.Toolsets) > 0 {
		best, bestP := "", -1
		for _, e := range entries {
			if d.Toolsets[e] > bestP {
				best, bestP = e, d.Toolsets[e]
			}
		}
		members := distributionMembers(best)
		valid := true
		for _, m := range members {
			if !r.ValidateToolset(ctx, scope, m) {
				valid = false
			}
		}
		if valid {
			selected = append(selected, members...)
		}
	}
	return selected, warnings, nil
}

// PlatformInfo is one messaging or local platform with its default bundle
type PlatformInfo struct {
	Label          string
	DefaultToolset string
}

// Platforms is the ordered platform registry the tool configurator serves
var Platforms = []struct {
	Key  string
	Info PlatformInfo
}{
	{"cli", PlatformInfo{"CLI", "icebath-cli"}},
	{"telegram", PlatformInfo{"Telegram", "icebath-telegram"}},
	{"discord", PlatformInfo{"Discord", "icebath-discord"}},
	{"slack", PlatformInfo{"Slack", "icebath-slack"}},
	{"whatsapp", PlatformInfo{"WhatsApp", "icebath-whatsapp"}},
	{"whatsapp_cloud", PlatformInfo{"WhatsApp Business (Cloud)", "icebath-whatsapp"}},
	{"signal", PlatformInfo{"Signal", "icebath-signal"}},
	{"bluebubbles", PlatformInfo{"BlueBubbles", "icebath-bluebubbles"}},
	{"email", PlatformInfo{"Email", "icebath-email"}},
	{"mattermost", PlatformInfo{"Mattermost", "icebath-mattermost"}},
	{"matrix", PlatformInfo{"Matrix", "icebath-matrix"}},
	{"dingtalk", PlatformInfo{"DingTalk", "icebath-dingtalk"}},
	{"feishu", PlatformInfo{"Feishu", "icebath-feishu"}},
	{"wecom", PlatformInfo{"WeCom", "icebath-wecom"}},
	{"wecom_callback", PlatformInfo{"WeCom Callback", "icebath-wecom-callback"}},
	{"weixin", PlatformInfo{"Weixin", "icebath-weixin"}},
	{"qqbot", PlatformInfo{"QQBot", "icebath-qqbot"}},
	{"yuanbao", PlatformInfo{"Yuanbao", "icebath-yuanbao"}},
	{"webhook", PlatformInfo{"Webhook", "icebath-webhook"}},
	{"api_server", PlatformInfo{"API Server", "icebath-api-server"}},
	{"cron", PlatformInfo{"Cron", "icebath-cron"}},
}

// PlatformInfoFor returns a known platform's info
func PlatformInfoFor(key string) (PlatformInfo, bool) {
	for _, p := range Platforms {
		if p.Key == key {
			return p.Info, true
		}
	}
	return PlatformInfo{}, false
}

// PlatformDefaultToolset returns the composite a platform falls back to
func PlatformDefaultToolset(platform string) string {
	if info, ok := PlatformInfoFor(platform); ok {
		return info.DefaultToolset
	}
	return BundlePrefix + platform
}

// ConfigurableToolset is one checklist row the configurator shows
type ConfigurableToolset struct {
	Key, Label, Description string
}

// ConfigurableToolsets are the toolsets the configurator checklist offers
var ConfigurableToolsets = []ConfigurableToolset{
	{"web", "Web Search & Scraping", "web_search, web_extract"},
	{"browser", "Browser Automation", "navigate, click, type, scroll"},
	{"terminal", "Terminal & Processes", "terminal, process"},
	{"file", "File Operations", "read, write, patch, search"},
	{"code_execution", "Code Execution", "execute_code"},
	{"vision", "Vision / Image Analysis", "vision_analyze"},
	{"video", "Video Analysis", "video_analyze (requires video-capable model)"},
	{"image_gen", "Image Generation", "image_generate"},
	{"video_gen", "Video Generation", "video_generate (text/image/reference)"},
	{"x_search", "X (Twitter) Search", "x_search (requires xAI OAuth or XAI_API_KEY)"},
	{"tts", "Text-to-Speech", "text_to_speech"},
	{"stt", "Speech-to-Text", "voice transcription (gateway voice messages + voice mode)"},
	{"skills", "Skills", "list, view, manage"},
	{"todo", "Task Planning", "todo_list"},
	{"kanban", "Kanban", "opt-in task board tools for this platform"},
	{"memory", "Memory", "persistent memory across sessions"},
	{"context_engine", "Context Engine", "runtime tools from the active context engine"},
	{"session_search", "Session Search", "search past conversations"},
	{"connections", "Connections", "remote connector tools and account authorization"},
	{"clarify", "Clarifying Questions", "clarify"},
	{"delegation", "Task Delegation", "delegate_task"},
	{"cronjob", "Cron Jobs", "create/list/update/pause/resume/run, with optional attached skills"},
	{"discord", "Discord (read/participate)", "fetch messages, search members, create thread"},
	{"discord_admin", "Discord Server Admin", "list channels/roles, pin, assign roles"},
	{"yuanbao", "Yuanbao", "group info, member queries, DM"},
	{"computer_use", "Computer Use (macOS/Windows/Linux)", "background desktop control via cua-driver"},
}

// DefaultOffToolsets are off for new installs until a user enables them
var DefaultOffToolsets = map[string]bool{"spotify": true, "discord": true, "discord_admin": true, "video": true, "video_gen": true, "x_search": true, "a2a": true, "kanban": true}

// ConfigOnlyToolsets have provider setup but expose no model schemas
var ConfigOnlyToolsets = map[string]bool{"stt": true}

// ToolsetPlatformRestrictions limits a toolset to the platforms that can host it
var ToolsetPlatformRestrictions = map[string]map[string]bool{"discord": {"discord": true}, "discord_admin": {"discord": true}}

// ToolsetAllowedForPlatform reports whether a toolset may run on a platform
func ToolsetAllowedForPlatform(key, platform string) bool {
	allowed, ok := ToolsetPlatformRestrictions[key]
	return !ok || allowed[platform]
}

// RecentlyShippedToolsets ship on by default for saved lists that predate them
var RecentlyShippedToolsets = map[string]bool{}

func configurableKeys() map[string]bool {
	out := map[string]bool{}
	for _, c := range ConfigurableToolsets {
		out[c.Key] = true
	}
	return out
}

// Keys of every configurable toolset as a fresh set
func ConfigurableKeys() map[string]bool { return configurableKeys() }

func platformDefaultKeys() map[string]bool {
	out := map[string]bool{}
	for _, p := range Platforms {
		out[p.Info.DefaultToolset] = true
	}
	return out
}

// ParsePlatformToolsetsValue reads a saved platform_toolsets value as the list it encodes
func ParsePlatformToolsetsValue(value any) ([]string, bool) {
	switch v := value.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, pyrepr.Str(item))
		}
		return out, true
	case []string:
		return append([]string{}, v...), true
	case string:
		trimmed := strings.TrimSpace(v)
		if !strings.HasPrefix(trimmed, "[") {
			return nil, false
		}
		items, ok := parseListLiteral(trimmed)
		if !ok {
			return nil, false
		}
		return items, true
	}
	return nil, false
}

// parseListLiteral reads a JSON or Python style list literal of scalars
func parseListLiteral(text string) ([]string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "[") || !strings.HasSuffix(text, "]") {
		return nil, false
	}
	body := strings.TrimSpace(text[1 : len(text)-1])
	if body == "" {
		return []string{}, true
	}
	var out []string
	var cur strings.Builder
	quote := byte(0)
	flush := func() bool {
		item := strings.TrimSpace(cur.String())
		cur.Reset()
		if item == "" {
			return false
		}
		if len(item) >= 2 && (item[0] == '"' || item[0] == '\'') && item[len(item)-1] == item[0] {
			item = item[1 : len(item)-1]
		}
		out = append(out, item)
		return true
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case quote != 0:
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
			cur.WriteByte(c)
		case c == ',':
			if !flush() {
				return nil, false
			}
		case c == '[' || c == ']':
			return nil, false
		default:
			cur.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, false
	}
	if strings.TrimSpace(cur.String()) != "" && !flush() {
		return nil, false
	}
	return out, true
}

// ParseConfigStringList reads a config value that may hold a list literal string into names
func ParseConfigStringList(value any) []string {
	switch v := value.(type) {
	case string:
		if items, ok := parseListLiteral(v); ok && strings.HasPrefix(strings.TrimSpace(v), "[") {
			return items
		}
		return []string{v}
	case []string:
		return append([]string{}, v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return out
	}
	return nil
}

func legacyExpandPlatformNames(names []string) []string {
	var out []string
	for _, n := range names {
		switch n {
		case "icebath", "hermes":
			out = append(out, "icebath-cli", "icebath-api-server")
		default:
			out = append(out, CanonicalToolsetName(n))
		}
	}
	return out
}

// PlatformToolsOptions carries the plugin and MCP facts the platform resolver needs
type PlatformToolsOptions struct {
	PluginToolsetKeys        map[string]bool
	PortableMCPServers       map[string]bool
	IncludeDefaultMCPServers bool
}

// MCPServerEnabled reports whether an mcp_servers entry is enabled unless explicitly off
func MCPServerEnabled(serverCfg map[string]any) bool {
	v, ok := serverCfg["enabled"]
	if !ok {
		v = true
	}
	return parseMCPBoolish(v, true)
}

var (
	mcpTrueWords    = map[string]bool{"true": true, "1": true, "yes": true, "on": true}
	mcpFalseWords   = map[string]bool{"false": true, "0": true, "no": true, "off": true}
	mcpCommonLogger = slog.Default
)

// Numbers by truthiness, words from a fixed set, anything else the default with a warning
func parseMCPBoolish(value any, def bool) bool {
	switch v := value.(type) {
	case nil:
		return def
	case bool:
		return v
	case string:
		lowered := strings.ToLower(strings.TrimSpace(v))
		if mcpTrueWords[lowered] {
			return true
		}
		if mcpFalseWords[lowered] {
			return false
		}
	default:
		if n, isInt := pyrepr.Int(v); isInt {
			return n.Sign() != 0
		}
		if name := pyrepr.TypeName(v); name == "float" {
			return pyTruthy(v)
		}
	}
	mcpCommonLogger().Warn(fmt.Sprintf("MCP config expected a boolean-ish value, got %s; using default=%s", pyrepr.Repr(value), pyrepr.Str(def)))
	return def
}

// EnabledMCPServerNames lists MCP servers enabled in config plus portable plugin servers
func EnabledMCPServerNames(cfg config.Tree, portable map[string]bool) map[string]bool {
	names := map[string]bool{}
	servers := cfg.Map("mcp_servers")
	for name, raw := range servers {
		if m := config.AsMap(raw); m != nil && MCPServerEnabled(m) {
			names[name] = true
		}
	}
	for p := range portable {
		if _, native := servers[p]; !native {
			names[p] = true
		}
	}
	return names
}

func configurableSubsetOf(ctx context.Context, r *Registry, scope string, toolNames map[string]bool, platform string) map[string]bool {
	out := map[string]bool{}
	for _, c := range ConfigurableToolsets {
		if !ToolsetAllowedForPlatform(c.Key, platform) {
			continue
		}
		members := r.ResolveStaticToolset(ctx, scope, c.Key)
		if len(members) == 0 {
			continue
		}
		subset := true
		for _, m := range members {
			if !toolNames[m] {
				subset = false
				break
			}
		}
		if subset {
			out[c.Key] = true
		}
	}
	return out
}

func defaultOffToolsets(platform string, explicit bool) map[string]bool {
	off := map[string]bool{}
	for k := range DefaultOffToolsets {
		off[k] = true
	}
	if off[platform] {
		if _, restricted := ToolsetPlatformRestrictions[platform]; !restricted {
			delete(off, platform)
		}
	}
	if explicit {
		for k := range off {
			if ToolsetPlatformRestrictions[k][platform] {
				delete(off, k)
			}
		}
	}
	return off
}

func (r *Registry) enableRecentlyShipped(ctx context.Context, scope string, enabled map[string]bool, cfg config.Tree, platform string) {
	declined := map[string]bool{}
	for _, n := range config.AsStrings(cfg.Sub("known_builtin_toolsets")[platform]) {
		declined[n] = true
	}
	var composite map[string]bool
	for _, key := range sortedSet(RecentlyShippedToolsets) {
		if enabled[key] || declined[key] || !ToolsetAllowedForPlatform(key, platform) {
			continue
		}
		members := r.ResolveStaticToolset(ctx, scope, key)
		if composite == nil {
			composite = map[string]bool{}
			for _, t := range r.ResolveToolset(ctx, scope, PlatformDefaultToolset(platform)) {
				composite[t] = true
			}
		}
		if len(members) == 0 {
			continue
		}
		subset := true
		for _, m := range members {
			if !composite[m] {
				subset = false
			}
		}
		if subset {
			enabled[key] = true
		}
	}
}

func (r *Registry) explicitToolsets(ctx context.Context, scope string, names []string, known map[string]bool, cfg config.Tree, platform string, explicit bool) map[string]bool {
	enabled := map[string]bool{}
	compositeTools := map[string]bool{}
	for _, n := range names {
		if known[n] && ToolsetAllowedForPlatform(n, platform) {
			enabled[n] = true
			continue
		}
		if _, static := staticToolset(n); static && !known[n] {
			for _, t := range r.ResolveToolset(ctx, scope, n) {
				compositeTools[t] = true
			}
		}
	}
	if len(compositeTools) > 0 {
		off := defaultOffToolsets(platform, explicit)
		for k := range configurableSubsetOf(ctx, r, scope, compositeTools, platform) {
			if !off[k] {
				enabled[k] = true
			}
		}
	}
	r.enableRecentlyShipped(ctx, scope, enabled, cfg, platform)
	return enabled
}

func (r *Registry) compositeToolsets(ctx context.Context, scope string, names []string, platform string, explicit bool) map[string]bool {
	all := map[string]bool{}
	for _, n := range names {
		for _, t := range r.ResolveToolset(ctx, scope, n) {
			all[t] = true
		}
	}
	enabled := configurableSubsetOf(ctx, r, scope, all, platform)
	off := defaultOffToolsets(platform, explicit)
	if ToolsetAllowedForPlatform("x_search", platform) && xaiCredentialsPresent(ctx) {
		enabled["x_search"] = true
		delete(off, "x_search")
	}
	for k := range off {
		delete(enabled, k)
	}
	return enabled
}

func (r *Registry) recoverPlatformNativeToolsets(ctx context.Context, scope string, enabled map[string]bool, platform string, skip map[string]bool) {
	universe := map[string]bool{}
	for _, t := range r.ResolveToolset(ctx, scope, PlatformDefaultToolset(platform)) {
		universe[t] = true
	}
	configurable := map[string]bool{}
	for _, c := range ConfigurableToolsets {
		for _, t := range r.ResolveToolset(ctx, scope, c.Key) {
			configurable[t] = true
		}
	}
	claimed := map[string]bool{}
	for k := range enabled {
		for _, t := range r.ResolveToolset(ctx, scope, k) {
			claimed[t] = true
		}
	}
	for _, name := range orderedToolsetNames() {
		spec, _ := staticToolset(name)
		if skip[name] || strings.HasPrefix(name, BundlePrefix) || (DefaultOffToolsets[name] && name != platform) || len(spec.Includes) > 0 || spec.Posture {
			continue
		}
		members := r.ResolveStaticToolset(ctx, scope, name)
		if len(members) == 0 {
			continue
		}
		inUniverse, allConfigurable, allClaimed := true, true, true
		for _, m := range members {
			if !universe[m] {
				inUniverse = false
			}
			if !configurable[m] {
				allConfigurable = false
			}
			if !claimed[m] {
				allClaimed = false
			}
		}
		if !inUniverse || allConfigurable || allClaimed {
			continue
		}
		enabled[name] = true
		for _, m := range members {
			claimed[m] = true
		}
	}
}

func mergeMCPServers(cfg config.Tree, names []string, passthrough map[string]bool, includeDefault bool, portable map[string]bool) map[string]bool {
	enabledServers := EnabledMCPServerNames(cfg, portable)
	known := map[string]bool{}
	for k := range enabledServers {
		known[k] = true
	}
	for k := range cfg.Map("mcp_servers") {
		known[k] = true
	}
	normalized := map[string]bool{}
	for p := range passthrough {
		if strings.HasPrefix(p, "mcp-") && !known[p] && known[p[4:]] {
			normalized[p[4:]] = true
		} else {
			normalized[p] = true
		}
	}
	result := map[string]bool{}
	for p := range normalized {
		if !known[p] {
			result[p] = true
		}
	}
	if containsString(names, "no_mcp") {
		delete(result, "no_mcp")
		return result
	}
	anyKnown := false
	for p := range normalized {
		if known[p] {
			anyKnown = true
		}
	}
	if includeDefault && !anyKnown {
		for k := range enabledServers {
			result[k] = true
		}
		return result
	}
	for p := range normalized {
		if enabledServers[p] {
			result[p] = true
		}
	}
	return result
}

// PlatformToolsExplicitlySaved reports whether platform_toolsets holds a saved list for the platform
func PlatformToolsExplicitlySaved(cfg config.Tree, platform string) bool {
	_, isList := coercePlatformToolsetsValue(cfg.Sub("platform_toolsets")[platform], platform)
	return isList
}

// PlatformTools resolves the enabled toolset names for one platform from config
func (r *Registry) PlatformTools(ctx context.Context, scope string, cfg config.Tree, platform string, opts PlatformToolsOptions) map[string]bool {
	names, explicit := coercePlatformToolsetsValue(cfg.Sub("platform_toolsets")[platform], platform)
	if !explicit {
		names = []string{PlatformDefaultToolset(platform)}
	}
	names = legacyExpandPlatformNames(names)
	configurable := configurableKeys()
	pluginKeys := opts.PluginToolsetKeys
	if pluginKeys == nil {
		pluginKeys = PluginToolsetKeys(ctx)
	}
	defaults := platformDefaultKeys()
	known := map[string]bool{}
	for k := range configurable {
		known[k] = true
	}
	for k := range pluginKeys {
		known[k] = true
	}
	anyKnown := false
	for _, n := range names {
		if known[n] {
			anyKnown = true
		}
	}
	var enabled map[string]bool
	if anyKnown {
		enabled = r.explicitToolsets(ctx, scope, names, known, cfg, platform, explicit)
	} else {
		enabled = r.compositeToolsets(ctx, scope, names, platform, explicit)
	}
	skip := map[string]bool{}
	for k := range known {
		skip[k] = true
	}
	for k := range defaults {
		skip[k] = true
	}
	r.recoverPlatformNativeToolsets(ctx, scope, enabled, platform, skip)
	if len(pluginKeys) > 0 {
		for p := range enabledPluginToolsets(cfg, platform, names, pluginKeys) {
			enabled[p] = true
		}
	}
	if contextEngineActive(cfg) && !(explicit && len(names) == 0) {
		enabled["context_engine"] = true
	}
	passthrough := map[string]bool{}
	for _, n := range names {
		if !known[n] && !defaults[n] {
			passthrough[n] = true
		}
	}
	portable := opts.PortableMCPServers
	if portable == nil {
		portable = portableMCPServerNames(ctx)
	}
	for k := range mergeMCPServers(cfg, names, passthrough, opts.IncludeDefaultMCPServers, portable) {
		enabled[k] = true
	}
	if !explicit && pyContainsKanban(cfg["toolsets"]) {
		enabled["kanban"] = true
	}
	if disabled := cfg.Get("agent.disabled_toolsets"); disabled != nil {
		var disabledNames []string
		for _, n := range ParseConfigStringList(disabled) {
			if strings.TrimSpace(n) != "" {
				disabledNames = append(disabledNames, strings.TrimSpace(n))
			}
		}
		if len(disabledNames) > 0 {
			enabled = r.pruneToolsetsStrippedByDisabled(ctx, scope, enabled, disabledNames)
		}
	}
	if explicit && len(names) > 0 {
		r.warnAllInvalidPlatformToolsets(ctx, scope, platform, names, cfg)
	}
	return enabled
}

func (r *Registry) pruneToolsetsStrippedByDisabled(ctx context.Context, scope string, enabled map[string]bool, disabled []string) map[string]bool {
	remaining := map[string]bool{}
	for k := range enabled {
		if !containsString(disabled, k) {
			remaining[k] = true
		}
	}
	resolved := map[string][]string{}
	surviving := map[string]bool{}
	for name := range remaining {
		var tools []string
		if r.ValidateToolset(ctx, scope, name) {
			tools = r.ResolveToolset(ctx, scope, name)
		}
		resolved[name] = tools
		for _, t := range tools {
			surviving[t] = true
		}
	}
	r.applyToolsetSelection(ctx, nil, surviving, disabled, true)
	out := map[string]bool{}
	for name, tools := range resolved {
		if len(tools) == 0 {
			out[name] = true
			continue
		}
		for _, t := range tools {
			if surviving[t] {
				out[name] = true
				break
			}
		}
	}
	return out
}

// DisabledEntryTools lists the tools one agent.disabled_toolsets entry removes at runtime
func (r *Registry) DisabledEntryTools(ctx context.Context, scope, name string) map[string]bool {
	universe := map[string]bool{}
	for _, t := range r.ResolveToolset(ctx, scope, name) {
		universe[t] = true
	}
	for _, t := range LegacyToolsetMap[name] {
		universe[t] = true
	}
	kept := map[string]bool{}
	for t := range universe {
		kept[t] = true
	}
	r.applyToolsetSelection(ctx, nil, kept, []string{name}, true)
	out := map[string]bool{}
	for t := range universe {
		if !kept[t] {
			out[t] = true
		}
	}
	return out
}

// PartialStrip reports the tools a disabled list cuts out of an enabled toolset without removing it
type PartialStrip struct {
	Stripped []string
	Entries  []string
	Hint     string
}

// ToolsetsPartiallyStripped reports enabled toolsets that disabled entries cut down but leave enabled
func (r *Registry) ToolsetsPartiallyStripped(ctx context.Context, scope string, cfg config.Tree, enabled map[string]bool) map[string]PartialStrip {
	var disabled []string
	for _, n := range ParseConfigStringList(cfg.Get("agent.disabled_toolsets")) {
		if strings.TrimSpace(n) != "" {
			disabled = append(disabled, strings.TrimSpace(n))
		}
	}
	report := map[string]PartialStrip{}
	for _, name := range sortedSet(enabled) {
		tools := map[string]bool{}
		if r.ValidateToolset(ctx, scope, name) {
			for _, t := range r.ResolveToolset(ctx, scope, name) {
				tools[t] = true
			}
		}
		by := map[string]map[string]bool{}
		for _, entry := range disabled {
			kept := map[string]bool{}
			for t := range tools {
				kept[t] = true
			}
			r.applyToolsetSelection(ctx, nil, kept, []string{entry}, true)
			removed := map[string]bool{}
			for t := range tools {
				if !kept[t] {
					removed[t] = true
				}
			}
			if len(removed) > 0 {
				by[entry] = removed
			}
		}
		if len(by) == 0 {
			continue
		}
		entries := sortedSet(map[string]bool(func() map[string]bool {
			m := map[string]bool{}
			for e := range by {
				m[e] = true
			}
			return m
		}()))
		clears := true
		for _, e := range entries {
			for t := range r.DisabledEntryTools(ctx, scope, e) {
				if !tools[t] {
					clears = false
				}
			}
		}
		stripped := map[string]bool{}
		for _, removed := range by {
			for t := range removed {
				stripped[t] = true
			}
		}
		hint := fmt.Sprintf("remove %s from agent.disabled_toolsets to restore", strings.Join(entries, ", "))
		if clears {
			hint = fmt.Sprintf("run `icebath tools enable %s` to restore", name)
		}
		report[name] = PartialStrip{Stripped: sortedSet(stripped), Entries: entries, Hint: hint}
	}
	return report
}

// SavePlatformTools writes a platform's selected toolset keys into the config tree
func (r *Registry) SavePlatformTools(ctx context.Context, scope string, cfg config.Tree, platform string, enabled map[string]bool, pluginKeys map[string]bool) error {
	filtered := map[string]bool{}
	for k := range enabled {
		if ToolsetAllowedForPlatform(k, platform) {
			filtered[k] = true
		}
	}
	drop := configurableKeys()
	for k := range pluginKeys {
		drop[k] = true
	}
	for k := range platformDefaultKeys() {
		drop[k] = true
	}
	drop["no_mcp"] = true
	existing, _ := coercePlatformToolsetsValue(cfg.Sub("platform_toolsets")[platform], platform)
	preserved := map[string]bool{}
	for _, e := range existing {
		if !drop[e] && !drop[CanonicalToolsetName(e)] {
			preserved[e] = true
		}
	}
	all := map[string]bool{}
	for k := range filtered {
		all[k] = true
	}
	for k := range preserved {
		all[k] = true
	}
	list := make([]any, 0, len(all))
	for _, k := range sortedSet(all) {
		list = append(list, k)
	}
	if err := cfg.Set("platform_toolsets."+platform, list); err != nil {
		return err
	}
	if len(pluginKeys) > 0 {
		keys := make([]any, 0, len(pluginKeys))
		for _, k := range sortedSet(pluginKeys) {
			keys = append(keys, k)
		}
		if err := cfg.Set("known_plugin_toolsets."+platform, keys); err != nil {
			return err
		}
	}
	builtin := make([]any, 0, len(ConfigurableToolsets))
	for _, k := range sortedSet(configurableKeys()) {
		builtin = append(builtin, k)
	}
	if err := cfg.Set("known_builtin_toolsets."+platform, builtin); err != nil {
		return err
	}
	newly := map[string]bool{}
	for k := range filtered {
		if !preserved[k] {
			newly[k] = true
		}
	}
	rawDisabled := cfg.Get("agent.disabled_toolsets")
	if rawDisabled != nil && len(newly) > 0 {
		parsed := ParseConfigStringList(rawDisabled)
		if len(parsed) > 0 {
			covered := map[string]bool{}
			for k := range newly {
				for _, t := range r.ResolveToolset(ctx, scope, k) {
					covered[t] = true
				}
			}
			var remaining []any
			changed := false
			for _, entry := range parsed {
				if newly[entry] {
					changed = true
					continue
				}
				tools := r.DisabledEntryTools(ctx, scope, entry)
				allCovered := len(tools) > 0
				for t := range tools {
					if !covered[t] {
						allCovered = false
					}
				}
				if allCovered {
					changed = true
					continue
				}
				remaining = append(remaining, entry)
			}
			if changed {
				if remaining == nil {
					remaining = []any{}
				}
				if err := cfg.Set("agent.disabled_toolsets", remaining); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// IsKnownPlatformBundle reports whether a name is the bundle of a platform icebath knows
func IsKnownPlatformBundle(platform, name string) bool {
	if CanonicalToolsetName(name) != PlatformDefaultToolset(platform) {
		return false
	}
	if _, ok := PlatformInfoFor(platform); ok || bundledPlatformNames()[platform] {
		return true
	}
	if LeftCorePlatforms != nil && containsString(LeftCorePlatforms(), platform) {
		return true
	}
	return PlatformRegistered(platform)
}

// LeftCorePlatforms lists the platforms that left core for a catalog plugin, installed by the provider slice
var LeftCorePlatforms func() []string

// Bundled platform plugin names under the install root, read once per process
var bundledPlatformNames = sync.OnceValue(loadBundledPlatformNames)

func loadBundledPlatformNames() map[string]bool {
	out := map[string]bool{}
	for _, n := range config.BundledPlatformDirNames(home.Process()) {
		out[n] = true
	}
	return out
}

const noToolsSuffix = "the agent will have no tools on this platform. Run `icebath tools` to reconfigure."

// ValidatePlatformToolsets returns the warnings for a platform_toolsets mapping judged with the platform scope rules
func ValidatePlatformToolsets(platformToolsets map[string]any, isValid func(string) bool) []string {
	return ValidatePlatformToolsetsWith(platformToolsets, isValid, ToolsetAllowedForPlatform)
}

// Default toolset is valid when the resolver accepts it on the platform or a plugin platform registered it
func platformDefaultIsValid(platform, def string, isValid func(string) bool, isAllowed func(string, string) bool) bool {
	if isValid(def) && isAllowed(def, platform) {
		return true
	}
	return platformRegisteredSafe(platform)
}

// Platform registry lookup where a failing lookup counts as not registered
func platformRegisteredSafe(platform string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return PlatformRegistered(platform)
}

// Mapping entries in Python iteration order, a Go map in sorted key order, nil for a non mapping or empty one
func platformToolsetsItems(value any) [][2]any {
	var items [][2]any
	switch m := value.(type) {
	case *pyjson.Object:
		if m == nil {
			return nil
		}
		for _, k := range m.Keys {
			v, _ := m.Get(k)
			items = append(items, [2]any{k, v})
		}
	case config.Tree:
		return platformToolsetsItems(map[string]any(m))
	case map[string]any:
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			items = append(items, [2]any{k, m[k]})
		}
	}
	return items
}

// Saved list as stored, string items kept and others marked so non string entries are skipped like upstream
func platformToolsetsRawList(raw any) ([]any, bool) {
	switch v := raw.(type) {
	case []any:
		return v, true
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out, true
	}
	names, ok := ParsePlatformToolsetsValue(raw)
	if !ok {
		return nil, false
	}
	out := make([]any, len(names))
	for i, s := range names {
		out[i] = s
	}
	return out, true
}

// ValidatePlatformToolsetsWith reports unknown, platform restricted, empty and non list entries of a platform_toolsets mapping
func ValidatePlatformToolsetsWith(platformToolsets any, isValid func(string) bool, isAllowed func(string, string) bool) []string {
	warnings := []string{}
	items := platformToolsetsItems(platformToolsets)
	if len(items) == 0 {
		return warnings
	}
	valid := 0
	for _, item := range items {
		platform, raw := pyrepr.Str(item[0]), item[1]
		def := PlatformDefaultToolset(platform)
		defaultValid := platformDefaultIsValid(platform, def, isValid, isAllowed)
		platformValid := 0
		list, ok := platformToolsetsRawList(raw)
		if !ok {
			if defaultValid {
				valid++
				platformValid++
			}
			fallback := fmt.Sprintf("falling back to unknown default '%s'", def)
			if defaultValid {
				fallback = fmt.Sprintf("falling back to '%s'", def)
			}
			var detail string
			switch v := raw.(type) {
			case nil:
				detail = "a null toolset value"
			case string:
				detail = fmt.Sprintf("invalid toolset value '%s'", v)
			default:
				detail = fmt.Sprintf("invalid %s toolset value", pyrepr.TypeName(raw))
			}
			warnings = append(warnings, fmt.Sprintf("platform '%s' has %s — %s. Run `icebath tools` to configure explicitly.", platform, detail, fallback))
			if platformValid == 0 {
				warnings = append(warnings, fmt.Sprintf("platform '%s' has no valid toolsets configured — %s", platform, noToolsSuffix))
			}
			continue
		}
		for _, entry := range list {
			name, isStr := entry.(string)
			if !isStr || name == "" {
				continue
			}
			switch {
			case !isValid(name) && !IsKnownPlatformBundle(platform, name):
				hint := ""
				if defaultValid {
					hint = fmt.Sprintf(" — did you mean '%s'?", def)
				}
				warnings = append(warnings, fmt.Sprintf("platform '%s' references unknown toolset '%s'%s", platform, name, hint))
			case isAllowed(name, platform):
				valid++
				platformValid++
			default:
				warnings = append(warnings, fmt.Sprintf("platform '%s' references toolset '%s' which is not available on this platform", platform, name))
			}
		}
		if platformValid == 0 {
			reason := "has no valid toolsets configured"
			if len(list) == 0 {
				reason = "is configured with an empty toolset list"
			}
			warnings = append(warnings, fmt.Sprintf("platform '%s' %s — %s", platform, reason, noToolsSuffix))
		}
	}
	if valid == 0 {
		warnings = append(warnings, "platform_toolsets resolves to zero valid toolsets — the agent will have no tools. Run `icebath tools` to reconfigure.")
	}
	return warnings
}

// FilterToEnabledToolsets keeps the unavailable entries whose name or tools fall inside the session toolset selection
func FilterToEnabledToolsets(unavailable []UnavailableToolset, enabled []string, resolve func(string) ([]string, error)) []UnavailableToolset {
	var names []string
	for _, n := range enabled {
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return append([]UnavailableToolset{}, unavailable...)
	}
	enabledTools := map[string]bool{}
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
		members, err := resolveNoticeToolset(resolve, n)
		if err != nil {
			continue
		}
		for _, t := range members {
			enabledTools[t] = true
		}
	}
	out := []UnavailableToolset{}
	for _, item := range unavailable {
		keep := nameSet[item.Name]
		for _, t := range item.Tools {
			if enabledTools[t] {
				keep = true
			}
		}
		if keep {
			out = append(out, item)
		}
	}
	return out
}

// Resolver call where a panic counts as an error so one failing bundle never aborts the filter
func resolveNoticeToolset(resolve func(string) ([]string, error), name string) (members []string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			members, err = nil, fmt.Errorf("%v", rec)
		}
	}()
	return resolve(name)
}

// CurrentTerminalBackend names the terminal backend this process selected, local when unset
func CurrentTerminalBackend(env *Env) (string, error) {
	cfg, err := getEnvConfig(env)
	if err != nil {
		return "", err
	}
	if v := cfg["env_type"]; pyTruthy(v) {
		return pyrepr.Str(v), nil
	}
	return "local", nil
}

var multiProviderNotices = map[string]string{
	"web": "[yellow]⚠ Web search is off[/] — no search provider is set up yet (any one of Nous subscription, Exa, " +
		"Tavily, Firecrawl, Brave, or free DuckDuckGo works). Run [bold]icebath setup tools[/] and set one up under " +
		"\"Web Search & Scraping\".",
}

const genericNoticeFooter = "[dim]   Run 'icebath setup tools' to configure[/]"

func terminalNoticeLine(backend, reason string) string {
	detail := ""
	if reason != "" {
		detail = " (" + reason + ")"
	}
	return fmt.Sprintf("[yellow]⚠ Terminal tool disabled:[/] the '%s' backend is not usable%s. ", backend, detail) +
		"Run [bold]icebath doctor[/] for details, or [bold]icebath setup terminal[/] to pick another backend."
}

// ToolAvailabilityWarningLines renders the Rich markup startup notices for toolsets switched off by missing requirements
func ToolAvailabilityWarningLines(unavailable []UnavailableToolset, terminalReason, terminalBackend string) []string {
	lines := []string{}
	var generic []UnavailableToolset
	for _, item := range unavailable {
		switch {
		case item.Name == "terminal":
			lines = append(lines, terminalNoticeLine(terminalBackend, terminalReason))
		case multiProviderNotices[item.Name] != "":
			lines = append(lines, multiProviderNotices[item.Name])
		case len(item.EnvVars) > 0:
			generic = append(generic, item)
		}
	}
	if len(generic) > 0 {
		lines = append(lines, "[yellow]⚠️  Some tools disabled (missing API keys):[/]")
		for _, item := range generic {
			lines = append(lines, fmt.Sprintf("   [dim]• %s[/] [dim italic](%s)[/]", item.Name, strings.Join(item.EnvVars, ", ")))
		}
		lines = append(lines, genericNoticeFooter)
	}
	return lines
}
