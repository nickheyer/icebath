package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Module probe of the managed interpreter, find_spec semantics with a failed probe reading as absent
var safeFindSpec = func(module string) bool { return pm.AnchorImportable(context.Background(), module) }

// Import time module flag, probed once per process on first read
func newFindSpecFlag(module string) func() bool {
	return sync.OnceValue(func() bool { return safeFindSpec(module) })
}

var (
	hasFasterWhisper = newFindSpecFlag("faster_whisper")
	hasOpenAI        = newFindSpecFlag("openai")
	hasMistral       = newFindSpecFlag("mistralai")
	hasPilk          = newFindSpecFlag("pilk")
)

// Serves the faster_whisper probe to the provider tool gateway status
func registerTranscriptionAPI() {
	provider.FasterWhisperImportable = func() bool { return hasFasterWhisper() }
}

// STT API key through the shared voice key resolver, resolved per call
var resolveSTTProviderKey = func(ctx context.Context, envVar, providerID string) string {
	return tools.ResolveProviderSecret(nil, envVar, providerID, "", nil)
}

// Reference counted cached model so an unload never closes a model a transcription still holds
type whisperHandle struct {
	model localWhisperModel
	mu    sync.Mutex
	refs  int
}

func (h *whisperHandle) acquire() {
	h.mu.Lock()
	h.refs++
	h.mu.Unlock()
}

// Drops one reference and closes the model with the last one
func (h *whisperHandle) release() {
	h.mu.Lock()
	h.refs--
	last := h.refs == 0
	h.mu.Unlock()
	if last {
		h.model.Close()
	}
}

var (
	localModelMu   sync.Mutex
	localModel     *whisperHandle
	localModelName string

	idleMgmtMu            sync.Mutex
	idleUnloadAlive       bool
	idleUnloadStop        = make(chan struct{})
	idleUnloadPolicyCtx   = context.Background()
	idleUnloadFallback    int
	lastTranscriptionNano atomic.Int64
	sttMonotonicBase      = time.Now()
	sttIdleCheckInterval  = 30 * time.Second
	sttBuiltinHandlers    map[string]sttHandler
)

// Built in provider handlers looked up at dispatch time so tests may replace one
func init() {
	sttBuiltinHandlers = map[string]sttHandler{"local": transcribeLocal, "local_command": transcribeLocalCommand, "groq": transcribeGroq,
		"openai": transcribeOpenAIHandler, "mistral": transcribeMistral, "xai": transcribeXAI, "elevenlabs": transcribeElevenLabs,
		"deepinfra": transcribeDeepInfra}
}

// Whether stt.enabled is truthy, loading the config when none is given
func isSTTEnabled(ctx context.Context, sttConfig map[string]any) bool {
	if sttConfig == nil {
		sttConfig = loadSTTConfig(ctx)
	}
	return isTruthyValue(dictGetDefault(sttConfig, "enabled", true), true)
}

// First non blank language of stt.<provider> keys, stt.language, then the env, empty for auto detect
func resolveSTTLanguage(ctx context.Context, providerKey string, sttConfig map[string]any, extraKeys ...string) string {
	if sttConfig == nil {
		sttConfig = loadSTTConfig(ctx)
	}
	providerCfg := getProviderSection(sttConfig, providerKey)
	candidates := []any{providerCfg["language"]}
	for _, k := range extraKeys {
		candidates = append(candidates, providerCfg[k])
	}
	candidates = append(candidates, sttConfig["language"])
	if v, ok := os.LookupEnv(localSTTLanguageEnv); ok {
		candidates = append(candidates, v)
	}
	for _, c := range candidates {
		if s, ok := c.(string); ok && pystr.Strip(s) != "" {
			return pystr.Strip(s)
		}
	}
	return ""
}

// Empty when OpenAI audio has usable credentials, else the selection specific reason
func openAIAudioUnavailableReason(ctx context.Context) (string, error) {
	_, _, err := resolveSTTOpenAIAudioClientConfig(ctx)
	if ve, ok := err.(*sttValueError); ok {
		return ve.msg, nil
	}
	return "", err
}

