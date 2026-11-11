package sherpaonnx

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	os.Exit(m.Run())
}

func wantPyError(t *testing.T, err error, class, msg string) {
	t.Helper()
	var pe *PyError
	if !errors.As(err, &pe) || pe.Class != class || pe.Msg != msg {
		t.Fatalf("error %v, want %s %q", err, class, msg)
	}
}

func cstr(p *byte) string {
	if p == nil {
		return "<null>"
	}
	return pyctypes.GoString(uintptr(unsafe.Pointer(p)))
}

// Records every call a spotter makes and serves one keyword result built in Go memory
type fakeAPI struct {
	calls  []string
	config map[string]any
	keep   []any
	ready  int32
	fail   bool
}

func (f *fakeAPI) api() *capi {
	log := func(name string) { f.calls = append(f.calls, name) }
	return &capi{
		createKeywordSpotter: func(c *cKeywordSpotterConfig) uintptr {
			f.config = map[string]any{"sample_rate": c.featConfig.sampleRate, "feature_dim": c.featConfig.featureDim,
				"encoder": cstr(c.modelConfig.transducerEncoder), "decoder": cstr(c.modelConfig.transducerDecoder),
				"joiner": cstr(c.modelConfig.transducerJoiner), "tokens": cstr(c.modelConfig.tokens), "num_threads": c.modelConfig.numThreads,
				"provider": cstr(c.modelConfig.provider), "debug": c.modelConfig.debug, "model_type": cstr(c.modelConfig.modelType),
				"paraformer": cstr(c.modelConfig.paraformerEncoder), "tokens_buf": cstr(c.modelConfig.tokensBuf),
				"max_active_paths": c.maxActivePaths, "num_trailing_blanks": c.numTrailingBlanks, "keywords_score": c.keywordsScore,
				"keywords_threshold": c.keywordsThreshold, "keywords_file": cstr(c.keywordsFile), "keywords_buf": cstr(c.keywordsBuf),
				"keywords_buf_size": c.keywordsBufSize}
			if f.fail {
				return 0
			}
			return 7
		},
		destroyKeywordSpotter: func(uintptr) {},
		createKeywordStream:   func(uintptr) uintptr { log("create_stream"); return 21 },
		createKeywordStreamWithKeywords: func(_ uintptr, keywords *byte) uintptr {
			log("create_stream:" + cstr(keywords))
			return 22
		},
		isKeywordStreamReady: func(spotter, stream uintptr) int32 { log("is_ready"); return f.ready },
		decodeKeywordStream:  func(spotter, stream uintptr) { log("decode") },
		resetKeywordStream:   func(spotter, stream uintptr) { log("reset") },
		decodeMultipleKeywordStreams: func(_ uintptr, streams *uintptr, n int32) {
			log("decode_many:" + strings.Join(strings.Fields(strings.Trim(strings.ReplaceAll(
				strings.Join(func() []string {
					out := []string{}
					for _, h := range unsafe.Slice(streams, n) {
						out = append(out, string(rune('0'+h-20)))
					}
					return out
				}(), " "), "  ", " "), " ")), ","))
		},
		getKeywordResult: func(spotter, stream uintptr) uintptr {
			if stream == 22 {
				return 0
			}
			keyword, t1, t2 := pyctypes.CString(" HEY_ICEBATH "), pyctypes.CString("▁HE"), pyctypes.CString("Y")
			arr := []uintptr{uintptr(unsafe.Pointer(&t1[0])), uintptr(unsafe.Pointer(&t2[0]))}
			stamps := []float32{0.25, 0.5}
			res := &cKeywordResult{keyword: uintptr(unsafe.Pointer(&keyword[0])), tokensArr: uintptr(unsafe.Pointer(&arr[0])), count: 2,
				timestamps: uintptr(unsafe.Pointer(&stamps[0]))}
			f.keep = append(f.keep, keyword, t1, t2, arr, stamps, res)
			return uintptr(unsafe.Pointer(res))
		},
		destroyKeywordResult: func(uintptr) { log("destroy_result") },
		destroyOnlineStream:  func(uintptr) {},
		onlineStreamAcceptWaveform: func(stream uintptr, rate int32, samples *float32, n int32) {
			sum := float32(0)
			for _, v := range unsafe.Slice(samples, n) {
				sum += v
			}
			log("accept:" + strings.Repeat("x", int(n)) + ":" + map[bool]string{true: "16k", false: "other"}[rate == 16000] +
				":" + map[bool]string{true: "sum1", false: "sum?"}[sum == 1])
		},
		onlineStreamInputFinished: func(uintptr) { log("input_finished") },
	}
}

func modelFiles(t *testing.T) KeywordSpotterOptions {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, 5)
	for i, name := range []string{"tokens.txt", "encoder.onnx", "decoder.onnx", "joiner.onnx", "keywords.txt"} {
		paths[i] = filepath.Join(dir, name)
		if err := os.WriteFile(paths[i], []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return DefaultKeywordSpotterOptions(paths[0], paths[1], paths[2], paths[3], paths[4])
}

func TestKeywordSpotterConfigReachesTheLibrary(t *testing.T) {
	fake := &fakeAPI{}
	m := &Module{api: fake.api()}
	o := modelFiles(t)
	o.NumThreads, o.KeywordsThreshold = 1, 0.25
	if _, err := m.NewKeywordSpotter(o); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"sample_rate": int32(16000), "feature_dim": int32(80), "encoder": o.Encoder, "decoder": o.Decoder, "joiner": o.Joiner,
		"tokens": o.Tokens, "num_threads": int32(1), "provider": "cpu", "debug": int32(0), "model_type": "<null>", "paraformer": "<null>",
		"tokens_buf": "<null>", "max_active_paths": int32(4), "num_trailing_blanks": int32(1), "keywords_score": float32(1),
		"keywords_threshold": float32(0.25), "keywords_file": o.KeywordsFile, "keywords_buf": "<null>", "keywords_buf_size": int32(0)}
	if !reflect.DeepEqual(fake.config, want) {
		t.Fatalf("config %v\nwant   %v", fake.config, want)
	}
	fake.fail = true
	_, err := m.NewKeywordSpotter(o)
	wantPyError(t, err, "RuntimeError", "Errors in config!")
}

