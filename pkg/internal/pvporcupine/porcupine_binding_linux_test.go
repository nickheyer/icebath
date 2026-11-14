//go:build linux && !386 && !arm && !faketime

package pvporcupine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// Compiles a C source of the test data into a shared library and returns its path
func buildLibrary(t *testing.T, source, name string) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Fatalf("a C compiler is needed to build %s: %v", source, err)
	}
	out := filepath.Join(t.TempDir(), name)
	if msg, err := exec.Command(cc, "-shared", "-fPIC", "-O0", "-ffp-contract=off", "-o", out, source).CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", source, err, msg)
	}
	return out
}

func TestBindingCallsEveryLibraryFunction(t *testing.T) {
	lib := buildLibrary(t, filepath.Join("testdata", "fake_pv_porcupine.c"), "libpv_porcupine.so")
	dir := t.TempDir()
	model, first, second := filepath.Join(dir, "params.pv"), filepath.Join(dir, "a.ppn"), filepath.Join(dir, "b.ppn")
	for _, path := range []string{model, first, second} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := NewPorcupine("key", lib, model, "cpu:1", []string{first, second}, []float64{0.25, 0.75})
	if err != nil {
		t.Fatal(err)
	}
	if want := "key|" + model + "|cpu:1|2|" + first + "|0.25|" + second + "|0.75"; p.Version() != want {
		t.Fatalf("library received %q, want %q", p.Version(), want)
	}
	if p.FrameLength() != 512 || p.SampleRate() != 16000 {
		t.Fatalf("frame length %d sample rate %d", p.FrameLength(), p.SampleRate())
	}
	frame := make([]int16, 512)
	for want := -1; want <= 1; want++ {
		frame[511] = int16(want + 1)
		if got, err := p.Process(frame); err != nil || got != want {
			t.Fatalf("process gave %d %v, want %d", got, err, want)
		}
	}
	_, err = p.Process(frame[:100])
	wantPyError(t, err, "ValueError", "Invalid frame length. expected 512 but received 100")
	frame[0] = -1
	var pe *PorcupineError
	if _, err = p.Process(frame); !errors.As(err, &pe) || pe.Class != "PorcupineInvalidStateError" ||
		pe.Error() != "Processing failed:\n  [0] first frame\n  [1] second frame" || pe.Message != "Processing failed" {
		t.Fatalf("failing process gave %v", err)
	}
	if _, err = NewPorcupine("refused", lib, model, "best", []string{first}, []float64{0.5}); !errors.As(err, &pe) ||
		pe.Class != "PorcupineActivationRefusedError" || !reflect.DeepEqual(pe.MessageStack, []string{"first frame", "second frame"}) ||
		pe.Error() != "Initialization failed:\n  [0] first frame\n  [1] second frame" {
		t.Fatalf("refused key gave %v", err)
	}
	devices, err := ListHardwareDevices(lib)
	if err != nil || !reflect.DeepEqual(devices, []string{"best", "cpu:2"}) {
		t.Fatalf("devices %v %v", devices, err)
	}
	p.Delete()
	m := &Module{Dir: dir, System: "Linux", Machine: "x86_64", KeywordPaths: map[string]string{"jarvis": first}}
	handle, err := m.Create("key", CreateOptions{LibraryPath: &lib, ModelPath: &model, Keywords: []string{"jarvis"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "key|" + model + "|best|1|" + first + "|0.50"; handle.Version() != want {
		t.Fatalf("factory passed %q, want %q", handle.Version(), want)
	}
	if got, err := m.AvailableDevices(&lib); err != nil || !reflect.DeepEqual(got, []string{"best", "cpu:2"}) {
		t.Fatalf("factory devices %v %v", got, err)
	}
	handle.Delete()
}
