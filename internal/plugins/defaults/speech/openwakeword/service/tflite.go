package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"unsafe"

	"github.com/nickheyer/icebath/internal/speech/native"
)

// A model's tensors are not the shape openWakeWord's are
var errShape = errors.New("unexpected wake word model tensor shape")

// The TensorFlow Lite C functions the pipeline calls
type tflite struct {
	modelCreate  func(path *byte) uintptr
	create       func(model, options uintptr) uintptr
	allocate     func(interpreter uintptr) int32
	invoke       func(interpreter uintptr) int32
	input        func(interpreter uintptr, index int32) uintptr
	output       func(interpreter uintptr, index int32) uintptr
	byteSize     func(tensor uintptr) uintptr
	numDims      func(tensor uintptr) int32
	dim          func(tensor uintptr, index int32) int32
	resize       func(interpreter uintptr, index int32, dims *int32, numDims int32) int32
	copyFrom     func(tensor uintptr, data unsafe.Pointer, size uintptr) int32
	copyTo       func(tensor uintptr, data unsafe.Pointer, size uintptr) int32
	deleteInterp func(interpreter uintptr)
	deleteModel  func(model uintptr)
}

// The tensorflowlite_c library pyopen-wakeword ships: in its lib folder, else in the folder of this platform's build beside it; its symbols
// made global for the delegates it loads
func library(dir string) (*tflite, error) {
	own := filepath.Join(dir, "lib")
	path := first(own)
	if path == "" {
		goos := runtime.GOOS
		if goos == "android" {
			goos = "linux"
		}
		switch goos + "/" + runtime.GOARCH {
		case "linux/amd64", "linux/arm64", "windows/amd64", "darwin/arm64":
		default:
			return nil, fmt.Errorf("tensorflowlite_c library not found in %s: no tensorflowlite_c build for %s/%s", own, runtime.GOOS, runtime.GOARCH)
		}
		platform := filepath.Join(filepath.Dir(filepath.Clean(dir)), "lib", goos+"_"+runtime.GOARCH)
		if path = first(platform); path == "" {
			return nil, fmt.Errorf("tensorflowlite_c library not found in %s or %s", own, platform)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	lib, err := native.Open(abs, true)
	if err != nil {
		return nil, err
	}
	t := &tflite{}
	if err := lib.BindAll(map[string]any{"TfLiteModelCreateFromFile": &t.modelCreate, "TfLiteInterpreterCreate": &t.create,
		"TfLiteInterpreterAllocateTensors": &t.allocate, "TfLiteInterpreterInvoke": &t.invoke, "TfLiteInterpreterGetInputTensor": &t.input,
		"TfLiteInterpreterGetOutputTensor": &t.output, "TfLiteTensorByteSize": &t.byteSize, "TfLiteTensorNumDims": &t.numDims, "TfLiteTensorDim": &t.dim,
		"TfLiteInterpreterResizeInputTensor": &t.resize, "TfLiteTensorCopyFromBuffer": &t.copyFrom, "TfLiteTensorCopyToBuffer": &t.copyTo,
		"TfLiteInterpreterDelete": &t.deleteInterp, "TfLiteModelDelete": &t.deleteModel}); err != nil {
		return nil, err
	}
	return t, nil
}

// First tensorflowlite_c library file of a folder
func first(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if ok, _ := filepath.Match("*tensorflowlite_c.*", entry.Name()); ok && !entry.IsDir() {
			return filepath.Join(dir, entry.Name())
		}
	}
	return ""
}

// Names of the TensorFlow Lite statuses
var statuses = []string{"", "error", "delegate error", "application error", "delegate data not found", "delegate data write error",
	"delegate data read error", "unresolved ops", "cancelled", "output shape not known"}

// One model of the pipeline, loaded into an interpreter with its input sized
type stage struct {
	t                  *tflite
	path               string
	model, interpreter uintptr
	in, out            uintptr
	buffer             []float32
}

func newStage(t *tflite, path string, shape []int32) (*stage, error) {
	s := &stage{t: t, path: path}
	held := native.CString(path)
	s.model = t.modelCreate(&held[0])
	runtime.KeepAlive(held)
	if s.model == 0 {
		return nil, s.failed("TfLiteModelCreateFromFile", 0)
	}
	if s.interpreter = t.create(s.model, 0); s.interpreter == 0 {
		s.release()
		return nil, s.failed("TfLiteInterpreterCreate", 0)
	}
	if len(shape) > 0 {
		if status := t.resize(s.interpreter, 0, unsafe.SliceData(shape), int32(len(shape))); status != 0 {
			s.release()
			return nil, s.failed("TfLiteInterpreterResizeInputTensor", status)
		}
	}
	if status := t.allocate(s.interpreter); status != 0 {
		s.release()
		return nil, s.failed("TfLiteInterpreterAllocateTensors", status)
	}
	if s.in = t.input(s.interpreter, 0); s.in == 0 {
		s.release()
		return nil, s.failed("TfLiteInterpreterGetInputTensor", 0)
	}
	if s.out = t.output(s.interpreter, 0); s.out == 0 {
		s.release()
		return nil, s.failed("TfLiteInterpreterGetOutputTensor", 0)
	}
	return s, nil
}

// Failure of a call: one that answered NULL, or a status
func (s *stage) failed(function string, status int32) error {
	if status == 0 {
		return fmt.Errorf("%s: %s returned NULL", s.path, function)
	}
	text := s.path + ": " + function + " failed with status " + strconv.Itoa(int(status))
	if status > 0 && int(status) < len(statuses) {
		text += " (" + statuses[status] + ")"
	}
	return errors.New(text)
}

func (s *stage) shape(detail string) error { return fmt.Errorf("%s: %w: %s", s.path, errShape, detail) }

func (s *stage) open() bool { return s != nil && s.interpreter != 0 && s.model != 0 }

func (s *stage) release() {
	if s == nil {
		return
	}
	if s.interpreter != 0 {
		s.t.deleteInterp(s.interpreter)
		s.interpreter = 0
	}
	if s.model != 0 {
		s.t.deleteModel(s.model)
		s.model = 0
	}
	s.in, s.out = 0, 0
}

// Refusal of an input that does not hold the values
func (s *stage) expect(values int) error {
	if size := s.t.byteSize(s.in); size != uintptr(values)*4 {
		return s.shape(fmt.Sprintf("input tensor holds %d bytes, want %d float32 values", size, values))
	}
	return nil
}

// Runs the model on the input, its output valid until the next run
func (s *stage) run(in []float32) ([]float32, error) {
	if status := s.t.copyFrom(s.in, unsafe.Pointer(unsafe.SliceData(in)), uintptr(len(in))*4); status != 0 {
		return nil, s.failed("TfLiteTensorCopyFromBuffer", status)
	}
	if status := s.t.invoke(s.interpreter); status != 0 {
		return nil, s.failed("TfLiteInterpreterInvoke", status)
	}
	size := s.t.byteSize(s.out)
	if size == 0 || size%4 != 0 {
		return nil, s.shape(fmt.Sprintf("output tensor holds %d bytes, not whole float32 values", size))
	}
	n := int(size / 4)
	if cap(s.buffer) < n {
		s.buffer = make([]float32, n)
	}
	out := s.buffer[:n]
	if status := s.t.copyTo(s.out, unsafe.Pointer(&out[0]), size); status != 0 {
		return nil, s.failed("TfLiteTensorCopyToBuffer", status)
	}
	return out, nil
}

// Shifts values into the end of a buffer, its oldest dropped
func shift(buf, values []float32) {
	if len(values) >= len(buf) {
		copy(buf, values[len(values)-len(buf):])
		return
	}
	copy(buf, buf[len(values):])
	copy(buf[len(buf)-len(values):], values)
}
