package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultCommandSTTTimeoutSeconds = 300
	defaultCommandSTTLanguage       = "en"
	defaultCommandSTTOutputFormat   = "txt"
	whisperPromptTokenCap           = 224
)

var (
	commandSTTOutputFormats       = sttSet("txt", "json", "srt", "vtt")
	nonCommandSTTNames            = sttSet("local", "local_command", "groq", "openai", "mistral", "xai", "elevenlabs", "deepinfra", "none")
	preTranscriptionMutableFields = sttSet("prompt", "language", "model")
	whisperPromptCappedProviders  = sttSet("local", "openai", "groq", "deepinfra")
	sttPromptCharsPerToken        = int(tools.CharsPerToken)
	runCommandSTT                 = runCommandProvider
	sttHasHook                    = plugins.HasHook
	sttInvokeHook                 = plugins.InvokeHook
	sttEnsurePluginsDiscovered    = func(ctx context.Context, force bool) { ensurePluginsDiscoveredHook(ctx, force) }
)

// stt.providers entry of a name, else stt.<name> for non built in names
func getNamedSTTProviderConfig(sttConfig any, name string) map[string]any {
	return namedProviderConfig(sttConfig, name, builtinSTTProviders)
}

// Command type provider config of a name, nil for built ins, none and unknown names
func resolveCommandSTTProviderConfig(providerName string, sttConfig any) map[string]any {
	return resolveCommandConfig(providerName, sttConfig, nonCommandSTTNames)
}

// Python RuntimeError raised by the command STT flow
type sttRuntimeError struct{ msg string }

func (e *sttRuntimeError) Error() string { return e.msg }

// Transcript from a non empty output file, else non empty stdout, else a RuntimeError
func readCommandSTTOutput(outputPath, stdout string) (string, error) {
	content := ""
	if raw, err := os.ReadFile(outputPath); err == nil {
		s, _ := pycodecs.DecodeReplace(raw, "utf-8-sig")
		content = pystr.Strip(s)
	}
	if content != "" {
		return content, nil
	}
	if s := pystr.Strip(stdout); s != "" {
		return s, nil
	}
	return "", &sttRuntimeError{msg: fmt.Sprintf("Command STT provider wrote no output file at %s and produced no stdout", outputPath)}
}

// Absolute path with symlinks resolved as far as they exist like Path.resolve
func sttResolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// File handed to a command provider, transcoded to 16 kHz mono when normalize is on, or the error text
func normalizeCommandSTTInput(audio, tmpdir, providerName string, cfg map[string]any) (string, string) {
	if !isTruthyValue(cfg["normalize"], false) {
		return sttResolvePath(audio), ""
	}
	converted, transcodeError := transcodeAudioForSTT(sttResolvePath(audio), tmpdir)
	if transcodeError != "" || converted == "" {
		if transcodeError == "" {
			transcodeError = "unknown ffmpeg failure"
		}
		return "", fmt.Sprintf("stt.providers.%s.normalize: true but the audio could not be normalized for the command provider: %s", providerName, transcodeError)
	}
	sttLog().Info(fmt.Sprintf("Normalized %s to 16 kHz mono for command STT provider '%s'", filepath.Base(audio), providerName))
	return converted, ""
}

// Home relative path expansion like Path.expanduser
func sttExpandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

