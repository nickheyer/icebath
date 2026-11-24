package agentimport

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"

	"github.com/nickheyer/icebath/pkg/internal/textutil"
)

// EntryDelimiter separates entries of the memory store files
const EntryDelimiter = memory.EntryDelimiter

// MemoryCharLimit is the character budget of a merged memory file
const MemoryCharLimit = 20_000

// SupportedAgents are the coding agents the directory importer reads
var SupportedAgents = []string{"claude-code", "codex", "cursor"}

// AgentDefaultDirs maps each supported agent to its config dir under the user home
var AgentDefaultDirs = map[string]string{"claude-code": ".claude", "codex": ".codex", "cursor": ".cursor"}

var skillCategory = map[string]string{"claude-code": "claude-code-imports", "codex": "codex-imports", "cursor": "cursor-imports"}

var (
	frontmatterRe  = pyre.MustCompile(`(?s)\A---[ \t]*\r?\n.*?\r?\n---[ \t]*(?:\r?\n|\Z)`)
	secretKeyRe    = pyre.MustCompile(`(?i)(?:^|_)(?:API[_-]?KEY|APIKEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIALS?|AUTH|PRIVATE[_-]?KEY|ACCESS[_-]?KEY)(?:_|$)|KEY$`)
	headingRe      = pyre.MustCompile(`^(#{1,6})\s+(.*\S)\s*$`)
	bulletRe       = pyre.MustCompile(`^\s*(?:[-*]|\d+\.)\s+(.*\S)\s*$`)
	contextFileRe  = pyre.MustCompile(`(?i)\b(MEMORY|USER|SOUL|AGENTS|TOOLS|IDENTITY|CLAUDE)\.md\b`)
	bashRuleRe     = pyre.MustCompile(`^Bash\((?P<inner>.*)\)$`)
	credentialName = map[string]bool{".credentials.json": true, "auth.json": true, "credentials.json": true}
)

// Reports whether a regexp2 pattern finds a match, a match error counting as none
func reMatch(re interface {
	MatchString(string) (bool, error)
}, s string) bool {
	ok, err := re.MatchString(s)
	return err == nil && ok
}

// IsSecretKey reports whether an env var name looks like a credential
func IsSecretKey(key string) bool { return reMatch(secretKeyRe, key) }

// StripFrontmatter drops a leading YAML frontmatter block holding routing metadata
func StripFrontmatter(text string) string {
	out, err := frontmatterRe.Replace(text, "", -1, 1)
	if err != nil {
		return text
	}
	return out
}

// NormalizeText collapses whitespace runs to one space and lowercases
func NormalizeText(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(text)), " "))
}

// Reads a file as utf-8-sig text with undecodable bytes replaced and line endings unified like text mode
func readText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return pystr.TranslateNewlines(pystr.DecodeUTF8SigReplace(raw)), nil
}

// Writes text through a temp file and rename, an existing file keeping its mode and a link its target
func atomicWriteText(ctx context.Context, path, content string) error {
	return pyfs.AtomicWriteTextWith(ctx, path, content, pyfs.TextWrite{Prefix: ".tmp_", PreserveMode: true})
}

// ConfigReadError is an existing config.yaml that cannot be read or parsed, so a merge must not replace it
type ConfigReadError struct{ Err error }

func (e *ConfigReadError) Error() string { return e.Err.Error() }

func (e *ConfigReadError) Unwrap() error { return e.Err }

// Mapping of config.yaml for a merge, empty when absent and a ConfigReadError when unreadable
func loadConfigFile(ctx context.Context, path string) (map[string]any, error) {
	m, err := config.RequireReadableConfigBeforeWrite(ctx, path)
	if err != nil {
		return nil, &ConfigReadError{Err: err}
	}
	return m, nil
}

// Replaces config.yaml with the whole merged mapping, comments kept
func writeConfigFile(ctx context.Context, targetRoot, path string, data map[string]any) error {
	return config.AtomicConfigReplace(ctx, targetRoot, path, data, "")
}

