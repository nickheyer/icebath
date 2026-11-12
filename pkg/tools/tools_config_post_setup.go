package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Seams over the pm client, host facts and the dependency environment the post setup hooks read
var (
	postSetupEnsure = func(ctx context.Context, name string, opts pm.EnsureOptions) error {
		_, err := pm.Ensure(ctx, name, opts)
		return err
	}
	postSetupSyncVenv           = pm.SyncVenv
	postSetupModuleImportable   = pm.AnchorImportable
	postSetupIsTermux           = hostinfo.IsTermux
	postSetupGOOS               = runtime.GOOS
	postSetupWhich              = func(cmd string) string { p, _ := pyshutil.Which(cmd, nil); return p }
	postSetupInterpreter        = selectedEnvironmentPython
	postSetupFileExists         = func(path string) bool { _, err := os.Stat(path); return err == nil }
	errCodexTokensMissing       = errors.New("'tokens'")
	postSetupRuntimeOrOSClasses = map[string]bool{
		"RuntimeError": true, "NotImplementedError": true, "RecursionError": true, "PythonFinalizationError": true,
		"InstallError": true, "ResolutionConflict": true, "BuildFailure": true, "NoCommittedEnvironment": true,
		"PinMismatch": true, "DownloadError": true, "_RangeError": true, "HashError": true, "DownloadPaused": true,
		"DownloadTransportError": true, "FeatureProbeError": true, "OSError": true, "IOError": true,
		"EnvironmentError": true, "FileNotFoundError": true, "FileExistsError": true, "PermissionError": true,
		"IsADirectoryError": true, "NotADirectoryError": true, "TimeoutError": true, "InterruptedError": true,
		"BlockingIOError": true, "ChildProcessError": true, "ProcessLookupError": true, "ConnectionError": true,
		"BrokenPipeError": true, "ConnectionAbortedError": true, "ConnectionRefusedError": true, "ConnectionResetError": true,
	}
)

// Interpreter of the selected dependency environment, the sys.executable the application runs under
func selectedEnvironmentPython(ctx context.Context) (string, error) {
	root := pm.RepoRoot()
	venv, err := pm.SelectedVenv(ctx, root)
	if err != nil {
		return "", err
	}
	command, err := pm.VenvCommand(ctx, root, venv, nil)
	if err != nil {
		return "", err
	}
	return command[0], nil
}

// Whether a pm failure is the RuntimeError or OSError family the Chromium hook reports instead of raising
func pmRuntimeOrOSError(err error) bool {
	var install *pm.InstallError
	var download *pm.DownloadError
	var probe *pm.FeatureProbeError
	if errors.As(err, &install) || errors.As(err, &download) || errors.As(err, &probe) || pypath.IsOSError(err) {
		return true
	}
	var py *pm.PyError
	if errors.As(err, &py) {
		return postSetupRuntimeOrOSClasses[py.Class]
	}
	var tar *pm.TarReadError
	if errors.As(err, &tar) {
		return postSetupRuntimeOrOSClasses[tar.Class]
	}
	return false
}

// Local Whisper model sizes with the default marked
func localSTTModelSummary() string {
	catalogFn, defFn := ToolsConfigHooks.STTModelCatalog, ToolsConfigHooks.STTDefaultLocalModel
	requireToolsConfigHook("STTModelCatalog", catalogFn != nil)
	requireToolsConfigHook("STTDefaultLocalModel", defFn != nil)
	models, ok := catalogFn("local")
	if !ok {
		panic("KeyError: 'local'")
	}
	parts := make([]string, len(models))
	for i, m := range models {
		parts[i] = m
		if m == defFn() {
			parts[i] = m + " (default)"
		}
	}
	return strings.Join(parts, ", ")
}

// Prints each line as a four space indented info row
func (s *ToolsSetup) infoLines(lines ...string) {
	for _, line := range lines {
		s.Con.PrintInfo("    " + line)
	}
}

