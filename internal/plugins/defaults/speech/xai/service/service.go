// Grok's voices and transcription, through the xAI key or the Grok sign in
package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/xai/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What the credentials of xAI answer when none resolve
const noCredentials = "xAI needs a sign-in or an API key. Sign in to xAI in /providers, or set XAI_API_KEY in your keys file."

// Speeds xAI takes
const (
	slowest = 0.7
	fastest = 1.5
)

// Agent xAI is told
var agent = provider.UserAgent("Icebath-Agent")

func Register(registry *speech.Registry) {
	setup := speech.Setup{Keys: []string{config.KeyEnv}, SignIn: web.XAIOAuth, Link: "https://console.x.ai"}
	speaks := func(job *speech.Job) bool { _, ok := job.XAI(true); return ok }
	registry.Register(speech.Services{
		Speakers: []speech.Voice{{
			Name: config.Name, Title: "xAI TTS", About: "Grok's voices, needs an xAI API key or a Grok sign-in",
			Ready:      speaks,
			Limit:      func(*speech.Job) int { return config.DefaultLimit },
			Synthesize: synthesize,
			Stream:     &speech.Stream{Rank: 4, Label: "xAI streaming TTS", Ready: speaks, Open: stream},
			Opus:       speech.TranscodedOpus,
			Setup:      setup,
		}},
		Recognizing: []speech.Recognizer{{
			Name: config.Name, Title: "xAI", About: "Grok's speech to text, needs an xAI API key or a Grok sign-in", Cloud: true, Rank: 6,
			Label: "xAI Grok STT", Detected: "xAI Grok STT API", Warning: "STT provider 'xai' configured but no xAI credentials are available",
			Missing: func(_ context.Context, job *speech.Job) string {
				if _, _, ok := listener(job); !ok {
					return noCredentials
				}
				return ""
			},
			Model: func(job *speech.Job, model string) string {
				return Model(strs.Filled(model, job.Text(config.STTModel, "")))
			},
			Language:   func(job *speech.Job) string { return job.Text(config.STTLanguage, "") },
			Limit:      func(string) speech.Limit { return speech.Limit{Bytes: 500 << 20} },
			Transcribe: transcribe,
			Live:       live,
			Setup:      setup,
			Choices:    []speech.Choices{speech.Fixed(config.STTModel, config.DefaultSTTModel, "grok-voice-transcribe-1.0")},
		}},
	})
}

// Model of xAI recognition, a blank name or the retired grok-stt taking the default
func Model(model string) string {
	if model = strings.TrimSpace(model); model == "" || model == "grok-stt" {
		return config.DefaultSTTModel
	}
	return model
}

// Tags of xAI speech: those said in place and those wrapped around words
var (
	inline   = []string{"pause", "long-pause", "hum-tune", "laugh", "chuckle", "giggle", "cry", "tsk", "tongue-click", "lip-smack", "breath", "inhale", "exhale", "sigh"}
	wrapping = []string{"soft", "whisper", "loud", "build-intensity", "decrease-intensity", "higher-pitch", "lower-pitch", "slow", "fast", "sing-song", "singing",
		"laugh-speak", "emphasis"}
)

var (
	speechTag      = regexp.MustCompile(`(?i)(\[(?:` + strings.Join(inline, "|") + `)\]|</?(?:` + strings.Join(wrapping, "|") + `)>)`)
	firstSentence  = regexp2.MustCompile(`(?s)^(.{12,120}?[.!?…])\s+(?=\S)`, regexp2.None)
	paragraphBreak = regexp.MustCompile(`\n\s*\n+`)
	lineBreak      = regexp.MustCompile(`\s*\n\s*`)
	multiSpace     = regexp.MustCompile(`\s{2,}`)
)

