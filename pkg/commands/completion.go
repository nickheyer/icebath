package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Completion is one prompt completion replacing the word under the cursor
type Completion struct {
	Text    string
	Start   int
	Display string
	Meta    string
}

// NameMeta pairs a completion name with its description
type NameMeta struct {
	Name string
	Meta string
}

// ToolsetRow describes one toolset the /tools completer may offer
type ToolsetRow struct {
	Key     string
	Label   string
	Enabled bool
}

// PlatformHome names a connected gateway platform and its home channel
type PlatformHome struct {
	Name string
	Home string
}

// CompleterSources supplies the live data the slash completer reads per keystroke
type CompleterSources struct {
	Registry           *Registry
	Cfg                func() config.Tree
	Skills             func() map[string]SkillInfo
	Bundles            func() map[string]BundleInfo
	PluginCommands     func() map[string]string
	CommandAllowed     func(string) bool
	Skins              func() []NameMeta
	ToolsetRows        func() []ToolsetRow
	ConnectedPlatforms func() []PlatformHome
}

// PickerCommands get no trailing space on exact match because a space would block the picker
var PickerCommands = map[string]bool{"model": true, "skin": true, "personality": true}

// StaticContextRefs are the fixed @ references offered before path and fuzzy file matches
var StaticContextRefs = []NameMeta{
	{"@diff", "Git working tree diff"},
	{"@staged", "Git staged diff"},
	{"@file:", "Attach a file"},
	{"@folder:", "Attach a folder"},
	{"@git:", "Git log with diffs (e.g. @git:5)"},
	{"@url:", "Fetch web content"},
}

// SlashCompleter completes slash commands, subcommands, skill commands, and @ references
type SlashCompleter struct {
	src       CompleterSources
	mu        sync.Mutex
	files     []string
	filesAt   time.Time
	filesCWD  string
	fileTTL   time.Duration
	timeoutMS int
}

// NewCompleter builds a completer over the given sources
func NewCompleter(src CompleterSources) *SlashCompleter {
	if src.Registry == nil {
		src.Registry = Default
	}
	return &SlashCompleter{src: src, fileTTL: 5 * time.Second, timeoutMS: 2000}
}

func completion(text, word, display, meta string) Completion {
	return Completion{Text: text, Start: -len([]rune(word)), Display: display, Meta: meta}
}

func prefixCompletions(rows []NameMeta, partial string, skipExact bool) []Completion {
	lowered := strings.ToLower(partial)
	var out []Completion
	for _, r := range rows {
		if strings.HasPrefix(r.Name, lowered) && !(skipExact && r.Name == lowered) {
			out = append(out, completion(r.Name, partial, r.Name, r.Meta))
		}
	}
	return out
}

// Splits completed words from the partial word, a trailing space meaning a fresh word
func splitArgs(sub string) ([]string, string) {
	parts := strings.Fields(sub)
	if strings.HasSuffix(sub, " ") || len(parts) == 0 {
		return parts, ""
	}
	return parts[:len(parts)-1], parts[len(parts)-1]
}

