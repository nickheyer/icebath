// Mistral's Voxtral voices and Voxtral Transcribe
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/mistral/config"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Longest a Mistral call may take
const deadline = 120 * time.Second

func Register(registry *speech.Registry) {
	setup := speech.Setup{Keys: []string{config.KeyEnv}, Link: config.Link, Badge: "paid"}
	registry.Register(speech.Services{
		Speakers: []speech.Voice{{
			Name: config.Name, Title: "Mistral Voxtral TTS", About: "Voices in many languages, needs a Mistral API key", Label: "Mistral Voxtral TTS",
			Ready:      func(job *speech.Job) bool { return key(job) != "" },
			Limit:      func(*speech.Job) int { return config.DefaultLimit },
			Synthesize: synthesize,
			Opus:       speech.NativeOpus,
			Setup:      setup,
		}},
		Recognizing: []speech.Recognizer{{
			Name: config.Name, Title: "Mistral Voxtral", About: "Speech to text in many languages, needs a Mistral API key", Cloud: true, Rank: 5,
			Detected: "Mistral Voxtral Transcribe API", Warning: "STT provider 'mistral' configured but " + config.KeyEnv + " not set",
			Missing: func(_ context.Context, job *speech.Job) string {
				if key(job) == "" {
					return "Mistral needs an API key. Set " + config.KeyEnv + " in your keys file."
				}
				return ""
			},
			Model: func(job *speech.Job, model string) string {
				return strs.Filled(model, job.Text(config.STTModel, config.DefaultSTTModel))
			},
			Language:   func(job *speech.Job) string { return job.Text(config.STTLanguage, "") },
			Limit:      func(string) speech.Limit { return speech.Limit{Bytes: 500 << 20, Seconds: 3600} },
			Transcribe: transcribe,
			Setup:      setup,
			Choices:    []speech.Choices{speech.Fixed(config.STTModel, "voxtral-mini-latest", "voxtral-mini-2602")},
		}},
	})
}

func key(job *speech.Job) string { return job.Key(config.KeyEnv) }

// One call to Mistral's API as its SDK makes it: the reply's object, or the kind of failure the SDK names with the error
func call(ctx context.Context, address, apiKey, kind string, body []byte) (map[string]any, string, error) {
	bounded, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	request, err := http.NewRequestWithContext(bounded, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return nil, "SDKError", err
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", kind)
	request.Header.Set("Accept", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, "ConnectError", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, "ReadError", err
	}
	switch {
	case response.StatusCode == http.StatusUnprocessableEntity:
		return nil, "HTTPValidationError", fmt.Errorf("API error occurred: Status %d. Body: %s", response.StatusCode, raw)
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return nil, "SDKError", fmt.Errorf("API error occurred: Status %d. Body: %s", response.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "ResponseValidationError", err
	}
	return out, "", nil
}

func synthesize(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
	apiKey := key(job)
	if apiKey == "" {
		return "", speech.Misconfigured(config.KeyEnv + " not set. Get one at " + config.Link)
	}
	payload, err := json.Marshal(map[string]any{"model": job.Text(config.TTSModel, config.DefaultModel), "input": request.Text,
		"voice_id": job.Text(config.VoiceID, config.DefaultVoice), "response_format": speech.ResponseFormat(request.Path)})
	if err != nil {
		return "", err
	}
	server := strings.TrimRight(job.Text(config.TTSBaseURL, config.DefaultBase), "/")
	reply, kind, err := call(ctx, server+"/v1/audio/speech", apiKey, "application/json", payload)
	if err == nil {
		if _, ok := reply["audio_data"].(string); !ok {
			kind, err = "ResponseValidationError", errors.New("Response validation failed: audio_data missing")
		}
	}
	if err != nil {
		slog.Error(fmt.Sprintf("Mistral TTS failed: %v", err))
		return "", fmt.Errorf("Mistral TTS failed: %s", kind)
	}
	audio, err := speech.Base64Std(reply["audio_data"].(string))
	if err != nil {
		return "", err
	}
	return request.Path, os.WriteFile(request.Path, audio, 0o666)
}

func transcribe(ctx context.Context, job *speech.Job, path string, hints speech.Hints) speech.Transcript {
	r := speech.Recognizer{Name: config.Name}
	apiKey := key(job)
	if apiKey == "" {
		return r.Failure(config.KeyEnv + " not set")
	}
	parts := []speech.Part{{Name: "model", Value: hints.Model}}
	if hints.Language != "" {
		parts = append(parts, speech.Part{Name: "language", Value: hints.Language})
	}
	if hints.Prompt != "" {
		parts = append(parts, speech.Part{Name: "prompt", Value: hints.Prompt})
	}
	parts = append(parts, speech.Part{Name: "file", File: path})
	form, kind, err := speech.Form(parts)
	if err != nil {
		return r.CloudFailure(err, path, "Mistral transcription", "")
	}
	reply, class, err := call(ctx, config.DefaultBase+"/v1/audio/transcriptions", apiKey, kind, form)
	if err != nil {
		return r.CloudFailure(err, path, "Mistral transcription", class)
	}
	text, err := speech.TranscriptOf(reply)
	if err != nil {
		return r.CloudFailure(err, path, "Mistral transcription", "STTResponseError")
	}
	slog.Info(fmt.Sprintf("Transcribed %s via Mistral API (%s, %d chars)", filepath.Base(path), hints.Model, utf8.RuneCountInString(text)))
	return r.Heard(text)
}
