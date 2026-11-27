// Picovoice Porcupine, its library, model and built in keywords read from the pvporcupine package of its Python set
package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/porcupine/config"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/speech/native"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
)

func Register(registry *speech.Registry) {
	registry.Register(speech.Services{Waking: []speech.Engine{{
		Name: config.Name, Title: "Porcupine", About: "Hears a few fixed keywords, needs a free Picovoice access key",
		Rank: 3, Key: true, Extra: config.Extra,
		Supported: func(sp *speech.Speech) bool {
			reason, known := sp.Packages().ExtraUnsupported(config.Extra)
			return known && reason == ""
		},
		Installed: func(sp *speech.Speech) bool {
			_, ok := sp.Packages().ExtraModule(config.Extra, "pvporcupine")
			return ok
		},
		New: open,
	}}})
}

// Picovoice status codes and what each failure means
var statuses = []string{"success", "out of memory", "I/O error", "invalid argument", "stop iteration", "key error", "invalid state", "runtime error",
	"activation error", "activation limit reached", "activation throttled", "activation refused"}

// A failed step of the library, with its status and the messages of its error stack
type failure struct {
	status  int32
	message string
	stack   []string
}

func (f *failure) Error() string {
	text := f.message
	if f.status != 0 {
		name := fmt.Sprintf("status %d", f.status)
		if f.status > 0 && int(f.status) < len(statuses) {
			name = statuses[f.status]
		}
		text += " (" + name + ")"
	}
	if len(f.stack) > 0 {
		text += ":"
		for i, line := range f.stack {
			text += fmt.Sprintf("\n  [%d] %s", i, line)
		}
	}
	return text
}

// Raspberry Pi cores Porcupine has builds for, by the CPU part of the kernel's description
var piCores = map[string]string{"0xb76": "arm11", "0xd03": "cortex-a53", "0xd08": "cortex-a72", "0xd0b": "cortex-a76"}

// Raspberry Pi cores of 64 bit builds, besides those of piCores
var piMachines = strs.Set("arm11", "cortex-a53", "cortex-a72", "cortex-a76", "cortex-a53-aarch64", "cortex-a72-aarch64", "cortex-a76-aarch64")

// Machine the kernel names, as uname -m answers, Go's own name of it when it can't be asked
func uname() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "uname", "-m").Output()
	if machine := strings.TrimSpace(string(out)); err == nil && machine != "" {
		return machine
	}
	return runtime.GOARCH
}

// System and machine of this host as Picovoice names them, a Raspberry Pi's machine its core with -aarch64 on a 64 bit build
func host() (string, string, error) {
	switch runtime.GOOS {
	case "darwin":
		return "Darwin", strings.ReplaceAll(runtime.GOARCH, "amd64", "x86_64"), nil
	case "windows":
		return "Windows", cmp.Or(map[string]string{"amd64": "AMD64", "arm64": "ARM64", "386": "x86"}[runtime.GOARCH], strings.ToUpper(runtime.GOARCH)), nil
	case "linux", "android":
	default:
		system := map[string]string{"freebsd": "FreeBSD", "openbsd": "OpenBSD", "netbsd": "NetBSD", "dragonfly": "DragonFly", "solaris": "SunOS",
			"illumos": "SunOS", "aix": "AIX"}[runtime.GOOS]
		return "", "", fmt.Errorf("Porcupine has no build for %s.", cmp.Or(system, strings.ToUpper(runtime.GOOS[:1])+runtime.GOOS[1:]))
	}
	machine := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if machine == "" {
		machine = uname()
	}
	suffix := ""
	switch machine {
	case "x86_64":
		return "Linux", machine, nil
	case "aarch64", "armv7l", "armv6l":
		if strconv.IntSize == 64 {
			suffix = "-" + machine
		}
	default:
		return "", "", fmt.Errorf("Porcupine has no build for %s processors.", machine)
	}
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", "", fmt.Errorf("The processor of this computer could not be told: %w", err)
	}
	info := strings.ToValidUTF8(string(raw), "�")
	part, found := "", false
	for _, line := range strings.Split(info, "\n") {
		if strings.Contains(line, "CPU part") {
			fields := strings.Fields(line)
			part, found = strings.ToLower(fields[len(fields)-1]), true
			break
		}
	}
	if !found {
		return "", "", errors.New("The processor of this computer could not be told, because /proc/cpuinfo has no CPU part line.")
	}
	if core, ok := piCores[part]; ok {
		return "Linux", core + suffix, nil
	}
	if machine == "armv7l" {
		slog.Warn(fmt.Sprintf("WARNING: Please be advised that this device (CPU part = %s) is not officially supported by Picovoice. Falling back to "+
			"the armv6-based (Raspberry Pi Zero) library. This is not tested nor optimal.", part))
		return "Linux", "arm11", nil
	}
	return "", "", fmt.Errorf("Porcupine has no build for the processor with CPU part %s.", part)
}

