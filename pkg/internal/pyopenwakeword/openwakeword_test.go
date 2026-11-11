package pyopenwakeword

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	os.Exit(m.Run())
}

const fakeInputWindows = 16

// Arithmetic interpreters keyed by model file name, the same formulas the simulation script applies
type fakeLite struct {
	inputs  map[uintptr][]float32
	outputs map[uintptr][]float32
	deleted []string
	nullAt  string
}

func newFakeLite() *fakeLite {
	return &fakeLite{inputs: map[uintptr][]float32{}, outputs: map[uintptr][]float32{}}
}

func (f *fakeLite) ModelCreateFromFile(path string) uintptr {
	if f.nullAt == "model" {
		return 0
	}
	switch filepath.Base(path) {
	case "melspectrogram.tflite":
		return 1
	case "embedding_model.tflite":
		return 2
	}
	return 3
}

func (f *fakeLite) InterpreterCreate(model, options uintptr) uintptr {
	if f.nullAt == "interpreter" {
		return 0
	}
	return model + 10
}
func (f *fakeLite) InterpreterAllocateTensors(uintptr) int32 { return 0 }

func (f *fakeLite) InterpreterInvoke(interpreter uintptr) int32 {
	x := f.inputs[interpreter+100]
	var out []float32
	switch interpreter {
	case 11:
		out = make([]float32, 8*NumMels)
		for i := range out {
			out[i] = float32(x[(i*7)%MelSamples]*float32(0.001)) + float32(i)
		}
	case 12:
		out = make([]float32, WWFeatures)
		n := EmbFeatures * NumMels
		for j := range out {
			out[j] = float32(x[(j*25)%n]*float32(0.5)) + x[(j*25+1000)%n]
		}
	default:
		sum := float32(x[0]*float32(0.25)) + float32(x[fakeInputWindows*WWFeatures-1]*float32(0.125))
		out = []float32{sum + x[777]}
	}
	f.outputs[interpreter+200] = out
	return 0
}

func (f *fakeLite) InterpreterGetInputTensor(interpreter uintptr, index int32) (uintptr, error) {
	if f.nullAt == "input" {
		return notNull(0, "TfLiteInterpreterGetInputTensor")
	}
	return interpreter + 100, nil
}

func (f *fakeLite) InterpreterGetOutputTensor(interpreter uintptr, index int32) (uintptr, error) {
	if f.nullAt == "output" {
		return notNull(0, "TfLiteInterpreterGetOutputTensor")
	}
	return interpreter + 200, nil
}
func (f *fakeLite) TensorByteSize(tensor uintptr) uintptr { return uintptr(len(f.outputs[tensor]) * 4) }
func (f *fakeLite) TensorNumDims(uintptr) int32           { return 3 }
func (f *fakeLite) TensorDim(tensor uintptr, index int32) int32 {
	return []int32{1, fakeInputWindows, WWFeatures}[index]
}
func (f *fakeLite) InterpreterResizeInputTensor(uintptr, int32, []int32) int32 { return 0 }
func (f *fakeLite) TensorCopyFromBuffer(tensor uintptr, data []float32) int32 {
	f.inputs[tensor] = append([]float32(nil), data...)
	return 0
}
func (f *fakeLite) TensorCopyToBuffer(tensor uintptr, data []float32, size uintptr) int32 {
	copy(data, f.outputs[tensor][:size/4])
	return 0
}
func (f *fakeLite) InterpreterDelete(interpreter uintptr) {
	f.deleted = append(f.deleted, "interpreter")
}
func (f *fakeLite) ModelDelete(model uintptr) { f.deleted = append(f.deleted, "model") }

func fakePair(t *testing.T, lite *fakeLite) (*OpenWakeWordFeatures, *OpenWakeWord) {
	t.Helper()
	base := &TfLiteWakeWord{lib: lite}
	features, err := newOpenWakeWordFeatures("models/melspectrogram.tflite", "models/embedding_model.tflite", base)
	if err != nil {
		t.Fatal(err)
	}
	model, err := newOpenWakeWord("hey_hermes", "models/hey_hermes.tflite", base)
	if err != nil {
		t.Fatal(err)
	}
	return features, model
}

type simStep struct {
	Reset    bool       `json:"reset"`
	Size     int        `json:"size"`
	Embs     [][]uint32 `json:"embs"`
	Scores   []uint32   `json:"scores"`
	NewAudio int        `json:"new_audio"`
	NewMels  int        `json:"new_mels"`
	NewEmb   int        `json:"new_emb"`
}