// ExtractMarkdownEntries splits markdown into memory entries with headings as context prefixes, code and tables skipped
func ExtractMarkdownEntries(text string) []string {
	var entries, headings, paragraph []string
	addEntry := func(content string) {
		var kept []string
		for _, h := range headings {
			if h != "" && !reMatch(contextFileRe, h) {
				kept = append(kept, h)
			}
		}
		if prefix := strings.Join(kept, " > "); prefix != "" {
			entries = append(entries, prefix+": "+content)
		} else {
			entries = append(entries, content)
		}
	}
	flush := func() {
		parts := make([]string, len(paragraph))
		for i, l := range paragraph {
			parts[i] = strings.TrimSpace(l)
		}
		block := strings.TrimSpace(strings.Join(parts, " "))
		paragraph = paragraph[:0]
		if block != "" {
			addEntry(block)
		}
	}
	inCode := false
	for _, raw := range textutil.Lines(text) {
		line := strings.TrimRightFunc(raw, unicode.IsSpace)
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "```") {
			inCode = !inCode
			flush()
			continue
		}
		if inCode {
			continue
		}
		if m, _ := headingRe.FindStringMatch(stripped); m != nil {
			flush()
			level := utf8.RuneCountInString(m.GroupByNumber(1).String())
			if level-1 < len(headings) {
				headings = headings[:level-1]
			}
			headings = append(headings, strings.TrimSpace(m.GroupByNumber(2).String()))
			continue
		}
		if m, _ := bulletRe.FindStringMatch(line); m != nil {
			flush()
			addEntry(strings.TrimSpace(m.GroupByNumber(1).String()))
			continue
		}
		if stripped == "" || (strings.HasPrefix(stripped, "|") && strings.HasSuffix(stripped, "|")) {
			flush()
			continue
		}
		paragraph = append(paragraph, stripped)
	}
	flush()
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		n := NormalizeText(e)
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, strings.TrimSpace(e))
		}
	}
	return out
}

// Entries of the destination memory store split on the store delimiter
func parseExistingMemoryEntries(path string) ([]string, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	text, err := readText(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range strings.Split(text, EntryDelimiter) {
		if s := strings.TrimSpace(e); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// Counts of one memory merge
type mergeStats struct{ existing, added, duplicates, overflowed int }

// Appends new deduplicated entries while the joined store fits the limit
func mergeEntries(existing, incoming []string, limit int) ([]string, mergeStats) {
	merged := append([]string(nil), existing...)
	seen := map[string]bool{}
	for _, e := range existing {
		if strings.TrimSpace(e) != "" {
			seen[NormalizeText(e)] = true
		}
	}
	stats := mergeStats{existing: len(existing)}
	delim := utf8.RuneCountInString(EntryDelimiter)
	current := utf8.RuneCountInString(strings.Join(merged, EntryDelimiter))
	for _, entry := range incoming {
		n := NormalizeText(entry)
		if n == "" {
			continue
		}
		if seen[n] {
			stats.duplicates++
			continue
		}
		size := utf8.RuneCountInString(entry)
		candidate := size
		if len(merged) > 0 {
			candidate = current + delim + size
		}
		if candidate > limit {
			stats.overflowed++
			continue
		}
		merged = append(merged, entry)
		seen[n] = true
		current = candidate
		stats.added++
	}
	return merged, stats
}

// ClaudeRuleToCommandPattern maps a Bash rule to its command pattern, a trailing prefix marker becoming a star, else empty
func ClaudeRuleToCommandPattern(rule string) string {
	m, _ := bashRuleRe.FindStringMatch(strings.TrimSpace(rule))
	if m == nil {
		return ""
	}
	inner := strings.TrimSpace(m.GroupByNumber(1).String())
	if strings.HasSuffix(inner, ":*") {
		return inner[:len(inner)-2] + "*"
	}
	return inner
}

// User home the importers resolve default dirs against
func userHome() string {
	h, err := home.UserHome()
	if err != nil {
		return ""
	}
	return h
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// DetectAgents lists the supported agents whose default dir exists
func DetectAgents() []string {
	h := userHome()
	var out []string
	for _, a := range SupportedAgents {
		if isDir(filepath.Join(h, AgentDefaultDirs[a])) {
			out = append(out, a)
		}
	}
	return out
}

// SanitizeMCPEnv splits an MCP env mapping into kept values and the stripped secret names
func SanitizeMCPEnv(env any) (*pyjson.Object, []string) {
	kept := pyjson.NewObject()
	o, ok := env.(*pyjson.Object)
	if !ok || o == nil {
		return kept, nil
	}
	var stripped []string
	for _, k := range o.Keys {
		if IsSecretKey(k) {
			stripped = append(stripped, k)
		} else {
			kept.Set(k, o.Vals[k])
		}
	}
	return kept, stripped
}

// Python truthiness of a decoded JSON or TOML value
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case *big.Int:
		return t.Sign() != 0
	case []any:
		return len(t) > 0
	case *pyjson.Object:
		return t != nil && t.Len() > 0
	case map[string]any:
		return len(t) > 0
	}
	return pyrepr.Truthy(v)
}

// Value of a key of a decoded object, nil when absent or not an object
func get(v any, key string) any {
	if o, ok := v.(*pyjson.Object); ok && o != nil {
		return o.Vals[key]
	}
	return nil
}

func asObject(v any) (*pyjson.Object, bool) {
	o, ok := v.(*pyjson.Object)
	return o, ok && o != nil
}

// Python str of a decoded value
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Config value of a decoded value, objects becoming ordered config mappings
func toConfig(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		m := map[string]any{}
		for _, k := range t.Keys {
			m[k] = toConfig(t.Vals[k])
		}
		config.SetMappingKeys(m, append([]string(nil), t.Keys...))
		return m
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = toConfig(item)
		}
		return out
	case int64:
		return int(t)
	}
	return v
}