func unsupported(system, machine string) error {
	return fmt.Errorf("Porcupine has no build for %s on %s.", system, machine)
}

// Library of the host under the package
func library(dir, system, machine string) (string, error) {
	switch {
	case system == "Darwin" && (machine == "x86_64" || machine == "arm64"):
		return filepath.Join(dir, "lib", "mac", machine, "libpv_porcupine.dylib"), nil
	case system == "Linux" && machine == "x86_64":
		return filepath.Join(dir, "lib", "linux", "x86_64", "libpv_porcupine.so"), nil
	case system == "Linux" && piMachines[machine]:
		return filepath.Join(dir, "lib", "raspberry-pi", machine, "libpv_porcupine.so"), nil
	case system == "Windows" && (strings.EqualFold(machine, "amd64") || strings.EqualFold(machine, "arm64")):
		return filepath.Join(dir, "lib", "windows", strings.ToLower(machine), "libpv_porcupine.dll"), nil
	}
	return "", unsupported(system, machine)
}

// Built in keywords of the host by name, each the file under the package whose name starts with it
func keywords(dir, system, machine string) (map[string]string, error) {
	subdir := ""
	switch {
	case system == "Darwin":
		subdir = "mac"
	case system == "Linux" && machine == "x86_64":
		subdir = "linux"
	case system == "Linux" && piMachines[machine]:
		subdir = "raspberry-pi"
	case system == "Windows":
		subdir = "windows"
	default:
		return nil, unsupported(system, machine)
	}
	folder := filepath.Join(dir, "resources", "keyword_files", subdir)
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, fmt.Errorf("The keyword folder of Porcupine can't be read: %w", err)
	}
	out := map[string]string{}
	for _, entry := range entries {
		out[strings.Split(entry.Name(), "_")[0]] = filepath.Join(folder, entry.Name())
	}
	return out, nil
}

// Whether a keyword names a file rather than a built in keyword
func file(keyword string) bool {
	if strings.ContainsRune(keyword, os.PathSeparator) {
		return true
	}
	for _, ext := range []string{".onnx", ".tflite", ".ppn"} {
		if strings.HasSuffix(keyword, ext) {
			return true
		}
	}
	_, err := os.Stat(keyword)
	return keyword != "" && err == nil
}

// Refusal of a file the engine needs that is not there, named as it was given
func need(path, shown, what string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("There is no %s at %s.", what, shown)
	}
	return nil
}