// Whether a provider is exempt from the remote upload cap
func isLocalSTTProvider(providerName string) bool {
	p := pystr.Strip(strings.ToLower(providerName))
	return p == "local" || p == "local_command"
}

// Availability probe of an optional SDK flag and a resolvable API key
func hasKeyProbe(envVar, providerID string, needsOpenAI, needsMistral bool) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		sdkOK := (!needsOpenAI || hasOpenAI()) && (!needsMistral || hasMistral())
		return sdkOK && resolveSTTProviderKey(ctx, envVar, providerID) != "", nil
	}
}

// xAI credential probe that never fails
func hasXAISTTCredentialsQuietly(ctx context.Context) (ok bool, err error) {
	if perr := pluginCall(func() error { ok = hasXAISTTCredentials(ctx); return nil }); perr != nil {
		return false, nil
	}
	return ok, nil
}

// Explicit openai selection resolved with the real unavailability reason logged
func resolveExplicitOpenAI(ctx context.Context) (string, error) {
	if !hasOpenAI() {
		sttLog().Warn("STT provider 'openai' configured but no API key available")
		return "none", nil
	}
	reason, err := openAIAudioUnavailableReason(ctx)
	if err != nil {
		return "", err
	}
	if reason == "" {
		return "openai", nil
	}
	sttLog().Warn("STT provider 'openai' configured but unavailable: " + reason)
	return "none", nil
}

// faster-whisper first, then a local whisper CLI, empty when no local backend is installed
func detectLocalBackend(ctx context.Context) string {
	if hasFasterWhisper() {
		return "local"
	}
	if hasLocalCommand(ctx) {
		return "local_command"
	}
	return ""
}

// Explicit local selection resolved to an installed backend or none
func resolveExplicitLocal(ctx context.Context) (string, error) {
	backend := detectLocalBackend(ctx)
	if backend == "" {
		sttLog().Warn("STT provider 'local' configured but unavailable (install faster-whisper or set " + localSTTCommandEnv + ")")
		return "none", nil
	}
	return backend, nil
}

// Explicit local_command selection, falling back to faster-whisper
func resolveExplicitLocalCommand(ctx context.Context) (string, error) {
	if hasLocalCommand(ctx) {
		return "local_command", nil
	}
	if hasFasterWhisper() {
		sttLog().Info("Local STT command unavailable, using local faster-whisper")
		return "local", nil
	}
	sttLog().Warn("STT provider 'local_command' configured but unavailable")
	return "none", nil
}

// Explicit probe, auto detect probe, explicit warning and auto detect log of one cloud provider
type cloudSTTSpec struct {
	name     string
	explicit func(context.Context) (bool, error)
	auto     func(context.Context) (bool, error)
	warning  string
	message  string
}

var (
	cloudSTTProviderSpecs []cloudSTTSpec
	explicitSTTResolvers  map[string]func(context.Context) (string, error)
)

