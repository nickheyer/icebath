package media

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
)

const (
	sttDefaultProvider      = "local"
	sttDefaultLocalModel    = "base"
	sttDefaultLocalLanguage = "en"
	sttDefaultTimeout       = 60.0
	legacyXAISTTModel       = "grok-stt"
	localSTTCommandEnv      = "ICEBATH_LOCAL_STT_COMMAND"
	localSTTLanguageEnv     = "ICEBATH_LOCAL_STT_LANGUAGE"
	sttMaxFileSize          = 25 * 1024 * 1024
	// Endpoints a recognition service is reached at when its base_url setting is unset
	groqSTTBaseURL       = "https://api.groq.com/openai/v1"
	openAISTTBaseURL     = "https://api.openai.com/v1"
	xaiSTTBaseURL        = "https://api.x.ai/v1"
	elevenLabsSTTBaseURL = "https://api.elevenlabs.io/v1"
)

var (
	sttDefaultOpenAIModel     = "whisper-1"
	sttDefaultGroqModel       = "whisper-large-v3-turbo"
	sttDefaultMistralModel    = "voxtral-mini-latest"
	sttDefaultElevenLabsModel = "scribe_v2"

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

// Valid xAI model with blank or the grok-stt alias resolved to the catalog default
func normalizeXAISTTModel(model string) string {
	value := strings.TrimSpace(model)
	if value == "" || value == legacyXAISTTModel {
		return sttModelCatalog["xai"][0]
	}
	return value
}

// Helper binary exited nonzero, outputs kept as string, bytes or nil like CalledProcessError
type sttProcessError struct {
	Cmd        []string
	ReturnCode int
	Stdout     any
	Stderr     any
}

func (e *sttProcessError) Error() string {
	return fmt.Sprintf("%s exited with status %d", strings.Join(e.Cmd, " "), e.ReturnCode)
}

// Helper binary outlived its timeout
type sttTimeoutError struct {
	Cmd     []string
	Timeout float64
}

func (e *sttTimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %s seconds", strings.Join(e.Cmd, " "), errchain.Show(e.Timeout))
}

// Stderr then stdout then the exception text of a failed helper binary
func processErrorDetail(e *sttProcessError) string {
	for _, output := range []any{e.Stderr, e.Stdout} {
		var detail string
		switch v := output.(type) {
		case []byte:
			s, _ := charsetutil.DecodeReplace(v, "utf-8")
			detail = strings.TrimSpace(s)
		case nil:
			detail = ""
		default:
			detail = strings.TrimSpace(anyutil.String(v))
		}
		if detail != "" {
			return detail
		}
	}
	return e.Error()
}

// Debug record that a backend ignores transcription prompts
func logPromptUnsupported(label string) {
	pkgLog().Debug(fmt.Sprintf("%s does not support transcription prompts — proceeding without the prompt.", label))
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