// Sets a key of a config mapping, a new key appended to its order
func setOrdered(m map[string]any, key string, v any) {
	_, had := m[key]
	m[key] = v
	if !had {
		config.AppendMappingKey(m, key)
	}
}

// Maps one Claude or Codex MCP server entry to the config shape with the stripped secret paths
func translateMCPServer(name string, srv *pyjson.Object) (map[string]any, []string) {
	out := map[string]any{}
	var stripped []string
	if truthy(srv.Vals["command"]) {
		setOrdered(out, "command", toConfig(srv.Vals["command"]))
		if truthy(srv.Vals["args"]) {
			setOrdered(out, "args", toConfig(srv.Vals["args"]))
		}
		kept, gone := SanitizeMCPEnv(srv.Vals["env"])
		if kept.Len() > 0 {
			setOrdered(out, "env", toConfig(kept))
		}
		for _, k := range gone {
			stripped = append(stripped, fmt.Sprintf("mcp_servers.%s.env.%s", name, k))
		}
		if truthy(srv.Vals["cwd"]) {
			setOrdered(out, "cwd", toConfig(srv.Vals["cwd"]))
		}
	}
	if truthy(srv.Vals["url"]) {
		setOrdered(out, "url", toConfig(srv.Vals["url"]))
		if headers, ok := asObject(srv.Vals["headers"]); ok {
			kept := pyjson.NewObject()
			for _, k := range headers.Keys {
				if !IsSecretKey(k) && !strings.Contains(strings.ToLower(k), "authorization") {
					kept.Set(k, headers.Vals[k])
				}
			}
			if kept.Len() > 0 {
				setOrdered(out, "headers", toConfig(kept))
			}
			for _, k := range headers.Keys {
				if _, ok := kept.Vals[k]; !ok {
					stripped = append(stripped, fmt.Sprintf("mcp_servers.%s.headers.%s", name, k))
				}
			}
		}
	}
	return out, stripped
}

