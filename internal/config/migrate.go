package config

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

// Oldest stamped schema version the migrations still upgrade, older files are left untouched
const SupportFloorVersion = 12

// Kind of answer a migration question takes
type PromptKind int

// Kinds of migration questions
const (
	PromptLine PromptKind = iota
	PromptSecret
	PromptYesNo
)

// Question a migration asks the person, its text shown as the input prompt
type Prompt struct {
	Kind PromptKind
	Text string
}

// How one migration pass runs, Ask answering its questions and Emit receiving each line as it arises
type MigrateOptions struct {
	Interactive bool
	Quiet       bool
	Ask         func(ctx context.Context, p Prompt) (string, error)
	Emit        func(Line)
}

// What one migration pass did, with every line it printed in order
type MigrationReport struct {
	EnvAdded    []string
	ConfigAdded []string
	Warnings    []string
	Notices     []string
	Lines       []Line
}

// State of one upgrade pass over the config of one home
type migration struct {
	ctx        context.Context
	s          *Store
	home       string
	report     *MigrationReport
	quiet      bool
	configOnly bool
	emit       func(Line)
}

// Records an output row and hands it to the receiver
func (m *migration) line(l Line) {
	m.report.Lines = append(m.report.Lines, l)
	if m.emit != nil {
		m.emit(l)
	}
}

// Prints a plain line unless the pass is quiet
func (m *migration) say(text string) {
	if !m.quiet {
		m.line(plainLine(text))
	}
}

// Raw user config as a mapping the step may change
func (m *migration) raw() (*yaml.Node, error) {
	doc, err := m.s.RawDocument(m.ctx)
	if err != nil {
		return nil, err
	}
	if !isMapping(doc.root) {
		return emptyMapping(), nil
	}
	return doc.root, nil
}

// Persists a migrated config, only values differing from the defaults written, in memory under a backend without a file
func (m *migration) persist(config *yaml.Node) error {
	b, err := m.s.backend(m.ctx)
	if err != nil {
		return err
	}
	doc := &YAMLDocument{root: config}
	if !b.SupportsFileTooling() {
		return b.ApplyInMemory(m.ctx, m.home, doc)
	}
	return m.s.SaveDocument(AppliedWrite(m.ctx), doc)
}

// Persists config, records what was added and prints the message unless quiet
func (m *migration) commit(config *yaml.Node, added, message string) error {
	if err := m.persist(config); err != nil {
		return err
	}
	if added != "" {
		m.report.ConfigAdded = append(m.report.ConfigAdded, added)
	}
	if message != "" {
		m.say(message)
	}
	return nil
}

// Mapping under key of config, a fresh unattached mapping when the key holds anything else
func dictAt(config *yaml.Node, key string) *yaml.Node {
	if v := childOf(config, key); isMapping(v) {
		return v
	}
	return emptyMapping()
}

// Integer scalar node
func intNode(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagInt, Value: fmt.Sprint(v)}
}

// Boolean scalar node
func boolNode(v bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagBool, Value: fmt.Sprint(v)}
}

// One rewrite of a single key of a section when its current value matches
type keyRewrite struct {
	section       string
	key           string
	match         func(cur *yaml.Node) bool
	guard         func(raw *yaml.Node) bool
	value         *yaml.Node
	added         string
	message       string
	createSection bool
}

// Applies a key change, a nil value deleting the key and a missing section skipped unless creatable
func (m *migration) rewrite(r keyRewrite) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	raw := childOf(config, r.section)
	if !isMapping(raw) {
		if !r.createSection {
			return nil
		}
		raw = emptyMapping()
	}
	if !r.match(childOf(raw, r.key)) || (r.guard != nil && !r.guard(raw)) {
		return nil
	}
	if r.value == nil {
		dropKey(raw, r.key)
	} else {
		putKey(raw, r.key, plainCopy(r.value))
	}
	putKey(config, r.section, raw)
	return m.commit(config, r.added, r.message)
}

