package openclaw

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Inputs of one migration run, empty paths meaning None and an empty selection meaning every option
type MigratorOptions struct {
	SourceRoot        string
	TargetRoot        string
	Execute           bool
	WorkspaceTarget   string
	Overwrite         bool
	MigrateSecrets    bool
	OutputDir         string
	SelectedOptions   map[string]bool
	PresetName        string
	SkillConflictMode string
}

// Option ids that write config.yaml, skipped after an earlier config.yaml conflict or error during apply
var configMutatingOptions = map[string]bool{
	"model-config": true, "tts-config": true, "mcp-servers": true, "plugins-config": true, "cron-jobs": true,
	"hooks-config": true, "agent-config": true, "gateway-config": true, "full-providers": true, "deep-channels": true,
	"browser-config": true, "tools-config": true, "approvals-config": true, "memory-backend": true, "skills-config": true,
	"ui-identity": true, "logging-config": true, "command-allowlist": true,
}

// One OpenClaw to Icebath migration, a preview when Execute is off
type Migrator struct {
	sourceRoot         string
	targetRoot         string
	execute            bool
	workspaceTarget    string
	overwrite          bool
	migrateSecrets     bool
	selected           map[string]bool
	presetName         string
	skillConflictMode  string
	timestamp          string
	outputDir          string
	archiveDir         string
	backupDir          string
	overflowDir        string
	items              []ItemResult
	configApplyBlocked bool
	customWorkspace    string
	memoryLimit        int
	userLimit          int
}

// Migrator for the options, a ValueError for an unknown skill conflict mode
func NewMigrator(o MigratorOptions) (m *Migrator, err error) {
	defer catch(&err)
	m = &Migrator{
		sourceRoot:        o.SourceRoot,
		targetRoot:        o.TargetRoot,
		execute:           o.Execute,
		workspaceTarget:   o.WorkspaceTarget,
		overwrite:         o.Overwrite,
		migrateSecrets:    o.MigrateSecrets,
		selected:          map[string]bool{},
		presetName:        lower(strip(o.PresetName)),
		skillConflictMode: lower(strip(o.SkillConflictMode)),
		timestamp:         migrationTimestamp(time.Now()),
	}
	if len(o.SelectedOptions) == 0 {
		for _, id := range allOptionIDs() {
			m.selected[id] = true
		}
	} else {
		for id, on := range o.SelectedOptions {
			if on {
				m.selected[id] = true
			}
		}
	}
	if m.skillConflictMode == "" {
		m.skillConflictMode = defaultSkillConflictMode
	}
	m.outputDir = o.OutputDir
	if m.outputDir == "" && o.Execute {
		m.outputDir = pypath.PathJoin(o.TargetRoot, "migration", "openclaw", m.timestamp)
	}
	if m.outputDir != "" {
		m.archiveDir = pypath.PathJoin(m.outputDir, "archive")
		m.backupDir = pypath.PathJoin(m.outputDir, "backups")
		m.overflowDir = pypath.PathJoin(m.outputDir, "overflow")
	}
	ocConfig := m.loadOpenClawConfig()
	ws := strip(pyStrOrEmpty(or(dget(dget(dget(ocConfig, "agents", pyjson.NewObject()), "defaults", pyjson.NewObject()), "workspace", nil), "")))
	if ws != "" {
		expanded, eerr := pypath.PathExpanduser(ws)
		if eerr != nil {
			raise(eerr)
		}
		wsPath, rerr := pypath.Resolve(expanded)
		if rerr != nil {
			raise(rerr)
		}
		if isDir(wsPath) {
			if _, inside := pypath.PathRelativeTo(wsPath, m.sourceRoot); !inside {
				m.customWorkspace = wsPath
			}
		}
	}
	cfg, cerr := LoadYAMLFile(pypath.PathJoin(m.targetRoot, "config.yaml"))
	if cerr != nil {
		var cre *ConfigReadError
		if !errors.As(cerr, &cre) {
			raise(cerr)
		}
		cfg = newYAMLMap()
	}
	var memCfg any = newYAMLMap()
	if v := dget(cfg, "memory", nil); isDict(v) {
		memCfg = v
	}
	if m.memoryLimit, err = pyInt(dget(memCfg, "memory_char_limit", int64(DefaultMemoryCharLimit))); err != nil {
		return nil, err
	}
	if m.userLimit, err = pyInt(dget(memCfg, "user_char_limit", int64(DefaultUserCharLimit))); err != nil {
		return nil, err
	}
	if !containsString(SkillConflictModes, m.skillConflictMode) {
		return nil, &PyError{Type: "ValueError", Msg: "Unknown skill conflict mode: " + m.skillConflictMode + ". Valid modes: " + strings.Join(SkillConflictModes, ", ")}
	}
	return m, nil
}

// Text of a str value, AttributeError for any other type the way str.strip fails
func pyStrOrEmpty(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	raisef("AttributeError", "expected text, got %s", errchain.Kind(v))
	return ""
}

// Reports whether an option takes part in this run
func (m *Migrator) isSelected(id string) bool { return m.selected[id] }

// Appends one item, a conflict or error on config.yaml blocking later config writes
func (m *Migrator) record(kind, source, destination, status, reason string, details ...any) {
	m.recordItem(kind, strOrNone(source), strOrNone(destination), status, reason, details...)
}

// Appends one item with explicit None handling of source and destination
func (m *Migrator) recordItem(kind string, source, destination *string, status, reason string, details ...any) {
	d := pyjson.NewObject()
	sensitive := false
	for i := 0; i+1 < len(details); i += 2 {
		key := details[i].(string)
		if key == "sensitive" {
			sensitive = truthy(details[i+1])
			continue
		}
		d.Set(key, toJSON(details[i+1]))
	}
	m.items = append(m.items, ItemResult{Kind: kind, Source: source, Destination: destination, Status: status, Reason: reason, Details: d, Sensitive: sensitive})
	if (status == StatusConflict || status == StatusError) && destination != nil {
		if strings.HasSuffix(*destination, "config.yaml") || strings.HasSuffix(*destination, "config.yml") {
			m.configApplyBlocked = true
		}
	}
}

// First existing source path among the candidates, trying the renamed workspace folders and the configured workspace
func (m *Migrator) sourceCandidate(relativePaths ...string) string {
	for _, rel := range relativePaths {
		candidate := pypath.PathJoin(m.sourceRoot, rel)
		if exists(candidate) {
			return candidate
		}
		if suffix, ok := strings.CutPrefix(rel, "workspace/"); ok {
			for _, variant := range []string{"workspace-main", "workspace-assistant"} {
				if alt := pypath.PathJoin(m.sourceRoot, variant, suffix); exists(alt) {
					return alt
				}
			}
		} else if suffix, ok := strings.CutPrefix(rel, "workspace.default/"); ok {
			if alt := pypath.PathJoin(m.sourceRoot, "workspace-main", suffix); exists(alt) {
				return alt
			}
		}
	}
	if m.customWorkspace != "" {
		for _, rel := range relativePaths {
			for _, prefix := range []string{"workspace/", "workspace.default/"} {
				if suffix, ok := strings.CutPrefix(rel, prefix); ok {
					if alt := pypath.PathJoin(m.customWorkspace, suffix); exists(alt) {
						return alt
					}
					break
				}
			}
		}
	}
	return ""
}

// Skill destination, a free -imported name when renaming onto an existing folder
func (m *Migrator) resolveSkillDestination(destination string) string {
	if m.skillConflictMode != "rename" || !exists(destination) {
		return destination
	}
	name := pypath.PathName(destination)
	parent := pypath.PathParent(destination)
	candidate := pypath.PathJoin(parent, name+"-imported")
	for counter := 2; exists(candidate); counter++ {
		candidate = pypath.PathJoin(parent, fmt.Sprintf("%s-imported-%d", name, counter))
	}
	return candidate
}