// Orders paths the way pathlib sorts them, part by part
func pathLess(a, b string) bool {
	pa, pb := strings.Split(filepath.ToSlash(a), "/"), strings.Split(filepath.ToSlash(b), "/")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

func sortPaths(paths []string) {
	sort.SliceStable(paths, func(i, j int) bool { return pathLess(paths[i], paths[j]) })
}

// Every regular file below root at any depth, symlinked dirs not entered, in pathlib order
func treeFiles(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if p != root && isFile(p) && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	sortPaths(out)
	return out
}

// Final suffix of a path name like pathlib, empty for a dotfile or a name ending in a dot
func pySuffix(p string) string {
	name := filepath.Base(p)
	i := strings.LastIndex(name, ".")
	if i > 0 && i < len(name)-1 {
		return name[i:]
	}
	return ""
}

// Cursor user rules, the md and mdc files at any depth below the rules dir
func cursorRuleFiles(rulesDir string) []string {
	var out []string
	for _, p := range treeFiles(rulesDir) {
		if ext := pySuffix(p); ext == ".md" || ext == ".mdc" {
			out = append(out, p)
		}
	}
	return out
}

// Folders holding a SKILL.md at any depth below root, symlinks followed once per real folder
func nestedSkillDirs(root string) []string {
	var found []string
	seen := map[string]bool{}
	var walk func(dir string)
	walk = func(dir string) {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			real = dir
		}
		if seen[real] {
			return
		}
		seen[real] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		var dirs []string
		hasSkill := false
		for _, e := range entries {
			if isDir(filepath.Join(dir, e.Name())) {
				dirs = append(dirs, e.Name())
			} else if e.Name() == "SKILL.md" {
				hasSkill = true
			}
		}
		if dir != root && hasSkill {
			found = append(found, dir)
			return
		}
		sort.Strings(dirs)
		for _, d := range dirs {
			walk(filepath.Join(dir, d))
		}
	}
	walk(root)
	sortPaths(found)
	return found
}

// Item is one planned or applied change of an import
type Item struct {
	Kind        string
	Source      string
	Destination string
	Status      string
	Reason      string
	Details     map[string]any
}

// Report is the outcome of one importer run
type Report struct {
	Agent           string
	Source          string
	Target          string
	DryRun          bool
	Items           []Item
	Summary         map[string]int
	StrippedSecrets []string
}

// Shared recording, applying and merging of every importer
type base struct {
	ctx             context.Context
	agent           string
	sourceRoot      string
	targetRoot      string
	execute         bool
	overwrite       bool
	syncSkills      map[string]any
	items           []Item
	strippedSecrets []string
}

func (b *base) record(kind, source, destination, status, reason string, details map[string]any) {
	b.items = append(b.items, Item{Kind: kind, Source: source, Destination: destination, Status: status, Reason: reason, Details: details})
}

// Config mapping of the destination, or a recorded refusal and nil, in dry runs too
func (b *base) loadTargetConfig(kind, source, destination string) (map[string]any, error) {
	cfg, err := loadConfigFile(b.ctx, destination)
	var refused *ConfigReadError
	if errors.As(err, &refused) {
		b.record(kind, source, destination, "error", err.Error(), nil)
		return nil, nil
	}
	return cfg, err
}

// Records imported, running the action first when executing, a returned message recorded as an error
func (b *base) apply(kind, source, destination, would string, action func() (string, error), details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	failure := ""
	if b.execute {
		msg, err := action()
		if err != nil {
			return err
		}
		failure = msg
	}
	switch {
	case failure != "":
		b.record(kind, source, destination, "error", failure, details)
	case b.execute:
		b.record(kind, source, destination, "imported", "", details)
	default:
		b.record(kind, source, destination, "imported", would, details)
	}
	return nil
}

func (b *base) buildReport() *Report {
	summary := map[string]int{"imported": 0, "skipped": 0, "conflict": 0, "error": 0}
	for _, it := range b.items {
		summary[it.Status]++
	}
	r := &Report{Agent: b.agent, Source: b.sourceRoot, Target: b.targetRoot, DryRun: !b.execute, Items: b.items, Summary: summary}
	if len(b.strippedSecrets) > 0 {
		set := map[string]bool{}
		for _, s := range b.strippedSecrets {
			if !set[s] {
				set[s] = true
				r.StrippedSecrets = append(r.StrippedSecrets, s)
			}
		}
		sort.Strings(r.StrippedSecrets)
	}
	return r
}