// Match for a value still equal to an old default, numbers compared by value
func stillEquals(old *yaml.Node) func(cur *yaml.Node) bool {
	return func(cur *yaml.Node) bool { return cur != nil && valueEqual(cur, old, false) }
}

// Match for a text equal to word once trimmed and lowered
func lowerIs(word string) func(cur *yaml.Node) bool {
	return func(cur *yaml.Node) bool {
		return isStringNode(cur) && strings.ToLower(trimEnvSpace(cur.Value)) == word
	}
}

// Reports a section holding key
func hasKey(key string) func(raw *yaml.Node) bool {
	return func(raw *yaml.Node) bool { return mappingIndex(raw, key) >= 0 }
}

// Whole number a config value converts to, floats truncated and digit text parsed, ok false otherwise
func wholeOf(n *yaml.Node) (int, bool) {
	if n == nil || n.Kind != yaml.ScalarNode || isNull(n) {
		return 0, false
	}
	tag, text := scalarKind(n)
	switch tag {
	case tagInt, tagFloat, tagBool:
		return int(numberOf(n)), true
	}
	v, ok := wholeNumberLiteral(text)
	if !ok || !v.IsInt64() {
		return 0, false
	}
	return int(v.Int64()), true
}

// Items of a list valued setting as text, bracketed list text read as that list, bare text as one name
func configStringList(n *yaml.Node) []string {
	switch {
	case isStringNode(n):
		if items, ok := bracketedList(n.Value); ok {
			return items
		}
		return []string{n.Value}
	case isSequence(n):
		out := make([]string, 0, len(n.Content))
		for _, it := range n.Content {
			out = append(out, displayText(it))
		}
		return out
	}
	return nil
}

// Items of a text spelling a bracketed list, ok false when it does not parse as one
func bracketedList(text string) ([]string, bool) {
	text = trimEnvSpace(text)
	if !strings.HasPrefix(text, "[") {
		return nil, false
	}
	var parsed yaml.Node
	if err := LoadYAML([]byte(text), &parsed); err != nil || !isSequence(&parsed) {
		return nil, false
	}
	out := make([]string, 0, len(parsed.Content))
	for _, it := range parsed.Content {
		out = append(out, displayText(it))
	}
	return out, true
}

// Toolset list a saved platform_toolsets value spells, ok false for any shape that is no list
func platformToolsets(n *yaml.Node) ([]string, bool) {
	switch {
	case isSequence(n):
		out := make([]string, 0, len(n.Content))
		for _, it := range n.Content {
			out = append(out, displayText(it))
		}
		return out, true
	case isStringNode(n):
		return bracketedList(n.Value)
	}
	return nil, false
}

// Sequence node of text items
func stringSeq(items []string) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq}
	for _, it := range items {
		seq.Content = append(seq.Content, stringNode(it))
	}
	return seq
}

// Sorted distinct texts
func sortedSet(items ...string) []string {
	out := slices.Clone(items)
	slices.Sort(out)
	return slices.Compact(out)
}

// One registered migration step with the schema version it upgrades to
type migrationStep struct {
	version int
	run     func(m *migration) error
}

// Step applying one key rewrite
func rewriteStep(version int, r keyRewrite) migrationStep {
	return migrationStep{version, func(m *migration) error { return m.rewrite(r) }}
}

