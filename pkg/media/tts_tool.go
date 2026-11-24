package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/media/speech"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Python ImportError of a provider package missing from the managed environment
type ttsImportError struct{ msg string }

func (e *ttsImportError) Error() string { return e.msg }

// Importer of a local engine package, failing like a missing import
func ttsSDKImporter(module string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if !ttsModuleImportable(ctx, module) {
			return &ttsImportError{msg: fmt.Sprintf("the Python package %s is not installed", module)}
		}
		return nil
	}
}

// Whether the managed environment finds a module, the find_spec seam tests replace
var ttsModuleImportable = func(ctx context.Context, module string) bool { return pm.AnchorImportable(ctx, module) }

var (
	ttsImportKittenTTS = ttsSDKImporter("kittentts")
	ttsSpeechSupported = speech.Supported
)

// Whether an importer succeeds
func ttsImportable(ctx context.Context, importer func(context.Context) error) bool {
	return importer(ctx) == nil
}

func checkNeuTTSAvailable(ctx context.Context) bool    { return ttsModuleImportable(ctx, "neutts") }
func checkKittenTTSAvailable(ctx context.Context) bool { return ttsModuleImportable(ctx, "kittentts") }

const defaultTTSProvider = "edge"

// Audio cache directory of the active profile home, the seam tests replace
var ttsDefaultOutputDir = func(ctx context.Context, env *tools.Env) string {
	return home.IcebathDir(ttsHome(ctx, env), "cache/audio", "audio_cache")
}

var opusVoicePlatforms = map[string]bool{"telegram": true, "matrix": true, "feishu": true, "whatsapp": true, "signal": true}

var mediaDirectiveRe = regexp.MustCompile("(?i)media:\\s*[`'\"*_]*(?:[`'\"]|[a-z]:[/\\\\]|~?/)")

var nativeOpusProviders = map[string]bool{"openai": true, "elevenlabs": true, "mistral": true, "gemini": true}
var ffmpegOpusProviders = map[string]bool{"edge": true, "neutts": true, "minimax": true, "xai": true, "kittentts": true, "piper": true}

// Built in engine generator signature
type ttsGenerator func(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest) (string, error)

// One built in dispatch row, availability nil when the engine needs no package
type builtinTTSEntry struct {
	available    func(ctx context.Context) bool
	label        string
	generator    func() ttsGenerator
	missingError string
}

var builtinTTSDispatch = map[string]builtinTTSEntry{
	"elevenlabs": {nil, "ElevenLabs", func() ttsGenerator { return ttsGenerateElevenLabs }, ""},
	"openai":     {nil, "OpenAI TTS", nil, ""},
	"deepinfra":  {nil, "DeepInfra TTS", func() ttsGenerator { return ttsGenerateDeepInfra }, ""},
	"minimax":    {nil, "MiniMax TTS", func() ttsGenerator { return ttsGenerateMiniMax }, ""},
	"xai":        {nil, "xAI TTS", func() ttsGenerator { return ttsGenerateXAI }, ""},
	"mistral":    {nil, "Mistral Voxtral TTS", func() ttsGenerator { return ttsGenerateMistral }, ""},
	"gemini":     {nil, "Google Gemini TTS", func() ttsGenerator { return ttsGenerateGemini }, ""},
	"neutts": {func(ctx context.Context) bool { return ttsCheckNeuTTS(ctx) }, "NeuTTS (local)", func() ttsGenerator { return ttsGenerateNeuTTS },
		"NeuTTS provider selected but neutts is not installed. Start icebath, type /setup voice and choose NeuTTS; espeak-ng is also required."},
	"kittentts": {func(ctx context.Context) bool { return ttsImportable(ctx, ttsImportKittenTTS) }, "KittenTTS (local, ~25MB)", func() ttsGenerator { return ttsGenerateKittenTTS },
		"KittenTTS provider selected but 'kittentts' package not installed. Start 'icebath', type '/setup voice' and choose KittenTTS."},
	"piper": {func(context.Context) bool { return ttsSpeechSupported() }, "Piper (local)", func() ttsGenerator { return ttsGeneratePiper },
		"Piper provider selected but the local speech engine has no build for this platform. Start 'icebath', type '/setup voice' and choose another provider."},
}

