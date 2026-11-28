// ElevenLabs voices, which the model may search and pick per call, and its Scribe transcription
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/elevenlabs/config"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Characters one request of each model takes
var modelLimits = map[string]int{
	"eleven_v3": 5000, "eleven_ttv_v3": 5000,
	"eleven_multilingual_v2": 10000, "eleven_multilingual_v1": 10000,
	"eleven_english_sts_v2": 10000, "eleven_english_sts_v1": 10000,
	"eleven_flash_v2": 30000, "eleven_flash_v2_5": 40000,
}

// Voice ids find_voices returns
var voiceID = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)

// Longest a speech request may take, as ElevenLabs' SDK allows
const deadline = 240 * time.Second

func Register(registry *speech.Registry) {
	setup := speech.Setup{Keys: []string{config.KeyEnv}, Link: config.KeysLink, Badge: "paid"}
	ready := func(job *speech.Job) bool { return key(job) != "" }
	registry.Register(speech.Services{
		Speakers: []speech.Voice{{
			Name: config.Name, Title: "ElevenLabs", About: "The most lifelike voices, needs an ElevenLabs API key",
			Ready: ready,
			Limit: func(job *speech.Job) int {
				if limit := modelLimits[job.Text(config.ModelID, config.DefaultModel)]; limit > 0 {
					return limit
				}
				return config.DefaultLimit
			},
			Synthesize: synthesize,
			Stream:     &speech.Stream{Rank: 1, Ready: ready, Open: stream},
			Opus:       speech.NativeOpus,
			Picker: &speech.Picker{
				Sentence: "The provider is user-configured (ElevenLabs); pick a voice per call with voice_id, found with find_voices (one character per voice, " +
					"e.g. for game NPCs).",
				About:  "ElevenLabs voice for this audio (a voice_id from find_voices). Omit to use the user's configured voice.",
				Search: searchSchema(),
				Valid: func(id string) error {
					if !voiceID.MatchString(id) {
						return fmt.Errorf("Invalid ElevenLabs voice_id %s: use a voice_id returned by find_voices.", id)
					}
					return nil
				},
				Find: find,
			},
			Setup: setup,
			Choices: []speech.Choices{{Setting: config.VoiceID, List: listVoices},
				speech.Fixed(config.ModelID, config.DefaultModel, "eleven_flash_v2_5", "eleven_turbo_v2_5", "eleven_v3")},
		}},
		Recognizing: []speech.Recognizer{{
			Name: config.Name, Title: "ElevenLabs Scribe", About: "Tells speakers apart and marks sounds, needs an API key", Cloud: true, Rank: 7,
			Detected: "ElevenLabs Scribe STT API", Warning: "STT provider 'elevenlabs' configured but " + config.KeyEnv + " not set",
			Missing: func(_ context.Context, job *speech.Job) string {
				if key(job) == "" {
					return "ElevenLabs needs an API key. Set " + config.KeyEnv + " in your keys file."
				}
				return ""
			},
			Model: func(job *speech.Job, model string) string {
				return strs.Filled(model, job.Text(config.STTModelID, config.DefaultSTTModel))
			},
			Language:   func(job *speech.Job) string { return job.Text(config.LanguageCode, "") },
			Limit:      func(string) speech.Limit { return speech.Limit{Bytes: 5<<30 - 1} },
			Transcribe: transcribe,
			Live:       live,
			Setup:      setup,
			Choices:    []speech.Choices{speech.Fixed(config.STTModelID, "scribe_v2", "scribe_v1")},
		}},
	})
}

func key(job *speech.Job) string { return job.Key(config.KeyEnv) }

func base(job *speech.Job) string {
	return strings.TrimRight(job.Text(config.TTSBaseURL, config.DefaultBase), "/")
}

