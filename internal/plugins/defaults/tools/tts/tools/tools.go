// text_to_speech: the model's text spoken into audio files by the voice of the profile, which the chat apps deliver as audio
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/tools"
)

// Name of the tool
const Name = "text_to_speech"

// What the description says of who picks the voice, when the person does and when the model may
const configured = "Voice and provider are user-configured (built-in providers like edge/openai or custom command providers under tts.providers.<name>), " +
	"not model-selected."

// What the tool does, up to the folder the audio is saved in, which the schema names after it
const description = "Convert text to speech audio. Returns a MEDIA: path that the platform delivers as native audio. Compatible providers render as a " +
	"voice bubble on Telegram; otherwise audio is sent as a regular attachment. In CLI mode, saves to "

type registrant struct {
	speech *speech.Speech
}

func Register(registry *tools.Registry, sp *speech.Speech) {
	registry.Register(&registrant{speech: sp})
}

func (r *registrant) Tools() []tools.Def {
	return []tools.Def{{
		Name: Name, Toolset: "tts", Title: "Speak", About: "Turn text into speech that you can listen to", Group: "Text to speech",
		Icon: tools.IconRecord, Subject: "text",
		Schema:    schema("the profile's audio cache", nil),
		Dynamic:   r.schema,
		Handler:   r.speak,
		Check:     func(env *tools.Env) bool { return r.job(env).CanSpeak() },
		Effect:    tools.Mutates,
		Unfocused: true,
	}}
}

// Job of a call: the profile's settings, the session's helper models and the surface the audio goes to
func (r *registrant) job(env *tools.Env) *speech.Job {
	job := r.speech.Job(env.Config)
	if env.Aux != nil {
		job.Aux = env.Aux
	}
	job.Platform = env.Platform
	return job
}

func schema(cache string, picker *speech.Picker) map[string]any {
	properties := map[string]any{
		"text": map[string]any{"type": "string", "description": "The text to convert to speech. Provider-specific per-request character caps apply automatically " +
			"(OpenAI 4096, xAI 15000, MiniMax 10000, ElevenLabs 5k-40k depending on model); longer input is split into ordered chunks without silent truncation."},
		"output_path": map[string]any{"type": "string", "description": "Optional custom file path to save the audio. Defaults to " + cache +
			"/tts_<timestamp>.mp3"},
		"speed": map[string]any{"type": "number", "description": "Playback speed multiplier. 1.0 = normal, 0.5 = very slow (language learning), 2.0 = fast. " +
			"Range: 0.25-4.0. Overrides the speed configured in config.yaml."},
		"instructions": map[string]any{"type": "string", "description": "Optional voice-design guidance: tone, emotion, pacing, accent, whispering, impressions " +
			"(e.g. 'Speak in a cheerful, excited whisper'). Forwarded to the OpenAI backend (gpt-4o-mini-tts and OpenAI-compatible voice-design servers). " +
			"Silently ignored by backends that don't support it."},
	}
	who := configured
	required := []string{"text"}
	if picker != nil {
		properties["voice_id"] = map[string]any{"type": "string", "description": picker.About}
		properties["find_voices"] = picker.Search
		who = picker.Sentence
		required = []string{}
	}
	return tools.Schema(description+cache+". "+who, required, properties)
}

// Schema of the session: the profile's audio cache named, and the voice picking of a voice that lets the model pick
func (r *registrant) schema(env *tools.Env) map[string]any {
	job := r.job(env)
	return schema(display(job.AudioCache()), job.Speaker().Picker)
}

// Path with the home folder written as ~
func display(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home); ok && (rest == "" || strings.HasPrefix(rest, string(filepath.Separator))) {
		return "~" + filepath.ToSlash(rest)
	}
	return path
}

// Failure the model reads, its error cut to the error bound
func failure(message string) string {
	return tools.JSON(map[string]any{"error": tools.BoundError(message), "success": false})
}

func (r *registrant) speak(ctx context.Context, call *tools.Call) string {
	job := r.job(call.Env)
	args := call.Args
	if query, ok := args["find_voices"]; ok && query != nil {
		return r.find(ctx, job, query)
	}
	text, _ := args["text"].(string)
	request := speech.Call{Text: text}
	request.OutputPath, _ = args["output_path"].(string)
	request.Instructions, _ = args["instructions"].(string)
	request.Voice, _ = args["voice_id"].(string)
	if value, ok := args["speed"]; ok && value != nil {
		speed, err := number(value)
		if err != nil {
			return tools.ErrorResult(err)
		}
		request.Speed = &speed
	}
	return job.TextToSpeech(ctx, request).Result()
}

// Speed the model gave: a number, a true or false, or text that reads as one
func number(value any) (float64, error) {
	switch v := value.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case string:
		speed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, fmt.Errorf("%s is not a number", v)
		}
		return speed, nil
	case []any:
		return 0, fmt.Errorf("expected a number, got list")
	case map[string]any:
		return 0, fmt.Errorf("expected a number, got dict")
	}
	return 0, fmt.Errorf("expected a number, got %T", value)
}

// One page of voices of a voice that lets the model pick
func (r *registrant) find(ctx context.Context, job *speech.Job, query any) string {
	asked, ok := query.(map[string]any)
	if !ok {
		return failure(`find_voices must be an object, e.g. {"query": "orc"}`)
	}
	voice := job.Speaker()
	if voice.Picker == nil {
		return failure("find_voices needs tts.provider: elevenlabs")
	}
	page, err := voice.Picker.Find(ctx, job, asked)
	if err != nil {
		return failure(err.Error())
	}
	return tools.JSON(page)
}