// Generator seams resolved at call time so tests can replace each engine
var (
	ttsGenerateEdge       ttsGenerator = generateEdgeTTS
	ttsGenerateElevenLabs ttsGenerator = generateElevenLabs
	ttsGenerateDeepInfra  ttsGenerator = generateDeepInfraTTS
	ttsGenerateMiniMax    ttsGenerator = generateMiniMaxTTS
	ttsGenerateXAI        ttsGenerator = generateXAITTS
	ttsGenerateMistral    ttsGenerator = generateMistralTTS
	ttsGenerateGemini     ttsGenerator = generateGeminiTTS
	ttsGenerateNeuTTS     ttsGenerator = generateNeuTTS
	ttsGenerateKittenTTS  ttsGenerator = generateKittenTTS
	ttsGeneratePiper      ttsGenerator = generatePiperTTS
	ttsCheckNeuTTS                     = checkNeuTTSAvailable
)

// Python json.dumps with ensure_ascii off over an ordered object
func ttsDumps(v any) string {
	s, err := jsonutil.Encode(v)
	if err != nil {
		return tools.ErrorResult(err)
	}
	return s
}

// Failure envelope with success first then the error
func ttsErrorJSON(message string) string {
	return ttsDumps(jsonutil.ObjectOf("success", false, "error", message))
}

// Registry tool_error envelope carrying success false
func ttsToolError(message string) string {
	return ttsDumps(jsonutil.ObjectOf("error", tools.BoundErrorText(message), "success", false))
}

// Built in engine of a provider with no error, unknown names taking the Edge default
func selectBuiltinEngine(ctx context.Context, provider string) (string, string) {
	if entry, ok := builtinTTSDispatch[provider]; ok && entry.available != nil && !entry.available(ctx) {
		return provider, ttsErrorJSON(entry.missingError)
	}
	return provider, ""
}

// Runs the already selected built in engine
func synthesizeBuiltin(ctx context.Context, env *tools.Env, engine, text, fileStr string, req *ttsRequest, instructions string) error {
	entry, ok := builtinTTSDispatch[engine]
	label := "Edge TTS"
	if ok {
		label = entry.label
	}
	pkgLog().Info(fmt.Sprintf("Generating speech with %s...", label))
	var err error
	switch {
	case !ok:
		_, err = ttsGenerateEdge(ctx, env, text, fileStr, req)
	case engine == "openai":
		_, err = ttsGenerateOpenAI(ctx, env, text, fileStr, req, openAITTSOptions{Instructions: instructions})
	default:
		_, err = entry.generator()(ctx, env, text, fileStr, req)
	}
	return err
}

// Voice bubble eligibility, converting to Opus when the provider and platform need it
func finalizeVoiceDelivery(ctx context.Context, fileStr, provider string, command *commandProvider, wantOpus bool) (string, bool, error) {
	var optedIn bool
	switch {
	case command != nil:
		optedIn = command.VoiceCompatible
	case !builtinTTSProviders[provider]:
		optedIn = pluginProviderIsVoiceCompatible(ctx, provider)
	case wantOpus && ffmpegOpusProviders[provider] && !strings.HasSuffix(fileStr, ".ogg"):
		opusPath, err := convertToOpus(fileStr)
		if err != nil {
			return "", false, err
		}
		if opusPath != "" {
			return opusPath, true, nil
		}
		return fileStr, false, nil
	default:
		return fileStr, nativeOpusProviders[provider] && wantOpus && strings.HasSuffix(fileStr, ".ogg"), nil
	}
	if !optedIn {
		return fileStr, false, nil
	}
	if !strings.HasSuffix(fileStr, ".ogg") {
		converted, err := convertToOpus(fileStr)
		if err != nil {
			return "", false, err
		}
		if converted != "" {
			fileStr = converted
		}
	}
	return fileStr, strings.HasSuffix(fileStr, ".ogg"), nil
}

var onceWarnings sync.Map

