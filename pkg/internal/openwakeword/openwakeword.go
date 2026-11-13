package openwakeword

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/fetch"
)

// Pipeline constants, 80 ms chunks at 16 kHz through mel, embedding and classifier windows
const (
	SampleRate      = 16000
	SamplesPerChunk = 1280
	autofillSamples = 8 * SampleRate
	maxSamples      = 10 * SampleRate
	melSamples      = 1760
	numMels         = 32
	maxMels         = 10 * 97
	embFeatures     = 76
	embStep         = 8
	maxEmb          = 10 * embStep
	wwFeatures      = 96
)

const wheelHost = "https://files.pythonhosted.org/packages/"

// Published package of each platform carrying the TensorFlow Lite C library and the shared models
var wheels = map[string]fetch.Asset{
	"darwin": {Name: "pyopen_wakeword-1.1.0-py3-none-macosx_15_0_universal2.whl", Size: 6746161,
		URL:    wheelHost + "1f/0a/45c9c7c1405f13e632d694ed1fac48e8c9f1421eed930a8cb1afc5eda17e/pyopen_wakeword-1.1.0-py3-none-macosx_15_0_universal2.whl",
		SHA256: "81f31adc31ba9e85b5c83f1b16f571f630ad867710f111443ecf42523f73fe08"},
	"linux/arm64": {Name: "pyopen_wakeword-1.1.0-py3-none-manylinux_2_35_aarch64.whl", Size: 6628214,
		URL:    wheelHost + "8c/74/746a3b3f81e2c1c69fc736c9e630f983797e413f7f58c1eac59d66d6687d/pyopen_wakeword-1.1.0-py3-none-manylinux_2_35_aarch64.whl",
		SHA256: "32a865123ebcc270ce6408b675456c3aecd8a6f0ddfe6311680616c9c01f990a"},
	"linux/amd64": {Name: "pyopen_wakeword-1.1.0-py3-none-manylinux_2_35_x86_64.whl", Size: 7132152,
		URL:    wheelHost + "f0/d7/228edc51d9b35b9d46cf685509e79144262ffd8756a49243a27da3302059/pyopen_wakeword-1.1.0-py3-none-manylinux_2_35_x86_64.whl",
		SHA256: "754347a59de2b3d378a0cbc404a8b41164036e74fa1c185f88056974e4bfb6b4"},
	"windows/amd64": {Name: "pyopen_wakeword-1.1.0-py3-none-win_amd64.whl", Size: 6997274,
		URL:    wheelHost + "69/d1/d788a1edf7c200b73c3a8b0cc73ebd8b1013e352222fb8521415dbb7e2c8/pyopen_wakeword-1.1.0-py3-none-win_amd64.whl",
		SHA256: "93c4ba03b434fb4130282a254000e58e29cf3dc49b610ce7e998abc785a27a90"},
}

// Pinned package of this platform, false where openWakeWord publishes none
func wheel() (fetch.Asset, bool) {
	if runtime.GOOS == "darwin" {
		a, ok := wheels["darwin"]
		return a, ok
	}
	a, ok := wheels[runtime.GOOS+"/"+runtime.GOARCH]
	return a, ok
}

// Whether a TensorFlow Lite build is published for this platform
func Supported() bool {
	_, ok := wheel()
	return ok && fetch.NativeSupported
}

// The bound TensorFlow Lite C library with the models directory of the package
type Library struct {
	Models string

	modelCreateFromFile  func(path *byte) uintptr
	interpreterCreate    func(model, options uintptr) uintptr
	allocateTensors      func(interpreter uintptr) int32
	invoke               func(interpreter uintptr) int32
	inputTensor          func(interpreter uintptr, index int32) uintptr
	outputTensor         func(interpreter uintptr, index int32) uintptr
	tensorByteSize       func(tensor uintptr) uintptr
	tensorNumDims        func(tensor uintptr) int32
	tensorDim            func(tensor uintptr, index int32) int32
	resizeInputTensor    func(interpreter uintptr, index int32, dims *int32, n int32) int32
	tensorCopyFromBuffer func(tensor uintptr, data unsafe.Pointer, size uintptr) int32
	tensorCopyToBuffer   func(tensor uintptr, data unsafe.Pointer, size uintptr) int32
	interpreterDelete    func(interpreter uintptr)
	modelDelete          func(model uintptr)
}

var loaded struct {
	sync.Mutex
	lib *Library
}

