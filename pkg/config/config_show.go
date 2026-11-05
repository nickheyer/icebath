package config

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

const defaultVercelImage = "vercel/sandbox/universal:latest"

// One skill declared config variable as config show lists it
type SkillConfigVar struct {
	Key         string
	Skill       string
	Description string
	Prompt      string
	Default     any
}

// Readers config show consults, supplied by the facade, auth, personality, skills and redaction owners
type ShowSources struct {
	Config            map[string]any
	ConfigPath        string
	EnvPath           string
	ProjectRoot       string
	EnvValue          func(name string) string
	EnvFile           func() (map[string]string, error)
	AnthropicKey      func() string
	ActivePersonality func(cfg map[string]any) (string, error)
	SkillSettings     func() ([]SkillConfigVar, map[string]any, error)
	MaskSecret        func(value, empty string) string
	RedactConfigValue func(value any) any
}

var showConfigAPIKeys = [][2]string{
	{"OPENROUTER_API_KEY", "OpenRouter"}, {"VOICE_TOOLS_OPENAI_KEY", "OpenAI (STT/TTS)"}, {"EXA_API_KEY", "Exa"},
	{"PARALLEL_API_KEY", "Parallel"}, {"FIRECRAWL_API_KEY", "Firecrawl"}, {"TAVILY_API_KEY", "Tavily"},
	{"PERPLEXITY_API_KEY", "Perplexity"}, {"BROWSERBASE_API_KEY", "Browserbase"}, {"BROWSER_USE_API_KEY", "Browser Use"},
	{"FAL_KEY", "FAL"},
}

type showPrinter struct{ out io.Writer }

func (p showPrinter) line(format string, args ...any) { fmt.Fprintf(p.out, format+"\n", args...) }

func (p showPrinter) section(title string) {
	p.line("")
	p.line("%s", colorize("◆ "+title, ColorCyan, ColorBold))
}

// Value at key of a mapping or the fallback when the mapping lacks it
func getOr(m map[string]any, key string, fallback any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return fallback
}

// Mapping at key of a mapping, empty when absent and nil when present but not a mapping
func subMap(m map[string]any, key string) map[string]any {
	v, ok := m[key]
	if !ok {
		return map[string]any{}
	}
	return AsMap(v)
}

// Number of a numeric config value for percentage display
func showNumber(v any, key string) (float64, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case int:
		return float64(x), nil
	case float64:
		return x, nil
	}
	return 0, fmt.Errorf("compression.%s must be a number, got %s", key, PyTypeName(v))
}

// Integer conversion of a value the way Python int accepts it
func pyInt(v any) (int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return x, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int(x), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		return n, err == nil
	}
	return 0, false
}

