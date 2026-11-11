package tuiserver

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

const slashCompletionLimit = 30

// One completion of the classic prompt slash completer, display and meta already plain text
type SlashCompletion struct {
	Text, Display, Meta string
}

// Hooks into pkg/commands assigned in cmd/icebath/wiring.go, the slash completer and the gateway command names
var (
	SlashCompleter       func(ctx context.Context, skillCommands, skillBundles skills.SkillCommands) func(text string) []SlashCompletion
	GatewayKnownCommands func() map[string]bool
)

// Static slash entries the TUI adds when the completer offers none of the same text
var slashExtras = [][2]string{
	{"/density", "Toggle compact display mode"}, {"/details", "Control agent detail visibility"},
	{"/logs", "Show recent gateway log lines"},
	{"/mouse", "Set mouse tracking preset [on|off|toggle|wheel|buttons|all]"},
}

var (
	detailsSections = []string{"thinking", "tools", "subagents", "activity"}
	detailsModes    = []string{"hidden", "collapsed", "expanded"}
)

// Logger of the slash discovery builders
func discoveryLogger() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.command_discovery")
}

// Completion item with its display defaulting to the text
func completionItem(text, meta string, display ...string) map[string]any {
	shown := text
	if len(display) > 0 {
		shown = display[0]
	}
	return map[string]any{"text": text, "display": shown, "meta": meta}
}

// Skill activity count and hub, bundled or local origin lookups, zero and local when the usage store fails
func skillUsageLookup(ctx context.Context) (func(string) *big.Int, func(string) string) {
	scope := skills.NewScope(home.Dir(ctx), nil)
	records, err := scope.LoadUsage()
	var bundled map[string]bool
	if err == nil {
		bundled, err = scope.ReadBundledNames()
	}
	if err != nil {
		discoveryLogger().Debug(fmt.Sprintf("skill usage lookup unavailable: %s", err))
		return func(string) *big.Int { return big.NewInt(0) }, func(string) string { return "local" }
	}
	hub := scope.ReadHubInstalledNames()
	usage := func(name string) *big.Int {
		rec, _ := records.GetOr(name, nil).(*pyjson.Object)
		if rec == nil {
			rec = pyjson.NewObject()
		}
		count, err := skills.ActivityCount(rec)
		if err != nil {
			return big.NewInt(0)
		}
		return count
	}
	origin := func(name string) string {
		switch {
		case hub[name]:
			return "hub"
		case bundled[name]:
			return "bundled"
		}
		return "local"
	}
	return usage, origin
}

// Lowercased command name of a completion item without its slash
func slashNameOf(item map[string]any) string {
	return strings.ToLower(strings.TrimLeft(pystr.Strip(itemField(item, "text")), "/"))
}

// Registry commands in order then skills by score, use and name, each kind capped at the completion limit
func rankSlashCompletions(items []map[string]any, usage func(string) *big.Int, originOf func(string) string, browsing bool, scoreOf func(map[string]any) float64, registryCommandNames map[string]bool) []map[string]any {
	var commands, skillItems []map[string]any
	for _, item := range items {
		if item["kind"] == "skill" {
			skillItems = append(skillItems, item)
		} else {
			commands = append(commands, item)
		}
	}
	if browsing {
		skillItems = slices.DeleteFunc(skillItems, func(item map[string]any) bool {
			name := slashNameOf(item)
			return originOf(name) == "bundled" && usage(name).Sign() <= 0
		})
	}
	slices.SortStableFunc(skillItems, func(a, b map[string]any) int {
		if scoreOf != nil {
			if c := cmpFloat(scoreOf(a), scoreOf(b)); c != 0 {
				return c
			}
		}
		if c := usage(slashNameOf(b)).Cmp(usage(slashNameOf(a))); c != 0 {
			return c
		}
		return strings.Compare(slashNameOf(a), slashNameOf(b))
	})
	var ranked []map[string]any
	if browsing {
		if registryCommandNames == nil {
			registryCommandNames = GatewayKnownCommands()
		}
		var other []map[string]any
		for _, c := range commands {
			if registryCommandNames[slashNameOf(c)] {
				ranked = append(ranked, c)
			} else {
				other = append(other, c)
			}
		}
		ranked = append(ranked, other[:min(len(other), slashCompletionLimit)]...)
	} else {
		ranked = append(ranked, commands[:min(len(commands), slashCompletionLimit)]...)
	}
	return append(ranked, skillItems[:min(len(skillItems), slashCompletionLimit)]...)
}

// Three way float comparison with infinities ordered last
func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Meta of a root candidate of the details command
func detailsRootMeta(candidate string) string {
	if slices.Contains(detailsSections, candidate) {
		return "section override"
	}
	if candidate == "cycle" {
		return "cycle global mode"
	}
	return "global mode"
}

