package commands

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
)

// CommandReply is the surface independent text of a shared executor with the data it derived
type CommandReply struct {
	Text   string
	Data   map[string]any
	Format string
}

// CommandContext carries the surface name, the raw argument string and the surface options to an executor
type CommandContext struct {
	Surface string
	Args    string
	Options map[string]any
}

// Executor formats one shared command reply from its context, never mutating an agent or session
type Executor func(ctx context.Context, cc CommandContext) (CommandReply, error)

// BannerVersionLabel is the startup banner version label, assigned by cmd/icebath from pkg/app
var BannerVersionLabel func(ctx context.Context) string

// Executors maps a command's execute key to its shared executor
var Executors = map[string]Executor{
	"version":          execVersion,
	"egress":           execEgress,
	"profile":          execProfile,
	"bundles":          execBundles,
	"autoprune":        execAutoprune,
	"gateway_help":     execHelp,
	"gateway_commands": execCommands,
}

// Plain reply carrying text only
func plainReply(text string) CommandReply {
	return CommandReply{Text: text, Data: map[string]any{}, Format: "plain"}
}

// Markdown reply carrying text only
func markdownReply(text string) CommandReply {
	return CommandReply{Text: text, Data: map[string]any{}, Format: "markdown"}
}

// Banner version label as the core version text
func execVersion(ctx context.Context, _ CommandContext) (CommandReply, error) {
	if BannerVersionLabel == nil {
		panic("commands: BannerVersionLabel is not assigned, cmd/icebath installs it from pkg/app")
	}
	return plainReply(BannerVersionLabel(ctx)), nil
}

// Docker egress proxy status as the core egress text
func execEgress(ctx context.Context, _ CommandContext) (CommandReply, error) {
	text, err := provider.FormatEgressStatusText(home.Dir(ctx), false)
	if err != nil {
		return CommandReply{}, err
	}
	return plainReply(text), nil
}

// Option string a surface passed, empty when absent or falsy
func optionText(cc CommandContext, key string) string {
	v, ok := cc.Options[key]
	if !ok || !pyrepr.Truthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Active profile name and home, a multiplexed gateway passing its own per source values in options
func execProfile(ctx context.Context, cc CommandContext) (CommandReply, error) {
	profileName := optionText(cc, "profile_name")
	if profileName == "" {
		profileName = profiles.GetActiveProfileName(ctx)
	}
	homeDisplay := optionText(cc, "home_display")
	if homeDisplay == "" {
		homeDisplay = home.Display(home.Dir(ctx))
	}
	label := profileName
	if dir, err := profiles.GetProfileDir(profileName); err == nil {
		label = profiles.FormatProfileLabel(profileName, profiles.ReadProfileMeta(dir).DisplayName)
	}
	return CommandReply{Text: i18n.T(ctx, "gateway.profile.plain", "", map[string]any{"profile": label, "home": homeDisplay}),
		Data: map[string]any{"profile": profileName, "home": homeDisplay}, Format: "plain"}, nil
}

// Auto prune decision recorded or its state shown
func execAutoprune(ctx context.Context, cc CommandContext) (CommandReply, error) {
	text, err := state.AutoPruneCommand(ctx, cc.Args, home.Dir(ctx))
	if err != nil {
		return CommandReply{}, err
	}
	return plainReply(text), nil
}

// Items of a list valued bundle field, empty for anything else
func bundleSkills(info map[string]any) []any {
	switch v := info["skills"].(type) {
	case []any:
		return v
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out
	}
	return []any{}
}

// Installed skill bundles listed with their skills
func execBundles(ctx context.Context, _ CommandContext) (CommandReply, error) {
	h := home.Dir(ctx)
	bundles, err := skills.NewScope(h, nil).ListBundles()
	if err != nil {
		return CommandReply{}, err
	}
	dir := home.BundlesDir(h)
	if bundles == nil {
		bundles = []map[string]any{}
	}
	if len(bundles) == 0 {
		return CommandReply{Text: i18n.T(ctx, "gateway.bundles.none_plain", "", map[string]any{"dir": dir}),
			Data: map[string]any{"bundles": bundles, "dir": dir}, Format: "plain"}, nil
	}
	lines := []string{i18n.T(ctx, "gateway.bundles.header_plain", "", map[string]any{"count": len(bundles)})}
	for _, info := range bundles {
		names := bundleSkills(info)
		desc := info["description"]
		if !pyrepr.Truthy(desc) {
			desc = i18n.T(ctx, "gateway.bundles.default_desc", "", map[string]any{"count": len(names)})
		}
		lines = append(lines, i18n.T(ctx, "gateway.bundles.item_plain", "", map[string]any{"slug": info["slug"], "desc": desc, "count": len(names)}))
		for _, s := range names {
			lines = append(lines, "    · "+pyrepr.Str(s))
		}
	}
	lines = append(lines, i18n.T(ctx, "gateway.bundles.invoke_hint_plain", "", nil))
	return CommandReply{Text: strings.Join(lines, "\n"), Data: map[string]any{"bundles": bundles, "dir": dir}, Format: "plain"}, nil
}

// Registered skill commands of the process home, empty when the skill subsystem fails
func skillCommands(ctx context.Context) (out skills.SkillCommands) {
	defer func() {
		if recover() != nil {
			out = skills.SkillCommands{}
		}
	}()
	return skills.NewScope(home.Dir(ctx), nil).GetSkillCommands()
}

// Canonical names a gateway caller may run, nil when the surface sets no restriction
func allowedOption(cc CommandContext) []string {
	raw, ok := cc.Options["allowed_commands"]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			out = append(out, fmt.Sprint(x))
		}
		return out
	case map[string]bool:
		out := make([]string, 0, len(v))
		for k := range v {
			out = append(out, k)
		}
		return out
	}
	return []string{}
}

