package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Setup module names the wizard resolves at call time, the hermes_cli.setup attributes tests replace
type ttsSetupModule struct {
	prompt          func(question, def string, password bool) string
	promptChoice    func(question string, choices []string, def int, description string) int
	promptYesNo     func(question string, def bool, description string) bool
	printHeader     func(title string)
	info            func(lines ...any)
	printInfo       func(text string)
	printSuccess    func(text string)
	printWarning    func(text string)
	printError      func(text string)
	getEnvValue     func(ctx context.Context, key string) (string, error)
	saveEnvValue    func(ctx context.Context, key, value string) error
	saveConfig      func(ctx context.Context, cfg config.Tree) error
	moduleInstalled func(ctx context.Context, name string) bool
}

var ttsSetup = ttsSetupModule{
	prompt:       provider.SetupPrompt,
	promptChoice: provider.PromptChoice,
	promptYesNo:  provider.PromptYesNo,
	printHeader:  func(title string) { provider.PrintSetupHeader(title, false) },
	info:         provider.SetupInfo,
	printInfo:    provider.SetupPrintInfo,
	printSuccess: provider.SetupPrintSuccess,
	printWarning: provider.SetupPrintWarning,
	printError:   provider.SetupPrintError,
	getEnvValue: func(ctx context.Context, key string) (string, error) {
		v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
		return v, err
	},
	saveEnvValue: func(ctx context.Context, key, value string) error {
		return config.SaveEnvValueContext(ctx, home.Dir(ctx), key, value)
	},
	saveConfig: func(ctx context.Context, cfg config.Tree) error {
		return config.SaveConfig(ctx, home.Dir(ctx), map[string]any(cfg), config.SaveOptions{})
	},
	moduleInstalled: provider.SetupModuleInstalled,
}

// Package manager, host, subscription and xAI login calls of the wizard, the patched names of its tests
type ttsSetupHooks struct {
	extraSupported          func(ctx context.Context, extra string) (bool, error)
	syncVenv                func(ctx context.Context, opts pm.SyncVenvOptions) error
	which                   func(name string) bool
	runInstall              func(ctx context.Context, argv []string) error
	platform                func() string
	managedNousToolsEnabled func(ctx context.Context, cfg config.Tree) bool
	nousAuthPresent         func(ctx context.Context, cfg config.Tree) bool
	xaiOAuthLoggedIn        func(ctx context.Context) bool
	xaiOAuthLogin           func(ctx context.Context) bool
}

var ttsHooks = ttsSetupHooks{
	extraSupported: pm.ExtraSupported,
	syncVenv:       pm.SyncVenv,
	which: func(name string) bool {
		_, found := pyshutil.Which(name, nil)
		return found
	},
	runInstall: runInheritedCheck,
	platform:   sysPlatform,
	managedNousToolsEnabled: func(ctx context.Context, cfg config.Tree) bool {
		h := home.Dir(ctx)
		return tools.ManagedNousToolsEnabled(&tools.Env{Home: h, Config: cfg, Secrets: config.NewSecrets(h)}, false)
	},
	nousAuthPresent: func(ctx context.Context, cfg config.Tree) bool {
		return provider.GetNousSubscriptionFeatures(ttsPoolEnv(ctx), map[string]any(cfg), false).NousAuthPresent
	},
	xaiOAuthLoggedIn: xaiOAuthLoggedInForSetup,
	xaiOAuthLogin:    func(ctx context.Context) bool { return provider.RunXAIOAuthLoginFromSetup(ttsPoolEnv(ctx)) },
}

// Pool environment of the home in ctx so credential reads and writes follow the profile
func ttsPoolEnv(ctx context.Context) provider.PoolEnv {
	h := home.Dir(ctx)
	return provider.PoolEnv{Store: provider.NewStore(h), Secrets: config.NewSecrets(h), Scope: provider.SecretScopeFrom(ctx),
		Config: provider.ConfigLoader(h), Ctx: ctx}
}

// Python sys.platform of the host for the platforms the espeak table names
func sysPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// Runs argv on the inherited terminal like subprocess.run with check, a nonzero exit raising CalledProcessError
func runInheritedCheck(ctx context.Context, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &pysubprocess.CalledProcessError{Cmd: argv, Returncode: pysubprocess.Returncode(exitErr)}
	}
	return err
}