// Warns once per requested value that a per call provider override disagrees with the config
func warnIgnoredTTSProviderOverride(requested, configured string) {
	if configured == "" {
		return
	}
	if _, seen := onceWarnings.LoadOrStore("tts_provider_override\x00"+requested, true); seen {
		return
	}
	pkgLog().Warn(fmt.Sprintf("Ignoring per-call TTS provider override %s; tts.provider is %s", errchain.Show(requested), errchain.Show(configured)))
}

// Request with a call's own speed laid over, a provider override that disagrees with the configured one ignored
func applyCallOverrides(req *ttsRequest, speed any, provider string) (*ttsRequest, error) {
	if speed != nil {
		f, err := ttsFloat(speed)
		if err != nil {
			return nil, err
		}
		req = req.withCallSpeed(f)
	}
	if requested := strings.TrimSpace(strings.ToLower(provider)); requested != "" && requested != req.Provider {
		warnIgnoredTTSProviderOverride(requested, req.Provider)
	}
	return req, nil
}

var elevenLabsVoiceIDRe = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)

// Whether built in ElevenLabs is the configured provider so the model may pick voices
func elevenLabsVoiceSelectionActive(req *ttsRequest) bool {
	return req.Provider == "elevenlabs"
}

// Request with a call's own ElevenLabs voice laid over, ignored with a warning on other providers
func applyVoiceOverride(req *ttsRequest, voiceID string) (*ttsRequest, string) {
	voiceID = strings.TrimSpace(voiceID)
	if voiceID == "" {
		return req, ""
	}
	if !elevenLabsVoiceSelectionActive(req) {
		pkgLog().Warn(fmt.Sprintf("Ignoring per-call TTS voice_id; tts.provider is %s", errchain.Show(req.Provider)))
		return req, ""
	}
	if !elevenLabsVoiceIDRe.MatchString(voiceID) {
		return req, ttsToolError(fmt.Sprintf("Invalid ElevenLabs voice_id %s: use a voice_id returned by find_voices.", errchain.Show(voiceID)))
	}
	out := *req
	out.ElevenLabs.VoiceID = voiceID
	return &out, ""
}

// One page of ElevenLabs voices for the find_voices argument
func findVoices(ctx context.Context, env *tools.Env, find any) string {
	m, ok := find.(map[string]any)
	if !ok {
		return ttsToolError("find_voices must be an object, e.g. {\"query\": \"orc\"}")
	}
	req := loadTTSRequest(ctx, env)
	if !elevenLabsVoiceSelectionActive(req) {
		return ttsToolError("find_voices needs tts.provider: elevenlabs")
	}
	apiKey := ttsResolveProviderKey(env, "ELEVENLABS_API_KEY", "elevenlabs")
	if apiKey == "" {
		return ttsToolError("ELEVENLABS_API_KEY not set. Get one at https://elevenlabs.io/")
	}
	return searchVoices(ctx, m, req, apiKey)
}

// Session platform and whether it wants Ogg Opus voice bubbles
func sessionPlatform(env *tools.Env) (string, bool) {
	platform := strings.ToLower(tools.GetSessionEnv(env, "ICEBATH_SESSION_PLATFORM", ""))
	return platform, opusVoicePlatforms[platform]
}

