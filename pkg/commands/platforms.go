package commands

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
)

// NameDesc is one platform menu entry
type NameDesc struct {
	Name        string
	Description string
}

// GatewaySkill is a skill eligible as a gateway slash command with its path under its scan root
type GatewaySkill struct {
	Key         string
	Description string
	RelParts    []string
}

// SkillSource yields the gateway eligible skills for a platform
type SkillSource func(platform string) []GatewaySkill

const cmdNameLimit = 32

var tgInvalidChars = regexp.MustCompile(`[^a-z0-9_]`)
var tgMultiUnderscore = regexp.MustCompile(`_{2,}`)
var tgDashes = regexp.MustCompile("[‒–—―−]")

// ResolveConfigGates returns canonical names whose gateway config gate is truthy in config
func ResolveConfigGates(cfg config.Tree) map[string]bool {
	out := map[string]bool{}
	for _, d := range Builtin {
		gate := builtinMeta[d.Name].GatewayConfigGate
		if gate != "" && config.Truthy(cfg.Get(gate)) {
			out[d.Name] = true
		}
	}
	return out
}

// GatewayAvailable reports whether a command is offered on gateway surfaces
func GatewayAvailable(d *Def, gates map[string]bool) bool {
	if !d.CLIOnly {
		return true
	}
	if MetaOf(d.Name).GatewayConfigGate == "" {
		return false
	}
	return gates[d.Name]
}

// GatewayAvailableCommands lists registry commands visible on gateway surfaces
func GatewayAvailableCommands(r *Registry, cfg config.Tree) []*Def {
	gates := ResolveConfigGates(cfg)
	var out []*Def
	for _, d := range All(r) {
		if d.Plugin == "" && GatewayAvailable(d, gates) {
			out = append(out, d)
		}
	}
	return out
}

// RequiresArgument reports whether a hint makes a bare invocation incomplete
func RequiresArgument(argsHint string) bool {
	return strings.HasPrefix(strings.TrimSpace(argsHint), "<")
}

// SanitizeTelegramName lowers, folds hyphens and drops a name that would lose letters
func SanitizeTelegramName(raw string) string {
	lowered := strings.ReplaceAll(strings.ToLower(raw), "-", "_")
	for _, m := range tgInvalidChars.FindAllString(lowered, -1) {
		for _, ch := range m {
			if unicode.IsLetter(ch) || unicode.IsDigit(ch) {
				return ""
			}
		}
	}
	name := tgInvalidChars.ReplaceAllString(lowered, "")
	return strings.Trim(tgMultiUnderscore.ReplaceAllString(name, "_"), "_")
}

// NormalizeTelegramDesc folds Unicode dashes BotFather rejects to ASCII
func NormalizeTelegramDesc(desc string) string { return tgDashes.ReplaceAllString(desc, "-") }

// TruncateDesc clamps a description to a limit with an ellipsis tail
func TruncateDesc(desc string, limit int) string {
	r := []rune(desc)
	if len(r) <= limit {
		return desc
	}
	return string(r[:limit-3]) + "..."
}

type menuEntry struct {
	name, desc, cmdKey, raw, source string
}