// Runs every selected step and returns the report, written to the output directory when there is one
func (m *Migrator) Migrate() (report *Report, err error) {
	defer catch(&err)
	if !exists(m.sourceRoot) {
		m.record("source", m.sourceRoot, "", StatusError, "OpenClaw directory does not exist")
		return m.buildReport(), nil
	}
	cfg := m.loadOpenClawConfig()
	m.runIfSelected("soul", m.migrateSoul)
	m.runIfSelected("workspace-agents", m.migrateWorkspaceAgents)
	m.runIfSelected("memory", func() {
		m.migrateMemory(m.sourceCandidate("workspace/MEMORY.md", "workspace.default/MEMORY.md"),
			pypath.PathJoin(m.targetRoot, "memories", "MEMORY.md"), m.memoryLimit, "memory")
	})
	m.runIfSelected("user-profile", func() {
		m.migrateMemory(m.sourceCandidate("workspace/USER.md", "workspace.default/USER.md"),
			pypath.PathJoin(m.targetRoot, "memories", "USER.md"), m.userLimit, "user-profile")
	})
	m.runIfSelected("messaging-settings", func() { m.migrateMessagingSettings(cfg) })
	m.runIfSelected("secret-settings", func() { m.handleSecretSettings(cfg) })
	m.runIfSelected("discord-settings", func() { m.migrateDiscordSettings(cfg) })
	m.runIfSelected("slack-settings", func() { m.migrateSlackSettings(cfg) })
	m.runIfSelected("whatsapp-settings", func() { m.migrateWhatsAppSettings(cfg) })
	m.runIfSelected("signal-settings", func() { m.migrateSignalSettings(cfg) })
	m.runIfSelected("provider-keys", func() { m.handleProviderKeys(cfg) })
	m.runIfSelected("model-config", func() { m.migrateModelConfig(cfg) })
	m.runIfSelected("tts-config", func() { m.migrateTTSConfig(cfg) })
	m.runIfSelected("command-allowlist", m.migrateCommandAllowlist)
	m.runIfSelected("skills", m.migrateSkills)
	m.runIfSelected("shared-skills", m.migrateSharedSkills)
	m.runIfSelected("daily-memory", m.migrateDailyMemory)
	m.runIfSelected("tts-assets", func() {
		m.copyTreeNonDestructive(m.sourceCandidate("workspace/tts"), pypath.PathJoin(m.targetRoot, "tts"), "tts-assets",
			map[string]bool{".venv": true, "generated": true, "__pycache__": true})
	})
	m.runIfSelected("archive", m.archiveDocs)
	m.runIfSelected("mcp-servers", func() { m.migrateMCPServers(cfg) })
	m.runIfSelected("plugins-config", func() { m.migratePluginsConfig(cfg) })
	m.runIfSelected("cron-jobs", func() { m.migrateCronJobs(cfg) })
	m.runIfSelected("hooks-config", func() { m.migrateHooksConfig(cfg) })
	m.runIfSelected("agent-config", func() { m.migrateAgentConfig(cfg) })
	m.runIfSelected("gateway-config", func() { m.migrateGatewayConfig(cfg) })
	m.runIfSelected("session-config", func() { m.migrateSessionConfig(cfg) })
	m.runIfSelected("full-providers", func() { m.migrateFullProviders(cfg) })
	m.runIfSelected("deep-channels", func() { m.migrateDeepChannels(cfg) })
	m.runIfSelected("browser-config", func() { m.migrateBrowserConfig(cfg) })
	m.runIfSelected("tools-config", func() { m.migrateToolsConfig(cfg) })
	m.runIfSelected("approvals-config", func() { m.migrateApprovalsConfig(cfg) })
	m.runIfSelected("memory-backend", func() { m.migrateMemoryBackend(cfg) })
	m.runIfSelected("skills-config", func() { m.migrateSkillsConfig(cfg) })
	m.runIfSelected("ui-identity", func() { m.migrateUIIdentity(cfg) })
	m.runIfSelected("logging-config", func() { m.migrateLoggingConfig(cfg) })
	m.generateMigrationNotes()
	return m.buildReport(), nil
}

// Runs one option, recording it skipped when unselected or blocked and an error when config.yaml cannot be read
func (m *Migrator) runIfSelected(optionID string, fn func()) {
	meta, _ := OptionByID(optionID)
	if !m.isSelected(optionID) {
		m.record(optionID, "", "", StatusSkipped, "Not selected for this run", "option_label", meta.Label)
		return
	}
	if m.execute && m.configApplyBlocked && configMutatingOptions[optionID] {
		m.record(optionID, "", "", StatusSkipped, ReasonBlockedByApplyConflict, "option_label", meta.Label)
		return
	}
	err := try(fn)
	if err == nil {
		return
	}
	var cre *ConfigReadError
	if !errors.As(err, &cre) {
		raise(err)
	}
	m.record(optionID, "", pypath.PathJoin(m.targetRoot, "config.yaml"), StatusError, cre.Error(), "option_label", meta.Label)
}

// Report of every recorded item, written to the output directory when there is one
func (m *Migrator) buildReport() *Report {
	summary := newSummary()
	for _, item := range m.items {
		summary.add(item.Status, 1)
	}
	mode := "dry-run"
	if m.execute {
		mode = "execute"
	}
	selected := sortedKeys(m.selected)
	r := &Report{
		Timestamp:         m.timestamp,
		Mode:              mode,
		SourceRoot:        pypath.PathStr(m.sourceRoot),
		TargetRoot:        pypath.PathStr(m.targetRoot),
		WorkspaceTarget:   strOrNone(m.workspaceTarget),
		OutputDir:         strOrNone(m.outputDir),
		MigrateSecrets:    m.migrateSecrets,
		Preset:            strOrNone(m.presetName),
		SkillConflictMode: m.skillConflictMode,
		Selected:          selected,
		Summary:           summary,
		Items:             append([]ItemResult(nil), m.items...),
		Warnings:          m.buildWarnings(summary),
		NextSteps:         m.buildNextSteps(summary),
	}
	if m.outputDir != "" {
		if err := WriteReport(m.outputDir, r.JSON()); err != nil {
			raise(err)
		}
	}
	return r
}

// Actionable warnings of the report
func (m *Migrator) buildWarnings(summary *Summary) []string {
	warnings := []string{}
	if summary.Get(StatusConflict) > 0 {
		warnings = append(warnings, "Conflicts were found. Re-run with --overwrite to replace conflicting targets after item-level backups.")
	}
	if summary.Get(StatusError) > 0 {
		warnings = append(warnings, "One or more items failed. Inspect the report and re-run after fixing the underlying cause.")
	}
	if m.configApplyBlocked && m.execute {
		warnings = append(warnings, "A config.yaml write hit a conflict or error mid-apply; later config items were skipped to avoid a partial write.")
	}
	providerKeysSkipped := false
	for _, item := range m.items {
		if item.Kind == "provider-keys" && item.Status == StatusSkipped {
			providerKeysSkipped = true
		}
	}
	if providerKeysSkipped && !m.migrateSecrets {
		warnings = append(warnings, "API keys and other credentials were detected but not imported. Re-run with --migrate-secrets to copy supported keys into the Icebath env file.")
	}
	return warnings
}

// Next step guidance of the report
func (m *Migrator) buildNextSteps(summary *Summary) []string {
	if !m.execute {
		return []string{
			"Re-run without --dry-run to apply the migration.",
			"Pass --overwrite to resolve conflicts, or --migrate-secrets to include API keys.",
		}
	}
	steps := []string{}
	if summary.Get(StatusMigrated) > 0 {
		if m.outputDir != "" {
			steps = append(steps, "Review the migration report at "+m.outputDir+"/summary.md")
		} else {
			steps = append(steps, "Review the migration report.")
		}
		steps = append(steps, "Start a new Icebath session (or /reset) to pick up the imported config.")
	}
	if summary.Get(StatusConflict) > 0 {
		steps = append(steps, "Re-run with --overwrite to apply items that were blocked by conflicts.")
	}
	return steps
}