// Merges entries into a memory store file, deduplicated within the budget, the old file backed up first
func (b *base) mergeMemoryEntries(kind, source, destination string, incoming []string) error {
	existing, err := parseExistingMemoryEntries(destination)
	if err != nil {
		return err
	}
	merged, stats := mergeEntries(existing, incoming, MemoryCharLimit)
	details := map[string]any{"existing_entries": stats.existing, "added_entries": stats.added,
		"duplicate_entries": stats.duplicates, "overflowed_entries": stats.overflowed}
	if stats.added == 0 {
		b.record(kind, source, destination, "skipped", "No new entries to import", details)
		return nil
	}
	write := func() (string, error) {
		if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
			return "", err
		}
		step := "back up existing"
		if exists(destination) {
			backup := fmt.Sprintf("%s.bak.%d", destination, time.Now().Unix())
			if err := pyshutil.Copy2(destination, backup); err != nil {
				return fmt.Sprintf("Could not %s memory file: %s", step, pyErrText(err)), nil
			}
			details["backup"] = backup
		}
		step = "write merged"
		content := strings.Join(merged, EntryDelimiter)
		if len(merged) > 0 {
			content += "\n"
		}
		if err := atomicWriteText(b.ctx, destination, content); err != nil {
			return fmt.Sprintf("Could not %s memory file: %s", step, pyErrText(err)), nil
		}
		return "", nil
	}
	return b.apply(kind, source, destination, "Would merge entries", write, details)
}

// Merges MCP servers into config.yaml mcp_servers, existing names a conflict unless overwriting
func (b *base) importMCPServers(servers *pyjson.Object, kind string) error {
	destination := filepath.Join(b.targetRoot, "config.yaml")
	if servers == nil || servers.Len() == 0 {
		b.record(kind, "", destination, "skipped", "No MCP servers found", nil)
		return nil
	}
	cfg, err := b.loadTargetConfig(kind, "", destination)
	if err != nil || cfg == nil {
		return err
	}
	existing, ok := cfg["mcp_servers"].(map[string]any)
	if !ok {
		existing = map[string]any{}
	}
	added := 0
	for _, name := range servers.Keys {
		srv, ok := asObject(servers.Vals[name])
		if !ok {
			b.record(kind, name, "", "skipped", "Server entry is not a mapping", nil)
			continue
		}
		if _, has := existing[name]; has && !b.overwrite {
			b.record(kind, name, "mcp_servers."+name, "conflict", "MCP server already exists in Icebath config", nil)
			continue
		}
		entry, stripped := translateMCPServer(name, srv)
		b.strippedSecrets = append(b.strippedSecrets, stripped...)
		if len(entry) == 0 {
			b.record(kind, name, "", "skipped", "Server has neither a command nor a url", nil)
			continue
		}
		setOrdered(existing, name, entry)
		added++
		b.record(kind, name, "config.yaml mcp_servers."+name, "imported", "", nil)
	}
	if added > 0 && b.execute {
		setOrdered(cfg, "mcp_servers", existing)
		return writeConfigFile(b.ctx, b.targetRoot, destination, cfg)
	}
	return nil
}

// AgentImporter plans and applies the import of one Claude Code, Codex or Cursor tree
type AgentImporter struct {
	base
}

// NewAgentImporter builds the importer of a supported agent, syncSkills naming skills an earlier import wrote with their digest
func NewAgentImporter(ctx context.Context, agent, sourceRoot, targetRoot string, execute, overwrite bool, syncSkills map[string]any) (*AgentImporter, error) {
	if _, ok := AgentDefaultDirs[agent]; !ok {
		return nil, fmt.Errorf("Unsupported agent: %s", agent)
	}
	if syncSkills == nil {
		syncSkills = map[string]any{}
	}
	return &AgentImporter{base{ctx: ctx, agent: agent, sourceRoot: sourceRoot, targetRoot: targetRoot, execute: execute,
		overwrite: overwrite, syncSkills: syncSkills}}, nil
}

// Run plans every item, applying them when executing, and returns the report
func (a *AgentImporter) Run() (*Report, error) {
	if !isDir(a.sourceRoot) {
		a.record("source", a.sourceRoot, "", "error", "Source directory does not exist", nil)
		return a.buildReport(), nil
	}
	var err error
	switch a.agent {
	case "claude-code":
		err = a.runClaudeCode()
	case "codex":
		err = a.runCodex()
	case "cursor":
		err = a.runCursor()
	}
	if err != nil {
		return nil, err
	}
	return a.buildReport(), nil
}

