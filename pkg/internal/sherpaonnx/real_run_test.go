//go:build wakewheels

package sherpaonnx

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypinyin"
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

// Samples of the data chunk of a 16 bit RIFF WAVE file scaled to the range -1 to 1
func wavSamples(t *testing.T, path string, padding int) []float32 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for pos := 12; pos+8 <= len(raw); {
		size := int(binary.LittleEndian.Uint32(raw[pos+4:]))
		if bytes.Equal(raw[pos:pos+4], []byte("data")) {
			data := raw[pos+8 : min(pos+8+size, len(raw))]
			out := make([]float32, len(data)/2+padding)
			for i := 0; i < len(data)/2; i++ {
				out[i] = float32(int16(binary.LittleEndian.Uint16(data[2*i:]))) / 32768.0
			}
			return out
		}
		pos += 8 + size + size%2
	}
	t.Fatalf("%s has no data chunk", path)
	return nil
}

func TestRealLibraryMatchesPython(t *testing.T) {
	core := unpackWheel(t, filepath.Join("testdata", "sherpa_onnx_core", "sherpa_onnx_core-1.13.8-py3-none-manylinux2014_x86_64.whl"))
	pinyin := unpackWheel(t, filepath.Join("testdata", "pypinyin", "pypinyin-0.55.0-py2.py3-none-any.whl"))
	modelDir := filepath.Join("testdata", "sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01")
	raw, err := os.ReadFile("testdata/real_run.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Text2Token map[string][]json.RawMessage `json:"text2token"`
		Pinyin     [][]json.RawMessage          `json:"pinyin"`
		Syllables  [][3]string                  `json:"syllables"`
		Wavs       map[string][][]any           `json:"wavs"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	py, err := pypinyin.Import(filepath.Join(pinyin, "pypinyin"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := Import(filepath.Join(core, "sherpa_onnx"), Environment{Stdout: os.Stdout, ImportSentencepiece: func() error { return nil },
		ImportPypinyin: func() (*pypinyin.Module, error) { return py, nil }})
	if err != nil {
		t.Fatal(err)
	}
	tokens, bpe := filepath.Join(modelDir, "tokens.txt"), filepath.Join(modelDir, "bpe.model")
	var texts []string
	var wantTokens [][]any
	if err := json.Unmarshal(ref.Text2Token["bpe"][0], &texts); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ref.Text2Token["bpe"][1], &wantTokens); err != nil {
		t.Fatal(err)
	}
	got, err := m.Text2Token(texts, tokens, Text2TokenOptions{TokensType: "bpe", BPEModel: &bpe})
	if err != nil || !reflect.DeepEqual(got, wantTokens) {
		t.Fatalf("text2token %v %v, Python has %v", got, err, wantTokens)
	}
	for _, entry := range ref.Pinyin {
		var text string
		var want []string
		if err := json.Unmarshal(entry[0], &text); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(entry[1], &want); err != nil {
			t.Fatal(err)
		}
		readings, err := py.Pinyin(text)
		if err != nil {
			t.Fatal(err)
		}
		first := []string{}
		for _, reading := range readings {
			first = append(first, reading[0])
		}
		if !reflect.DeepEqual(first, want) {
			t.Errorf("pinyin of %q is %q, Python has %q", text, first, want)
		}
	}
	for _, s := range ref.Syllables {
		if initial, final := pypinyin.ToInitialsNotStrict(s[0]), pypinyin.ToFinalsToneNotStrict(s[0]); initial != s[1] || final != s[2] {
			t.Errorf("%q split into %q and %q, Python has %q and %q", s[0], initial, final, s[1], s[2])
		}
	}
	for name, want := range ref.Wavs {
		o := DefaultKeywordSpotterOptions(tokens, filepath.Join(modelDir, "encoder-epoch-12-avg-2-chunk-16-left-64.onnx"),
			filepath.Join(modelDir, "decoder-epoch-12-avg-2-chunk-16-left-64.onnx"), filepath.Join(modelDir, "joiner-epoch-12-avg-2-chunk-16-left-64.onnx"),
			filepath.Join(modelDir, "test_wavs", "test_keywords.txt"))
		o.NumThreads = 1
		spotter, err := m.NewKeywordSpotter(o)
		if err != nil {
			t.Fatal(err)
		}
		stream := spotter.CreateStream(nil)
		pcm := wavSamples(t, filepath.Join(modelDir, "test_wavs", name), int(0.66*16000))
		hits := [][]any{}
		for n, i := 0, 0; i+1280 <= len(pcm); n, i = n+1, i+1280 {
			stream.AcceptWaveform(16000, pcm[i:i+1280])
			for spotter.IsReady(stream) {
				spotter.DecodeStream(stream)
				if r := spotter.GetResult(stream); r != "" {
					toks := []any{}
					for _, tok := range spotter.Tokens(stream) {
						toks = append(toks, tok)
					}
					hits = append(hits, []any{float64(n), r, toks})
					spotter.ResetStream(stream)
				}
			}
		}
		if want == nil {
			want = [][]any{}
		}
		if !reflect.DeepEqual(hits, want) {
			t.Errorf("%s keywords %v, Python has %v", name, hits, want)
		}
	}
}