// Compact human readable file size or empty on error
func fileSizeLabel(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	size := st.Size()
	switch {
	case size < 1024:
		return fmt.Sprintf("%dB", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.0fK", float64(size)/1024)
	case size < 1024*1024*1024:
		return fmt.Sprintf("%.1fM", float64(size)/(1024*1024))
	}
	return fmt.Sprintf("%.1fG", float64(size)/(1024*1024*1024))
}

func (c *SlashCompleter) cfg() config.Tree {
	if c.src.Cfg == nil {
		return config.Tree{}
	}
	return c.src.Cfg()
}

func (c *SlashCompleter) skinCompletions(sub string) []Completion {
	if c.src.Skins == nil {
		return nil
	}
	return prefixCompletions(c.src.Skins(), sub, true)
}

func (c *SlashCompleter) personalityCompletions(sub string) []Completion {
	rows := []NameMeta{{"none", "clear personality overlay"}}
	personalities := AvailablePersonalities(c.cfg())
	for _, name := range PersonalityNames(personalities) {
		rows = append(rows, NameMeta{name, DescribePersonality(personalities[name], 50)})
	}
	return prefixCompletions(rows, sub, true)
}

func (c *SlashCompleter) toolsCompletions(sub string) []Completion {
	completed, partial := splitArgs(sub)
	if len(completed) == 0 {
		return prefixCompletions([]NameMeta{{"list", ""}, {"disable", ""}, {"enable", ""}}, partial, true)
	}
	subcommand := strings.ToLower(completed[0])
	if subcommand != "enable" && subcommand != "disable" {
		return nil
	}
	already := map[string]bool{}
	for _, w := range completed[1:] {
		already[w] = true
	}
	wantEnabled := subcommand != "enable"
	var rows []NameMeta
	if c.src.ToolsetRows != nil {
		for _, r := range c.src.ToolsetRows() {
			if r.Enabled == wantEnabled {
				rows = append(rows, NameMeta{r.Key, r.Label})
			}
		}
	}
	servers := c.cfg().Map("mcp_servers")
	for _, srv := range sortedKeys(servers) {
		rows = append(rows, NameMeta{srv + ":", "MCP server '" + srv + "'"})
	}
	var filtered []NameMeta
	for _, r := range rows {
		if !already[r.Name] {
			filtered = append(filtered, r)
		}
	}
	return prefixCompletions(filtered, partial, false)
}

func (c *SlashCompleter) handoffCompletions(sub string) []Completion {
	completed, partial := splitArgs(sub)
	if len(completed) > 0 || c.src.ConnectedPlatforms == nil {
		return nil
	}
	var out []Completion
	for _, p := range c.src.ConnectedPlatforms() {
		if !strings.HasPrefix(p.Name, strings.ToLower(partial)) {
			continue
		}
		meta := "send this session here"
		if p.Home != "" {
			meta = "→ " + p.Home
		}
		out = append(out, completion(p.Name, partial, p.Name, meta))
	}
	return out
}

// Word under the cursor when it holds a slash and no URL scheme
func extractPathWord(text string) (string, bool) {
	word := text[strings.LastIndex(text, " ")+1:]
	if word != "" && !strings.Contains(word, "://") && strings.Contains(word, "/") {
		return word, true
	}
	return "", false
}

func dirCompletions(expanded, word string, limit int, textFor func(string) string, wantDir *bool) []Completion {
	var searchDir, prefix string
	if strings.HasSuffix(expanded, "/") {
		searchDir, prefix = expanded, ""
	} else {
		searchDir, prefix = filepath.Dir(expanded), filepath.Base(expanded)
		if searchDir == "" {
			searchDir = "."
		}
	}
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	prefixLower := strings.ToLower(prefix)
	var out []Completion
	for _, entry := range names {
		if !strings.HasPrefix(strings.ToLower(entry), prefixLower) {
			continue
		}
		full := filepath.Join(searchDir, entry)
		st, err := os.Stat(full)
		isDir := err == nil && st.IsDir()
		if wantDir != nil && *wantDir != isDir {
			continue
		}
		if len(out) >= limit {
			break
		}
		suffix := ""
		meta := fileSizeLabel(full)
		if isDir {
			suffix, meta = "/", "dir"
		}
		out = append(out, completion(textFor(full)+suffix, word, entry+suffix, meta))
	}
	return out
}

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return home + p[1:]
		}
	}
	return p
}

func relPath(target string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return target
	}
	rel, err := filepath.Rel(cwd, target)
	if err != nil {
		return target
	}
	return rel
}

// Path completions keeping the typed style of tilde, absolute, or relative
func pathCompletions(word string, limit int) []Completion {
	var textFor func(string) string
	switch {
	case strings.HasPrefix(word, "~"):
		home, _ := os.UserHomeDir()
		textFor = func(fp string) string {
			rel, err := filepath.Rel(home, fp)
			if err != nil {
				return fp
			}
			return "~/" + rel
		}
	case filepath.IsAbs(word):
		textFor = func(fp string) string { return fp }
	default:
		textFor = relPath
	}
	return dirCompletions(expandUser(word), word, limit, textFor, nil)
}

