package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Prompt configuration the agent resolves at init and the per session prompt bookkeeping the builders write
type systemPromptState struct {
	platformHintOverrides      map[string]any
	taskCompletionGuidance     bool
	parallelToolCallGuidance   bool
	toolUseEnforcement         any
	executionGuidance          any
	environmentProbe           bool
	botModeProtocol            bool
	kanbanWorkerGuidance       *string
	contextCwdIsLaunchArtifact bool
	sessionTitleHint           string
	cachedSystemPrompt         string
	cachedSystemPromptStatic   string
	staticRebuildFailedFor     string
	pluginSectionsSnapshot     []plugins.RenderedSystemPromptSection
	pluginSectionsFrozen       bool
	pluginSectionsPrevious     []plugins.RenderedSystemPromptSection
	autoLoadSkillsResolved     bool
	autoLoadSkillsResult       autoLoadSkillsResult
	botChatTimelessPrompt      bool
	frozenWorkspaceSnapshot    *workspacePin
}

// The skills.auto_load prompt with the loaded and missing skill names
type autoLoadSkillsResult struct {
	prompt          string
	loaded, missing []string
}

// The workspace snapshot block pinned for the cwd it was probed in
type workspacePin struct{ key, block string }

// The three ordered cache tiers of the system prompt
type SystemPromptParts struct {
	Stable, Context, Volatile string
}

// PlatformRegistryHint returns a registered platform's prompt hint and whether the registry has the entry, set by the gateway
var PlatformRegistryHint func(ctx context.Context, platform string) (string, bool)

// Seams the prompt builders call so tests can replace the file and host probes
var (
	loadSoulMDFn              = LoadSoulMD
	buildEnvironmentHintsFn   = BuildEnvironmentHints
	buildContextFilesPromptFn = BuildContextFilesPrompt
	hermesNow                 = config.TimeNow
	hermesTimezone            = config.TimeZone
	safeStrftime              = config.SafeStrftime
)

var (
	pluginSectionFrameRe = regexp.MustCompile(`(?m)^## Plugin Context: ([a-z0-9][a-z0-9._-]{0,127})\n<!-- icebath-plugin-section-chars:([0-9]{1,4}) -->\n\n`)
	sessionStampRe       = regexp.MustCompile(`^(\p{Nd}{8})_(\p{Nd}{6})`)
	gateWords            = map[string]bool{"true": true, "always": true, "yes": true, "on": true, "false": false, "never": false, "no": false, "off": false}
	skillToolNames       = []string{"skills_list", "skill_view", "skill_manage"}
	truthyStrings        = map[string]bool{"1": true, "true": true, "yes": true, "on": true}
)

const tuiEmbeddedPaneClarifierText = " You're in its embedded terminal pane, beside the GUI chat — the user can " +
	"select your output (Option-drag on macOS, Shift-drag elsewhere) and press " +
	"Cmd/Ctrl+L to send it to the chat composer."

// Reports whether an environment variable holds one of the shared truthy strings
func envTruthy(name string) bool {
	v, ok := os.LookupEnv(name)
	return ok && truthyStrings[pystr.Lower(pystr.Strip(v))]
}

// Resolves a config gate from booleans, gate words, a model substring list, or the default model list
func modelGate(setting any, model string, defaultModels []string) bool {
	switch v := setting.(type) {
	case bool:
		return v
	case string:
		if on, ok := gateWords[pystr.Lower(v)]; ok {
			return on
		}
	case []any:
		ml := pystr.Lower(model)
		for _, p := range v {
			if s, ok := p.(string); ok && strings.Contains(ml, pystr.Lower(s)) {
				return true
			}
		}
		return false
	case []string:
		ml := pystr.Lower(model)
		for _, s := range v {
			if strings.Contains(ml, pystr.Lower(s)) {
				return true
			}
		}
		return false
	}
	ml := pystr.Lower(model)
	for _, p := range defaultModels {
		if strings.Contains(ml, p) {
			return true
		}
	}
	return false
}

// Applies the platform_hints replace or append override, a malformed entry keeps the default hint
func resolvePlatformHint(a *AIAgent, platformKey, defaultHint string) string {
	var spec any
	if platformKey != "" && a.platformHintOverrides != nil {
		spec = a.platformHintOverrides[platformKey]
	}
	if s, ok := spec.(string); ok {
		spec = map[string]any{"append": s}
	}
	m, ok := pyDict(spec)
	if !ok {
		return defaultHint
	}
	text := func(v any) string {
		if s, ok := v.(string); ok {
			return pystr.Strip(s)
		}
		return ""
	}
	replaceText, appendText := text(m["replace"]), text(m["append"])
	base := replaceText
	if base == "" {
		base = defaultHint
	}
	if appendText != "" {
		return pystr.Strip(base + "\n\n" + appendText)
	}
	return base
}