// Argument completions of the details command, nil when the text is not that command
func detailsCompletions(text string) []map[string]any {
	if len(text) < len("/details") || strings.ToLower(text[:len("/details")]) != "/details" {
		return nil
	}
	stripped := pystr.Strip(text)
	if stripped != "" && !strings.HasPrefix("/details", strings.Fields(strings.ToLower(stripped))[0]) {
		return nil
	}
	body := strings.TrimPrefix(text[len("/details"):], " ")
	parts := strings.Fields(body)
	trailing := strings.HasSuffix(text, " ")
	rootCandidates := slices.Concat(detailsModes, []string{"cycle"}, detailsSections)
	out := []map[string]any{}
	if body == "" || (len(parts) == 0 && trailing) {
		lead := " "
		if trailing {
			lead = ""
		}
		for _, c := range rootCandidates {
			out = append(out, completionItem(lead+c, detailsRootMeta(c)))
		}
		return out
	}
	if len(parts) == 1 && !trailing {
		prefix := strings.ToLower(parts[0])
		for _, c := range rootCandidates {
			if strings.HasPrefix(c, prefix) && c != prefix {
				out = append(out, completionItem(c, detailsRootMeta(c)))
			}
		}
		return out
	}
	section := ""
	if len(parts) > 0 {
		section = strings.ToLower(parts[0])
	}
	if !slices.Contains(detailsSections, section) {
		return out
	}
	sectionMeta := func(candidate string) string {
		if candidate == "reset" {
			return "clear " + section + " override"
		}
		return "set " + section
	}
	modeCandidates := append(slices.Clone(detailsModes), "reset")
	if len(parts) == 1 {
		for _, c := range modeCandidates {
			out = append(out, completionItem(c, sectionMeta(c)))
		}
		return out
	}
	if len(parts) == 2 && !trailing {
		prefix := strings.ToLower(parts[1])
		for _, c := range modeCandidates {
			if strings.HasPrefix(c, prefix) && c != prefix {
				out = append(out, completionItem(c, sectionMeta(c)))
			}
		}
	}
	return out
}

// Character index of the token after the last space, else the given default
func replaceFrom(text string, otherwise int) int {
	at := strings.LastIndex(text, " ")
	if at < 0 {
		return otherwise
	}
	return utf8.RuneCountInString(text[:at]) + 1
}

// Slash popup items for the text, skills and bundles read inside the scope of the calling session
func slashCompletions(ctx context.Context, text string, scope func(context.Context) (context.Context, error)) (map[string]any, error) {
	if !strings.HasPrefix(text, "/") {
		return map[string]any{"items": []any{}}, nil
	}
	scoped := ctx
	if scope != nil {
		var err error
		if scoped, err = scope(ctx); err != nil {
			return nil, err
		}
	}
	skillScope := skills.NewScope(home.Dir(scoped), nil)
	cwd, err := tools.ResolveAgentCwd(tools.ToolEnvFrom(scoped))
	if err != nil {
		return nil, err
	}
	skillScope.Cwd = cwd
	skillCommands, err := skillScope.GetInteractiveSkillCommands(scoped)
	if err != nil {
		return nil, err
	}
	skillBundles, err := skillScope.GetSkillBundles()
	if err != nil {
		return nil, err
	}
	complete := SlashCompleter(ctx, skillCommands, skillBundles)
	skillNames := map[string]bool{}
	for key := range skillCommands {
		skillNames[strings.ToLower(strings.TrimLeft(key, "/"))] = true
	}
	for key := range skillBundles {
		skillNames[strings.ToLower(strings.TrimLeft(key, "/"))] = true
	}
	toItems := func(doc string) []map[string]any {
		var out []map[string]any
		for _, c := range complete(doc) {
			display := c.Text
			if c.Display != "" {
				display = c.Display
			}
			kind := "command"
			if skillNames[strings.ToLower(strings.TrimLeft(pystr.Strip(c.Text), "/"))] {
				kind = "skill"
			}
			out = append(out, map[string]any{"text": c.Text, "display": display, "meta": c.Meta, "kind": kind})
		}
		return out
	}
	items := toItems(text)
	lastToken := text[strings.LastIndex(text, " ")+1:]
	if strings.HasPrefix(lastToken, "/") {
		var scoreOf func(map[string]any) float64
		if !strings.Contains(text, " ") && utf8.RuneCountInString(text) > 1 {
			items, scoreOf = fuzzyRankSlashItems(items, toItems("/"), normalizeSlashSearchQuery(text))
		}
		usage, originOf := skillUsageLookup(ctx)
		items = rankSlashCompletions(items, usage, originOf, text == "/", scoreOf, nil)
	} else {
		items = items[:min(len(items), slashCompletionLimit)]
	}
	textLower := strings.ToLower(text)
	for _, extra := range slashExtras {
		if strings.HasPrefix(extra[0], textLower) && !slices.ContainsFunc(items, func(item map[string]any) bool { return item["text"] == extra[0] }) {
			row := completionItem(extra[0], extra[1])
			row["kind"] = "command"
			items = append(items, row)
		}
	}
	if details := detailsCompletions(text); details != nil {
		return map[string]any{"items": anyItems(details), "replace_from": replaceFrom(text, utf8.RuneCountInString(text))}, nil
	}
	return map[string]any{"items": anyItems(items), "replace_from": replaceFrom(text, 1)}, nil
}

// Item dicts as a JSON list
func anyItems(items []map[string]any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}
