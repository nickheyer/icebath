package pyopenwakeword

import (
	"runtime"
	"strings"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
)

// Built in openWakeWord model names
const (
	ModelOkayNabu   = "okay_nabu"
	ModelHeyJarvis  = "hey_jarvis"
	ModelHeyMycroft = "hey_mycroft"
	ModelAlexa      = "alexa"
	ModelHeyRhasspy = "hey_rhasspy"
)

// A builtin Python exception the package raises, named by its class
type PyError struct{ Class, Msg string }

func (e *PyError) Error() string { return e.Msg }

// The TensorFlow Lite C functions the wake word classes call
type tfLite interface {
	ModelCreateFromFile(path string) uintptr
	InterpreterCreate(model, options uintptr) uintptr
	InterpreterAllocateTensors(interpreter uintptr) int32
	InterpreterInvoke(interpreter uintptr) int32
	InterpreterGetInputTensor(interpreter uintptr, index int32) (uintptr, error)
	InterpreterGetOutputTensor(interpreter uintptr, index int32) (uintptr, error)
	TensorByteSize(tensor uintptr) uintptr
	TensorNumDims(tensor uintptr) int32
	TensorDim(tensor uintptr, index int32) int32
	InterpreterResizeInputTensor(interpreter uintptr, index int32, dims []int32) int32
	TensorCopyFromBuffer(tensor uintptr, data []float32) int32
	TensorCopyToBuffer(tensor uintptr, data []float32, size uintptr) int32
	InterpreterDelete(interpreter uintptr)
	ModelDelete(model uintptr)
}

// The loaded tensorflowlite_c library with its functions bound
type tfLiteLibrary struct {
	modelCreateFromFile          func(path *byte) uintptr
	interpreterCreate            func(model, options uintptr) uintptr
	interpreterAllocateTensors   func(interpreter uintptr) int32
	interpreterInvoke            func(interpreter uintptr) int32
	interpreterGetInputTensor    func(interpreter uintptr, index int32) uintptr
	interpreterGetOutputTensor   func(interpreter uintptr, index int32) uintptr
	tensorByteSize               func(tensor uintptr) uintptr
	tensorNumDims                func(tensor uintptr) int32
	tensorDim                    func(tensor uintptr, index int32) int32
	interpreterResizeInputTensor func(interpreter uintptr, index int32, dims *int32, numDims int32) int32
	tensorCopyFromBuffer         func(tensor uintptr, data unsafe.Pointer, size uintptr) int32
	tensorCopyToBuffer           func(tensor uintptr, data unsafe.Pointer, size uintptr) int32
	interpreterDelete            func(interpreter uintptr)
	modelDelete                  func(model uintptr)
}

func (l *tfLiteLibrary) ModelCreateFromFile(path string) uintptr {
	return l.modelCreateFromFile(&pyctypes.CString(path)[0])
}

func (l *tfLiteLibrary) InterpreterCreate(model, options uintptr) uintptr {
	return l.interpreterCreate(model, options)
}

func (l *tfLiteLibrary) InterpreterAllocateTensors(interpreter uintptr) int32 {
	return l.interpreterAllocateTensors(interpreter)
}

func (l *tfLiteLibrary) InterpreterInvoke(interpreter uintptr) int32 {
	return l.interpreterInvoke(interpreter)
}

// Input tensor at an index, a RuntimeError when the library returns NULL
func (l *tfLiteLibrary) InterpreterGetInputTensor(interpreter uintptr, index int32) (uintptr, error) {
	return notNull(l.interpreterGetInputTensor(interpreter, index), "TfLiteInterpreterGetInputTensor")
}

// Output tensor at an index, a RuntimeError when the library returns NULL
func (l *tfLiteLibrary) InterpreterGetOutputTensor(interpreter uintptr, index int32) (uintptr, error) {
	return notNull(l.interpreterGetOutputTensor(interpreter, index), "TfLiteInterpreterGetOutputTensor")
}

func (l *tfLiteLibrary) TensorByteSize(tensor uintptr) uintptr { return l.tensorByteSize(tensor) }
func (l *tfLiteLibrary) TensorNumDims(tensor uintptr) int32    { return l.tensorNumDims(tensor) }
func (l *tfLiteLibrary) TensorDim(tensor uintptr, index int32) int32 {
	return l.tensorDim(tensor, index)
}