// Browser harness command of browser_exec, nil when it is missing
func (s *ToolsSetup) findBrowserUseCLI() []string {
	fn := ToolsConfigHooks.FindBrowserUseCLI
	requireToolsConfigHook("FindBrowserUseCLI", fn != nil)
	return fn(s.toolEnv())
}

// Seam over the harness check every browser row hook runs, the _ensure_browser_use_cli patch target
var postSetupEnsureBrowserUseCLI = (*ToolsSetup).ensureBrowserUseCLI

// Confirms the browser harness browser_exec runs is present, a miss meaning the environment needs a re-sync
func (s *ToolsSetup) ensureBrowserUseCLI() {
	if s.findBrowserUseCLI() != nil {
		s.Con.PrintSuccess("    browser_exec ready (browser-harness, bundled with Icebath)")
		return
	}
	s.Con.PrintWarning("    browser-harness is missing from Icebath's Python environment")
	s.Con.PrintInfo("    Re-sync it with: icebath update")
}

// Lightpanda row check, the harness then the lightpanda binary with its install hint when missing
func (s *ToolsSetup) postSetupLightpanda() error {
	postSetupEnsureBrowserUseCLI(s)
	find, hint := ToolsConfigHooks.LightpandaBinary, ToolsConfigHooks.LightpandaInstallHint
	requireToolsConfigHook("LightpandaBinary", find != nil)
	requireToolsConfigHook("LightpandaInstallHint", hint != nil)
	if bin := find(s.toolEnv()); bin != "" {
		s.Con.PrintSuccess("    Lightpanda found: " + bin)
		return nil
	}
	s.Con.PrintWarning("    lightpanda binary not found on PATH, ~/.lightpanda or ~/.local/bin")
	s.Con.PrintInfo("    " + hint())
	if postSetupGOOS == "windows" {
		s.Con.PrintInfo("    Lightpanda has no native Windows build; run Icebath under WSL2.")
	}
	return nil
}

// Local Browser row setup, pm's Chromium installed unless Termux or the baked image own the payload
func (s *ToolsSetup) postSetupChromium() error {
	chromium, baked, missingHint := ToolsConfigHooks.ChromiumExecutable, ToolsConfigHooks.BakedChromiumFile, ToolsConfigHooks.ChromiumMissingHint
	requireToolsConfigHook("ChromiumExecutable", chromium != nil)
	requireToolsConfigHook("BakedChromiumFile", baked != nil)
	requireToolsConfigHook("ChromiumMissingHint", missingHint != nil)
	env := s.toolEnv()
	postSetupEnsureBrowserUseCLI(s)
	if chromium(env) == "" && !(postSetupIsTermux() || postSetupFileExists(baked())) {
		if err := postSetupEnsure(s.ctx(), "chromium", pm.EnsureOptions{Explicit: true}); err != nil {
			if !pmRuntimeOrOSError(err) {
				return err
			}
			s.Con.PrintWarning("    Chromium install failed: " + pyExcText(err))
			s.infoLines("Retry with: icebath tools post-setup chromium")
			return nil
		}
	}
	found := chromium(env)
	if found == "" {
		s.Con.PrintWarning("    " + missingHint(env))
		return nil
	}
	s.Con.PrintSuccess("    Chromium ready: " + found)
	if postSetupGOOS == "linux" && !postSetupIsTermux() {
		s.infoLines("Chromium also needs system libraries supplied by your distribution.")
		if postSetupWhich("apt-get") != "" && s.moduleInstalled("playwright") {
			python, err := postSetupInterpreter(s.ctx())
			if err != nil {
				return err
			}
			s.infoLines("Install missing system libraries with: " + pyshlex.Join([]string{python, "-m", "playwright", "install-deps", "chromium"}))
		} else {
			s.infoLines("System dependency installation guide:",
				"  https://playwright.dev/python/docs/browsers#install-system-dependencies")
		}
	}
	return nil
}

