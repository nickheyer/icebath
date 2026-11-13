package speech

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/fetch"
)

// Serves the fixture archives in place of the release host and pins them
func fixtureHost(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	server := httptest.NewServer(http.FileServer(http.Dir("testdata")))
	t.Cleanup(server.Close)
	pin := func(name string) pinned {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		return pinned{int64(len(data)), hex.EncodeToString(sum[:])}
	}
	savedBase, savedEspeak := voiceBase, espeakData
	voiceBase = server.URL + "/"
	espeak := pin("espeak-ng-data.tar.bz2")
	espeakData = fetch.Asset{Name: "espeak-ng-data.tar.bz2", URL: voiceBase + "espeak-ng-data.tar.bz2", Size: espeak.size, SHA256: espeak.sha256}
	piperArchives["xx_XX-test-low"] = pin("vits-piper-xx_XX-test-low.tar.bz2")
	t.Cleanup(func() {
		voiceBase, espeakData = savedBase, savedEspeak
		delete(piperArchives, "xx_XX-test-low")
	})
}

func TestFetchPiperUnpacksThePinnedVoiceWithItsScales(t *testing.T) {
	fixtureHost(t)
	voice, err := FetchPiper(context.Background(), "xx_XX-test-low")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(voice.Model) != "xx_XX-test-low.onnx" || voice.NoiseScale != 0.333 || voice.LengthScale != 1.1 || voice.NoiseW != 0.5 {
		t.Fatalf("voice = %+v", voice)
	}
	for _, path := range []string{voice.Model, voice.Tokens, filepath.Join(voice.DataDir, "phontab")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	options := voice.Options(2)
	if options.NoiseScale != 0.333 || options.NoiseScaleW != 0.5 || options.Threads != 2 || options.Voices != "" {
		t.Fatalf("options = %+v", options)
	}
	var unknown *UnknownVoiceError
	if _, err := FetchPiper(context.Background(), "en_XX-nobody-high"); !errors.As(err, &unknown) {
		t.Fatalf("unknown voice error = %v", err)
	}
	piperArchives["xx_XX-test-low"] = pinned{piperArchives["xx_XX-test-low"].size, strings.Repeat("0", 64)}
	var digest *fetch.DigestError
	if _, err := FetchPiper(context.Background(), "xx_XX-test-low"); !errors.As(err, &digest) {
		t.Fatalf("a voice with the wrong digest loaded: %v", err)
	}
}

func TestAdoptPiperConvertsAPublishedVoiceOnce(t *testing.T) {
	fixtureHost(t)
	dir := t.TempDir()
	model := filepath.Join(dir, "de_DE-thorsten-medium.onnx")
	os.WriteFile(model, []byte("onnx-bytes"), 0o644)
	os.WriteFile(model+".json", []byte(`{"audio":{"sample_rate":22050},"espeak":{"voice":"de"},"inference":{"noise_scale":0.5},
		"phoneme_type":"espeak","phoneme_id_map":{"a":[14],"_":[0]," ":[3]},"num_speakers":2,"language":{"name_native":"Deutsch"}}`), 0o644)
	voice, err := AdoptPiper(context.Background(), model)
	if err != nil {
		t.Fatal(err)
	}
	converted, _ := os.ReadFile(voice.Model)
	want := []byte("onnx-bytes")
	for _, kv := range [][2]string{{"model_type", "vits"}, {"comment", "piper"}, {"language", "Deutsch"}, {"voice", "de"}, {"has_espeak", "1"}, {"n_speakers", "2"}, {"sample_rate", "22050"}} {
		want = append(want, 0x72, byte(4+len(kv[0])+len(kv[1])), 0x0a, byte(len(kv[0])))
		want = append(append(append(want, kv[0]...), 0x12, byte(len(kv[1]))), kv[1]...)
	}
	if !bytes.Equal(converted, want) {
		t.Fatalf("converted model = %q", converted)
	}
	if tokens, _ := os.ReadFile(voice.Tokens); string(tokens) != "_ 0\n  3\na 14\n" {
		t.Fatalf("tokens = %q", tokens)
	}
	if voice.NoiseScale != 0.5 || voice.NoiseW != 0.8 || voice.LengthScale != 1.0 {
		t.Fatalf("scales = %+v", voice)
	}
	if _, err := os.Stat(filepath.Join(voice.DataDir, "phontab")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(voice.Model)
	again, err := AdoptPiper(context.Background(), model)
	after, _ := os.Stat(voice.Model)
	if err != nil || again != voice || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("second adoption converted again: %v", err)
	}
}

func TestAdoptPiperAcceptsEngineVoicesAndRefusesTheRest(t *testing.T) {
	fixtureHost(t)
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready.onnx")
	os.WriteFile(ready, []byte("m"), 0o644)
	if _, err := AdoptPiper(context.Background(), ready); err == nil || !strings.Contains(err.Error(), "ready.onnx.json") {
		t.Fatalf("bare model error = %v", err)
	}
	os.WriteFile(filepath.Join(dir, "tokens.txt"), []byte("_ 0\n"), 0o644)
	voice, err := AdoptPiper(context.Background(), ready)
	if err != nil || voice.Model != ready || voice.Tokens != filepath.Join(dir, "tokens.txt") || voice.NoiseScale != 0.667 {
		t.Fatalf("engine voice = %+v, %v", voice, err)
	}
	text := filepath.Join(t.TempDir(), "text.onnx")
	os.WriteFile(text, []byte("m"), 0o644)
	os.WriteFile(text+".json", []byte(`{"audio":{"sample_rate":22050},"espeak":{"voice":"ar"},"phoneme_type":"text","phoneme_id_map":{"a":[1]}}`), 0o644)
	if _, err := AdoptPiper(context.Background(), text); err == nil || !strings.Contains(err.Error(), "text phonemes") {
		t.Fatalf("text phoneme voice error = %v", err)
	}
	os.WriteFile(text+".json", []byte(`{"phoneme_type":"espeak"}`), 0o644)
	if _, err := AdoptPiper(context.Background(), text); err == nil || !strings.Contains(err.Error(), "lacks") {
		t.Fatalf("incomplete description error = %v", err)
	}
	if _, err := AdoptPiper(context.Background(), filepath.Join(dir, "absent.onnx")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing model error = %v", err)
	}
}

func TestCatalogListsEveryVoiceInOrder(t *testing.T) {
	names := PiperVoices()
	if len(names) < 180 || names[0] >= names[1] {
		t.Fatalf("catalog holds %d voices", len(names))
	}
	for name, pin := range piperArchives {
		if len(pin.sha256) != 64 || pin.size == 0 {
			t.Fatalf("%s is not pinned", name)
		}
	}
	if _, ok := piperArchives["en_US-lessac-medium"]; !ok {
		t.Fatal("the default voice is missing from the catalog")
	}
}