// Scores a file path against a fuzzy query, higher better, zero no match, one empty query
func ScorePath(filePath, query string) int {
	if query == "" {
		return 1
	}
	lowerFile := strings.ToLower(filepath.Base(filePath))
	lowerQ := strings.ToLower(query)
	switch {
	case lowerFile == lowerQ:
		return 100
	case strings.HasPrefix(lowerFile, lowerQ):
		return 80
	case strings.Contains(lowerFile, lowerQ):
		return 60
	case strings.Contains(strings.ToLower(filePath), lowerQ):
		return 40
	}
	qr := []rune(lowerQ)
	qi, boundary := 0, 0
	prev := '_'
	for _, ch := range lowerFile {
		if qi < len(qr) && ch == qr[qi] {
			if strings.ContainsRune("_-./", prev) {
				boundary++
			}
			qi++
		}
		prev = ch
	}
	if qi < len(qr) {
		return 0
	}
	if float64(boundary) >= float64(len(qr))*0.5 {
		return 35
	}
	return 25
}

func (c *SlashCompleter) callSkills() map[string]SkillInfo {
	if c.src.Skills == nil {
		return nil
	}
	return c.src.Skills()
}

func (c *SlashCompleter) commandAllowed(cmd string) bool {
	return c.src.CommandAllowed == nil || c.src.CommandAllowed(cmd)
}

func (c *SlashCompleter) isSkillCommand(token string) bool {
	_, ok := ResolveSlashKey(strings.TrimLeft(token, "/"), c.callSkills())
	return ok
}

// Skill completions for a stacked chain while every completed token is a distinct skill
func (c *SlashCompleter) stackedSkillCompletions(text string) []Completion {
	completed, current := splitArgs(text)
	skills := c.callSkills()
	seen := map[string]bool{}
	for _, token := range completed {
		key, ok := ResolveSlashKey(strings.TrimLeft(token, "/"), skills)
		if !ok || seen[key] {
			return nil
		}
		seen[key] = true
	}
	if len(seen) >= MaxStackedSkills || !strings.HasPrefix(current, "/") {
		return nil
	}
	wordKey := NormalizeSkillToken(current)
	var out []Completion
	for _, cmd := range sortedKeys(skills) {
		norm := NormalizeSkillToken(cmd)
		if seen[cmd] || !strings.HasPrefix(norm, wordKey) {
			continue
		}
		textOut := cmd
		if norm == wordKey {
			textOut = cmd + " "
		}
		desc := skills[cmd].Description
		if desc == "" {
			desc = "Skill command"
		}
		out = append(out, completion(textOut, current, cmd, "⚡ "+desc))
	}
	return out
}

// Replacement text, exact matches gaining a trailing space except picker commands
func completionText(cmdName, word string) string {
	if cmdName == word && !PickerCommands[cmdName] {
		return cmdName + " "
	}
	return cmdName
}

func extractContextWord(text string) (string, bool) {
	word := text[strings.LastIndex(text, " ")+1:]
	return word, strings.HasPrefix(word, "@")
}

func (c *SlashCompleter) contextCompletions(word string, limit int) []Completion {
	lowered := strings.ToLower(word)
	var out []Completion
	for _, ref := range StaticContextRefs {
		if strings.HasPrefix(ref.Name, lowered) && ref.Name != lowered {
			out = append(out, completion(ref.Name, word, ref.Name, ref.Meta))
		}
	}
	for _, prefix := range []string{"@file:", "@folder:"} {
		bare := prefix[:len(prefix)-1]
		if word == bare || strings.HasPrefix(word, prefix) {
			expanded := ""
			if word != bare {
				expanded = expandUser(word[len(prefix):])
			}
			if expanded == "" || expanded == "." {
				expanded = "./"
			}
			wantDir := prefix == "@folder:"
			p := prefix
			return append(out, dirCompletions(expanded, word, limit, func(fp string) string { return p + relPath(fp) }, &wantDir)...)
		}
	}
	return append(out, c.fuzzyFileCompletions(word, word[1:], limit)...)
}

