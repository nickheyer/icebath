// KittenTTS, a small voice model run on this machine by its Python set
package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/kittentts/config"
	"github.com/nickheyer/icebath/internal/speech"
)

// Worker that loads a model, then speaks each request line into a WAV at 24 kHz
const script = `import json, os, sys
proto = os.fdopen(os.dup(1), "w", encoding="utf-8")
os.dup2(2, 1)
sys.stdout = sys.stderr
def reply(obj):
    proto.write(json.dumps(obj) + "\n")
    proto.flush()
spec = json.loads(sys.stdin.readline())
try:
    from kittentts import KittenTTS
    import espeakng_loader
    from phonemizer.backend.espeak.wrapper import EspeakWrapper
    EspeakWrapper.set_data_path(short_espeak_data(espeakng_loader.get_data_path()))
    model = KittenTTS(spec["model_name"])
except BaseException as exc:
    reply({"error": str(exc), "type": type(exc).__name__})
    sys.exit(0)
reply({"ok": True})
for line in sys.stdin:
    req = json.loads(line)
    try:
        audio = model.generate(req["text"], voice=req["voice"], speed=req["speed"], clean_text=req["clean_text"])
        import soundfile as sf
        sf.write(req["wav_path"], audio, 24000)
        reply({"ok": True})
    except BaseException as exc:
        reply({"error": str(exc), "type": type(exc).__name__})
`

func Register(registry *speech.Registry) {
	k := &kitten{}
	registry.Register(speech.Services{Speakers: []speech.Voice{{
		Name: config.Name, Title: "KittenTTS", Label: "KittenTTS (local, ~25MB)", About: "A small voice model on this computer, no API key needed",
		Local:      true,
		Ready:      func(job *speech.Job) bool { return job.Provisioned(config.Extra) },
		Limit:      func(*speech.Job) int { return config.DefaultLimit },
		Synthesize: k.synthesize,
		Opus:       speech.TranscodedOpus,
		Warm:       func(ctx context.Context, job *speech.Job) error { _, err := k.load(job); return err },
		Release: func(context.Context, *speech.Job) error {
			if n := k.models.Clear(); n > 0 {
				slog.Info(fmt.Sprintf("[TTS] released %d resident local model(s)", n))
			}
			return nil
		},
		Resident: true,
		Setup:    speech.Setup{Extra: config.Extra, Badge: "free"},
	}}})
}

type kitten struct {
	models speech.Resident[*speech.Worker]
}

// Failure a worker answered, with the kind of Python error it was
type answer struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Type  string `json:"type"`
}

func (a answer) err() error {
	if a.Error == "" {
		return nil
	}
	return speech.Raised(a.Type, a.Error)
}

// Worker holding the model of the settings, loaded on first use
func (k *kitten) load(job *speech.Job) (*speech.Worker, error) {
	if !job.Provisioned(config.Extra) {
		return nil, speech.Absent("KittenTTS is not installed. Install it in Settings › Voice › Text to speech › KittenTTS, or run `" +
			packages.InstallHint(config.Extra) + "`.")
	}
	model := job.Text(config.Model, config.DefaultModel)
	return k.models.Load(model, func() (*speech.Worker, error) {
		slog.Info("[KittenTTS] Loading model: " + model)
		w, err := job.Worker(config.Extra, "import os\n"+speech.ShortEspeak+script, nil)
		if err != nil {
			return nil, err
		}
		var verdict answer
		if err := w.Ask(map[string]any{"model_name": model}, &verdict); err != nil {
			w.Close()
			return nil, err
		}
		if err := verdict.err(); err != nil {
			w.Close()
			return nil, err
		}
		slog.Info("[KittenTTS] Model loaded successfully")
		return w, nil
	})
}

// Speech at 24 kHz as WAV, then the container the path asks for
func (k *kitten) synthesize(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
	w, err := k.load(job)
	if err != nil {
		return "", err
	}
	speed, _ := job.Speed(request, config.Speed)
	wav := speech.WAVPath(request.Path)
	var reply answer
	if err := w.Ask(map[string]any{"text": request.Text, "wav_path": wav, "voice": job.Text(config.Voice, config.DefaultVoice), "speed": speed,
		"clean_text": job.Config.GetBool(config.CleanText)}, &reply); err != nil {
		return "", err
	}
	if err := reply.err(); err != nil {
		return "", err
	}
	return job.FromWAV(ctx, wav, request.Path)
}