// Prints a bare newline on the setup console
func ttsPrintln() { fmt.Fprintln(provider.SetupOutput()) }

// Records the engine in the dependency union for the next launch, reporting a refused install with the retry command
func installTTSExtra(ctx context.Context, extra string) (bool, error) {
	err := ttsHooks.syncVenv(ctx, pm.SyncVenvOptions{Extras: []string{extra}, Explicit: true})
	if err != nil {
		if !tools.SetupSyncFailureHandled(err) {
			return false, err
		}
		detail := err.Error()
		if pypath.IsOSError(err) {
			detail = pypath.OSErrorText(err)
		}
		ttsSetup.printError(fmt.Sprintf("Failed to install %s: %s", extra, detail))
		ttsSetup.printInfo("Retry with: icebath setup tts")
		return false, nil
	}
	ttsSetup.printSuccess(extra + " installed. Restart Icebath to use it.")
	return true, nil
}

// Manual install hint and install command of espeak-ng per sys.platform, anything else using linux
var espeakInstall = map[string]struct {
	hint string
	cmd  []string
}{
	"darwin": {"Install with: brew install espeak-ng", []string{"brew", "install", "espeak-ng"}},
	"win32":  {"Install with: choco install espeak-ng", []string{"choco", "install", "espeak-ng", "-y"}},
	"linux":  {"Install with: sudo apt install espeak-ng", []string{"sudo", "apt", "install", "-y", "espeak-ng"}},
}

// Installs the NeuTTS engine and offers the espeak-ng system package it phonemizes with, true on success
func installNeuTTSDeps(ctx context.Context) (bool, error) {
	supported, err := ttsHooks.extraSupported(ctx, "neutts")
	if err != nil {
		return false, err
	}
	if !supported {
		ttsSetup.printError("NeuTTS is not supported by this Python/platform.")
		return false, nil
	}
	if !ttsHooks.which("espeak-ng") && !ttsHooks.which("espeak") {
		row, ok := espeakInstall[ttsHooks.platform()]
		if !ok {
			row = espeakInstall["linux"]
		}
		ttsPrintln()
		ttsSetup.printWarning("NeuTTS requires espeak-ng for phonemization.")
		ttsSetup.printInfo(row.hint)
		ttsPrintln()
		if ttsSetup.promptYesNo("Install espeak-ng now?", true, "") {
			if err := ttsHooks.runInstall(ctx, row.cmd); err != nil {
				var called *pysubprocess.CalledProcessError
				detail := ""
				switch {
				case errors.As(err, &called):
					detail = called.Error()
				case pypath.OSErrorClass(err) == "FileNotFoundError":
					detail = pypath.OSErrorText(err)
				default:
					return false, err
				}
				ttsSetup.printWarning("Could not install espeak-ng automatically: " + detail)
				ttsSetup.printInfo("Please install it manually and re-run setup.")
				return false, nil
			}
			ttsSetup.printSuccess("espeak-ng installed")
		} else {
			ttsSetup.printWarning("espeak-ng is required for NeuTTS. Install it manually before using NeuTTS.")
		}
	}
	ttsSetup.info(nil, "Installing neutts Python package...", "This will also download the TTS model (~300MB) on first use.", nil)
	return installTTSExtra(ctx, "neutts")
}

// Installs the KittenTTS engine, true on success
func installKittenTTSDeps(ctx context.Context) (bool, error) {
	ttsSetup.info(nil, "Installing kittentts Python package (~25-80MB model downloaded on first use)...", nil)
	return installTTSExtra(ctx, "kittentts")
}

// Whether xAI Grok OAuth credentials are stored so the TTS setup can skip the API key prompt
func xaiOAuthLoggedInForSetup(ctx context.Context) bool {
	return pyrepr.Truthy(provider.GetXAIOAuthAuthStatus(ttsPoolEnv(ctx))["logged_in"])
}

// One provider row of the TTS menu
type ttsChoice struct{ key, label string }