func TestKeywordSpotterArgumentChecks(t *testing.T) {
	m := &Module{api: (&fakeAPI{}).api()}
	base := modelFiles(t)
	for _, field := range []*string{&base.Tokens, &base.Encoder, &base.Decoder, &base.Joiner} {
		o := base
		kept := *field
		*field = kept + ".absent"
		o = base
		_, err := m.NewKeywordSpotter(o)
		wantPyError(t, err, "AssertionError", kept+".absent does not exist")
		*field = kept
	}
	o := base
	o.Tokens = filepath.Dir(base.Tokens)
	_, err := m.NewKeywordSpotter(o)
	wantPyError(t, err, "AssertionError", o.Tokens+" does not exist")
	o = base
	o.NumThreads = 0
	_, err = m.NewKeywordSpotter(o)
	wantPyError(t, err, "AssertionError", "0")
	o.NumThreads = -3
	_, err = m.NewKeywordSpotter(o)
	wantPyError(t, err, "AssertionError", "-3")
	for name, set := range map[string]func(*KeywordSpotterOptions){
		"sample_rate": func(o *KeywordSpotterOptions) { o.SampleRate = 0 }, "feature_dim": func(o *KeywordSpotterOptions) { o.FeatureDim = 0 },
		"max_active_paths":    func(o *KeywordSpotterOptions) { o.MaxActivePaths = 0 },
		"keywords_score":      func(o *KeywordSpotterOptions) { o.KeywordsScore = 0 },
		"keywords_threshold":  func(o *KeywordSpotterOptions) { o.KeywordsThreshold = 0 },
		"num_trailing_blanks": func(o *KeywordSpotterOptions) { o.NumTrailingBlanks = 0 },
	} {
		o := base
		set(&o)
		_, err := m.NewKeywordSpotter(o)
		wantPyError(t, err, "ValueError", name+"=0 is replaced by the library default in the sherpa-onnx C API")
	}
	o = base
	o.Device = 1
	_, err = m.NewKeywordSpotter(o)
	wantPyError(t, err, "ValueError", "device=1 has no field in the sherpa-onnx C API keyword spotter config")
}

func TestKeywordSpotterDrivesTheLibrary(t *testing.T) {
	fake := &fakeAPI{ready: 1}
	m := &Module{api: fake.api()}
	spotter, err := m.NewKeywordSpotter(modelFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := spotter.CreateStream(nil)
	extra := "▁HE Y @HEY"
	other := spotter.CreateStream(&extra)
	stream.AcceptWaveform(16000, []float32{0.25, 0.75})
	stream.AcceptWaveform(8000, nil)
	stream.InputFinished()
	if !spotter.IsReady(stream) {
		t.Fatal("ready stream reported not ready")
	}
	spotter.DecodeStream(stream)
	spotter.DecodeStreams([]*OnlineStream{stream, other})
	if got := spotter.GetResult(stream); got != "HEY_ICEBATH" {
		t.Fatalf("result %q", got)
	}
	if got := spotter.Tokens(stream); !reflect.DeepEqual(got, []string{"▁HE", "Y"}) {
		t.Fatalf("tokens %v", got)
	}
	if got := spotter.Timestamps(stream); !reflect.DeepEqual(got, []float32{0.25, 0.5}) {
		t.Fatalf("timestamps %v", got)
	}
	if got := spotter.GetResult(other); got != "" {
		t.Fatalf("null result gave %q", got)
	}
	spotter.ResetStream(stream)
	fake.ready = 0
	if spotter.IsReady(stream) {
		t.Fatal("drained stream reported ready")
	}
	want := []string{"create_stream", "create_stream:▁HE Y @HEY", "accept:xx:16k:sum1", "accept::other:sum?", "input_finished", "is_ready", "decode",
		"decode_many:1,2", "destroy_result", "destroy_result", "destroy_result", "reset", "is_ready"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("calls %v\nwant  %v", fake.calls, want)
	}
	runtime.KeepAlive(fake)
}

func TestLibraryNames(t *testing.T) {
	for goos, want := range map[string]string{"linux": "libsherpa-onnx-c-api.so", "android": "libsherpa-onnx-c-api.so", "freebsd": "libsherpa-onnx-c-api.so",
		"darwin": "libsherpa-onnx-c-api.dylib", "windows": "sherpa-onnx-c-api.dll"} {
		if got := libraryName(goos); got != want {
			t.Errorf("%s library %q, want %q", goos, got, want)
		}
	}
	_, err := Import(t.TempDir(), Environment{})
	var pe *PyError
	if !errors.As(err, &pe) || pe.Class != "ImportError" || pe.Msg == "" {
		t.Fatalf("import without the library gave %v", err)
	}
}