// Every migration step in strictly ascending version order
func migrationSteps() []migrationStep {
	return []migrationStep{
		{12, migrateTo12}, {13, migrateTo13}, {14, migrateTo14}, {16, migrateTo16}, {17, migrateTo17}, {21, migrateTo21}, {23, migrateTo23},
		rewriteStep(25, keyRewrite{section: "model_catalog", key: "ttl_hours", match: stillEquals(intNode(24)), value: intNode(1),
			added: "model_catalog.ttl_hours 24→1", message: "  ✓ Lowered model_catalog.ttl_hours to 1 (hourly picker refresh)"}),
		{29, migrateTo29},
		rewriteStep(31, keyRewrite{section: "agent", key: "verify_on_stop", value: boolNode(false), createSection: true,
			match: func(cur *yaml.Node) bool { return cur == nil || isNull(cur) || lowerIs("auto")(cur) },
			added: "agent.verify_on_stop=false",
			message: "  ✓ Turned off verify-on-stop (agent.verify_on_stop: false). " +
				"Set it to true to re-enable, or \"auto\" for the legacy surface-aware behavior."}),
		rewriteStep(32, keyRewrite{section: "agent", key: "verify_on_stop", match: stillEquals(boolNode(true)), value: boolNode(false),
			guard: func(raw *yaml.Node) bool { v, ok := boolScalar(childOf(raw, "verify_on_stop")); return ok && v },
			added: "agent.verify_on_stop=false",
			message: "  ✓ Turned off verify-on-stop (agent.verify_on_stop: false) — " +
				"the old default was written into your config as a literal true. Set it to true again to re-enable, " +
				"or \"auto\" for the legacy surface-aware behavior."}),
		{33, migrateTo33}, {34, migrateTo34},
		rewriteStep(35, keyRewrite{section: "display", key: "background_process_notifications", match: lowerIs("all"), value: stringNode("concise"),
			added: "display.background_process_notifications=concise (was: all)",
			message: "  ✓ Background process notifications switched from 'all' to 'concise' — completions now show a one-line status message " +
				"instead of the raw output dump. Set display.background_process_notifications: all to restore the old behavior."}),
		rewriteStep(36, keyRewrite{section: "delegation", key: "max_iterations", match: stillEquals(intNode(50)), value: intNode(250),
			added: "delegation.max_iterations=250 (was: 50)",
			message: "  ✓ Raised delegation.max_iterations from 50 to 250 — subagents now get a larger per-child tool-call budget so delegated work " +
				"finishes instead of truncating. Set delegation.max_iterations back to 50 to restore the old cap."}),
		rewriteStep(37, keyRewrite{section: "delegation", key: "max_concurrent_children", match: stillEquals(intNode(3)), value: intNode(10),
			added: "delegation.max_concurrent_children=10 (was: 3)",
			message: "  ✓ Raised delegation.max_concurrent_children from 3 to 10 — independent delegated children now fan out wider in parallel. " +
				"Each child consumes API tokens independently; set delegation.max_concurrent_children back to 3 to restore the old cap."}),
		{38, migrateTo38}, {39, migrateTo39},
		rewriteStep(40, keyRewrite{section: "model_catalog", key: "ttl_hours", match: stillEquals(intNode(1)),
			guard: func(raw *yaml.Node) bool { return mappingIndex(raw, "ttl_minutes") < 0 },
			added: "model_catalog.ttl_hours 1 → ttl_minutes 20 (default)", message: "  ✓ Model catalog now refreshes every 20 minutes (model_catalog.ttl_minutes)"}),
		{41, migrateTo41},
		rewriteStep(42, keyRewrite{section: "cron", key: "model_drift_guard", match: func(cur *yaml.Node) bool { return cur != nil && !isNull(cur) },
			added: "removed cron.model_drift_guard",
			message: "  ✓ Removed cron.model_drift_guard — unpinned cron jobs now keep running on the model/provider they were created under " +
				"when the global default changes, instead of being skipped. Pin a job or set cron.model to move it."}),
		rewriteStep(43, keyRewrite{section: "gateway", key: "multiplex_profile_allowlist", match: func(*yaml.Node) bool { return true },
			guard: hasKey("multiplex_profile_allowlist"), added: "removed gateway.multiplex_profile_allowlist",
			message: "  ✓ Removed gateway.multiplex_profile_allowlist — the multiplexing gateway now serves every profile under profiles/. " +
				"Delete or archive a profile you do not want served."}),
		rewriteStep(44, keyRewrite{section: "curator", key: "stale_after_days", match: stillEquals(intNode(30)), value: intNode(14),
			added:   "curator.stale_after_days=14 (was: 30)",
			message: "  ✓ curator.stale_after_days 30→14 — unused skills are flagged stale after two weeks."}),
		rewriteStep(44, keyRewrite{section: "curator", key: "archive_after_days", match: stillEquals(intNode(90)), value: intNode(30),
			added: "curator.archive_after_days=30 (was: 90)",
			message: "  ✓ curator.archive_after_days 90→30 — skills unused for a month are archived to skills/.archive/ " +
				"(recoverable with `icebath curator restore`). Set it back to 90 to keep the old window."}),
		{45, migrateTo45}, {46, migrateTo46},
		rewriteStep(47, keyRewrite{section: "compression", key: "threshold_tokens", match: stillEquals(intNode(256000)),
			added: "removed compression.threshold_tokens: 256000 (the old default)",
			message: "  ✓ Removed compression.threshold_tokens: 256000 — the old default. Compaction follows compression.threshold " +
				"(50% of the window) again. Set threshold_tokens to a token count to cap it on purpose."}),
		{48, migrateTo48}, {49, migrateTo49}, {50, migrateTo50},
		rewriteStep(51, keyRewrite{section: "cron", key: "bot_chat_delivery_timeout_seconds", match: func(*yaml.Node) bool { return true },
			guard: hasKey("bot_chat_delivery_timeout_seconds"), added: "removed cron.bot_chat_delivery_timeout_seconds",
			message: "  ✓ Removed cron.bot_chat_delivery_timeout_seconds — bot-chat deliveries are now admitted to the target profile's running gateway " +
				"and tracked by receipt, so cron no longer runs (or times out) a Bot Chat turn of its own."}),
		{52, migrateTo52},
	}
}

