package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/elevenlabs/config"
	"github.com/nickheyer/icebath/internal/speech"
)

// Live transcription through Scribe realtime, committed by its voice detection; none without a key
func live(ctx context.Context, job *speech.Job, hints speech.Hints, partial func(string)) (speech.Live, error) {
	apiKey := key(job)
	if apiKey == "" {
		return nil, nil
	}
	query := url.Values{"model_id": {config.RealtimeModel}, "audio_format": {"pcm_" + strconv.Itoa(speech.LiveRate)}, "commit_strategy": {"vad"}}
	if hints.Language != "" {
		query.Set("language_code", hints.Language)
	}
	address := speech.SocketURL(sttBase(job), "/speech-to-text/realtime", query.Encode())
	return speech.Socketed(ctx, config.Name, address, http.Header{"Xi-Api-Key": {apiKey}}, speech.LiveRate, &realtime{}, partial), nil
}

// Wire of a Scribe realtime session: the text committed so far and the words not yet committed
type realtime struct {
	committed []string
	interim   string
	ending    bool
}

func chunk(pcm []byte, commit bool) map[string]any {
	return map[string]any{"message_type": "input_audio_chunk", "audio_base_64": base64.StdEncoding.EncodeToString(pcm), "commit": commit,
		"sample_rate": speech.LiveRate}
}

func (w *realtime) Open(*speech.Socket) error { return nil }

func (w *realtime) Ready() bool { return true }

func (w *realtime) Send(socket *speech.Socket, pcm []byte) error {
	return socket.JSON(chunk(pcm, false))
}

func (w *realtime) End(socket *speech.Socket) (*speech.Transcript, error) {
	w.ending = true
	return nil, socket.JSON(chunk(nil, true))
}

func (w *realtime) text() string {
	return strings.TrimSpace(strings.Join(append(append([]string(nil), w.committed...), w.interim), " "))
}

func (w *realtime) Event(session *speech.Session, event map[string]any) (*speech.Transcript, error) {
	switch kind := speech.Field(event, "message_type"); {
	case kind == "partial_transcript":
		w.interim = strings.TrimSpace(speech.Field(event, "text"))
		session.Partial(w.text())
	case kind == "committed_transcript":
		if text := strings.TrimSpace(speech.Field(event, "text")); text != "" {
			w.committed = append(w.committed, text)
		}
		w.interim = ""
		session.Partial(w.text())
		if w.ending {
			return session.Heard(w.text()), nil
		}
	case (kind == "insufficient_audio_activity" || kind == "commit_throttled") && w.ending:
		return session.Heard(w.text()), nil
	case kind != "" && kind != "session_started" && kind != "committed_transcript_with_timestamps" && kind != "warning":
		if failure, ok := event["error"]; ok {
			return session.Refused(fmt.Sprintf("%s: %s", kind, speech.Text(failure))), nil
		}
	}
	return nil, nil
}