// Copies an existing target into the backups directory during apply, empty when nothing was copied
func (m *Migrator) maybeBackup(path string) string {
	if !m.execute || m.backupDir == "" || !exists(path) {
		return ""
	}
	return backupExisting(path, m.backupDir)
}

// Writes entries that overflowed a memory limit to the overflow directory, empty when there is nothing or nowhere to write
func (m *Migrator) writeOverflowEntries(kind string, entries []string) string {
	if len(entries) == 0 || m.overflowDir == "" {
		return ""
	}
	mkdirAll(m.overflowDir)
	path := pypath.PathJoin(m.overflowDir, strings.ReplaceAll(kind, "-", "_")+"_overflow.txt")
	writeText(path, strings.Join(entries, "\n")+"\n")
	return path
}

// Copies one file onto its destination, refusing an existing differing target unless overwriting or replaceable
func (m *Migrator) copyFile(source, destination, kind string, transform func(string) string, replaceable bool) {
	if source == "" || !exists(source) {
		return
	}
	if exists(destination) {
		if transform == nil && mustSHA(source) == mustSHA(destination) {
			m.record(kind, source, destination, StatusSkipped, "Target already matches source")
			return
		}
		if !m.overwrite && !replaceable {
			m.record(kind, source, destination, StatusConflict, ReasonTargetExists)
			return
		}
	}
	if !m.execute {
		m.record(kind, source, destination, StatusMigrated, "Would copy")
		return
	}
	backupPath := m.maybeBackup(destination)
	ensureParent(destination)
	if transform != nil {
		writeText(destination, transform(mustReadText(source)))
		if err := copyStat(source, destination); err != nil {
			raise(err)
		}
	} else if err := pyshutil.Copy2(source, destination); err != nil {
		raise(err)
	}
	var backup any
	if backupPath != "" {
		backup = backupPath
	}
	m.record(kind, source, destination, StatusMigrated, "", "backup", backup)
}

// Hex sha256 of a file, the read failure raised
func mustSHA(path string) string {
	sum, err := sha256File(path)
	if err != nil {
		raise(err)
	}
	return sum
}

// Imports SOUL.md, replacing a seeded default without needing overwrite
func (m *Migrator) migrateSoul() {
	source := m.sourceCandidate("workspace/SOUL.md", "workspace.default/SOUL.md")
	destination := pypath.PathJoin(m.targetRoot, "SOUL.md")
	if source == "" {
		m.record("soul", "", destination, StatusSkipped, "No OpenClaw SOUL.md found")
		return
	}
	replaceable := isFile(destination) && isIcebathSeededSoul(mustReadText(destination))
	m.copyFile(source, destination, "soul", RebrandText, replaceable)
}

// Copies the workspace instructions file into the chosen workspace target
func (m *Migrator) migrateWorkspaceAgents() {
	source := m.sourceCandidate("workspace/"+workspaceInstructions, "workspace.default/"+workspaceInstructions)
	if source == "" {
		m.record("workspace-agents", "workspace/AGENTS.md", "", StatusSkipped, "Source file not found")
		return
	}
	if m.workspaceTarget == "" {
		m.record("workspace-agents", source, "", StatusSkipped, "No workspace target was provided")
		return
	}
	m.copyFile(source, pypath.PathJoin(m.workspaceTarget, workspaceInstructions), "workspace-agents", RebrandText, false)
}

// Detail pairs of a memory merge in report order
func mergeDetails(stats MergeStats, limit int, merged []string) []any {
	final := 0
	if len(merged) > 0 {
		final = pystr.Len(strings.Join(merged, EntryDelimiter))
	}
	return []any{
		"existing_entries", int64(stats.Existing), "added_entries", int64(stats.Added),
		"duplicate_entries", int64(stats.Duplicates), "overflowed_entries", int64(stats.Overflowed),
		"char_limit", int64(limit), "final_char_count", int64(final),
	}
}

// First five overflowed entries as a JSON list
func overflowPreview(overflowed []string) []any {
	return stringsToAny(overflowed[:min(5, len(overflowed))])
}

// Merges one markdown source into a memory store under its character limit
func (m *Migrator) migrateMemory(source, destination string, limit int, kind string) {
	if source == "" || !exists(source) {
		m.record(kind, "", destination, StatusSkipped, "Source file not found")
		return
	}
	incoming := ExtractMarkdownEntries(mustReadText(source))
	if len(incoming) == 0 {
		m.record(kind, source, destination, StatusSkipped, "No importable entries found")
		return
	}
	for i, e := range incoming {
		incoming[i] = RebrandText(e)
	}
	existing := parseExistingMemoryEntries(destination)
	merged, stats, overflowed := MergeEntries(existing, incoming, limit)
	details := mergeDetails(stats, limit, merged)
	if overflowFile := m.writeOverflowEntries(kind, overflowed); overflowFile != "" {
		details = append(details, "overflow_file", overflowFile)
	}
	m.writeMergedMemory(kind, source, destination, merged, stats, overflowed, details, "Would merge entries")
}

// Writes a merged memory store during apply or records the preview
func (m *Migrator) writeMergedMemory(kind, source, destination string, merged []string, stats MergeStats, overflowed []string, details []any, previewReason string) {
	if !m.execute {
		m.record(kind, source, destination, StatusMigrated, previewReason, append([]any{"overflow_preview", overflowPreview(overflowed)}, details...)...)
		return
	}
	if stats.Added == 0 && len(overflowed) == 0 {
		m.record(kind, source, destination, StatusSkipped, "No new entries to import", details...)
		return
	}
	backupPath := m.maybeBackup(destination)
	ensureParent(destination)
	text := strings.Join(merged, EntryDelimiter)
	if len(merged) > 0 {
		text += "\n"
	}
	writeText(destination, text)
	m.record(kind, source, destination, StatusMigrated, "", append([]any{"backup", backupPath, "overflow_preview", overflowPreview(overflowed)}, details...)...)
}

// Merges OpenClaw exec approval patterns into command_allowlist
func (m *Migrator) migrateCommandAllowlist() {
	source := pypath.PathJoin(m.sourceRoot, "exec-approvals.json")
	destination := pypath.PathJoin(m.targetRoot, "config.yaml")
	if !exists(source) {
		m.record("command-allowlist", "", destination, StatusSkipped, "No OpenClaw exec approvals file found")
		return
	}
	text, err := readText(source)
	if err != nil {
		m.record("command-allowlist", source, destination, StatusError, "Could not read file: "+osErrText(err))
		return
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		m.record("command-allowlist", source, destination, StatusError, "Invalid JSON: "+err.Error())
		return
	}
	var patterns []any
	if agents := dget(data, "agents", pyjson.NewObject()); isDict(agents) {
		for _, agent := range ditems(agents) {
			var allowlist any = []any{}
			if isDict(agent.Val) {
				allowlist = dget(agent.Val, "allowlist", []any{})
			}
			for _, entry := range pyIter(allowlist) {
				var pattern any
				if isDict(entry) {
					pattern = dget(entry, "pattern", nil)
				}
				if truthy(pattern) {
					patterns = append(patterns, pattern)
				}
			}
		}
	}
	patterns = pySorted(uniqueInOrder(patterns))
	if len(patterns) == 0 {
		m.record("command-allowlist", source, destination, StatusSkipped, "No allowlist patterns found")
		return
	}
	if !exists(destination) {
		m.record("command-allowlist", source, destination, StatusSkipped, "Icebath config.yaml does not exist yet")
		return
	}
	cfg := loadYAML(destination)
	current, ok := dget(cfg, "command_allowlist", []any{}).([]any)
	if !ok {
		current = []any{}
	}
	merged := pySorted(uniqueInOrder(append(append([]any{}, current...), patterns...)))
	var added []any
	for _, p := range merged {
		if !contains(current, p) {
			added = append(added, p)
		}
	}
	if len(added) == 0 {
		m.record("command-allowlist", source, destination, StatusSkipped, "All patterns already present")
		return
	}
	if !m.execute {
		m.record("command-allowlist", source, destination, StatusMigrated, "Would merge patterns", "added_patterns", added)
		return
	}
	backupPath := m.maybeBackup(destination)
	dset(cfg, "command_allowlist", merged)
	dumpYAML(destination, cfg)
	m.record("command-allowlist", source, destination, StatusMigrated, "", "backup", backupPath, "added_patterns", added)
}

