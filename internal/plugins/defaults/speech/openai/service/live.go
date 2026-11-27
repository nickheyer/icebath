package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/openai/config"
	"github.com/nickheyer/icebath/internal/speech"
)

// Rate OpenAI's realtime transcription takes
const realtimeRate = 24000

// Live transcription through OpenAI's realtime API, the 16 kHz audio resampled to 24 kHz; none without a key
func live(ctx context.Context, job *speech.Job, hints speech.Hints, partial func(string)) (speech.Live, error) {
	key, base, err := listenRoute(job)
	if err != nil {
		return nil, nil
	}
	w := &realtime{model: job.Text(config.StreamingModel, config.DefaultStreamingModel), language: hints.Language, prompt: hints.Prompt,
		resample: speech.NewResampler(speech.LiveRate, realtimeRate)}
	return speech.Socketed(ctx, config.Name, speech.SocketURL(base, "/realtime", "intent=transcription"), http.Header{"Authorization": {"Bearer " + key}},
		speech.LiveRate, w, partial), nil
}

// Wire of a realtime transcription session
type realtime struct {
	model, language, prompt string
	resample                *speech.Resampler
	text                    string
	sent                    bool
}

func (w *realtime) Open(socket *speech.Socket) error {
	transcription := map[string]any{"model": w.model}
	if w.language != "" {
		if w.model == config.DefaultStreamingModel {
			transcription["languages"] = []string{w.language}
		} else {
			transcription["language"] = w.language
		}
	}
	if w.prompt != "" {
		transcription["prompt"] = w.prompt
	}
	return socket.JSON(map[string]any{"type": "session.update", "session": map[string]any{"type": "transcription", "audio": map[string]any{
		"input": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": realtimeRate}, "transcription": transcription, "turn_detection": nil}}}})
}

func (w *realtime) Ready() bool { return true }

func (w *realtime) Send(socket *speech.Socket, chunk []byte) error {
	pcm := w.resample.Resample(chunk)
	if len(pcm) == 0 {
		return nil
	}
	w.sent = true
	return socket.JSON(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(pcm)})
}

// Commits the buffer, or finishes empty at once when no audio was sent
func (w *realtime) End(socket *speech.Socket) (*speech.Transcript, error) {
	if !w.sent {
		return &speech.Transcript{Success: true, Provider: config.Name}, nil
	}
	return nil, socket.JSON(map[string]any{"type": "input_audio_buffer.commit"})
}

func (w *realtime) Event(session *speech.Session, event map[string]any) (*speech.Transcript, error) {
	kind := speech.Field(event, "type")
	switch {
	case strings.HasSuffix(kind, "input_audio_transcription.delta"):
		w.text += speech.Field(event, "delta")
		session.Partial(w.text)
	case strings.HasSuffix(kind, "input_audio_transcription.completed"):
		transcript := speech.Field(event, "transcript")
		if transcript == "" {
			transcript = w.text
		}
		return session.Heard(transcript), nil
	case kind == "error":
		message := "provider error"
		if raw := event["error"]; speech.Held(raw) {
			detail, isObject := raw.(map[string]any)
			if !isObject {
				return nil, fmt.Errorf("OpenAI realtime error is %s where a mapping was expected", speech.Kind(raw))
			}
			if text := speech.Field(detail, "message"); text != "" {
				message = text
			}
		}
		if strings.Contains(strings.ToLower(message), "buffer too small") {
			return session.Heard(""), nil
		}
		return session.Refused(message), nil
	}
	return nil, nil
}