// Returns a mapping value as a string keyed dict, false for anything that is not a dict
func pyDict(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case config.Tree:
		return map[string]any(m), true
	}
	return nil, false
}

// Appends the desktop embedded terminal clarifier once when the desktop terminal flag is set
func tuiEmbeddedPaneClarifier(hint string) string {
	if hint == "" || strings.Contains(hint, tuiEmbeddedPaneClarifierText) || !envTruthy("ICEBATH_DESKTOP_TERMINAL") {
		return hint
	}
	return hint + tuiEmbeddedPaneClarifierText
}

// Session metadata exposed to plugin prompt sections at render time
func pluginSessionInfo(ctx context.Context, a *AIAgent) map[string]any {
	cwd, err := tools.ResolveContextCwd(a.env, true)
	if err != nil {
		cwd = ""
	}
	return map[string]any{
		"session_id":   a.sessionID,
		"model":        a.model,
		"provider":     a.provider,
		"platform":     a.platform,
		"profile_name": activeProfileName(ctx, a, ambientPluginProfileName),
		"cwd":          cwd,
	}
}

// Active profile name from the profiles module, default when none resolves
func ambientPluginProfileName(ctx context.Context) string {
	if name := profiles.GetActiveProfileName(ctx); name != "" {
		return name
	}
	return "default"
}

// Profile name from the agent's own home, else the ambient resolver
func activeProfileName(ctx context.Context, a *AIAgent, ambient func(context.Context) string) string {
	if h := agentOwnHome(ctx, a); h != "" {
		return profileNameForHome(h)
	}
	return ambient(ctx)
}

// Plugin sections rendered once per session and frozen, restored from a cached prompt when one exists
func frozenPluginPromptSections(ctx context.Context, a *AIAgent) []plugins.RenderedSystemPromptSection {
	if a.pluginSectionsFrozen {
		return a.pluginSectionsSnapshot
	}
	var rendered []plugins.RenderedSystemPromptSection
	if a.cachedSystemPrompt != "" {
		rendered = restoredPluginPromptSections(a.cachedSystemPrompt)
	} else {
		rendered = renderPluginPromptSections(ctx, a)
	}
	a.pluginSectionsSnapshot, a.pluginSectionsFrozen = rendered, true
	return rendered
}

// Renders plugin sections, a failing render keeps the previous frozen sections when there are any
func renderPluginPromptSections(ctx context.Context, a *AIAgent) (rendered []plugins.RenderedSystemPromptSection) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		switch r.(type) {
		case plugins.UserInterrupt, *plugins.UserInterrupt:
			panic(r)
		}
		if len(a.pluginSectionsPrevious) > 0 {
			slog.Warn(fmt.Sprintf("Plugin system prompt sections failed to re-render (%v); keeping the previous frozen sections", r))
			rendered = a.pluginSectionsPrevious
			return
		}
		slog.Warn(fmt.Sprintf("Plugin system prompt sections could not be rendered: %v", r))
		rendered = nil
	}()
	return plugins.RenderSystemPromptSections(ctx, pluginSessionInfo(ctx, a))
}

// Recovers frozen section bytes from the exact canonical container that precedes the conversation start line
func restoredPluginPromptSections(prompt string) []plugins.RenderedSystemPromptSection {
	start := strings.LastIndex(prompt, plugins.PluginSectionsStart)
	if start < 0 {
		return nil
	}
	rel := strings.Index(prompt[start+len(plugins.PluginSectionsStart):], plugins.PluginSectionsEnd)
	if rel < 0 {
		return nil
	}
	afterEnd := start + len(plugins.PluginSectionsStart) + rel + len(plugins.PluginSectionsEnd)
	if !strings.HasPrefix(prompt[afterEnd:], "\n\nConversation started:") {
		return nil
	}
	framed := prompt[start:afterEnd]
	var restored []plugins.RenderedSystemPromptSection
	for _, m := range pluginSectionFrameRe.FindAllStringSubmatchIndex(framed, -1) {
		contentLen, _ := strconv.Atoi(framed[m[4]:m[5]])
		content := firstRunes(framed[m[1]:], contentLen)
		if contentLen > plugins.MaxSystemPromptSectionChars || utf8.RuneCountInString(content) != contentLen {
			continue
		}
		restored = append(restored, plugins.RenderedSystemPromptSection{ID: framed[m[2]:m[3]], Content: content, Position: "after_memory", Plugin: "persisted-prompt"})
	}
	if plugins.FormatSystemPromptSections(restored) != framed {
		return nil
	}
	return restored
}