func (l *tfLiteLibrary) InterpreterResizeInputTensor(interpreter uintptr, index int32, dims []int32) int32 {
	return l.interpreterResizeInputTensor(interpreter, index, unsafe.SliceData(dims), int32(len(dims)))
}

func (l *tfLiteLibrary) TensorCopyFromBuffer(tensor uintptr, data []float32) int32 {
	return l.tensorCopyFromBuffer(tensor, unsafe.Pointer(unsafe.SliceData(data)), uintptr(len(data))*4)
}

func (l *tfLiteLibrary) TensorCopyToBuffer(tensor uintptr, data []float32, size uintptr) int32 {
	return l.tensorCopyToBuffer(tensor, unsafe.Pointer(unsafe.SliceData(data)), size)
}

func (l *tfLiteLibrary) InterpreterDelete(interpreter uintptr) { l.interpreterDelete(interpreter) }
func (l *tfLiteLibrary) ModelDelete(model uintptr)             { l.modelDelete(model) }

func notNull(result uintptr, name string) (uintptr, error) {
	if result == 0 {
		return 0, &PyError{Class: "RuntimeError", Msg: name + " returned NULL"}
	}
	return result, nil
}

// The base of both wake word classes, the resolved library path and the bound library
type TfLiteWakeWord struct {
	LibtensorflowliteCPath string
	lib                    tfLite
}

// Loads the tensorflowlite_c library with global symbol scope off Windows and binds the functions the classes call
func newTfLiteWakeWord(libtensorflowliteCPath string) (*TfLiteWakeWord, error) {
	resolved, err := pypath.Resolve(libtensorflowliteCPath)
	if err != nil {
		return nil, err
	}
	cdll, err := pyctypes.LoadLibrary(resolved, runtime.GOOS != "windows")
	if err != nil {
		return nil, err
	}
	lib := &tfLiteLibrary{}
	for _, entry := range []struct {
		fptr any
		name string
	}{
		{&lib.modelCreateFromFile, "TfLiteModelCreateFromFile"}, {&lib.interpreterCreate, "TfLiteInterpreterCreate"},
		{&lib.interpreterAllocateTensors, "TfLiteInterpreterAllocateTensors"}, {&lib.interpreterInvoke, "TfLiteInterpreterInvoke"},
		{&lib.interpreterGetInputTensor, "TfLiteInterpreterGetInputTensor"}, {&lib.interpreterGetOutputTensor, "TfLiteInterpreterGetOutputTensor"},
		{&lib.tensorByteSize, "TfLiteTensorByteSize"}, {&lib.tensorNumDims, "TfLiteTensorNumDims"}, {&lib.tensorDim, "TfLiteTensorDim"},
		{nil, "TfLiteTensorType"}, {nil, "TfLiteTensorQuantizationParams"},
		{&lib.interpreterResizeInputTensor, "TfLiteInterpreterResizeInputTensor"}, {&lib.tensorCopyFromBuffer, "TfLiteTensorCopyFromBuffer"},
		{&lib.tensorCopyToBuffer, "TfLiteTensorCopyToBuffer"}, {&lib.interpreterDelete, "TfLiteInterpreterDelete"},
		{&lib.modelDelete, "TfLiteModelDelete"},
	} {
		addr, err := cdll.Symbol(entry.name)
		if err != nil {
			return nil, err
		}
		if entry.fptr != nil {
			pyctypes.RegisterFunc(entry.fptr, addr)
		}
	}
	return &TfLiteWakeWord{LibtensorflowliteCPath: resolved, lib: lib}, nil
}

// Python sys.platform spelling of the running OS
func sysPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "android":
		return "linux"
	}
	return runtime.GOOS
}

// Library directory name for this host, false where no tensorflowlite_c build exists
func GetPlatform() (string, bool) { return getPlatform(pyplatform.Machine(), sysPlatform()) }

// Library directory name for a machine and sys.platform pair
func getPlatform(machine, system string) (string, bool) {
	machine = strings.ToLower(machine)
	isArm := strings.Contains(machine, "arm") || strings.Contains(machine, "aarch")
	isAmd64 := machine == "x86_64" || machine == "amd64"
	if strings.HasPrefix(system, "linux") {
		if isArm {
			return "linux_arm64", true
		}
		if isAmd64 {
			return "linux_amd64", true
		}
	}
	if system == "win32" && isAmd64 {
		return "windows_amd64", true
	}
	if system == "darwin" && isArm {
		return "darwin_arm64", true
	}
	return "", false
}