// Replays the chunk plan of the numpy simulation and requires the same embeddings, scores and counters at every step
func replayNumpySimulation(t *testing.T, features *OpenWakeWordFeatures, model *OpenWakeWord) map[string][2]string {
	t.Helper()
	raw, err := os.ReadFile("testdata/streaming_sim.json")
	if err != nil {
		t.Fatal(err)
	}
	var sim struct {
		Steps  []simStep            `json:"steps"`
		Errors map[string][2]string `json:"errors"`
	}
	if err := json.Unmarshal(raw, &sim); err != nil {
		t.Fatal(err)
	}
	if model.ID() != "hey_hermes" || model.inputWindows != fakeInputWindows {
		t.Fatalf("model id %q windows %d", model.ID(), model.inputWindows)
	}
	state := uint64(12345)
	for n, step := range sim.Steps {
		if step.Reset {
			features.Reset()
			model.Reset()
			continue
		}
		chunk := make([]byte, 2*step.Size)
		for i := 0; i < step.Size; i++ {
			state = (state*1103515245 + 12345) % (1 << 31)
			binary.NativeEndian.PutUint16(chunk[2*i:], uint16(int16(int64((state>>8)%65536)-32768)))
		}
		embs, scores := [][]uint32{}, []uint32{}
		err := features.ProcessStreaming(chunk, func(emb []float32) bool {
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
			t.Fatalf("step %d size %d: %v", n, step.Size, err)
		}
		if step.Embs == nil {
			step.Embs = [][]uint32{}
		}
		if step.Scores == nil {
			step.Scores = []uint32{}
		}
		if !reflect.DeepEqual(embs, step.Embs) || !reflect.DeepEqual(scores, step.Scores) {
			t.Fatalf("step %d size %d: %d embeddings %d scores, numpy gave %d and %d", n, step.Size, len(embs), len(scores), len(step.Embs), len(step.Scores))
		}
		if features.newAudioSamples != step.NewAudio || features.newMels != step.NewMels || model.newEmbeddings != step.NewEmb {
			t.Fatalf("step %d counters %d %d %d, numpy gave %d %d %d", n, features.newAudioSamples, features.newMels, model.newEmbeddings,
				step.NewAudio, step.NewMels, step.NewEmb)
		}
	}
	return sim.Errors
}

func TestStreamingArithmeticMatchesNumpy(t *testing.T) {
	features, model := fakePair(t, newFakeLite())
	sim := replayNumpySimulation(t, features, model)
	fresh := func() (*OpenWakeWordFeatures, *OpenWakeWord) { return fakePair(t, newFakeLite()) }
	wantErr := func(name string, err error) {
		t.Helper()
		var pe *PyError
		if !errors.As(err, &pe) || pe.Class != sim[name][0] || pe.Msg != sim[name][1] {
			t.Fatalf("%s error %v, numpy gave %v", name, err, sim[name])
		}
	}
	f, m := fresh()
	wantErr("empty", f.ProcessStreaming(nil, func([]float32) bool { return true }))
	wantErr("odd", f.ProcessStreaming([]byte("abc"), func([]float32) bool { return true }))
	wantErr("huge", f.ProcessStreaming(make([]byte, 2*160001), func([]float32) bool { return true }))
	wantErr("emb_many", m.ProcessStreaming(make([]float32, 81*96), func(float32) bool { return true }))
	wantErr("emb_none", m.ProcessStreaming(nil, func(float32) bool { return true }))
	err := m.ProcessStreaming(make([]float32, 100), func(float32) bool { return true })
	var pe *PyError
	if !errors.As(err, &pe) || pe.Msg != "cannot reshape array of size 100 into shape (1,1,newaxis,96)" {
		t.Fatalf("ragged embedding error %v", err)
	}
}

func TestStreamingStopsWhenConsumerStopsAndAfterClose(t *testing.T) {
	lite := newFakeLite()
	features, model := fakePair(t, lite)
	groups := 0
	if err := features.ProcessStreaming(make([]byte, 2*1280), func([]float32) bool { groups++; return false }); err != nil || groups != 1 {
		t.Fatalf("stopped consumer saw %d groups, %v", groups, err)
	}
	if features.newMels == 0 {
		t.Fatal("pending mel windows were dropped when the consumer stopped")
	}
	features.Close()
	features.Close()
	model.Close()
	model.Close()
	if !reflect.DeepEqual(lite.deleted, []string{"interpreter", "model", "interpreter", "model", "interpreter", "model"}) {
		t.Fatalf("released %v", lite.deleted)
	}
	if err := features.ProcessStreaming(make([]byte, 2*1280), func([]float32) bool { t.Fatal("closed features yielded"); return true }); err != nil {
		t.Fatal(err)
	}
	if err := model.ProcessStreaming(make([]float32, 96), func(float32) bool { t.Fatal("closed model yielded"); return true }); err != nil {
		t.Fatal(err)
	}
}

func TestNullHandlesRaiseRuntimeError(t *testing.T) {
	for at, want := range map[string]string{"model": "TfLiteModelCreateFromFile returned NULL", "interpreter": "TfLiteInterpreterCreate returned NULL",
		"input": "TfLiteInterpreterGetInputTensor returned NULL", "output": "TfLiteInterpreterGetOutputTensor returned NULL"} {
		lite := newFakeLite()
		lite.nullAt = at
		base := &TfLiteWakeWord{lib: lite}
		var pe *PyError
		if _, err := newOpenWakeWord("x", "x.tflite", base); !errors.As(err, &pe) || pe.Class != "RuntimeError" || pe.Msg != want {
			t.Errorf("classifier with null %s gave %v", at, err)
		}
		if _, err := newOpenWakeWordFeatures("melspectrogram.tflite", "embedding_model.tflite", base); !errors.As(err, &pe) || pe.Msg != want {
			t.Errorf("features with null %s gave %v", at, err)
		}
	}
}

func TestGetPlatformTable(t *testing.T) {
	cases := []struct{ machine, system, want string }{
		{"x86_64", "linux", "linux_amd64"}, {"AMD64", "linux", "linux_amd64"}, {"aarch64", "linux", "linux_arm64"},
		{"armv7l", "linux", "linux_arm64"}, {"arm64", "linux2", "linux_arm64"}, {"riscv64", "linux", ""},
		{"AMD64", "win32", "windows_amd64"}, {"x86_64", "win32", "windows_amd64"}, {"ARM64", "win32", ""},
		{"arm64", "darwin", "darwin_arm64"}, {"x86_64", "darwin", ""}, {"amd64", "freebsd14", ""}, {"", "linux", ""},
	}
	for _, c := range cases {
		got, ok := getPlatform(c.machine, c.system)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s on %s gave %q %v, want %q", c.machine, c.system, got, ok, c.want)
		}
	}
}