// Cached project file list with a five second TTL from rg then fd
func (c *SlashCompleter) projectFiles() []string {
	cwd, _ := os.Getwd()
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.files) > 0 && c.filesCWD == cwd && now.Sub(c.filesAt) < c.fileTTL {
		return c.files
	}
	var files []string
	for _, argv := range [][]string{
		{"rg", "--files", "--sortr=modified", cwd},
		{"rg", "--files", cwd},
		{"fd", "--type", "f", "--base-directory", cwd},
	} {
		if _, err := exec.LookPath(argv[0]); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.timeoutMS)*time.Millisecond)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = cwd
		outBytes, err := cmd.Output()
		cancel()
		if err != nil || strings.TrimSpace(string(outBytes)) == "" {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(outBytes)), "\n")
		if len(lines) > 5000 {
			lines = lines[:5000]
		}
		for _, p := range lines {
			if filepath.IsAbs(p) {
				rel, err := filepath.Rel(cwd, p)
				if err != nil {
					continue
				}
				p = rel
			}
			files = append(files, p)
		}
		break
	}
	c.files, c.filesAt, c.filesCWD = files, now, cwd
	return files
}

func (c *SlashCompleter) fuzzyFileCompletions(word, query string, limit int) []Completion {
	files := c.projectFiles()
	if query != "" {
		type scored struct {
			s  int
			fp string
		}
		var hits []scored
		for _, fp := range files {
			if s := ScorePath(fp, query); s > 0 {
				hits = append(hits, scored{s, fp})
			}
		}
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].s != hits[j].s {
				return hits[i].s > hits[j].s
			}
			return hits[i].fp < hits[j].fp
		})
		files = files[:0:0]
		for _, h := range hits {
			files = append(files, h.fp)
		}
	}
	if len(files) > limit {
		files = files[:limit]
	}
	cwd, _ := os.Getwd()
	var out []Completion
	for _, fp := range files {
		isDir := strings.HasSuffix(fp, "/")
		meta := "dir"
		kind := "folder"
		if !isDir {
			meta = fileSizeLabel(filepath.Join(cwd, fp))
			kind = "file"
		}
		if query != "" {
			if meta != "" {
				meta = fp + "  " + meta
			} else {
				meta = fp
			}
		}
		out = append(out, completion("@"+kind+":"+fp, word, filepath.Base(fp), meta))
	}
	return out
}

func (c *SlashCompleter) pluginCommands() map[string]string {
	if c.src.PluginCommands != nil {
		if found := c.src.PluginCommands(); found != nil {
			return found
		}
	}
	out := map[string]string{}
	for _, d := range PluginEntries(c.src.Registry) {
		out[d.Name] = d.Description
	}
	return out
}

func (c *SlashCompleter) dynamic(base string) (func(string) []Completion, bool, bool) {
	switch base {
	case "/skin":
		return c.skinCompletions, true, true
	case "/personality":
		return c.personalityCompletions, true, true
	case "/tools":
		return c.toolsCompletions, false, true
	case "/handoff":
		return c.handoffCompletions, false, true
	}
	return nil, false, false
}

