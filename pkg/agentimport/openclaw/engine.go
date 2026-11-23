// Package openclaw ports the OpenClaw migration engine and the import-agent openclaw and claw commands
package openclaw

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	EntryDelimiter           = "\n§\n"
	DefaultMemoryCharLimit   = 2200
	DefaultUserCharLimit     = 1375
	SkillCategoryDirname     = "openclaw-imports"
	SkillCategoryDescription = "Skills migrated from an OpenClaw workspace."
	workspaceInstructions    = "AGENTS" + ".md"
)

// Item statuses of the report, the vocabulary report.json consumers read
const (
	StatusMigrated = "migrated"
	StatusArchived = "archived"
	StatusSkipped  = "skipped"
	StatusConflict = "conflict"
	StatusError    = "error"
)

const (
	ReasonTargetExists              = "Target exists and overwrite is disabled"
	ReasonBlockedByApplyConflict    = "blocked by earlier apply conflict"
	RedactedMigrationValue          = "[redacted]"
	defaultSkillConflictMode        = "skip"
	defaultPresetName               = "full"
	migrationTimestampLayout        = "20060102T150405"
	tmpConfigPrefix, tmpConfigSuffx = ".tmp_", ".yaml"
)

// Imported skill directory conflict policies
var SkillConflictModes = []string{"overwrite", "rename", "skip"}

// Secrets the allowlisted secret import may write
var SupportedSecretTargets = []string{
	"ANTHROPIC_API_KEY", "ELEVENLABS_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "TELEGRAM_BOT_TOKEN", "VOICE_TOOLS_OPENAI_KEY",
}

// One selectable migration option with its label and description
type OptionMeta struct {
	ID          string
	Label       string
	Description string
}

// Every migration option in report order
var MigrationOptions = []OptionMeta{
	{"soul", "SOUL.md", "Import the OpenClaw persona file into Icebath."},
	{"workspace-agents", "Workspace instructions", "Copy the OpenClaw workspace instructions file into a chosen workspace."},
	{"memory", "MEMORY.md", "Import long-term memory entries into Icebath memories."},
	{"user-profile", "USER.md", "Import user profile entries into Icebath memories."},
	{"messaging-settings", "Messaging settings", "Import Icebath-compatible messaging settings such as allowlists and working directory."},
	{"secret-settings", "Allowlisted secrets", "Import the small allowlist of Icebath-compatible secrets when explicitly enabled."},
	{"command-allowlist", "Command allowlist", "Merge OpenClaw exec approval patterns into Icebath command_allowlist."},
	{"skills", "User skills", "Copy OpenClaw skills into ~/.icebath/skills/openclaw-imports/."},
	{"tts-assets", "TTS assets", "Copy compatible workspace TTS assets into ~/.icebath/tts/."},
	{"discord-settings", "Discord settings", "Import Discord bot token and allowlist into Icebath .env."},
	{"slack-settings", "Slack settings", "Import Slack bot/app tokens and allowlist into Icebath .env."},
	{"whatsapp-settings", "WhatsApp settings", "Import WhatsApp allowlist into Icebath .env."},
	{"signal-settings", "Signal settings", "Import Signal account, HTTP URL, and allowlist into Icebath .env."},
	{"provider-keys", "Provider API keys", "Import model provider API keys into Icebath .env (requires --migrate-secrets)."},
	{"model-config", "Default model", "Import the default model setting into Icebath config.yaml."},
	{"tts-config", "TTS configuration", "Import TTS provider and voice settings into Icebath config.yaml."},
	{"shared-skills", "Shared skills", "Copy shared OpenClaw skills from ~/.openclaw/skills/ into Icebath."},
	{"daily-memory", "Daily memory files", "Merge daily memory entries from workspace/memory/ into Icebath MEMORY.md."},
	{"archive", "Archive unmapped docs", "Archive compatible-but-unmapped docs for later manual review."},
	{"mcp-servers", "MCP servers", "Import MCP server definitions from OpenClaw into Icebath config.yaml."},
	{"plugins-config", "Plugins configuration", "Archive OpenClaw plugin configuration and installed extensions for manual review."},
	{"cron-jobs", "Cron / scheduled tasks", "Import cron job definitions. Archive for manual recreation via 'icebath cron'."},
	{"hooks-config", "Hooks and webhooks", "Archive OpenClaw hook configuration (internal hooks, webhooks, Gmail integration)."},
	{"agent-config", "Agent defaults and multi-agent setup", "Import agent defaults (compaction, context, thinking) into Icebath config. Archive multi-agent list."},
	{"gateway-config", "Gateway configuration", "Import gateway port and auth settings. Archive full gateway config for manual setup."},
	{"session-config", "Session configuration", "Archive advanced session settings; automatic reset timers are not imported."},
	{"full-providers", "Full model provider definitions", "Import custom model providers (baseUrl, apiType, headers) into Icebath providers."},
	{"deep-channels", "Deep channel configuration", "Import extended channel settings (Matrix, Mattermost, IRC, group configs). Archive complex settings."},
	{"browser-config", "Browser configuration", "Import browser automation settings into Icebath config.yaml."},
	{"tools-config", "Tools configuration", "Import tool settings (exec timeout, sandbox, web search) into Icebath config.yaml."},
	{"approvals-config", "Approval rules", "Import approval mode and rules into Icebath config.yaml approvals section."},
	{"memory-backend", "Memory backend configuration", "Archive OpenClaw memory backend settings (QMD, vector search, citations) for manual review."},
	{"skills-config", "Skills registry configuration", "Archive per-skill enabled/config/env settings from OpenClaw skills.entries."},
	{"ui-identity", "UI and identity settings", "Archive OpenClaw UI theme, assistant identity, and display preferences."},
	{"logging-config", "Logging and diagnostics", "Archive OpenClaw logging and diagnostics configuration."},
}