func TestFindTensorflowliteC(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "pyopen_wakeword")
	touch := func(parts ...string) string {
		path := filepath.Join(parts...)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	wantValueError := func(err error, msg string) {
		t.Helper()
		var pe *PyError
		if !errors.As(err, &pe) || pe.Class != "ValueError" || pe.Msg != msg {
			t.Fatalf("error %v, want ValueError %q", err, msg)
		}
	}
	_, err := findTensorflowliteC(pkg, "", false)
	wantValueError(err, "Unable to detect platform for tensorflowlite_c")
	_, err = findTensorflowliteC(pkg, "linux_amd64", true)
	wantValueError(err, "Failed to find tensorflowlite_c library")
	touch(root, "lib", "linux_arm64", "libtensorflowlite_c.so")
	_, err = findTensorflowliteC(pkg, "linux_amd64", true)
	wantValueError(err, "Failed to find tensorflowlite_c library")
	repoLib := touch(root, "lib", "linux_amd64", "libtensorflowlite_c.so")
	if got, err := findTensorflowliteC(pkg, "linux_amd64", true); err != nil || got != repoLib {
		t.Fatalf("repository library %q %v", got, err)
	}
	touch(pkg, "lib", "notes.txt")
	moduleLib := touch(pkg, "lib", "tensorflowlite_c.dll")
	if got, err := findTensorflowliteC(pkg, "", false); err != nil || got != moduleLib {
		t.Fatalf("module library %q %v", got, err)
	}
	m := Import(pkg)
	if got, err := m.findTensorflowliteC(); err != nil || got != moduleLib {
		t.Fatalf("imported module library %q %v", got, err)
	}
	if !strings.HasSuffix(m.modelsDir(), filepath.Join("pyopen_wakeword", "models")) {
		t.Fatalf("models dir %q", m.modelsDir())
	}
}