// Output file of a call, an error envelope for unsafe caller paths, the timestamped cache default otherwise
func resolveOutputBase(ctx context.Context, env *tools.Env, outputPath, provider string, command *commandProvider, wantOpus bool) (string, string, error) {
	var filePath string
	if outputPath != "" {
		if tools.HasUnsafePathChars(outputPath) {
			return "", ttsErrorJSON("output_path contains control characters or line separators; use a plain filesystem path"), nil
		}
		if mediaDirectiveRe.MatchString(outputPath) {
			return "", ttsErrorJSON("output_path must not contain a media directive (MEDIA:<path>)"), nil
		}
		if tools.HasTraversalComponent(outputPath) {
			return "", ttsErrorJSON(fmt.Sprintf("output_path contains '..' traversal component: %s. Use an absolute path or one relative to the current directory without '..'.", outputPath)), nil
		}
		filePath = pathutil.ExpandUser(outputPath)
		if command != nil {
			p, err := pathutil.WithSuffix(filePath, "."+command.OutputFormat)
			if err != nil {
				return "", "", err
			}
			filePath = p
		}
		if tools.IsWriteDenied(env, filePath) || tools.IsWriteApprovalRequired(env, filePath) {
			return "", ttsErrorJSON(fmt.Sprintf("output_path targets a protected credential or system path: %s. Choose a normal audio output location.", filePath)), nil
		}
	} else {
		ext := "mp3"
		if command != nil {
			ext = command.OutputFormat
		} else if wantOpus && nativeOpusProviders[provider] {
			ext = "ogg"
		}
		now := ttsNow()
		stamp := fmt.Sprintf("%s_%06d", now.Format("20060102_150405"), now.Nanosecond()/1000)
		filePath = filepath.Join(ttsDefaultOutputDir(ctx, env), "tts_"+stamp+"."+ext)
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o777); err != nil {
		return "", "", err
	}
	return filePath, "", nil
}

// Wall clock of default output names, the seam tests freeze
var ttsNow = time.Now

// MEDIA lines with the voice bubble marker when voice compatible
func mediaTag(paths []string, voiceCompatible bool) string {
	lines := make([]string, len(paths))
	for i, p := range paths {
		lines[i] = "MEDIA:" + p
	}
	tag := strings.Join(lines, "\n")
	if voiceCompatible {
		return "[[audio_as_voice]]\n" + tag
	}
	return tag
}

var valueErrorClasses = map[string]bool{"ValueError": true, "UnicodeDecodeError": true, "UnicodeEncodeError": true, "UnicodeError": true, "JSONDecodeError": true, "Error": true}

// Python exception family of an error, value, file_not_found or other
func ttsErrorFamily(err error) string {
	var ve *ttsValueError
	var le *localEngineError
	switch {
	case errors.As(err, &ve):
		return "value"
	case errors.As(err, &le):
		if valueErrorClasses[le.Class] {
			return "value"
		}
		if le.Class == "FileNotFoundError" {
			return "file_not_found"
		}
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, exec.ErrNotFound):
		return "file_not_found"
	}
	return "other"
}

// Plain text of an error, a missing command and a file failure named with their reason
func ttsErrorText(err error) string {
	var pe *fs.PathError
	var ee *exec.Error
	switch {
	case errors.As(err, &ee):
		return ee.Name + " is not installed or not on PATH"
	case errors.As(err, &pe):
		return pe.Err.Error() + ": " + pe.Path
	}
	return err.Error()
}

// Logs and wraps a synthesis failure as the standard error envelope
func ttsToolFailure(prefix, provider string, err error) string {
	msg := fmt.Sprintf("%s (%s): %s", prefix, provider, ttsErrorText(err))
	pkgLog().Error(msg, "error", err)
	return ttsToolError(msg)
}

// Options of one chunk synthesis
type ttsSingleOptions struct {
	Provider     string
	Request      *ttsRequest
	Command      *commandProvider
	WantOpus     bool
	Instructions string
}

