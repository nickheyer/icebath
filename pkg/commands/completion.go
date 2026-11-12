package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
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
	Ctx                context.Context
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

func (c *SlashCompleter) ctx() context.Context {
	if c.src.Ctx == nil {
		return context.Background()
	}
	return c.src.Ctx
}

// CLI config loader the personality completer reads, assigned in wiring with the app loader
var LoadCLIConfig func(ctx context.Context) map[string]any

// Config file whose signature keys the personality memo
var getConfigPath = func(ctx context.Context) string { return home.ConfigPath(home.Dir(ctx)) }

var (
	personalitiesMu   sync.Mutex
	personalitiesMemo *personalitiesMemoEntry
)

type personalitiesMemoEntry struct {
	sig           []any
	personalities map[string]any
}

// Personalities of the CLI config memoised on config path and cache version so a disk edit reloads them
func personalitiesFromCLIConfig(ctx context.Context) map[string]any {
	cfgPath := getConfigPath(ctx)
	sig := []any{nil, nil, nil, nil, nil}
	if version, err := config.ConfigCacheVersion(ctx, cfgPath); err == nil {
		sig = append([]any{cfgPath}, version...)
	}
	personalitiesMu.Lock()
	defer personalitiesMu.Unlock()
	if personalitiesMemo == nil || !reflect.DeepEqual(personalitiesMemo.sig, sig) {
		personalitiesMemo = &personalitiesMemoEntry{sig: sig, personalities: AvailablePersonalities(config.Tree(LoadCLIConfig(ctx)))}
	}
	return personalitiesMemo.personalities
}

// Runs a completion generator and keeps what it produced before a panic ended the stream
func quiet(gen func(sub string, yield func(Completion))) func(string) []Completion {
	return func(sub string) (out []Completion) {
		defer func() { _ = recover() }()
		gen(sub, func(c Completion) { out = append(out, c) })
		return out
	}
}

// Yields every completion of a slice producing source
func yieldAll(source func(string) []Completion) func(string, func(Completion)) {
	return func(sub string, yield func(Completion)) {
		for _, c := range source(sub) {
			yield(c)
		}
	}
}

// Provider result, the zero value when the provider is unset or panics
func callProvider[T any](provider func() T) (out T) {
	if provider == nil {
		return out
	}
	defer func() {
		if recover() != nil {
			var zero T
			out = zero
		}
	}()
	return provider()
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

// None plus the configured personalities matching the typed prefix, described lazily per row
func personalityCompletions(ctx context.Context, sub string, yield func(Completion)) {
	personalities := personalitiesFromCLIConfig(ctx)
	lowered := strings.ToLower(sub)
	emit := func(name, meta string) {
		if strings.HasPrefix(name, lowered) && name != lowered {
			yield(completion(name, sub, name, meta))
		}
	}
	emit("none", "clear personality overlay")
	for _, name := range PersonalityNames(personalities) {
		emit(name, DescribePersonality(personalities[name], 50))
	}
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
	entries, err := listDir(searchDir)
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

// Directory listing the path completer reads, the real filesystem in production
var listDir = os.ReadDir

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

func (c *SlashCompleter) callSkills() map[string]SkillInfo { return callProvider(c.src.Skills) }

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

// Plugin command names and descriptions, the provider's answer else this process's plugin registry
func (c *SlashCompleter) pluginCommands() (out map[string]string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("plugin command completion unavailable", "panic", r)
			out = map[string]string{}
		}
	}()
	if c.src.PluginCommands != nil {
		if found := c.src.PluginCommands(); found != nil {
			return found
		}
	}
	out = map[string]string{}
	for name, info := range plugins.GetPluginCommands(c.ctx()) {
		out[name] = info.Description
	}
	return out
}