var ttsProviderChoices = []ttsChoice{
	{"edge", "Edge TTS (free, cloud-based, no setup needed)"},
	{"elevenlabs", "ElevenLabs (premium quality, needs API key)"},
	{"openai", "OpenAI TTS (good quality, needs API key)"},
	{"xai", "xAI TTS (Grok voices — OAuth login or API key)"},
	{"minimax", "MiniMax TTS (high quality with voice cloning, needs API key)"},
	{"mistral", "Mistral Voxtral TTS (multilingual, native Opus, needs API key)"},
	{"gemini", "Google Gemini TTS (30 prebuilt voices, prompt-controllable, needs API key)"},
	{"neutts", "NeuTTS (local on-device, free, ~300MB model download)"},
	{"kittentts", "KittenTTS (local on-device, free, lightweight ~25-80MB ONNX)"},
}

// Short provider label, the menu label up to its parenthetical
var ttsProviderLabels = func() map[string]string {
	out := map[string]string{}
	for _, c := range ttsProviderChoices {
		head, _, _ := strings.Cut(c.label, " (")
		out[c.key] = head
	}
	return out
}()

// One Edge voice row, the voice id and its menu label
type edgeVoice struct {
	voice any
	label string
}

var edgeVoiceChoices = []edgeVoice{
	{"en-US-AriaNeural", "English (US) — Aria"},
	{"zh-CN-XiaoxiaoNeural", "Chinese (Simplified) — Xiaoxiao"},
	{"zh-TW-HsiaoChenNeural", "Chinese (Traditional) — HsiaoChen"},
	{"zh-HK-HiuMaanNeural", "Chinese (Hong Kong) — HiuMaan"},
}

// Env vars that satisfy an API key provider, the var it saves, its prompt, success line and pre prompt hint
type ttsAPIKeyProvider struct {
	envVars                      []string
	saveVar, prompt, saved, hint string
}

var ttsAPIKeyProviders = map[string]ttsAPIKeyProvider{
	"elevenlabs": {[]string{"ELEVENLABS_API_KEY"}, "ELEVENLABS_API_KEY", "ElevenLabs API key", "ElevenLabs API key saved", ""},
	"openai": {[]string{"VOICE_TOOLS_OPENAI_KEY", "OPENAI_API_KEY"}, "VOICE_TOOLS_OPENAI_KEY", "OpenAI API key for TTS",
		"OpenAI TTS API key saved", ""},
	"minimax": {[]string{"MINIMAX_API_KEY"}, "MINIMAX_API_KEY", "MiniMax API key for TTS", "MiniMax TTS API key saved", ""},
	"mistral": {[]string{"MISTRAL_API_KEY"}, "MISTRAL_API_KEY", "Mistral API key for TTS", "Mistral TTS API key saved", ""},
	"gemini": {[]string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, "GEMINI_API_KEY", "Gemini API key for TTS", "Gemini TTS API key saved",
		"Get a free API key at https://aistudio.google.com/app/apikey"},
}

// Module, display name, requirement lines, install question and installer of a local engine
type ttsLocalProvider struct {
	module, name string
	lines        []string
	question     string
	installer    func(ctx context.Context) (bool, error)
}

var ttsLocalProviders = map[string]ttsLocalProvider{
	"neutts": {"neutts", "NeuTTS", []string{"NeuTTS requires:", "  • Python package: neutts (~50MB install + ~300MB model on first use)",
		"  • System package: espeak-ng (phonemizer)"}, "Install NeuTTS dependencies now?", installNeuTTSDeps},
	"kittentts": {"kittentts", "KittenTTS", []string{"KittenTTS is lightweight (~25-80MB, CPU-only, no API key required).",
		"Voices: Jasper, Bella, Luna, Bruno, Rosie, Hugo, Kiki, Leo"}, "Install KittenTTS now?", installKittenTTSDeps},
}