// Metadata of an option id and whether it exists
func OptionByID(id string) (OptionMeta, bool) {
	for _, o := range MigrationOptions {
		if o.ID == id {
			return o, true
		}
	}
	return OptionMeta{}, false
}

// Every option id in report order
func allOptionIDs() []string {
	out := make([]string, len(MigrationOptions))
	for i, o := range MigrationOptions {
		out[i] = o.ID
	}
	return out
}

// One named preset and the option ids it selects
type Preset struct {
	ID      string
	Options []string
}

// Named presets in report order, user-data leaving out the secret groups
var MigrationPresets = []Preset{
	{"user-data", []string{
		"soul", "workspace-agents", "memory", "user-profile", "messaging-settings", "command-allowlist", "skills", "tts-assets",
		"discord-settings", "slack-settings", "whatsapp-settings", "signal-settings", "model-config", "tts-config", "shared-skills",
		"daily-memory", "archive", "mcp-servers", "agent-config", "session-config", "browser-config", "tools-config",
		"approvals-config", "deep-channels", "full-providers", "plugins-config", "cron-jobs", "hooks-config", "memory-backend",
		"skills-config", "ui-identity", "logging-config", "gateway-config",
	}},
	{"full", allOptionIDs()},
}

// Option ids of a preset and whether it exists
func presetOptions(name string) ([]string, bool) {
	for _, p := range MigrationPresets {
		if p.ID == name {
			return p.Options, true
		}
	}
	return nil, false
}

// Sorted preset names
func presetNames() []string {
	out := make([]string, len(MigrationPresets))
	for i, p := range MigrationPresets {
		out[i] = p.ID
	}
	sort.Strings(out)
	return out
}

// Comma separated values split, stripped and lowercased with empty parts dropped
func ParseSelectionValues(values []string) []string {
	var parsed []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = lower(strip(part))
			if part != "" {
				parsed = append(parsed, part)
			}
		}
	}
	return parsed
}