// Text with pauses tagged where paragraphs break and after the first sentence, then rewritten by the speech tags model with expressive
// tags when it carried none of its own
func tagged(ctx context.Context, job *speech.Job, text string) string {
	clean := strings.TrimSpace(text)
	if clean == "" {
		return text
	}
	local := lineBreak.ReplaceAllString(paragraphBreak.ReplaceAllString(clean, " [pause] "), " ")
	if !speechTag.MatchString(local) {
		if replaced, err := firstSentence.Replace(local, "$1 [pause] ", -1, 1); err == nil {
			local = replaced
		}
	}
	local = strings.TrimSpace(multiSpace.ReplaceAllString(local, " "))
	if speechTag.MatchString(clean) {
		return local
	}
	system := "You rewrite transcripts for the xAI /v1/tts endpoint by inserting expressive speech tags.\n\n" +
		"Valid inline tags (use as `[tag]`): " + strings.Join(inline, ", ") + ".\n" +
		"Valid wrapping tags (use as `[tag]...[/tag]`): " + strings.Join(wrapping, ", ") + ".\n\n" +
		speech.TagRules("- Use inline `[tag]` for short modifiers (laughs, sighs, pause, etc.).\n"+
			"- Use wrapping `[tag]...[/tag]` for sustained effects (whisper, soft, slow, fast, loud, etc.).\n"+
			"- Do not use angle-bracket tags like `<tag>...</tag>` — xAI uses BBCode-style closing tags with `[/tag]`.\n"+
			"- Do not use SSML.\n")
	return job.Tagged(ctx, system, "TRANSCRIPT TO TAG:\n"+local, local, "xAI TTS", "locally-tagged text", slog.LevelDebug)
}

// Speech through xAI's tts endpoint, MP3 unless the path asks for WAV
func synthesize(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
	bearer, ok := job.XAI(true)
	if !ok || bearer.Key == "" {
		return "", speech.Misconfigured(noCredentials)
	}
	text := request.Text
	if job.Config.GetBool(config.AutoSpeechTags) {
		text = tagged(ctx, job, text)
	}
	speed, set := job.Speed(request, config.Speed)
	speed = math.Max(slowest, math.Min(fastest, speed))
	var base string
	switch {
	case bearer.Source == web.XAIOAuth:
		base = bearer.BaseURL
	case job.Text(config.TTSBaseURL, "") != "":
		base = job.Text(config.TTSBaseURL, "")
	default:
		base = strs.Filled(bearer.BaseURL, job.Getenv("XAI_BASE_URL"))
	}
	base = strings.TrimRight(strs.Filled(strings.TrimSpace(base), config.DefaultBase), "/")
	codec := "mp3"
	if strings.HasSuffix(request.Path, ".wav") {
		codec = "wav"
	}
	rate, bits := job.Config.GetInt(config.SampleRate), job.Config.GetInt(config.BitRate)
	payload := map[string]any{"text": text, "voice_id": job.Text(config.VoiceID, config.DefaultVoice), "language": job.Text(config.TTSLanguage, config.DefaultLanguage)}
	if codec != "mp3" || rate != config.DefaultRate || bits != config.DefaultBitRate {
		format := map[string]any{"codec": codec}
		if rate != 0 {
			format["sample_rate"] = rate
		}
		if codec == "mp3" && bits != 0 {
			format["bit_rate"] = bits
		}
		payload["output_format"] = format
	}
	if set && speed != 1 {
		payload["speed"] = speed
	}
	if boost, ok := latency(job); ok && boost != 0 {
		payload["optimize_streaming_latency"] = boost
	}
	if job.Config.GetBool(config.TextNormalization) {
		payload["text_normalization"] = true
	}
	response, err := speech.PostJSON(ctx, base+"/tts", payload, map[string]string{"Authorization": "Bearer " + bearer.Key, "User-Agent": agent})
	if err != nil {
		return "", err
	}
	if err := speech.Failed(response); err != nil {
		return "", err
	}
	return speech.Save(response, request.Path, "xAI TTS")
}

// Latency boost of xAI speech, 0 to 2: its own setting, else the general one; false when neither is set
func latency(job *speech.Job) (int, bool) {
	for _, key := range []string{config.Latency, speechconfig.Latency} {
		if job.Set(key) {
			return max(0, min(2, job.Config.GetInt(key))), true
		}
	}
	return 0, false
}

