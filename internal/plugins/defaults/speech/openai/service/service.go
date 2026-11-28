// OpenAI's speech and transcription, and those of the servers that copy its audio API
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	configapi "github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/openai/config"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Where an OpenAI key is made
const link = "https://platform.openai.com/api-keys"

// What a missing key says: the one place a key of OpenAI's speech is kept, the keys file
const noKey = "OpenAI needs an API key. Give it one in Settings › Voice › %s › OpenAI, which keeps it in your keys file as " + speech.OpenAIAudioKey + "."

// Models of Groq's catalog, retired ones included, swapped for OpenAI's default
var groqModels = strs.Set("whisper-large-v3-turbo", "whisper-large-v3", "distil-whisper-large-v3-en")

// Longest audio one transcription of each model takes, in seconds
var modelSeconds = map[string]float64{"gpt-4o-transcribe": 450, "gpt-4o-mini-transcribe": 450, "whisper-1": 600}

func Register(registry *speech.Registry) {
	setup := speech.Setup{Keys: []string{speech.OpenAIAudioKey, "OPENAI_API_KEY"}, SignIn: "openai-api", Link: link, Badge: "paid"}
	registry.Register(speech.Services{
		Speakers: []speech.Voice{{
			Name: config.Name, Title: "OpenAI TTS", About: "Natural voices, needs an OpenAI API key",
			Ready:      func(job *speech.Job) bool { _, _, err := speakRoute(job); return err == nil },
			Limit:      func(*speech.Job) int { return config.DefaultLimit },
			Synthesize: synthesize,
			Stream: &speech.Stream{Rank: 3, Label: "OpenAI streaming TTS", Ready: func(job *speech.Job) bool { return streamKey(job) != "" },
				Open: stream},
			Opus:  speech.NativeOpus,
			Setup: setup,
			Choices: []speech.Choices{
				speech.Fixed(config.TTSVoice, "alloy", "ash", "ballad", "coral", "echo", "fable", "nova", "onyx", "sage", "shimmer", "verse"),
				speech.Fixed(config.TTSModel, "gpt-4o-mini-tts", "tts-1", "tts-1-hd"),
			},
		}},
		Recognizing: []speech.Recognizer{{
			Name: config.Name, Title: "OpenAI", About: "Accurate speech to text, needs an OpenAI API key", Cloud: true, Rank: 4, Whisper: true,
			Detected: "OpenAI Whisper API",
			Missing: func(_ context.Context, job *speech.Job) string {
				_, _, err := listenRoute(job)
				if err != nil {
					return err.Error()
				}
				return ""
			},
			Model: func(job *speech.Job, model string) string {
				return strs.Filled(model, job.Text(config.STTModel, config.DefaultSTTModel))
			},
			Language: func(job *speech.Job) string { return job.Text(config.STTLanguage, "") },
			Limit: func(model string) speech.Limit {
				return speech.Limit{Bytes: speech.UploadCap, Seconds: modelSeconds[model]}
			},
			Transcribe: transcribe,
			Live:       live,
			Setup:      setup,
			Choices:    []speech.Choices{speech.Fixed(config.STTModel, "whisper-1", "gpt-4o-mini-transcribe", "gpt-4o-transcribe", "gpt-transcribe")},
		}},
	})
}

// Key and address of OpenAI speech: the voice key of the keys file, then the OpenAI key or account; the refusal saying where a key goes
// when none is set
func speakRoute(job *speech.Job) (string, string, error) {
	if key := job.OpenAIKey(); key != "" {
		return key, job.Text(config.TTSBaseURL, speech.OpenAIBase), nil
	}
	return "", "", speech.Misconfigured(fmt.Sprintf(noKey, "Text to speech"))
}

// Fields beside OpenAI's own an OpenAI compatible speech server takes: the language as lang_code and the consent of a cloned voice
func Extra(job *speech.Job) map[string]any {
	extra := map[string]any{}
	if language := job.Text(config.TTSLanguage, ""); language != "" {
		extra["lang_code"] = language
	}
	if consent := job.Text(config.ConsentAttestation, ""); consent != "" {
		extra["consent_attestation"] = consent
	}
	return extra
}

func synthesize(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
	key, base, err := speakRoute(job)
	if err != nil {
		return "", err
	}
	speed, _ := job.Speed(request, config.Speed)
	err = speech.SpeechClient(key, base).Speak(ctx, speech.SpeechRequest{Model: job.Text(config.TTSModel, config.DefaultModel),
		Voice: job.Text(config.TTSVoice, config.DefaultVoice), Text: request.Text, Path: request.Path, Speed: speed, Instructions: request.Instructions,
		Extra: Extra(job)})
	return request.Path, err
}

// Key of live speech: the voice key, then the OpenAI key or account
func streamKey(job *speech.Job) string {
	return job.OpenAIKey()
}