// Versions of the steps a legacy key or identifier triggers, the only steps a never stamped file gets
func legacyKeySteps() []int {
	return []int{12, 14, 16, 17, 29, 33, 38, 39, 42, 43, 46, 50, 51, 52}
}

// Versions of the legacy key steps that run on every pass whatever the stamp says
func stampIndependentSteps() []int { return []int{46, 50} }

// Applies every due upgrade step, a failing step warned about and skipped so the others proceed
func (m *migration) run(current int, unversioned bool) {
	for _, step := range migrationSteps() {
		due := current < step.version || slices.Contains(stampIndependentSteps(), step.version)
		if !due || (unversioned && !slices.Contains(legacyKeySteps(), step.version)) {
			continue
		}
		if err := step.run(m); err != nil {
			warning := fmt.Sprintf("config migration to v%d failed and was skipped: %v", step.version, err)
			m.report.Warnings = append(m.report.Warnings, warning)
			slog.Warn(warning)
			m.say("  ⚠ " + warning)
		}
	}
}

// Explanation shown for a config stamped below the support floor
func (s *Store) supportFloorMessage(h string) string {
	shown, err := home.Display(h)
	if err != nil {
		shown = h
	}
	return fmt.Sprintf("This config predates version %d (~2 years old) and can no longer be auto-migrated. Back up "+
		"%s/config.yaml and run `icebath setup` to regenerate, or manually set _config_version: %d after reviewing the changelog.",
		SupportFloorVersion, shown, SupportFloorVersion)
}

// Asks one migration question, a secret returned as typed and any other answer trimmed, a missing answerer named
func (m *migration) ask(o MigrateOptions, kind PromptKind, text string) (string, error) {
	if o.Ask == nil {
		return "", &SeamError{Seam: "migration prompt"}
	}
	answer, err := o.Ask(m.ctx, Prompt{Kind: kind, Text: text})
	if kind == PromptSecret {
		return answer, err
	}
	return trimEnvSpace(answer), err
}

