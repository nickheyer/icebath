//go:build !icebath_core && !icebath_noextras

package media

import (
	"errors"
	"slices"
	"testing"

	portaudioutil "github.com/nickheyer/icebath/pkg/utils/portaudio"
)

// Input stream returning blocks at scripted levels, the last level repeating
type levelStream struct {
	fakeStream
	levels    []int16
	readCount int
}

func (s *levelStream) Read(frames int) ([]int16, bool, error) {
	level := s.levels[min(s.readCount, len(s.levels)-1)]
	s.readCount++
	return fullFrame(frames, level), false, nil
}

func levels(parts ...any) []int16 {
	var out []int16
	for i := 0; i < len(parts); i += 2 {
		for range parts[i+1].(int) {
			out = append(out, int16(parts[i].(int)))
		}
	}
	return out
}

func withLevelStream(t *testing.T, lv []int16) *levelStream {
	sd := withFakeSD(t)
	stream := &levelStream{levels: lv}
	sd.inputFactory = func(portaudioutil.StreamParams, portaudioutil.InputCallback) (portaudioutil.InputStream, error) {
		return stream, nil
	}
	return stream
}

func stopAfter(n int) func() bool {
	calls := 0
	return func() bool {
		calls++
		return calls > n
	}
}

// Captures the audio handed to the WAV writer and returns the fixed path
func withCapturedWAV(t *testing.T, path string) *[]int16 {
	var written []int16
	captured := &written
	swapVoice(t, &voiceWriteWAV, func(audio []int16, _ int) (string, error) {
		*captured = audio
		return path, nil
	})
	return captured
}

func countLevel(audio []int16, v int16) int {
	n := 0
	for _, s := range audio {
		if s == v {
			n++
		}
	}
	return n
}

// Runs full_duplex_listen over scripted levels with playback reported between two read counts
func runDuplex(t *testing.T, lv []int16, playingFrom, playingUntil int) (string, *[]int16, *levelStream, []string) {
	stream := withLevelStream(t, lv)
	audio := withCapturedWAV(t, "/tmp/fd.wav")
	var phases []string
	o := DefaultDuplexOptions()
	o.IsPlaying = func() bool {
		if playingFrom < 0 || stream.readCount < playingFrom {
			return false
		}
		return playingUntil < 0 || stream.readCount < playingUntil
	}
	o.OnTrigger = func(phase string) { phases = append(phases, phase) }
	path := FullDuplexListen(stopAfter(len(lv)), o)
	return path, audio, stream, phases
}

func TestFullDuplexListenGenerationPhaseSpeechTripsAndCaptures(t *testing.T) {
	path, audio, _, phases := runDuplex(t, levels(100, 15, 5000, 30, 0, 500), -1, -1)
	if path != "/tmp/fd.wav" || !slices.Equal(phases, []string{"generation"}) || countLevel(*audio, 5000) == 0 {
		t.Fatalf("path %q phases %v", path, phases)
	}
}

func TestFullDuplexListenPlaybackBleedAloneDoesNotTrip(t *testing.T) {
	path, _, _, phases := runDuplex(t, levels(100, 15, 1000, 300), 15, -1)
	if path != "" || len(phases) != 0 {
		t.Fatalf("path %q phases %v", path, phases)
	}
}

func TestFullDuplexListenSpeechOverBleedTripsInPlaybackPhase(t *testing.T) {
	path, _, _, phases := runDuplex(t, levels(100, 15, 1000, 16+20, 5000, 30, 1000, 200), 15, -1)
	if path != "/tmp/fd.wav" || !slices.Equal(phases, []string{"playback"}) {
		t.Fatalf("path %q phases %v", path, phases)
	}
}

func TestFullDuplexListenGraceWindowSuppressesPlaybackOnset(t *testing.T) {
	path, _, stream, phases := runDuplex(t, levels(100, 15, 5000, 8, 800, 40, 5000, 30, 0, 200), 15, -1)
	if path != "/tmp/fd.wav" || !slices.Equal(phases, []string{"playback"}) || stream.readCount <= 15+8+40 {
		t.Fatalf("path %q phases %v reads %d", path, phases, stream.readCount)
	}
}

func TestFullDuplexListenStreamFailureReturnsNothing(t *testing.T) {
	sd := withFakeSD(t)
	sd.inputFactory = func(portaudioutil.StreamParams, portaudioutil.InputCallback) (portaudioutil.InputStream, error) {
		return nil, errors.New("device unavailable")
	}
	if path := FullDuplexListen(func() bool { return false }, DefaultDuplexOptions()); path != "" {
		t.Fatalf("path %q", path)
	}
}