// Gateway help lines of the default registry under the raw config gates of the process home
func defaultGatewayHelpLines(ctx context.Context, allowed []string) []string {
	raw, err := config.ReadRawConfig(ctx, home.Dir(ctx))
	if err != nil {
		raw = nil
	}
	return GatewayHelpLines(Default, config.Tree(raw), allowed)
}

// Gateway help body with the first ten skill commands, before platform mention decoration
func execHelp(ctx context.Context, cc CommandContext) (CommandReply, error) {
	allowed := allowedOption(cc)
	lines := append([]string{i18n.T(ctx, "gateway.help.header", "", nil)}, defaultGatewayHelpLines(ctx, allowed)...)
	skillCmds := skills.SkillCommands{}
	if allowed == nil {
		skillCmds = skillCommands(ctx)
	}
	if len(skillCmds) > 0 {
		lines = append(lines, i18n.T(ctx, "gateway.help.skill_header", "", map[string]any{"count": len(skillCmds)}))
		names := make([]string, 0, len(skillCmds))
		for cmd := range skillCmds {
			names = append(names, cmd)
		}
		sort.Strings(names)
		listed := true
		for _, cmd := range names[:min(10, len(names))] {
			desc, ok := skillCmds[cmd]["description"]
			if !ok {
				listed = false
				break
			}
			lines = append(lines, "`"+cmd+"` — "+pyrepr.Str(desc))
		}
		if listed && len(names) > 10 {
			lines = append(lines, i18n.T(ctx, "gateway.help.more_use_commands", "", map[string]any{"count": len(names) - 10}))
		}
	}
	return markdownReply(strings.Join(lines, "\n")), nil
}

