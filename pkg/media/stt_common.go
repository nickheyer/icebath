package media

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

const (
	sttDefaultProvider      = "local"
	sttDefaultLocalModel    = "base"
	sttDefaultLocalLanguage = "en"
	sttDefaultTimeout       = 60.0
	xaiSTTModelEnv          = "STT_XAI_MODEL"
	legacyXAISTTModel       = "grok-stt"
	localSTTCommandEnv      = "ICEBATH_LOCAL_STT_COMMAND"
	localSTTLanguageEnv     = "ICEBATH_LOCAL_STT_LANGUAGE"
	sttMaxFileSize          = 25 * 1024 * 1024
)

// Environment value read once at import, the default when the variable is unset
func sttEnvDefault(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

var (
	sttDefaultOpenAIModel     = sttEnvDefault("STT_OPENAI_MODEL", "whisper-1")
	sttDefaultGroqModel       = sttEnvDefault("STT_GROQ_MODEL", "whisper-large-v3-turbo")
	sttDefaultMistralModel    = sttEnvDefault("STT_MISTRAL_MODEL", "voxtral-mini-latest")
	sttDefaultElevenLabsModel = sttEnvDefault("STT_ELEVENLABS_MODEL", "scribe_v2")
	groqSTTBaseURL            = sttEnvDefault("GROQ_BASE_URL", "https://api.groq.com/openai/v1")
	openAISTTBaseURL          = sttEnvDefault("STT_OPENAI_BASE_URL", "https://api.openai.com/v1")
	xaiSTTBaseURL             = sttEnvDefault("XAI_STT_BASE_URL", "https://api.x.ai/v1")
	elevenLabsSTTBaseURL      = sttEnvDefault("ELEVENLABS_STT_BASE_URL", "https://api.elevenlabs.io/v1")

	sttCommonLocalBinDirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}
	sttSupportedFormats   = map[string]bool{".mp3": true, ".mp4": true, ".mpeg": true, ".mpga": true, ".m4a": true, ".wav": true,
		".webm": true, ".ogg": true, ".oga": true, ".opus": true, ".aac": true, ".flac": true, ".caf": true}
	sttLocalNativeAudioFormats = map[string]bool{".wav": true, ".aiff": true, ".aif": true}

	sttModelCatalog = map[string][]string{
		"local":      {"base", "tiny", "small", "medium", "large-v3", "turbo"},
		"groq":       {"whisper-large-v3-turbo", "whisper-large-v3"},
		"openai":     {"whisper-1", "gpt-4o-mini-transcribe", "gpt-4o-transcribe", "gpt-transcribe"},
		"mistral":    {"voxtral-mini-latest", "voxtral-mini-2602"},
		"xai":        {"grok-voice-transcribe-2.0", "grok-voice-transcribe-1.0"},
		"elevenlabs": {"scribe_v2", "scribe_v1"},
	}
	sttModelConfigKey = map[string]string{"elevenlabs": "model_id"}

	sttOpenAIModels      = sttSet(sttModelCatalog["openai"]...)
	sttGroqModels        = sttSet(sttModelCatalog["groq"]...)
	sttRetiredGroqModels = sttSet("distil-whisper-large-v3-en")

	builtinSTTProviders = sttSet("local", "local_command", "groq", "openai", "mistral", "xai", "elevenlabs", "deepinfra")
	cloudSTTProviders   = sttSet("groq", "openai", "mistral", "xai", "elevenlabs", "deepinfra")
)

