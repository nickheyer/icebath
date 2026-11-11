package pvporcupine

import (
	"fmt"
	"os"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// An error the engine reports, Class naming the subclass raised
type PorcupineError struct {
	Class        string
	Message      string
	MessageStack []string
}

func (e *PorcupineError) Error() string {
	message := e.Message
	if len(e.MessageStack) > 0 {
		message += ":"
		for i, line := range e.MessageStack {
			message += fmt.Sprintf("\n  [%d] %s", i, line)
		}
	}
	return message
}

// Picovoice status codes
const (
	StatusSuccess = iota
	StatusOutOfMemory
	StatusIOError
	StatusInvalidArgument
	StatusStopIteration
	StatusKeyError
	StatusInvalidState
	StatusRuntimeError
	StatusActivationError
	StatusActivationLimitReached
	StatusActivationThrottled
	StatusActivationRefused
)

var statusToException = map[int32]string{
	StatusOutOfMemory:            "PorcupineMemoryError",
	StatusIOError:                "PorcupineIOError",
	StatusInvalidArgument:        "PorcupineInvalidArgumentError",
	StatusStopIteration:          "PorcupineStopIterationError",
	StatusKeyError:               "PorcupineKeyError",
	StatusInvalidState:           "PorcupineInvalidStateError",
	StatusRuntimeError:           "PorcupineRuntimeError",
	StatusActivationError:        "PorcupineActivationError",
	StatusActivationLimitReached: "PorcupineActivationLimitError",
	StatusActivationThrottled:    "PorcupineActivationThrottledError",
	StatusActivationRefused:      "PorcupineActivationRefusedError",
}

// Error for a non success status, a ValueError for a code outside the status enum
func statusError(status int32, message string, stack []string) error {
	class, ok := statusToException[status]
	if !ok {
		return &PyError{Class: "ValueError", Msg: fmt.Sprintf("%d is not a valid Porcupine.PicovoiceStatuses", status)}
	}
	return &PorcupineError{Class: class, Message: message, MessageStack: stack}
}

// One wake word engine handle over the native library
type Porcupine struct {
	handle        uintptr
	getErrorStack func(stack *uintptr, depth *int32) int32
	freeStack     func(stack uintptr)
	deleteFunc    func(handle uintptr)
	processFunc   func(handle uintptr, pcm *int16, result *int32) int32
	version       string
	frameLength   int
	sampleRate    int
}

// Loads the dynamic library and initializes an engine for the keyword files with their sensitivities
func NewPorcupine(accessKey, libraryPath, modelPath, device string, keywordPaths []string, sensitivities []float64) (*Porcupine, error) {
	if accessKey == "" {
		return nil, &PyError{Class: "ValueError", Msg: "access_key should be a non-empty string."}
	}
	if !pathExists(libraryPath) {
		return nil, &PyError{Class: "OSError", Msg: fmt.Sprintf("Couldn't find Porcupine's dynamic library at '%s'.", libraryPath)}
	}
	library, err := pyctypes.LoadLibrary(libraryPath, false)
	if err != nil {
		return nil, err
	}
	if !pathExists(modelPath) {
		return nil, &PyError{Class: "OSError", Msg: fmt.Sprintf("Couldn't find model file at '%s'.", modelPath)}
	}
	if len(device) == 0 {
		return nil, &PyError{Class: "ValueError", Msg: "`device` should be a non-empty string."}
	}
	if len(keywordPaths) != len(sensitivities) {
		return nil, &PyError{Class: "ValueError", Msg: "Number of keywords does not match the number of sensitivities."}
	}
	for _, x := range keywordPaths {
		if !pathExists(pypath.Expanduser(x)) {
			return nil, &PyError{Class: "OSError", Msg: fmt.Sprintf("Couldn't find keyword file at '%s'.", x)}
		}
	}
	for _, x := range sensitivities {
		if !(0 <= x && x <= 1) {
			return nil, &PyError{Class: "ValueError", Msg: "A sensitivity value should be within [0, 1]."}
		}
	}
	p := &Porcupine{}
	var (
		setSDK      func(sdk *byte)
		initFunc    func(accessKey, modelPath, device *byte, numKeywords int32, keywordPaths *uintptr, sensitivities *float32, handle *uintptr) int32
		versionFunc func() uintptr
		frameLength func() int32
		sampleRate  func() int32
	)
	bind := func(fptr any, name string) error {
		addr, err := library.Symbol(name)
		if err != nil {
			return err
		}
		pyctypes.RegisterFunc(fptr, addr)
		return nil
	}
	if err := bind(&setSDK, "pv_set_sdk"); err != nil {
		return nil, err
	}
	setSDK(&pyctypes.CString("python")[0])
	if err := bind(&p.getErrorStack, "pv_get_error_stack"); err != nil {
		return nil, err
	}
	if err := bind(&p.freeStack, "pv_free_error_stack"); err != nil {
		return nil, err
	}
	if err := bind(&initFunc, "pv_porcupine_init"); err != nil {
		return nil, err
	}
	cPaths := make([][]byte, len(keywordPaths))
	cPathPtrs := make([]uintptr, len(keywordPaths)+1)
	cSensitivities := make([]float32, len(keywordPaths)+1)
	for i, x := range keywordPaths {
		cPaths[i] = pyctypes.CString(pypath.Expanduser(x))
		cPathPtrs[i] = uintptr(unsafe.Pointer(&cPaths[i][0]))
		cSensitivities[i] = float32(sensitivities[i])
	}
	cAccessKey, cModelPath, cDevice := pyctypes.CString(accessKey), pyctypes.CString(modelPath), pyctypes.CString(device)
	status := initFunc(&cAccessKey[0], &cModelPath[0], &cDevice[0], int32(len(keywordPaths)), &cPathPtrs[0], &cSensitivities[0], &p.handle)
	keepAlive(cPaths)
	if status != StatusSuccess {
		stack, err := p.errorStack()
		if err != nil {
			return nil, err
		}
		return nil, statusError(status, "Initialization failed", stack)
	}
	if err := bind(&p.deleteFunc, "pv_porcupine_delete"); err != nil {
		return nil, err
	}
	if err := bind(&p.processFunc, "pv_porcupine_process"); err != nil {
		return nil, err
	}
	if err := bind(&versionFunc, "pv_porcupine_version"); err != nil {
		return nil, err
	}
	p.version = pyctypes.GoString(versionFunc())
	if err := bind(&frameLength, "pv_porcupine_frame_length"); err != nil {
		return nil, err
	}
	p.frameLength = int(frameLength())
	if err := bind(&sampleRate, "pv_sample_rate"); err != nil {
		return nil, err
	}
	p.sampleRate = int(sampleRate())
	return p, nil
}

// Releases the resources the engine acquired
func (p *Porcupine) Delete() { p.deleteFunc(p.handle) }

// Index of the keyword detected at the end of one frame of audio, -1 when none
func (p *Porcupine) Process(pcm []int16) (int, error) {
	if len(pcm) != p.frameLength {
		return 0, &PyError{Class: "ValueError", Msg: fmt.Sprintf("Invalid frame length. expected %d but received %d", p.frameLength, len(pcm))}
	}
	frame := append(make([]int16, 0, len(pcm)+1), pcm...)
	var result int32
	status := p.processFunc(p.handle, unsafe.SliceData(frame), &result)
	if status != StatusSuccess {
		stack, err := p.errorStack()
		if err != nil {
			return 0, err
		}
		return 0, statusError(status, "Processing failed", stack)
	}
	return int(result), nil
}

// Release string the native library reports
func (p *Porcupine) Version() string { return p.version }

// Number of audio samples per frame
func (p *Porcupine) FrameLength() int { return p.frameLength }

// Audio sample rate the engine accepts
func (p *Porcupine) SampleRate() int { return p.sampleRate }

// Messages of the native error stack, freed after reading
func (p *Porcupine) errorStack() ([]string, error) {
	var stackRef uintptr
	var depth int32
	if status := p.getErrorStack(&stackRef, &depth); status != StatusSuccess {
		return nil, statusError(status, "Unable to get Porcupine error state", nil)
	}
	stack := []string{}
	for i := 0; i < int(depth); i++ {
		stack = append(stack, pyctypes.GoString(pyctypes.PointerAt(stackRef, i)))
	}
	p.freeStack(stackRef)
	return stack, nil
}

// Names of the devices the native library can run inference on
func ListHardwareDevices(libraryPath string) ([]string, error) {
	closeDLLDir, err := pyctypes.AddDLLDirectory(pypath.Dirname(libraryPath))
	if err != nil {
		return nil, err
	}
	library, err := pyctypes.LoadLibrary(libraryPath, false)
	if err != nil {
		return nil, err
	}
	closeDLLDir()
	var listFunc func(devices *uintptr, num *int32) int32
	addr, err := library.Symbol("pv_porcupine_list_hardware_devices")
	if err != nil {
		return nil, err
	}
	pyctypes.RegisterFunc(&listFunc, addr)
	var cDevices uintptr
	var cNum int32
	if status := listFunc(&cDevices, &cNum); status != StatusSuccess {
		return nil, statusError(status, "`pv_porcupine_list_hardware_devices` failed.", nil)
	}
	res := []string{}
	for i := 0; i < int(cNum); i++ {
		res = append(res, pyctypes.GoString(pyctypes.PointerAt(cDevices, i)))
	}
	var freeFunc func(devices uintptr, num int32)
	if addr, err = library.Symbol("pv_porcupine_free_hardware_devices"); err != nil {
		return nil, err
	}
	pyctypes.RegisterFunc(&freeFunc, addr)
	freeFunc(cDevices, cNum)
	return res, nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