// Synthesizes one provider safe chunk into the path and returns its result envelope
func textToSpeechSingle(ctx context.Context, env *tools.Env, text, fileStr string, o ttsSingleOptions) string {
	provider := o.Provider
	result, err := func() (string, error) {
		if o.Command != nil {
			pkgLog().Info(fmt.Sprintf("Generating speech with command TTS provider '%s'...", provider))
			p, err := ttsGenerateCommand(tools.WithEnvHome(ctx, env), env, text, fileStr, o.Command, o.Request)
			if err != nil {
				return "", err
			}
			fileStr = p
		} else if pluginPath, err := ttsDispatchPlugin(ctx, text, fileStr, provider, o.Request, builtinTTSProviders[provider]); err != nil {
			return "", err
		} else if pluginPath != "" {
			fileStr = pluginPath
		} else {
			engine, errJSON := selectBuiltinEngine(ctx, provider)
			provider = engine
			if errJSON != "" {
				return errJSON, nil
			}
			if err := synthesizeBuiltin(ctx, env, engine, text, fileStr, o.Request, o.Instructions); err != nil {
				return "", err
			}
		}
		if info, err := os.Stat(fileStr); err != nil || info.Size() == 0 {
			return ttsErrorJSON(fmt.Sprintf("TTS generation produced no output (provider: %s)", provider)), nil
		}
		repaired, err := repairOggContainer(fileStr)
		if err != nil {
			return "", err
		}
		final, voiceCompatible, err := finalizeVoiceDelivery(ctx, repaired, provider, o.Command, o.WantOpus)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(final)
		if err != nil {
			return "", err
		}
		pkgLog().Info(fmt.Sprintf("TTS audio saved: %s (%s bytes, provider: %s)", final, textutil.Thousands(info.Size()), provider))
		return ttsDumps(jsonutil.ObjectOf("success", true, "file_path", final, "media_tag", mediaTag([]string{final}, voiceCompatible),
			"provider", provider, "voice_compatible", voiceCompatible)), nil
	}()
	if err == nil {
		return result
	}
	switch ttsErrorFamily(err) {
	case "value":
		return ttsToolFailure("TTS configuration error", provider, err)
	case "file_not_found":
		return ttsToolFailure("TTS dependency missing", provider, err)
	}
	return ttsToolFailure("TTS generation failed", provider, err)
}

// Command provider generator, the seam tests replace
var ttsGenerateCommand = generateCommandTTS

// Plugin dispatch for names that are not built in, empty when no plugin services the provider
func ttsDispatchPlugin(ctx context.Context, text, fileStr, provider string, req *ttsRequest, builtin bool) (string, error) {
	if builtin {
		return "", nil
	}
	return ttsDispatchToPlugin(ctx, text, fileStr, provider, req)
}

// Plugin dispatcher, the seam tests replace
var ttsDispatchToPlugin = dispatchToPluginTTSProvider

// Error of one chunk reporting failure, its text the final tool error
type chunkFailedError struct{ msg string }

func (e *chunkFailedError) Error() string { return e.msg }

// Synthesizes every chunk into numbered paths beside the base, recording each touched path
func synthesizeChunks(ctx context.Context, env *tools.Env, chunks []string, basePath string, generated map[string]bool, o ttsSingleOptions) ([]string, []map[string]any, error) {
	var encoded []string
	var results []map[string]any
	for index, chunk := range chunks {
		chunkPath := basePath
		if len(chunks) > 1 {
			chunkPath = withName(basePath, fmt.Sprintf("%s.chunk%03d%s", pathutil.Stem(basePath), index+1, pathutil.Suffix(basePath)))
		}
		if _, err := os.Lstat(chunkPath); err == nil {
			chunkPath = withName(chunkPath, "."+pathutil.Stem(chunkPath)+"."+randomHex(16)+pathutil.Suffix(chunkPath))
		}
		generated[chunkPath] = true
		raw := textToSpeechSingle(ctx, env, chunk, chunkPath, o)
		var result map[string]any
		if err := json.Unmarshal([]byte(raw), &result); err != nil || result == nil {
			return nil, nil, &ttsRuntimeError{msg: fmt.Sprintf("TTS chunk %d returned invalid JSON: %s", index+1, clipRunes(raw, 200))}
		}
		if !pyTruthy(result["success"]) {
			errMsg := dictGetDefault(result, "error", "unknown error")
			return nil, nil, &chunkFailedError{msg: fmt.Sprintf("TTS chunk %d failed (%s): %s", index+1, o.Provider, anyutil.String(errMsg))}
		}
		actual := chunkPath
		if pyTruthy(result["file_path"]) {
			actual = anyutil.String(result["file_path"])
		}
		if info, err := os.Stat(actual); err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
			return nil, nil, &ttsRuntimeError{msg: fmt.Sprintf("TTS chunk %d produced no final audio: %s", index+1, actual)}
		}
		generated[actual] = true
		encoded = append(encoded, actual)
		results = append(results, result)
	}
	return encoded, results, nil
}

// Arguments of one text_to_speech call, nil or empty meaning Python None
type ttsCallArgs struct {
	Text         string
	OutputPath   string
	Speed        any
	Instructions string
	Provider     string
	VoiceID      string
	FindVoices   any
}

