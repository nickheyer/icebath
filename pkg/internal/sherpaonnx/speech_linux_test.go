//go:build linux && !386 && !arm && !faketime

package sherpaonnx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/fetch"
	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
)

// Module over a library built from the real C header whose functions report the structs they were handed
func speechModule(t *testing.T) (*Module, func() string) {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Fatalf("a C compiler is needed to build the test library: %v", err)
	}
	pkg := filepath.Join(t.TempDir(), "sherpa-onnx")
	if err := os.MkdirAll(filepath.Join(pkg, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(pkg, "lib", "libsherpa-onnx-c-api.so")
	build := exec.Command(cc, "-shared", "-fPIC", "-O0", "-o", lib, filepath.Join("testdata", "fake_sherpa_onnx_c_api.c"), filepath.Join("testdata", "fake_speech.c"))
	if msg, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the test library: %v\n%s", err, msg)
	}
	m, err := Import(pkg, Environment{})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := m.lib.Symbol("FakeLastConfig")
	if err != nil {
		t.Fatal(err)
	}
	var lastConfig func() string
	pyctypes.RegisterFunc(&lastConfig, addr)
	return m, lastConfig
}

func touch(t *testing.T, names ...string) []string {
	t.Helper()
	dir := t.TempDir()
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = filepath.Join(dir, name)
		if err := os.WriteFile(out[i], []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestWhisperConfigAndAudioReachTheLibrary(t *testing.T) {
	m, lastConfig := speechModule(t)
	f := touch(t, "enc.onnx", "dec.onnx", "tokens.txt")
	rec, err := m.NewWhisper(WhisperOptions{Encoder: f[0], Decoder: f[1], Tokens: f[2], Language: "de", Threads: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"16000", "80", f[0], f[1], "de", "transcribe", "-1", f[2], "3", "cpu", "greedy_search", "<null>", "<null>", "<null>", "<null>"}, "|")
	if got := lastConfig(); got != want {
		t.Fatalf("library received %q\nwant              %q", got, want)
	}
	samples := make([]float32, 1600)
	samples[0], samples[1599] = 0.5, 0.25
	got, err := rec.Decode(16000, samples)
	if err != nil || got != (Transcript{Text: "rate=16000 n=1600 edge=0.75 decoded", Language: "en"}) {
		t.Fatalf("decode = %+v, %v", got, err)
	}
	if empty, err := rec.Decode(16000, nil); err != nil || empty != (Transcript{}) {
		t.Fatalf("empty audio = %+v, %v", empty, err)
	}
	rec.Close()
	if _, err := rec.Decode(16000, samples); err == nil {
		t.Fatal("a closed recognizer decoded")
	}
	if _, err := m.NewWhisper(WhisperOptions{Encoder: f[0], Decoder: filepath.Join(t.TempDir(), "absent.onnx"), Tokens: f[2]}); err == nil || !strings.Contains(err.Error(), "absent.onnx") {
		t.Fatalf("missing decoder error = %v", err)
	}
	bad := touch(t, "rejected.onnx")
	if _, err := m.NewWhisper(WhisperOptions{Encoder: bad[0], Decoder: f[1], Tokens: f[2]}); err == nil {
		t.Fatal("a model the library rejects loaded")
	}
}

func TestVoiceConfigsAndSamplesCrossTheBoundary(t *testing.T) {
	m, lastConfig := speechModule(t)
	f := touch(t, "voice.onnx", "tokens.txt", "voices.bin")
	data := t.TempDir()
	piper, err := m.NewVoice(VoiceOptions{Model: f[0], Tokens: f[1], DataDir: data, NoiseScale: 0.667, NoiseScaleW: 0.8, LengthScale: 1.25, Threads: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{f[0], f[1], data, "0.667", "0.800", "1.250", "2", "cpu", "<null>", "<null>", "<null>", "<null>", "0.000", "1", "0.20", "<null>", "<null>"}, "|")
	if got := lastConfig(); got != want {
		t.Fatalf("library received %q\nwant              %q", got, want)
	}
	audio, rate, err := piper.Speak("abc", 2, 1.5)
	if err != nil || rate != 22050 || !reflect.DeepEqual(audio, []float32{3.7, 4.7, 5.7}) || piper.Speakers() != 3 {
		t.Fatalf("speak = %v at %d, %v", audio, rate, err)
	}
	piper.Close()
	if _, _, err := piper.Speak("abc", 0, 1); err == nil {
		t.Fatal("a closed voice spoke")
	}
	kitten, err := m.NewVoice(VoiceOptions{Model: f[0], Tokens: f[1], Voices: f[2], DataDir: data, LengthScale: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	defer kitten.Close()
	want = strings.Join([]string{"<null>", "<null>", "<null>", "0.000", "0.000", "0.000", "1", "cpu", f[0], f[2], f[1], data, "0.900", "1", "0.20", "<null>", "<null>"}, "|")
	if got := lastConfig(); got != want {
		t.Fatalf("library received %q\nwant              %q", got, want)
	}
}

func TestSpeechSegmentsFeedsWholeWindowsAndCopiesEachSegment(t *testing.T) {
	m, lastConfig := speechModule(t)
	f := touch(t, "silero_vad.onnx")
	samples := make([]float32, 512*5+100)
	for i := range samples {
		samples[i] = float32(i / 512)
	}
	segments, err := m.SpeechSegments(VADOptions{Model: f[0], MinSilence: 0.5, MaxSpeech: 28}, samples)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{f[0], "0.50", "0.50", "0.25", "512", "28.0", "16000", "1", "cpu", "<null>", "29.2"}, "|")
	if got := lastConfig(); got != want {
		t.Fatalf("library received %q\nwant              %q", got, want)
	}
	wantSegments := []Segment{{Start: 0, Samples: []float32{0, 0}}, {Start: 1024, Samples: []float32{2, 1}}, {Start: 2048, Samples: []float32{4, 2}}}
	if !reflect.DeepEqual(segments, wantSegments) {
		t.Fatalf("segments = %+v", segments)
	}
}

func TestEveryPlatformArchiveIsPinned(t *testing.T) {
	for gpu, table := range map[bool]map[string]fetch.Asset{false: libraryArchives, true: gpuArchives} {
		for target := range table {
			goos, goarch, _ := strings.Cut(target, "/")
			asset, ok := libraryArchive(goos, goarch, gpu)
			if !ok || len(asset.SHA256) != 64 || asset.Size == 0 || !strings.HasPrefix(asset.URL, "https://github.com/k2-fsa/sherpa-onnx/releases/download/v"+Version+"/") {
				t.Fatalf("%s is not pinned: %+v", target, asset)
			}
		}
	}
	if _, ok := libraryArchive("plan9", "amd64", false); ok {
		t.Fatal("an unpublished platform has an archive")
	}
	if _, ok := libraryArchive("darwin", "arm64", true); ok {
		t.Fatal("a platform without a GPU build has a GPU archive")
	}
}

func TestProviderFollowsTheDeviceAndConflictingDevicesAreRefused(t *testing.T) {
	m, lastConfig := speechModule(t)
	m.GPU = true
	f := touch(t, "voice.onnx", "tokens.txt")
	voice, err := m.NewVoice(VoiceOptions{Model: f[0], Tokens: f[1]})
	if err != nil {
		t.Fatal(err)
	}
	voice.Close()
	if got := strings.Split(lastConfig(), "|")[7]; got != "cuda" {
		t.Fatalf("provider = %q on the GPU build", got)
	}
	loaded.module = m
	t.Cleanup(func() { loaded.module = nil })
	var device *DeviceError
	if _, err := Load(context.Background(), false); !errors.As(err, &device) || device.Running != "GPU" || device.Asked != "CPU" {
		t.Fatalf("conflicting device error = %v", err)
	}
	if again, err := Load(context.Background(), true); err != nil || again != m {
		t.Fatalf("matching device reload = %v", err)
	}
}
