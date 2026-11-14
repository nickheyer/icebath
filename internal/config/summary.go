package config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

// Blank output line
func blankLine() Line { return plainLine("") }

// Section heading of the config summary, a blank line then the title
func sectionLines(title string) []Line {
	return []Line{blankLine(), tonedLine(ToneTitle, "◆ "+title)}
}

// Value of a name through the secret scope of ctx, else the home env file, empty when absent
func (s *Store) envValue(ctx context.Context, name string) (string, error) {
	v, _, err := s.Env().Get(ctx, name)
	return v, err
}

// Span showing an API key masked, the dim not set text when it is empty
func keySpan(key string) Span {
	if masked := MaskSecret(key); masked != "" {
		return Span{Text: masked}
	}
	return Span{Text: KeyNotSet, Tone: ToneDim}
}

// Text of a value under a key of a section, def when the section lacks the key
func textOr(section *yaml.Node, key, def string) string {
	if at := mappingIndex(section, key); at >= 0 {
		return displayText(section.Content[at])
	}
	return def
}

// Whole number with a comma between every three digits
func groupedDigits(v int) string {
	text := strconv.Itoa(v)
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	var b strings.Builder
	for i, r := range text {
		if i > 0 && (len(text)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String()
}

// Keys of the summary shown under API Keys with their display names
func summaryAPIKeys() [][2]string {
	return [][2]string{
		{"OPENROUTER_API_KEY", "OpenRouter"}, {"VOICE_TOOLS_OPENAI_KEY", "OpenAI (STT/TTS)"}, {"EXA_API_KEY", "Exa"},
		{"PARALLEL_API_KEY", "Parallel"}, {"FIRECRAWL_API_KEY", "Firecrawl"}, {"TAVILY_API_KEY", "Tavily"},
		{"PERPLEXITY_API_KEY", "Perplexity"}, {"BROWSERBASE_API_KEY", "Browserbase"}, {"BROWSER_USE_API_KEY", "Browser Use"},
		{"FAL_KEY", "FAL"},
	}
}

// Lines telling the person which settings the administrator pins, none without a managed scope
func (s *Store) managedBanner(ctx context.Context) ([]Line, error) {
	keys, err := s.ManagedKeys(ctx)
	if err != nil {
		return nil, err
	}
	env, err := s.ManagedEnv(ctx)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 && len(env) == 0 {
		return nil, nil
	}
	out := []Line{blankLine(), tonedLine(ToneAlert, fmt.Sprintf(
		"  ⚷ Some settings are managed by your administrator (%s) and cannot be changed", ManagedDir()))}
	if len(keys) > 0 {
		out = append(out, tonedLine(ToneWarning, "    Managed config keys: "+strings.Join(keys, ", ")))
	}
	if len(env) > 0 {
		names := make([]string, 0, len(env))
		for name := range env {
			names = append(names, name)
		}
		slices.Sort(names)
		out = append(out, tonedLine(ToneWarning, "    Managed env keys: "+strings.Join(names, ", ")))
	}
	return out, nil
}

// Model lines of the summary, with the warning for a stale iteration cap left in the env file
func (s *Store) summaryModel(ctx context.Context, tree *yaml.Node) ([]Line, error) {
	out := sectionLines("Model")
	model := "not set"
	if at := mappingIndex(tree, "model"); at >= 0 {
		model = displayText(RedactConfigValue(tree.Content[at]))
	}
	out = append(out, plainLine("  Model:        "+model))
	turns := textOr(childOf(tree, "agent"), "max_turns", "None")
	out = append(out, plainLine("  Max turns:    "+turns))
	file, err := s.Env().File(ctx)
	if err != nil {
		return nil, err
	}
	if ghost, ok := file["ICEBATH_MAX_ITERATIONS"]; ok && trimEnvSpace(ghost) != trimEnvSpace(turns) {
		out = append(out, tonedLine(ToneWarning, fmt.Sprintf(
			"                ⚠ .env has stale ICEBATH_MAX_ITERATIONS=%s (run 'icebath doctor --fix' to remove)", ghost)))
	}
	return out, nil
}

// Word for a switch
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// Display lines of the summary
func (s *Store) summaryDisplay(cfg *Config) ([]Line, error) {
	if s.know.ActivePersonality == nil {
		return nil, &SeamError{Seam: "active personality"}
	}
	personality := s.know.ActivePersonality(cfg)
	if personality == "" {
		personality = "none"
	}
	d := cfg.Display
	return append(sectionLines("Display"),
		plainLine("  Personality:  "+personality),
		plainLine("  Reasoning:    "+onOff(d.ShowReasoning)),
		plainLine(fmt.Sprintf("  Bell:         complete=%s, prompt=%s", onOff(d.BellOnComplete), onOff(d.BellOnPrompt))),
		plainLine(fmt.Sprintf("  User preview: first %d line(s), last %d line(s)", d.UserMessagePreview.FirstLines, d.UserMessagePreview.LastLines)),
	), nil
}

// Terminal lines of the summary, with the lines of the selected backend
func (s *Store) summaryTerminal(ctx context.Context, tree *yaml.Node) ([]Line, error) {
	terminal := childOf(tree, "terminal")
	out := append(sectionLines("Terminal"),
		plainLine("  Backend:      "+textOr(terminal, "backend", "local")),
		plainLine("  Working dir:  "+textOr(terminal, "cwd", ".")),
		plainLine("  Timeout:      "+textOr(terminal, "timeout", "60")+"s"))
	have := func(names ...string) (bool, error) {
		for _, name := range names {
			v, err := s.envValue(ctx, name)
			if err != nil || v == "" {
				return false, err
			}
		}
		return true, nil
	}
	configured := func(names ...string) (string, error) {
		ok, err := have(names...)
		if ok {
			return "configured", err
		}
		return KeyNotSet, err
	}
	orNotSet := func(name string) (string, error) {
		v, err := s.envValue(ctx, name)
		if v == "" {
			v = KeyNotSet
		}
		return v, err
	}
	var err error
	var state string
	switch textOr(terminal, "backend", "") {
	case "docker":
		out = append(out, plainLine("  Docker image: "+textOr(terminal, "docker_image", DefaultSandboxImage)))
	case "modal":
		if state, err = configured("MODAL_TOKEN_ID"); err != nil {
			return nil, err
		}
		out = append(out, plainLine("  Modal image:  "+textOr(terminal, "modal_image", DefaultSandboxImage)), plainLine("  Modal token:  "+state))
	case "daytona":
		if state, err = configured("DAYTONA_API_KEY"); err != nil {
			return nil, err
		}
		out = append(out, plainLine("  Daytona image: "+textOr(terminal, "daytona_image", DefaultSandboxImage)), plainLine("  API key:      "+state))
	case "vercel_sandbox":
		image := orText(childOf(terminal, "vercel_runtime"))
		if image == "" {
			image = orText(childOf(terminal, "vercel_image"))
		}
		if image == "" {
			image = DefaultVercelImage
		}
		ok, err := have("VERCEL_OIDC_TOKEN")
		if err != nil {
			return nil, err
		}
		if !ok {
			if ok, err = have("VERCEL_TOKEN", "VERCEL_PROJECT_ID", "VERCEL_TEAM_ID"); err != nil {
				return nil, err
			}
		}
		state = KeyNotSet
		if ok {
			state = "configured"
		}
		out = append(out, plainLine("  Vercel image:   "+image), plainLine("  Vercel auth:    "+state))
	case "ssh":
		host, err := orNotSet("TERMINAL_SSH_HOST")
		if err != nil {
			return nil, err
		}
		user, err := orNotSet("TERMINAL_SSH_USER")
		if err != nil {
			return nil, err
		}
		out = append(out, plainLine("  SSH host:     "+host), plainLine("  SSH user:     "+user))
	}
	return out, nil
}

// Compression lines of the summary
func summaryCompression(cfg *Config) []Line {
	c := cfg.Compression
	out := sectionLines("Context Compression")
	if !c.Enabled {
		return append(out, plainLine("  Enabled:      no"))
	}
	out = append(out, plainLine("  Enabled:      yes"), plainLine(fmt.Sprintf("  Threshold:    %.0f%%", c.Threshold*100)))
	if c.ThresholdTokens != nil && *c.ThresholdTokens > 0 {
		out = append(out, plainLine(fmt.Sprintf("  Token cap:    %s tokens (takes lower of ratio vs absolute)", groupedDigits(*c.ThresholdTokens))))
	}
	out = append(out,
		plainLine(fmt.Sprintf("  Target ratio: %.0f%% of threshold preserved", c.TargetRatio*100)),
		plainLine(fmt.Sprintf("  Protect last: %d messages", c.ProtectLastN)),
		plainLine(fmt.Sprintf("  Protect first: %d non-system head messages", c.ProtectFirstN)))
	aux := cfg.Auxiliary.Compression
	model := aux.Model
	if model == "" {
		model = "(auto)"
	}
	out = append(out, plainLine("  Model:        "+model))
	if aux.Provider != "" && aux.Provider != "auto" {
		out = append(out, plainLine("  Provider:     "+aux.Provider))
	}
	return out
}

// Lines of the read only configuration summary, in the order the command prints them
func (s *Store) Summary(ctx context.Context) ([]Line, error) {
	cfg, err := s.LoadReadonly(ctx)
	if err != nil {
		return nil, err
	}
	tree, err := treeOf(cfg)
	if err != nil {
		return nil, err
	}
	out := []Line{
		blankLine(),
		tonedLine(ToneFrame, "┌─────────────────────────────────────────────────────────┐"),
		tonedLine(ToneFrame, "│                 icebath Configuration                   │"),
		tonedLine(ToneFrame, "└─────────────────────────────────────────────────────────┘"),
	}
	banner, err := s.managedBanner(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, banner...)
	configPath, envPath, err := s.Paths(ctx)
	if err != nil {
		return nil, err
	}
	install, err := home.InstallRoot()
	if err != nil {
		return nil, err
	}
	out = append(out, sectionLines("Paths")...)
	out = append(out, plainLine("  Config:       "+configPath), plainLine("  Secrets:      "+envPath), plainLine("  Install:      "+install))
	out = append(out, sectionLines("API Keys")...)
	for _, k := range summaryAPIKeys() {
		v, err := s.envValue(ctx, k[0])
		if err != nil {
			return nil, err
		}
		out = append(out, Line{Spans: []Span{{Text: fmt.Sprintf("  %-14s ", k[1])}, keySpan(v)}})
	}
	if s.know.AnthropicKey == nil {
		return nil, &SeamError{Seam: "anthropic key"}
	}
	anthropic, err := s.know.AnthropicKey(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, Line{Spans: []Span{{Text: fmt.Sprintf("  %-14s ", "Anthropic")}, keySpan(anthropic)}})
	for _, part := range []func() ([]Line, error){
		func() ([]Line, error) { return s.summaryModel(ctx, tree) },
		func() ([]Line, error) { return s.summaryDisplay(cfg) },
		func() ([]Line, error) { return s.summaryTerminal(ctx, tree) },
	} {
		lines, err := part()
		if err != nil {
			return nil, err
		}
		out = append(out, lines...)
	}
	out = append(out, sectionLines("Timezone")...)
	zone := Span{Text: "(server-local)", Tone: ToneDim}
	if tz := orText(childOf(tree, "timezone")); tz != "" {
		zone = Span{Text: tz}
	}
	out = append(out, Line{Spans: []Span{{Text: "  Timezone:     "}, zone}})
	out = append(out, summaryCompression(cfg)...)
	if v := cfg.Auxiliary.Vision; v.Provider != "auto" || v.Model != "" {
		parts := []string{"provider=" + v.Provider}
		if v.Model != "" {
			parts = append(parts, "model="+v.Model)
		}
		out = append(out, sectionLines("Auxiliary Models (overrides)")...)
		out = append(out, plainLine(fmt.Sprintf("  %-12s  %s", "Vision", strings.Join(parts, ", "))))
	}
	out = append(out, sectionLines("Messaging Platforms")...)
	for _, p := range [][2]string{{"Telegram", "TELEGRAM_BOT_TOKEN"}, {"Discord", "DISCORD_BOT_TOKEN"}} {
		v, err := s.envValue(ctx, p[1])
		if err != nil {
			return nil, err
		}
		state := Span{Text: "not configured", Tone: ToneDim}
		if v != "" {
			state = Span{Text: "configured"}
		}
		out = append(out, Line{Spans: []Span{{Text: fmt.Sprintf("  %-13s ", p[0]+":")}, state}})
	}
	skills, err := s.summarySkills(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, skills...)
	return append(out,
		blankLine(),
		tonedLine(ToneDim, strings.Repeat("─", 60)),
		tonedLine(ToneDim, "  icebath config edit     # Edit config file"),
		tonedLine(ToneDim, "  icebath config set <key> <value>"),
		tonedLine(ToneDim, "  icebath setup           # Run setup wizard"),
		blankLine()), nil
}

// Skill setting lines of the summary, none when no skill declares a setting
func (s *Store) summarySkills(ctx context.Context) ([]Line, error) {
	if s.know.SkillConfigVars == nil {
		return nil, &SeamError{Seam: "skill config variables"}
	}
	vars, err := s.know.SkillConfigVars(ctx)
	if err != nil || len(vars) == 0 {
		return nil, err
	}
	if s.know.SkillConfigValues == nil {
		return nil, &SeamError{Seam: "skill config values"}
	}
	resolved, err := s.know.SkillConfigValues(ctx, vars)
	if err != nil {
		return nil, err
	}
	out := sectionLines("Skill Settings")
	for _, v := range vars {
		value := Span{Text: KeyNotSet, Tone: ToneDim}
		if text := resolved[v.Key]; text != "" {
			value = Span{Text: text}
		}
		out = append(out, Line{Spans: []Span{{Text: fmt.Sprintf("  %-20s ", v.Key)}, value, {Text: "  "}, {Text: "[" + v.Skill + "]", Tone: ToneDim}}})
	}
	return out, nil
}

// One default key the merged config lacks
type MissingField struct {
	Key         string
	Default     *yaml.Node
	Description string
}

// Default keys the merged config of ctx lacks, the user named moa presets never descended into
func (s *Store) MissingFields(ctx context.Context) ([]MissingField, error) {
	defaults, err := s.defaults()
	if err != nil {
		return nil, err
	}
	tree, err := s.mergedTree(ctx)
	if err != nil {
		return nil, err
	}
	var out []MissingField
	var check func(def, cur *yaml.Node, prefix string)
	check = func(def, cur *yaml.Node, prefix string) {
		for i := 0; i+1 < len(def.Content); i += 2 {
			key := def.Content[i].Value
			if strings.HasPrefix(key, "_") {
				continue
			}
			full := key
			if prefix != "" {
				full = prefix + "." + key
			}
			at := mappingIndex(cur, key)
			switch {
			case at < 0:
				out = append(out, MissingField{Key: full, Default: plainCopy(def.Content[i+1]), Description: "New config option: " + full})
			case isMapping(def.Content[i+1]) && isMapping(cur.Content[at]) && !(prefix == "moa" && key == "presets"):
				check(def.Content[i+1], cur.Content[at], full)
			}
		}
	}
	check(defaults, tree, "")
	return out, nil
}

// Settings skills declare under skills.config that the config leaves unset or blank
func (s *Store) MissingSkillConfigVars(ctx context.Context) ([]SkillConfigVar, error) {
	if s.know.SkillConfigVars == nil {
		return nil, &SeamError{Seam: "skill config variables"}
	}
	vars, err := s.know.SkillConfigVars(ctx)
	if err != nil || len(vars) == 0 {
		return nil, err
	}
	tree, err := s.mergedTree(ctx)
	if err != nil {
		return nil, err
	}
	var out []SkillConfigVar
	for _, v := range vars {
		value := treeGet(tree, strings.Split("skills.config."+v.Key, ".")...)
		if value == nil || isNull(value) || (isStringNode(value) && trimEnvSpace(value.Value) == "") {
			out = append(out, v)
		}
	}
	return out, nil
}

// First two tools an optional variable unlocks under a format with one verb, empty when it unlocks none
func toolsSuffix(v EnvVar, format string) string {
	if len(v.Tools) == 0 {
		return ""
	}
	return fmt.Sprintf(format, strings.Join(v.Tools[:min(2, len(v.Tools))], ", "))
}

// Notes on saved toolsets and on bundled platforms disabled while their credentials are configured
func (s *Store) checkDiagnostics(ctx context.Context) ([]string, error) {
	if s.know.ToolsetDiagnostics == nil {
		return nil, &SeamError{Seam: "toolset diagnostics"}
	}
	raw, err := s.RawDocumentReadonly(ctx)
	if err != nil {
		return nil, err
	}
	out, err := s.know.ToolsetDiagnostics(ctx, raw.Node())
	if err != nil {
		return nil, err
	}
	cfg, err := s.LoadReadonly(ctx)
	if err != nil {
		return nil, err
	}
	var disabled []string
	if cfg.Plugins.Disabled != nil {
		disabled = *cfg.Plugins.Disabled
	}
	for _, set := range BundledPlatformEnv() {
		key := "platforms/" + set.Dir
		if !slices.Contains(disabled, key) && !slices.Contains(disabled, set.Name) {
			continue
		}
		required, all := 0, true
		for _, v := range set.Vars {
			if !v.Required {
				continue
			}
			required++
			value, err := s.envValue(ctx, v.Name)
			if err != nil {
				return nil, err
			}
			all = all && value != ""
		}
		if required > 0 && all {
			out = append(out, fmt.Sprintf("platform plugin '%s' is disabled while its required credentials are configured. "+
				"Run `icebath plugins enable %s` if you want it active.", key, key))
		}
	}
	return out, nil
}

// Lines of the report on missing and stale configuration, in the order the command prints them
func (s *Store) Check(ctx context.Context) ([]Line, error) {
	out := []Line{blankLine(), tonedLine(ToneTitle, "📋 Configuration Status"), blankLine()}
	current, latest, err := s.CheckConfigVersion(ctx, true)
	if err != nil {
		return nil, err
	}
	if current >= latest {
		out = append(out, plainLine(fmt.Sprintf("  Config version: %d ✓", current)))
	} else {
		out = append(out, tonedLine(ToneWarning, fmt.Sprintf("  Config version: %d → %d (update available)", current, latest)))
	}
	out = append(out, blankLine(), tonedLine(ToneHeading, "  Required:"), blankLine(), tonedLine(ToneHeading, "  Optional:"))
	registry := s.envs.registry
	for _, name := range registry.Names() {
		value, err := s.envValue(ctx, name)
		if err != nil {
			return nil, err
		}
		if value != "" {
			out = append(out, plainLine("    ✓ "+name))
			continue
		}
		entry, _ := registry.Get(name)
		out = append(out, tonedLine(ToneDim, "    ○ "+name+toolsSuffix(entry, " → %s")))
	}
	missing, err := s.MissingFields(ctx)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		out = append(out, blankLine(),
			tonedLine(ToneWarning, fmt.Sprintf("  %d new config option(s) available", len(missing))),
			plainLine("    Run 'icebath config migrate' to add them"))
	}
	diagnostics, err := s.checkDiagnostics(ctx)
	if err != nil {
		return nil, err
	}
	if len(diagnostics) > 0 {
		out = append(out, blankLine(), tonedLine(ToneHeading, "  Saved configuration:"))
		for _, d := range diagnostics {
			out = append(out, tonedLine(ToneWarning, "    ⚠ "+d))
		}
	}
	return append(out, blankLine()), nil
}

// What the command line does to edit the config, the editor to run on the path when one was found
type EditPlan struct {
	Path   string
	Editor string
	Lines  []Line
}

// Editors tried in order when neither EDITOR nor VISUAL names one
func editorCandidates() []string {
	if runtime.GOOS == "windows" {
		return []string{"notepad", "code", "vim", "vi", "nano"}
	}
	return []string{"nano", "vim", "vi", "code", "notepad"}
}

// Plan for opening the config file in an editor, seeding a missing file from the template first
func (s *Store) Edit(ctx context.Context) (*EditPlan, error) {
	plan := &EditPlan{}
	if l, err := s.managedInstallLine(ctx, "edit configuration"); err != nil || l != nil {
		if l != nil {
			plan.Lines = append(plan.Lines, *l)
		}
		return plan, err
	}
	b, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	if !b.SupportsFileTooling() {
		return nil, &CommandError{Text: "`icebath config edit` opens the local config file, and this config backend has none. " +
			"Use `icebath config set <key> <value>` instead."}
	}
	_, path, err := s.configPath(ctx)
	if err != nil {
		return nil, err
	}
	plan.Path = path
	created, err := s.SeedConfigFile(ctx)
	if err != nil {
		return nil, err
	}
	if created {
		plan.Lines = append(plan.Lines, plainLine("Created "+path))
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		for _, c := range editorCandidates() {
			if _, err := exec.LookPath(c); err == nil {
				editor = c
				break
			}
		}
	}
	if editor == "" {
		plan.Lines = append(plan.Lines, plainLine("No editor found. Config file is at:"), plainLine("  "+path))
		return plan, nil
	}
	plan.Editor = editor
	plan.Lines = append(plan.Lines, plainLine(fmt.Sprintf("Opening %s in %s...", path, editor)))
	return plan, nil
}