// Ensures the key of an API key provider exists, falling back to edge when none is given
func ttsAPIKeyStep(ctx context.Context, selected string) (string, error) {
	p := ttsAPIKeyProviders[selected]
	for _, name := range p.envVars {
		v, err := ttsSetup.getEnvValue(ctx, name)
		if err != nil {
			return "", err
		}
		if v != "" {
			return selected, nil
		}
	}
	ttsPrintln()
	if p.hint != "" {
		ttsSetup.printInfo(p.hint)
	}
	if apiKey := ttsSetup.prompt(p.prompt, "", true); apiKey != "" {
		if err := ttsSetup.saveEnvValue(ctx, p.saveVar, apiKey); err != nil {
			return "", err
		}
		ttsSetup.printSuccess(p.saved)
		return selected, nil
	}
	ttsSetup.printWarning("No API key provided. Falling back to Edge TTS.")
	return "edge", nil
}

// Offers a local engine install, retaining an explicit selection the platform cannot install
func ttsLocalInstallStep(ctx context.Context, selected string) (string, error) {
	p := ttsLocalProviders[selected]
	supported, err := ttsHooks.extraSupported(ctx, selected)
	if err != nil {
		return "", err
	}
	if !supported {
		ttsSetup.printWarning(p.name + " is not supported by this Python/platform. " +
			"Your provider selection is saved, but speech requires a supported engine.")
		return selected, nil
	}
	if ttsSetup.moduleInstalled(ctx, p.module) {
		ttsSetup.printSuccess(p.name + " is already installed")
		return selected, nil
	}
	ttsPrintln()
	for _, line := range p.lines {
		ttsSetup.printInfo(line)
	}
	ttsPrintln()
	if !ttsSetup.promptYesNo(p.question, true, "") {
		ttsSetup.printInfo(fmt.Sprintf("Skipping install. Set tts.provider to '%s' after installing manually.", selected))
		return "edge", nil
	}
	ok, err := p.installer(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		ttsSetup.printWarning(p.name + " installation incomplete. Falling back to Edge TTS.")
		return "edge", nil
	}
	return selected, nil
}

// Runs the xAI OAuth login, empty on success and the fallback warning otherwise
func xaiOAuthPath(ctx context.Context) (string, error) {
	if ttsHooks.xaiOAuthLogin(ctx) {
		ttsSetup.printSuccess("Logged in — xAI TTS will use these OAuth credentials")
		return "", nil
	}
	return "xAI Grok OAuth login did not complete. Falling back to Edge TTS.", nil
}

// Saves a pasted xAI API key, empty on success and the fallback warning when none was given
func xaiAPIKeyPath(ctx context.Context) (string, error) {
	if apiKey := ttsSetup.prompt("xAI API key for TTS", "", true); apiKey != "" {
		if err := ttsSetup.saveEnvValue(ctx, "XAI_API_KEY", apiKey); err != nil {
			return "", err
		}
		ttsSetup.printSuccess("xAI TTS API key saved")
		return "", nil
	}
	return "No xAI API key provided for TTS. Configure XAI_API_KEY via icebath setup model " +
		"or " + home.Display(home.Dir(ctx)) + "/.env to use xAI TTS. Falling back to Edge TTS.", nil
}

// Resolves xAI TTS auth preferring an existing key over stored OAuth tokens, then sets an optional voice id
func ttsXAIStep(ctx context.Context, cfg config.Tree) (string, error) {
	key, err := ttsSetup.getEnvValue(ctx, "XAI_API_KEY")
	if err != nil {
		return "", err
	}
	switch {
	case key != "":
		ttsSetup.printSuccess("xAI TTS will use your existing XAI_API_KEY (preferred over xAI Grok OAuth)")
	case ttsHooks.xaiOAuthLoggedIn(ctx):
		ttsSetup.printSuccess("xAI TTS will use your xAI Grok OAuth (SuperGrok / Premium+) credentials")
	default:
		ttsPrintln()
		idx := ttsSetup.promptChoice("How do you want xAI TTS to authenticate?",
			[]string{"Sign in with xAI Grok OAuth (SuperGrok / Premium+) — browser login",
				"Paste an xAI API key (console.x.ai)", "Skip → fallback to Edge TTS"}, 0, "")
		var fallback string
		switch idx {
		case 0:
			fallback, err = xaiOAuthPath(ctx)
		case 1:
			fallback, err = xaiAPIKeyPath(ctx)
		default:
			fallback = "xAI TTS skipped. Falling back to Edge TTS."
		}
		if err != nil {
			return "", err
		}
		if fallback != "" {
			ttsSetup.printWarning(fallback)
			return "edge", nil
		}
	}
	ttsPrintln()
	voiceID := pystr.Strip(ttsSetup.prompt("xAI voice_id (Enter for 'eve', or paste a custom voice ID)", "", false))
	if voiceID != "" {
		tts, err := setdefaultDict(cfg, "tts", "setdefault")
		if err != nil {
			return "", err
		}
		xai, err := setdefaultDict(tts, "xai", "__setitem__")
		if err != nil {
			return "", err
		}
		xai["voice_id"] = voiceID
		ttsSetup.printSuccess("xAI voice_id set to: " + voiceID)
	}
	return "xai", nil
}