// Selected option ids for include, exclude and preset, a ValueError for unknown names
func ResolveSelectedOptions(include, exclude []string, preset string) (map[string]bool, error) {
	includeValues := ParseSelectionValues(include)
	excludeValues := ParseSelectionValues(exclude)
	valid := map[string]bool{}
	for _, id := range allOptionIDs() {
		valid[id] = true
	}
	presetName := lower(strip(preset))
	if presetName != "" {
		if _, ok := presetOptions(presetName); !ok {
			return nil, &PyError{Type: "ValueError", Msg: "Unknown migration preset: " + presetName + ". Valid presets: " + strings.Join(presetNames(), ", ")}
		}
	}
	unknownSet := map[string]bool{}
	for _, v := range append(append([]string{}, includeValues...), excludeValues...) {
		if v != "all" && !valid[v] {
			unknownSet[v] = true
		}
	}
	if len(unknownSet) > 0 {
		validNames := allOptionIDs()
		sort.Strings(validNames)
		return nil, &PyError{Type: "ValueError", Msg: "Unknown migration option(s): " + strings.Join(sortedKeys(unknownSet), ", ") + ". Valid options: " + strings.Join(validNames, ", ")}
	}
	selected := map[string]bool{}
	switch {
	case presetName != "":
		opts, _ := presetOptions(presetName)
		for _, id := range opts {
			selected[id] = true
		}
	case len(includeValues) == 0 || containsString(includeValues, "all"):
		for id := range valid {
			selected[id] = true
		}
	default:
		for _, id := range includeValues {
			selected[id] = true
		}
	}
	if containsString(excludeValues, "all") {
		selected = map[string]bool{}
	}
	for _, id := range excludeValues {
		if id != "all" {
			delete(selected, id)
		}
	}
	return selected, nil
}

// Sorted keys of a string set
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Sorts strings in place by code point
func sortStrings(values []string) { sort.Strings(values) }

// Reports whether values holds s
func containsString(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

// Hex sha256 of a file
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// File text decoded as UTF-8 with undecodable bytes replaced
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return pystr.DecodeUTF8Replace(data), nil
}

// File text as readText, an unreadable file raised as the OSError it is
func mustReadText(path string) string {
	text, err := readText(path)
	if err != nil {
		raise(err)
	}
	return text
}

// Writes text as UTF-8, raising the OSError of a failed write
func writeText(path, text string) {
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
		raise(err)
	}
}

// Reports whether a path exists, symlinks followed
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Reports whether a path is a directory, symlinks followed
func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// Reports whether a path is a regular file, symlinks followed
func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Python str of an OSError
func osErrText(err error) string { return errchain.OSText(err) }

var seededDefaultSoul = strip(config.DefaultIdentity)

// Reports whether a SOUL.md still holds the default Icebath seeded or an older seed nobody edited
func isIcebathSeededSoul(text string) bool {
	normalized := strip(strings.TrimLeft(strings.ReplaceAll(text, "\r\n", "\n"), "\ufeff"))
	return normalized == seededDefaultSoul || config.IsLegacyTemplateSoul(text)
}

var whitespaceRun = pyre.MustCompile(`\s+`)

// Text stripped with every whitespace run collapsed to one space
func normalizeText(text string) string {
	out, err := whitespaceRun.Replace(strip(text), " ", -1, -1)
	if err != nil {
		raise(err)
	}
	return out
}

// Creates the parent directories of a path
func ensureParent(path string) {
	if err := os.MkdirAll(pypath.PathParent(path), 0o777); err != nil {
		raise(err)
	}
}

// Creates a directory and its parents
func mkdirAll(path string) {
	if err := os.MkdirAll(path, 0o777); err != nil {
		raise(err)
	}
}

var envTemplate = pyre.MustCompile(`^\$\{(\w+)\}$`)

// Plain string of an OpenClaw SecretInput, an env template or env SecretRef resolved through env
func ResolveSecretInput(value any, env map[string]string) (string, bool) {
	switch v := value.(type) {
	case string:
		m, err := envTemplate.FindStringMatch(strip(v))
		if err != nil {
			raise(err)
		}
		if m != nil && len(env) > 0 {
			got := strip(env[m.GroupByNumber(1).String()])
			return got, got != ""
		}
		s := strip(v)
		return s, s != ""
	default:
		if !isDict(value) {
			return "", false
		}
		source := dget(value, "source", "")
		refID := dget(value, "id", "")
		if eqStr(source, "env") && truthy(refID) && len(env) > 0 {
			key, isKey := refID.(string)
			if !isKey {
				return "", false
			}
			got := strip(env[key])
			return got, got != ""
		}
	}
	return "", false
}

