// Speech and recognition models DeepInfra hosts behind its OpenAI compatible API, picked from its live catalog unless one is set
package service

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/internal/models"
	deepinfraconfig "github.com/nickheyer/icebath/internal/plugins/defaults/speech/deepinfra/config"
	openaiservice "github.com/nickheyer/icebath/internal/plugins/defaults/speech/openai/service"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Address of DeepInfra's OpenAI compatible API
const defaultBase = "https://api.deepinfra.com/v1/openai"

func Register(registry *speech.Registry, catalog *models.Models) {
	d := &deepinfra{catalog: catalog}
	setup := speech.Setup{Keys: []string{deepinfraconfig.KeyEnv}, SignIn: deepinfraconfig.Name, Link: deepinfraconfig.Link, Badge: "paid"}
	registry.Register(speech.Services{
		Speakers: []speech.Voice{{
			Name: deepinfraconfig.Name, Title: "DeepInfra TTS", About: "Open voice models, needs a DeepInfra API key",
			Ready:      func(job *speech.Job) bool { return key(job) != "" },
			Synthesize: d.synthesize,
			Setup:      setup,
			Choices: []speech.Choices{speech.Listed(deepinfraconfig.TTSModel, func(ctx context.Context, job *speech.Job) []string {
				return d.models(ctx, job, "tts")
			})},
		}},
		Recognizing: []speech.Recognizer{{
			Name: deepinfraconfig.Name, Title: "DeepInfra", About: "Open speech to text models, needs a DeepInfra API key", Cloud: true, Rank: 8, Whisper: true,
			Detected: "DeepInfra Whisper API", Warning: "STT provider 'deepinfra' configured but " + deepinfraconfig.KeyEnv + " not set",
			Missing: func(_ context.Context, job *speech.Job) string {
				if key(job) == "" {
					return "DeepInfra needs an API key. Set " + deepinfraconfig.KeyEnv + " in your keys file."
				}
				return ""
			},
			Model: func(job *speech.Job, model string) string {
				return strs.Filled(model, job.Text(deepinfraconfig.STTModel, ""))
			},
			Transcribe: d.transcribe,
			Setup:      setup,
			Choices: []speech.Choices{speech.Listed(deepinfraconfig.STTModel, func(ctx context.Context, job *speech.Job) []string {
				return d.models(ctx, job, "stt")
			})},
		}},
	})
}

type deepinfra struct {
	catalog *models.Models
}

func key(job *speech.Job) string { return job.Key(deepinfraconfig.KeyEnv, deepinfraconfig.Name) }

// Address of a service: its setting, else DEEPINFRA_BASE_URL, else DeepInfra's
func base(job *speech.Job, setting string) string {
	return strings.TrimRight(strs.Filled(job.Text(setting, ""), strs.Filled(job.Getenv(deepinfraconfig.BaseURLEnv), defaultBase)), "/")
}

// Models of a surface DeepInfra's catalog tags, in its order
func (d *deepinfra) models(ctx context.Context, job *speech.Job, tag string) []string {
	route := provider.Route{Provider: deepinfraconfig.Name, Spec: deepinfraconfig.Name, BaseURL: base(job, ""), APIKey: key(job)}
	return d.catalog.TaggedIDs(ctx, job.Config, route, tag)
}

func (d *deepinfra) synthesize(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
	apiKey := key(job)
	if apiKey == "" {
		return "", speech.Misconfigured("DeepInfra needs an API key. Give it one in Settings › Voice › Text to speech › DeepInfra TTS, which keeps it in " +
			"your keys file as " + deepinfraconfig.KeyEnv + ".")
	}
	model := job.Text(deepinfraconfig.TTSModel, "")
	if model == "" {
		candidates := d.models(ctx, job, "tts")
		if len(candidates) == 0 {
			return "", speech.Misconfigured("No DeepInfra TTS model available. Pin one in config.yaml under tts.deepinfra.model, or check connectivity to " +
				"api.deepinfra.com so the live catalog can be fetched.")
		}
		model = candidates[0]
	}
	speed, _ := job.Speed(request, deepinfraconfig.Speed)
	err := speech.SpeechClient(apiKey, base(job, deepinfraconfig.TTSBaseURL)).Speak(ctx, speech.SpeechRequest{Model: model,
		Voice: job.Text(deepinfraconfig.Voice, deepinfraconfig.DefaultVoice), Text: request.Text, Path: request.Path, Speed: speed, Extra: openaiservice.Extra(job)})
	return request.Path, err
}

func (d *deepinfra) transcribe(ctx context.Context, job *speech.Job, path string, hints speech.Hints) speech.Transcript {
	r := speech.Recognizer{Name: deepinfraconfig.Name}
	apiKey := key(job)
	if apiKey == "" {
		return r.Failure(deepinfraconfig.KeyEnv + " not set")
	}
	model := hints.Model
	if model == "" {
		if candidates := d.models(ctx, job, "stt"); len(candidates) > 0 {
			model = candidates[0]
		}
	}
	if model == "" {
		return r.Failure("No DeepInfra STT model available. Pin one in config.yaml under stt.deepinfra.model, or check connectivity to api.deepinfra.com " +
			"so the live catalog can be fetched.")
	}
	return job.OpenAITranscribe(ctx, r, openaiservice.Client(job, apiKey, base(job, deepinfraconfig.STTBaseURL)), path, model, hints, deepinfraconfig.Name)
}