// Long form speech synthesis returning the JSON result envelope, or a voice search when find_voices is set
func textToSpeechTool(ctx context.Context, env *tools.Env, a ttsCallArgs) (string, error) {
	if a.FindVoices != nil {
		return findVoices(ctx, env, a.FindVoices), nil
	}
	if strings.TrimSpace(a.Text) == "" {
		return ttsToolError("Text is required"), nil
	}
	text := tools.PrepareSpokenText(a.Text, nil)
	if text == "" {
		return ttsToolError("Text is empty after TTS cleanup"), nil
	}
	req, err := applyCallOverrides(loadTTSRequest(ctx, env), a.Speed, a.Provider)
	if err != nil {
		return "", err
	}
	req, errJSON := applyVoiceOverride(req, a.VoiceID)
	if errJSON != "" {
		return errJSON, nil
	}
	provider := req.Provider
	command := req.command(provider)
	maxLen := resolveMaxTextLength(provider, req)
	chunks := splitTextForTTS(text, maxLen)
	if len(chunks) == 0 {
		return ttsToolError("Text is required"), nil
	}
	if len(chunks) > 1 {
		pkgLog().Info(fmt.Sprintf("TTS text for provider %s split into %d chunks (input=%d chars, cap=%d)", provider, len(chunks), utf8.RuneCountInString(text), maxLen))
	}
	platform, wantOpus := sessionPlatform(env)
	profile := resolveAudioDeliveryProfile(platform, req)
	basePath, errJSON, err := resolveOutputBase(ctx, env, a.OutputPath, provider, command, wantOpus)
	if err != nil {
		return "", err
	}
	if errJSON != "" {
		return errJSON, nil
	}
	generated := map[string]bool{}
	var finalPaths []string
	defer func() {
		final := map[string]bool{}
		for _, p := range finalPaths {
			final[absPath(p)] = true
		}
		for artifact := range generated {
			if !final[absPath(artifact)] {
				removeQuietly(artifact)
			}
		}
	}()
	result, err := func() (string, error) {
		encoded, results, err := synthesizeChunks(ctx, env, chunks, basePath, generated, ttsSingleOptions{Provider: provider, Request: req,
			Command: command, WantOpus: wantOpus, Instructions: a.Instructions})
		if err != nil {
			return "", err
		}
		voiceCompatible := len(results) > 0
		for _, r := range results {
			voiceCompatible = voiceCompatible && pyTruthy(r["voice_compatible"])
		}
		deliveryBase, err := pathutil.WithSuffix(basePath, pathutil.Suffix(encoded[0]))
		if err != nil {
			return "", &ttsValueError{msg: err.Error()}
		}
		paths, combined, err := buildAudioDeliveryFiles(encoded, deliveryBase, profile, voiceCompatible)
		if err != nil {
			return "", err
		}
		finalPaths = paths
		for _, p := range finalPaths {
			info, err := os.Stat(p)
			if err != nil {
				return "", err
			}
			pkgLog().Info(fmt.Sprintf("TTS audio saved: %s (%s bytes, provider: %s)", p, textutil.Thousands(info.Size()), provider))
		}
		filePaths := make([]any, len(finalPaths))
		for i, p := range finalPaths {
			filePaths[i] = p
		}
		return ttsDumps(jsonutil.ObjectOf("success", true, "file_path", finalPaths[0], "file_paths", filePaths,
			"media_tag", mediaTag(finalPaths, voiceCompatible), "provider", dictGetDefault(results[0], "provider", provider),
			"voice_compatible", voiceCompatible, "chunk_count", len(chunks), "delivery_file_count", len(finalPaths),
			"combined_chunks", combined, "delivery_profile", jsonutil.ObjectOf("platform", profile.Platform,
				"max_file_bytes", profile.MaxFileBytes, "target_file_bytes", profile.targetFileBytes()))), nil
	}()
	if err == nil {
		return result, nil
	}
	var chunkFailed *chunkFailedError
	if errors.As(err, &chunkFailed) {
		return ttsToolError(chunkFailed.msg), nil
	}
	if ttsErrorFamily(err) == "value" {
		return ttsToolFailure("TTS delivery error", provider, err), nil
	}
	return ttsToolFailure("TTS long-form generation failed", provider, err), nil
}