// Skill command and collision note entries of the commands listing, stopping where the Python block raises
func commandListingExtras(ctx context.Context, skillCmds skills.SkillCommands) []string {
	var entries []string
	if len(skillCmds) > 0 {
		entries = append(entries, "", i18n.T(ctx, "gateway.commands.skill_header", "", nil))
		names := make([]string, 0, len(skillCmds))
		for cmd := range skillCmds {
			names = append(names, cmd)
		}
		sort.Strings(names)
		for _, cmd := range names {
			desc := ""
			if raw, ok := skillCmds[cmd]["description"]; ok {
				text, isText := raw.(string)
				if !isText {
					return entries
				}
				desc = strings.TrimSpace(text)
			}
			if desc == "" {
				desc = i18n.T(ctx, "gateway.commands.default_desc", "", nil)
			}
			entries = append(entries, "`"+cmd+"` — "+desc)
		}
	}
	var names []string
	for _, s := range skills.NewScope(home.Dir(ctx), nil).FindAllSkills(true) {
		name, ok := s["name"].(string)
		if !ok {
			return entries
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if note, ok := skills.SkillCommandCollisionNote(name); ok && note != "" {
			entries = append(entries, "⚠ "+note)
		}
	}
	return entries
}

// Page size option as Python int reads it, twenty when absent or unconvertible
func pageSizeOption(cc CommandContext) (int64, error) {
	raw, ok := cc.Options["page_size"]
	if !ok {
		return 20, nil
	}
	n, err := pyint.Of(raw)
	var conv *pyint.Error
	if errors.As(err, &conv) && (conv.Kind == "TypeError" || conv.Kind == "ValueError") {
		return 20, nil
	}
	if err != nil {
		return 0, err
	}
	if n.Cmp(big.NewInt(1)) < 0 {
		return 1, nil
	}
	if !n.IsInt64() {
		return 1 << 62, nil
	}
	return n.Int64(), nil
}

// Paginated gateway listing of every command and skill command with page navigation
func execCommands(ctx context.Context, cc CommandContext) (CommandReply, error) {
	requested := big.NewInt(1)
	if s := strings.TrimSpace(cc.Args); s != "" {
		n, ok := pyint.Parse(s)
		if !ok {
			return markdownReply(i18n.T(ctx, "gateway.commands.usage", "", nil)), nil
		}
		requested = n
	}
	allowed := allowedOption(cc)
	entries := defaultGatewayHelpLines(ctx, allowed)
	skillCmds := skills.SkillCommands{}
	if allowed == nil {
		skillCmds = skillCommands(ctx)
	}
	entries = append(entries, commandListingExtras(ctx, skillCmds)...)
	if len(entries) == 0 {
		return markdownReply(i18n.T(ctx, "gateway.commands.none", "", nil)), nil
	}
	pageSize, err := pageSizeOption(cc)
	if err != nil {
		return CommandReply{}, err
	}
	totalPages := max(1, (int64(len(entries))+pageSize-1)/pageSize)
	page := totalPages
	switch {
	case requested.Cmp(big.NewInt(1)) < 0:
		page = 1
	case requested.Cmp(big.NewInt(totalPages)) < 0:
		page = requested.Int64()
	}
	start := (page - 1) * pageSize
	end := min(start+pageSize, int64(len(entries)))
	lines := append([]string{i18n.T(ctx, "gateway.commands.header", "", map[string]any{"total": len(entries), "page": page, "total_pages": totalPages}), ""},
		entries[start:end]...)
	if totalPages > 1 {
		var nav []string
		if page > 1 {
			nav = append(nav, i18n.T(ctx, "gateway.commands.nav_prev", "", map[string]any{"page": page - 1}))
		}
		if page < totalPages {
			nav = append(nav, i18n.T(ctx, "gateway.commands.nav_next", "", map[string]any{"page": page + 1}))
		}
		lines = append(lines, "", strings.Join(nav, " | "))
	}
	if big.NewInt(page).Cmp(requested) != 0 {
		lines = append(lines, i18n.T(ctx, "gateway.commands.out_of_range", "", map[string]any{"requested": requested.String(), "page": page}))
	}
	return markdownReply(strings.Join(lines, "\n")), nil
}

// Shared executor of a command definition, nil when its command has no execute key in the table
func ResolveExecutor(d *Def) Executor {
	if d == nil {
		return nil
	}
	return Executors[MetaOf(d.Name).Execute]
}

// Reply of the definition's shared executor, false when the command has none
func RunExecute(ctx context.Context, d *Def, cc CommandContext) (CommandReply, bool, error) {
	fn := ResolveExecutor(d)
	if fn == nil {
		return CommandReply{}, false, nil
	}
	reply, err := fn(ctx, cc)
	return reply, true, err
}

// NoExecutorError reports a command name with no registry owned executor
type NoExecutorError struct{ Name string }

func (e *NoExecutorError) Error() string { return "no registry-owned executor for /" + e.Name }

// Runs the shared executor of the named command, a NoExecutorError when it is unknown or has none
func ExecuteCommand(ctx context.Context, name string, cc CommandContext) (CommandReply, error) {
	if d := Resolve(name); d != nil {
		if reply, ok, err := RunExecute(ctx, d, cc); ok {
			return reply, err
		}
	}
	return CommandReply{}, &NoExecutorError{Name: name}
}