// One Python provider hook, its import anchor, pyproject extra, label, progress line and closing info lines
type pythonPostSetupHook struct {
	module, extra, label, installing string
	onInstall                        func() []string
	always                           []string
}

// Hook spec keyed by the UI provider identifier, the extra naming a pyproject optional dependency
func pythonHook(module, extra, label, installing string, onInstall func() []string, always ...string) pythonPostSetupHook {
	if onInstall == nil {
		onInstall = func() []string { return nil }
	}
	return pythonPostSetupHook{module: module, extra: extra, label: label, installing: installing, onInstall: onInstall, always: always}
}

func staticLines(lines ...string) func() []string { return func() []string { return lines } }

var pythonPostSetupHookKeys = []string{"faster_whisper", "kittentts", "piper"}

var pythonPostSetupHooks = map[string]pythonPostSetupHook{
	"faster_whisper": pythonHook("faster_whisper", "stt-whisper", "faster-whisper",
		"Installing faster-whisper (model ~150MB downloads on first use)...", func() []string {
			return []string{"Model sizes: " + localSTTModelSummary(), "Change via stt.local.model in config.yaml"}
		}),
	"kittentts": pythonHook("kittentts", "kittentts", "kittentts", "Installing kittentts (~25-80MB model, CPU-only)...",
		staticLines("Voices: Jasper, Bella, Luna, Bruno, Rosie, Hugo, Kiki, Leo",
			"Models: KittenML/kitten-tts-nano-0.8-int8 (25MB), micro (41MB), mini (80MB)")),
	"piper": pythonHook("piper", "piper", "piper-tts", "Installing piper-tts (~14MB wheel, voices downloaded on first use)...", nil,
		"Default voice: en_US-lessac-medium (downloaded on first TTS call)",
		"Full voice list: https://github.com/OHF-Voice/piper1-gpl/blob/main/docs/VOICES.md",
		"Switch voices by setting tts.piper.voice in config.yaml"),
}

// Enables one Python provider through the application dependency transaction
func (s *ToolsSetup) postSetupPython(spec pythonPostSetupHook) error {
	s.Con.PrintInfo("    " + spec.installing)
	if err := postSetupSyncVenv(s.ctx(), pm.SyncVenvOptions{Extras: []string{spec.extra}, Explicit: true}); err != nil {
		if !setupSyncFailureHandled(err) {
			return err
		}
		s.Con.PrintWarning("    " + spec.label + " install failed: " + pyExcText(err))
		s.infoLines("Retry with: icebath tools")
		return nil
	}
	s.Con.PrintSuccess("    " + spec.label + " dependencies ready. Restart Icebath to use them.")
	s.infoLines(append(spec.onInstall(), spec.always...)...)
	return nil
}