// Parsed openclaw.json, or the legacy clawdbot.json or moltbot.json, an empty object when none parses as a mapping
func (m *Migrator) loadOpenClawConfig() *pyjson.Object {
	for _, name := range []string{"openclaw.json", "clawdbot.json", "moltbot.json"} {
		path := pypath.PathJoin(m.sourceRoot, name)
		if !exists(path) {
			continue
		}
		text, err := readText(path)
		if err != nil {
			continue
		}
		data, err := pyjson.Loads(text)
		if err != nil {
			continue
		}
		if o, ok := data.(*pyjson.Object); ok {
			return o
		}
		return pyjson.NewObject()
	}
	return pyjson.NewObject()
}

// The OpenClaw config passed in, reloaded when it is empty
func (m *Migrator) cfgOr(cfg *pyjson.Object) *pyjson.Object {
	if cfg != nil && cfg.Len() > 0 {
		return cfg
	}
	return m.loadOpenClawConfig()
}

// Pairs of the OpenClaw .env file
func (m *Migrator) loadOpenClawEnv() map[string]string {
	return parseEnvMap(pypath.PathJoin(m.sourceRoot, ".env"))
}

// Merges env values into the target .env, a differing existing value a conflict unless overwriting
func (m *Migrator) mergeEnvValues(additions *envFile, kind, source string) {
	destination := pypath.PathJoin(m.targetRoot, ".env")
	envData := parseEnvFile(destination)
	added := map[string]bool{}
	conflicts := []any{}
	for _, key := range additions.keys {
		value := additions.vals[key]
		current, has := envData.get(key)
		if has && current == value {
			continue
		}
		if has && current != "" && !m.overwrite {
			conflicts = append(conflicts, key)
			continue
		}
		envData.set(key, value)
		added[key] = true
	}
	if len(conflicts) > 0 && len(added) == 0 {
		m.record(kind, source, destination, StatusConflict, "Destination .env already has different values", "conflicting_keys", conflicts)
		return
	}
	if len(conflicts) == 0 && len(added) == 0 {
		m.record(kind, source, destination, StatusSkipped, "All env values already present")
		return
	}
	addedKeys := stringsToAny(sortedKeys(added))
	if !m.execute {
		m.record(kind, source, destination, StatusMigrated, "Would merge env values", "added_keys", addedKeys, "conflicting_keys", conflicts)
		return
	}
	backupPath := m.maybeBackup(destination)
	saveEnvFile(destination, envData)
	m.record(kind, source, destination, StatusMigrated, "", "backup", backupPath, "added_keys", addedKeys, "conflicting_keys", conflicts)
}

// Fresh ordered env additions
func newAdditions() *envFile { return &envFile{vals: map[string]string{}} }

// Stripped non empty str() of every list element joined with commas
func joinUsers(list []any) string {
	var users []string
	for _, u := range list {
		if s := strip(pyStr(u)); s != "" {
			users = append(users, s)
		}
	}
	return strings.Join(users, ",")
}

// Imports the Telegram allowlist and maps the OpenClaw workspace onto terminal.cwd
func (m *Migrator) migrateMessagingSettings(cfg *pyjson.Object) {
	cfg = m.cfgOr(cfg)
	additions := newAdditions()
	workspace := dget(dget(dget(cfg, "agents", pyjson.NewObject()), "defaults", pyjson.NewObject()), "workspace", nil)
	envPath := pypath.PathJoin(m.targetRoot, ".env")
	allowlistPath := pypath.PathJoin(m.sourceRoot, "credentials", "telegram-default-allowFrom.json")
	if exists(allowlistPath) {
		text, rerr := readText(allowlistPath)
		var data any
		var jerr error
		if rerr == nil {
			data, jerr = pyjson.Loads(text)
		}
		switch {
		case rerr != nil:
			m.record("messaging-settings", allowlistPath, envPath, StatusError, "Could not read Telegram allowlist file: "+osErrText(rerr))
		case jerr != nil:
			m.record("messaging-settings", allowlistPath, envPath, StatusError, "Invalid JSON in Telegram allowlist file")
		default:
			if list, ok := dget(data, "allowFrom", []any{}).([]any); ok {
				if users := joinUsers(list); users != "" {
					additions.set("TELEGRAM_ALLOWED_USERS", users)
				}
			}
		}
	}
	if len(additions.keys) > 0 {
		m.mergeEnvValues(additions, "messaging-settings", pypath.PathJoin(m.sourceRoot, "openclaw.json"))
	} else {
		m.record("messaging-settings", pypath.PathJoin(m.sourceRoot, "openclaw.json"), envPath, StatusSkipped, "No Icebath-compatible messaging settings found")
	}
	if ws, ok := workspace.(string); ok && strip(ws) != "" {
		m.migrateWorkingDirectory(strip(ws))
	}
}

// Sets terminal.cwd to the OpenClaw workspace unless it lives inside the OpenClaw directory
func (m *Migrator) migrateWorkingDirectory(wsPath string) {
	source := pypath.PathJoin(m.sourceRoot, "openclaw.json")
	destination := pypath.PathJoin(m.targetRoot, "config.yaml")
	inside := false
	expanded, err := pypath.PathExpanduser(wsPath)
	if err != nil {
		raise(err)
	}
	if resolved, err := pypath.Resolve(expanded); err == nil {
		if root, err := pypath.Resolve(m.sourceRoot); err == nil {
			_, inside = pypath.PathRelativeTo(resolved, root)
		}
	}
	if inside {
		m.record("working-directory", source, destination, StatusSkipped, "OpenClaw's workspace is inside the OpenClaw directory; not used as Icebath's terminal.cwd")
		return
	}
	cfg := loadYAML(destination)
	var terminalCfg any = dget(cfg, "terminal", nil)
	if !isDict(terminalCfg) {
		terminalCfg = newYAMLMap()
	}
	current := strip(pyStr(or(dget(terminalCfg, "cwd", nil), "")))
	if current == wsPath {
		m.record("working-directory", source, destination, StatusSkipped, "terminal.cwd already set to the same value")
		return
	}
	if current != "" && current != "." && current != "auto" && current != "cwd" && !m.overwrite {
		m.record("working-directory", source, destination, StatusConflict, "terminal.cwd already set and overwrite is disabled", "current", current, "incoming", wsPath)
		return
	}
	if !m.execute {
		m.record("working-directory", source, destination, StatusMigrated, "Would set terminal.cwd", "cwd", wsPath)
		return
	}
	backupPath := m.maybeBackup(destination)
	dset(terminalCfg, "cwd", wsPath)
	dset(cfg, "terminal", terminalCfg)
	dumpYAML(destination, cfg)
	m.record("working-directory", source, destination, StatusMigrated, "", "backup", backupPath, "cwd", wsPath)
}

// Supported secret targets as a sorted JSON list
func supportedTargets() []any { return stringsToAny(SupportedSecretTargets) }

