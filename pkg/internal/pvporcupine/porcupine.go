package pvporcupine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"unsafe"

	dlutil "github.com/nickheyer/icebath/pkg/utils/dl"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// An error the native engine reports: the Picovoice status it returned, the failed step and the engine's message stack
type PorcupineError struct {
	Status       int32
	Message      string
	MessageStack []string
}

// The failed step, the status it ended in when there is one and the numbered message stack
func (e *PorcupineError) Error() string {
	message := e.Message
	if e.Status != StatusSuccess {
		message += " (" + statusName(e.Status) + ")"
	}
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

// Description of each Picovoice failure status
var statusNames = map[int32]string{
	StatusOutOfMemory:            "out of memory",
	StatusIOError:                "I/O error",
	StatusInvalidArgument:        "invalid argument",
	StatusStopIteration:          "stop iteration",
	StatusKeyError:               "key error",
	StatusInvalidState:           "invalid state",
	StatusRuntimeError:           "runtime error",
	StatusActivationError:        "activation error",
	StatusActivationLimitReached: "activation limit reached",
	StatusActivationThrottled:    "activation throttled",
	StatusActivationRefused:      "activation refused",
}

// Description of a Picovoice status, its number for a code outside the known set
func statusName(status int32) string {
	if name, ok := statusNames[status]; ok {
		return name
	}
	return "status " + strconv.Itoa(int(status))
}

// Error for a status other than success the native library returned, kept with its code whether or not the code is a known one
func statusError(status int32, message string, stack []string) error {
	return &PorcupineError{Status: status, Message: message, MessageStack: stack}
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
		return nil, &Error{Kind: InvalidArgument, Msg: "Access key should be a non-empty string."}
	}
	if err := requireFile(libraryPath, libraryPath, "Porcupine's dynamic library"); err != nil {
		return nil, err
	}
	library, err := dlutil.Open(libraryPath, false)
	if err != nil {
		return nil, err
	}
	if err := requireFile(modelPath, modelPath, "model file"); err != nil {
		return nil, err
	}
	if len(device) == 0 {
		return nil, &Error{Kind: InvalidArgument, Msg: "Device should be a non-empty string."}
	}
	if len(keywordPaths) != len(sensitivities) {
		return nil, &Error{Kind: InvalidArgument, Msg: "Number of keywords does not match the number of sensitivities."}
	}
	for _, x := range keywordPaths {
		if err := requireFile(pathutil.ExpandUser(x), x, "keyword file"); err != nil {
			return nil, err
		}
	}
	for _, x := range sensitivities {
		if !(0 <= x && x <= 1) {
			return nil, &Error{Kind: InvalidArgument, Msg: "A sensitivity value should be within [0, 1]."}
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
		dlutil.RegisterFunc(fptr, addr)
		return nil
	}
	if err := bind(&setSDK, "pv_set_sdk"); err != nil {
		return nil, err
	}
	setSDK(&dlutil.CString("python")[0])
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
		cPaths[i] = dlutil.CString(pathutil.ExpandUser(x))
		cPathPtrs[i] = uintptr(unsafe.Pointer(&cPaths[i][0]))
		cSensitivities[i] = float32(sensitivities[i])
	}
	cAccessKey, cModelPath, cDevice := dlutil.CString(accessKey), dlutil.CString(modelPath), dlutil.CString(device)
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
	p.version = dlutil.GoString(versionFunc())
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
		return 0, &Error{Kind: InvalidArgument, Msg: fmt.Sprintf("Invalid frame length. expected %d but received %d", p.frameLength, len(pcm))}
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
		stack = append(stack, dlutil.GoString(dlutil.PointerAt(stackRef, i)))
	}
	p.freeStack(stackRef)
	return stack, nil
}

// Names of the devices the native library can run inference on
func ListHardwareDevices(libraryPath string) ([]string, error) {
	closeDLLDir, err := dlutil.AddDLLDirectory(filepath.Dir(libraryPath))
	if err != nil {
		return nil, err
	}
	library, err := dlutil.Open(libraryPath, false)
	if err != nil {
		return nil, err
	}
	closeDLLDir()
	var listFunc func(devices *uintptr, num *int32) int32
	addr, err := library.Symbol("pv_porcupine_list_hardware_devices")
	if err != nil {
		return nil, err
	}
	dlutil.RegisterFunc(&listFunc, addr)
	var cDevices uintptr
	var cNum int32
	if status := listFunc(&cDevices, &cNum); status != StatusSuccess {
		return nil, statusError(status, "`pv_porcupine_list_hardware_devices` failed.", nil)
	}
	res := []string{}
	for i := 0; i < int(cNum); i++ {
		res = append(res, dlutil.GoString(dlutil.PointerAt(cDevices, i)))
	}
	var freeFunc func(devices uintptr, num int32)
	if addr, err = library.Symbol("pv_porcupine_free_hardware_devices"); err != nil {
		return nil, err
	}
	dlutil.RegisterFunc(&freeFunc, addr)
	freeFunc(cDevices, cNum)
	return res, nil
}

// Error naming the file the engine needs as shown, when there is nothing to stat at path
func requireFile(path, shown, what string) error {
	if _, err := os.Stat(path); err != nil {
		return &Error{Kind: FileUnavailable, Msg: fmt.Sprintf("Couldn't find %s at %q.", what, shown), Err: err}
	}
	return nil
}

// Loads the host platform dynamic library and resolves the engine init function
func (m *Module) LoadLibrary() error {
	libraryPath, err := m.PvLibraryPath("")
	if err != nil {
		return err
	}
	if err := requireFile(libraryPath, libraryPath, "Porcupine's dynamic library"); err != nil {
		return err
	}
	library, err := dlutil.Open(libraryPath, false)
	if err != nil {
		return err
	}
	_, err = library.Symbol("pv_porcupine_init")
	return err
}