// Cloud providers in auto detect priority order with deepinfra last, and the explicit resolvers
func init() {
	groq := hasKeyProbe("GROQ_API_KEY", "groq", true, false)
	mistral := hasKeyProbe("MISTRAL_API_KEY", "mistral", false, true)
	elevenlabs := hasKeyProbe("ELEVENLABS_API_KEY", "elevenlabs", false, false)
	deepinfra := hasKeyProbe("DEEPINFRA_API_KEY", "deepinfra", true, false)
	openaiAuto := func(ctx context.Context) (bool, error) {
		if !hasOpenAI() {
			return false, nil
		}
		reason, err := openAIAudioUnavailableReason(ctx)
		return err == nil && reason == "", err
	}
	xaiExplicit := func(ctx context.Context) (bool, error) { return hasXAISTTCredentials(ctx), nil }
	cloudSTTProviderSpecs = []cloudSTTSpec{
		{"groq", groq, groq, "STT provider 'groq' configured but GROQ_API_KEY not set", "No local STT available, using Groq Whisper API"},
		{"openai", nil, openaiAuto, "", "No local STT available, using OpenAI Whisper API"},
		{"mistral", mistral, mistral, "STT provider 'mistral' configured but mistralai package not installed or MISTRAL_API_KEY not set", "No local STT available, using Mistral Voxtral Transcribe API"},
		{"xai", xaiExplicit, hasXAISTTCredentialsQuietly, "STT provider 'xai' configured but no xAI credentials are available", "No local STT available, using xAI Grok STT API"},
		{"elevenlabs", elevenlabs, elevenlabs, "STT provider 'elevenlabs' configured but ELEVENLABS_API_KEY not set", "No local STT available, using ElevenLabs Scribe STT API"},
		{"deepinfra", deepinfra, deepinfra, "STT provider 'deepinfra' configured but DEEPINFRA_API_KEY not set (or openai package missing)", "No local STT available, using DeepInfra Whisper API"},
	}
	explicitSTTResolvers = map[string]func(context.Context) (string, error){
		"local": resolveExplicitLocal, "local_command": resolveExplicitLocalCommand, "openai": resolveExplicitOpenAI}
}

// Explicit stt.provider resolved to a usable name or none, unknown names passed through untouched
func resolveExplicitProvider(ctx context.Context, providerName string) (string, error) {
	if resolver, ok := explicitSTTResolvers[providerName]; ok {
		return resolver(ctx)
	}
	for _, spec := range cloudSTTProviderSpecs {
		if spec.name != providerName {
			continue
		}
		ok, err := spec.explicit(ctx)
		if err != nil || ok {
			return providerName, err
		}
		sttLog().Warn(spec.warning)
		return "none", nil
	}
	return providerName, nil
}

// STT provider in use, an explicit stt.provider honoured, else local then cloud auto detection
func getProvider(ctx context.Context, sttConfig map[string]any) (string, error) {
	if !isSTTEnabled(ctx, sttConfig) {
		return "none", nil
	}
	_, explicit := sttConfig["provider"]
	raw := dictGetDefault(sttConfig, "provider", sttDefaultProvider)
	providerName := pyrepr.Str(raw)
	if s, ok := raw.(string); ok && pystr.Lower(pystr.Strip(s)) == "nous" {
		providerName = "openai"
	}
	if explicit && providerName == "local" && sttReadSelection(ctx) == "" {
		explicit = false
	}
	if explicit {
		return resolveExplicitProvider(ctx, providerName)
	}
	if backend := detectLocalBackend(ctx); backend != "" {
		return backend, nil
	}
	for _, spec := range cloudSTTProviderSpecs {
		ok, err := spec.auto(ctx)
		if err != nil {
			return "", err
		}
		if ok {
			sttLog().Info(spec.message)
			return spec.name, nil
		}
	}
	return "none", nil
}

// Provider resolution, preprocessing, CAF conversion and dispatch looked up at call time so tests may replace them
var (
	sttGetProvider      = getProvider
	sttPrepareAudio     = prepareAudioForTranscription
	sttConvertCAF       = convertCAFToWAV
	sttDispatchProvider = dispatchSTTProvider
)

// Releases the cached local whisper model
func unloadLocalModel() {
	localModelMu.Lock()
	defer localModelMu.Unlock()
	if localModel != nil {
		name := localModelName
		if name == "" {
			name = "unknown"
		}
		sttLog().Info(fmt.Sprintf("Unloading local whisper model '%s' after idle timeout", name))
		localModel.release()
		localModel, localModelName = nil, ""
	}
}

// Idle unload seconds of the stt.local section, an error for a non mapping section
func configuredIdleUnloadSeconds(ctx context.Context) (int, error) {
	localCfg, err := orEmptyDict(loadSTTConfig(ctx), "local")
	if err != nil {
		return 0, err
	}
	return getIdleUnloadSeconds(localCfg), nil
}

// Whether a cached model is loaded
func localModelLoaded() bool {
	localModelMu.Lock()
	defer localModelMu.Unlock()
	return localModel != nil
}