// Imports the allowlisted secrets when enabled, otherwise records why they were left out
func (m *Migrator) handleSecretSettings(cfg *pyjson.Object) {
	cfg = m.cfgOr(cfg)
	if m.migrateSecrets {
		m.migrateSecretSettings(cfg)
		return
	}
	configPath := pypath.PathJoin(m.sourceRoot, "openclaw.json")
	envPath := pypath.PathJoin(m.targetRoot, ".env")
	if exists(configPath) {
		m.record("secret-settings", configPath, envPath, StatusSkipped, "Secret migration disabled. Re-run with --migrate-secrets to import allowlisted secrets.", "supported_targets", supportedTargets())
	} else {
		m.record("secret-settings", configPath, envPath, StatusSkipped, "OpenClaw config file not found", "supported_targets", supportedTargets())
	}
}

// Imports the Telegram bot token
func (m *Migrator) migrateSecretSettings(cfg *pyjson.Object) {
	additions := newAdditions()
	tg := dget(dget(cfg, "channels", pyjson.NewObject()), "telegram", pyjson.NewObject())
	var token any
	if isDict(tg) {
		token = channelField(tg, "botToken")
	}
	if s, ok := token.(string); ok && strip(s) != "" {
		additions.set("TELEGRAM_BOT_TOKEN", strip(s))
	}
	if len(additions.keys) > 0 {
		m.mergeEnvValues(additions, "secret-settings", pypath.PathJoin(m.sourceRoot, "openclaw.json"))
		return
	}
	m.record("secret-settings", pypath.PathJoin(m.sourceRoot, "openclaw.json"), pypath.PathJoin(m.targetRoot, ".env"), StatusSkipped,
		"No allowlisted Icebath-compatible secrets found", "supported_targets", supportedTargets())
}

// Channel field from the flat layout, falling back to accounts.default
func channelField(ch any, field string) any {
	if v := dget(ch, field, nil); v != nil {
		return v
	}
	if accounts := dget(ch, "accounts", nil); isDict(accounts) {
		if def := dget(accounts, "default", nil); isDict(def) {
			return dget(def, field, nil)
		}
	}
	return nil
}

// Channel config under channels.name
func channelCfg(cfg *pyjson.Object, name string) any {
	return dget(dget(cfg, "channels", pyjson.NewObject()), name, pyjson.NewObject())
}

// Adds a stripped non empty str channel field under envKey
func addStrField(additions *envFile, ch any, field, envKey string) {
	if s, ok := channelField(ch, field).(string); ok && strip(s) != "" {
		additions.set(envKey, strip(s))
	}
}

// Adds the channel allowFrom list joined with commas under envKey
func addAllowFrom(additions *envFile, ch any, envKey string) {
	if list, ok := or(channelField(ch, "allowFrom"), []any{}).([]any); ok {
		if users := joinUsers(list); users != "" {
			additions.set(envKey, users)
		}
	}
}

// Merges channel additions into .env or records that the channel had nothing
func (m *Migrator) finishChannel(additions *envFile, kind, emptyReason string) {
	source := pypath.PathJoin(m.sourceRoot, "openclaw.json")
	if len(additions.keys) > 0 {
		m.mergeEnvValues(additions, kind, source)
		return
	}
	m.record(kind, source, pypath.PathJoin(m.targetRoot, ".env"), StatusSkipped, emptyReason)
}

// Imports the Discord bot token and allowlist
func (m *Migrator) migrateDiscordSettings(cfg *pyjson.Object) {
	cfg = m.cfgOr(cfg)
	additions := newAdditions()
	if ch := channelCfg(cfg, "discord"); isDict(ch) {
		addStrField(additions, ch, "token", "DISCORD_BOT_TOKEN")
		addAllowFrom(additions, ch, "DISCORD_ALLOWED_USERS")
	}
	m.finishChannel(additions, "discord-settings", "No Discord settings found")
}

// Imports the Slack bot and app tokens and allowlist
func (m *Migrator) migrateSlackSettings(cfg *pyjson.Object) {
	cfg = m.cfgOr(cfg)
	additions := newAdditions()
	if ch := channelCfg(cfg, "slack"); isDict(ch) {
		addStrField(additions, ch, "botToken", "SLACK_BOT_TOKEN")
		addStrField(additions, ch, "appToken", "SLACK_APP_TOKEN")
		addAllowFrom(additions, ch, "SLACK_ALLOWED_USERS")
	}
	m.finishChannel(additions, "slack-settings", "No Slack settings found")
}

// Imports the WhatsApp allowlist
func (m *Migrator) migrateWhatsAppSettings(cfg *pyjson.Object) {
	cfg = m.cfgOr(cfg)
	additions := newAdditions()
	if ch := channelCfg(cfg, "whatsapp"); isDict(ch) {
		addAllowFrom(additions, ch, "WHATSAPP_ALLOWED_USERS")
	}
	m.finishChannel(additions, "whatsapp-settings", "No WhatsApp settings found")
}

// Imports the Signal account, HTTP URL and allowlist
func (m *Migrator) migrateSignalSettings(cfg *pyjson.Object) {
	cfg = m.cfgOr(cfg)
	additions := newAdditions()
	if ch := channelCfg(cfg, "signal"); isDict(ch) {
		addStrField(additions, ch, "account", "SIGNAL_ACCOUNT")
		addStrField(additions, ch, "httpUrl", "SIGNAL_HTTP_URL")
		addAllowFrom(additions, ch, "SIGNAL_ALLOWED_USERS")
	}
	m.finishChannel(additions, "signal-settings", "No Signal settings found")
}

// Imports provider API keys when secrets are enabled, otherwise records why they were left out
func (m *Migrator) handleProviderKeys(cfg *pyjson.Object) {
	cfg = m.cfgOr(cfg)
	if !m.migrateSecrets {
		m.record("provider-keys", pypath.PathJoin(m.sourceRoot, "openclaw.json"), pypath.PathJoin(m.targetRoot, ".env"), StatusSkipped,
			"Secret migration disabled. Re-run with --migrate-secrets to import provider API keys.", "supported_targets", supportedTargets())
		return
	}
	m.migrateProviderKeys(cfg)
}

// OpenClaw env names copied into the Icebath env file, in lookup order
var envKeyMapping = []string{
	"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ELEVENLABS_API_KEY", "TELEGRAM_BOT_TOKEN",
	"DEEPSEEK_API_KEY", "GEMINI_API_KEY", "ZAI_API_KEY", "MINIMAX_API_KEY",
}