// Tool env of the profile the context scopes, the caller's bound tool env carried over with that profile's secrets
func scopedToolEnv(ctx context.Context) *tools.Env {
	return tools.EnvForProfileHome(ctx, tools.ToolEnvFrom(ctx), home.Dir(ctx))
}

// TextToSpeechTool speaks text into the output path for gateway auto TTS, returning the tool result envelope
func TextToSpeechTool(ctx context.Context, text, outputPath string) (string, error) {
	return textToSpeechTool(ctx, scopedToolEnv(ctx), ttsCallArgs{Text: text, OutputPath: outputPath})
}

// Whether the MiniMax region and credential resolve
func miniMaxRequirements(ctx context.Context, env *tools.Env) bool {
	_, err := resolveMiniMaxTTSRuntime(env, loadTTSRequest(ctx, env))
	return err == nil
}

// Whether an xAI API key or OAuth bearer resolves, preferring the API key
func xaiRequirements(env *tools.Env) bool {
	ok := false
	_ = pluginCall(func() error {
		_, key, _ := ttsResolveXAICredentials(env)
		ok = key != ""
		return nil
	})
	return ok
}

// Passive availability of each built in provider, never installing
var builtinTTSRequirements = map[string]func(ctx context.Context, env *tools.Env) bool{
	"edge": func(ctx context.Context, env *tools.Env) bool { return true },
	"elevenlabs": func(ctx context.Context, env *tools.Env) bool {
		return ttsResolveProviderKey(env, "ELEVENLABS_API_KEY", "elevenlabs") != ""
	},
	"openai": func(ctx context.Context, env *tools.Env) bool {
		return hasOpenAIAudioBackend(ctx, env)
	},
	"deepinfra": func(ctx context.Context, env *tools.Env) bool {
		return ttsResolveProviderKey(env, "DEEPINFRA_API_KEY", "deepinfra") != ""
	},
	"minimax": miniMaxRequirements,
	"xai":     func(ctx context.Context, env *tools.Env) bool { return xaiRequirements(env) },
	"gemini": func(ctx context.Context, env *tools.Env) bool {
		return ttsResolveProviderKey(env, "GEMINI_API_KEY", "gemini") != "" || ttsResolveProviderKey(env, "GOOGLE_API_KEY", "gemini") != ""
	},
	"mistral": func(ctx context.Context, env *tools.Env) bool {
		return ttsResolveProviderKey(env, "MISTRAL_API_KEY", "mistral") != ""
	},
	"neutts":    func(ctx context.Context, env *tools.Env) bool { return ttsCheckNeuTTS(ctx) },
	"kittentts": func(ctx context.Context, env *tools.Env) bool { return checkKittenTTSAvailable(ctx) },
	"piper":     func(ctx context.Context, env *tools.Env) bool { return ttsSpeechSupported() },
}

// Whether the resolved provider can run now
func checkTTSRequirements(ctx context.Context, env *tools.Env) bool {
	req := loadTTSRequest(ctx, env)
	provider := req.Provider
	if req.command(provider) != nil {
		return true
	}
	if check, ok := builtinTTSRequirements[provider]; ok {
		return check(ctx, env)
	}
	return pluginProviderIsAvailable(ctx, provider)
}

// CheckTTSRequirements reports the configured text to speech provider usable for gateway auto TTS
func CheckTTSRequirements(ctx context.Context) bool {
	return checkTTSRequirements(ctx, scopedToolEnv(ctx))
}

const configuredVoiceSentence = "Voice and provider are user-configured (built-in providers like edge/openai or custom command providers under tts.providers.<name>), not model-selected."
const elevenLabsVoiceSentence = "The provider is user-configured (ElevenLabs); pick a voice per call with voice_id, found with find_voices (one character per voice, e.g. for game NPCs)."

