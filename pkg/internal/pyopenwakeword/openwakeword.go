package pyopenwakeword

import (
	"encoding/binary"
	"fmt"
	"runtime"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Constants of the openWakeWord pipeline, 80 ms chunks at 16 kHz through mel, embedding and classifier windows
const (
	BatchSize       = 1
	AutofillSeconds = 8
	MaxSeconds      = 10
	SampleRate      = 16000
	maxSamples      = MaxSeconds * SampleRate
	SamplesPerChunk = 1280
	MsPerChunk      = SamplesPerChunk / SampleRate
	MelsPerSecond   = 97
	MaxMels         = MaxSeconds * MelsPerSecond
	MelSamples      = 1760
	NumMels         = 32
	EmbFeatures     = 76
	EmbStep         = 8
	MaxEmb          = MaxSeconds * EmbStep
	WWFeatures      = 96
)

var (
	melShape = []int32{BatchSize, MelSamples}
	embShape = []int32{BatchSize, EmbFeatures, NumMels, 1}
)

// The imported pyopen_wakeword package rooted at its directory
type Module struct{ Dir string }

// Imports the package unpacked at dir
func Import(dir string) *Module { return &Module{Dir: dir} }

func (m *Module) modelsDir() string { return pypath.PathJoin(m.Dir, "models") }

// Bounds of the numpy slice start to stop over n rows, negative indexes counted from the end
func sliceBounds(start, stop, n int) (int, int) {
	clamp := func(i int) int {
		if i < 0 {
			i += n
		}
		return min(max(i, 0), n)
	}
	lo, hi := clamp(start), clamp(stop)
	return lo, max(lo, hi)
}

// numpy assignment of src rows into dst rows lo to hi, a ValueError when the row counts differ
func assignRows(dst []float32, lo, hi int, src []float32, width int) error {
	if (hi-lo)*width != len(src) {
		return &PyError{Class: "ValueError", Msg: fmt.Sprintf("could not broadcast input array from shape %s into shape %s",
			rowsShape(len(src)/width, width), rowsShape(hi-lo, width))}
	}
	copy(dst[lo*width:hi*width], src)
	return nil
}

func rowsShape(rows, width int) string {
	if width == 1 {
		return fmt.Sprintf("(%d,)", rows)
	}
	return fmt.Sprintf("(%d,%d)", rows, width)
}

// Shifts a rolling buffer of rows left by n rows and writes the n new rows at its end
func shiftAppend(buf, rows []float32, width int) error {
	total, n := len(buf)/width, len(rows)/width
	lo, hi := sliceBounds(0, -n, total)
	if n == 0 {
		lo, hi = 0, 0
	}
	tailLo, tailHi := sliceBounds(n, total, total)
	if err := assignRows(buf, lo, hi, buf[tailLo*width:tailHi*width], width); err != nil {
		return err
	}
	lo, hi = sliceBounds(-n, total, total)
	if n == 0 {
		lo, hi = 0, total
	}
	return assignRows(buf, lo, hi, rows, width)
}

// Rows of a rolling buffer from pending back from its end through a window of size rows
func pendingWindow(buf []float32, pending, size, width int) (int, int) {
	total := len(buf) / width
	if pending == 0 {
		return sliceBounds(0, total+size, total)
	}
	return sliceBounds(-pending, total-pending+size, total)
}

// Output tensor of an interpreter copied out as float32 values
func readOutput(lib tfLite, tensor uintptr) []float32 {
	outputBytes := lib.TensorByteSize(tensor)
	out := make([]float32, outputBytes/4)
	lib.TensorCopyToBuffer(tensor, out, outputBytes)
	return out
}

// One wake word classifier over rolling embedding windows
type OpenWakeWord struct {
	*TfLiteWakeWord
	id            string
	TfliteModel   string
	model         uintptr
	interpreter   uintptr
	inputTensor   uintptr
	outputTensor  uintptr
	inputWindows  int
	newEmbeddings int
	embeddings    []float32
}

// Loads a classifier model into an interpreter and reads its window count from the input shape
func newOpenWakeWord(id, tfliteModel string, base *TfLiteWakeWord) (*OpenWakeWord, error) {
	resolved, err := pypath.Resolve(tfliteModel)
	if err != nil {
		return nil, err
	}
	w := &OpenWakeWord{TfLiteWakeWord: base, id: id, TfliteModel: resolved}
	lib := w.lib
	if w.model, err = notNull(lib.ModelCreateFromFile(resolved), "TfLiteModelCreateFromFile"); err != nil {
		return nil, err
	}
	if w.interpreter, err = notNull(lib.InterpreterCreate(w.model, 0), "TfLiteInterpreterCreate"); err != nil {
		w.Close()
		return nil, err
	}
	runtime.SetFinalizer(w, (*OpenWakeWord).Close)
	lib.InterpreterAllocateTensors(w.interpreter)
	if w.inputTensor, err = lib.InterpreterGetInputTensor(w.interpreter, 0); err != nil {
		return nil, err
	}
	if w.outputTensor, err = lib.InterpreterGetOutputTensor(w.interpreter, 0); err != nil {
		return nil, err
	}
	numInputDims := lib.TensorNumDims(w.inputTensor)
	inputShape := make([]int32, 0, max(numInputDims, 0))
	for i := int32(0); i < numInputDims; i++ {
		inputShape = append(inputShape, lib.TensorDim(w.inputTensor, i))
	}
	if len(inputShape) < 2 {
		return nil, &PyError{Class: "IndexError", Msg: "list index out of range"}
	}
	w.inputWindows = int(inputShape[1])
	w.embeddings = make([]float32, MaxEmb*WWFeatures)
	return w, nil
}

// The model name, its file stem
func (w *OpenWakeWord) ID() string { return w.id }

// Feeds embedding windows and yields one probability per classifier window that became ready
func (w *OpenWakeWord) ProcessStreaming(embeddings []float32, yield func(float32) bool) error {
	if w.interpreter == 0 || w.model == 0 {
		return nil
	}
	if len(embeddings)%WWFeatures != 0 {
		return &PyError{Class: "ValueError", Msg: fmt.Sprintf("cannot reshape array of size %d into shape (1,1,newaxis,%d)", len(embeddings), WWFeatures)}
	}
	numEmbeddingWindows := len(embeddings) / WWFeatures
	if err := shiftAppend(w.embeddings, embeddings, WWFeatures); err != nil {
		return err
	}
	w.newEmbeddings = min(MaxEmb, w.newEmbeddings+numEmbeddingWindows)
	for w.newEmbeddings >= w.inputWindows {
		embTensor := make([]float32, max(w.inputWindows, 0)*WWFeatures)
		lo, hi := pendingWindow(w.embeddings, w.newEmbeddings, w.inputWindows, WWFeatures)
		if err := assignRows(embTensor, 0, max(w.inputWindows, 0), w.embeddings[lo*WWFeatures:hi*WWFeatures], WWFeatures); err != nil {
			return err
		}
		w.newEmbeddings = max(0, w.newEmbeddings-1)
		w.lib.TensorCopyFromBuffer(w.inputTensor, embTensor)
		w.lib.InterpreterInvoke(w.interpreter)
		probs := readOutput(w.lib, w.outputTensor)
		if len(probs) != 1 {
			return &PyError{Class: "ValueError", Msg: "can only convert an array of size 1 to a Python scalar"}
		}
		if !yield(probs[0]) {
			return nil
		}
	}
	return nil
}

// Clears the rolling embeddings so earlier audio cannot score
func (w *OpenWakeWord) Reset() {
	w.newEmbeddings = 0
	w.embeddings = make([]float32, MaxEmb*WWFeatures)
}

// Loads a classifier from a model file, finding the bundled library when no library path is given
func (m *Module) FromModel(modelPath string, libtensorflowliteCPath *string) (*OpenWakeWord, error) {
	base, err := m.loadLibrary(libtensorflowliteCPath)
	if err != nil {
		return nil, err
	}
	return newOpenWakeWord(pypath.PathStem(modelPath), modelPath, base)
}

// Loads a built in classifier by name from the models directory, the package one when modelsDir is nil
func (m *Module) FromBuiltin(model string, modelsDir, libtensorflowliteCPath *string) (*OpenWakeWord, error) {
	dir := m.modelsDir()
	if modelsDir != nil {
		dir = *modelsDir
	}
	return m.FromModel(pypath.PathJoin(dir, model+".tflite"), libtensorflowliteCPath)
}

// Releases the interpreter and the model
func (w *OpenWakeWord) Close() {
	if w.interpreter != 0 {
		w.lib.InterpreterDelete(w.interpreter)
		w.interpreter = 0
	}
	if w.model != 0 {
		w.lib.ModelDelete(w.model)
		w.model = 0
	}
}

// Turns streamed audio into embedding windows through the mel and embedding models
type OpenWakeWordFeatures struct {
	*TfLiteWakeWord
	MelPath         string
	EmbPath         string
	melModel        uintptr
	melInterpreter  uintptr
	melInputTensor  uintptr
	melOutputTensor uintptr
	embModel        uintptr
	embInterpreter  uintptr
	embInputTensor  uintptr
	embOutputTensor uintptr
	newAudioSamples int
	audio           []float32
	newMels         int
	mels            []float32
}

// Model and interpreter for one feature stage with its input resized and tensors allocated
func featureStage(lib tfLite, path string, shape []int32) (model, interpreter, input, output uintptr, err error) {
	if model, err = notNull(lib.ModelCreateFromFile(path), "TfLiteModelCreateFromFile"); err != nil {
		return
	}
	if interpreter, err = notNull(lib.InterpreterCreate(model, 0), "TfLiteInterpreterCreate"); err != nil {
		return
	}
	lib.InterpreterResizeInputTensor(interpreter, 0, shape)
	lib.InterpreterAllocateTensors(interpreter)
	if input, err = lib.InterpreterGetInputTensor(interpreter, 0); err != nil {
		return
	}
	output, err = lib.InterpreterGetOutputTensor(interpreter, 0)
	return
}

// Loads the melspectrogram and embedding models with eight seconds of silence counted as pending audio
func newOpenWakeWordFeatures(melspectrogramModel, embeddingModel string, base *TfLiteWakeWord) (*OpenWakeWordFeatures, error) {
	f := &OpenWakeWordFeatures{TfLiteWakeWord: base}
	var err error
	if f.MelPath, err = pypath.Resolve(melspectrogramModel); err != nil {
		return nil, err
	}
	if f.EmbPath, err = pypath.Resolve(embeddingModel); err != nil {
		return nil, err
	}
	runtime.SetFinalizer(f, (*OpenWakeWordFeatures).Close)
	if f.melModel, f.melInterpreter, f.melInputTensor, f.melOutputTensor, err = featureStage(f.lib, f.MelPath, melShape); err != nil {
		f.Close()
		return nil, err
	}
	if f.embModel, f.embInterpreter, f.embInputTensor, f.embOutputTensor, err = featureStage(f.lib, f.EmbPath, embShape); err != nil {
		f.Close()
		return nil, err
	}
	f.newAudioSamples = AutofillSeconds * SampleRate
	f.audio = make([]float32, maxSamples)
	f.mels = make([]float32, MaxMels*NumMels)
	return f, nil
}

// Feeds 16 bit native endian audio and yields each embedding window group that became ready
func (f *OpenWakeWordFeatures) ProcessStreaming(audioChunk []byte, yield func([]float32) bool) error {
	if f.melInterpreter == 0 || f.melModel == 0 || f.embInterpreter == 0 || f.embModel == 0 {
		return nil
	}
	if len(audioChunk)%2 != 0 {
		return &PyError{Class: "ValueError", Msg: "buffer size must be a multiple of element size"}
	}
	chunkArray := make([]float32, len(audioChunk)/2)
	for i := range chunkArray {
		chunkArray[i] = float32(int16(binary.NativeEndian.Uint16(audioChunk[2*i:])))
	}
	if err := shiftAppend(f.audio, chunkArray, 1); err != nil {
		return err
	}
	f.newAudioSamples = min(len(f.audio), f.newAudioSamples+len(chunkArray))
	for f.newAudioSamples >= MelSamples {
		audioTensor := make([]float32, BatchSize*MelSamples)
		lo, hi := pendingWindow(f.audio, f.newAudioSamples, MelSamples, 1)
		if err := assignRows(audioTensor, 0, MelSamples, f.audio[lo:hi], 1); err != nil {
			return err
		}
		f.newAudioSamples = max(0, f.newAudioSamples-SamplesPerChunk)
		f.lib.TensorCopyFromBuffer(f.melInputTensor, audioTensor)
		f.lib.InterpreterInvoke(f.melInterpreter)
		mels := readOutput(f.lib, f.melOutputTensor)
		for i, v := range mels {
			mels[i] = v/10 + 2
		}
		if len(mels)%NumMels != 0 {
			return &PyError{Class: "ValueError", Msg: fmt.Sprintf("cannot reshape array of size %d into shape (1,1,newaxis,%d)", len(mels), NumMels)}
		}
		numMelWindows := len(mels) / NumMels
		if err := shiftAppend(f.mels, mels, NumMels); err != nil {
			return err
		}
		f.newMels = min(MaxMels, f.newMels+numMelWindows)
		for f.newMels >= EmbFeatures {
			melsTensor := make([]float32, BatchSize*EmbFeatures*NumMels)
			lo, hi := pendingWindow(f.mels, f.newMels, EmbFeatures, NumMels)
			if err := assignRows(melsTensor, 0, EmbFeatures, f.mels[lo*NumMels:hi*NumMels], NumMels); err != nil {
				return err
			}
			f.newMels = max(0, f.newMels-EmbStep)
			f.lib.TensorCopyFromBuffer(f.embInputTensor, melsTensor)
			f.lib.InterpreterInvoke(f.embInterpreter)
			emb := readOutput(f.lib, f.embOutputTensor)
			if len(emb)%WWFeatures != 0 {
				return &PyError{Class: "ValueError", Msg: fmt.Sprintf("cannot reshape array of size %d into shape (1,1,newaxis,%d)", len(emb), WWFeatures)}
			}
			if !yield(emb) {
				return nil
			}
		}
	}
	return nil
}

// Clears the rolling audio and mel buffers back to the freshly built state
func (f *OpenWakeWordFeatures) Reset() {
	f.newAudioSamples = AutofillSeconds * SampleRate
	f.audio = make([]float32, maxSamples)
	f.newMels = 0
	f.mels = make([]float32, MaxMels*NumMels)
}

// Loads the shared feature models from the models directory, the package one when modelsDir is nil
func (m *Module) FeaturesFromBuiltin(modelsDir, libtensorflowliteCPath *string) (*OpenWakeWordFeatures, error) {
	dir := m.modelsDir()
	if modelsDir != nil {
		dir = *modelsDir
	}
	base, err := m.loadLibrary(libtensorflowliteCPath)
	if err != nil {
		return nil, err
	}
	return newOpenWakeWordFeatures(pypath.PathJoin(dir, "melspectrogram.tflite"), pypath.PathJoin(dir, "embedding_model.tflite"), base)
}

// Releases both interpreters and both models
func (f *OpenWakeWordFeatures) Close() {
	if f.melInterpreter != 0 {
		f.lib.InterpreterDelete(f.melInterpreter)
		f.melInterpreter = 0
	}
	if f.melModel != 0 {
		f.lib.ModelDelete(f.melModel)
		f.melModel = 0
	}
	if f.embInterpreter != 0 {
		f.lib.InterpreterDelete(f.embInterpreter)
		f.embInterpreter = 0
	}
	if f.embModel != 0 {
		f.lib.ModelDelete(f.embModel)
		f.embModel = 0
	}
}

// Binds the library at the given path, the one found beside the package when the path is nil
func (m *Module) loadLibrary(libtensorflowliteCPath *string) (*TfLiteWakeWord, error) {
	path := ""
	if libtensorflowliteCPath == nil {
		found, err := m.findTensorflowliteC()
		if err != nil {
			return nil, err
		}
		path = found
	} else {
		path = *libtensorflowliteCPath
	}
	return newTfLiteWakeWord(path)
}

// Path of the tensorflowlite_c library in the package lib directory, else in the repository lib directory of the platform
func (m *Module) findTensorflowliteC() (string, error) {
	platform, ok := GetPlatform()
	return findTensorflowliteC(m.Dir, platform, ok)
}

func findTensorflowliteC(dir, platform string, platformKnown bool) (string, error) {
	first := func(libDir string) string {
		hits, err := pypath.Glob(libDir, "*tensorflowlite_c.*")
		if err != nil || len(hits) == 0 {
			return ""
		}
		return hits[0]
	}
	path := first(pypath.PathJoin(dir, "lib"))
	if path == "" {
		if !platformKnown {
			return "", &PyError{Class: "ValueError", Msg: "Unable to detect platform for tensorflowlite_c"}
		}
		path = first(pypath.PathJoin(pypath.PathJoin(pypath.Dirname(dir), "lib"), platform))
	}
	if path == "" {
		return "", &PyError{Class: "ValueError", Msg: "Failed to find tensorflowlite_c library"}
	}
	return path, nil
}

// Loads the tensorflowlite_c library the package carries and binds the functions the models call
func (m *Module) LoadLibrary() error {
	path, err := m.findTensorflowliteC()
	if err != nil {
		return err
	}
	_, err = newTfLiteWakeWord(path)
	return err
}