// Transcription through a user declared stt.providers command with quote aware placeholders
func transcribeCommandSTT(ctx context.Context, filePath, providerName string, cfg, sttConfig map[string]any, modelOverride, languageOverride, prompt string) map[string]any {
	if prompt != "" {
		logPromptUnsupported(fmt.Sprintf("Command STT provider '%s'", providerName))
	}
	fail := func(message string) map[string]any { return sttError(message, "provider", providerName) }
	template := ""
	if v := cfg["command"]; pyTruthy(v) {
		template = pystr.Strip(pyrepr.Str(v))
	}
	if template == "" {
		return fail(fmt.Sprintf("stt.providers.%s.command is not configured", providerName))
	}
	audio := sttExpandUser(filePath)
	if _, err := os.Stat(audio); err != nil {
		return fail("Audio file not found: " + filePath)
	}
	timeout := commandTimeout(cfg, defaultCommandSTTTimeoutSeconds)
	outputFormat := commandOutputFormat(cfg, commandSTTOutputFormats, defaultCommandSTTOutputFormat)
	var language any = languageOverride
	if languageOverride == "" {
		language = cfg["language"]
		if !pyTruthy(language) {
			language = resolveSTTLanguage(ctx, providerName, sttConfig)
		}
		if !pyTruthy(language) {
			language = defaultCommandSTTLanguage
		}
	}
	model := modelOverride
	if model == "" {
		if v := cfg["model"]; pyTruthy(v) {
			model = pyrepr.Str(v)
		}
	}
	transcript, err := func() (string, error) {
		tmpdir, err := os.MkdirTemp("", "icebath-cmd-stt-"+providerName+"-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmpdir)
		inputPath, normalizeError := normalizeCommandSTTInput(audio, tmpdir, providerName, cfg)
		if normalizeError != "" {
			return "", &sttNormalizeError{msg: normalizeError}
		}
		outputPath := filepath.Join(tmpdir, "transcript."+outputFormat)
		command := renderCommandTemplate(template, map[string]string{"input_path": inputPath, "output_path": outputPath,
			"output_dir": filepath.Dir(outputPath), "format": outputFormat, "language": pyrepr.Str(language), "model": model})
		sttLog().Info(fmt.Sprintf("Transcribing %s via command STT provider '%s'...", filepath.Base(audio), providerName))
		result, err := runCommandSTT(ctx, nil, command, timeout, commandEnvPassthrough(cfg))
		if err != nil {
			return "", err
		}
		return readCommandSTTOutput(outputPath, result.Stdout)
	}()
	var ne *sttNormalizeError
	var te *commandTimeoutError
	var ee *commandExitError
	var re *sttRuntimeError
	switch {
	case err == nil:
	case errors.As(err, &ne):
		return fail(ne.msg)
	case errors.As(err, &te):
		g, _ := pyrepr.Format(timeout, "g")
		return fail(fmt.Sprintf("STT command provider '%s' timed out after %ss", providerName, g))
	case errors.As(err, &ee):
		return fail(fmt.Sprintf("STT command provider '%s' exited with code %d: %s", providerName, ee.Returncode, commandFailureDetail(ee)))
	case errors.As(err, &re):
		return fail(re.msg)
	default:
		return fail(fmt.Sprintf("STT command provider '%s' failed: %s", providerName, sttOSErrorText(err)))
	}
	sttLog().Info(fmt.Sprintf("Transcribed %s via command STT provider '%s' (%d chars)", filepath.Base(audio), providerName, utf8.RuneCountInString(transcript)))
	return sttOK(transcript, providerName)
}

// Normalization failure the command flow returns as its own envelope
type sttNormalizeError struct{ msg string }

func (e *sttNormalizeError) Error() string { return e.msg }

// Python OSError text of a filesystem failure, the error text otherwise
func sttOSErrorText(err error) string {
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

// Envelope naming a configured provider no backend registered
func unregisteredSTTProviderError(providerName string) map[string]any {
	key := pystr.Strip(providerName)
	return sttError(fmt.Sprintf("stt.provider='%s' is set but no built-in, command, or plugin provider registered that name. "+
		"Run `icebath plugins list` to see installed STT plugins, or configure a command provider under `stt.providers.%s.command`.", key, key),
		"provider", key, "error_type", "provider_not_registered")
}

// Plugin transcription of a provider name, nil when no plugin claims it
func dispatchToSTTPluginProvider(ctx context.Context, filePath, providerName string, sttConfig map[string]any, model, language any, prompt string) map[string]any {
	key := pystr.Strip(pystr.Lower(providerName))
	if key == "" || nonCommandSTTNames[key] {
		return nil
	}
	if sttConfig != nil && isCommandProviderConfig(getNamedSTTProviderConfig(sttConfig, key)) {
		return nil
	}
	var p TranscriptionProvider
	if err := pluginCall(func() error {
		sttEnsurePluginsDiscovered(ctx, false)
		p = GetTranscriptionProvider(ctx, key, "")
		if p == nil {
			sttEnsurePluginsDiscovered(ctx, true)
			p = GetTranscriptionProvider(ctx, key, "")
		}
		return nil
	}); err != nil {
		sttLog().Debug(fmt.Sprintf("STT plugin dispatch skipped (discovery failed): %v", err))
		return nil
	}
	if p == nil {
		return nil
	}
	available := false
	if err := pluginCall(func() error { available = p.IsAvailable(); return nil }); err != nil {
		sttLog().Warn(fmt.Sprintf("STT plugin provider '%s' is_available() raised: %v — treating as unavailable", key, err))
		available = false
	}
	if !available {
		sttLog().Info(fmt.Sprintf("STT plugin provider '%s' reports not available; returning unavailability envelope.", key))
		return sttError(fmt.Sprintf("STT plugin '%s' is not available — check that its required credentials / dependencies are configured.", key), "provider", key)
	}
	sttLog().Info(fmt.Sprintf("Transcribing with plugin STT provider '%s'...", key))
	opts := TranscribeOptions{Model: model, Language: language, Extra: map[string]any{}}
	if prompt != "" {
		opts.Extra["prompt"] = prompt
	}
	var result any
	err := pluginCall(func() error {
		var err error
		result, err = p.Transcribe(ctx, filePath, opts)
		return err
	})
	if err != nil {
		sttLog().Warn(fmt.Sprintf("STT plugin provider '%s' raised: %v", key, err))
		return sttError(fmt.Sprintf("STT plugin '%s' raised: %v", key, err), "provider", key)
	}
	out, ok := result.(map[string]any)
	if !ok {
		return sttError(fmt.Sprintf("STT plugin '%s' returned a non-dict result", key), "provider", key)
	}
	if _, ok := out["provider"]; !ok {
		out["provider"] = key
	}
	return out
}

// Prompt truncated to the whisper family token cap keeping the tail, other providers unchanged
func enforcePromptLengthLimit(prompt, providerName string) string {
	maxChars := whisperPromptTokenCap * sttPromptCharsPerToken
	n := utf8.RuneCountInString(prompt)
	if prompt == "" || !whisperPromptCappedProviders[providerName] || n <= maxChars {
		return prompt
	}
	sttLog().Warn(fmt.Sprintf("Transcription prompt is ~%d tokens; whisper-family provider '%s' only uses the final ~%d — truncating to the last %d characters.",
		n/sttPromptCharsPerToken, providerName, whisperPromptTokenCap, maxChars))
	r := []rune(prompt)
	return string(r[len(r)-maxChars:])
}

// None for an unset optional string hint
func sttNone(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Fires pre_transcription hooks and returns the model, language override and prompt, fail open on any error
func applyPreTranscriptionHook(ctx context.Context, filePath, providerName, model string, language any, prompt, source string) (string, string, string) {
	outModel, outLanguage, outPrompt := model, "", prompt
	err := pluginCall(func() error {
		if !sttHasHook(ctx, "pre_transcription") {
			return nil
		}
		results := sttInvokeHook(ctx, "pre_transcription", map[string]any{"file_path": filePath, "provider": providerName,
			"model": sttNone(model), "language": language, "prompt": sttNone(prompt), "source": sttNone(source)})
		overrides := map[string]string{}
		for _, r := range results {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			for _, k := range sttOrderedKeys(m) {
				v := m[k]
				switch {
				case k == "file_path":
					sttLog().Warn("pre_transcription hook attempted to change file_path (read-only) — ignoring the attempt.")
				case !preTranscriptionMutableFields[k]:
					sttLog().Debug(fmt.Sprintf("pre_transcription hook returned unsupported field %s — ignoring.", pyrepr.Repr(k)))
				default:
					s, ok := v.(string)
					if !ok {
						sttLog().Debug(fmt.Sprintf("pre_transcription hook returned non-string value %s for field %s — ignoring.", pyrepr.Repr(v), pyrepr.Repr(k)))
						continue
					}
					overrides[k] = s
				}
			}
		}
		if p, ok := overrides["prompt"]; ok {
			outPrompt = p
		}
		if m, ok := overrides["model"]; ok {
			outModel = m
		}
		outLanguage = overrides["language"]
		return nil
	})
	if err != nil {
		sttLog().Debug(fmt.Sprintf("pre_transcription hook error: %v", err))
		return model, "", prompt
	}
	return outModel, outLanguage, outPrompt
}

// Mapping keys in sorted order so hook field handling is deterministic
func sttOrderedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