// Asks a yes or no question, only y and yes meaning yes and an answerer that fails meaning no
func (m *migration) askYesNo(o MigrateOptions, text string) (bool, error) {
	if o.Ask == nil {
		return false, &SeamError{Seam: "migration prompt"}
	}
	answer, err := o.Ask(m.ctx, Prompt{Kind: PromptYesNo, Text: text})
	if err != nil {
		return false, nil
	}
	answer = strings.ToLower(trimEnvSpace(answer))
	return answer == "y" || answer == "yes", nil
}

// Boolean a scalar holds, ok false for any other value
func boolScalar(n *yaml.Node) (value, ok bool) {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return false, false
	}
	tag, text := scalarKind(n)
	if tag != tagBool {
		return false, false
	}
	word, _ := resolveTagged(tagBool, text)
	return word == "true", true
}

// Prompts for one env variable, saves a given value and records it, false when the person skipped it
func (m *migration) promptAndSaveEnv(o MigrateOptions, name string, v EnvVar, prompt string) (bool, error) {
	kind := PromptLine
	if v.Password != nil && *v.Password {
		kind = PromptSecret
	}
	value, err := m.ask(o, kind, prompt)
	if err != nil || value == "" {
		return false, err
	}
	if err := m.s.Env().Set(m.ctx, name, value); err != nil {
		return false, err
	}
	m.report.EnvAdded = append(m.report.EnvAdded, name)
	m.line(plainLine("  ✓ Saved " + name))
	return true, nil
}

// Prints a bulleted offer and asks, false with the set later hint when declined
func (m *migration) offerList(o MigrateOptions, heading string, items []string, question string) (bool, error) {
	m.line(plainLine(heading))
	for _, item := range items {
		m.line(plainLine("    • " + item))
	}
	m.line(blankLine())
	yes, err := m.askYesNo(o, question)
	if err != nil {
		return false, err
	}
	if !yes {
		m.line(plainLine("  Set later with: icebath config set <key> <value>"))
		return false, nil
	}
	m.line(blankLine())
	return true, nil
}

// Optional variables each schema version introduced, offered once to a config from before that version
func envVarsByVersion() map[int][]string {
	return map[int][]string{
		3:  {"FIRECRAWL_API_KEY", "BROWSERBASE_API_KEY", "BROWSERBASE_PROJECT_ID", "FAL_KEY"},
		4:  {"VOICE_TOOLS_OPENAI_KEY", "ELEVENLABS_API_KEY"},
		5:  {"WHATSAPP_ENABLED", "WHATSAPP_MODE", "WHATSAPP_ALLOWED_USERS", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "SLACK_ALLOWED_USERS"},
		10: {"TAVILY_API_KEY"},
		11: {"TERMINAL_MODAL_MODE"},
	}
}

// Offers the optional variables that are new since the previous schema version of the config and still unset
func (m *migration) offerNewOptionalEnv(o MigrateOptions, current, latest int) error {
	var names []string
	for ver := current + 1; ver <= latest; ver++ {
		names = append(names, envVarsByVersion()[ver]...)
	}
	registry := m.s.envs.registry
	type offer struct {
		name string
		v    EnvVar
	}
	var offers []offer
	for _, name := range sortedSet(names...) {
		value, err := m.s.envValue(m.ctx, name)
		if err != nil {
			return err
		}
		if v, ok := registry.Get(name); ok && value == "" {
			offers = append(offers, offer{name, v})
		}
	}
	if len(offers) == 0 {
		return nil
	}
	items := make([]string, len(offers))
	for i, of := range offers {
		items[i] = of.name + " — " + of.v.Description
	}
	yes, err := m.offerList(o, fmt.Sprintf("\n  %d new optional key(s) in this update:", len(offers)), items, "  Configure new keys? [y/N]: ")
	if err != nil || !yes {
		return err
	}
	for _, of := range offers {
		shown, prompt := of.v.Description, of.v.Prompt
		if shown == "" {
			shown = of.name
		}
		if prompt == "" {
			prompt = of.name
		}
		m.line(plainLine("  " + shown))
		if of.v.URL != nil && *of.v.URL != "" {
			m.line(plainLine("  Get your key at: " + *of.v.URL))
		}
		if _, err := m.promptAndSaveEnv(o, of.name, of.v, "  "+prompt+" (Enter to skip): "); err != nil {
			return err
		}
		m.line(blankLine())
	}
	return nil
}