// Seconds since the last transcription activity
func secondsSinceTranscription() float64 {
	return (time.Since(sttMonotonicBase) - time.Duration(lastTranscriptionNano.Load())).Seconds()
}

// Ensures the single idle unload watcher runs, governed by the profile of the latest caller
func startIdleUnloadWatcher(ctx context.Context, timeoutSeconds int) {
	idleMgmtMu.Lock()
	defer idleMgmtMu.Unlock()
	idleUnloadPolicyCtx, idleUnloadFallback = context.WithoutCancel(ctx), timeoutSeconds
	if idleUnloadAlive {
		return
	}
	idleUnloadStop = make(chan struct{})
	stop, interval := idleUnloadStop, sttIdleCheckInterval
	idleUnloadAlive = true
	go func() {
		defer func() {
			idleMgmtMu.Lock()
			idleUnloadAlive = false
			idleMgmtMu.Unlock()
		}()
		for {
			select {
			case <-stop:
				return
			case <-time.After(interval):
			}
			if !localModelLoaded() {
				return
			}
			idleMgmtMu.Lock()
			owner, fallback := idleUnloadPolicyCtx, idleUnloadFallback
			idleMgmtMu.Unlock()
			timeout, err := configuredIdleUnloadSeconds(owner)
			if err != nil {
				timeout = fallback
			}
			if timeout <= 0 {
				return
			}
			if secondsSinceTranscription() >= float64(timeout) {
				unloadLocalModel()
				return
			}
		}
	}()
}

// Records transcription activity, resetting the idle timer
func touchTranscriptionTime() {
	lastTranscriptionNano.Store(int64(time.Since(sttMonotonicBase)))
}

// Cached faster-whisper model loaded under the model lock, returned with a reference the caller releases
func getOrLoadLocalModel(ctx context.Context, modelName string, localCfg map[string]any) (*whisperHandle, error) {
	localModelMu.Lock()
	defer localModelMu.Unlock()
	if localModel == nil || localModelName != modelName {
		sttLog().Info(fmt.Sprintf("Loading faster-whisper model '%s' (first load downloads the model)...", modelName))
		m, err := loadLocalWhisperModel(ctx, modelName, dictGetDefault(localCfg, "device", "auto"), dictGetDefault(localCfg, "compute_type", "auto"))
		if err != nil {
			return nil, err
		}
		if localModel != nil {
			localModel.release()
		}
		localModel, localModelName = nil, modelName
		if m == nil {
			return nil, nil
		}
		localModel = &whisperHandle{model: m, refs: 1}
	}
	localModel.acquire()
	return localModel, nil
}

// Loads a model on CPU int8 and makes it the cached singleton, returned with a caller reference
func replaceCachedModelOnCPU(ctx context.Context, modelName string) (*whisperHandle, error) {
	m, err := loadLocalWhisperModel(ctx, modelName, "cpu", "int8")
	if err != nil {
		return nil, err
	}
	localModelMu.Lock()
	defer localModelMu.Unlock()
	if localModel != nil {
		localModel.release()
	}
	localModel, localModelName = &whisperHandle{model: m, refs: 2}, modelName
	return localModel, nil
}

// faster-whisper transcription with a one time CPU retry when the CUDA runtime fails mid decode
func transcribeLocal(ctx context.Context, filePath, modelName string, h sttHints) (map[string]any, error) {
	if !hasFasterWhisper() && !tryLazyInstallSTT(ctx) {
		return sttError("faster-whisper not installed"), nil
	}
	result, err := transcribeLocalBody(ctx, filePath, modelName, h)
	if err != nil {
		sttLog().Error(fmt.Sprintf("Local transcription failed: %v", err))
		return sttError(fmt.Sprintf("Local transcription failed: %v", err)), nil
	}
	return result, nil
}

