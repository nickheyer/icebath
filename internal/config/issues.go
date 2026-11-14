package config

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

// How serious a detected config structure problem is
type Severity string

// Severities of config structure problems
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// One detected config structure problem with the fix to suggest
type Issue struct {
	Severity Severity
	Message  string
	Hint     string
}

// Lines of the startup report of config structure problems on stderr, none for a healthy config
func IssueLines(issues []Issue) []Line {
	if len(issues) == 0 {
		return nil
	}
	out := []Line{stderrLine(ToneWarning, "⚠ Config issues detected in config.yaml:")}
	for _, issue := range issues {
		marker := Span{Text: "⚠", Tone: ToneWarning}
		if issue.Severity == SeverityError {
			marker = Span{Text: "✗", Tone: ToneError}
		}
		out = append(out, Line{Spans: []Span{{Text: "  "}, marker, {Text: " " + issue.Message}}, Stderr: true})
	}
	return append(out, Line{Spans: []Span{{Text: "  "}, {Text: "Run 'icebath doctor' for fix suggestions.", Tone: ToneDim}}, Stderr: true},
		Line{Spans: []Span{{}}, Stderr: true})
}

// Matched by every operation that needs knowledge of another package no caller wired into the store
var ErrSeamUnfilled = errors.New("config seam is not filled")

// Operation that needs knowledge of another package no caller wired into the store
type SeamError struct {
	Seam string
}

// Names the seam the program left empty
func (e *SeamError) Error() string {
	return fmt.Sprintf("config: the %s seam is not wired into the store", e.Seam)
}

// Matches ErrSeamUnfilled
func (e *SeamError) Unwrap() error { return ErrSeamUnfilled }

// Locale ids the message catalogs of the program supply, asked when display.language is set
type Languages interface {
	Resolve(ctx context.Context, value string) (id string, ok bool)
	Supported(ctx context.Context) ([]string, error)
}

// One setting a skill declares under skills.config with the skill that declares it
type SkillConfigVar struct {
	Key         string
	Description string
	Prompt      string
	Default     string
	Skill       string
}

// Facts from the packages above config that validation, the config commands and the migrations consult
type Knowledge struct {
	Languages           Languages
	RemovedBackend      func(section, name string) string
	SelectionKeys       func() []string
	PluginConfigKeys    func(ctx context.Context) ([]string, error)
	RouteOwner          func(ctx context.Context, provider, baseURL string, root *yaml.Node) (owned, known bool)
	ToolsetDiagnostics  func(ctx context.Context, raw *yaml.Node) ([]string, error)
	SkillConfigVars     func(ctx context.Context) ([]SkillConfigVar, error)
	SkillConfigValues   func(ctx context.Context, vars []SkillConfigVar) (map[string]string, error)
	ActivePersonality   func(cfg *Config) string
	AnthropicKey        func(ctx context.Context) (string, error)
	RedactionEnabled    func(ctx context.Context) bool
	MCPServerIssues     func(name string, entry *yaml.Node) []string
	Personalities       func(config *yaml.Node) PersonalityFacts
	ExplicitToolsetKeys func(ctx context.Context) ([]string, error)
	ToolsetAllowed      func(toolset, platform string) bool
	InToolchainStore    func(path string) bool
}

// Wires the knowledge of the packages above config that validation and the config commands consult
func WithKnowledge(k Knowledge) Option {
	return func(s *Store) { s.know = k }
}

// Type word the validation messages print for the kind of a value
func typeName(n *yaml.Node) string {
	n = resolveAlias(n)
	switch {
	case n == nil || isNull(n):
		return "NoneType"
	case n.Kind == yaml.SequenceNode:
		return "list"
	case n.Kind == yaml.MappingNode:
		return "dict"
	}
	tag, text := scalarKind(n)
	switch tag {
	case tagBool:
		return "bool"
	case tagInt:
		return "int"
	case tagFloat:
		return "float"
	case "!!binary":
		return "bytes"
	case "!!timestamp":
		if m := timestampText.FindStringSubmatch(text); m != nil && m[4] == "" {
			return "date"
		}
		return "datetime"
	}
	return "str"
}

// Root keys valid on disk that the defaults leave out, optional sections and the top level forms of gateway keys
func extraKnownRootKeys() []string {
	return []string{
		"custom_providers", "fallback_model", "mcp_servers", "image_gen", "video_gen", "plugins",
		"smart_model_routing", "platform_toolsets", "known_plugin_toolsets", "known_builtin_toolsets",
		"tool_gateway_declined_tools", "group_sessions_per_user", "thread_sessions_per_user",
		"stt_echo_transcripts", "reset_triggers", "always_log_local", "filter_silence_narration",
		"multiplex_profiles", "profile_routes", "platforms", "require_mention",
		"unauthorized_dm_behavior", "signal", "allow_all_users", "timeouts",
	}
}

