//go:build linux && !386 && !arm && !faketime

package sherpaonnx

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
)

func TestBindingCallsEveryLibraryFunction(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Fatalf("a C compiler is needed to build the test library: %v", err)
	}
	pkg := filepath.Join(t.TempDir(), "sherpa_onnx")
	if err := os.MkdirAll(filepath.Join(pkg, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(pkg, "lib", "libsherpa-onnx-c-api.so")
	if msg, err := exec.Command(cc, "-shared", "-fPIC", "-O0", "-o", lib, filepath.Join("testdata", "fake_sherpa_onnx_c_api.c")).CombinedOutput(); err != nil {
		t.Fatalf("building the test library: %v\n%s", err, msg)
	}
	m, err := Import(pkg, Environment{})
	if err != nil {
		t.Fatal(err)
	}
	o := modelFiles(t)
	o.NumThreads, o.KeywordsThreshold, o.KeywordsScore, o.MaxActivePaths, o.NumTrailingBlanks = 3, 0.375, 1.5, 6, 2
	spotter, err := m.NewKeywordSpotter(o)
	if err != nil {
		t.Fatal(err)
	}
	config := strings.Join([]string{"16000", "80", o.Encoder, o.Decoder, o.Joiner, "<null>", "<null>", "<null>", o.Tokens, "3", "cpu", "0", "<null>",
		"<null>", "<null>", "<null>", "0", "<null>", "<null>", "6", "2", "1.500", "0.375", o.KeywordsFile, "<null>", "0"}, "|")
	stream := spotter.CreateStream(nil)
	if got := spotter.GetResult(stream); got != config+"#0#0#0.00#0#0#0#" {
		t.Fatalf("library received %q\nwant              %q", got, config+"#0#0#0.00#0#0#0#")
	}
	if tokens, stamps := spotter.Tokens(stream), spotter.Timestamps(stream); len(tokens) != 0 || len(stamps) != 0 {
		t.Fatalf("empty stream gave tokens %v timestamps %v", tokens, stamps)
	}
	samples := make([]float32, 1280)
	samples[0], samples[1279] = 0.5, 0.25
	stream.AcceptWaveform(16000, samples)
	decodes := 0
	for spotter.IsReady(stream) {
		spotter.DecodeStream(stream)
		decodes++
	}
	stream.InputFinished()
	if got := spotter.GetResult(stream); decodes != 2 || got != config+"#16000#1280#0.75#0#1#0#" {
		t.Fatalf("after %d decodes the library reports %q", decodes, got)
	}
	if got := spotter.Tokens(stream); !reflect.DeepEqual(got, []string{"▁HE", "Y"}) {
		t.Fatalf("tokens %v", got)
	}
	if got := spotter.Timestamps(stream); !reflect.DeepEqual(got, []float32{0.04, 0.32}) {
		t.Fatalf("timestamps %v", got)
	}
	extra := "▁HE Y @HEY"
	other := spotter.CreateStream(&extra)
	other.AcceptWaveform(8000, samples[:640])
	stream.AcceptWaveform(16000, samples)
	spotter.DecodeStreams([]*OnlineStream{stream, other})
	spotter.ResetStream(stream)
	if got := spotter.GetResult(other); got != config+"#8000#640#0.50#0#0#0#▁HE Y @HEY" {
		t.Fatalf("second stream reports %q", got)
	}
	if got := spotter.GetResult(stream); got != config+"#16000#0#0.00#0#1#1#" {
		t.Fatalf("reset stream reports %q", got)
	}
	o.KeywordsFile = filepath.Join(filepath.Dir(o.KeywordsFile), "refused.txt")
	_, err = m.NewKeywordSpotter(o)
	wantPyError(t, err, "RuntimeError", "Errors in config!")

	cdll, err := pyctypes.LoadLibrary(lib, false)
	if err != nil {
		t.Fatal(err)
	}
	var live func(kind int32) int32
	addr, err := cdll.Symbol("fake_live_objects")
	if err != nil {
		t.Fatal(err)
	}
	pyctypes.RegisterFunc(&live, addr)
	if live(0) != 1 || live(1) != 2 || live(2) != 0 {
		t.Fatalf("library holds %d spotters %d streams %d results, want 1 2 0", live(0), live(1), live(2))
	}
	runtime.KeepAlive(stream)
	runtime.KeepAlive(other)
	runtime.KeepAlive(spotter)
}