// Offers the settings skills declare that the config leaves unset and saves the answers
func (m *migration) offerSkillConfig(o MigrateOptions, missing []SkillConfigVar) error {
	items := make([]string, len(missing))
	for i, v := range missing {
		skill := v.Skill
		if skill == "" {
			skill = "unknown"
		}
		items[i] = fmt.Sprintf("%s — %s (from skill: %s)", v.Key, v.Description, skill)
	}
	yes, err := m.offerList(o, fmt.Sprintf("\n  %d skill setting(s) not configured:", len(missing)), items, "  Configure skill settings? [y/N]: ")
	if err != nil || !yes {
		return err
	}
	config, err := m.raw()
	if err != nil {
		return err
	}
	for _, v := range missing {
		hint := ""
		if v.Default != "" {
			hint = " (default: " + v.Default + ")"
		}
		value, err := m.ask(o, PromptLine, "  "+v.Prompt+hint+": ")
		if err != nil {
			return err
		}
		if value == "" {
			value = v.Default
		}
		if value == "" {
			skill := v.Skill
			if skill == "" {
				skill = "?"
			}
			m.report.Warnings = append(m.report.Warnings, fmt.Sprintf("Skipped %s — skill '%s' may ask for it later", v.Key, skill))
			m.line(blankLine())
			continue
		}
		if err := setNested(config, "skills.config."+v.Key, stringNode(value)); err != nil {
			return err
		}
		m.report.ConfigAdded = append(m.report.ConfigAdded, v.Key)
		m.line(plainLine(fmt.Sprintf("  ✓ Saved %s = %s", v.Key, value)))
		m.line(blankLine())
	}
	return m.persist(config)
}

// Disables MCP server entries shaped like exfiltration, keeping each stanza for review
func (m *migration) disableSuspiciousMCP() error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	servers := childOf(config, "mcp_servers")
	if !isMapping(servers) {
		return nil
	}
	if m.s.know.MCPServerIssues == nil {
		return &SeamError{Seam: "MCP server security check"}
	}
	touched := false
	for i := 0; i+1 < len(servers.Content); i += 2 {
		name, entry := displayText(servers.Content[i]), servers.Content[i+1]
		if !isMapping(entry) {
			continue
		}
		issues := m.s.know.MCPServerIssues(name, plainCopy(entry))
		if len(issues) == 0 {
			continue
		}
		putKey(entry, "enabled", boolNode(false))
		touched = true
		m.report.Warnings = append(m.report.Warnings, fmt.Sprintf("Disabled suspicious MCP server '%s'", name))
		for _, issue := range issues {
			m.say("  ⚠ " + issue)
		}
		m.say(fmt.Sprintf("  ⚠ Disabled MCP server '%s' pending review", name))
	}
	if !touched {
		return nil
	}
	return m.persist(config)
}

// Surfaces toolset names in platform_toolsets that resolve to nothing, a check that cannot run never stopping the pass
func (m *migration) warnInvalidToolsets() error {
	if m.s.know.ToolsetDiagnostics == nil {
		return &SeamError{Seam: "toolset diagnostics"}
	}
	config, err := m.raw()
	var warnings []string
	if err == nil {
		warnings, err = m.s.know.ToolsetDiagnostics(m.ctx, plainCopy(config))
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("platform_toolsets validation skipped: %v", err))
		return nil
	}
	for _, w := range warnings {
		m.report.Warnings = append(m.report.Warnings, w)
		m.say("  ⚠ " + w)
	}
	return nil
}