func (a *AgentImporter) runClaudeCode() error {
	settings := a.loadSourceMapping("settings", filepath.Join(a.sourceRoot, "settings.json"), parseJSON, true, "settings.json is not a JSON object")
	if err := a.importContextFile(filepath.Join(a.sourceRoot, "CLAUDE.md"), "claude-md"); err != nil {
		return err
	}
	if err := a.importPermissionRules(settings, "allow"); err != nil {
		return err
	}
	if err := a.importPermissionRules(settings, "deny"); err != nil {
		return err
	}
	claudeJSON := a.loadSourceMapping("mcp-servers", filepath.Join(filepath.Dir(a.sourceRoot), ".claude.json"), parseJSON, false, "")
	servers := pyjson.NewObject()
	for _, src := range []any{claudeJSON.Vals["mcpServers"], settings.Vals["mcpServers"]} {
		if o, ok := asObject(src); ok {
			for _, name := range o.Keys {
				if _, has := servers.Vals[name]; !has {
					servers.Set(name, o.Vals[name])
				}
			}
		}
	}
	if err := a.importMCPServers(servers, "mcp-servers"); err != nil {
		return err
	}
	if err := a.importSkills(filepath.Join(a.sourceRoot, "skills"), false); err != nil {
		return err
	}
	commandsDir := filepath.Join(a.sourceRoot, "commands")
	if isDir(commandsDir) {
		if matches, _ := filepath.Glob(filepath.Join(globEscape(commandsDir), "*.md")); len(matches) > 0 {
			a.record("slash-commands", commandsDir, "", "skipped",
				"Claude slash commands have no direct Icebath equivalent — consider converting them into skills", nil)
		}
	}
	return nil
}

func (a *AgentImporter) runCodex() error {
	cfg := a.loadSourceMapping("config", filepath.Join(a.sourceRoot, "config.toml"), parseTOML, true, "")
	if err := a.importContextFile(filepath.Join(a.sourceRoot, "AGENTS.md"), "agents-md"); err != nil {
		return err
	}
	mcp, _ := asObject(cfg.Vals["mcp_servers"])
	if err := a.importMCPServers(mcp, "mcp-servers"); err != nil {
		return err
	}
	memoriesDir := filepath.Join(a.sourceRoot, "memories")
	var files []string
	present := isDir(memoriesDir)
	if present {
		files, _ = filepath.Glob(filepath.Join(globEscape(memoriesDir), "*.md"))
		sortPaths(files)
	}
	if err := a.importMarkdownFiles("memories", memoriesDir, files, present, "No memories directory found", false, false); err != nil {
		return err
	}
	return a.importSkills(filepath.Join(a.sourceRoot, "skills"), false)
}

func (a *AgentImporter) runCursor() error {
	if err := a.importContextFile(filepath.Join(a.sourceRoot, "AGENTS.md"), "agents-md"); err != nil {
		return err
	}
	rulesDir := filepath.Join(a.sourceRoot, "rules")
	present := isDir(rulesDir)
	var files []string
	if present {
		files = cursorRuleFiles(rulesDir)
	}
	if err := a.importMarkdownFiles("rules", rulesDir, files, present, "No rules directory found", false, true); err != nil {
		return err
	}
	mcpFile := a.loadSourceMapping("mcp-servers", filepath.Join(a.sourceRoot, "mcp.json"), parseJSON, false, "")
	mcp, _ := asObject(mcpFile.Vals["mcpServers"])
	if err := a.importMCPServers(mcp, "mcp-servers"); err != nil {
		return err
	}
	return a.importSkills(filepath.Join(a.sourceRoot, "skills"), true)
}

// Escapes glob metacharacters of a literal dir so a pattern below it matches only its own children
func globEscape(dir string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)
	if filepath.Separator == '\\' {
		r = strings.NewReplacer(`*`, `[*]`, `?`, `[?]`, `[`, `[[]`)
	}
	return r.Replace(dir)
}

// Parses JSON text into ordered objects
func parseJSON(text string) (any, error) { return pyjson.Loads(text) }

// Parses TOML text into ordered objects in document order
func parseTOML(text string) (any, error) { return loadsTOMLOrdered(text) }

