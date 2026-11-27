package speech

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

// Most bytes of PCM one sentence streams before the rest is dropped
const sentenceCap = 16 << 20

// Where a piece of speech the app plays comes from: the start of each clip, its format and the rate of PCM, then its bytes
type Sink struct {
	Clip func(format string, rate int) error
	Data func(data []byte) error
}

// Voice that streams the profile's speech, nil for speech written as whole files: tts.streaming.provider, auto trying every voice that
// streams in its order, else the voice the profile speaks with
func (j *Job) streamer() (Voice, bool) {
	choice := Normal(j.Config.GetString(speechconfig.StreamingProvider))
	if choice == speechconfig.AutoStreaming {
		voices := slices.DeleteFunc(j.Speech.Voices(j.Config), func(v Voice) bool { return v.Stream == nil })
		slices.SortStableFunc(voices, func(a, b Voice) int { return a.Stream.Rank - b.Stream.Rank })
		for _, voice := range voices {
			if voice.Stream.Ready == nil || voice.Stream.Ready(j) {
				return voice, true
			}
		}
		return Voice{}, false
	}
	name := cmpOr(choice, Normal(j.Config.GetString(speechconfig.TTSProvider)))
	voice, ok := j.Speech.Voice(j.Config, name)
	if !ok || voice.Stream == nil || voice.Stream.Ready != nil && !voice.Stream.Ready(j) {
		return Voice{}, false
	}
	return voice, true
}

// Whether the profile's speech streams as PCM while it is made
func (j *Job) Streams() bool {
	_, ok := j.streamer()
	return ok
}

// Speaks one sentence for the app to play: as PCM while it is made when asked and a voice streams, else as the files text_to_speech
// writes, each sent as a clip and removed
func (j *Job) Speak(ctx context.Context, text string, live bool, sink Sink) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if voice, ok := j.streamer(); live && ok {
		return j.stream(ctx, voice, text, sink)
	}
	dir, err := os.MkdirTemp("", "icebath-speech-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	outcome := j.TextToSpeech(ctx, Call{Text: text, OutputPath: filepath.Join(dir, "speech.mp3")})
	if !outcome.Success {
		return errors.New(outcome.Error)
	}
	for _, path := range outcome.FilePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := sink.Clip(strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."), 0); err != nil {
			return err
		}
		if err := sink.Data(data); err != nil {
			return err
		}
	}
	return nil
}

// PCM of one sentence as the voice makes it, the sentence cut to the configured voice's limit and the stream to its byte cap
func (j *Job) stream(ctx context.Context, voice Voice, text string, sink Sink) error {
	if limit := j.speakerLimit(); len([]rune(text)) > limit {
		text = string([]rune(text)[:limit])
	}
	pcm, err := voice.Stream.Open(ctx, j, text)
	if err != nil {
		return fmt.Errorf("streaming TTS synthesis failed: %w", err)
	}
	if err := sink.Clip("pcm", pcm.Rate); err != nil {
		return err
	}
	total := 0
	for chunk, err := range pcm.Chunks {
		if err != nil {
			return fmt.Errorf("streaming TTS failed mid-sentence: %w", err)
		}
		if total += len(chunk); total > sentenceCap {
			slog.Warn(fmt.Sprintf("%s exceeded %d bytes for one sentence; truncating", cmpOr(voice.Stream.Label, voice.Title+" streaming TTS"), sentenceCap))
			return nil
		}
		if err := sink.Data(chunk); err != nil {
			return err
		}
	}
	return nil
}
