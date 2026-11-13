package porcupine

import (
	"context"
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

// Release of the Picovoice Porcupine package whose libraries, model and keywords this binding loads
const Version = "4.0.3"

// The published package carrying every platform library, the model parameters and the built in keyword files
var wheel = fetch.Asset{Name: "pvporcupine-4.0.3-py3-none-any.whl", Size: 3659883,
	URL:    "https://files.pythonhosted.org/packages/2a/ef/1c4b8e47d8248fe1b615772028265ca65d9ff3ea98022d84cd973d46db87/pvporcupine-4.0.3-py3-none-any.whl",
	SHA256: "92796dbd3cf80a56db1ce20702cbceb151aee34f19ef730591f815eb16f2ebfb"}

// Raspberry Pi core of a 64 bit ARM board by the CPU part the kernel reports
var raspberryPiParts = map[string]string{"0xd03": "cortex-a53-aarch64", "0xd08": "cortex-a72-aarch64", "0xd0b": "cortex-a76-aarch64"}

// Library directory and keyword file directory of this host inside the package
func platformDirs() (string, string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/amd64":
		return "lib/mac/x86_64", "mac", nil
	case "darwin/arm64":
		return "lib/mac/arm64", "mac", nil
	case "linux/amd64":
		return "lib/linux/x86_64", "linux", nil
	case "windows/amd64":
		return "lib/windows/amd64", "windows", nil
	case "windows/arm64":
		return "lib/windows/arm64", "windows", nil
	case "linux/arm64":
		info, err := os.ReadFile("/proc/cpuinfo")
		if err != nil {
			return "", "", fmt.Errorf("Porcupine needs the CPU part of this ARM board: %w", err)
		}
		part := ""
		for _, line := range strings.Split(string(info), "\n") {
			if strings.Contains(line, "CPU part") {
				fields := strings.Fields(line)
				part = strings.ToLower(fields[len(fields)-1])
				break
			}
		}
		if core, ok := raspberryPiParts[part]; ok {
			return "lib/raspberry-pi/" + core, "raspberry-pi", nil
		}
		return "", "", fmt.Errorf("Porcupine publishes 64 bit ARM Linux libraries only for Raspberry Pi boards, this CPU part is %q", part)
	}
	return "", "", fmt.Errorf("Porcupine publishes no library for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// Whether Porcupine publishes a library for this host
func Supported() bool {
	_, _, err := platformDirs()
	return err == nil && fetch.NativeSupported
}

func libraryName() string {
	switch runtime.GOOS {
	case "windows":
		return "libpv_porcupine.dll"
	case "darwin":
		return "libpv_porcupine.dylib"
	}
	return "libpv_porcupine.so"
}

// Picovoice status names in status code order
var statusNames = []string{"success", "out of memory", "I/O error", "invalid argument", "stop iteration", "key error",
	"invalid state", "runtime error", "activation error", "activation limit reached", "activation throttled", "activation refused"}

// An engine failure with the status the library returned and its error stack
type Error struct {
	Op     string
	Status int32
	Stack  []string
}

func (e *Error) Error() string {
	name := fmt.Sprintf("status %d", e.Status)
	if e.Status >= 0 && int(e.Status) < len(statusNames) {
		name = statusNames[e.Status]
	}
	msg := "Porcupine " + e.Op + " failed: " + name
	for i, line := range e.Stack {
		msg += fmt.Sprintf("\n  [%d] %s", i, line)
	}
	return msg
}

// The bound engine library with its model and built in keywords
type Library struct {
	Dir      string
	Model    string
	Keywords map[string]string

	setSDK         func(sdk *byte)
	init           func(accessKey, modelPath, device *byte, numKeywords int32, keywordPaths *uintptr, sensitivities *float32, handle *uintptr) int32
	deleteHandle   func(handle uintptr)
	process        func(handle uintptr, pcm *int16, result *int32) int32
	frameLength    func() int32
	sampleRate     func() int32
	version        func() uintptr
	getErrorStack  func(stack *uintptr, depth *int32) int32
	freeErrorStack func(stack uintptr)
}

var loaded struct {
	sync.Mutex
	lib *Library
}

// Library of this host, the package fetched into the icebath cache on first use
func Load(ctx context.Context) (*Library, error) {
	loaded.Lock()
	defer loaded.Unlock()
	if loaded.lib != nil {
		return loaded.lib, nil
	}
	libDir, keywordDir, err := platformDirs()
	if err != nil {
		return nil, err
	}
	tree, err := fetch.Tree(ctx, wheel)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(tree, "pvporcupine")
	native, err := fetch.OpenLibrary(filepath.Join(root, libDir, libraryName()))
	if err != nil {
		return nil, err
	}
	l := &Library{Dir: root, Model: filepath.Join(root, "lib", "common", "porcupine_params.pv"), Keywords: map[string]string{}}
	if err := native.BindAll([]fetch.Binding{{&l.setSDK, "pv_set_sdk"}, {&l.init, "pv_porcupine_init"},
		{&l.deleteHandle, "pv_porcupine_delete"}, {&l.process, "pv_porcupine_process"}, {&l.frameLength, "pv_porcupine_frame_length"},
		{&l.sampleRate, "pv_sample_rate"}, {&l.version, "pv_porcupine_version"}, {&l.getErrorStack, "pv_get_error_stack"},
		{&l.freeErrorStack, "pv_free_error_stack"}}); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "resources", "keyword_files", keywordDir))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name, _, _ := strings.Cut(e.Name(), "_")
		l.Keywords[name] = filepath.Join(root, "resources", "keyword_files", keywordDir, e.Name())
	}
	l.setSDK(fetch.CString("go"))
	loaded.lib = l
	return l, nil
}