func clampCommandNames(entries []menuEntry, reserved map[string]bool) []menuEntry {
	used := map[string]bool{}
	for k := range reserved {
		used[k] = true
	}
	var out []menuEntry
	for _, e := range entries {
		name := e.name
		if len(name) > cmdNameLimit {
			candidate := name[:cmdNameLimit]
			if used[candidate] {
				prefix := name[:cmdNameLimit-1]
				found := false
				for digit := 0; digit < 10; digit++ {
					candidate = prefix + string(rune('0'+digit))
					if !used[candidate] {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			name = candidate
		}
		if used[name] {
			continue
		}
		used[name] = true
		e.name = name
		out = append(out, e)
	}
	return out
}

// TelegramBotCommands returns sanitized canonical commands for setMyCommands, plugins optionally included
func TelegramBotCommands(r *Registry, cfg config.Tree, includePlugins bool) []NameDesc {
	var pairs []NameDesc
	for _, d := range GatewayAvailableCommands(r, cfg) {
		pairs = append(pairs, NameDesc{d.Name, d.Description})
	}
	if includePlugins {
		for _, p := range PluginEntries(r) {
			if !RequiresArgument(p.ArgsHint) {
				pairs = append(pairs, NameDesc{p.Name, PluginDescription(p)})
			}
		}
	}
	var out []NameDesc
	for _, p := range pairs {
		if tg := SanitizeTelegramName(p.Name); tg != "" {
			out = append(out, NameDesc{tg, NormalizeTelegramDesc(p.Description)})
		}
	}
	return out
}

// TelegramDefaultMenuMax is the default visible menu cap for Telegram
const TelegramDefaultMenuMax = 60

// TelegramBotAPIMaxCommands is the Bot API limit for setMyCommands
const TelegramBotAPIMaxCommands = 100

var telegramPriorityTiers = map[string][]string{
	"prepend": {"configured", "default"},
	"append":  {"default", "configured"},
	"replace": {"configured"},
}

// TelegramMenuPriority ranks the builtins that survive the small visible menu
var TelegramMenuPriority = []string{
	"help", "new", "stop", "status", "egress", "resume", "sessions", "model",
	"debug", "restart", "update", "verbose", "commands",
	"approve", "deny", "queue", "steer", "bg", "btw",
	"reasoning", "usage", "platforms", "platform", "profile", "whoami",
}

// TelegramMenuConfig is the normalized platforms.telegram.extra.command_menu block
type TelegramMenuConfig struct {
	MaxCommands  int
	PriorityMode string
	Priority     []string
}

// TelegramCommandMenuConfig reads the command menu config with safe defaults
func TelegramCommandMenuConfig(cfg config.Tree) TelegramMenuConfig {
	menu := cfg.Sub("platforms.telegram.extra.command_menu")
	maxCommands := menu.Int("max_commands", TelegramDefaultMenuMax)
	if maxCommands < 1 {
		maxCommands = 1
	}
	if maxCommands > TelegramBotAPIMaxCommands {
		maxCommands = TelegramBotAPIMaxCommands
	}
	mode := strings.ToLower(strings.TrimSpace(menu.String("priority_mode", "prepend")))
	if mode == "" {
		mode = "prepend"
	}
	if _, ok := telegramPriorityTiers[mode]; !ok {
		mode = "prepend"
	}
	var priority []string
	for _, item := range menu.Strings("priority") {
		if strings.TrimSpace(item) != "" {
			priority = append(priority, item)
		}
	}
	return TelegramMenuConfig{MaxCommands: maxCommands, PriorityMode: mode, Priority: priority}
}

// TelegramMenuMaxCommands returns the configured Telegram menu cap within Bot API bounds
func TelegramMenuMaxCommands(cfg config.Tree) int { return TelegramCommandMenuConfig(cfg).MaxCommands }

func sanitizedRank(raw []string) map[string]int {
	rank := map[string]int{}
	for _, r := range raw {
		name := SanitizeTelegramName(r)
		if _, seen := rank[name]; name != "" && !seen {
			rank[name] = len(rank)
		}
	}
	return rank
}

func prioritizeTelegram(cfg config.Tree, candidates []menuEntry) []menuEntry {
	menu := TelegramCommandMenuConfig(cfg)
	configured := sanitizedRank(menu.Priority)
	defaults := sanitizedRank(TelegramMenuPriority)
	tiers := telegramPriorityTiers[menu.PriorityMode]
	type ranked struct {
		tier, index, stable int
		entry               menuEntry
	}
	rows := make([]ranked, 0, len(candidates))
	for i, c := range candidates {
		row := ranked{tier: len(tiers), index: 0, stable: i, entry: c}
		for tier, table := range tiers {
			var idx int
			var ok bool
			if table == "configured" {
				if idx, ok = configured[c.raw]; !ok {
					idx, ok = configured[c.name]
				}
			} else if c.source == "core" {
				idx, ok = defaults[c.name]
			}
			if ok {
				row.tier, row.index = tier, idx
				break
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].tier != rows[j].tier {
			return rows[i].tier < rows[j].tier
		}
		if rows[i].index != rows[j].index {
			return rows[i].index < rows[j].index
		}
		return rows[i].stable < rows[j].stable
	})
	out := make([]menuEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.entry)
	}
	return out
}

func collectGatewaySkillEntries(r *Registry, platform string, skills SkillSource, maxSlots int, reserved map[string]bool,
	descLimit int, sanitize func(string) string) ([]menuEntry, int) {
	if sanitize == nil {
		sanitize = func(n string) string { return n }
	}
	entries := func(rows []menuEntry) []menuEntry {
		var out []menuEntry
		for _, row := range rows {
			if name := sanitize(row.raw); name != "" {
				out = append(out, menuEntry{name: name, desc: TruncateDesc(row.desc, descLimit), cmdKey: row.cmdKey, raw: name})
			}
		}
		return clampCommandNames(out, reserved)
	}
	var pluginRows []menuEntry
	for _, p := range PluginEntries(r) {
		if platform == "telegram" && RequiresArgument(p.ArgsHint) {
			continue
		}
		desc := p.Description
		if desc == "" {
			desc = "Plugin command"
		}
		pluginRows = append(pluginRows, menuEntry{raw: p.Name, desc: desc})
	}
	pluginEntries := entries(pluginRows)
	for _, e := range pluginEntries {
		reserved[e.name] = true
	}
	var skillRows []menuEntry
	if skills != nil {
		for _, s := range skills(platform) {
			skillRows = append(skillRows, menuEntry{raw: strings.TrimLeft(s.Key, "/"), desc: s.Description, cmdKey: s.Key})
		}
	}
	skillEntries := entries(skillRows)
	if maxSlots < 0 {
		return append(pluginEntries, skillEntries...), 0
	}
	remaining := maxSlots - len(pluginEntries)
	if remaining < 0 {
		remaining = 0
	}
	hidden := len(skillEntries) - remaining
	if hidden < 0 {
		hidden = 0
	}
	if len(skillEntries) > remaining {
		skillEntries = skillEntries[:remaining]
	}
	all := append(pluginEntries, skillEntries...)
	if len(all) > maxSlots {
		all = all[:maxSlots]
	}
	return all, hidden
}

// TelegramMenuCommands builds the capped Telegram menu with core, plugin then skill tiers and the hidden count
func TelegramMenuCommands(r *Registry, cfg config.Tree, skills SkillSource, maxCommands int) ([]NameDesc, int) {
	core := TelegramBotCommands(r, cfg, false)
	reserved := map[string]bool{}
	for _, c := range core {
		reserved[c.Name] = true
	}
	entries, hidden := collectGatewaySkillEntries(r, "telegram", skills, -1, reserved, 40, SanitizeTelegramName)
	var candidates []menuEntry
	for _, c := range core {
		candidates = append(candidates, menuEntry{name: c.Name, desc: c.Description, source: "core", raw: c.Name})
	}
	for _, e := range entries {
		source := "plugin"
		if e.cmdKey != "" {
			source = "skill"
		}
		candidates = append(candidates, menuEntry{name: e.name, desc: e.desc, source: source, raw: e.raw})
	}
	candidates = prioritizeTelegram(cfg, candidates)
	overflow := len(candidates) - maxCommands
	if overflow < 0 {
		overflow = 0
	}
	if len(candidates) > maxCommands {
		candidates = candidates[:maxCommands]
	}
	menu := make([]NameDesc, 0, len(candidates))
	for _, c := range candidates {
		menu = append(menu, NameDesc{c.name, NormalizeTelegramDesc(c.desc)})
	}
	return menu, hidden + overflow
}

// DiscordSkillEntry is one /skill autocomplete row
type DiscordSkillEntry struct {
	Name        string
	Description string
	CmdKey      string
}

// DiscordSkillCategory groups skills nested under one top level folder
type DiscordSkillCategory struct {
	Name    string
	Entries []DiscordSkillEntry
}

// DiscordCollision describes a skill dropped because its clamped name was taken
type DiscordCollision struct {
	Name     string
	CmdKey   string
	Prior    string
	Reserved bool
}

// DiscordSkillCommandsByCategory groups gateway skills for Discord autocomplete and reports clamp collisions
func DiscordSkillCommandsByCategory(skills SkillSource, reserved map[string]bool) ([]DiscordSkillCategory, []DiscordSkillEntry, []DiscordCollision) {
	namesUsed := map[string]string{}
	for k := range reserved {
		namesUsed[k] = "<reserved>"
	}
	var categories []DiscordSkillCategory
	index := map[string]int{}
	var uncategorized []DiscordSkillEntry
	var collisions []DiscordCollision
	if skills == nil {
		return nil, nil, nil
	}
	for _, s := range skills("discord") {
		name := strings.TrimLeft(s.Key, "/")
		if len(name) > cmdNameLimit {
			name = name[:cmdNameLimit]
		}
		if prior, taken := namesUsed[name]; taken {
			collisions = append(collisions, DiscordCollision{Name: name, CmdKey: s.Key, Prior: prior, Reserved: prior == "<reserved>"})
			continue
		}
		namesUsed[name] = s.Key
		entry := DiscordSkillEntry{name, TruncateDesc(s.Description, 100), s.Key}
		if len(s.RelParts) >= 2 {
			i, ok := index[s.RelParts[0]]
			if !ok {
				i = len(categories)
				index[s.RelParts[0]] = i
				categories = append(categories, DiscordSkillCategory{Name: s.RelParts[0]})
			}
			categories[i].Entries = append(categories[i].Entries, entry)
		} else {
			uncategorized = append(uncategorized, entry)
		}
	}
	return categories, uncategorized, collisions
}

// SlackMaxSlashCommands is the manifest cap on slash commands
const SlackMaxSlashCommands = 50

var slackInvalidChars = regexp.MustCompile(`[^a-z0-9_\-]`)

// SlackReservedCommands are Slack built ins an app cannot register
var SlackReservedCommands = map[string]bool{
	"me": true, "status": true, "away": true, "dnd": true, "shrug": true, "remind": true, "msg": true, "feed": true,
	"who": true, "collapse": true, "expand": true, "leave": true, "join": true, "open": true, "search": true,
	"topic": true, "mute": true, "pro": true, "shortcuts": true,
}

// SlackViaIcebathOnly lists canonical commands reachable on Slack only through the /icebath slash
var SlackViaIcebathOnly = map[string]bool{
	"topup": true, "moa": true, "debug": true, "egress": true, "init": true, "version": true, "diff": true,
	"update": true, "heartbeat": true, "refine": true, "review": true, "pause": true, "whoami": true,
	"platform": true, "insights": true, "login": true, "discard": true, "autoprune": true,
}

// SanitizeSlackName lowers, strips invalid characters and edge punctuation and clamps to 32
func SanitizeSlackName(raw string) string {
	name := strings.Trim(slackInvalidChars.ReplaceAllString(strings.ToLower(raw), ""), "-_")
	if len(name) > cmdNameLimit {
		name = name[:cmdNameLimit]
	}
	return name
}

// SlackSlash is one native Slack slash command
type SlackSlash struct {
	Name        string
	Description string
	UsageHint   string
}

// SlackNativeSlashes lists every gateway command as a standalone slash under the manifest cap
func SlackNativeSlashes(r *Registry, cfg config.Tree) []SlackSlash {
	available := GatewayAvailableCommands(r, cfg)
	var wanted []SlackSlash
	for _, d := range available {
		wanted = append(wanted, SlackSlash{d.Name, d.Description, d.ArgsHint})
	}
	for _, d := range available {
		for _, a := range d.Aliases {
			wanted = append(wanted, SlackSlash{a, "Alias for /" + d.Name + " — " + d.Description, d.ArgsHint})
		}
	}
	for _, p := range PluginEntries(r) {
		wanted = append(wanted, SlackSlash{p.Name, p.Description, p.ArgsHint})
	}
	entries := []SlackSlash{{"icebath", "Talk to icebath or run a subcommand", "[subcommand] [args]"}}
	seen := map[string]bool{"icebath": true}
	for _, w := range wanted {
		name := SanitizeSlackName(w.Name)
		if name == "" || seen[name] || SlackReservedCommands[name] || SlackViaIcebathOnly[name] || len(entries) >= SlackMaxSlashCommands {
			continue
		}
		entries = append(entries, SlackSlash{name, truncateRunes(w.Description, 140), truncateRunes(w.UsageHint, 100)})
		seen[name] = true
	}
	return entries
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// SlackAppManifest returns the features.slash_commands manifest portion
func SlackAppManifest(r *Registry, cfg config.Tree, requestURL string) map[string]any {
	if requestURL == "" {
		requestURL = "https://icebath.local/slack/commands"
	}
	var slashes []map[string]any
	for _, s := range SlackNativeSlashes(r, cfg) {
		desc := s.Description
		if desc == "" {
			desc = "Run /" + s.Name
		}
		entry := map[string]any{"command": "/" + s.Name, "description": desc, "should_escape": false, "url": requestURL}
		if s.UsageHint != "" {
			entry["usage_hint"] = s.UsageHint
		}
		slashes = append(slashes, entry)
	}
	return map[string]any{"features": map[string]any{"slash_commands": slashes}}
}

// SlackSubcommandMap maps every name and alias to its /command for the /icebath handler
func SlackSubcommandMap(r *Registry, cfg config.Tree) map[string]string {
	out := map[string]string{}
	for _, d := range GatewayAvailableCommands(r, cfg) {
		out[d.Name] = "/" + d.Name
		for _, a := range d.Aliases {
			out[a] = "/" + a
		}
	}
	for _, p := range PluginEntries(r) {
		if _, ok := out[p.Name]; !ok {
			out[p.Name] = "/" + p.Name
		}
	}
	return out
}

// GatewayHelpLines renders the gateway help catalog, restricted to allowed canonical names when given
func GatewayHelpLines(r *Registry, cfg config.Tree, allowed []string) []string {
	gates := ResolveConfigGates(cfg)
	var allowedSet map[string]bool
	if allowed != nil {
		allowedSet = map[string]bool{}
		for _, a := range allowed {
			allowedSet[a] = true
		}
	}
	var lines []string
	for _, d := range All(r) {
		if d.Plugin != "" || !GatewayAvailable(d, gates) {
			continue
		}
		if allowedSet != nil && !allowedSet[d.Name] {
			continue
		}
		args := ""
		if d.ArgsHint != "" {
			args = " " + d.ArgsHint
		}
		var aliasParts []string
		for _, a := range d.Aliases {
			if strings.ReplaceAll(a, "-", "_") == strings.ReplaceAll(d.Name, "-", "_") && a != d.Name {
				continue
			}
			aliasParts = append(aliasParts, "`/"+a+"`")
		}
		note := ""
		if len(aliasParts) > 0 {
			note = " (alias: " + strings.Join(aliasParts, ", ") + ")"
		}
		lines = append(lines, "`/"+d.Name+args+"` -- "+d.Description+note)
	}
	return lines
}