func (c *SlashCompleter) dynamic(base string) (func(string) []Completion, bool, bool) {
	switch base {
	case "/skin":
		return quiet(yieldAll(c.skinCompletions)), true, true
	case "/personality":
		return quiet(func(sub string, yield func(Completion)) { personalityCompletions(c.ctx(), sub, yield) }), true, true
	case "/tools":
		return quiet(yieldAll(c.toolsCompletions)), false, true
	case "/handoff":
		return quiet(yieldAll(c.handoffCompletions)), false, true
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
	parts := pystr.SplitWhitespace(text, 1)
	base := pystr.Lower(parts[0])
	if len(parts) > 1 || strings.HasSuffix(text, " ") {
		sub := ""
		if len(parts) > 1 {
			sub = parts[1]
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
	if bundles := callProvider(c.src.Bundles); bundles != nil {
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

// SlashCommandAutoSuggest ghosts slash commands and subcommands and falls back to a history suggester
type SlashCommandAutoSuggest[B, D any] struct {
	history   func(B, D) (string, bool)
	completer *SlashCompleter
}

// Suggester over an optional history source and an optional completer whose filter and skill tables it reuses
func NewSlashCommandAutoSuggest[B, D any](history func(B, D) (string, bool), completer *SlashCompleter) *SlashCommandAutoSuggest[B, D] {
	return &SlashCommandAutoSuggest[B, D]{history: history, completer: completer}
}

func (s *SlashCommandAutoSuggest[B, D]) allowed(cmd string) bool {
	return s.completer == nil || s.completer.commandAllowed(cmd)
}

func (s *SlashCommandAutoSuggest[B, D]) historySuggestion(b B, doc D) (string, bool) {
	if s.history == nil {
		return "", false
	}
	return s.history(b, doc)
}

// Ghost text for the text before the cursor, ok false when nothing is suggested
func (s *SlashCommandAutoSuggest[B, D]) GetSuggestion(b B, doc D, text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return s.historySuggestion(b, doc)
	}
	parts := pystr.SplitWhitespace(text, 1)
	base := pystr.Lower(parts[0])
	if len(parts) == 1 && !strings.HasSuffix(text, " ") {
		word := pystr.Lower(text[1:])
		reg := Default
		if s.completer != nil {
			reg = s.completer.src.Registry
		}
		names := commandsInOrder(reg)
		sort.SliceStable(names, func(i, j int) bool { return utf8.RuneCountInString(names[i]) < utf8.RuneCountInString(names[j]) })
		for _, cmd := range names {
			name := cmd[1:]
			if s.allowed(cmd) && strings.HasPrefix(name, word) && name != word {
				return name[len(word):], true
			}
		}
		return "", false
	}
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	subLower := pystr.Lower(sub)
	if s.completer != nil && s.completer.isSkillCommand(base) {
		for _, comp := range s.completer.stackedSkillCompletions(text) {
			remainder := comp.Text
			if comp.Start != 0 {
				remainder = pySliceFrom(comp.Text, -comp.Start)
			}
			if pystr.Strip(remainder) != "" {
				return remainder, true
			}
		}
	}
	if !s.allowed(base) {
		return "", false
	}
	if !strings.Contains(sub, " ") {
		reg := Default
		if s.completer != nil {
			reg = s.completer.src.Registry
		}
		for _, cand := range Subcommands(reg)[base] {
			if strings.HasPrefix(cand, subLower) && cand != subLower {
				return pySliceFrom(cand, utf8.RuneCountInString(sub)), true
			}
		}
	}
	return s.historySuggestion(b, doc)
}

// Flat command table keys in category order, a repeated key keeping its first position
func commandsInOrder(r *Registry) []string {
	var out []string
	seen := map[string]bool{}
	for _, g := range CommandsByCategory(r) {
		for _, row := range g.Rows {
			if !seen[row.Command] {
				seen[row.Command] = true
				out = append(out, row.Command)
			}
		}
	}
	return out
}

// Code point suffix from index n as Python slicing gives it, empty past the end
func pySliceFrom(s string, n int) string {
	r := []rune(s)
	if n >= len(r) {
		return ""
	}
	if n < 0 {
		n = max(len(r)+n, 0)
	}
	return string(r[n:])
}