// Migrates the config of ctx to the current schema version, asking for new settings when interactive
func (s *Store) Migrate(ctx context.Context, o MigrateOptions) (*MigrationReport, error) {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	m := &migration{ctx: ctx, s: s, home: h, report: &MigrationReport{}, quiet: o.Quiet, emit: o.Emit}
	if _, err := s.StampMissingConfig(ctx); err != nil {
		return nil, err
	}
	stamp, stamped, latest, err := s.ReadConfigVersionStamp(ctx, true)
	if err != nil {
		return nil, err
	}
	current := 0
	if stamped {
		current = stamp
	}
	if fixes, err := s.Env().Sanitize(ctx); err != nil {
		slog.Debug(fmt.Sprintf("Could not normalize the env file before migrating: %v", err))
	} else if fixes > 0 {
		m.say(fmt.Sprintf("  ✓ Normalized .env line formatting (%d line(s) changed)", fixes))
	}
	floorRefused := stamped && current < SupportFloorVersion && current < latest
	if floorRefused {
		msg := s.supportFloorMessage(h)
		m.report.Warnings = append(m.report.Warnings, msg)
		m.line(stderrLine(TonePlain, "⚠ icebath config: "+msg))
		m.say("  ⚠ " + msg)
	} else {
		m.run(current, !stamped)
	}
	if err := m.disableSuspiciousMCP(); err != nil {
		return m.report, err
	}
	if err := m.warnInvalidToolsets(); err != nil {
		return m.report, err
	}
	if current < latest && !floorRefused {
		m.say(fmt.Sprintf("Config version: %d → %d", current, latest))
	}
	missingEnv, err := s.Env().Missing(ctx, true)
	if err != nil {
		return m.report, err
	}
	if len(missingEnv) > 0 {
		m.say("\n⚠️  Missing required environment variables:")
		for _, v := range missingEnv {
			m.say(fmt.Sprintf("   • %s: %s", v.Name, v.Var.Description))
		}
	}
	if o.Interactive && len(missingEnv) > 0 {
		m.line(plainLine("\nLet's configure them now:\n"))
		for _, v := range missingEnv {
			if v.Var.URL != nil && *v.Var.URL != "" {
				m.line(plainLine("  Get your key at: " + *v.Var.URL))
			}
			saved, err := m.promptAndSaveEnv(o, v.Name, v.Var, "  "+v.Var.Prompt+": ")
			if err != nil {
				return m.report, err
			}
			if !saved {
				m.report.Warnings = append(m.report.Warnings, fmt.Sprintf("Skipped %s - some features may not work", v.Name))
			}
			m.line(blankLine())
		}
	}
	if o.Interactive && !o.Quiet {
		if err := m.offerNewOptionalEnv(o, current, latest); err != nil {
			return m.report, err
		}
	}
	missingFields, err := s.MissingFields(ctx)
	if err != nil {
		return m.report, err
	}
	for _, f := range missingFields {
		m.report.ConfigAdded = append(m.report.ConfigAdded, f.Key)
	}
	if current < latest && !floorRefused {
		config, err := m.raw()
		if err != nil {
			return m.report, err
		}
		putKey(config, "_config_version", intNode(latest))
		if err := m.persist(config); err != nil {
			return m.report, err
		}
	}
	missingSkill, err := s.MissingSkillConfigVars(ctx)
	if err != nil {
		return m.report, err
	}
	if len(missingSkill) > 0 && o.Interactive && !o.Quiet {
		if err := m.offerSkillConfig(o, missingSkill); err != nil {
			return m.report, err
		}
	}
	return m.report, nil
}