// Completions for the text before the cursor
func (c *SlashCompleter) Complete(text string) []Completion {
	if !strings.HasPrefix(text, "/") {
		if word, ok := extractContextWord(text); ok {
			return c.contextCompletions(word, 30)
		}
		if word, ok := extractPathWord(text); ok {
			return pathCompletions(word, 30)
		}
		return nil
	}
	parts := strings.SplitN(text, " ", 2)
	base := strings.ToLower(parts[0])
	if len(parts) > 1 || strings.HasSuffix(text, " ") {
		sub := ""
		if len(parts) > 1 {
			sub = strings.TrimLeft(parts[1], " ")
		}
		firstArg := !strings.Contains(sub, " ")
		if c.isSkillCommand(base) {
			return c.stackedSkillCompletions(text)
		}
		handler, singleWord, ok := c.dynamic(base)
		if ok && (!singleWord || firstArg) {
			return handler(sub)
		}
		subs := Subcommands(c.src.Registry)
		if firstArg && c.commandAllowed(base) {
			if list, ok := subs[base]; ok {
				rows := make([]NameMeta, len(list))
				for i, s := range list {
					rows[i] = NameMeta{Name: s}
				}
				return prefixCompletions(rows, sub, true)
			}
		}
		return nil
	}
	word := text[1:]
	var out []Completion
	cmdCompletion := func(name, meta string) Completion {
		return completion(completionText(name, word), word, "/"+name, meta)
	}
	for _, g := range CommandsByCategory(c.src.Registry) {
		for _, row := range g.Rows {
			if c.commandAllowed(row.Command) && strings.HasPrefix(row.Command[1:], word) {
				out = append(out, cmdCompletion(row.Command[1:], row.Description))
			}
		}
	}
	if c.src.Bundles != nil {
		bundles := c.src.Bundles()
		for _, cmd := range sortedKeys(bundles) {
			if strings.HasPrefix(cmd[1:], word) {
				info := bundles[cmd]
				desc := info.Description
				if desc == "" {
					desc = "Skill bundle"
				}
				out = append(out, cmdCompletion(cmd[1:], fmt.Sprintf("▣ %s (%d skills)", desc, len(info.Skills))))
			}
		}
	}
	skills := c.callSkills()
	for _, cmd := range sortedKeys(skills) {
		if strings.HasPrefix(cmd[1:], word) {
			desc := skills[cmd].Description
			if desc == "" {
				desc = "Skill command"
			}
			out = append(out, cmdCompletion(cmd[1:], "⚡ "+desc))
		}
	}
	plugins := c.pluginCommands()
	for _, name := range sortedKeys(plugins) {
		if strings.HasPrefix(name, word) {
			desc := plugins[name]
			if desc == "" {
				desc = "Plugin command"
			}
			out = append(out, cmdCompletion(name, "🔌 "+desc))
		}
	}
	return out
}

// Inline ghost text for the slash command or subcommand being typed, empty when none
func (c *SlashCompleter) Suggest(text string) string {
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	parts := strings.SplitN(text, " ", 2)
	base := strings.ToLower(parts[0])
	if len(parts) == 1 && !strings.HasSuffix(text, " ") {
		word := strings.ToLower(text[1:])
		names := CommandNames(c.src.Registry)
		sort.SliceStable(names, func(i, j int) bool { return len(names[i]) < len(names[j]) })
		for _, cmd := range names {
			name := cmd[1:]
			if c.commandAllowed(cmd) && strings.HasPrefix(name, word) && name != word {
				return name[len(word):]
			}
		}
		return ""
	}
	sub := ""
	if len(parts) > 1 {
		sub = strings.TrimLeft(parts[1], " ")
	}
	subLower := strings.ToLower(sub)
	if c.isSkillCommand(base) {
		for _, comp := range c.stackedSkillCompletions(text) {
			remainder := comp.Text
			if comp.Start < 0 {
				r := []rune(comp.Text)
				if -comp.Start <= len(r) {
					remainder = string(r[-comp.Start:])
				}
			}
			if strings.TrimSpace(remainder) != "" {
				return remainder
			}
		}
	}
	if !c.commandAllowed(base) {
		return ""
	}
	if !strings.Contains(sub, " ") {
		for _, s := range Subcommands(c.src.Registry)[base] {
			if strings.HasPrefix(s, subLower) && s != subLower {
				return s[len(sub):]
			}
		}
	}
	return ""
}