// Body of the local transcription whose failures the caller maps to one envelope
func transcribeLocalBody(ctx context.Context, filePath, modelName string, h sttHints) (map[string]any, error) {
	sttConfig := loadSTTConfig(ctx)
	localCfg, err := orEmptyDict(sttConfig, "local")
	if err != nil {
		return nil, err
	}
	touchTranscriptionTime()
	handle, err := getOrLoadLocalModel(ctx, modelName, localCfg)
	if err != nil {
		return nil, err
	}
	if handle == nil {
		return sttError("Local whisper model failed to load"), nil
	}
	defer func() { handle.release() }()
	kwargs, err := buildLocalTranscribeKwargs(ctx, sttConfig)
	if err != nil {
		return nil, err
	}
	var effective any = h.Language
	if h.Language == "" {
		effective = kwargs["language"]
	}
	if normalized := normalizeLocalSTTLanguage(effective, handle.model.SupportedLanguages()); normalized != "" {
		kwargs["language"] = normalized
	} else {
		delete(kwargs, "language")
	}
	if h.Prompt != "" {
		kwargs["initial_prompt"] = h.Prompt
	}
	segments, info, err := handle.model.Transcribe(ctx, filePath, kwargs)
	if err != nil {
		if !looksLikeCudaLibError(err) {
			return nil, err
		}
		sttLog().Warn(fmt.Sprintf("faster-whisper CUDA runtime failed mid-transcribe (%v) — evicting cached model and retrying on CPU (int8).", err))
		replacement, rerr := replaceCachedModelOnCPU(ctx, modelName)
		if rerr != nil {
			return nil, rerr
		}
		handle.release()
		handle = replacement
		if segments, info, err = handle.model.Transcribe(ctx, filePath, kwargs); err != nil {
			return nil, err
		}
	}
	transcript := joinConfidentSegments(segments, localCfg)
	sttLog().Info(fmt.Sprintf("Transcribed %s via local whisper (%s, lang=%s, %.1fs audio)", filepath.Base(filePath), modelName, info.Language, info.Duration))
	touchTranscriptionTime()
	if idle := getIdleUnloadSeconds(localCfg); idle > 0 {
		startIdleUnloadWatcher(ctx, idle)
	}
	return sttOK(transcript, "local"), nil
}

// Envelope refusing to ship a credential store to an STT provider, nil when the path is allowed
func readBlockErrorResult(filePath string) map[string]any {
	if blocked := sttReadBlockError(filePath); blocked != "" {
		return sttError(blocked)
	}
	return nil
}

// Credential read guard of the file safety policy, the seam tests replace
var sttReadBlockError = func(filePath string) string { return tools.ReadBlockError(nil, filePath) }

