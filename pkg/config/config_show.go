package config

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
)

// One skill declared config variable as config show lists it
type SkillConfigVar struct {
	Key         string
	Skill       string
	Description string
	Prompt      string
	Default     any
}

// Readers config show consults, supplied by the facade, auth, skills and redaction owners
type ShowSources struct {
	ConfigPath    string
	EnvPath       string
	ProjectRoot   string
	EnvValue      func(name string) string
	AnthropicKey  func() string
	SkillSettings func() ([]SkillConfigVar, map[string]any, error)
	MaskSecret    func(value, empty string) string
}

// One settings group as config show prints it
type ShowGroup struct {
	Title string
	Rows  []ShowRow
}

// One setting as config show prints it: its label, its value as text and where the value came from
type ShowRow struct {
	Label, Value, Source string
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

// Every settings group with its everyday settings and the advanced ones that are set
func (p showPrinter) settingsSections(ctx context.Context) {
	if settingsHooks.Show == nil {
		return
	}
	for _, group := range settingsHooks.Show(ctx) {
		p.section(group.Title)
		for _, row := range group.Rows {
			value := row.Value
			if row.Source != "default" && row.Source != "global" {
				value += colorize("  ("+row.Source+")", ColorDim)
			}
			p.line("  %-22s %s", row.Label+":", value)
		}
	}
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

// Prints the configuration of the home in ctx: paths, masked keys, then every settings group
func ShowConfig(ctx context.Context, out io.Writer, src ShowSources) error {
	p := showPrinter{out: out}
	p.line("")
	p.line("%s", colorize("┌─────────────────────────────────────────────────────────┐", ColorCyan))
	p.line("%s", colorize("│              󰆧 Icebath Configuration                    │", ColorCyan))
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
	p.settingsSections(ctx)
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
	p.line("%s", colorize("  icebath, type /setup   # Set up in the terminal UI", ColorDim))
	p.line("")
	return nil
}