// Returns the first n code points of s, or all of s when shorter
func firstRunes(s string, n int) string {
	i := 0
	for count := 0; count < n && i < len(s); count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

// RestorePluginPromptSections seeds a resumed agent's frozen plugin sections from persisted prompt bytes
func RestorePluginPromptSections(a *AIAgent, prompt string) {
	a.pluginSectionsSnapshot, a.pluginSectionsFrozen = restoredPluginPromptSections(prompt), true
}

// Formats the sections at one position as a single framed block, nothing when none match
func pluginSectionBlocks(sections []plugins.RenderedSystemPromptSection, position string) []string {
	var picked []plugins.RenderedSystemPromptSection
	for _, s := range sections {
		if s.Position == position {
			picked = append(picked, s)
		}
	}
	if block := plugins.FormatSystemPromptSections(picked); block != "" {
		return []string{block}
	}
	return nil
}

// Byte stable conversation start from the lineage root or session id stamp, the session start, else now
func sessionStartLike(a *AIAgent, now time.Time) time.Time {
	root := ""
	if a.sessionDB != nil && a.sessionID != "" {
		if r, err := a.sessionDB.GetConversationRoot(a.sessionID); err == nil {
			root = r
		}
	}
	for _, candidate := range []string{root, a.sessionID} {
		m := sessionStampRe.FindStringSubmatch(candidate)
		if m == nil {
			continue
		}
		if t, _, err := pydatetime.Strptime(m[1]+"_"+m[2], "%Y%m%d_%H%M%S"); err == nil {
			return stampDisplayTime(t, now)
		}
	}
	if !a.sessionStart.IsZero() {
		return a.sessionStart.In(now.Location())
	}
	return now
}

// Attaches the local offset in force at a naive stamp then converts to now's zone, naive when localtime refuses
func stampDisplayTime(naive, now time.Time) time.Time {
	secs, ok := pydatetime.LocalToSeconds(naive, time.Local, 0, pydatetime.LocaltimeAccepts)
	if !ok {
		return naive
	}
	return time.Unix(secs, 0).In(now.Location())
}

// The agent's own profile home from a bound home override or its session database, empty for ambient
func agentOwnHome(ctx context.Context, a *AIAgent) string {
	if o := home.Override(ctx); o != "" {
		return o
	}
	if a.sessionDB != nil {
		if p := a.sessionDB.Path(); p != "" {
			return filepath.Dir(p)
		}
	}
	return ""
}

// The skills dir inside the agent's own home, empty for ambient
func agentSkillsDir(ctx context.Context, a *AIAgent) string {
	if h := agentOwnHome(ctx, a); h != "" {
		return filepath.Join(h, "skills")
	}
	return ""
}

// Profile name X for a home at root profiles X, default for anything else
func profileNameForHome(h string) string {
	resolved, err := pypath.Realpath(h, pypath.Lax)
	if err != nil {
		return "default"
	}
	profilesDir, err := pypath.Realpath(filepath.Join(home.Root(), "profiles"), pypath.Lax)
	if err != nil {
		return "default"
	}
	rel, err := filepath.Rel(profilesDir, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "default"
	}
	if rel == "." {
		return "default"
	}
	return strings.Split(rel, string(filepath.Separator))[0]
}

// Tool aware memory, session search, skills and kanban guidance joined into one block
func toolGuidanceBlock(ctx context.Context, a *AIAgent) string {
	names := a.validToolNames
	memoryGuidance := ""
	if names["memory"] {
		memoryGuidance = memory.BuildMemoryGuidance(a.memoryEnabled, a.userProfileEnabled, names["skill_manage"])
	}
	kanban := ""
	if a.kanbanWorkerGuidance != nil {
		kanban = *a.kanbanWorkerGuidance
	} else if names["kanban_show"] && tools.OwnedKanbanTask(tools.WithToolEnv(ctx, a.env)) != "" {
		kanban = KanbanGuidance
	}
	guidance := []string{memoryGuidance}
	if names["session_search"] {
		guidance = append(guidance, SessionSearchGuidance)
	}
	if names["skill_manage"] {
		guidance = append(guidance, SkillsGuidance)
	}
	guidance = append(guidance, kanban)
	var kept []string
	for _, g := range guidance {
		if g != "" {
			kept = append(kept, g)
		}
	}
	return strings.Join(kept, " ")
}

// Reports whether any skills tool is loaded on the agent
func hasSkillTools(a *AIAgent) bool {
	for _, name := range skillToolNames {
		if a.validToolNames[name] {
			return true
		}
	}
	return false
}

// Skills index for the loaded toolsets, focus mode demotes non coding categories to names only
func skillsPrompt(ctx context.Context, a *AIAgent) (string, error) {
	if !hasSkillTools(a) {
		return "", nil
	}
	scope := tools.ScopeKey(home.Dir(ctx))
	toolsets := map[string]bool{}
	for name := range a.validToolNames {
		if ts := tools.Default.ToolsetForTool(ctx, scope, name); ts != "" {
			toolsets[ts] = true
		}
	}
	var compact map[string]bool
	if cwd, err := tools.ResolveContextCwd(a.env, true); err == nil {
		compact = tools.CodingCompactSkillCategories(a.env, a.platform, cwd)
	}
	if len(compact) == 0 {
		compact = nil
	}
	return skills.ScopeFromEnv(a.env).BuildSkillsSystemPrompt(ctx, skills.SkillsPromptOptions{
		AvailableTools: a.validToolNames, AvailableToolsets: toolsets, CompactCategories: compact, SkillsDirOverride: agentSkillsDir(ctx, a),
	})
}

// A skills scope resolving every home read at the agent's own home when it has one
func agentSkillsScope(ctx context.Context, a *AIAgent) (context.Context, *skills.Scope) {
	h := agentOwnHome(ctx, a)
	if h == "" {
		return ctx, skills.ScopeFromEnv(a.env)
	}
	env := tools.Env{Home: h}
	if a.env != nil {
		env = *a.env
		env.Home, env.Config, env.Secrets = h, nil, nil
	}
	return home.WithOverride(ctx, h), skills.ScopeFromEnv(&env)
}

// skills.auto_load blocks resolved once per agent lifecycle so the stable prefix never changes
func autoLoadParts(ctx context.Context, a *AIAgent) []string {
	if a.skipContextFiles || !hasSkillTools(a) {
		return nil
	}
	if !a.autoLoadSkillsResolved {
		var res autoLoadSkillsResult
		if !envTruthy("ICEBATH_IGNORE_RULES") {
			sctx, scope := agentSkillsScope(ctx, a)
			res.prompt, res.loaded, res.missing = scope.BuildAutoLoadPrompt(sctx, a.sessionID, nil)
		}
		if len(res.missing) > 0 {
			slog.Warn("skills.auto_load: skill(s) not found or disabled, skipped: " + strings.Join(res.missing, ", "))
		}
		a.autoLoadSkillsResult, a.autoLoadSkillsResolved = res, true
	}
	if p := a.autoLoadSkillsResult.prompt; p != "" {
		return []string{p}
	}
	return nil
}

// Bot Mode teammate protocol and capability epoch, only in a bot's Bot Chat session, marking the prompt timeless
func botModeParts(ctx context.Context, a *AIAgent) []string {
	title := pystr.Strip(a.sessionTitleHint)
	if title == "" && a.sessionDB != nil && a.sessionID != "" {
		t, _, err := a.sessionDB.Title(a.sessionID)
		if err != nil {
			return nil
		}
		title = pystr.Strip(t)
	}
	if title != botmode.BotChatTitle {
		return nil
	}
	h := agentOwnHome(ctx, a)
	section := botmode.ProtocolSection(h, false)
	if section == "" {
		return nil
	}
	parts := []string{section, botmode.EpochLine(h)}
	a.botChatTimelessPrompt = true
	return parts
}

// Profile X when the ambient home resolves under the resolved root's profiles dir, default otherwise
func ambientFileSafetyProfileName(ctx context.Context) string {
	resolved, err := pypath.Realpath(home.Dir(ctx), pypath.Lax)
	if err != nil {
		return "default"
	}
	root, err := pypath.Realpath(home.Root(), pypath.Lax)
	if err != nil {
		return "default"
	}
	rel, err := filepath.Rel(filepath.Join(root, "profiles"), resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "default"
	}
	return strings.Split(rel, string(filepath.Separator))[0]
}

// Names the running profile and where the default profile's data lives
func activeProfileLine(ctx context.Context, a *AIAgent) string {
	ownHome := agentOwnHome(ctx, a)
	active := activeProfileName(ctx, a, ambientFileSafetyProfileName)
	if active == "default" {
		root := home.Dir(ctx)
		if ownHome != "" {
			root = home.Root()
		}
		return "Active Icebath profile: default. Other profiles (if any) live " +
			"under " + root + "/profiles/<name>/. Each profile has its own " +
			"skills/, plugins/, cron/, and memories/ that affect a different " +
			"session than this one. Do not modify another profile's " +
			"skills/plugins/cron/memories unless the user explicitly directs " +
			"you to."
	}
	profileHome := ownHome
	if profileHome == "" {
		profileHome = home.Dir(ctx)
	}
	root := home.Root()
	return fmt.Sprintf("Active Icebath profile: %s. This session reads "+
		"and writes %s/. The default "+
		"profile's data lives at %s/skills/, %s/plugins/, "+
		"%s/cron/, %s/memories/ — those belong to a "+
		"different session run from a different shell. Do NOT modify "+
		"another profile's skills/plugins/cron/memories unless the user "+
		"explicitly directs you to.", active, profileHome, root, root, root, root)
}

// The registered platform hint and whether an entry exists, no entry while no registry is installed
func platformRegistryEntry(ctx context.Context, platformKey string) (string, bool) {
	if PlatformRegistryHint == nil {
		return "", false
	}
	return PlatformRegistryHint(ctx, platformKey)
}

// Built in hint, else the platform registry hint after plugin discovery, plus the Telegram rich messages opt in
func defaultPlatformHint(ctx context.Context, platformKey string) string {
	hint := PlatformHints[platformKey]
	if hint == "" && platformKey != "" {
		h, found := platformRegistryEntry(ctx, platformKey)
		if !found {
			plugins.DiscoverPlugins(ctx, false)
			h, found = platformRegistryEntry(ctx, platformKey)
		}
		if found {
			hint = h
		}
	}
	if platformKey == "telegram" && hint != "" && telegramRichMessagesEnabled(ctx) {
		hint = pystr.Rstrip(hint) + " " + TelegramRichMessagesHint
	}
	return hint
}

// The cron delivery channel's hint with its override, empty without an auto deliver platform
func cronDeliveryHint(ctx context.Context, a *AIAgent) string {
	key := pystr.Strip(pystr.Lower(tools.SessionEnvValue(a.env, "ICEBATH_CRON_AUTO_DELIVER_PLATFORM")))
	if key == "" || key == "cron" {
		return ""
	}
	hint := resolvePlatformHint(a, key, defaultPlatformHint(ctx, key))
	if hint == "" {
		return ""
	}
	return fmt.Sprintf("Delivery destination (%s): %s", key, hint)
}

// PlatformHint is the platform hint with its override, the desktop TUI clarifier and a cron agent's delivery hint
func PlatformHint(ctx context.Context, a *AIAgent) string {
	key := pystr.Strip(pystr.Lower(a.platform))
	hint := resolvePlatformHint(a, key, defaultPlatformHint(ctx, key))
	if key == "tui" && hint != "" {
		hint = tuiEmbeddedPaneClarifier(hint)
	}
	if key == "cron" {
		if delivery := cronDeliveryHint(ctx, a); delivery != "" {
			hint = pystr.Strip(hint + "\n\n" + delivery)
		}
	}
	return hint
}

// Python (x or {}).get(key), false when a truthy non dict would raise
func cfgGet(v any, key string) (any, bool) {
	if !config.PyTruthy(v) {
		return nil, true
	}
	m, ok := pyDict(v)
	if !ok {
		return nil, false
	}
	return m[key], true
}

// Reads a nested config key chain the way chained (x or {}).get calls do
func cfgDig(v any, keys ...string) (any, bool) {
	for _, k := range keys {
		var ok bool
		if v, ok = cfgGet(v, k); !ok {
			return nil, false
		}
	}
	return v, true
}

// Telegram rich_messages with platforms.telegram.extra overriding gateway.platforms.telegram.extra, false on any read failure
func telegramRichMessagesEnabled(ctx context.Context) bool {
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return false
	}
	gw, ok := cfgDig(map[string]any(cfg), "gateway", "platforms", "telegram", "extra")
	if !ok {
		return false
	}
	top, ok := cfgDig(map[string]any(cfg), "platforms", "telegram", "extra")
	if !ok {
		return false
	}
	merged := map[string]any{}
	for _, src := range []any{gw, top} {
		if m, ok := pyDict(src); ok {
			for k, v := range m {
				merged[k] = v
			}
		}
	}
	return config.PyTruthy(merged["rich_messages"])
}