// Collects provider keys from models.providers, TTS settings, the OpenClaw .env, the env object and auth profiles
func (m *Migrator) migrateProviderKeys(cfg *pyjson.Object) {
	additions := newAdditions()
	openclawEnv := m.loadOpenClawEnv()
	configPath := pypath.PathJoin(m.sourceRoot, "openclaw.json")
	if providers := dget(dget(cfg, "models", pyjson.NewObject()), "providers", pyjson.NewObject()); isDict(providers) {
		for _, p := range ditems(providers) {
			if !isDict(p.Val) {
				continue
			}
			rawKey := dget(p.Val, "apiKey", nil)
			apiKey, ok := ResolveSecretInput(rawKey, openclawEnv)
			if !ok {
				if isDict(rawKey) {
					src := dget(rawKey, "source", nil)
					requireHashable(src)
					if eqStr(src, "file") || eqStr(src, "exec") {
						m.record("provider-keys", configPath, "", StatusSkipped,
							fmt.Sprintf("Provider '%s' uses a %s-backed SecretRef that cannot be auto-migrated. Add this key manually via: icebath config set", p.Key, pyStr(dindex(rawKey, "source"))))
					}
				}
				continue
			}
			baseURL := dget(p.Val, "baseUrl", "")
			apiType := dget(p.Val, "api", "")
			envVar := ""
			if s, isStr := baseURL.(string); isStr {
				switch low := lower(s); {
				case strings.Contains(low, "openrouter"):
					envVar = "OPENROUTER_API_KEY"
				case strings.Contains(low, "openai.com"):
					envVar = "OPENAI_API_KEY"
				case strings.Contains(low, "anthropic"):
					envVar = "ANTHROPIC_API_KEY"
				}
			}
			if envVar == "" && eqStr(apiType, "anthropic-messages") {
				envVar = "ANTHROPIC_API_KEY"
			}
			if envVar == "" {
				name := lower(p.Key)
				if name == "openrouter" {
					envVar = "OPENROUTER_API_KEY"
				} else if strings.Contains(name, "openai") {
					envVar = "OPENAI_API_KEY"
				}
			}
			if envVar != "" {
				additions.set(envVar, apiKey)
			}
		}
	}
	if tts := dget(dget(cfg, "messages", pyjson.NewObject()), "tts", pyjson.NewObject()); isDict(tts) {
		if el := dget(tts, "elevenlabs", pyjson.NewObject()); isDict(el) {
			if s, ok := dget(el, "apiKey", nil).(string); ok && strip(s) != "" {
				additions.set("ELEVENLABS_API_KEY", strip(s))
			}
		}
		if oai := dget(tts, "openai", pyjson.NewObject()); isDict(oai) {
			if s, ok := dget(oai, "apiKey", nil).(string); ok && strip(s) != "" {
				additions.set("VOICE_TOOLS_OPENAI_KEY", strip(s))
			}
		}
	}
	openclawEnv = m.loadOpenClawEnv()
	for _, key := range envKeyMapping {
		if val := strip(openclawEnv[key]); val != "" {
			if _, has := additions.get(key); !has {
				additions.set(key, val)
			}
		}
	}
	if jsonEnv := dget(cfg, "env", nil); isDict(jsonEnv) {
		sources := []any{jsonEnv}
		if vars := dget(jsonEnv, "vars", nil); isDict(vars) {
			sources = append(sources, vars)
		}
		for _, src := range sources {
			for _, key := range envKeyMapping {
				if s, ok := dget(src, key, nil).(string); ok && strip(s) != "" {
					if _, has := additions.get(key); !has {
						additions.set(key, strip(s))
					}
				}
			}
		}
	}
	m.collectAuthProfileKeys(additions)
	if len(additions.keys) > 0 {
		m.mergeEnvValues(additions, "provider-keys", configPath)
		return
	}
	m.record("provider-keys", configPath, pypath.PathJoin(m.targetRoot, ".env"), StatusSkipped, "No provider API keys found", "supported_targets", supportedTargets())
}

// Adds keys from agents/main/agent/auth-profiles.json, unreadable or invalid JSON ignored
func (m *Migrator) collectAuthProfileKeys(additions *envFile) {
	path := pypath.PathJoin(m.sourceRoot, "agents", "main", "agent", "auth-profiles.json")
	if !exists(path) {
		return
	}
	text, err := readText(path)
	if err != nil {
		return
	}
	profiles, err := pyjson.Loads(text)
	if err != nil || !isDict(profiles) {
		return
	}
	entries := profiles
	if inner := dget(profiles, "profiles", nil); isDict(inner) {
		entries = dget(profiles, "profiles", profiles)
	}
	for _, p := range ditems(entries) {
		if !isDict(p.Val) {
			continue
		}
		apiKey := or(dget(p.Val, "key", ""), dget(p.Val, "apiKey", ""))
		s, ok := apiKey.(string)
		if !ok || strip(s) == "" {
			continue
		}
		name := lower(p.Key)
		missing := func(key string) bool {
			_, has := additions.get(key)
			return !has
		}
		switch {
		case strings.Contains(name, "openrouter") && missing("OPENROUTER_API_KEY"):
			additions.set("OPENROUTER_API_KEY", strip(s))
		case strings.Contains(name, "openai") && missing("OPENAI_API_KEY"):
			additions.set("OPENAI_API_KEY", strip(s))
		case strings.Contains(name, "anthropic") && missing("ANTHROPIC_API_KEY"):
			additions.set("ANTHROPIC_API_KEY", strip(s))
		}
	}
}

// Sets model.default from agents.defaults.model, a catalog alias resolved to its API id
func (m *Migrator) migrateModelConfig(cfg *pyjson.Object) {
	cfg = m.cfgOr(cfg)
	destination := pypath.PathJoin(m.targetRoot, "config.yaml")
	sourcePath := pypath.PathJoin(m.sourceRoot, "openclaw.json")
	defaults := dget(dget(cfg, "agents", pyjson.NewObject()), "defaults", pyjson.NewObject())
	modelValue := dget(defaults, "model", nil)
	if modelValue == nil {
		m.record("model-config", sourcePath, destination, StatusSkipped, "No default model found in OpenClaw config")
		return
	}
	modelAny := modelValue
	if isDict(modelValue) {
		modelAny = dget(modelValue, "primary", nil)
	}
	modelStr, ok := modelAny.(string)
	if !ok || strip(modelStr) == "" {
		m.record("model-config", sourcePath, destination, StatusSkipped, "Default model value is empty or invalid")
		return
	}
	modelStr = strip(modelStr)
	catalog := dget(dget(dget(cfg, "agents", pyjson.NewObject()), "defaults", pyjson.NewObject()), "models", pyjson.NewObject())
	if isDict(catalog) && !contains(catalog, modelStr) {
		for _, e := range ditems(catalog) {
			if isDict(e.Val) && pyrepr.Equal(dget(e.Val, "alias", nil), modelStr) {
				modelStr = e.Key
				break
			}
			if s, isStr := e.Val.(string); isStr && s == modelStr {
				modelStr = e.Key
				break
			}
		}
	}
	icebathCfg := loadYAML(destination)
	currentModel := dget(icebathCfg, "model", nil)
	if pyrepr.Equal(currentModel, modelStr) {
		m.record("model-config", sourcePath, destination, StatusSkipped, "Model already set to the same value")
		return
	}
	if truthy(currentModel) && !m.overwrite {
		m.record("model-config", sourcePath, destination, StatusConflict, "Model already set and overwrite is disabled", "current", currentModel, "incoming", modelStr)
		return
	}
	if !m.execute {
		m.record("model-config", sourcePath, destination, StatusMigrated, "Would set model", "model", modelStr)
		return
	}
	backupPath := m.maybeBackup(destination)
	if existing := dget(icebathCfg, "model", nil); isDict(existing) {
		dset(existing, "default", modelStr)
	} else {
		fresh := newYAMLMap()
		dset(fresh, "default", modelStr)
		dset(icebathCfg, "model", fresh)
	}
	dumpYAML(destination, icebathCfg)
	m.record("model-config", sourcePath, destination, StatusMigrated, "", "backup", backupPath, "model", modelStr)
}

// Preferred provider block when a dict, else the talk provider block when a dict, else the legacy flat block
func ttsProviderBlock(providers, talkProviders, tts any, name string) any {
	if isDict(dget(providers, name, nil)) {
		return or(dget(providers, name, nil), pyjson.NewObject())
	}
	if isDict(dget(talkProviders, name, nil)) {
		return or(dget(talkProviders, name, nil), pyjson.NewObject())
	}
	return or(dget(tts, name, nil), pyjson.NewObject())
}

// Stripped non empty str value of the first truthy candidate
func firstStripped(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || strip(s) == "" {
		return "", false
	}
	return strip(s), true
}