// Library of this platform, its package fetched into the icebath cache on first use
func Load(ctx context.Context) (*Library, error) {
	loaded.Lock()
	defer loaded.Unlock()
	if loaded.lib != nil {
		return loaded.lib, nil
	}
	asset, ok := wheel()
	if !ok {
		return nil, fmt.Errorf("openWakeWord publishes no TensorFlow Lite build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	tree, err := fetch.Tree(ctx, asset)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(tree, "pyopen_wakeword")
	libs, _ := filepath.Glob(filepath.Join(root, "lib", "*tensorflowlite_c.*"))
	if len(libs) == 0 {
		return nil, fmt.Errorf("the openWakeWord package at %s holds no TensorFlow Lite library", root)
	}
	native, err := fetch.OpenLibrary(libs[0])
	if err != nil {
		return nil, err
	}
	l := &Library{Models: filepath.Join(root, "models")}
	if err := native.BindAll([]fetch.Binding{{&l.modelCreateFromFile, "TfLiteModelCreateFromFile"},
		{&l.interpreterCreate, "TfLiteInterpreterCreate"}, {&l.allocateTensors, "TfLiteInterpreterAllocateTensors"},
		{&l.invoke, "TfLiteInterpreterInvoke"}, {&l.inputTensor, "TfLiteInterpreterGetInputTensor"},
		{&l.outputTensor, "TfLiteInterpreterGetOutputTensor"}, {&l.tensorByteSize, "TfLiteTensorByteSize"},
		{&l.tensorNumDims, "TfLiteTensorNumDims"}, {&l.tensorDim, "TfLiteTensorDim"},
		{&l.resizeInputTensor, "TfLiteInterpreterResizeInputTensor"}, {&l.tensorCopyFromBuffer, "TfLiteTensorCopyFromBuffer"},
		{&l.tensorCopyToBuffer, "TfLiteTensorCopyToBuffer"}, {&l.interpreterDelete, "TfLiteInterpreterDelete"},
		{&l.modelDelete, "TfLiteModelDelete"}}); err != nil {
		return nil, err
	}
	loaded.lib = l
	return l, nil
}

// Names of the wake word models the package ships
func (l *Library) Builtin() []string {
	entries, _ := os.ReadDir(l.Models)
	var out []string
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".tflite")
		if name != e.Name() && name != "melspectrogram" && name != "embedding_model" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Model file of a reference, a shipped model name or a path to a model file
func (l *Library) ModelPath(ref string) (string, error) {
	if !strings.ContainsAny(ref, `/\`) && !strings.HasSuffix(ref, ".tflite") {
		path := filepath.Join(l.Models, ref+".tflite")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("openWakeWord has no built in model %q, use a .tflite model file or one of %s", ref, strings.Join(l.Builtin(), ", "))
	}
	if strings.HasPrefix(ref, "~") {
		if dir, err := os.UserHomeDir(); err == nil {
			ref = filepath.Join(dir, strings.TrimPrefix(ref, "~"))
		}
	}
	abs, err := filepath.Abs(ref)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("openWakeWord model file %s is missing", abs)
	}
	return abs, nil
}

// One model with its interpreter and the tensors it reads and writes
type stage struct {
	lib         *Library
	model       uintptr
	interpreter uintptr
	input       uintptr
	output      uintptr
}

// Loads a model into an interpreter, resizing its input first when a shape is given
func (l *Library) stage(path string, shape []int32) (*stage, error) {
	s := &stage{lib: l, model: l.modelCreateFromFile(fetch.CString(path))}
	if s.model == 0 {
		return nil, fmt.Errorf("TensorFlow Lite could not load the model %s", path)
	}
	if s.interpreter = l.interpreterCreate(s.model, 0); s.interpreter == 0 {
		s.close()
		return nil, fmt.Errorf("TensorFlow Lite could not build an interpreter for %s", path)
	}
	if shape != nil {
		l.resizeInputTensor(s.interpreter, 0, &shape[0], int32(len(shape)))
	}
	if l.allocateTensors(s.interpreter) != 0 {
		s.close()
		return nil, fmt.Errorf("TensorFlow Lite could not allocate the tensors of %s", path)
	}
	s.input, s.output = l.inputTensor(s.interpreter, 0), l.outputTensor(s.interpreter, 0)
	if s.input == 0 || s.output == 0 {
		s.close()
		return nil, fmt.Errorf("the model %s has no input or output tensor", path)
	}
	return s, nil
}

// Runs the model over one input and returns its output values
func (s *stage) run(in []float32) ([]float32, error) {
	if s.lib.tensorCopyFromBuffer(s.input, unsafe.Pointer(&in[0]), uintptr(len(in))*4) != 0 {
		return nil, errors.New("TensorFlow Lite refused the input tensor")
	}
	if s.lib.invoke(s.interpreter) != 0 {
		return nil, errors.New("TensorFlow Lite inference failed")
	}
	out := make([]float32, s.lib.tensorByteSize(s.output)/4)
	if len(out) == 0 || s.lib.tensorCopyToBuffer(s.output, unsafe.Pointer(&out[0]), uintptr(len(out))*4) != 0 {
		return nil, errors.New("TensorFlow Lite returned no output")
	}
	return out, nil
}

func (s *stage) close() {
	if s.interpreter != 0 {
		s.lib.interpreterDelete(s.interpreter)
		s.interpreter = 0
	}
	if s.model != 0 {
		s.lib.modelDelete(s.model)
		s.model = 0
	}
}

// Shifts rows of a rolling buffer left and writes the new rows at its end
func shiftIn(buf, rows []float32) {
	if len(rows) >= len(buf) {
		copy(buf, rows[len(rows)-len(buf):])
		return
	}
	copy(buf, buf[len(rows):])
	copy(buf[len(buf)-len(rows):], rows)
}

// Window of size rows starting pending rows back from the end of a buffer
func window(buf []float32, pending, size, width int) []float32 {
	start := len(buf)/width - pending
	return buf[start*width : (start+size)*width]
}

// Turns streamed audio into embedding windows through the mel and embedding models
type Features struct {
	mel, emb *stage
	audio    []float32
	newAudio int
	mels     []float32
	newMels  int
}

// Mel and embedding stages of the package with eight seconds of silence counted as pending audio
func (l *Library) NewFeatures() (*Features, error) {
	mel, err := l.stage(filepath.Join(l.Models, "melspectrogram.tflite"), []int32{1, melSamples})
	if err != nil {
		return nil, err
	}
	emb, err := l.stage(filepath.Join(l.Models, "embedding_model.tflite"), []int32{1, embFeatures, numMels, 1})
	if err != nil {
		mel.close()
		return nil, err
	}
	f := &Features{mel: mel, emb: emb}
	f.Reset()
	return f, nil
}

// Feeds 16 bit native endian audio and yields every embedding that became ready
func (f *Features) Process(chunk []byte, yield func(embedding []float32) error) error {
	samples := make([]float32, len(chunk)/2)
	for i := range samples {
		samples[i] = float32(int16(binary.NativeEndian.Uint16(chunk[2*i:])))
	}
	shiftIn(f.audio, samples)
	f.newAudio = min(len(f.audio), f.newAudio+len(samples))
	for f.newAudio >= melSamples {
		in := append([]float32(nil), window(f.audio, f.newAudio, melSamples, 1)...)
		f.newAudio = max(0, f.newAudio-SamplesPerChunk)
		mels, err := f.mel.run(in)
		if err != nil {
			return err
		}
		for i, v := range mels {
			mels[i] = v/10 + 2
		}
		shiftIn(f.mels, mels)
		f.newMels = min(maxMels, f.newMels+len(mels)/numMels)
		for f.newMels >= embFeatures {
			in := append([]float32(nil), window(f.mels, f.newMels, embFeatures, numMels)...)
			f.newMels = max(0, f.newMels-embStep)
			emb, err := f.emb.run(in)
			if err != nil {
				return err
			}
			if err := yield(emb); err != nil {
				return err
			}
		}
	}
	return nil
}

// Clears the rolling audio and mel buffers back to the freshly built state
func (f *Features) Reset() {
	f.audio, f.newAudio = make([]float32, maxSamples), autofillSamples
	f.mels, f.newMels = make([]float32, maxMels*numMels), 0
}

func (f *Features) Close() {
	f.mel.close()
	f.emb.close()
}

// One wake word classifier over rolling embedding windows
type Model struct {
	ID         string
	stage      *stage
	windows    int
	embeddings []float32
	pending    int
}

// Loads a classifier model file and reads its window count from the input shape
func (l *Library) NewModel(path string) (*Model, error) {
	s, err := l.stage(path, nil)
	if err != nil {
		return nil, err
	}
	if l.tensorNumDims(s.input) < 2 {
		s.close()
		return nil, fmt.Errorf("%s is not an openWakeWord classifier, its input has no window axis", path)
	}
	m := &Model{ID: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), stage: s, windows: int(l.tensorDim(s.input, 1))}
	m.Reset()
	return m, nil
}

// Feeds embedding windows and yields one probability per classifier window that became ready
func (m *Model) Process(embeddings []float32, yield func(score float32)) error {
	shiftIn(m.embeddings, embeddings)
	m.pending = min(maxEmb, m.pending+len(embeddings)/wwFeatures)
	for m.pending >= m.windows {
		in := append([]float32(nil), window(m.embeddings, m.pending, m.windows, wwFeatures)...)
		m.pending = max(0, m.pending-1)
		probs, err := m.stage.run(in)
		if err != nil {
			return err
		}
		yield(probs[0])
	}
	return nil
}

// Clears the rolling embeddings so earlier audio cannot score
func (m *Model) Reset() {
	m.embeddings, m.pending = make([]float32, maxEmb*wwFeatures), 0
}

func (m *Model) Close() { m.stage.close() }