// Transcribes a validated file with the configured provider, converting CAF, trimming silence and splitting oversized uploads
func transcribePreparedAudio(ctx context.Context, filePath, model, source string) (map[string]any, error) {
	if e := readBlockErrorResult(filePath); e != nil {
		return e, nil
	}
	if e := validateAudioFile(filePath, false); e != nil {
		return e, nil
	}
	sttConfig := loadSTTConfig(ctx)
	if !isSTTEnabled(ctx, sttConfig) {
		return sttError("STT is disabled in config.yaml (stt.enabled: false)."), nil
	}
	providerName, err := sttGetProvider(ctx, sttConfig)
	if err != nil {
		return nil, err
	}
	remote := !isLocalSTTProvider(providerName)
	if remote && strings.ToLower(sttSuffix(filePath)) == ".caf" {
		workDir, err := os.MkdirTemp("", "icebath-caf-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(workDir)
		converted, err := sttConvertCAF(filePath, workDir)
		if err != nil {
			return nil, err
		}
		if converted == "" {
			return sttError("CAF audio could not be converted to WAV."), nil
		}
		filePath = converted
	}
	if cloudSTTProviders[providerName] {
		if trimmed := trimSilenceForCloudSTT(filePath, sttConfig); trimmed != "" {
			filePath = trimmed
			defer os.RemoveAll(filepath.Dir(trimmed))
		}
	}
	if remote {
		limit := uploadLimit(providerName, limitModelName(providerName, sttConfig, model))
		if exceedsUploadLimit(filePath, limit) {
			hModel, hLanguage, hPrompt := resolveDispatchHints(ctx, filePath, providerName, sttConfig, model, source)
			return transcribeOversized(filePath, limit, func(part string) (map[string]any, error) {
				return routeSTTProvider(ctx, part, providerName, sttConfig, hModel, hLanguage, hPrompt)
			})
		}
	}
	return sttDispatchProvider(ctx, filePath, providerName, sttConfig, model, source)
}

// Model whose duration cap applies to an upload, built ins resolving config and defaults
func limitModelName(providerName string, sttConfig map[string]any, model string) string {
	if _, ok := builtinSTTModelKeys[providerName]; ok {
		return builtinModelName(providerName, sttConfig, model)
	}
	return model
}

// Config section, key, default and empty as missing flag of a built in provider model
type sttModelKey struct {
	section, key, def string
	emptyIsMissing    bool
}

var builtinSTTModelKeys = map[string]sttModelKey{
	"local":         {"local", "model", sttDefaultLocalModel, false},
	"local_command": {"local", "model", sttDefaultLocalModel, false},
	"groq":          {"groq", "model", sttDefaultGroqModel, true},
	"openai":        {"openai", "model", sttDefaultOpenAIModel, false},
	"mistral":       {"mistral", "model", sttDefaultMistralModel, false},
	"xai":           {"xai", "model", "", true},
	"elevenlabs":    {"elevenlabs", "model_id", sttDefaultElevenLabsModel, false},
	"deepinfra":     {"deepinfra", "model", "", true},
}

// String of a config value, empty for None
func sttStrOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// Built in provider model from the caller override, then stt.<provider> config, then the default
func builtinModelName(providerName string, sttConfig map[string]any, model string) string {
	if model != "" {
		return model
	}
	k := builtinSTTModelKeys[providerName]
	cfg := getProviderSection(sttConfig, k.section)
	var resolved any
	if k.emptyIsMissing {
		resolved = cfg[k.key]
		if !pyTruthy(resolved) {
			resolved = k.def
		}
	} else {
		resolved = dictGetDefault(cfg, k.key, k.def)
	}
	if providerName == "xai" {
		return normalizeXAISTTModel(sttStrOrEmpty(resolved))
	}
	return sttStrOrEmpty(resolved)
}

// Model, language and prompt of one recording from stt.prompt and pre_transcription hooks
func resolveDispatchHints(ctx context.Context, filePath, providerName string, sttConfig map[string]any, model, source string) (string, string, string) {
	prompt := ""
	if s, ok := sttConfig["prompt"].(string); ok && pystr.Strip(s) != "" {
		prompt = s
	}
	model, language, prompt := applyPreTranscriptionHook(ctx, filePath, providerName, model, getProviderSection(sttConfig, providerName)["language"], prompt, source)
	return model, language, enforcePromptLengthLimit(prompt, providerName)
}

// Routes a file to the handler of a provider, built in then command then plugin
func dispatchSTTProvider(ctx context.Context, filePath, providerName string, sttConfig map[string]any, model, source string) (map[string]any, error) {
	m, l, p := resolveDispatchHints(ctx, filePath, providerName, sttConfig, model, source)
	return routeSTTProvider(ctx, filePath, providerName, sttConfig, m, l, p)
}

// Handler call of a provider with resolved hints, the no provider envelope when nothing claims it
func routeSTTProvider(ctx context.Context, filePath, providerName string, sttConfig map[string]any, model, language, prompt string) (map[string]any, error) {
	if builtinSTTProviders[providerName] {
		modelName := builtinModelName(providerName, sttConfig, model)
		if providerName == "local" || providerName == "local_command" {
			modelName = normalizeLocalModel(modelName)
		}
		return sttBuiltinHandlers[providerName](ctx, filePath, modelName, sttHints{Language: language, Prompt: prompt})
	}
	if cfg := resolveCommandSTTProviderConfig(providerName, sttConfig); cfg != nil {
		return transcribeCommandSTT(ctx, filePath, providerName, cfg, sttConfig, model, language, prompt), nil
	}
	var pluginModel any = sttNone(model)
	if model == "" {
		pluginModel = getProviderSection(sttConfig, providerName)["model"]
	}
	pluginLanguage := language
	if pluginLanguage == "" {
		pluginLanguage = resolveSTTLanguage(ctx, providerName, sttConfig)
	}
	if result := dispatchToSTTPluginProvider(ctx, filePath, providerName, sttConfig, pluginModel, sttNone(pluginLanguage), prompt); result != nil {
		return result, nil
	}
	return noProviderError(ctx, providerName, sttConfig)
}

// Error envelope when nothing claimed a provider, unregistered name then openai selection reason then the setup hint
func noProviderError(ctx context.Context, providerName string, sttConfig map[string]any) (map[string]any, error) {
	key := pystr.Lower(pystr.Strip(providerName))
	if _, set := sttConfig["provider"]; set && key != "" && !builtinSTTProviders[key] && key != "none" {
		return unregisteredSTTProviderError(key), nil
	}
	configured := ""
	if v := sttConfig["provider"]; pyTruthy(v) {
		configured = pyrepr.Str(v)
	}
	if key == "none" && configured == "openai" && hasOpenAI() {
		reason, err := openAIAudioUnavailableReason(ctx)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return sttError(reason), nil
		}
	}
	return sttError("No STT provider available. Install faster-whisper for free local " +
		"transcription, configure " + localSTTCommandEnv + " or install a local whisper CLI, " +
		"set GROQ_API_KEY for free Groq Whisper, set MISTRAL_API_KEY for Mistral " +
		"Voxtral Transcribe, configure xAI OAuth or set XAI_API_KEY for xAI Grok STT, " +
		"set ELEVENLABS_API_KEY for ElevenLabs Scribe, or set VOICE_TOOLS_OPENAI_KEY " +
		"or OPENAI_API_KEY for the OpenAI Whisper API."), nil
}