// Transforms the config a backend without a file serves through every due step, touching no local file
func (s *Store) MigrateInMemory(ctx context.Context, current int, unversioned bool) (*MigrationReport, error) {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	m := &migration{ctx: ctx, s: s, home: h, report: &MigrationReport{}, quiet: true, configOnly: true}
	m.run(current, unversioned)
	return m.report, nil
}

// Lines of the pre migration report and whether anything is left to migrate
func (s *Store) MigrationPreview(ctx context.Context) ([]Line, bool, error) {
	out := []Line{blankLine(), tonedLine(ToneTitle, "🔄 Checking configuration for updates..."), blankLine()}
	missingEnv, err := s.Env().Missing(ctx, false)
	if err != nil {
		return nil, false, err
	}
	missingConfig, err := s.MissingFields(ctx)
	if err != nil {
		return nil, false, err
	}
	current, latest, err := s.CheckConfigVersion(ctx, true)
	if err != nil {
		return nil, false, err
	}
	if len(missingEnv) == 0 && len(missingConfig) == 0 && current >= latest {
		return append(out, tonedLine(ToneSuccess, "✓ Configuration is up to date!"), blankLine()), false, nil
	}
	if current < latest {
		out = append(out, plainLine(fmt.Sprintf("  Config version: %d → %d", current, latest)))
	}
	if len(missingConfig) > 0 {
		out = append(out, plainLine(fmt.Sprintf("\n  %d new config option(s) will be added with defaults", len(missingConfig))))
	}
	var optional []MissingEnvVar
	for _, v := range missingEnv {
		if !v.Var.Advanced {
			optional = append(optional, v)
		}
	}
	if len(optional) > 0 {
		out = append(out, plainLine(fmt.Sprintf("\n  ℹ️  %d optional API key(s) not configured:", len(optional))))
		for _, v := range optional {
			out = append(out, plainLine("     • "+v.Name+toolsSuffix(v.Var, " (enables: %s)")))
		}
	}
	return append(out, blankLine()), true, nil
}

// Closing lines of the migrate command for what a pass reported
func MigrationSummaryLines(r *MigrationReport) []Line {
	out := []Line{blankLine()}
	if len(r.EnvAdded) > 0 || len(r.ConfigAdded) > 0 {
		out = append(out, tonedLine(ToneSuccess, "✓ Configuration updated!"))
	}
	if len(r.Warnings) > 0 {
		out = append(out, blankLine())
		for _, w := range r.Warnings {
			out = append(out, tonedLine(ToneWarning, "  ⚠️  "+w))
		}
	}
	return append(out, blankLine())
}

// Lines warning that the env file still sets a working directory variable the config owns, none when it does not
func (s *Store) DeprecatedCwdLines(ctx context.Context) ([]Line, error) {
	file, err := s.Env().File(ctx)
	if err != nil {
		return nil, err
	}
	var out []Line
	for _, name := range []string{"MESSAGING_CWD", "TERMINAL_CWD"} {
		if val := trimEnvSpace(file[name]); val != "" {
			out = append(out, Line{Spans: []Span{{Text: "  "}, {Text: "⚠", Tone: ToneWarning},
				{Text: fmt.Sprintf(" %s=%s found in .env — this is deprecated.", name, val)}}, Stderr: true})
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	h, err := s.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	shown, err := home.Display(h)
	if err != nil {
		shown = h
	}
	out = append([]Line{stderrLine(ToneWarning, "⚠ Deprecated .env settings detected:")}, out...)
	return append(out,
		Line{Spans: []Span{{Text: "  "}, {Text: "Move to config.yaml instead:  terminal:\\n    cwd: /your/project/path", Tone: ToneDim}}, Stderr: true},
		Line{Spans: []Span{{Text: "  "}, {Text: "Then remove the old entries from " + shown + "/.env", Tone: ToneDim}}, Stderr: true},
		Line{Spans: []Span{{}}, Stderr: true}), nil
}