// Rate parameter of a content type, spaced by any of the white space a header may hold
var contentRate = regexp.MustCompile("(?i)(?:^|[;\\s\\x0b\\x1c-\\x1f\\x{85}\\x{a0}])rate[\\s\\x0b\\x1c-\\x1f\\x{85}\\x{a0}]*=[\\s\\x0b\\x1c-\\x1f\\x{85}\\x{a0}]*([0-9]+)")

// Header bytes read as Latin-1 text, as HTTP clients decode header values
func latin1(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteRune(rune(s[i]))
	}
	return b.String()
}

// Rate an OpenAI compatible server names for its PCM in X-Audio-Sample-Rate or the rate parameter of its content type, the largest int for
// one past it
func headerRate(h http.Header) (int, bool) {
	var raw string
	if values := h.Values("X-Audio-Sample-Rate"); len(values) > 0 {
		raw = latin1(strings.Join(values, ", "))
	} else {
		m := contentRate.FindStringSubmatch(latin1(strings.Join(h.Values("Content-Type"), ", ")))
		if m == nil {
			return 0, false
		}
		raw = m[1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) || n <= 0 {
		return 0, false
	}
	return int(min(n, math.MaxInt)), true
}

// Speech of a sentence as PCM while OpenAI makes it, at the rate the server reports over the configured one
func stream(ctx context.Context, job *speech.Job, text string) (*speech.PCM, error) {
	rate := job.Config.GetInt(config.PCMSampleRate)
	if rate <= 0 {
		slog.Warn(fmt.Sprintf("Invalid tts.openai.pcm_sample_rate %d; using %d Hz", rate, config.DefaultRate))
		rate = config.DefaultRate
	}
	base := strs.Filled(job.Text(config.TTSBaseURL, ""), job.Getenv("OPENAI_BASE_URL"))
	body := map[string]any{"model": job.Text(config.TTSModel, config.DefaultModel), "voice": job.Text(config.TTSVoice, config.DefaultVoice), "input": text,
		"response_format": "pcm"}
	for name, value := range Extra(job) {
		body[name] = value
	}
	response, err := speech.SpeechClient(streamKey(job), base).SpeechStream(ctx, body, nil)
	if err != nil {
		return nil, err
	}
	if reported, ok := headerRate(response.Header); ok && reported != rate {
		slog.Info(fmt.Sprintf("TTS endpoint reports %d Hz PCM (expected %d Hz); honoring it", reported, rate))
		rate = reported
	}
	return &speech.PCM{Rate: rate, Chunks: speech.Chunks(response.Body, 64<<10, false)}, nil
}

// Whether an address is this machine, a private network or a LAN name, where a server needs no key
func private(address string) bool {
	parsed, err := url.Parse(address)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".lan") || strings.HasSuffix(host, ".internal") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && web.Private(addr)
}

// Key and address of OpenAI transcription: the voice key of the keys file with the configured endpoint, the key of OpenAI's speech that it
// is; a private endpoint with no key; else the OpenAI key or account, which goes to the configured endpoint only when STT_OPENAI_BASE_URL
// set it, else to OpenAI
func listenRoute(job *speech.Job) (string, string, error) {
	base := job.Text(config.STTBaseURL, "")
	switch key := job.Getenv(speech.OpenAIAudioKey); {
	case key != "":
		return key, strs.Filled(base, speech.OpenAIBase), nil
	case base != "" && private(base):
		return "not-needed", base, nil
	}
	if key := job.Key("OPENAI_API_KEY", "openai-api"); key != "" {
		if _, setting, ok := job.Config.Setting(config.STTBaseURL); ok {
			if source, _ := job.Config.Source(setting); source == configapi.FromEnvironment {
				return key, strs.Filled(base, speech.OpenAIBase), nil
			}
		}
		return key, speech.OpenAIBase, nil
	}
	return "", "", speech.Misconfigured(fmt.Sprintf(noKey, "Speech to text"))
}

// Transcription client of an OpenAI compatible service, such as Groq and DeepInfra beside OpenAI itself, tried and bounded as the OpenAI
// recognition settings say
func Client(job *speech.Job, key, base string) speech.OpenAI {
	return speech.OpenAI{Key: key, BaseURL: base, Retries: max(0, job.Config.GetInt(config.MaxRetries)),
		Timeout: time.Duration(job.Seconds(config.Timeout, time.Second) * float64(time.Second))}
}

func transcribe(ctx context.Context, job *speech.Job, path string, hints speech.Hints) speech.Transcript {
	r := speech.Recognizer{Name: config.Name}
	key, base, err := listenRoute(job)
	if err != nil {
		return r.Failure(err.Error())
	}
	model := hints.Model
	if groqModels[model] {
		slog.Info(fmt.Sprintf("Model %s not available on OpenAI, using %s", model, config.DefaultSTTModel))
		model = config.DefaultSTTModel
	}
	return job.OpenAITranscribe(ctx, r, Client(job, key, base), path, model, hints, config.Name)
}
