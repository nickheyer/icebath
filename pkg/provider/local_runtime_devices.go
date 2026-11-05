package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"
)

// Device types of enum ggml_backend_dev_type in ggml-backend.h
const (
	GGMLDeviceGPU  = 1
	GGMLDeviceIGPU = 2
)

// Argument that turns this executable into the disposable ggml device reader child
const ggmlDevicesProbeArg = "__icebath_ggml_devices__"

// A GPU or integrated GPU the engine's ggml backend registers, Free capped at Total
type GGMLDevice struct {
	Description string
	Type        int
	Free        int64
	Total       int64
}

func init() {
	if len(os.Args) == 4 && os.Args[1] == ggmlDevicesProbeArg {
		os.Exit(ggmlDevicesMain(os.Args[2], os.Args[3], os.Stdout, os.Stderr))
	}
}

// Prints the device list of backend in directory as JSON, exit status 1 when the libraries fail
func ggmlDevicesMain(directory, backend string, stdout, stderr io.Writer) int {
	devices, err := readGGMLDevices(directory, backend)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, d := range devices {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, `{"description": %s, "type": %d, "free": %d, "total": %d}`,
			pyJSONQuote(d.Description), d.Type, d.Free, d.Total)
	}
	b.WriteString("]\n")
	_, _ = io.WriteString(stdout, b.String())
	return 0
}

// Shared library file name of name in directory for this platform
func ggmlLibrary(directory, name string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(directory, name+".dll")
	}
	return filepath.Join(directory, "lib"+name+".so")
}

// Reads every device the ggml backend registry of backend reports through the engine libraries
func readGGMLDevices(directory, backend string) ([]GGMLDevice, error) {
	release, err := addNativeDLLDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer release()
	base, err := openNativeLibrary(ggmlLibrary(directory, "ggml-base"), true)
	if err != nil {
		return nil, err
	}
	core, err := openNativeLibrary(ggmlLibrary(directory, "ggml"), true)
	if err != nil {
		return nil, err
	}
	load, err := core.sym("ggml_backend_load")
	if err != nil {
		return nil, err
	}
	cpath := append([]byte(ggmlLibrary(directory, "ggml-"+backend)), 0)
	registry := callNative(load, uintptr(unsafe.Pointer(&cpath[0])))
	runtime.KeepAlive(cpath)
	if registry == 0 {
		return []GGMLDevice{}, nil
	}
	names := []string{"ggml_backend_reg_dev_count", "ggml_backend_reg_dev_get", "ggml_backend_dev_description",
		"ggml_backend_dev_type", "ggml_backend_dev_memory"}
	fns := make([]uintptr, len(names))
	for i, name := range names {
		if fns[i], err = base.sym(name); err != nil {
			return nil, err
		}
	}
	devices := []GGMLDevice{}
	for index := uintptr(0); index < min(callNative(fns[0], registry), 64); index++ {
		device := callNative(fns[1], registry, index)
		if device == 0 {
			continue
		}
		var free, total uint64
		callNativeOut(fns[4], device, &free, &total)
		description := callNative(fns[2], device)
		if description == 0 {
			return nil, errors.New("'NoneType' object has no attribute 'decode'")
		}
		devices = append(devices, GGMLDevice{
			Description: loadDecodeReplace(cStringAt(description)),
			Type:        int(int32(callNative(fns[3], device))),
			Free:        saturatingInt64(min(free, total)),
			Total:       saturatingInt64(total),
		})
	}
	return devices, nil
}

func saturatingInt64(v uint64) int64 {
	if v > 1<<63-1 {
		return 1<<63 - 1
	}
	return int64(v)
}

var (
	ggmlDevicesExecutable = os.Executable
	ggmlDevicesTimeout    = 20 * time.Second
)

// GPU and iGPU devices the backend library in engineDir registers, empty on any failure
func ProbeDevices(engineDir, backend string) []GGMLDevice {
	exe, err := ggmlDevicesExecutable()
	if err != nil {
		return []GGMLDevice{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), ggmlDevicesTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, ggmlDevicesProbeArg, engineDir, backend)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	hideChildWindow(cmd)
	if err := cmd.Run(); err != nil {
		return []GGMLDevice{}
	}
	parsed, ok := moaJSONLoads(loadDecodeReplace(stdout.Bytes()))
	items, isList := parsed.([]any)
	if !ok || !isList {
		return []GGMLDevice{}
	}
	return ggmlDevicesFromJSON(items)
}

// Keeps dict entries of GPU or iGPU type with a positive integer total and a string description
func ggmlDevicesFromJSON(items []any) []GGMLDevice {
	out := []GGMLDevice{}
	for _, item := range items {
		d, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind, ok := pyNumericInt(d["type"])
		if !ok || (kind != GGMLDeviceGPU && kind != GGMLDeviceIGPU) {
			continue
		}
		total, ok := pyIntValue(d["total"])
		description, isStr := d["description"].(string)
		if !ok || total <= 0 || !isStr {
			continue
		}
		free, _ := pyIntValue(d["free"])
		out = append(out, GGMLDevice{Description: description, Type: int(kind), Free: free, Total: total})
	}
	return out
}

// Integer value of a Python int or bool
func pyIntValue(v any) (int64, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// Integer a JSON number or bool compares equal to in Python, false for fractional values
func pyNumericInt(v any) (int64, bool) {
	if f, ok := v.(float64); ok {
		if f != float64(int64(f)) {
			return 0, false
		}
		return int64(f), true
	}
	return pyIntValue(v)
}
