package commands

import (
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
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
	Name        string
	Type        string
	Target      string
	Command     string
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
		q := QuickCommand{Name: name}
		q.Type, _ = entry["type"].(string)
		q.Target, _ = entry["target"].(string)
		q.Command, _ = entry["command"].(string)
		q.Description, _ = entry["description"].(string)
		out[name] = q
	}
	return out
}

// AliasTarget rewrites an alias quick command into its target line, false without a target
func (q QuickCommand) AliasTarget(args string) (string, bool) {
	target := strings.TrimSpace(q.Target)
	if target == "" {
		return "", false
	}
	if !strings.HasPrefix(target, "/") {
		target = "/" + target
	}
	return strings.TrimSpace(target + " " + strings.TrimSpace(args)), true
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
	// RouteQuickExec runs a type exec quick command shell snippet
	RouteQuickExec
	// RouteQuickError reports a malformed quick command with its message
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
)

// Route is the resolved destination of one typed slash line
type Route struct {
	Kind        RouteKind
	Inv         *Invocation
	Quick       *QuickCommand
	Message     string
	PluginName  string
	Bundle      *BundleInfo
	BundleKey   string
	SkillKeys   []string
	Instruction string
	Candidates  []string
	Lines       [2]string
	Expanded    string
}

// Router resolves slash lines that no builtin handles through quick commands, plugins, bundles and skills
type Router struct {
	Registry *Registry
	Config   config.Tree
	Skills   func() map[string]SkillInfo
	Bundles  func() map[string]BundleInfo
	Plugins  func() []string
	Surface  string
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
func (rt *Router) Route(line string) Route {
	return rt.route(line, map[string]bool{})
}

func (rt *Router) route(line string, seenQuick map[string]bool) Route {
	inv, kind := ParseWith(rt.registry(), line)
	if kind == KindSlash {
		return Route{Kind: RouteBuiltin, Inv: inv}
	}
	if kind != KindSkill {
		return Route{Kind: RouteUnknown, Inv: inv}
	}
	original := strings.TrimSpace(line)
	lowered := strings.ToLower(original)
	baseCmd := strings.Fields(lowered)[0]
	bare := strings.TrimLeft(baseCmd, "/")
	userArgs := strings.TrimSpace(original[len(baseCmd):])
	quick := QuickCommands(rt.Config)
	if q, ok := quick[bare]; ok {
		return rt.routeQuick(baseCmd, q, userArgs, seenQuick)
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
	return rt.expandPrefix(original, lowered, skills, bundles, seenQuick)
}

func (rt *Router) routeQuick(baseCmd string, q QuickCommand, userArgs string, seenQuick map[string]bool) Route {
	msgs := quickMessages(rt.Surface)
	switch q.Type {
	case "alias":
		target, ok := q.AliasTarget(userArgs)
		if !ok {
			return Route{Kind: RouteQuickError, Quick: &q, Message: msgs.noTarget(baseCmd)}
		}
		if seenQuick[q.Name] {
			return Route{Kind: RouteQuickError, Quick: &q, Message: msgs.noTarget(baseCmd)}
		}
		seenQuick[q.Name] = true
		r := rt.route(target, seenQuick)
		r.Expanded = target
		return r
	case "exec":
		if q.Command == "" {
			return Route{Kind: RouteQuickError, Quick: &q, Message: msgs.noCommand(baseCmd)}
		}
		return Route{Kind: RouteQuickExec, Quick: &q}
	}
	return Route{Kind: RouteQuickError, Quick: &q, Message: msgs.unsupported(baseCmd)}
}

func (rt *Router) expandPrefix(original, lowered string, skills map[string]SkillInfo, bundles map[string]BundleInfo, seenQuick map[string]bool) Route {
	typed := strings.Fields(lowered)[0]
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
			minLen := len(matches[0])
			for _, m := range matches {
				if len(m) < minLen {
					minLen = len(m)
				}
			}
			var shortest []string
			for _, m := range matches {
				if len(m) == minLen {
					shortest = append(shortest, m)
				}
			}
			if len(shortest) == 1 {
				matches = shortest
			}
		}
	}
	if len(matches) == 1 && matches[0] != typed {
		expanded := matches[0] + strings.TrimSpace(original)[len(typed):]
		r := rt.route(expanded, seenQuick)
		r.Expanded = expanded
		return r
	}
	if len(matches) > 1 {
		return Route{Kind: RouteAmbiguous, Candidates: matches,
			Lines: [2]string{"Ambiguous command: " + lowered, "Did you mean: " + strings.Join(matches, ", ") + "?"}}
	}
	names := make([]string, 0, len(known))
	for k := range known {
		names = append(names, k)
	}
	sort.Strings(names)
	lead, pointer := UnknownCommandLines(lowered, names)
	return Route{Kind: RouteUnknown, Lines: [2]string{lead, pointer}}
}