// Fields that belong inside a custom_providers entry and look misplaced at the root
func customProviderLikeFields() []string {
	return []string{"base_url", "api_key", "rate_limit_delay", "api_mode"}
}

// One required field of a list entry with the hint shown when it is missing
type requiredField struct {
	name string
	hint string
}

// Appends a warning for every field of entry that is missing or holds a value read as unset
func requireFields(issues *[]Issue, entry *yaml.Node, label string, fields []requiredField, suffix string) {
	for _, f := range fields {
		if !isSet(childOf(entry, f.name)) {
			*issues = append(*issues, Issue{SeverityWarning, fmt.Sprintf("%s is missing '%s' field%s", label, f.name, suffix), f.hint})
		}
	}
}

// Change advised for a custom_providers value that is not a list
const customProvidersListHint = "Change to:\n  custom_providers:\n    - name: my-provider\n      base_url: https://...\n      api_key: ..."

// Problems of a custom_providers value, which must be a list of mappings
func validateCustomProviders(cp *yaml.Node, issues *[]Issue) {
	switch {
	case isMapping(cp):
		*issues = append(*issues, Issue{SeverityError,
			"custom_providers is a dict — it must be a YAML list (items prefixed with '-')", customProvidersListHint})
		var suspicious []string
		for i := 0; i+1 < len(cp.Content); i += 2 {
			if k := cp.Content[i].Value; slices.Contains(customProviderLikeFields(), k) && !slices.Contains(suspicious, k) {
				suspicious = append(suspicious, k)
			}
		}
		if len(suspicious) > 0 {
			slices.Sort(suspicious)
			quotedNames := make([]string, len(suspicious))
			for i, name := range suspicious {
				quotedNames[i] = quoted(name)
			}
			*issues = append(*issues, Issue{SeverityWarning,
				fmt.Sprintf("Root-level keys [%s] look like custom_providers entry fields", strings.Join(quotedNames, ", ")),
				"These should be indented under a '- name: ...' list entry, not at root level"})
		}
	case isSequence(cp):
		fields := []requiredField{
			{"name", "Add a name, e.g.: name: my-provider"},
			{"base_url", "Add the API endpoint URL, e.g.: base_url: https://api.example.com/v1"},
		}
		for i, entry := range cp.Content {
			if !isMapping(entry) {
				*issues = append(*issues, Issue{SeverityWarning,
					fmt.Sprintf("custom_providers[%d] is not a dict (got %s)", i, typeName(entry)),
					"Each entry should have at minimum: name, base_url"})
				continue
			}
			requireFields(issues, entry, fmt.Sprintf("custom_providers[%d]", i), fields, "")
		}
	default:
		*issues = append(*issues, Issue{SeverityError,
			fmt.Sprintf("custom_providers is a %s — it must be a YAML list (items prefixed with '-'); "+
				"legacy custom_providers entries are ignored until it is", typeName(cp)), customProvidersListHint})
	}
}

// Problems of a fallback_model value, one route mapping or a list of them
func validateFallbackModel(fb *yaml.Node, issues *[]Issue) {
	switch {
	case isSequence(fb):
		fields := []requiredField{
			{"provider", "Add: provider: openrouter (or another provider)"},
			{"model", "Add: model: <model-name>"},
		}
		for i, entry := range fb.Content {
			if !isMapping(entry) {
				*issues = append(*issues, Issue{SeverityError,
					fmt.Sprintf("fallback_model[%d] should be a dict, got %s", i, typeName(entry)), "Each entry needs provider + model"})
				continue
			}
			requireFields(issues, entry, fmt.Sprintf("fallback_model[%d]", i), fields, "")
		}
	case !isMapping(fb):
		*issues = append(*issues, Issue{SeverityError,
			fmt.Sprintf("fallback_model should be a dict with 'provider' and 'model', got %s", typeName(fb)),
			"Change to:\n  fallback_model:\n    provider: openrouter\n    model: anthropic/claude-sonnet-4"})
	case len(fb.Content) > 0:
		requireFields(issues, fb, "fallback_model", []requiredField{
			{"provider", "Add: provider: openrouter (or another provider)"},
			{"model", "Add: model: anthropic/claude-sonnet-4 (or another model)"},
		}, " — fallback will be disabled")
	}
}

// Problem of a voice.submit_mode that is neither direct nor draft
func validateVoice(config *yaml.Node, issues *[]Issue) {
	voice := childOf(config, "voice")
	at := mappingIndex(voice, "submit_mode")
	if at < 0 {
		return
	}
	mode := voice.Content[at]
	if isStringNode(mode) {
		switch strings.ToLower(trimEnvSpace(mode.Value)) {
		case "direct", "draft":
			return
		}
	}
	*issues = append(*issues, Issue{SeverityError,
		fmt.Sprintf("voice.submit_mode must be 'direct' or 'draft', got %s", literalText(mode)),
		"Set voice.submit_mode to direct (submit immediately) or draft (edit before sending)"})
}