// Dict of a config node, the node itself when it is a mapping
func asDict(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case config.Tree:
		return map[string]any(m), true
	}
	return nil, false
}

// AttributeError of calling a dict method on a node that is not a dict, item assignment raising TypeError
func notDictError(v any, method string) error {
	if method == "__setitem__" {
		if _, isList := v.([]any); isList {
			return errors.New("TypeError: list indices must be integers or slices, not str")
		}
		return fmt.Errorf("TypeError: '%s' object does not support item assignment", pyrepr.TypeName(v))
	}
	return fmt.Errorf("AttributeError: '%s' object has no attribute '%s'", pyrepr.TypeName(v), method)
}

// The dict.get(key, {}) of m, failing on a stored value that is not a dict when its get runs next
func getDict(m map[string]any, key string) (map[string]any, error) {
	v, ok := m[key]
	if !ok {
		return map[string]any{}, nil
	}
	d, ok := asDict(v)
	if !ok {
		return nil, notDictError(v, "get")
	}
	return d, nil
}

// The dict.setdefault(key, {}) of m, a stored non dict failing with the error its next method raises
func setdefaultDict(m map[string]any, key, next string) (map[string]any, error) {
	v, ok := m[key]
	if !ok {
		d := map[string]any{}
		m[key] = d
		return d, nil
	}
	d, ok := asDict(v)
	if !ok {
		return nil, notDictError(v, next)
	}
	return d, nil
}

// TypeError of using a mapping or a list as a dict key or set member
func unhashableError(v any) error {
	switch v.(type) {
	case map[string]any, config.Tree, []any:
		return fmt.Errorf("TypeError: unhashable type: '%s'", pyrepr.TypeName(v))
	}
	return nil
}

// Python sequence index of a menu answer, negative counting from the end, false past either end
func pyIndex(idx, n int) (int, bool) {
	if idx < 0 {
		idx += n
	}
	return idx, idx >= 0 && idx < n
}