// Output path hint naming the audio cache under a home
func outputPathDescription(h string) string {
	return "Optional custom file path to save the audio. Defaults to " + h + "/audio_cache/<timestamp>.mp3"
}

// Static text_to_speech schema
func ttsSchema() *jsonutil.Object {
	return jsonutil.ObjectOf(
		"name", "text_to_speech",
		"description", "Convert text to speech audio. Returns a MEDIA: path that the platform delivers as native audio. Compatible providers render as a voice bubble on Telegram; otherwise audio is sent as a regular attachment. In CLI mode, saves to ~/voice-memos/. "+configuredVoiceSentence,
		"parameters", jsonutil.ObjectOf(
			"type", "object",
			"properties", jsonutil.ObjectOf(
				"text", jsonutil.ObjectOf("type", "string",
					"description", "The text to convert to speech. Provider-specific per-request character caps apply automatically (OpenAI 4096, xAI 15000, MiniMax 10000, ElevenLabs 5k-40k depending on model); longer input is split into ordered chunks without silent truncation."),
				"output_path", jsonutil.ObjectOf("type", "string", "description", outputPathDescription("the profile ICEBATH_HOME")),
				"speed", jsonutil.ObjectOf("type", "number",
					"description", "Playback speed multiplier. 1.0 = normal, 0.5 = very slow (language learning), 2.0 = fast. Range: 0.25-4.0. Overrides the speed configured in config.yaml."),
				"instructions", jsonutil.ObjectOf("type", "string",
					"description", "Optional voice-design guidance: tone, emotion, pacing, accent, whispering, impressions (e.g. 'Speak in a cheerful, excited whisper'). Forwarded to the OpenAI backend (gpt-4o-mini-tts and OpenAI-compatible voice-design servers). Silently ignored by backends that don't support it."),
			),
			"required", []any{"text"},
		),
	)
}

// Schema overrides rebuilt from the active profile, adding voice selection when ElevenLabs is configured
func ttsSchemaOverrides(env *tools.Env) *jsonutil.Object {
	ctx := context.Background()
	schema := ttsSchema()
	params := jsonutil.Clone(schema.Value("parameters")).(*jsonutil.Object)
	props := params.Value("properties").(*jsonutil.Object)
	props.Value("output_path").(*jsonutil.Object).Set("description", outputPathDescription(home.Display(ttsHome(ctx, env))))
	if !elevenLabsVoiceSelectionActive(loadTTSRequest(ctx, env)) {
		return jsonutil.ObjectOf("parameters", params)
	}
	props.Set("voice_id", jsonutil.ObjectOf("type", "string", "description", "ElevenLabs voice for this audio (a voice_id from find_voices). Omit to use the user's configured voice."))
	props.Set("find_voices", findVoicesSchema())
	params.Set("required", []any{})
	return jsonutil.ObjectOf("parameters", params, "description", strings.ReplaceAll(schema.Value("description").(string), configuredVoiceSentence, elevenLabsVoiceSentence))
}

// Optional string argument, empty for None or a non string
func ttsArgString(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// Tool handler mapping model arguments onto the synthesis call
func handleTextToSpeech(ctx context.Context, call *tools.Call) string {
	text, _ := dictGetDefault(call.Args, "text", "").(string)
	out, err := textToSpeechTool(ctx, call.Env, ttsCallArgs{Text: text, OutputPath: ttsArgString(call.Args, "output_path"), Speed: call.Args["speed"],
		Instructions: ttsArgString(call.Args, "instructions"), VoiceID: ttsArgString(call.Args, "voice_id"), FindVoices: call.Args["find_voices"]})
	if err != nil {
		return tools.ErrorResult(err)
	}
	return out
}

// Registers text_to_speech in the tts toolset
func registerTextToSpeechTool() {
	tools.Register(tools.Def{
		Name: "text_to_speech", Toolset: "tts", Schema: ttsSchema(), Handler: handleTextToSpeech,
		Check: func(env *tools.Env) bool { return checkTTSRequirements(context.Background(), env) }, RequiresEnv: []string{},
		Emoji: "🔊", DynamicSchema: ttsSchemaOverrides,
	})
}