// Reports whether this process can load the time zone database at all
func zoneDatabaseAvailable() bool {
	_, err := time.LoadLocation("Etc/UTC")
	return err == nil
}

// Reports a name the time zone database holds, the Local word of the runtime not being a zone
func validZoneName(name string) bool {
	if name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// Problem of a timezone value that is no IANA zone name the runtime can load
func validateTimezone(config *yaml.Node, issues *[]Issue) {
	at := mappingIndex(config, "timezone")
	if at < 0 {
		return
	}
	tz := config.Content[at]
	hint := "Use an IANA zone name such as America/New_York or Asia/Tokyo (see " +
		"`timedatectl list-timezones`). With an invalid value the agent clock and cron " +
		"schedules silently fall back to server-local time. ICEBATH_TIMEZONE overrides " +
		"this key when set."
	if !isNull(tz) && !isStringNode(tz) {
		*issues = append(*issues, Issue{SeverityError, fmt.Sprintf("timezone must be an IANA zone name string, got %s", literalText(tz)), hint})
		return
	}
	if isNull(tz) || trimEnvSpace(tz.Value) == "" {
		return
	}
	name := trimEnvSpace(tz.Value)
	if !zoneDatabaseAvailable() || validZoneName(name) {
		return
	}
	*issues = append(*issues, Issue{SeverityError, fmt.Sprintf("timezone %s is not a valid IANA zone name", quoted(name)), hint})
}

// Warnings for web backend selections naming a backend the program no longer ships
func (s *Store) validateWebBackends(config *yaml.Node, issues *[]Issue) error {
	web := childOf(config, "web")
	if !isMapping(web) {
		return nil
	}
	var seen []string
	for _, key := range []string{"backend", "search_backend", "extract_backend"} {
		value := strings.ToLower(trimEnvSpace(orText(childOf(web, key))))
		if value == "" || slices.Contains(seen, value) {
			continue
		}
		seen = append(seen, value)
		if s.know.RemovedBackend == nil {
			return &SeamError{Seam: "removed backend note"}
		}
		if note := s.know.RemovedBackend("web", value); note != "" {
			*issues = append(*issues, Issue{SeverityWarning,
				fmt.Sprintf("web.%s is set to '%s', but %s — web_search/web_extract will fail until it is changed", key, value, note),
				"Run 'icebath tools' and pick a different Web Search & Extract provider"})
		}
	}
	return nil
}

// Characters a shell word may hold without quoting
var shellSafeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// Text as one POSIX shell word, in single quotes unless every character is safe bare
func shellQuote(s string) string {
	if shellSafeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Kind of container the schema fixes a dotted key to
type containerKind string

// Container kinds a schema slot may be fixed to
const (
	containerList    containerKind = "list"
	containerMapping containerKind = "mapping"
)

// Containers whose shape their readers fix although the defaults leave them out
func knownContainerTypes() map[string]containerKind {
	return map[string]containerKind{
		"custom_providers":                 containerList,
		"providers":                        containerMapping,
		"model.aliases":                    containerMapping,
		"model_aliases":                    containerMapping,
		"plugins.enabled":                  containerList,
		"plugins.disabled":                 containerList,
		"model_catalog.excluded_providers": containerList,
	}
}

// List slots whose readers take one bare name as a one item list
func scalarAsOneItemListKeys() []string {
	return []string{"agent.disabled_toolsets", "skills.disabled"}
}

// One schema slot fixed to a container, in schema order
type containerSlot struct {
	key  string
	kind containerKind
}

// Every dotted key the defaults or the known container table fix to a list or a mapping
func containerSlots(defaults *yaml.Node) []containerSlot {
	var out []containerSlot
	at := map[string]int{}
	put := func(key string, kind containerKind) {
		if i, ok := at[key]; ok {
			out[i].kind = kind
			return
		}
		at[key] = len(out)
		out = append(out, containerSlot{key, kind})
	}
	var walk func(n *yaml.Node, prefix string)
	walk = func(n *yaml.Node, prefix string) {
		for i := 0; i+1 < len(n.Content); i += 2 {
			path := n.Content[i].Value
			if prefix != "" {
				path = prefix + "." + path
			}
			switch v := n.Content[i+1]; {
			case isMapping(v):
				put(path, containerMapping)
				walk(v, path)
			case isSequence(v):
				put(path, containerList)
			}
		}
	}
	if isMapping(defaults) {
		walk(defaults, "")
	}
	known := knownContainerTypes()
	names := make([]string, 0, len(known))
	for name := range known {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		put(name, known[name])
	}
	return out
}

// Warnings for container slots holding one quoted string that every reader ignores
func validateQuotedContainers(config, defaults *yaml.Node, issues *[]Issue) {
	for _, slot := range containerSlots(defaults) {
		if slices.Contains(scalarAsOneItemListKeys(), slot.key) {
			continue
		}
		value := treeGet(config, strings.Split(slot.key, ".")...)
		if !isStringNode(value) || !looksStructured(value.Value) {
			continue
		}
		var parsed yaml.Node
		if err := LoadYAML([]byte(value.Value), &parsed); err != nil {
			continue
		}
		if !isMapping(&parsed) && !isSequence(&parsed) {
			continue
		}
		*issues = append(*issues, Issue{SeverityWarning,
			fmt.Sprintf("%s is the quoted string %s — icebath expects a YAML %s here and every reader ignores the string",
				slot.key, quoted(value.Value), slot.kind),
			fmt.Sprintf("Run: icebath config set %s %s  (stores a real %s), or remove the quotes in config.yaml",
				slot.key, shellQuote(value.Value), slot.kind)})
	}
}

// Problems in the structure of a config value tree, the common YAML mistakes that surface as confusing runtime errors
func (s *Store) ValidateTree(config *yaml.Node) ([]Issue, error) {
	defaults, err := s.defaults()
	if err != nil {
		return nil, err
	}
	if !isMapping(config) {
		config = emptyMapping()
	}
	issues := []Issue{}
	validateVoice(config, &issues)
	validateTimezone(config, &issues)
	cp, fb := childOf(config, "custom_providers"), childOf(config, "fallback_model")
	if cp != nil && !isNull(cp) {
		validateCustomProviders(cp, &issues)
	}
	if fb != nil && !isNull(fb) {
		validateFallbackModel(fb, &issues)
	}
	if isMapping(cp) && mappingIndex(config, "fallback_model") < 0 && mappingIndex(cp, "fallback_model") >= 0 {
		issues = append(issues, Issue{SeverityError, "fallback_model appears inside custom_providers instead of at root level",
			"Move fallback_model to the top level of config.yaml (no indentation)"})
	}
	if isSet(cp) && !isSet(childOf(config, "model")) {
		issues = append(issues, Issue{SeverityWarning,
			"custom_providers defined but no 'model' section — icebath won't know which provider to use",
			"Add a model section:\n  model:\n    provider: custom\n    default: your-model-name\n    base_url: https://..."})
	}
	known := append(mappingKeys(defaults), extraKnownRootKeys()...)
	for _, key := range mappingKeys(config) {
		if !strings.HasPrefix(key, "_") && !slices.Contains(known, key) && slices.Contains(customProviderLikeFields(), key) {
			issues = append(issues, Issue{SeverityWarning,
				fmt.Sprintf("Root-level key '%s' looks misplaced — should it be under 'model:' or inside a 'custom_providers' entry?", key),
				fmt.Sprintf("Move '%s' under the appropriate section", key)})
		}
	}
	if err := s.validateWebBackends(config, &issues); err != nil {
		return nil, err
	}
	validateQuotedContainers(config, defaults, &issues)
	return issues, nil
}

// Keys of a mapping value tree in order
func mappingKeys(m *yaml.Node) []string {
	if !isMapping(m) {
		return nil
	}
	out := make([]string, 0, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		out = append(out, m.Content[i].Value)
	}
	return out
}

// Issue standing in for a config that could not be loaded at all
func loadIssue(err error) Issue {
	var init *home.InitError
	if errors.As(err, &init) || isReadError(err) {
		return Issue{SeverityError, fmt.Sprintf("icebath storage is unavailable: %v", err),
			"Check the reported path, link target, mount and permissions; keep config.yaml unchanged."}
	}
	return Issue{SeverityError, "Could not load config.yaml", "Run 'icebath setup' to create a valid config"}
}

// Problems in the structure of the merged config of ctx, a config that cannot load reported as one issue
func (s *Store) Validate(ctx context.Context) ([]Issue, error) {
	tree, err := s.mergedTree(ctx)
	if err != nil {
		if errors.Is(err, ErrSeamUnfilled) || errors.Is(err, ErrBackendUnavailable) {
			return nil, err
		}
		return []Issue{loadIssue(err)}, nil
	}
	return s.ValidateTree(tree)
}

// Value tree of the merged config of ctx, defaults under the user file under the managed overlay
func (s *Store) mergedTree(ctx context.Context) (*yaml.Node, error) {
	cfg, err := s.LoadReadonly(ctx)
	if err != nil {
		return nil, err
	}
	return treeOf(cfg)
}