// IANA key, a differing abbreviation and the UTC offset of now
func zoneBits(now time.Time, tz *time.Location, haveTZ bool) []string {
	iana := ""
	if haveTZ && tz != nil {
		iana = tz.String()
	}
	abbrev, offset := safeStrftime(now, "%Z"), safeStrftime(now, "%z")
	var bits []string
	if iana != "" {
		bits = append(bits, iana)
	}
	if abbrev != "" && abbrev != iana {
		bits = append(bits, abbrev)
	}
	if offset != "" {
		head, tail := offset, ""
		if len(offset) > 3 {
			head, tail = offset[:3], offset[3:]
		}
		bits = append(bits, "UTC"+head+":"+tail)
	}
	return bits
}

// Date only conversation start line with zone, an as of line on later rebuild days, and the session trailer
func timestampLine(ctx context.Context, a *AIAgent) string {
	now := hermesNow(ctx)
	tz, haveTZ := hermesTimezone(ctx)
	bits := zoneBits(now, tz, haveTZ)
	zoneSuffix := ""
	if len(bits) > 0 {
		zoneSuffix = " (" + strings.Join(bits, ", ") + ")"
	}
	start := sessionStartLike(a, now)
	line := "Conversation started: " + safeStrftime(start, "%A, %B %d, %Y") + zoneSuffix
	if now.Format("20060102") != start.Format("20060102") {
		line += "\nToday's date (as of the last context rebuild): " + safeStrftime(now, "%A, %B %d, %Y") +
			" — trust this over the start date for what day it is now; query tools for exact time."
	}
	if a.botChatTimelessPrompt {
		line = ""
		if len(bits) > 0 {
			line = "Timezone: " + strings.Join(bits, ", ")
		}
	}
	sessionID := ""
	if a.passSessionID {
		sessionID = a.sessionID
	}
	trailer := [][2]string{{"Session ID", sessionID}, {"Model", a.model}, {"Provider", a.provider}, {"Platform", a.platform}}
	var b strings.Builder
	b.WriteString(line)
	for _, kv := range trailer {
		if kv[1] != "" {
			b.WriteString("\n" + kv[0] + ": " + kv[1])
		}
	}
	return b.String()
}