// Shared xAI credential bootstrap, a SuperGrok OAuth login preferred over a pasted XAI_API_KEY
func (s *ToolsSetup) postSetupXAIGrok() error {
	oauthLoggedIn := false
	if status := ToolsConfigHooks.XAIOAuthStatus; status != nil {
		func() {
			defer func() { _ = recover() }()
			oauthLoggedIn = pyTruthy(status(s.ctx(), s.home())["logged_in"])
		}()
	}
	if oauthLoggedIn {
		s.Con.PrintSuccess("    xAI will use your xAI Grok OAuth (SuperGrok / Premium+) credentials")
		return nil
	}
	key, err := s.getEnvValue("XAI_API_KEY")
	if err != nil {
		return err
	}
	if key != "" {
		s.Con.PrintSuccess("    xAI will use your existing XAI_API_KEY")
		return nil
	}
	s.Con.PrintInfo("    xAI needs credentials. Choose one:")
	choose, ask, login := ToolsConfigHooks.SetupPromptChoice, ToolsConfigHooks.SetupPrompt, ToolsConfigHooks.XAIOAuthLoginFromSetup
	if choose == nil || ask == nil || login == nil {
		s.Con.PrintWarning("    Could not load setup helpers: " + errSetupHelpersUnwired.Error())
		s.infoLines("Run later: icebath auth add xai-oauth   (or set XAI_API_KEY)")
		return nil
	}
	idx := choose("    How do you want xAI to authenticate?", []string{
		"Sign in with xAI Grok OAuth (SuperGrok / Premium+) — browser login",
		"Paste an xAI API key (console.x.ai)",
		"Skip — configure later via `icebath auth add xai-oauth`",
	}, 0)
	switch idx {
	case 0:
		if login(s.ctx(), s.home()) {
			s.Con.PrintSuccess("    Logged in — xAI will use these OAuth credentials")
		} else {
			s.Con.PrintWarning("    xAI Grok OAuth login did not complete. Run later: icebath auth add xai-oauth")
		}
	case 1:
		apiKey := ask("    xAI API key", "", true)
		if apiKey == "" {
			s.Con.PrintWarning("    No API key provided. Run later: icebath auth add xai-oauth")
			return nil
		}
		if err := s.saveEnvValue("XAI_API_KEY", apiKey); err != nil {
			return err
		}
		s.Con.PrintSuccess("    XAI_API_KEY saved")
	default:
		s.Con.PrintInfo("    xAI will remain inactive until credentials are configured.")
	}
	return nil
}

// Offline check for Codex or ChatGPT OAuth credentials in the auth store and pool
func (s *ToolsSetup) codexCredentialsPresent() (present bool) {
	status := ToolsConfigHooks.CodexAuthStatus
	if status == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			present = false
		}
	}()
	return pyTruthy(status(s.ctx(), s.home())["logged_in"])
}

// Codex device code login for keyless Codex rows, saving tokens without switching the chat provider
func (s *ToolsSetup) postSetupOpenAICodex() error {
	if s.codexCredentialsPresent() {
		s.Con.PrintSuccess("    Image generation will use your existing Codex/ChatGPT OAuth credentials")
		return nil
	}
	relogin := "icebath auth add openai-codex"
	s.Con.PrintInfo("    OpenAI (Codex auth) needs credentials.")
	login, save := ToolsConfigHooks.CodexDeviceCodeLogin, ToolsConfigHooks.SaveCodexTokens
	noninteractive, choose := ToolsConfigHooks.SetupIsNoninteractive, ToolsConfigHooks.SetupPromptChoice
	if login == nil || save == nil || noninteractive == nil || choose == nil {
		s.Con.PrintWarning("    Could not load setup helpers: " + errSetupHelpersUnwired.Error())
		s.infoLines("Run later: " + relogin)
		return nil
	}
	if noninteractive() {
		s.infoLines("No terminal to sign in from. Run: " + relogin)
		return nil
	}
	idx := choose("    How do you want to sign in?", []string{
		"Sign in with ChatGPT/Codex OAuth — browser login",
		"Skip — configure later via `" + relogin + "`",
	}, 0)
	if idx != 0 {
		s.Con.PrintInfo("    Codex image generation will remain inactive until you sign in.")
		return nil
	}
	if err := s.codexSignIn(login, save); err != nil {
		s.Con.PrintWarning("    Codex sign-in did not complete: " + pyExcText(err) + ". Run later: " + relogin)
		return nil
	}
	s.Con.PrintSuccess("    Logged in — image generation will use these Codex OAuth credentials")
	return nil
}

// Device code login then token save, any failure or interrupt returned as the error
func (s *ToolsSetup) codexSignIn(login func(context.Context, string, io.Writer) (map[string]any, error),
	save func(context.Context, string, map[string]any, string, bool) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	creds, err := login(s.ctx(), s.home(), s.Con.Out)
	if err != nil {
		return err
	}
	raw, ok := creds["tokens"]
	if !ok {
		return errCodexTokensMissing
	}
	tokens, _ := pyDictOf(raw)
	lastRefresh := ""
	if v, ok := creds["last_refresh"]; ok && v != nil {
		lastRefresh = pyrepr.Str(v)
	}
	return save(s.ctx(), s.home(), tokens, lastRefresh, false)
}

