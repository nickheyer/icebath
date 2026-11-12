//go:build wakewheels

package pyopenwakeword

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Directory holding every member of a wheel archive written with its recorded permissions
func unpackWheel(t *testing.T, wheel string) string {
	t.Helper()
	r, err := zip.OpenReader(wheel)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dir := t.TempDir()
	for _, f := range r.File {
		target := filepath.Join(dir, filepath.FromSlash(f.Name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		src, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(src)
		src.Close()
		if err != nil {
			t.Fatal(err)
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Sample bytes of the data chunk of a RIFF WAVE file
func wavData(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for pos := 12; pos+8 <= len(raw); {
		size := int(binary.LittleEndian.Uint32(raw[pos+4:]))
		if bytes.Equal(raw[pos:pos+4], []byte("data")) {
			return raw[pos+8 : min(pos+8+size, len(raw))]
		}
		pos += 8 + size + size%2
	}
	t.Fatalf("%s has no data chunk", path)
	return nil
}

func TestRealLibraryMatchesPython(t *testing.T) {
	root := unpackWheel(t, filepath.Join("testdata", "pyopen_wakeword", "pyopen_wakeword-1.1.0-py3-none-manylinux_2_35_x86_64.whl"))
	raw, err := os.ReadFile("testdata/real_run.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref map[string]struct {
		ID           string `json:"id"`
		InputWindows int    `json:"input_windows"`
		Frames       []struct {
			Embs   [][]uint32 `json:"embs"`
			Scores []uint32   `json:"scores"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	audio := wavData(t, filepath.Join("testdata", "sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01", "test_wavs", "0.wav"))
	m := Import(filepath.Join(root, "pyopen_wakeword"))
	for name, want := range ref {
		var model *OpenWakeWord
		if name == "okay_nabu" {
			model, err = m.FromBuiltin(ModelOkayNabu, nil, nil)
		} else {
			model, err = m.FromModel(filepath.Join("..", "..", "media", "wakewords", name+".tflite"), nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		features, err := m.FeaturesFromBuiltin(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if model.ID() != want.ID || model.inputWindows != want.InputWindows {
			t.Fatalf("%s id %q windows %d, Python has %q %d", name, model.ID(), model.inputWindows, want.ID, want.InputWindows)
		}
		frame := 0
		for i := 0; i+2*SamplesPerChunk <= len(audio); i += 2 * SamplesPerChunk {
			embs, scores := [][]uint32{}, []uint32{}
			err := features.ProcessStreaming(audio[i:i+2*SamplesPerChunk], func(emb []float32) bool {
				embs = append(embs, []uint32{uint32(len(emb)), math.Float32bits(emb[0]), math.Float32bits(emb[47]), math.Float32bits(emb[95])})
				if err := model.ProcessStreaming(emb, func(score float32) bool {
					scores = append(scores, math.Float32bits(score))
					return true
				}); err != nil {
					t.Fatal(err)
				}
				return true
			})
			if err != nil {
				t.Fatal(err)
			}
			if frame >= len(want.Frames) {
				t.Fatalf("%s produced more than the %d frames Python scored", name, len(want.Frames))
			}
			py := want.Frames[frame]
			if py.Embs == nil {
				py.Embs = [][]uint32{}
			}
			if py.Scores == nil {
				py.Scores = []uint32{}
			}
			if !reflect.DeepEqual(embs, py.Embs) || !reflect.DeepEqual(scores, py.Scores) {
				t.Fatalf("%s frame %d embeddings %v scores %v, Python has %v and %v", name, frame, embs, scores, py.Embs, py.Scores)
			}
			frame++
		}
		if frame != len(want.Frames) {
			t.Fatalf("%s scored %d frames, Python scored %d", name, frame, len(want.Frames))
		}
		features.Close()
		model.Close()
	}
}