// TranscribeAudio validates, preprocesses supported inputs and dispatches transcription, source labelling the caller surface
func TranscribeAudio(ctx context.Context, filePath, model, source string) (map[string]any, error) {
	if blocked := readBlockErrorResult(filePath); blocked != nil {
		return blocked, nil
	}
	isSilk := strings.ToLower(sttSuffix(filePath)) == ".silk"
	if e := validateAudioSourceFile(filePath, isSilk); e != nil {
		return e, nil
	}
	prepared, cleanupDir, prepError := sttPrepareAudio(ctx, filePath)
	if prepError != nil || prepared == "" {
		if prepError != nil {
			return prepError, nil
		}
		return sttError("Audio preprocessing did not produce a file for transcription."), nil
	}
	if cleanupDir != "" {
		defer os.RemoveAll(cleanupDir)
	}
	if e := validateAudioFile(prepared, false); e != nil {
		return e, nil
	}
	return transcribePreparedAudio(ctx, prepared, model, source)
}

// TranscribeAudioLocalFallback tries an installed local backend without installing anything or falling through to cloud
func TranscribeAudioLocalFallback(ctx context.Context, filePath, model string) (map[string]any, error) {
	if e := validateAudioFile(filePath, false); e != nil {
		return e, nil
	}
	localModel := model
	if localModel == "" {
		localCfg, err := orEmptyDict(loadSTTConfig(ctx), "local")
		if err != nil {
			return nil, err
		}
		localModel = sttStrOrEmpty(dictGetDefault(localCfg, "model", sttDefaultLocalModel))
	}
	if hasFasterWhisper() {
		return transcribeLocal(ctx, filePath, normalizeLocalModel(localModel), sttHints{})
	}
	if hasLocalCommand(ctx) {
		return transcribeLocalCommand(ctx, filePath, normalizeLocalModel(localModel), sttHints{})
	}
	return sttError("No installed local STT backend is available.", "provider", "local"), nil
}