// Built in keyword names in sorted order
func (l *Library) KeywordNames() []string {
	out := make([]string, 0, len(l.Keywords))
	for k := range l.Keywords {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Messages of the native error stack, freed after reading
func (l *Library) errorStack() []string {
	var stack uintptr
	var depth int32
	if l.getErrorStack(&stack, &depth) != 0 {
		return nil
	}
	out := make([]string, 0, depth)
	for i := 0; i < int(depth); i++ {
		out = append(out, fetch.GoString(fetch.PointerAt(stack, i)))
	}
	l.freeErrorStack(stack)
	return out
}

// One wake word detector over keyword files with their sensitivities
type Engine struct {
	lib         *Library
	mu          sync.Mutex
	handle      uintptr
	FrameLength int
	SampleRate  int
	Version     string
}

// Starts a detector for keyword files or built in keyword names, each with a sensitivity in 0 to 1
func (l *Library) New(accessKey string, keywords []string, sensitivities []float64) (*Engine, error) {
	if accessKey == "" {
		return nil, errors.New("Porcupine needs a Picovoice access key")
	}
	if len(keywords) == 0 || len(keywords) != len(sensitivities) {
		return nil, errors.New("Porcupine needs one sensitivity for each keyword")
	}
	paths := make([]string, len(keywords))
	for i, k := range keywords {
		if builtin, ok := l.Keywords[k]; ok {
			paths[i] = builtin
			continue
		}
		if strings.HasPrefix(k, "~") {
			if dir, err := os.UserHomeDir(); err == nil {
				k = filepath.Join(dir, strings.TrimPrefix(k, "~"))
			}
		}
		if _, err := os.Stat(k); err != nil {
			return nil, fmt.Errorf("Porcupine keyword %q is neither a keyword file nor a built in keyword (%s)", keywords[i], strings.Join(l.KeywordNames(), ", "))
		}
		paths[i] = k
	}
	ptrs := make([]uintptr, len(paths)+1)
	held := make([]*byte, len(paths))
	sens := make([]float32, len(paths)+1)
	for i, p := range paths {
		held[i] = fetch.CString(p)
		ptrs[i] = uintptr(unsafe.Pointer(held[i]))
		if sensitivities[i] < 0 || sensitivities[i] > 1 {
			return nil, errors.New("Porcupine sensitivities must be within 0 to 1")
		}
		sens[i] = float32(sensitivities[i])
	}
	e := &Engine{lib: l}
	status := l.init(fetch.CString(accessKey), fetch.CString(l.Model), fetch.CString("best"), int32(len(paths)), &ptrs[0], &sens[0], &e.handle)
	runtime.KeepAlive(held)
	if status != 0 {
		return nil, &Error{Op: "initialization", Status: status, Stack: l.errorStack()}
	}
	e.FrameLength, e.SampleRate, e.Version = int(l.frameLength()), int(l.sampleRate()), fetch.GoString(l.version())
	return e, nil
}

// Index of the keyword detected at the end of one frame, -1 when none
func (e *Engine) Process(pcm []int16) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.handle == 0 {
		return -1, errors.New("the Porcupine detector is closed")
	}
	if len(pcm) != e.FrameLength {
		return -1, fmt.Errorf("Porcupine frames hold %d samples, got %d", e.FrameLength, len(pcm))
	}
	var result int32
	if status := e.lib.process(e.handle, &pcm[0], &result); status != 0 {
		return -1, &Error{Op: "processing", Status: status, Stack: e.lib.errorStack()}
	}
	return int(result), nil
}

// Releases the detector
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.handle != 0 {
		e.lib.deleteHandle(e.handle)
		e.handle = 0
	}
}
