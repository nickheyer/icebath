package commands

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// CommandArg returns everything after the slash command word, optionally lowercased
func CommandArg(cmd string, lower bool) string {
	arg := SlashArgs(cmd)
	if lower {
		return strings.ToLower(arg)
	}
	return arg
}

// ShlexArgs splits the tokens after the command word, whitespace splitting on unbalanced quotes
func ShlexArgs(cmd string) []string {
	if strings.TrimSpace(cmd) == "" {
		return nil
	}
	tokens, err := Shlex(cmd)
	if err != nil {
		tokens = strings.Fields(cmd)
	}
	if len(tokens) == 0 {
		return nil
	}
	return tokens[1:]
}

// ShlexError reports unbalanced quoting
type ShlexError struct{ Message string }

func (e *ShlexError) Error() string { return e.Message }

// Shlex splits a line with POSIX shell quoting rules
func Shlex(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\\':
			if i+1 >= len(runes) {
				return nil, &ShlexError{"No escaped character"}
			}
			i++
			cur.WriteRune(runes[i])
			inWord = true
		case c == '\'':
			inWord = true
			j := i + 1
			for j < len(runes) && runes[j] != '\'' {
				j++
			}
			if j >= len(runes) {
				return nil, &ShlexError{"No closing quotation"}
			}
			cur.WriteString(string(runes[i+1 : j]))
			i = j
		case c == '"':
			inWord = true
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				if runes[j] == '\\' && j+1 < len(runes) && strings.ContainsRune("\"\\$`\n", runes[j+1]) {
					j++
				}
				cur.WriteRune(runes[j])
				j++
			}
			if j >= len(runes) {
				return nil, &ShlexError{"No closing quotation"}
			}
			i = j
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

// SkillInfo describes one installed skill command
type SkillInfo struct {
	Name            string
	Description     string
	SkillDir        string
	SkillMDPath     string
	SkillIdentifier string
}

// BundleInfo describes one skill bundle
type BundleInfo struct {
	Name        string
	Slug        string
	Description string
	Skills      []string
}

// MaxStackedSkills caps the leading skill tokens one line may stack
const MaxStackedSkills = 5

var skillInvalidChars = regexp.MustCompile(`[^\w-]`)
var skillMultiHyphen = regexp.MustCompile(`-{2,}`)

// SlugifySkillName normalizes a skill or bundle name to its slash slug
func SlugifySkillName(name string) string {
	cmd := strings.ToLower(name)
	cmd = strings.ReplaceAll(cmd, " ", "-")
	cmd = strings.ReplaceAll(cmd, "_", "-")
	cmd = skillInvalidChars.ReplaceAllString(cmd, "")
	return strings.Trim(skillMultiHyphen.ReplaceAllString(cmd, "-"), "-")
}

// NormalizeSkillToken returns the canonical hyphenated slash key of a typed token
func NormalizeSkillToken(token string) string {
	return "/" + strings.ToLower(strings.ReplaceAll(strings.TrimLeft(token, "/"), "_", "-"))
}

// ResolveSlashKey maps a typed command to its /slug table key, underscores reading as hyphens
func ResolveSlashKey[T any](command string, table map[string]T) (string, bool) {
	if command == "" || table == nil {
		return "", false
	}
	exact := "/" + strings.ToLower(command)
	if _, ok := table[exact]; ok {
		return exact, true
	}
	key := "/" + strings.ToLower(strings.ReplaceAll(command, "_", "-"))
	if _, ok := table[key]; ok {
		return key, true
	}
	return "", false
}