// Integer text with comma thousands separators
func commaInt(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// Administrator pinned config and env keys with the managed dir, nothing without a managed scope
func (p showPrinter) managedBanner() {
	keys := ManagedConfigKeys()
	env := LoadManagedEnv()
	if len(keys) == 0 && len(env) == 0 {
		return
	}
	p.line("")
	p.line("%s", colorize("  ⚷ Some settings are managed by your administrator ("+ManagedDir()+") and cannot be changed", ColorYellow, ColorBold))
	envKeys := make([]string, 0, len(env))
	for k := range env {
		envKeys = append(envKeys, k)
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	sort.Strings(envKeys)
	for _, row := range []struct {
		label string
		keys  []string
	}{{"config", sorted}, {"env", envKeys}} {
		if len(row.keys) > 0 {
			p.line("%s", colorize("    Managed "+row.label+" keys: "+strings.Join(row.keys, ", "), ColorYellow))
		}
	}
}

func (p showPrinter) modelSection(cfg map[string]any, src ShowSources) {
	p.section("Model")
	p.line("  Model:        %s", PyStr(src.RedactConfigValue(getOr(cfg, "model", "not set"))))
	maxTurns := getOr(subMap(cfg, "agent"), "max_turns", Defaults().Map("agent")["max_turns"])
	p.line("  Max turns:    %s", PyStr(maxTurns))
	env, err := src.EnvFile()
	if err != nil {
		return
	}
	if ghost, ok := env["ICEBATH_MAX_ITERATIONS"]; ok && strings.TrimSpace(ghost) != strings.TrimSpace(PyStr(maxTurns)) {
		p.line("%s", colorize("                ⚠ .env has stale ICEBATH_MAX_ITERATIONS="+ghost+" (run 'icebath doctor --fix' to remove)", ColorYellow))
	}
}

func (p showPrinter) displaySection(cfg map[string]any, src ShowSources) {
	p.section("Display")
	display := subMap(cfg, "display")
	personality, err := src.ActivePersonality(cfg)
	if err != nil {
		personality = PyStr(display["personality"])
		if !PyTruthy(display["personality"]) {
			personality = ""
		}
	}
	if personality == "" {
		personality = "none"
	}
	onOff := func(v any) string {
		if PyTruthy(v) {
			return "on"
		}
		return "off"
	}
	p.line("  Personality:  %s", personality)
	p.line("  Reasoning:    %s", onOff(getOr(display, "show_reasoning", true)))
	p.line("  Bell:         complete=%s, prompt=%s", onOff(getOr(display, "bell_on_complete", false)), onOff(getOr(display, "bell_on_prompt", false)))
	ump := AsMap(getOr(display, "user_message_preview", map[string]any{}))
	if ump == nil {
		ump = map[string]any{}
	}
	p.line("  User preview: first %s line(s), last %s line(s)", PyStr(getOr(ump, "first_lines", 2)), PyStr(getOr(ump, "last_lines", 2)))
}

func (p showPrinter) terminalSection(cfg map[string]any, src ShowSources) {
	p.section("Terminal")
	terminal := subMap(cfg, "terminal")
	p.line("  Backend:      %s", PyStr(getOr(terminal, "backend", "local")))
	p.line("  Working dir:  %s", PyStr(getOr(terminal, "cwd", ".")))
	p.line("  Timeout:      %ss", PyStr(getOr(terminal, "timeout", 60)))
	configured := func(names ...string) string {
		for _, n := range names {
			if src.EnvValue(n) == "" {
				return "(not set)"
			}
		}
		return "configured"
	}
	orNotSet := func(v string) string {
		if v == "" {
			return "(not set)"
		}
		return v
	}
	backend, _ := terminal["backend"].(string)
	switch backend {
	case "docker":
		p.line("  Docker image: %s", PyStr(getOr(terminal, "docker_image", DefaultSandboxImage)))
	case "modal":
		p.line("  Modal image:  %s", PyStr(getOr(terminal, "modal_image", DefaultSandboxImage)))
		p.line("  Modal token:  %s", configured("MODAL_TOKEN_ID"))
	case "daytona":
		p.line("  Daytona image: %s", PyStr(getOr(terminal, "daytona_image", DefaultSandboxImage)))
		p.line("  API key:      %s", configured("DAYTONA_API_KEY"))
	case "vercel_sandbox":
		image := any(defaultVercelImage)
		if PyTruthy(terminal["vercel_runtime"]) {
			image = terminal["vercel_runtime"]
		} else if PyTruthy(terminal["vercel_image"]) {
			image = terminal["vercel_image"]
		}
		auth := "(not set)"
		if src.EnvValue("VERCEL_OIDC_TOKEN") != "" || (src.EnvValue("VERCEL_TOKEN") != "" && src.EnvValue("VERCEL_PROJECT_ID") != "" && src.EnvValue("VERCEL_TEAM_ID") != "") {
			auth = "configured"
		}
		p.line("  Vercel image:   %s", PyStr(image))
		p.line("  Vercel auth:    %s", auth)
	case "ssh":
		p.line("  SSH host:     %s", orNotSet(src.EnvValue("TERMINAL_SSH_HOST")))
		p.line("  SSH user:     %s", orNotSet(src.EnvValue("TERMINAL_SSH_USER")))
	}
}

func (p showPrinter) compressionSection(cfg map[string]any) error {
	p.section("Context Compression")
	compression := subMap(cfg, "compression")
	enabled := PyTruthy(getOr(compression, "enabled", true))
	if !enabled {
		p.line("  Enabled:      no")
		return nil
	}
	p.line("  Enabled:      yes")
	threshold, err := showNumber(getOr(compression, "threshold", 0.50), "threshold")
	if err != nil {
		return err
	}
	p.line("  Threshold:    %.0f%%", threshold*100)
	if tt := compression["threshold_tokens"]; tt != nil {
		if n, ok := pyInt(tt); ok && n > 0 {
			p.line("  Token cap:    %s tokens (takes lower of ratio vs absolute)", commaInt(n))
		}
	}
	ratio, err := showNumber(getOr(compression, "target_ratio", 0.20), "target_ratio")
	if err != nil {
		return err
	}
	p.line("  Target ratio: %.0f%% of threshold preserved", ratio*100)
	p.line("  Protect last: %s messages", PyStr(getOr(compression, "protect_last_n", 20)))
	p.line("  Protect first: %s non-system head messages", PyStr(getOr(compression, "protect_first_n", 3)))
	aux := subMap(subMap(cfg, "auxiliary"), "compression")
	model := "(auto)"
	if v := getOr(aux, "model", ""); PyTruthy(v) {
		model = PyStr(v)
	}
	p.line("  Model:        %s", model)
	if prov := getOr(aux, "provider", "auto"); PyTruthy(prov) && prov != "auto" {
		p.line("  Provider:     %s", PyStr(prov))
	}
	return nil
}

func (p showPrinter) auxOverrides(cfg map[string]any) {
	vision := subMap(subMap(cfg, "auxiliary"), "vision")
	prov, model := getOr(vision, "provider", "auto"), getOr(vision, "model", "")
	if prov == "auto" && !PyTruthy(model) {
		return
	}
	p.section("Auxiliary Models (overrides)")
	parts := []string{"provider=" + PyStr(prov)}
	if PyTruthy(model) {
		parts = append(parts, "model="+PyStr(model))
	}
	p.line("  %-12s  %s", "Vision", strings.Join(parts, ", "))
}

func (p showPrinter) skillSettings(src ShowSources) {
	vars, resolved, err := src.SkillSettings()
	if err != nil || len(vars) == 0 {
		return
	}
	p.section("Skill Settings")
	for _, v := range vars {
		display := colorize("(not set)", ColorDim)
		if val := resolved[v.Key]; PyTruthy(val) {
			display = PyStr(val)
		}
		p.line("  %-20s %s  %s", v.Key, display, colorize("["+v.Skill+"]", ColorDim))
	}
}

// Prints the current configuration with paths, masked keys and the main sections
func ShowConfig(out io.Writer, src ShowSources) error {
	p := showPrinter{out: out}
	cfg := src.Config
	p.line("")
	p.line("%s", colorize("┌─────────────────────────────────────────────────────────┐", ColorCyan))
	p.line("%s", colorize("│              ☤ Icebath Configuration                   │", ColorCyan))
	p.line("%s", colorize("└─────────────────────────────────────────────────────────┘", ColorCyan))
	p.managedBanner()
	p.section("Paths")
	p.line("  Config:       %s", src.ConfigPath)
	p.line("  Secrets:      %s", src.EnvPath)
	p.line("  Install:      %s", src.ProjectRoot)
	p.section("API Keys")
	notSet := colorize("(not set)", ColorDim)
	for _, row := range showConfigAPIKeys {
		p.line("  %-14s %s", row[1], src.MaskSecret(src.EnvValue(row[0]), notSet))
	}
	p.line("  %-14s %s", "Anthropic", src.MaskSecret(src.AnthropicKey(), notSet))
	p.modelSection(cfg, src)
	p.displaySection(cfg, src)
	p.terminalSection(cfg, src)
	p.section("Timezone")
	tz := colorize("(server-local)", ColorDim)
	if v := getOr(cfg, "timezone", ""); PyTruthy(v) {
		tz = PyStr(v)
	}
	p.line("  Timezone:     %s", tz)
	if err := p.compressionSection(cfg); err != nil {
		return err
	}
	p.auxOverrides(cfg)
	p.section("Messaging Platforms")
	for _, row := range [][2]string{{"Telegram", "TELEGRAM_BOT_TOKEN"}, {"Discord", "DISCORD_BOT_TOKEN"}} {
		state := colorize("not configured", ColorDim)
		if src.EnvValue(row[1]) != "" {
			state = "configured"
		}
		p.line("  %-13s %s", row[0]+":", state)
	}
	p.skillSettings(src)
	p.line("")
	p.line("%s", colorize(strings.Repeat("─", 60), ColorDim))
	p.line("%s", colorize("  icebath config edit    # Edit config file", ColorDim))
	p.line("%s", colorize("  icebath config set <key> <value>", ColorDim))
	p.line("%s", colorize("  icebath setup          # Run setup wizard", ColorDim))
	p.line("")
	return nil
}