// The function form tool maps of the agent's tool surface
func toolSurface(list []any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		if m, ok := t.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Built in memory and user profile blocks plus the external provider block when its tools are exposed
func memoryParts(ctx context.Context, a *AIAgent) []string {
	var parts []string
	if a.memoryStore != nil {
		for _, kind := range []struct {
			enabled bool
			name    string
		}{{a.memoryEnabled, "memory"}, {a.userProfileEnabled, "user"}} {
			if !kind.enabled {
				continue
			}
			if block := a.memoryStore.FormatForSystemPrompt(kind.name); block != "" {
				parts = append(parts, block)
			}
		}
	}
	if a.memoryManager != nil && memory.MemoryProviderToolsExposed(ctx, a.env, toolSurface(a.tools)) {
		if block := a.memoryManager.BuildSystemPrompt(ctx); block != "" {
			parts = append(parts, block)
		}
	}
	return parts
}

// SOUL.md from the agent's own home as the identity, else the default identity, with whether SOUL loaded
func identityParts(ctx context.Context, a *AIAgent, ctxLen *int) ([]string, bool) {
	if a.loadSoulIdentity || !a.skipContextFiles {
		if soul, ok := loadSoulMDFn(ctx, a.env, ctxLen, agentOwnHome(ctx, a)); ok && soul != "" {
			return []string{soul}, true
		}
	}
	return []string{config.DefaultIdentity}, false
}

// Universal, tool aware and model gated guidance blocks, each behind its config gate
func guidanceParts(ctx context.Context, a *AIAgent) []string {
	var parts []string
	hasTools := len(a.validToolNames) > 0
	if hasTools {
		if a.taskCompletionGuidance {
			parts = append(parts, TaskCompletionGuidance)
		}
		if a.parallelToolCallGuidance {
			parts = append(parts, ParallelToolCallGuidance)
		}
	}
	parts = append(parts, toolGuidanceBlock(ctx, a))
	if !hasTools {
		return parts
	}
	if !a.agentIsOneshot() {
		parts = append(parts, SteerChannelNote)
	}
	if modelGate(a.toolUseEnforcement, a.model, ToolUseEnforcementModels) {
		parts = append(parts, ToolUseEnforcementGuidance)
		ml := pystr.Lower(a.model)
		if strings.Contains(ml, "gemini") || strings.Contains(ml, "gemma") {
			parts = append(parts, GoogleModelOperationalGuidance)
		}
	}
	if modelGate(a.executionGuidance, a.model, ExecutionGuidanceModels) {
		parts = append(parts, ExecutionGuidanceText(a.validToolNames))
	}
	return parts
}

// The real model identity for the Alibaba Coding Plan, which always reports glm-4.7
func alibabaIdentityPart(a *AIAgent) []string {
	if a.provider != "alibaba" {
		return nil
	}
	short := a.model[strings.LastIndex(a.model, "/")+1:]
	return []string{"You are powered by the model named " + short + ". " +
		"The exact model ID is " + a.model + ". " +
		"When asked what model you are, always answer based on this information, " +
		"not on any model name returned by the API."}
}

// The directory the workspace probe inspects, empty when the cwd was deleted
func workspacePinKey(a *AIAgent) string {
	cwd, err := tools.ResolveContextCwd(a.env, true)
	if err != nil {
		return ""
	}
	if cwd == "" {
		if cwd, err = tools.ResolveAgentCwd(a.env); err != nil {
			return ""
		}
	}
	return cwd
}

// Reports whether parent is a strict ancestor of p
func pathHasParent(p, parent string) bool {
	for cur := filepath.Dir(p); ; cur = filepath.Dir(cur) {
		if cur == parent {
			return true
		}
		if next := filepath.Dir(cur); next == cur {
			return false
		}
	}
}

// The workspace snapshot for key inside prompt, empty and true without one, false when it belongs to another root
func persistedWorkspaceBlock(prompt, key string) (string, bool) {
	head := "\n\n" + tools.WorkspaceBlockHeader + "\n- Root: "
	start := strings.Index(prompt, head)
	if start < 0 {
		return "", true
	}
	cwd, _ := pypath.Realpath(key, pypath.Lax)
	for start >= 0 {
		block, _, _ := strings.Cut(prompt[start+2:], "\n\n")
		lines := strings.SplitN(block, "\n", 3)
		root, _ := pypath.Realpath(lines[1][len("- Root: "):], pypath.Lax)
		if root == cwd || pathHasParent(cwd, root) {
			return block, true
		}
		next := strings.Index(prompt[start+2:], head)
		if next < 0 {
			break
		}
		start += 2 + next
	}
	return "", false
}

// Prompt bytes the session already sends, the cached copy else its persisted row
func sessionPrompt(a *AIAgent) string {
	if a.cachedSystemPrompt != "" {
		return a.cachedSystemPrompt
	}
	if a.sessionDB == nil || a.sessionID == "" {
		return ""
	}
	row, err := a.sessionDB.GetSession(context.Background(), a.sessionID)
	if err != nil {
		slog.Debug(fmt.Sprintf("workspace snapshot: session row read failed (session=%s)", a.sessionID), "err", err)
		return ""
	}
	if row == nil {
		return ""
	}
	prompt, _ := row.Get("system_prompt")
	text, _ := prompt.(string)
	return text
}

// Pins the workspace snapshot the session's existing prompt carries when it was taken in this cwd
func seedWorkspacePin(a *AIAgent, key string) {
	prompt := sessionPrompt(a)
	if prompt == "" {
		return
	}
	if stored := runtimeHostValue(prompt, "Current working directory"); stored != "" && stored != key {
		return
	}
	if block, ok := persistedWorkspaceBlock(prompt, key); ok && block != "" {
		a.frozenWorkspaceSnapshot = &workspacePin{key: key, block: block}
	}
}

// Coding posture prefix, workspace and trailing blocks with the workspace bytes pinned per session and cwd
func codingParts(a *AIAgent) ([]string, []string, []string) {
	if len(a.validToolNames) == 0 {
		return nil, nil, nil
	}
	cwd, err := tools.ResolveContextCwd(a.env, true)
	if err != nil {
		return nil, nil, nil
	}
	key := workspacePinKey(a)
	if a.frozenWorkspaceSnapshot == nil {
		seedWorkspacePin(a, key)
	}
	var replay *string
	if pin := a.frozenWorkspaceSnapshot; pin != nil && pin.key == key {
		block := pin.block
		replay = &block
	}
	prefix, workspace, trailing := tools.CodingSystemPromptParts(a.env, a.platform, cwd, a.model, a.validToolNames, replay)
	if replay == nil {
		block := ""
		if len(workspace) > 0 {
			block = workspace[0]
		}
		a.frozenWorkspaceSnapshot = &workspacePin{key: key, block: block}
	}
	return prefix, workspace, trailing
}

// Environment probe line, bot mode protocol and platform hint that follow the workspace context
func postWorkspaceParts(ctx context.Context, a *AIAgent) []string {
	var parts []string
	if a.environmentProbe {
		parts = append(parts, tools.EnvironmentProbeLine(a.env, false))
	}
	if a.botModeProtocol {
		parts = append(parts, botModeParts(ctx, a)...)
	}
	return append(parts, PlatformHint(ctx, a))
}

// Project context files for the context tier, install tree fallback only for cli and tui
func contextFilesPart(ctx context.Context, a *AIAgent, ctxLen *int, soulLoaded bool) ([]string, error) {
	if a.skipContextFiles {
		return nil, nil
	}
	cwd, err := tools.ResolveContextCwd(a.env, !a.contextCwdIsLaunchArtifact)
	if err != nil {
		return nil, err
	}
	o := ContextFilesOptions{SkipSoul: soulLoaded, ContextLength: ctxLen,
		AllowInstallTreeFallback: a.platform == "cli" || a.platform == "tui", HomeOverride: agentOwnHome(ctx, a)}
	if cwd != "" {
		o.Cwd = &cwd
	}
	p, err := buildContextFilesPromptFn(ctx, a.env, o)
	if err != nil {
		return nil, err
	}
	return []string{p}, nil
}

// Joins the stripped non blank parts with blank lines
func joinTier(parts []string) string {
	var kept []string
	for _, p := range parts {
		if s := pystr.Strip(p); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "\n\n")
}

// BuildSystemPromptParts assembles the stable, context and volatile cache tiers of the system prompt
func BuildSystemPromptParts(ctx context.Context, a *AIAgent, systemMessage *string) (SystemPromptParts, error) {
	var ctxLen *int
	if a.contextCompressor != nil {
		if n := a.contextCompressor.Base().ContextLength; n > 0 {
			ctxLen = &n
		}
	}
	stable, soulLoaded := identityParts(ctx, a, ctxLen)
	helpSlot := len(stable)
	stable = append(stable, IcebathAgentHelpGuidanceNoSkills)
	stable = append(stable, guidanceParts(ctx, a)...)
	skillsText, err := skillsPrompt(ctx, a)
	if err != nil {
		return SystemPromptParts{}, err
	}
	if a.validToolNames["skill_view"] && strings.Contains(skillsText, "- icebath-agent:") {
		stable[helpSlot] = IcebathAgentHelpGuidance
	}
	stable = append(stable, alibabaIdentityPart(a)...)
	stable = append(stable, autoLoadParts(ctx, a)...)
	environmentHints, err := buildEnvironmentHintsFn(ctx, a.env)
	if err != nil {
		return SystemPromptParts{}, err
	}
	codingPrefix, codingWorkspace, codingTrailing := codingParts(a)
	stable = append(stable, codingPrefix...)
	postWorkspace := postWorkspaceParts(ctx, a)
	var contextParts []string
	if systemMessage != nil {
		contextParts = append(contextParts, *systemMessage)
	}
	files, err := contextFilesPart(ctx, a, ctxLen, soulLoaded)
	if err != nil {
		return SystemPromptParts{}, err
	}
	contextParts = append(contextParts, files...)
	if len(codingWorkspace) > 0 {
		contextParts = append(append(append(contextParts, codingWorkspace...), codingTrailing...), postWorkspace...)
	} else {
		stable = append(append(stable, codingTrailing...), postWorkspace...)
	}
	volatile := append([]string{skillsText}, memoryParts(ctx, a)...)
	volatile = append(volatile, pluginSectionBlocks(frozenPluginPromptSections(ctx, a), "after_memory")...)
	volatile = append(volatile, activeProfileLine(ctx, a), timestampLine(ctx, a))
	if environmentHints != "" {
		environmentHints = strings.ReplaceAll(environmentHints, RuntimeEnvironmentHeading, "> "+RuntimeEnvironmentHeading)
		volatile = append(volatile, RuntimeEnvironmentHeading+"\n\n"+environmentHints+"\n\n"+RuntimeEnvironmentEnd)
	}
	return SystemPromptParts{Stable: joinTier(stable), Context: joinTier(contextParts), Volatile: joinTier(volatile)}, nil
}

// BuildSystemPrompt joins the tiers, records the stable prefix and surfaces context truncation warnings
func BuildSystemPrompt(ctx context.Context, a *AIAgent, systemMessage *string) (string, error) {
	parts, err := BuildSystemPromptParts(ctx, a, systemMessage)
	if err != nil {
		return "", err
	}
	a.cachedSystemPromptStatic = parts.Stable
	for _, warning := range tools.DrainTruncationWarnings(ctx) {
		a.emitDiagnosticStatus(warning)
	}
	var kept []string
	for _, p := range []string{parts.Stable, parts.Context, parts.Volatile} {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n"), nil
}

// InvalidateSystemPrompt forces a rebuild, stashes the frozen plugin sections and reloads memory from disk
func InvalidateSystemPrompt(a *AIAgent) error {
	a.cachedSystemPrompt, a.cachedSystemPromptStatic = "", ""
	if a.pluginSectionsFrozen {
		a.pluginSectionsPrevious = a.pluginSectionsSnapshot
		a.pluginSectionsSnapshot, a.pluginSectionsFrozen = nil, false
	}
	if a.memoryStore != nil {
		return a.memoryStore.LoadFromDisk()
	}
	return nil
}

// ReconstructStaticPrefix rebuilds the stable tier for a stored prompt, kept only when the prompt starts with it
func ReconstructStaticPrefix(ctx context.Context, a *AIAgent, systemMessage *string, logLabel string) {
	stored, existing := a.cachedSystemPrompt, a.cachedSystemPromptStatic
	if !a.usePromptCaching || stored == "" || (existing != "" && strings.HasPrefix(stored, existing)) || a.staticRebuildFailedFor == stored {
		return
	}
	parts, err := BuildSystemPromptParts(ctx, a, systemMessage)
	if err == nil && parts.Stable != "" && strings.HasPrefix(stored, parts.Stable) {
		a.cachedSystemPromptStatic, a.staticRebuildFailedFor = parts.Stable, ""
		return
	}
	if err != nil {
		slog.Debug("static system-prefix reconstruction failed on "+logLabel, "err", err)
	}
	a.cachedSystemPromptStatic, a.staticRebuildFailedFor = "", stored
}

// FormatToolsForSystemMessage renders the tool definitions as trajectory format JSON
func FormatToolsForSystemMessage(a *AIAgent) (string, error) {
	if len(a.tools) == 0 {
		return "[]", nil
	}
	out := make([]any, 0, len(a.tools))
	for _, t := range a.tools {
		tool, _ := pyDict(t)
		fn, ok := pyDict(tool["function"])
		if !ok {
			return "", fmt.Errorf("KeyError: 'function'")
		}
		name, ok := fn["name"]
		if !ok {
			return "", fmt.Errorf("KeyError: 'name'")
		}
		description, ok := fn["description"]
		if !ok {
			description = ""
		}
		parameters, ok := fn["parameters"]
		if !ok {
			parameters = map[string]any{}
		}
		row := pyjson.NewObject()
		row.Set("name", name)
		row.Set("description", description)
		row.Set("parameters", parameters)
		row.Set("required", nil)
		out = append(out, row)
	}
	return pyjson.Dumps(out, pyjson.EnsureASCII(false))
}
