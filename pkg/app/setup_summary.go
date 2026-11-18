package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// One voice provider summary row, available through any of its env names or an importable local engine module
type voiceSummaryRow struct {
	label   string
	envVars []string
	module  string
	hint    string
}

// Local engine rows name a module that must be importable
func (r voiceSummaryRow) local() bool { return r.module != "" }

var (
	ttsSummaryRows = map[string]voiceSummaryRow{
		"elevenlabs": {label: "ElevenLabs", envVars: []string{"ELEVENLABS_API_KEY"}},
		"openai":     {label: "OpenAI", envVars: []string{"VOICE_TOOLS_OPENAI_KEY", "OPENAI_API_KEY"}},
		"minimax":    {label: "MiniMax", envVars: []string{"MINIMAX_API_KEY"}},
		"mistral":    {label: "Mistral Voxtral", envVars: []string{"MISTRAL_API_KEY"}},
		"gemini":     {label: "Google Gemini", envVars: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
		"neutts":     {label: "NeuTTS", module: "neutts", hint: "type '/setup tts'"},
		"kittentts":  {label: "KittenTTS", module: "kittentts", hint: "type '/setup tts'"},
	}
	ttsSummaryDefault = voiceSummaryRow{label: "Edge TTS"}
	sttSummaryRows    = map[string]voiceSummaryRow{
		"openai":     {label: "OpenAI", envVars: []string{"VOICE_TOOLS_OPENAI_KEY", "OPENAI_API_KEY"}},
		"groq":       {label: "Groq Whisper", envVars: []string{"GROQ_API_KEY"}},
		"elevenlabs": {label: "ElevenLabs Scribe", envVars: []string{"ELEVENLABS_API_KEY"}},
		"xai":        {label: "xAI"},
		"deepinfra":  {label: "DeepInfra", envVars: []string{"DEEPINFRA_API_KEY"}},
	}
	sttSummaryDefault = voiceSummaryRow{label: "Local Whisper", module: "faster_whisper", hint: "run 'icebath tools' → Speech-to-Text"}
)

// Browser missing hints keyed by the configured provider, anything else getting the generic hint
var browserMissingHints = map[string]string{
	"Browserbase":   "BROWSERBASE_API_KEY/BROWSERBASE_PROJECT_ID",
	"Browser Use":   "BROWSER_USE_API_KEY",
	"Local browser": "Chromium: run 'icebath pm install chromium'",
}

const (
	browserMissingDefault = "Chromium (run 'icebath pm install chromium'), or configure Browser Use or Browserbase"
	webMissing            = "EXA_API_KEY, PARALLEL_API_KEY, FIRECRAWL_API_KEY/FIRECRAWL_API_URL, TAVILY_API_KEY, " +
		"PERPLEXITY_API_KEY, KEENABLE_API_KEY, or SEARXNG_URL"
)

var setupDoneBanner = []string{
	"┌─────────────────────────────────────────────────────────┐",
	"│              ✓ Setup Complete!                          │",
	"└─────────────────────────────────────────────────────────┘",
}

// Command and description rows, the description carrying its own alignment padding
var (
	editWizardRows = [][2]string{{"/setup", "                 Pick any setup section"}, {"/setup model", "           Change model/provider"}, {"/setup terminal", "        Change terminal backend"}, {"/setup gateway", "         Configure messaging"}, {"/setup tools", "           Configure tool providers"}}
	editConfigRows = [][2]string{{"icebath config", "         View current settings"}, {"icebath config edit", "    Open config in your editor"}, {"icebath config set <key> <value>", ""}}
	readyRows      = [][2]string{{"icebath", "              Start chatting"}, {"icebath gateway", "      Start messaging gateway"}, {"icebath doctor", "       Check for issues"}}
)

// One tool availability row, its name, whether it is available and the hint shown when it is not
type toolStatusRow struct {
	name      string
	available bool
	missing   string
}

// Seams over the probes the summary reads, the names the summary tests patch
var (
	setupResolveProvider = func(ctx context.Context) error {
		env := provider.HomePoolEnv(home.Dir(ctx))
		env.Ctx = ctx
		_, err := provider.ResolveAuthProvider(env, "")
		return err
	}
	setupToolFeatures = func(ctx context.Context, cfg config.Tree) (provider.ToolFeatures, error) {
		env := provider.HomePoolEnv(home.Dir(ctx))
		env.Ctx = ctx
		return provider.GetToolFeatures(env, cfg), nil
	}
	setupVisionBackends  = provider.GetAvailableVisionBackends
	setupModuleInstalled = provider.SetupModuleInstalled
)

// Summary row of a TTS or STT provider, a keyed provider missing its key falling through to the default row
func voiceProviderStatus(ctx context.Context, kind string, providerName any, rows map[string]voiceSummaryRow, def voiceSummaryRow) (toolStatusRow, error) {
	row := def
	switch name := providerName.(type) {
	case string:
		if r, ok := rows[name]; ok {
			row = r
		}
	case []any, map[string]any:
		return toolStatusRow{}, &provider.SetupTypeError{Msg: fmt.Sprintf("the %s provider is %s, expected a provider name", kind, errchain.Kind(providerName))}
	}
	if !row.local() && len(row.envVars) > 0 {
		set := false
		for _, v := range row.envVars {
			value, err := setupEnvValue(ctx, v)
			if err != nil {
				return toolStatusRow{}, err
			}
			if value != "" {
				set = true
				break
			}
		}
		if !set {
			row = def
		}
	}
	if !row.local() {
		return toolStatusRow{name: kind + " (" + row.label + ")", available: true}, nil
	}
	if setupModuleInstalled(ctx, row.module) {
		suffix := ""
		if kind == "Text-to-Speech" {
			suffix = " local"
		}
		return toolStatusRow{name: kind + " (" + row.label + suffix + ")", available: true}, nil
	}
	return toolStatusRow{name: kind + " (" + row.label + " — not installed)", missing: row.hint}, nil
}

// Display name of the first available plugin registered provider of a registry, skipping one name, empty when none
func firstAvailablePluginProvider(ctx context.Context, names []provider.CatalogProviderBase, skip string) string {
	plugins.GetPluginManager(ctx).DiscoverAndLoad(ctx, false)
	for _, p := range names {
		if skip != "" && p.Name() == skip {
			continue
		}
		if setupProviderAvailable(p) {
			return p.DisplayName()
		}
	}
	return ""
}

// Whether a registered provider reports available, a failing probe reading unavailable
func setupProviderAvailable(p provider.CatalogProviderBase) (available bool) {
	defer func() {
		if recover() != nil {
			available = false
		}
	}()
	return p.IsAvailable()
}

// Registered image and video generation providers as catalog providers, set by the media backends and nil in a build without them
var setupImageGenProviders, setupVideoGenProviders func(ctx context.Context) []provider.CatalogProviderBase

// Row of a tool feature, available with its provider before the missing hint
func providerRow(feature provider.ToolFeatureState, name, missingHint string) toolStatusRow {
	if feature.Available {
		if feature.CurrentProvider != "" {
			return toolStatusRow{name: name + " (" + feature.CurrentProvider + ")", available: true}
		}
		return toolStatusRow{name: name, available: true}
	}
	return toolStatusRow{name: name, missing: missingHint}
}

// One summary row builder, its rows in order, none leaving the row out
type toolRowBuilder func(ctx context.Context, cfg config.Tree, feats provider.ToolFeatures) ([]toolStatusRow, error)

// Vision through the same resolver the vision tools use
func visionRow(ctx context.Context, _ config.Tree, _ provider.ToolFeatures) ([]toolStatusRow, error) {
	backends, err := setupVisionBackends(ctx)
	ok := err == nil && len(backends) > 0
	hint := ""
	if !ok {
		hint = "type '/setup tools' to configure"
	}
	return []toolStatusRow{{"Vision (image analysis)", ok, hint}}, nil
}

// Web search and extract through Exa, Parallel, Firecrawl, Tavily or Keenable
func webRow(_ context.Context, _ config.Tree, feats provider.ToolFeatures) ([]toolStatusRow, error) {
	return []toolStatusRow{providerRow(feats.Feature("web"), "Web Search & Extract", webMissing)}, nil
}

// Browser automation, the missing hint keyed by the configured provider
func browserRow(_ context.Context, _ config.Tree, feats provider.ToolFeatures) ([]toolStatusRow, error) {
	browser := feats.Feature("browser")
	hint, ok := browserMissingHints[browser.CurrentProvider]
	if !ok {
		hint = browserMissingDefault
	}
	return []toolStatusRow{providerRow(browser, "Browser Automation", hint)}, nil
}

// Image generation through FAL directly, else the first available plugin provider, absent without the media backends
func imageGenRow(ctx context.Context, _ config.Tree, feats provider.ToolFeatures) ([]toolStatusRow, error) {
	if setupImageGenProviders == nil {
		return nil, nil
	}
	if feats.Feature("image_gen").Available {
		return []toolStatusRow{{name: "Image Generation", available: true}}, nil
	}
	if backend := firstAvailablePluginProvider(ctx, setupImageGenProviders(ctx), "fal"); backend != "" {
		return []toolStatusRow{{name: "Image Generation (" + backend + ")", available: true}}, nil
	}
	return []toolStatusRow{{name: "Image Generation", missing: "FAL_KEY or OPENAI_API_KEY"}}, nil
}

// Video generation, shown only when a plugin provider reports available
func videoGenRow(ctx context.Context, _ config.Tree, _ provider.ToolFeatures) ([]toolStatusRow, error) {
	if setupVideoGenProviders == nil {
		return nil, nil
	}
	if backend := firstAvailablePluginProvider(ctx, setupVideoGenProviders(ctx), ""); backend != "" {
		return []toolStatusRow{{name: "Video Generation (" + backend + ")", available: true}}, nil
	}
	return nil, nil
}

// Text to speech of the configured provider, gated on its key or local install
func ttsRow(ctx context.Context, cfg config.Tree, _ provider.ToolFeatures) ([]toolStatusRow, error) {
	ttsProvider, ok := config.CfgGet(cfg, "tts", "provider")
	if !ok {
		ttsProvider = "edge"
	}
	row, err := voiceProviderStatus(ctx, "Text-to-Speech", ttsProvider, ttsSummaryRows, ttsSummaryDefault)
	return []toolStatusRow{row}, err
}

// Speech to text of the configured provider, local Whisper when none is set
func sttRow(ctx context.Context, cfg config.Tree, _ provider.ToolFeatures) ([]toolStatusRow, error) {
	sttProvider, ok := config.CfgGet(cfg, "stt", "provider")
	if !ok || !pyrepr.Truthy(sttProvider) {
		sttProvider = "local"
	}
	row, err := voiceProviderStatus(ctx, "Speech-to-Text", sttProvider, sttSummaryRows, sttSummaryDefault)
	return []toolStatusRow{row}, err
}

// Modal execution for a modal backend, direct when its credentials are set and missing otherwise
func modalRow(_ context.Context, cfg config.Tree, feats provider.ToolFeatures) ([]toolStatusRow, error) {
	if backend, _ := config.CfgGet(cfg, "terminal", "backend"); backend == "modal" {
		if feats.Feature("modal").Available {
			return []toolStatusRow{{name: "Modal Execution (direct Modal)", available: true}}, nil
		}
		return []toolStatusRow{{name: "Modal Execution", missing: "type '/setup terminal'"}}, nil
	}
	return nil, nil
}

// Skills hub access through a GitHub token
func skillsHubRow(ctx context.Context, _ config.Tree, _ provider.ToolFeatures) ([]toolStatusRow, error) {
	token, err := setupEnvValue(ctx, "GITHUB_TOKEN")
	if err != nil {
		return nil, err
	}
	hint := ""
	if token == "" {
		hint = "GITHUB_TOKEN"
	}
	return []toolStatusRow{{"Skills Hub (GitHub)", token != "", hint}}, nil
}

// Terminal, task planning and skills, always available
func alwaysOnRows(context.Context, config.Tree, provider.ToolFeatures) ([]toolStatusRow, error) {
	return []toolStatusRow{{name: "Terminal/Commands", available: true}, {name: "Task Planning (todo)", available: true},
		{name: "Skills (view, create, edit)", available: true}}, nil
}

// Row builders in summary order
var toolRowBuilders = []toolRowBuilder{visionRow, webRow, browserRow, imageGenRow, videoGenRow, ttsRow, sttRow, modalRow, skillsHubRow, alwaysOnRows}

// Tool availability rows in summary order, a builder that returns none leaving its row out
func setupToolStatusRows(ctx context.Context, cfg config.Tree, feats provider.ToolFeatures) ([]toolStatusRow, error) {
	var rows []toolStatusRow
	for _, build := range toolRowBuilders {
		built, err := build(ctx, cfg, feats)
		if err != nil {
			return nil, err
		}
		rows = append(rows, built...)
	}
	return rows, nil
}

// Prints command rows as a green command followed by its padded description
func printCmdRows(rows [][2]string) {
	for _, row := range rows {
		setupPrintln("   " + setupColor(row[0], cliui.Green) + row[1])
	}
}

// Prints a dim rule and a bold cyan title, each followed by a blank line
func printSectionHeader(title string) {
	setupPrintln(setupColor(strings.Repeat("─", 60), cliui.Dim) + "\n")
	setupPrintln(setupColor(title, cliui.Cyan, cliui.Bold) + "\n")
}

// Prints the setup completion summary, provider readiness, tool availability and where everything lives
func printSetupSummary(ctx context.Context, cfg config.Tree, icebathHome string) error {
	h := home.Dir(ctx)
	if err := setupResolveProvider(ctx); err != nil {
		setupPrintln("")
		provider.SetupPrintWarning("No inference provider is configured — Icebath cannot chat yet.")
		provider.SetupInfo("  Finish this one step with:",
			"    icebath model            (pick any provider/model)")
	}
	setupPrintln("")
	provider.PrintSetupHeader("Tool Availability Summary", false)
	feats, err := setupToolFeatures(ctx, cfg)
	if err != nil {
		return err
	}
	toolStatus, err := setupToolStatusRows(ctx, cfg, feats)
	if err != nil {
		return err
	}
	availableCount := 0
	for _, row := range toolStatus {
		if row.available {
			availableCount++
		}
	}
	provider.SetupInfo(fmt.Sprintf("%d/%d tool categories available:", availableCount, len(toolStatus)), nil)
	for _, row := range toolStatus {
		if row.available {
			setupPrintln("   " + setupColor("✓", cliui.Green) + " " + row.name)
		} else {
			setupPrintln("   " + setupColor("✗", cliui.Red) + " " + row.name + " " + setupColor("(missing "+row.missing+")", cliui.Dim))
		}
	}
	setupPrintln("")
	display := home.Display(h)
	if availableCount < len(toolStatus) {
		provider.SetupPrintWarning("Some tools are disabled. Type '/setup tools' to configure them,")
		provider.SetupPrintWarning("or edit " + display + "/.env directly to add the missing API keys.")
		setupPrintln("")
	}
	setupPrintln("")
	for _, line := range setupDoneBanner {
		setupPrintln(setupColor(line, cliui.Green))
	}
	setupPrintln("")
	setupPrintln(setupColor("📁 All your files are in "+display+"/:", cliui.Cyan, cliui.Bold) + "\n")
	for _, row := range [][2]string{{"Settings:", "  " + home.ConfigPath(h)}, {"API Keys:", "  " + home.EnvPath(h)},
		{"Data:", "      " + icebathHome + "/cron/, sessions/, logs/"}} {
		setupPrintln("   " + setupColor(row[0], cliui.Yellow) + row[1])
	}
	setupPrintln("")
	printSectionHeader("📝 To edit your configuration:")
	printCmdRows(editWizardRows)
	setupPrintln("")
	printCmdRows(editConfigRows)
	setupPrintln("                          Set a specific value\n\n   Or edit the files directly:")
	for _, path := range []string{home.ConfigPath(h), home.EnvPath(h)} {
		setupPrintln("   " + setupColor("nano "+path, cliui.Dim))
	}
	setupPrintln("")
	printSectionHeader("🚀 Ready to go!")
	printCmdRows(readyRows)
	setupPrintln("")
	return nil
}