// POST of a speech request with the key, its reply open for its audio to be read as it arrives
func post(ctx context.Context, job *speech.Job, voice, format, model, text string) (*http.Response, error) {
	address := base(job) + "/v1/text-to-speech/" + url.PathEscape(voice) + "?output_format=" + url.QueryEscape(format)
	payload, err := json.Marshal(map[string]any{"text": text, "model_id": model})
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, deadline)
	request, err := http.NewRequestWithContext(bounded, http.MethodPost, address, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header.Set("xi-api-key", key(job))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer cancel()
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		var body any = string(raw)
		var parsed any
		if json.Unmarshal(raw, &parsed) == nil {
			body = parsed
		}
		return nil, fmt.Errorf("status_code: %d, body: %s", response.StatusCode, speech.Text(body))
	}
	response.Body = cancelling{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type cancelling struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelling) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// Voice of a request: the one the call picked, else tts.elevenlabs.voice_id
func voice(job *speech.Job, request string) string {
	return strs.Filled(strings.TrimSpace(request), job.Text(config.VoiceID, config.DefaultVoice))
}

// Opus at 48 kHz for an .ogg path, else MP3 at 44.1 kHz
func synthesize(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
	if key(job) == "" {
		return "", speech.Misconfigured(config.KeyEnv + " not set. Get one at " + config.Link)
	}
	format := "mp3_44100_128"
	if strings.HasSuffix(request.Path, ".ogg") {
		format = "opus_48000_64"
	}
	response, err := post(ctx, job, voice(job, request.Voice), format, job.Text(config.ModelID, config.DefaultModel), request.Text)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	f, err := os.Create(request.Path)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, response.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return request.Path, err
}

// PCM at 24 kHz of a sentence, the live model over the speech model
func stream(ctx context.Context, job *speech.Job, text string) (*speech.PCM, error) {
	model := strs.Filled(job.Text(config.StreamingModelID, ""), job.Text(config.ModelID, config.DefaultModel))
	response, err := post(ctx, job, voice(job, ""), "pcm_24000", model, text)
	if err != nil {
		return nil, err
	}
	return &speech.PCM{Rate: 24000, Chunks: speech.Chunks(response.Body, 1024, true)}, nil
}

func sttBase(job *speech.Job) string {
	return strings.TrimRight(job.Text(config.STTBaseURL, config.DefaultSTTBase), "/")
}

// Error detail of a reply: the message of its detail or error object, else the field as it is
func detail(body map[string]any) string {
	value := body["detail"]
	if !speech.Held(value) {
		value = body["error"]
	}
	if object, ok := value.(map[string]any); ok {
		if message := object["message"]; speech.Held(message) {
			return speech.Text(message)
		}
		return speech.Text(object)
	}
	if !speech.Held(value) {
		return ""
	}
	return speech.Text(value)
}

func transcribe(ctx context.Context, job *speech.Job, path string, hints speech.Hints) speech.Transcript {
	r := speech.Recognizer{Name: config.Name}
	if hints.Prompt != "" {
		speech.PromptIgnored("STT provider 'elevenlabs'")
	}
	apiKey := key(job)
	if apiKey == "" {
		return r.Failure(config.KeyEnv + " not set")
	}
	parts := []speech.Part{{Name: "model_id", Value: hints.Model}, {Name: "tag_audio_events", Value: strconv.FormatBool(job.Config.GetBool(config.TagAudioEvents))},
		{Name: "diarize", Value: strconv.FormatBool(job.Config.GetBool(config.Diarize))}}
	if hints.Language != "" {
		parts = append(parts, speech.Part{Name: "language_code", Value: hints.Language})
	}
	parts = append(parts, speech.Part{Name: "file", File: path})
	response, err := speech.Upload(ctx, sttBase(job)+"/speech-to-text", parts, map[string]string{"xi-api-key": apiKey}, 120*time.Second)
	transcript, _ := r.Replied(response, err, path, "ElevenLabs STT", detail, func(body map[string]any) (string, error) { return speech.TranscriptOf(body) })
	if transcript.Success {
		slog.Info(fmt.Sprintf("Transcribed %s via ElevenLabs Scribe (%s, %d chars)", filepath.Base(path), hints.Model, utf8.RuneCountInString(transcript.Text)))
	}
	return transcript
}