// Merges the TTS provider and voice settings into the tts section
func (m *Migrator) migrateTTSConfig(cfg *pyjson.Object) {
	cfg = m.cfgOr(cfg)
	destination := pypath.PathJoin(m.targetRoot, "config.yaml")
	sourcePath := pypath.PathJoin(m.sourceRoot, "openclaw.json")
	tts := dget(dget(cfg, "messages", pyjson.NewObject()), "tts", pyjson.NewObject())
	if !isDict(tts) || !truthy(tts) {
		m.record("tts-config", sourcePath, destination, StatusSkipped, "No TTS configuration found in OpenClaw config")
		return
	}
	ttsData := newYAMLMap()
	provider := dget(tts, "provider", nil)
	if p, ok := provider.(string); ok && (p == "elevenlabs" || p == "openai" || p == "edge" || p == "microsoft") {
		if p == "microsoft" {
			p = "edge"
		}
		dset(ttsData, "provider", p)
	}
	providers := or(dget(tts, "providers", nil), pyjson.NewObject())
	talkCfg := or(dget(m.cfgOr(cfg), "talk", nil), pyjson.NewObject())
	talkProviders := or(dget(talkCfg, "providers", nil), pyjson.NewObject())
	if el := ttsProviderBlock(providers, talkProviders, tts, "elevenlabs"); isDict(el) {
		settings := newYAMLMap()
		if v, ok := firstStripped(orElse(dget(el, "voiceId", nil), lazyGet(talkCfg, "voiceId"))); ok {
			dset(settings, "voice_id", v)
		}
		if v, ok := firstStripped(orElse(dget(el, "modelId", nil), lazyGet(talkCfg, "modelId"))); ok {
			dset(settings, "model_id", v)
		}
		if len(settings) > 0 {
			dset(ttsData, "elevenlabs", settings)
		}
	}
	if oai := ttsProviderBlock(providers, talkProviders, tts, "openai"); isDict(oai) {
		settings := newYAMLMap()
		if v, ok := firstStripped(orElse(dget(oai, "model", nil), lazyGet(oai, "modelId"))); ok {
			dset(settings, "model", v)
		}
		if v, ok := firstStripped(dget(oai, "voice", nil)); ok {
			dset(settings, "voice", v)
		}
		if len(settings) > 0 {
			dset(ttsData, "openai", settings)
		}
	}
	var edge any
	if isDict(dget(providers, "edge", nil)) || isDict(dget(providers, "microsoft", nil)) {
		edge = or(dget(providers, "edge", nil), or(dget(providers, "microsoft", nil), pyjson.NewObject()))
	} else {
		edge = or(dget(tts, "edge", nil), or(dget(tts, "microsoft", nil), pyjson.NewObject()))
	}
	if isDict(edge) {
		if v, ok := firstStripped(dget(edge, "voice", nil)); ok {
			voice := newYAMLMap()
			dset(voice, "voice", v)
			dset(ttsData, "edge", voice)
		}
	}
	if len(ttsData) == 0 {
		m.record("tts-config", sourcePath, destination, StatusSkipped, "No compatible TTS settings found")
		return
	}
	settingKeys := stringsToAny(mappingKeysOf(ttsData))
	icebathCfg := loadYAML(destination)
	existing := dget(icebathCfg, "tts", newYAMLMap())
	if !isDict(existing) {
		existing = newYAMLMap()
	}
	if !m.execute {
		m.record("tts-config", sourcePath, destination, StatusMigrated, "Would set TTS config", "settings", settingKeys)
		return
	}
	backupPath := m.maybeBackup(destination)
	merged := copyYAMLMap(existing)
	for _, e := range ditems(ttsData) {
		if cur := dget(merged, e.Key, nil); isDict(e.Val) && isDict(cur) {
			combined := copyYAMLMap(cur)
			for _, inner := range ditems(e.Val) {
				dset(combined, inner.Key, inner.Val)
			}
			dset(merged, e.Key, combined)
		} else {
			dset(merged, e.Key, e.Val)
		}
	}
	dset(icebathCfg, "tts", merged)
	dumpYAML(destination, icebathCfg)
	m.record("tts-config", sourcePath, destination, StatusMigrated, "", "backup", backupPath, "settings", settingKeys)
}

// Deferred d.get(key) for the right side of a Python or, evaluated only when the left side is falsy
func lazyGet(d any, key string) func() any { return func() any { return dget(d, key, nil) } }

// Python a or b with b evaluated only when a is falsy
func orElse(a any, b func() any) any {
	if truthy(a) {
		return a
	}
	return b()
}

// Shallow copy of a dict as a YAML mapping keeping its key order
func copyYAMLMap(d any) map[string]any {
	out := newYAMLMap()
	for _, e := range ditems(d) {
		dset(out, e.Key, e.Val)
	}
	return out
}

// Keys of a YAML mapping in insertion order
func mappingKeysOf(d map[string]any) []string {
	items := ditems(d)
	out := make([]string, len(items))
	for i, e := range items {
		out[i] = e.Key
	}
	return out
}

// Imports managed, personal and project level shared skill directories
func (m *Migrator) migrateSharedSkills() {
	userHome, err := pypath.PathHome()
	if err != nil {
		raise(err)
	}
	sources := []struct{ root, kind, desc string }{
		{pypath.PathJoin(m.sourceRoot, "skills"), "shared-skills", "managed skills"},
		{pypath.PathJoin(userHome, ".agents", "skills"), "personal-skills", "personal cross-project skills"},
		{pypath.PathJoin(m.sourceRoot, "workspace", ".agents", "skills"), "project-skills", "project-level shared skills"},
		{pypath.PathJoin(m.sourceRoot, "workspace.default", ".agents", "skills"), "project-skills", "project-level shared skills"},
	}
	found := false
	for _, s := range sources {
		if exists(s.root) {
			found = true
			m.importSkillDirectory(s.root, s.kind, s.desc)
		}
	}
	if !found {
		m.record("shared-skills", "", pypath.PathJoin(m.targetRoot, "skills", SkillCategoryDirname), StatusSkipped, "No shared OpenClaw skills directories found")
	}
}

// Child directories of root holding a SKILL.md, sorted by name, the listing failure raised
func skillDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		raise(err)
	}
	var out []string
	for _, e := range entries {
		p := pypath.PathJoin(root, e.Name())
		if isDir(p) && exists(pypath.PathJoin(p, "SKILL.md")) {
			out = append(out, p)
		}
	}
	return out
}

// Copies each skill directory under openclaw-imports honouring the skill conflict mode
func (m *Migrator) copySkillDirs(dirs []string, kind, previewReason, renamedReason string) {
	destinationRoot := pypath.PathJoin(m.targetRoot, "skills", SkillCategoryDirname)
	for _, skillDir := range dirs {
		destination := pypath.PathJoin(destinationRoot, pypath.PathName(skillDir))
		final := destination
		if exists(destination) {
			if m.skillConflictMode == "skip" {
				m.record(kind, skillDir, destination, StatusConflict, "Destination skill already exists")
				continue
			}
			if m.skillConflictMode == "rename" {
				final = m.resolveSkillDestination(destination)
			}
		}
		if !m.execute {
			if final != destination {
				m.record(kind, skillDir, final, StatusMigrated, renamedReason, "renamed_from", destination)
			} else {
				m.record(kind, skillDir, final, StatusMigrated, previewReason)
			}
			continue
		}
		backupPath := ""
		if final == destination && exists(destination) {
			backupPath = m.maybeBackup(destination)
		}
		mkdirAll(pypath.PathParent(final))
		if final == destination && exists(destination) {
			if err := pyshutil.Rmtree(destination); err != nil {
				raise(err)
			}
		}
		if err := pyshutil.CopyTree(skillDir, final, false, false); err != nil {
			raise(err)
		}
		details := []any{"backup", backupPath}
		if final != destination {
			details = append(details, "renamed_from", destination)
		}
		m.record(kind, skillDir, final, StatusMigrated, "", details...)
	}
}

// Writes the openclaw-imports category description when missing and reports it on preview and apply
func (m *Migrator) ensureCategoryDescription(kind string) {
	descPath := pypath.PathJoin(m.targetRoot, "skills", SkillCategoryDirname, "DESCRIPTION.md")
	if exists(descPath) {
		return
	}
	reason := "Would create category description"
	if m.execute {
		mkdirAll(pypath.PathParent(descPath))
		writeText(descPath, SkillCategoryDescription+"\n")
		reason = ""
	}
	m.record(kind, "", descPath, StatusMigrated, reason)
}