// Interactive TTS provider selection with the key prompts, local engine installs and the Edge voice picker
func setupTTSProvider(ctx context.Context, cfg config.Tree) error {
	ttsCfg, err := getDict(cfg, "tts")
	if err != nil {
		return err
	}
	var currentProvider any = "edge"
	if v, ok := ttsCfg["provider"]; ok {
		currentProvider = v
	}
	if err := unhashableError(currentProvider); err != nil {
		return err
	}
	currentLabel := pyrepr.Str(currentProvider)
	if s, ok := currentProvider.(string); ok {
		if label, known := ttsProviderLabels[s]; known {
			currentLabel = label
		}
	}
	ttsPrintln()
	ttsSetup.printHeader("Text-to-Speech Provider (optional)")
	ttsSetup.info("Current: "+currentLabel, nil)
	options := slices.Clone(ttsProviderChoices)
	if ttsHooks.managedNousToolsEnabled(ctx, cfg) && ttsHooks.nousAuthPresent(ctx, cfg) {
		options = slices.Insert(options, 0, ttsChoice{"nous-openai", "Nous Subscription (managed OpenAI TTS, billed to your subscription)"})
	}
	choices := make([]string, 0, len(options)+1)
	for _, o := range options {
		choices = append(choices, o.label)
	}
	choices = append(choices, "Keep current ("+currentLabel+")")
	keepCurrent := len(choices) - 1
	idx := ttsSetup.promptChoice("Select TTS provider:", choices, keepCurrent, "")
	if idx == keepCurrent {
		return nil
	}
	at, ok := pyIndex(idx, len(options))
	if !ok {
		return errors.New("IndexError: list index out of range")
	}
	selected := options[at].key
	_, local := ttsLocalProviders[selected]
	_, keyed := ttsAPIKeyProviders[selected]
	switch {
	case selected == "nous-openai":
		selected = "openai"
		ttsSetup.printInfo("OpenAI TTS will use the managed Nous gateway and bill to your subscription.")
		direct, err := ttsSetup.getEnvValue(ctx, "VOICE_TOOLS_OPENAI_KEY")
		if err != nil {
			return err
		}
		if direct == "" {
			if direct, err = ttsSetup.getEnvValue(ctx, "OPENAI_API_KEY"); err != nil {
				return err
			}
		}
		if direct != "" {
			ttsSetup.printWarning("Direct OpenAI credentials are still configured and may take precedence " +
				"until removed from ~/.icebath/.env.")
		}
	case local:
		if selected, err = ttsLocalInstallStep(ctx, selected); err != nil {
			return err
		}
	case keyed:
		if selected, err = ttsAPIKeyStep(ctx, selected); err != nil {
			return err
		}
	case selected == "xai":
		if selected, err = ttsXAIStep(ctx, cfg); err != nil {
			return err
		}
	}
	if selected == "edge" {
		if err := pickEdgeVoice(cfg); err != nil {
			return err
		}
	}
	tts, err := setdefaultDict(cfg, "tts", "__setitem__")
	if err != nil {
		return err
	}
	tts["provider"] = selected
	if err := ttsSetup.saveConfig(ctx, cfg); err != nil {
		return err
	}
	label, known := ttsProviderLabels[selected]
	if !known {
		label = selected
	}
	ttsSetup.printSuccess("TTS provider set to: " + label)
	return nil
}

// Edge voice menu keeping an unlisted current voice first and accepting a typed voice id
func pickEdgeVoice(cfg config.Tree) error {
	tts, err := setdefaultDict(cfg, "tts", "setdefault")
	if err != nil {
		return err
	}
	edge, err := setdefaultDict(tts, "edge", "get")
	if err != nil {
		return err
	}
	var currentVoice any = "en-US-AriaNeural"
	if v, ok := edge["voice"]; ok {
		currentVoice = v
	}
	if err := unhashableError(currentVoice); err != nil {
		return err
	}
	voices := slices.Clone(edgeVoiceChoices)
	currentIdx := -1
	for i, v := range voices {
		if v.voice == currentVoice {
			currentIdx = i
			break
		}
	}
	if currentIdx < 0 {
		voices = slices.Insert(voices, 0, edgeVoice{currentVoice, "Current voice — " + pyrepr.Str(currentVoice)})
		currentIdx = 0
	}
	labels := make([]string, 0, len(voices)+1)
	for _, v := range voices {
		labels = append(labels, v.label)
	}
	labels = append(labels, "Other voice (enter voice ID)")
	hint := "Choose a voice matching the language you want to speak."
	ttsSetup.printInfo(hint)
	voiceIdx := ttsSetup.promptChoice("Select Edge TTS voice:", labels, currentIdx, hint)
	if voiceIdx == len(voices) {
		def := ""
		if pyrepr.Truthy(currentVoice) {
			def = pyrepr.Str(currentVoice)
		}
		answer := ttsSetup.prompt("Edge TTS voice ID", def, false)
		if _, isStr := currentVoice.(string); !isStr && def != "" && answer == def {
			return fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", pyrepr.TypeName(currentVoice))
		}
		if voice := pystr.Strip(answer); voice != "" {
			edge["voice"] = voice
		} else {
			edge["voice"] = currentVoice
		}
		return nil
	}
	at, ok := pyIndex(voiceIdx, len(voices))
	if !ok {
		return errors.New("IndexError: list index out of range")
	}
	edge["voice"] = voices[at].voice
	return nil
}

// Standalone text to speech setup run by the setup wizard and icebath setup tts
func SetupTTS(ctx context.Context, cfg config.Tree) error {
	return setupTTSProvider(ctx, cfg)
}