// Bearer of xAI recognition and where it goes: XAI_API_KEY of the environment at XAI_BASE_URL, else the Grok sign in first
func listener(job *speech.Job) (web.XAI, string, bool) {
	if key := job.Getenv(config.KeyEnv); key != "" {
		base := strings.TrimRight(strs.Filled(job.Getenv("XAI_BASE_URL"), config.DefaultBase), "/")
		return web.XAI{Key: key, BaseURL: base, Source: "xai"}, base, true
	}
	bearer, ok := job.XAI(false)
	return bearer, bearer.BaseURL, ok && bearer.Key != ""
}

// Address of recognition: the sign in's own, else stt.xai.base_url, else the bearer's, else xAI's
func address(job *speech.Job, bearer web.XAI) string {
	base := bearer.BaseURL
	if bearer.Source != web.XAIOAuth {
		base = strs.Filled(job.Text(config.STTBaseURL, ""), base)
	}
	return strings.TrimRight(strs.Filled(strings.TrimSpace(base), config.DefaultBase), "/")
}

func transcribe(ctx context.Context, job *speech.Job, path string, hints speech.Hints) speech.Transcript {
	r := speech.Recognizer{Name: config.Name}
	if hints.Prompt != "" {
		speech.PromptIgnored("STT provider 'xai'")
	}
	bearer, _, ok := listener(job)
	if !ok {
		return r.Failure(noCredentials)
	}
	parts := []speech.Part{{Name: "model", Value: hints.Model}}
	if hints.Language != "" {
		parts = append(parts, speech.Part{Name: "language", Value: hints.Language})
		if job.Config.GetBool(config.Format) {
			parts = append(parts, speech.Part{Name: "format", Value: "true"})
		}
	}
	if job.Config.GetBool(config.Diarize) {
		parts = append(parts, speech.Part{Name: "diarize", Value: "true"})
	}
	parts = append(parts, speech.Part{Name: "file", File: path})
	send := func(b web.XAI) (*http.Response, error) {
		return speech.Upload(ctx, address(job, b)+"/stt", parts, map[string]string{"Authorization": "Bearer " + b.Key, "User-Agent": agent}, 120*time.Second)
	}
	response, err := send(bearer)
	if err == nil && (response.StatusCode == 401 || response.StatusCode == 403) && bearer.Source == web.XAIOAuth {
		status := response.StatusCode
		slog.Info(fmt.Sprintf("xAI STT got HTTP %d; refreshing OAuth credentials and retrying once", status))
		if rerr := job.RenewXAI(ctx); rerr != nil {
			slog.Warn(fmt.Sprintf("xAI STT OAuth refresh-and-retry after HTTP %d failed: %v", status, rerr))
		} else if renewed, ok := job.XAI(false); ok && renewed.Key != "" && renewed.Key != bearer.Key {
			if retried, rerr := send(renewed); rerr == nil {
				response.Body.Close()
				response = retried
			} else {
				slog.Warn(fmt.Sprintf("xAI STT OAuth refresh-and-retry after HTTP %d failed: %v", status, rerr))
			}
		}
	}
	detail := func(body map[string]any) string {
		if failure, ok := body["error"].(map[string]any); ok {
			if message, ok := failure["message"]; ok {
				return speech.Text(message)
			}
		}
		return ""
	}
	text := func(body map[string]any) (string, error) {
		value, ok := body["text"]
		if !ok {
			return "", nil
		}
		s, isText := value.(string)
		if !isText {
			return "", fmt.Errorf("xAI transcription text is %s where text was expected", speech.Kind(value))
		}
		return strings.TrimSpace(s), nil
	}
	transcript, body := r.Replied(response, err, path, "xAI STT", detail, text)
	if transcript.Success {
		language := any(hints.Language)
		if value, ok := body["language"]; ok {
			language = value
		}
		duration, _ := body["duration"].(float64)
		slog.Info(fmt.Sprintf("Transcribed %s via xAI Grok STT (lang=%s, %.1fs audio, %d chars)", filepath.Base(path), speech.Text(language), duration,
			utf8.RuneCountInString(transcript.Text)))
	}
	return transcript
}