// Existing config file present but unreadable, the merge refused so the file stays byte identical
type ConfigReadError struct{ Msg string }

func (e *ConfigReadError) Error() string { return e.Msg }

// Python exception class name of the error
func (e *ConfigReadError) PyTypeName() string { return "ConfigReadError" }

// YAML mapping of a config file, empty when absent or empty, ConfigReadError when unreadable or not a mapping
func LoadYAMLFile(path string) (map[string]any, error) {
	if !exists(path) {
		return newYAMLMap(), nil
	}
	raw, err := readText(path)
	if err != nil {
		return nil, &ConfigReadError{Msg: fmt.Sprintf("Refusing to overwrite %s: the existing file cannot be read (%s). Fix the file permissions or move it aside first.", path, osErrText(err))}
	}
	data, err := config.SafeLoad(raw)
	if err != nil {
		return nil, &ConfigReadError{Msg: fmt.Sprintf("Refusing to overwrite %s: the existing file is not valid YAML (%s). Fix it with `icebath config edit` (or move it aside), then re-run the migration.", path, err.Error())}
	}
	if data == nil {
		return newYAMLMap(), nil
	}
	m, ok := data.(map[string]any)
	if !ok {
		return nil, &ConfigReadError{Msg: fmt.Sprintf("Refusing to overwrite %s: expected the existing file to hold a YAML mapping but found %s. Fix it with `icebath config edit` (or move it aside), then re-run the migration.", path, errchain.Kind(data))}
	}
	return m, nil
}

// Config mapping as LoadYAMLFile reads it, the ConfigReadError raised
func loadYAML(path string) map[string]any {
	m, err := LoadYAMLFile(path)
	if err != nil {
		raise(err)
	}
	return m
}

// YAML 1.1 safe dump options of the migration writer, ASCII only with ruamel default indentation
var yamlDumpOptions = config.SafeDumpOptions{AllowUnicode: false, Width: 80, Indent: &config.RuamelDefaultIndent, Version: "1.1"}

// Writes data as YAML through a synced temp file and an atomic rename, a symlinked target written through its link
func DumpYAMLFile(path string, data map[string]any) (err error) {
	if err = os.MkdirAll(pypath.PathParent(path), 0o777); err != nil {
		return err
	}
	target := path
	if st, lerr := os.Lstat(path); lerr == nil && st.Mode()&os.ModeSymlink != 0 {
		if target, err = pypath.Realpath(path, pypath.Lax); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), tmpConfigPrefix+"*"+tmpConfigSuffx)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpPath)
		}
	}()
	text, err := config.SafeDumpWith(data, yamlDumpOptions)
	if err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(text); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, target); err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.EBUSY) {
		return err
	}
	if err = pyshutil.CopyFile(tmpPath, target); err != nil {
		return err
	}
	copyStat(tmpPath, target)
	if f, ferr := os.Open(target); ferr == nil {
		f.Sync()
		f.Close()
	}
	return os.Remove(tmpPath)
}

// Writes the config mapping, raising the write failure
func dumpYAML(path string, data map[string]any) {
	if err := DumpYAMLFile(path, data); err != nil {
		raise(err)
	}
}

