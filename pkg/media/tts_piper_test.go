package media

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/media/speech"
)

// Voice answering fixed samples and recording the speaker and speed it was asked for
type scriptedVoice struct {
	samples []float32
	speaker *int
}

func (v scriptedVoice) Speak(_ string, speaker int, _ float32) ([]float32, int, error) {
	*v.speaker = speaker
	return v.samples, 22050, nil
}

func (scriptedVoice) Close() {}

// Signed samples of the data chunk of a 16 bit WAV file with its sample rate
func readPCM16(t *testing.T, path string) ([]int16, int) {
	t.Helper()
	wav, err := ReadWave(path)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int16, len(wav.Frames)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(wav.Frames[2*i:]))
	}
	return out, wav.FrameRate
}

func TestPiperSynthesisShapesAudioAndCarriesEveryKnob(t *testing.T) {
	cleanLifecycle(t)
	var loadedFiles []speech.PiperVoice
	var devices []bool
	speaker := -1
	swap(t, &resolvePiperVoice, func(_ context.Context, voice, _ string) (speech.PiperVoice, error) {
		return speech.PiperVoice{Model: voice + ".onnx", NoiseScale: 0.667, NoiseW: 0.8, LengthScale: 1}, nil
	})
	swap(t, &loadPiperVoice, func(_ context.Context, files speech.PiperVoice, gpu bool) (localVoice, error) {
		loadedFiles, devices = append(loadedFiles, files), append(devices, gpu)
		return scriptedVoice{samples: []float32{0.1, -0.2}, speaker: &speaker}, nil
	})
	ctx, dir := context.Background(), t.TempDir()
	out, err := generatePiperTTS(ctx, nil, "hi", filepath.Join(dir, "a.wav"), map[string]any{"piper": map[string]any{"volume": 0.5}})
	if err != nil {
		t.Fatal(err)
	}
	if pcm, rate := readPCM16(t, out); rate != 22050 || !reflect.DeepEqual(pcm, []int16{8192, -16384}) || speaker != 0 {
		t.Fatalf("normalized half volume audio = %v at %d for speaker %d", pcm, rate, speaker)
	}
	knobs := map[string]any{"voice": "de_DE-thorsten-high", "normalize_audio": false, "volume": 2.0, "speaker_id": 3, "length_scale": 1.5, "noise_scale": 0.3,
		"noise_w_scale": 0.9, "use_cuda": true}
	out, err = generatePiperTTS(ctx, nil, "hi", filepath.Join(dir, "b.wav"), map[string]any{"piper": knobs})
	if err != nil {
		t.Fatal(err)
	}
	if pcm, _ := readPCM16(t, out); !reflect.DeepEqual(pcm, []int16{6553, -13107}) || speaker != 3 {
		t.Fatalf("raw double volume audio = %v for speaker %d", pcm, speaker)
	}
	want := []speech.PiperVoice{{Model: "en_US-lessac-medium.onnx", NoiseScale: 0.667, NoiseW: 0.8, LengthScale: 1},
		{Model: "de_DE-thorsten-high.onnx", NoiseScale: 0.3, NoiseW: 0.9, LengthScale: 1.5}}
	if !reflect.DeepEqual(loadedFiles, want) || !reflect.DeepEqual(devices, []bool{false, true}) {
		t.Fatalf("loaded %+v on %v", loadedFiles, devices)
	}
	if _, err := generatePiperTTS(ctx, nil, "again", filepath.Join(dir, "c.wav"), map[string]any{"piper": knobs}); err != nil || len(loadedFiles) != 2 {
		t.Fatalf("an unchanged voice loaded again: %v, %d loads", err, len(loadedFiles))
	}
	if got := shapePiperAudio([]float32{0, 0}, true, 1); !reflect.DeepEqual(got, []float32{0, 0}) {
		t.Fatalf("silence normalized to %v", got)
	}
}

func TestPiperVoiceResolutionPrefersFilesTheUserHas(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	own := t.TempDir()
	for _, name := range []string{"mine.onnx", "tokens.txt"} {
		if err := os.WriteFile(filepath.Join(own, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	byPath, err := resolvePiperVoice(ctx, filepath.Join(own, "mine.onnx"), t.TempDir())
	if err != nil || byPath.Model != filepath.Join(own, "mine.onnx") {
		t.Fatalf("voice given by path = %+v, %v", byPath, err)
	}
	byDir, err := resolvePiperVoice(ctx, "mine", own)
	if err != nil || byDir.Model != filepath.Join(own, "mine.onnx") {
		t.Fatalf("voice found in the voices directory = %+v, %v", byDir, err)
	}
	var unknown *speech.UnknownVoiceError
	if _, err := resolvePiperVoice(ctx, "xx_XX-nobody-low", own); !errors.As(err, &unknown) {
		t.Fatalf("a voice outside the catalog resolved: %v", err)
	}
}

func TestPiperReadsUnavailableOnlyWhereTheEngineHasNoBuild(t *testing.T) {
	fixedTTSConfig(t, map[string]any{"provider": "piper"})
	ctx := context.Background()
	if _, problem := selectBuiltinEngine(ctx, "piper"); problem != "" || !checkTTSRequirements(ctx, nil) {
		t.Fatalf("piper unavailable on a supported platform: %s", problem)
	}
	swap(t, &ttsSpeechSupported, func() bool { return false })
	if _, problem := selectBuiltinEngine(ctx, "piper"); !strings.Contains(problem, "no build for this platform") || checkTTSRequirements(ctx, nil) {
		t.Fatalf("piper available without an engine build: %s", problem)
	}
}