// Imports the skills of one shared skills directory
func (m *Migrator) importSkillDirectory(sourceRoot, kindLabel, desc string) {
	dirs := skillDirs(sourceRoot)
	if len(dirs) == 0 {
		m.record(kindLabel, sourceRoot, pypath.PathJoin(m.targetRoot, "skills", SkillCategoryDirname), StatusSkipped, "No skills with SKILL.md found in "+desc)
		return
	}
	m.copySkillDirs(dirs, kindLabel, "Would copy "+desc+" directory", "Would copy "+desc+" directory under a renamed folder")
	m.ensureCategoryDescription("shared-skill-category")
}

// Merges every workspace/memory markdown file into MEMORY.md
func (m *Migrator) migrateDailyMemory() {
	sourceDir := m.sourceCandidate("workspace/memory")
	destination := pypath.PathJoin(m.targetRoot, "memories", "MEMORY.md")
	if sourceDir == "" || !isDir(sourceDir) {
		m.record("daily-memory", "", destination, StatusSkipped, "No workspace/memory/ directory found")
		return
	}
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		raise(err)
	}
	var mdFiles []string
	for _, e := range entries {
		p := pypath.PathJoin(sourceDir, e.Name())
		if isFile(p) && pypath.PathSuffix(p) == ".md" {
			mdFiles = append(mdFiles, p)
		}
	}
	if len(mdFiles) == 0 {
		m.record("daily-memory", sourceDir, destination, StatusSkipped, "No .md files found in workspace/memory/")
		return
	}
	var incoming []string
	for _, f := range mdFiles {
		text, rerr := readText(f)
		if rerr != nil {
			continue
		}
		incoming = append(incoming, ExtractMarkdownEntries(text)...)
	}
	if len(incoming) == 0 {
		m.record("daily-memory", sourceDir, destination, StatusSkipped, "No importable entries found in daily memory files")
		return
	}
	for i, e := range incoming {
		incoming[i] = RebrandText(e)
	}
	existing := parseExistingMemoryEntries(destination)
	merged, stats, overflowed := MergeEntries(existing, incoming, m.memoryLimit)
	details := append([]any{"source_files", int64(len(mdFiles))}, mergeDetails(stats, m.memoryLimit, merged)...)
	if overflowFile := m.writeOverflowEntries("daily-memory", overflowed); overflowFile != "" {
		details = append(details, "overflow_file", overflowFile)
	}
	m.writeMergedMemory("daily-memory", sourceDir, destination, merged, stats, overflowed, details, "Would merge daily memory entries")
}

// Imports workspace skills under openclaw-imports
func (m *Migrator) migrateSkills() {
	sourceRoot := m.sourceCandidate("workspace/skills")
	destinationRoot := pypath.PathJoin(m.targetRoot, "skills", SkillCategoryDirname)
	if sourceRoot == "" || !exists(sourceRoot) {
		m.record("skills", "", destinationRoot, StatusSkipped, "No OpenClaw skills directory found")
		return
	}
	dirs := skillDirs(sourceRoot)
	if len(dirs) == 0 {
		m.record("skills", sourceRoot, destinationRoot, StatusSkipped, "No skills with SKILL.md found")
		return
	}
	m.copySkillDirs(dirs, "skill", "Would copy skill directory", "Would copy skill directory under a renamed folder")
	m.ensureCategoryDescription("skill-category")
}

// Files under root in pathlib rglob order, each directory listing its children before its subdirectories are walked
func rglobFiles(root string) []string {
	var dirs []string
	var walk func(dir string)
	walk = func(dir string) {
		dirs = append(dirs, dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				walk(pypath.PathJoin(dir, e.Name()))
			}
		}
	}
	walk(root)
	var files []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if p := pypath.PathJoin(dir, e.Name()); isFile(p) {
				files = append(files, p)
			}
		}
	}
	return files
}

// Copies a tree file by file without replacing differing files unless overwriting
func (m *Migrator) copyTreeNonDestructive(sourceRoot, destinationRoot, kind string, ignoreDirNames map[string]bool) {
	if sourceRoot == "" || !exists(sourceRoot) {
		m.record(kind, "", destinationRoot, StatusSkipped, "Source directory not found")
		return
	}
	var files []string
	for _, p := range rglobFiles(sourceRoot) {
		rel, _ := pypath.PathRelativeTo(p, sourceRoot)
		_, parts := pypath.PathParts(rel)
		ignored := false
		for _, part := range parts[:max(len(parts)-1, 0)] {
			if ignoreDirNames[part] {
				ignored = true
			}
		}
		if !ignored {
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		m.record(kind, sourceRoot, destinationRoot, StatusSkipped, "No files found")
		return
	}
	copied, unchanged, conflicts := 0, 0, 0
	for _, source := range files {
		rel, _ := pypath.PathRelativeTo(source, sourceRoot)
		destination := pypath.PathJoin(destinationRoot, rel)
		if exists(destination) {
			if mustSHA(source) == mustSHA(destination) {
				unchanged++
				continue
			}
			if !m.overwrite {
				conflicts++
				m.record(kind, source, destination, StatusConflict, "Destination file already exists")
				continue
			}
		}
		if m.execute {
			m.maybeBackup(destination)
			ensureParent(destination)
			if err := pyshutil.Copy2(source, destination); err != nil {
				raise(err)
			}
		}
		copied++
	}
	status, reason := StatusSkipped, ""
	if copied > 0 {
		status = StatusMigrated
	}
	if copied == 0 && conflicts > 0 {
		status, reason = StatusConflict, "All candidate files conflicted with existing destination files"
	} else if copied == 0 {
		reason = "No new files to copy"
	}
	m.record(kind, sourceRoot, destinationRoot, status, reason, "copied_files", int64(copied), "unchanged_files", int64(unchanged), "conflicts", int64(conflicts))
}

const archiveReason = "No direct Icebath destination; archived for manual review"

// Archives unmapped workspace docs and records the raw and sensitive files that were left behind
func (m *Migrator) archiveDocs() {
	for _, name := range []string{"IDENTITY.md", "TOOLS.md", "HEARTBEAT.md", "BOOTSTRAP.md"} {
		if c := m.sourceCandidate("workspace/"+name, "workspace.default/"+name); c != "" {
			m.archivePath(c, archiveReason)
		}
	}
	for _, rel := range []string{"workspace/.learnings", "workspace/memory"} {
		if c := pypath.PathJoin(m.sourceRoot, rel); exists(c) {
			m.archivePath(c, archiveReason)
		}
	}
	partial := []struct{ rel, reason string }{
		{"openclaw.json", "Selected Icebath-compatible values were extracted; raw OpenClaw config was not copied."},
		{"credentials/telegram-default-allowFrom.json", "Selected Icebath-compatible values were extracted; raw credentials file was not copied."},
	}
	for _, p := range partial {
		if c := pypath.PathJoin(m.sourceRoot, p.rel); exists(c) {
			m.record("raw-config-skip", c, "", StatusSkipped, p.reason)
		}
	}
	for _, rel := range []string{"memory/main.sqlite", "credentials", "devices", "identity", "workspace.zip"} {
		if c := pypath.PathJoin(m.sourceRoot, rel); exists(c) {
			m.record("sensitive-skip", c, "", StatusSkipped, "Contains secrets, binary state, or product-specific runtime data")
		}
	}
}

// Copies one source path into the archive directory during apply and records it archived
func (m *Migrator) archivePath(source, reason string) {
	destination := ""
	if m.archiveDir != "" {
		destination = pypath.PathJoin(m.archiveDir, relativeLabel(source, m.sourceRoot))
	}
	if m.execute && destination != "" {
		ensureParent(destination)
		if isDir(source) {
			if err := pyshutil.CopyTree(source, destination, false, true); err != nil {
				raise(err)
			}
		} else if err := pyshutil.Copy2(source, destination); err != nil {
			raise(err)
		}
	}
	m.record("archive", source, destination, StatusArchived, reason)
}