// Mapping parsed from a source file, problems recorded as items and an empty mapping returned
func (a *AgentImporter) loadSourceMapping(kind, path string, parse func(string) (any, error), recordMissing bool, nonMappingError string) *pyjson.Object {
	if _, err := os.Stat(path); err != nil {
		if recordMissing {
			a.record(kind, "", "", "skipped", "No "+filepath.Base(path)+" found", nil)
		}
		return pyjson.NewObject()
	}
	text, err := readText(path)
	var data any
	if err == nil {
		data, err = parse(text)
	}
	if err != nil {
		a.record(kind, path, "", "error", fmt.Sprintf("Could not parse %s: %s", filepath.Base(path), pyErrText(err)), nil)
		return pyjson.NewObject()
	}
	if o, ok := asObject(data); ok {
		return o
	}
	if nonMappingError != "" {
		a.record(kind, path, "", "error", nonMappingError, nil)
	}
	return pyjson.NewObject()
}

// Python str of an OS or decode error
func pyErrText(err error) string { return errchain.OSText(err) }

// CLAUDE.md or AGENTS.md entries merged into memories/MEMORY.md
func (a *AgentImporter) importContextFile(source, kind string) error {
	present := exists(source)
	var files []string
	if present {
		files = []string{source}
	}
	return a.importMarkdownFiles(kind, source, files, present, "No "+filepath.Base(source)+" found", true, false)
}

// Entries of the files merged into memories/MEMORY.md, a missing source skipped and unreadable files recorded
func (b *base) importMarkdownFiles(kind, source string, files []string, present bool, missingReason string, singleFile, stripMeta bool) error {
	destination := filepath.Join(b.targetRoot, "memories", "MEMORY.md")
	if !present {
		b.record(kind, "", destination, "skipped", missingReason, nil)
		return nil
	}
	var incoming []string
	failed := false
	for _, f := range files {
		text, err := readText(f)
		if err != nil {
			failed = true
			b.record(kind, f, destination, "error", "Could not read file: "+pyErrText(err), nil)
			continue
		}
		if stripMeta {
			text = StripFrontmatter(text)
		}
		incoming = append(incoming, ExtractMarkdownEntries(text)...)
	}
	if len(incoming) == 0 {
		if !(failed && singleFile) {
			b.record(kind, source, destination, "skipped", "No importable entries found", nil)
		}
		return nil
	}
	return b.mergeMemoryEntries(kind, source, destination, incoming)
}

// Settings key, item kind and config path of each Claude permission list
var permissionRules = map[string]struct {
	kind          string
	path          []string
	trackUnmapped bool
}{
	"allow": {"command-allowlist", []string{"command_allowlist"}, true},
	"deny":  {"command-denylist", []string{"approvals", "deny"}, false},
}

// settings.json permissions allow and deny rules merged into command_allowlist and approvals.deny
func (a *AgentImporter) importPermissionRules(settings *pyjson.Object, key string) error {
	spec := permissionRules[key]
	label := "settings.json permissions." + key
	destination := filepath.Join(a.targetRoot, "config.yaml")
	var rules []any
	if perms, ok := asObject(settings.Vals["permissions"]); ok {
		rules, _ = perms.Vals[key].([]any)
	}
	if len(rules) == 0 {
		a.record(spec.kind, "", destination, "skipped", "No permissions."+key+" rules found", nil)
		return nil
	}
	var patterns, skippedRules []string
	seen := map[string]bool{}
	for _, r := range rules {
		rule, ok := r.(string)
		if !ok {
			continue
		}
		p := ClaudeRuleToCommandPattern(rule)
		if p == "" {
			skippedRules = append(skippedRules, rule)
			continue
		}
		if !seen[p] {
			seen[p] = true
			patterns = append(patterns, p)
		}
	}
	sort.Strings(patterns)
	var unmapped map[string]any
	if spec.trackUnmapped {
		unmapped = map[string]any{"unmapped_rules": stringsOrEmpty(skippedRules)}
	}
	if len(patterns) == 0 {
		a.record(spec.kind, "", destination, "skipped", "No Bash(...) "+key+" rules to import", unmapped)
		return nil
	}
	if len(skippedRules) == 0 {
		unmapped = nil
	}
	cfg, err := a.loadTargetConfig(spec.kind, label, destination)
	if err != nil || cfg == nil {
		return err
	}
	parent := cfg
	for _, part := range spec.path[:len(spec.path)-1] {
		child, ok := parent[part].(map[string]any)
		if !ok {
			child = map[string]any{}
		}
		setOrdered(parent, part, child)
		parent = child
	}
	leaf := spec.path[len(spec.path)-1]
	current, _ := parent[leaf].([]any)
	merged := mergePatterns(current, patterns)
	var added []string
	for _, p := range merged {
		if !containsValue(current, p) {
			added = append(added, pyStr(p))
		}
	}
	if len(added) == 0 {
		a.record(spec.kind, label, destination, "skipped", "All patterns already present", nil)
		return nil
	}
	write := func() (string, error) {
		setOrdered(parent, leaf, merged)
		return "", writeConfigFile(a.ctx, a.targetRoot, destination, cfg)
	}
	details := map[string]any{"added_patterns": added}
	for k, v := range unmapped {
		details[k] = v
	}
	return a.apply(spec.kind, label, destination, "Would merge patterns", write, details)
}

func stringsOrEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func containsValue(list []any, v any) bool {
	for _, item := range list {
		if pyjson.Equal(item, v) {
			return true
		}
	}
	return false
}

// Existing list values and new patterns deduplicated and sorted
func mergePatterns(current []any, patterns []string) []any {
	var out []any
	for _, v := range current {
		if !containsValue(out, v) {
			out = append(out, v)
		}
	}
	for _, p := range patterns {
		if !containsValue(out, p) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return pyStr(out[i]) < pyStr(out[j]) })
	return out
}

// Skill dirs with a SKILL.md copied under skills/<category>, nested ones flat when recursive
func (a *AgentImporter) importSkills(sourceRoot string, recursive bool) error {
	destinationRoot := filepath.Join(a.targetRoot, "skills", skillCategory[a.agent])
	if !isDir(sourceRoot) {
		a.record("skills", "", destinationRoot, "skipped", "No skills directory found", nil)
		return nil
	}
	var skillDirs []string
	if recursive {
		skillDirs = nestedSkillDirs(sourceRoot)
	} else {
		entries, err := os.ReadDir(sourceRoot)
		if err != nil {
			return err
		}
		var all []string
		for _, e := range entries {
			all = append(all, filepath.Join(sourceRoot, e.Name()))
		}
		sortPaths(all)
		for _, p := range all {
			if isDir(p) && exists(filepath.Join(p, "SKILL.md")) {
				skillDirs = append(skillDirs, p)
			}
		}
	}
	if len(skillDirs) == 0 {
		a.record("skills", sourceRoot, destinationRoot, "skipped", "No skills with SKILL.md found", nil)
		return nil
	}
	planned := map[string]bool{}
	for _, skillDir := range skillDirs {
		name := filepath.Base(skillDir)
		destination := filepath.Join(destinationRoot, name)
		if planned[name] {
			a.record("skill", skillDir, destination, "conflict", "Duplicate skill name in source tree — already imported from another folder", nil)
			continue
		}
		planned[name] = true
		if exists(destination) && !a.overwrite {
			expected, managed := a.syncSkills[name]
			if !managed {
				a.record("skill", skillDir, destination, "conflict", "Destination skill already exists", nil)
				continue
			}
			if digest, isText := expected.(string); expected != nil && (!isText || SkillTreeDigest(destination) != digest) {
				a.record("skill", skillDir, destination, "conflict", "Imported skill was modified locally — not refreshed", nil)
				continue
			}
		}
		src, dst := skillDir, destination
		copyTree := func() (string, error) {
			if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
				return "", err
			}
			if exists(dst) {
				if err := os.RemoveAll(dst); err != nil {
					return "", err
				}
			}
			return "", pyfs.CopyTree(src, dst, false, false, nil)
		}
		if err := a.apply("skill", skillDir, destination, "Would copy skill directory", copyTree, nil); err != nil {
			return err
		}
	}
	return nil
}