func (s *ToolsSetup) xaiCredentialsReady() bool { return xaiCredentialsPresent(s.ctx()) }

// Credential bootstrap keys and their credentials present check, these rows having no install side effect
var postSetupAuthReady = map[string]func(*ToolsSetup) bool{
	"xai_grok":     (*ToolsSetup).xaiCredentialsReady,
	"openai_codex": (*ToolsSetup).codexCredentialsPresent,
}

// Post setup key to its hook, unknown keys a silent no op since callers validate against the visible rows
var postSetupHooks = buildPostSetupHooks()

func buildPostSetupHooks() map[string]func(*ToolsSetup) error {
	hooks := map[string]func(*ToolsSetup) error{
		"lightpanda":  (*ToolsSetup).postSetupLightpanda,
		"chromium":    (*ToolsSetup).postSetupChromium,
		"browserbase": func(s *ToolsSetup) error { postSetupEnsureBrowserUseCLI(s); return nil },
		"cua_driver": func(s *ToolsSetup) error {
			install := ToolsConfigHooks.InstallCuaDriver
			requireToolsConfigHook("InstallCuaDriver", install != nil)
			_, err := install(s.ctx(), s.toolEnv(), s.Con)
			return err
		},
		"xai_grok":     (*ToolsSetup).postSetupXAIGrok,
		"openai_codex": (*ToolsSetup).postSetupOpenAICodex,
	}
	for _, key := range pythonPostSetupHookKeys {
		spec := pythonPostSetupHooks[key]
		hooks[key] = func(s *ToolsSetup) error { return s.postSetupPython(spec) }
	}
	return hooks
}

// Runs the post setup hook of a provider row that needs extra installation steps
func (s *ToolsSetup) runPostSetup(postSetupKey string) error {
	hook, ok := postSetupHooks[postSetupKey]
	if !ok {
		return nil
	}
	return hook(s)
}

// Post setup keys declared by any visible provider, the allowlist the post-setup command validates against
func (s *ToolsSetup) ValidPostSetupKeys() map[string]bool {
	keys := map[string]bool{}
	for _, catKey := range ToolCategories.Keys {
		cat := catalogCategory(catKey)
		if cat == nil {
			continue
		}
		for _, prov := range catProviders(cat) {
			if ps := rowGet(prov, "post_setup"); pyTruthy(ps) {
				keys[pyrepr.Str(ps)] = true
			}
		}
	}
	builders := []func(*ToolsSetup) []*pyjson.Object{(*ToolsSetup).pluginWebSearchProviders, (*ToolsSetup).pluginImageGenProviders,
		(*ToolsSetup).pluginVideoGenProviders, (*ToolsSetup).pluginBrowserProviders}
	for _, builder := range builders {
		func() {
			defer func() { _ = recover() }()
			for _, prov := range builder(s) {
				if ps := rowGet(prov, "post_setup"); pyTruthy(ps) {
					keys[pyrepr.Str(ps)] = true
				}
			}
		}()
	}
	return keys
}

// Hook run of the post-setup command, a panic returned as the failure
func (s *ToolsSetup) runPostSetupCaught(key string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return s.runPostSetup(key)
}

