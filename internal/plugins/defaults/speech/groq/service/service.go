// Whisper on Groq through its OpenAI compatible API
package service

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/groq/config"
	openaiservice "github.com/nickheyer/icebath/internal/plugins/defaults/speech/openai/service"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Models Groq does not serve, OpenAI's and a retired one of its own, swapped for its default
var unserved = strs.Set("whisper-1", "gpt-4o-mini-transcribe", "gpt-4o-transcribe", "gpt-transcribe", "distil-whisper-large-v3-en")

func Register(registry *speech.Registry) {
	registry.Register(speech.Services{Recognizing: []speech.Recognizer{{
		Name: config.Name, Title: "Groq", About: "whisper-large-v3-turbo, whisper-large-v3 — very fast", Cloud: true, Rank: 3, Whisper: true,
		Detected: "Groq Whisper API", Warning: "STT provider 'groq' configured but " + config.KeyEnv + " not set",
		Missing: func(_ context.Context, job *speech.Job) string {
			if key(job) == "" {
				return config.KeyEnv + " not set"
			}
			return ""
		},
		Model: func(job *speech.Job, model string) string {
			return strs.Filled(model, job.Text(config.Model, config.DefaultModel))
		},
		Language:   func(job *speech.Job) string { return job.Text(config.Language, "") },
		Limit:      func(string) speech.Limit { return speech.Limit{Bytes: speech.UploadCap} },
		Transcribe: transcribe,
		Choices:    []speech.Choices{speech.Fixed(config.Model, "whisper-large-v3-turbo", "whisper-large-v3")},
		Setup:      speech.Setup{Keys: []string{config.KeyEnv}, Link: "https://console.groq.com/keys", Badge: "free tier"},
	}}})
}

func key(job *speech.Job) string { return job.Key(config.KeyEnv) }

func transcribe(ctx context.Context, job *speech.Job, path string, hints speech.Hints) speech.Transcript {
	r := speech.Recognizer{Name: config.Name}
	apiKey := key(job)
	if apiKey == "" {
		return r.Failure(config.KeyEnv + " not set")
	}
	model := hints.Model
	if unserved[model] {
		slog.Info(fmt.Sprintf("Model %s not available on Groq, using %s", model, config.DefaultModel))
		model = config.DefaultModel
	}
	fields := [][2]string{{"model", model}, {"response_format", "text"}}
	if hints.Language != "" {
		fields = append(fields, [2]string{"language", hints.Language})
	}
	if hints.Prompt != "" {
		fields = append(fields, [2]string{"prompt", hints.Prompt})
	}
	reply, err := openaiservice.Client(job, apiKey, job.Text(config.BaseURL, config.DefaultBase)).Transcription(ctx, path, fields)
	if err != nil {
		return r.OpenAIFailure(err, path, "Groq")
	}
	text, err := speech.TranscriptOf(reply)
	if err != nil {
		return r.OpenAIFailure(err, path, "Groq")
	}
	slog.Info(fmt.Sprintf("Transcribed %s via Groq API (%s, lang=%s, %d chars)", filepath.Base(path), model, strs.Filled(hints.Language, "auto"),
		utf8.RuneCountInString(text)))
	return r.Heard(text)
}