// SplitStackedSkillCommands consumes further leading skill tokens and returns the remaining instruction
func SplitStackedSkillCommands(rest string, skills map[string]SkillInfo) ([]string, string) {
	var keys []string
	remaining := rest
	for len(keys) < MaxStackedSkills-1 {
		stripped := strings.TrimLeft(remaining, " \t\n\r")
		if !strings.HasPrefix(stripped, "/") {
			break
		}
		token, tail, _ := strings.Cut(stripped, " ")
		if fields := strings.Fields(stripped); len(fields) > 0 {
			token = fields[0]
			tail = strings.TrimSpace(stripped[len(token):])
		}
		key, ok := ResolveSlashKey(strings.TrimLeft(token, "/"), skills)
		if !ok || contains(keys, key) {
			break
		}
		keys = append(keys, key)
		remaining = tail
	}
	return keys, strings.TrimSpace(remaining)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// SkillCommandCollisionNote explains a skill whose slug a builtin command already owns
func SkillCommandCollisionNote(r *Registry, name string) string {
	slug := SlugifySkillName(name)
	if slug == "" || r.Lookup(slug) == nil {
		return ""
	}
	return "slash command /" + slug + " unavailable — name taken by built-in; use /skill " + name
}

// QuickCommand is one user defined slash shortcut from config
type QuickCommand struct {
	Type        string
	Target      string
	Description string
}

// QuickCommands reads the quick_commands mapping from config, empty when unset or malformed
func QuickCommands(cfg config.Tree) map[string]QuickCommand {
	out := map[string]QuickCommand{}
	for name, raw := range cfg.Map("quick_commands") {
		entry := config.AsMap(raw)
		if entry == nil {
			continue
		}
		var q QuickCommand
		q.Type, _ = entry["type"].(string)
		q.Target, _ = entry["target"].(string)
		q.Description, _ = entry["description"].(string)
		out[name] = q
	}
	return out
}

// AliasChainError reports a quick command alias chain that cannot resolve
type AliasChainError struct{ Code string }

func (e *AliasChainError) Error() string { return "quick command alias: " + e.Code }

// ResolveAliasChain follows alias quick commands until a builtin or exec quick command
func ResolveAliasChain(r *Registry, quick map[string]QuickCommand, name, arg string) (string, string, error) {
	seen := map[string]bool{}
	for r.Lookup(name) == nil {
		entry, ok := quick[name]
		if !ok {
			break
		}
		if seen[name] {
			return "", "", &AliasChainError{"invalid_params"}
		}
		seen[name] = true
		if entry.Type != "alias" && entry.Type != "exec" {
			return "", "", &AliasChainError{"unsupported_command"}
		}
		if entry.Type == "exec" {
			return name, arg, nil
		}
		target := strings.TrimLeft(strings.TrimSpace(entry.Target), "/")
		if strings.TrimSpace(target) == "" {
			return "", "", &AliasChainError{"invalid_params"}
		}
		parts := strings.Fields(target)
		name = parts[0]
		var joined []string
		if len(parts) > 1 {
			joined = append(joined, strings.Join(parts[1:], " "))
		}
		if arg != "" {
			joined = append(joined, arg)
		}
		arg = strings.Join(joined, " ")
	}
	return name, arg, nil
}

// RouteKind names where a slash line is sent after resolution
type RouteKind int

const (
	// RouteBuiltin runs a registered command
	RouteBuiltin RouteKind = iota
	// RouteQuickExec runs a type exec quick command through RunCLIExecQuickCommand with Exec
	RouteQuickExec
	// RouteQuickError prints Message bold red for a malformed quick command
	RouteQuickError
	// RoutePlugin runs a plugin registered slash command
	RoutePlugin
	// RouteBundle loads a skill bundle
	RouteBundle
	// RouteSkill loads one or more stacked skills
	RouteSkill
	// RouteAmbiguous reports several prefix matches
	RouteAmbiguous
	// RouteUnknown reports a slash token nothing resolves
	RouteUnknown
	// RouteRaised is the exception hermes lets escape process_command for a malformed quick_commands value
	RouteRaised
)

// Route is the resolved destination of one typed slash line
type Route struct {
	Kind        RouteKind
	Inv         *Invocation
	Exec        any
	Message     string
	Err         error
	PluginName  string
	Bundle      *BundleInfo
	BundleKey   string
	SkillKeys   []string
	Instruction string
	Candidates  []string
	Lines       [2]string
	Expanded    string
}

// RaisedError is a Python exception with its class and str
type RaisedError struct {
	Class string
	Msg   string
}

func (e *RaisedError) Error() string { return e.Msg }

// Router resolves slash lines that no builtin handles through quick commands, plugins, bundles and skills
type Router struct {
	Registry *Registry
	Config   config.Tree
	Skills   func() map[string]SkillInfo
	Bundles  func() map[string]BundleInfo
	Plugins  func() []string
}

func (rt *Router) registry() *Registry {
	if rt.Registry == nil {
		return Default
	}
	return rt.Registry
}

func (rt *Router) skills() map[string]SkillInfo {
	if rt.Skills == nil {
		return map[string]SkillInfo{}
	}
	return rt.Skills()
}

func (rt *Router) bundles() map[string]BundleInfo {
	if rt.Bundles == nil {
		return map[string]BundleInfo{}
	}
	return rt.Bundles()
}

func (rt *Router) pluginNames() map[string]bool {
	out := map[string]bool{}
	if rt.Plugins != nil {
		for _, n := range rt.Plugins() {
			out[n] = true
		}
	}
	return out
}

// Route resolves a slash line with the precedence builtin, quick, plugin, bundle, skill, prefix expansion
func (rt *Router) Route(ctx context.Context, line string) Route {
	return rt.route(ctx, line, map[string]bool{})
}

func (rt *Router) route(ctx context.Context, line string, seenQuick map[string]bool) Route {
	inv, kind := ParseWith(rt.registry(), line)
	if kind == KindSlash {
		return Route{Kind: RouteBuiltin, Inv: inv}
	}
	if kind != KindSkill {
		return Route{Kind: RouteUnknown, Inv: inv}
	}
	original := pystr.Strip(line)
	lowered := pystr.Strip(pystr.Lower(line))
	baseCmd := pystr.SplitWhitespace(lowered, -1)[0]
	bare := pystr.LstripChars(baseCmd, "/")
	userArgs := ""
	if runes, n := []rune(original), len([]rune(baseCmd)); n < len(runes) {
		userArgs = pystr.Strip(string(runes[n:]))
	}
	entry, found, err := quickCommandEntry(rt.Config, bare)
	if err != nil {
		return Route{Kind: RouteRaised, Inv: inv, Err: err}
	}
	if found {
		return rt.routeQuick(ctx, baseCmd, bare, entry, userArgs, seenQuick)
	}
	skills := rt.skills()
	bundles := rt.bundles()
	if rt.pluginNames()[bare] {
		return Route{Kind: RoutePlugin, Inv: inv, PluginName: bare}
	}
	if b, ok := bundles[baseCmd]; ok {
		return Route{Kind: RouteBundle, Inv: inv, Bundle: &b, BundleKey: baseCmd, Instruction: userArgs}
	}
	if _, ok := skills[baseCmd]; ok {
		extra, instruction := SplitStackedSkillCommands(userArgs, skills)
		return Route{Kind: RouteSkill, Inv: inv, SkillKeys: append([]string{baseCmd}, extra...), Instruction: instruction}
	}
	return rt.expandPrefix(ctx, original, lowered, skills, bundles, seenQuick)
}

// Membership and lookup of bare in the raw quick_commands value with the TypeErrors in and indexing raise
func quickCommandEntry(cfg config.Tree, bare string) (any, bool, error) {
	raw, ok := cfg.Lookup("quick_commands")
	if !ok {
		return nil, false, nil
	}
	if m := config.AsMap(raw); m != nil {
		entry, found := m[bare]
		return entry, found, nil
	}
	if s, ok := raw.(string); ok {
		if strings.Contains(s, bare) {
			return nil, false, &RaisedError{"TypeError", "string indices must be integers, not 'str'"}
		}
		return nil, false, nil
	}
	if rv := reflect.ValueOf(raw); rv.Kind() == reflect.Slice {
		for i := 0; i < rv.Len(); i++ {
			if s, ok := rv.Index(i).Interface().(string); ok && s == bare {
				return nil, false, &RaisedError{"TypeError", "list indices must be integers or slices, not str"}
			}
		}
		return nil, false, nil
	}
	return nil, false, &RaisedError{"TypeError", fmt.Sprintf("argument of type '%s' is not a container or iterable", config.PyTypeName(raw))}
}

// Alias re-dispatch, type check and command check of one quick command entry as the classic CLI runs them
func (rt *Router) routeQuick(ctx context.Context, baseCmd, bare string, entry any, userArgs string, seenQuick map[string]bool) Route {
	qcmd := config.AsMap(entry)
	if qcmd == nil {
		return Route{Kind: RouteRaised, Err: &RaisedError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", config.PyTypeName(entry))}}
	}
	qtype, _ := qcmd["type"].(string)
	if qtype == "alias" {
		target := ""
		if raw, ok := qcmd["target"]; ok {
			s, isStr := raw.(string)
			if !isStr {
				return Route{Kind: RouteRaised, Err: &RaisedError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'strip'", config.PyTypeName(raw))}}
			}
			target = pystr.Strip(s)
		}
		if target == "" {
			return Route{Kind: RouteQuickError, Message: i18n.T(ctx, "cli.quick.no_target", "", map[string]any{"command": baseCmd})}
		}
		if !strings.HasPrefix(target, "/") {
			target = "/" + target
		}
		if seenQuick[bare] {
			return Route{Kind: RouteRaised, Err: &RaisedError{"RecursionError", "maximum recursion depth exceeded"}}
		}
		seenQuick[bare] = true
		line := pystr.Strip(target + " " + userArgs)
		r := rt.route(ctx, line, seenQuick)
		r.Expanded = line
		return r
	}
	if qtype != "exec" {
		return Route{Kind: RouteQuickError, Message: i18n.T(ctx, "cli.quick.unsupported_type", "", map[string]any{"command": baseCmd})}
	}
	command, ok := qcmd["command"]
	if !ok || !config.PyTruthy(command) {
		return Route{Kind: RouteQuickError, Message: i18n.T(ctx, "cli.quick.no_command", "", map[string]any{"command": baseCmd})}
	}
	return Route{Kind: RouteQuickExec, Exec: command}
}

func (rt *Router) expandPrefix(ctx context.Context, original, lowered string, skills map[string]SkillInfo, bundles map[string]BundleInfo, seenQuick map[string]bool) Route {
	typed := pystr.SplitWhitespace(lowered, -1)[0]
	known := map[string]bool{}
	for k := range Commands(rt.registry()) {
		known[k] = true
	}
	for k := range skills {
		known[k] = true
	}
	for k := range bundles {
		known[k] = true
	}
	var matches []string
	for k := range known {
		if strings.HasPrefix(k, typed) {
			matches = append(matches, k)
		}
	}
	sort.Strings(matches)
	if len(matches) > 1 {
		if known[typed] {
			matches = []string{typed}
		} else {
			minLen := pystr.Len(matches[0])
			for _, m := range matches {
				minLen = min(minLen, pystr.Len(m))
			}
			var shortest []string
			for _, m := range matches {
				if pystr.Len(m) == minLen {
					shortest = append(shortest, m)
				}
			}
			if len(shortest) == 1 {
				matches = shortest
			}
		}
	}
	if len(matches) == 1 && matches[0] != typed {
		expanded := matches[0]
		if runes, n := []rune(original), len([]rune(typed)); n < len(runes) {
			expanded += string(runes[n:])
		}
		r := rt.route(ctx, expanded, seenQuick)
		r.Expanded = expanded
		return r
	}
	if len(matches) > 1 {
		return Route{Kind: RouteAmbiguous, Candidates: matches,
			Lines: [2]string{i18n.T(ctx, "cli.command.ambiguous", "", map[string]any{"command": lowered}),
				i18n.T(ctx, "cli.command.did_you_mean_many", "", map[string]any{"candidates": strings.Join(matches, ", ")})}}
	}
	names := make([]string, 0, len(known))
	for k := range known {
		names = append(names, k)
	}
	sort.Strings(names)
	lead, pointer := UnknownCommandLines(lowered, names)
	return Route{Kind: RouteUnknown, Lines: [2]string{lead, pointer}}
}

// UnknownCommandLines returns the lead and pointer lines for a slash token with no handler
func UnknownCommandLines(typed string, known []string) (string, string) {
	base := typed
	if fields := strings.Fields(typed); len(fields) > 0 {
		base = fields[0]
	}
	hint := ""
	if close := pydifflib.GetCloseMatches(base, known, 1, 0.6); len(close) > 0 {
		hint = " Did you mean " + close[0] + "?"
	}
	return "Unknown command " + base + " — nothing was sent." + hint, "Type /help for the full list."
}

// DestructiveSkipTokens bypass the destructive slash confirmation inline
var DestructiveSkipTokens = map[string]bool{"now": true, "--yes": true, "-y": true}

// SplitDestructiveSkip removes the command word and skip tokens and reports whether one was present
func SplitDestructiveSkip(cmdText string) (string, bool) {
	tokens := strings.Fields(cmdText)
	if len(tokens) == 0 {
		return "", false
	}
	if strings.HasPrefix(tokens[0], "/") {
		tokens = tokens[1:]
	}
	kept := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if !DestructiveSkipTokens[strings.ToLower(tok)] {
			kept = append(kept, tok)
		}
	}
	return strings.Join(kept, " "), len(kept) != len(tokens)
}