type quickText struct {
	noTarget, unsupported, noCommand func(string) string
	NoOutput, TimedOut               string
	Error                            func(string) string
}

func quickMessages(surface string) quickText {
	if surface == "gateway" {
		return quickText{
			noTarget:    func(c string) string { return "Quick command '/" + strings.TrimLeft(c, "/") + "' has no target defined." },
			unsupported: func(c string) string { return "Quick command '/" + strings.TrimLeft(c, "/") + "' has unsupported type (supported: 'exec', 'alias')." },
			noCommand:   func(c string) string { return "Quick command '/" + strings.TrimLeft(c, "/") + "' has no command defined." },
			NoOutput:    "Command returned no output.",
			TimedOut:    "Quick command timed out (30s).",
			Error:       func(e string) string { return "Quick command error: " + e },
		}
	}
	return quickText{
		noTarget:    func(c string) string { return "Quick command '" + c + "' has no target defined" },
		unsupported: func(c string) string { return "Quick command '" + c + "' has unsupported type (supported: 'exec', 'alias')" },
		noCommand:   func(c string) string { return "Quick command '" + c + "' has no command defined" },
		NoOutput:    "Command returned no output",
		TimedOut:    "Quick command timed out (30s)",
		Error:       func(e string) string { return "Quick command error: " + e },
	}
}

// UnknownCommandLines returns the lead and pointer lines for a slash token with no handler
func UnknownCommandLines(typed string, known []string) (string, string) {
	base := typed
	if fields := strings.Fields(typed); len(fields) > 0 {
		base = fields[0]
	}
	hint := ""
	if close := CloseMatches(base, known, 1, 0.6); len(close) > 0 {
		hint = " Did you mean " + close[0] + "?"
	}
	return "Unknown command " + base + " — nothing was sent." + hint, "Type /help for the full list."
}

// CloseMatches returns the best fuzzy matches above a cutoff the way difflib ranks them
func CloseMatches(word string, possibilities []string, n int, cutoff float64) []string {
	type scored struct {
		score float64
		text  string
	}
	var results []scored
	for _, x := range possibilities {
		if score := Ratio(x, word); score >= cutoff {
			results = append(results, scored{score, x})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].text > results[j].text
	})
	if len(results) > n {
		results = results[:n]
	}
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.text)
	}
	return out
}

// Ratio is the difflib similarity of two strings, matched characters twice over the total length
func Ratio(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	total := len(ra) + len(rb)
	if total == 0 {
		return 1.0
	}
	return 2.0 * float64(matchingChars(ra, rb)) / float64(total)
}

func matchingChars(a, b []rune) int {
	b2j := map[rune][]int{}
	for j, r := range b {
		b2j[r] = append(b2j[r], j)
	}
	type span struct{ alo, ahi, blo, bhi int }
	queue := []span{{0, len(a), 0, len(b)}}
	matches := 0
	for len(queue) > 0 {
		s := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		i, j, k := longestMatch(a, b2j, s.alo, s.ahi, s.blo, s.bhi)
		if k == 0 {
			continue
		}
		matches += k
		if s.alo < i && s.blo < j {
			queue = append(queue, span{s.alo, i, s.blo, j})
		}
		if i+k < s.ahi && j+k < s.bhi {
			queue = append(queue, span{i + k, s.ahi, j + k, s.bhi})
		}
	}
	return matches
}

func longestMatch(a []rune, b2j map[rune][]int, alo, ahi, blo, bhi int) (int, int, int) {
	besti, bestj, bestsize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}
	return besti, bestj, bestsize
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