// Builds the engine from the profile's access key and its keyword, built in or a file; the sensitivity inverted, Porcupine's being
// looser the higher it is
func open(ctx context.Context, wake *speech.Wake) (speech.Hotword, error) {
	if _, ok := wake.Speech.Packages().ExtraPython(config.Extra); !ok {
		return nil, errors.New("Porcupine is not installed. Run " + packages.InstallHint(config.Extra) + " to install it.")
	}
	dir, ok := wake.Speech.Packages().ExtraModule(config.Extra, "pvporcupine")
	if !ok {
		return nil, errors.New("The Python package pvporcupine is missing from the install of Porcupine. Run " + packages.InstallHint(config.Extra) + " again.")
	}
	system, machine, err := host()
	if err != nil {
		return nil, err
	}
	builtin, err := keywords(dir, system, machine)
	if err != nil {
		return nil, err
	}
	if wake.AccessKey == "" {
		return nil, errors.New("Porcupine needs a Picovoice access key. Get one free at https://console.picovoice.ai, then set PORCUPINE_ACCESS_KEY in your keys file.")
	}
	keyword := wake.Setting(config.Keyword)
	if keyword == "" {
		keyword = config.DefaultKeyword
	}
	path, shown := terminal.ExpandUser(keyword), keyword
	if !file(keyword) {
		if path, ok = builtin[keyword]; !ok {
			names := slices.Sorted(maps.Keys(builtin))
			return nil, fmt.Errorf("Porcupine has no keyword %s. Set Keyword in /settings under Wake word › Porcupine to one of these, or to a keyword file: %s.", keyword, strings.Join(names, ", "))
		}
		shown = path
	}
	lib, err := library(dir, system, machine)
	if err != nil {
		return nil, err
	}
	model := filepath.Join(dir, "lib", "common", "porcupine_params.pv")
	for _, missing := range []error{need(lib, lib, "library of Porcupine"), need(model, model, "model file"), need(path, shown, "keyword file")} {
		if missing != nil {
			return nil, missing
		}
	}
	return create(wake.AccessKey, lib, model, path, float32(1-wake.Sensitivity))
}

// One engine of the native library
type engine struct {
	handle  uintptr
	process func(handle uintptr, pcm *int16, result *int32) int32
	remove  func(handle uintptr)
	stack   func(stack *uintptr, depth *int32) int32
	free    func(stack uintptr)
	length  int
}

func create(key, library, model, keyword string, sensitivity float32) (*engine, error) {
	lib, err := native.Open(library, false)
	if err != nil {
		return nil, err
	}
	e := &engine{}
	var (
		sdk    func(name *byte)
		init   func(key, model, device *byte, count int32, keywords *uintptr, sensitivities *float32, handle *uintptr) int32
		length func() int32
	)
	if err := lib.BindAll(map[string]any{"pv_set_sdk": &sdk, "pv_porcupine_init": &init, "pv_porcupine_frame_length": &length,
		"pv_porcupine_process": &e.process, "pv_porcupine_delete": &e.remove, "pv_get_error_stack": &e.stack, "pv_free_error_stack": &e.free}); err != nil {
		return nil, err
	}
	var held [][]byte
	sdk(native.Chars(&held, "python"))
	path := native.CString(keyword)
	paths := []uintptr{uintptr(unsafe.Pointer(&path[0])), 0}
	sensitivities := []float32{sensitivity, 0}
	status := init(native.Chars(&held, key), native.Chars(&held, model), native.Chars(&held, "best"), 1, &paths[0], &sensitivities[0], &e.handle)
	runtime.KeepAlive(held)
	runtime.KeepAlive(path)
	if status != 0 {
		return nil, e.failed(status, "Initialization failed")
	}
	e.length = int(length())
	return e, nil
}

// Failure of a step with the messages of the native error stack, freed after reading; the stack's own failure when it can't be read
func (e *engine) failed(status int32, message string) error {
	var at uintptr
	var depth int32
	if code := e.stack(&at, &depth); code != 0 {
		return &failure{status: code, message: "Unable to get Porcupine error state"}
	}
	var stack []string
	for i := range int(depth) {
		stack = append(stack, native.GoString(native.PointerAt(at, i)))
	}
	e.free(at)
	return &failure{status: status, message: message, stack: stack}
}

func (e *engine) FrameLength() int { return e.length }

// Whether the frame ends a keyword
func (e *engine) Process(frame []int16) (bool, error) {
	if len(frame) != e.length {
		return false, fmt.Errorf("Invalid frame length. expected %d but received %d", e.length, len(frame))
	}
	var result int32
	if status := e.process(e.handle, unsafe.SliceData(frame), &result); status != 0 {
		return false, e.failed(status, "Processing failed")
	}
	return result >= 0, nil
}

func (e *engine) Reset() error { return nil }

func (e *engine) Close() { e.remove(e.handle) }