// Membership set of the given names
func sttSet(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// Logger shared by every STT module under the origin module name
func sttLog() *slog.Logger { return slog.Default().With("logger", "tools.transcription_tools") }

// Failure envelope with optional extra key value pairs
func sttError(message string, extra ...any) map[string]any {
	out := map[string]any{"success": false, "transcript": "", "error": message}
	for i := 0; i+1 < len(extra); i += 2 {
		out[extra[i].(string)] = extra[i+1]
	}
	return out
}

// Success envelope naming the provider
func sttOK(transcript, provider string) map[string]any {
	return map[string]any{"success": true, "transcript": transcript, "provider": provider}
}

// Structured provider reply without transcript text, carrying the provider message verbatim
type sttResponseError struct{ msg string }

func (e *sttResponseError) Error() string { return e.msg }

// Valid xAI model with blank or the grok-stt alias resolved through STT_XAI_MODEL then the catalog default
func normalizeXAISTTModel(model string) string {
	value := pystr.Strip(model)
	if value == "" || value == legacyXAISTTModel {
		if env := pystr.Strip(os.Getenv(xaiSTTModelEnv)); env != "" {
			return env
		}
		return sttModelCatalog["xai"][0]
	}
	return value
}

// pm lazy install of an extra, the seam tests disable
var sttEnsureImport = pm.EnsureImport

// Offers the pm extra and swallows every failure
func lazyEnsureQuietly(ctx context.Context, extra string) {
	_ = pluginCall(func() error { return sttEnsureImport(ctx, extra) })
}

// Helper binary exited nonzero, outputs kept as string, bytes or nil like CalledProcessError
type sttProcessError struct {
	Cmd        []string
	ReturnCode int
	Stdout     any
	Stderr     any
}

func (e *sttProcessError) Error() string {
	cmd := make([]any, len(e.Cmd))
	for i, c := range e.Cmd {
		cmd[i] = c
	}
	return fmt.Sprintf("Command '%s' returned non-zero exit status %d.", pyrepr.Repr(cmd), e.ReturnCode)
}

// Helper binary outlived its timeout
type sttTimeoutError struct {
	Cmd     []string
	Timeout float64
}

func (e *sttTimeoutError) Error() string {
	cmd := make([]any, len(e.Cmd))
	for i, c := range e.Cmd {
		cmd[i] = c
	}
	return fmt.Sprintf("Command '%s' timed out after %s seconds", pyrepr.Repr(cmd), pyrepr.Str(e.Timeout))
}

// Stderr then stdout then the exception text of a failed helper binary
func processErrorDetail(e *sttProcessError) string {
	for _, output := range []any{e.Stderr, e.Stdout} {
		var detail string
		switch v := output.(type) {
		case []byte:
			s, _ := pycodecs.DecodeReplace(v, "utf-8")
			detail = pystr.Strip(s)
		case nil:
			detail = ""
		default:
			detail = pystr.Strip(pyrepr.Str(v))
		}
		if detail != "" {
			return detail
		}
	}
	return e.Error()
}

// Debug record that a backend ignores transcription prompts
func logPromptUnsupported(label string) {
	sttLog().Debug(fmt.Sprintf("%s does not support transcription prompts — proceeding without the prompt.", label))
}

// Config value cast to float, the default on a missing or bad value
func configNumberFloat(cfg map[string]any, key string, def float64) float64 {
	v, ok := cfg[key]
	if !ok {
		return def
	}
	if f, ok := sttCastFloat(v); ok {
		return f
	}
	return def
}

// Config value cast to int like Python int(), the default on a missing or bad value
func configNumberInt(cfg map[string]any, key string, def int) int {
	v, ok := cfg[key]
	if !ok {
		return def
	}
	switch x := v.(type) {
	case bool:
		if x {
			return 1
		}
		return 0
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(strings.ReplaceAll(x, "_", ""))); err == nil {
			return n
		}
	}
	return def
}

// Python float() of a config scalar
func sttCastFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(x, "_", "")), 64)
		return f, err == nil
	}
	return 0, false
}

// The stt section of the effective profile config, empty when unreadable, the seam tests replace
var loadSTTConfig = func(ctx context.Context) map[string]any {
	tree, err := config.LoadContext(ctx, home.Dir(ctx), config.Lookup(os.LookupEnv))
	if err != nil {
		return map[string]any{}
	}
	sec, ok := asStrMap(tree["stt"])
	if !ok || !pyTruthy(tree["stt"]) {
		return map[string]any{}
	}
	return sec
}