// Non interactive post-setup runner, 0 ok, 1 hook failure, 2 missing or unknown key
func (s *ToolsSetup) RunPostSetupCommand(ns map[string]any) int {
	key := ns["post_setup_key"]
	if !pyTruthy(key) {
		s.Con.PrintError("Usage: icebath tools post-setup <key>")
		return 2
	}
	valid := s.ValidPostSetupKeys()
	keyText, isStr := key.(string)
	if !isStr || !valid[keyText] {
		names := make([]string, 0, len(valid))
		for k := range valid {
			names = append(names, k)
		}
		sort.Strings(names)
		joined := strings.Join(names, ", ")
		if joined == "" {
			joined = "(none)"
		}
		s.Con.PrintError("Unknown post-setup key: " + pyrepr.Repr(key) + ". Valid keys: " + joined)
		return 2
	}
	s.Con.PrintInfo("Running post-setup hook: " + keyText)
	if err := s.runPostSetupCaught(keyText); err != nil {
		s.Con.PrintError("Post-setup failed: " + pyExcText(err))
		return 1
	}
	s.Con.PrintSuccess("Post-setup '" + keyText + "' complete")
	return 0
}

func (s *ToolsSetup) cuaDriverInstallReady() (bool, error) {
	fn := ToolsConfigHooks.CuaDriverInstallReady
	requireToolsConfigHook("CuaDriverInstallReady", fn != nil)
	return fn(s.toolEnv())
}

// Post setup keys whose install side effect has a local bounded check, forcing setup while it is unmet
var postSetupInstalled = map[string]func(*ToolsSetup) (bool, error){
	"cua_driver": (*ToolsSetup).cuaDriverInstallReady,
}

// Whether a post setup install side effect is satisfied, true when no check is registered or the check fails
func (s *ToolsSetup) PostSetupAlreadyInstalled(postSetupKey string) (installed bool) {
	predicate, ok := postSetupInstalled[postSetupKey]
	if !ok {
		return true
	}
	defer func() {
		if recover() != nil {
			installed = true
		}
	}()
	ok, err := predicate(s)
	return err != nil || ok
}

// Whether the dependency environment can import a module, false when the probe fails
func (s *ToolsSetup) moduleInstalled(moduleName string) (found bool) {
	defer func() {
		if recover() != nil {
			found = false
		}
	}()
	return postSetupModuleImportable(s.ctx(), moduleName)
}

// Everything the chromium hook provides is present, the harness and a Chromium
func (s *ToolsSetup) chromiumInstalled() (bool, error) {
	chromium := ToolsConfigHooks.ChromiumExecutable
	requireToolsConfigHook("ChromiumExecutable", chromium != nil)
	if ok, _ := s.browserHarnessInstalled(); !ok {
		return false, nil
	}
	return chromium(s.toolEnv()) != "", nil
}

// Installed check of the browserbase hook, the browser harness being the whole local contract of cloud rows
func (s *ToolsSetup) browserHarnessInstalled() (bool, error) {
	return s.findBrowserUseCLI() != nil, nil
}

// Whether a lightpanda binary is on PATH or in a known install dir
func (s *ToolsSetup) lightpandaInstalled() (found bool, err error) {
	find := ToolsConfigHooks.LightpandaBinary
	if find == nil {
		return false, nil
	}
	defer func() {
		if recover() != nil {
			found, err = false, nil
		}
	}()
	return find(s.toolEnv()) != "", nil
}

// Installed checks of the keyless post setup rows, ready against needs_setup in the picker
var postSetupReady = buildPostSetupReady()

func buildPostSetupReady() map[string]func(*ToolsSetup) (bool, error) {
	ready := map[string]func(*ToolsSetup) (bool, error){
		"chromium":    (*ToolsSetup).chromiumInstalled,
		"browserbase": (*ToolsSetup).browserHarnessInstalled,
		"lightpanda":  (*ToolsSetup).lightpandaInstalled,
		"cua_driver":  (*ToolsSetup).cuaDriverInstallReady,
	}
	for _, key := range pythonPostSetupHookKeys {
		module := pythonPostSetupHooks[key].module
		ready[key] = func(s *ToolsSetup) (bool, error) { return s.moduleInstalled(module), nil }
	}
	return ready
}