// Copies the access and modification times, extended attributes and mode bits of src onto dst
func copyStat(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.Chtimes(dst, pyshutil.FileAtime(st), st.ModTime()); err != nil {
		return err
	}
	if err := copyXattrs(src, dst); err != nil {
		return err
	}
	return os.Chmod(dst, st.Mode().Perm()|st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
}

// Ordered KEY=VALUE pairs of an env file
type envFile struct {
	keys []string
	vals map[string]string
}

// Value under key and whether it is set
func (e *envFile) get(key string) (string, bool) {
	v, ok := e.vals[key]
	return v, ok
}

// Stores value under key, a new key appended
func (e *envFile) set(key, value string) {
	if _, ok := e.vals[key]; !ok {
		e.keys = append(e.keys, key)
	}
	e.vals[key] = value
}

// KEY=VALUE pairs of an env file, comments and lines without an equals sign skipped, empty when unreadable
func parseEnvFile(path string) *envFile {
	out := &envFile{vals: map[string]string{}}
	if !exists(path) {
		return out
	}
	text, err := readText(path)
	if err != nil {
		return out
	}
	for _, rawLine := range pystr.Splitlines(text, false) {
		line := strip(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		out.set(strip(key), strip(value))
	}
	return out
}

// Plain map of an env file
func parseEnvMap(path string) map[string]string {
	return parseEnvFile(path).vals
}

// Writes the env pairs one per line
func saveEnvFile(path string, data *envFile) {
	ensureParent(path)
	lines := make([]string, 0, len(data.keys))
	for _, k := range data.keys {
		lines = append(lines, k+"="+data.vals[k])
	}
	text := strings.Join(lines, "\n")
	if len(lines) > 0 {
		text += "\n"
	}
	writeText(path, text)
}

// Copies an existing path under backupRoot at its absolute path, returning the copy or empty when absent
func backupExisting(path, backupRoot string) string {
	if !exists(path) {
		return ""
	}
	rel := path
	anchor, parts := pypath.PathParts(path)
	if anchor != "" && len(parts) > 0 {
		rel = pypath.PathJoin(parts[0], parts[1:]...)
	}
	dest := pypath.PathJoin(backupRoot, rel)
	ensureParent(dest)
	if isDir(path) {
		if err := pyshutil.CopyTree(path, dest, false, true); err != nil {
			raise(err)
		}
	} else if err := pyshutil.Copy2(path, dest); err != nil {
		raise(err)
	}
	return dest
}

var rebrandPatterns = []rebrandPattern{
	{pyre.MustCompile(`(?i)\bOpen[\s-]?Claw\b`), "Icebath"},
	{pyre.MustCompile(`(?i)\bClawdBot\b`), "Icebath"},
	{pyre.MustCompile(`(?i)\bMoltBot\b`), "Icebath"},
}

// One brand pattern and the name replacing it
type rebrandPattern struct {
	re          *regexp2.Regexp
	replacement string
}

// Text with OpenClaw, ClawdBot and MoltBot renamed to Icebath, an all lowercase match becoming lowercase icebath
func RebrandText(text string) string {
	for _, p := range rebrandPatterns {
		out, err := p.re.ReplaceFunc(text, func(m regexp2.Match) string {
			if matched := m.String(); matched != "" && isLower(matched) {
				return lower(p.replacement)
			}
			return p.replacement
		}, -1, -1)
		if err != nil {
			raise(err)
		}
		text = out
	}
	return text
}

// Entries of a destination memory store split on the entry delimiter, a store without one being a single entry
func parseExistingMemoryEntries(path string) []string {
	if !exists(path) {
		return nil
	}
	raw := mustReadText(path)
	if strip(raw) == "" {
		return nil
	}
	var out []string
	for _, e := range strings.Split(raw, EntryDelimiter) {
		if strip(e) != "" {
			out = append(out, strip(e))
		}
	}
	return out
}

var (
	contextHeadingFilter = pyre.MustCompile(`(?i)\b(MEMORY|USER|SOUL|AGENTS|TOOLS|IDENTITY)\.md\b`)
	headingLine          = pyre.MustCompile(`^(#{1,6})\s+(.*\S)\s*$`)
	bulletLine           = pyre.MustCompile(`^\s*(?:[-*]|\d+\.)\s+(.*\S)\s*$`)
)

// First match of re in s or nil
func firstMatch(re *regexp2.Regexp, s string) *regexp2.Match {
	m, err := re.FindStringMatch(s)
	if err != nil {
		raise(err)
	}
	return m
}

// Memory entries of a source markdown file, each bullet or paragraph prefixed with its heading path and deduplicated
func ExtractMarkdownEntries(text string) []string {
	var entries, headings, paragraph []string
	contextPrefix := func() string {
		var filtered []string
		for _, h := range headings {
			if h == "" {
				continue
			}
			m, err := contextHeadingFilter.FindStringMatch(h)
			if err != nil {
				raise(err)
			}
			if m == nil {
				filtered = append(filtered, h)
			}
		}
		return strings.Join(filtered, " > ")
	}
	flush := func() {
		if len(paragraph) == 0 {
			return
		}
		parts := make([]string, len(paragraph))
		for i, l := range paragraph {
			parts[i] = strip(l)
		}
		block := strip(strings.Join(parts, " "))
		paragraph = nil
		if block == "" {
			return
		}
		if prefix := contextPrefix(); prefix != "" {
			entries = append(entries, prefix+": "+block)
		} else {
			entries = append(entries, block)
		}
	}
	inCode := false
	for _, rawLine := range pystr.Splitlines(text, false) {
		line := pystr.Rstrip(rawLine)
		stripped := strip(line)
		if strings.HasPrefix(stripped, "```") {
			inCode = !inCode
			flush()
			continue
		}
		if inCode {
			continue
		}
		if m := firstMatch(headingLine, stripped); m != nil {
			flush()
			level := pystr.Len(m.GroupByNumber(1).String())
			value := strip(m.GroupByNumber(2).String())
			for len(headings) >= level {
				headings = headings[:len(headings)-1]
			}
			headings = append(headings, value)
			continue
		}
		if m := firstMatch(bulletLine, line); m != nil {
			flush()
			content := strip(m.GroupByNumber(1).String())
			if prefix := contextPrefix(); prefix != "" {
				entries = append(entries, prefix+": "+content)
			} else {
				entries = append(entries, content)
			}
			continue
		}
		if stripped == "" {
			flush()
			continue
		}
		if strings.HasPrefix(stripped, "|") && strings.HasSuffix(stripped, "|") {
			flush()
			continue
		}
		paragraph = append(paragraph, stripped)
	}
	flush()
	var deduped []string
	seen := map[string]bool{}
	for _, entry := range entries {
		n := normalizeText(entry)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		deduped = append(deduped, strip(entry))
	}
	return deduped
}

// Merge counters of one memory import
type MergeStats struct {
	Existing, Added, Duplicates, Overflowed int
}

// Existing entries plus each new incoming one that fits under limit, with the counters and the entries that overflowed
func MergeEntries(existing, incoming []string, limit int) ([]string, MergeStats, []string) {
	merged := append([]string(nil), existing...)
	seen := map[string]bool{}
	for _, e := range existing {
		if strip(e) != "" {
			seen[normalizeText(e)] = true
		}
	}
	stats := MergeStats{Existing: len(existing)}
	var overflowed []string
	currentLen := 0
	if len(merged) > 0 {
		currentLen = pystr.Len(strings.Join(merged, EntryDelimiter))
	}
	delimLen := pystr.Len(EntryDelimiter)
	for _, entry := range incoming {
		n := normalizeText(entry)
		if n == "" {
			continue
		}
		if seen[n] {
			stats.Duplicates++
			continue
		}
		candidate := pystr.Len(entry)
		if len(merged) > 0 {
			candidate = currentLen + delimLen + pystr.Len(entry)
		}
		if candidate > limit {
			stats.Overflowed++
			overflowed = append(overflowed, entry)
			continue
		}
		merged = append(merged, entry)
		seen[n] = true
		currentLen = candidate
		stats.Added++
	}
	return merged, stats, overflowed
}

// Path relative to root, the path itself when outside it
func relativeLabel(path, root string) string {
	if rel, ok := pypath.PathRelativeTo(path, root); ok {
		return rel
	}
	return pypath.PathStr(path)
}

// Local timestamp naming one migration run
func migrationTimestamp(now time.Time) string { return now.Format(migrationTimestampLayout) }
