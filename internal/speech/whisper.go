package speech

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Sizes of the Whisper model a local engine runs, the first the default
var WhisperModels = []string{"base", "tiny", "small", "medium", "large-v3", "turbo"}

// Model names only cloud services know, which a local engine can't run
var cloudModels = strs.Set("whisper-1", "gpt-4o-mini-transcribe", "gpt-4o-transcribe", "gpt-transcribe", "whisper-large-v3-turbo", "whisper-large-v3",
	"distil-whisper-large-v3-en")

// Model a local Whisper engine runs: base for none, and for a cloud only name with a warning
func WhisperModel(model string) string {
	if model == "" {
		return WhisperModels[0]
	}
	if cloudModels[model] {
		slog.Warn(fmt.Sprintf("STT model '%s' is a cloud-only name and cannot be used with the local provider. Falling back to '%s'. Set stt.local.model to a "+
			"valid faster-whisper size (%s).", model, WhisperModels[0], strings.Join(WhisperModels, ", ")))
		return WhisperModels[0]
	}
	return model
}

// Names of Chinese a person may write for its code
var languageAliases = map[string]string{"繁體中文": "zh", "繁体中文": "zh", "简体中文": "zh", "簡體中文": "zh"}

// Whisper codes of three letters
var longCodes = strs.Set("haw", "yue")

var fold = cases.Fold()

// Whisper code of a language hint such as en-US, empty with a warning when it is not a code, or not one of those supported when they are
// known
func WhisperLanguage(language string, supported []string) string {
	raw := strings.TrimSpace(language)
	if raw == "" {
		return ""
	}
	candidate, aliased := languageAliases[raw]
	if !aliased {
		candidate = strings.SplitN(strings.ReplaceAll(fold.String(raw), "_", "-"), "-", 2)[0]
	}
	letters := candidate != "" && !strings.ContainsFunc(candidate, func(r rune) bool { return r >= utf8.RuneSelf || !unicode.IsLetter(r) })
	if !letters || utf8.RuneCountInString(candidate) != 2 && !longCodes[candidate] {
		slog.Warn(fmt.Sprintf("Local STT language %q is not a language code; using fallback", raw))
		return ""
	}
	if supported != nil && !slices.ContainsFunc(supported, func(code string) bool { return fold.String(code) == candidate }) {
		slog.Warn(fmt.Sprintf("Local STT language %q is unsupported; using fallback", raw))
		return ""
	}
	return candidate
}
