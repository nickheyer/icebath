package local

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/dl"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	deviceProbeArg     = "__icebath_ggml_devices__"
	ggmlDeviceGPU      = 1
	ggmlDeviceIGPU     = 2
	deviceProbeTimeout = 20 * time.Second
	maxProbedDevices   = 64
)

type Device struct {
	Description string `json:"description"`
	Type        int    `json:"type"`
	Free        int64  `json:"free"`
	Total       int64  `json:"total"`
}

func init() {
	if len(os.Args) > 1 && os.Args[1] == deviceProbeArg {
		os.Exit(runDeviceProbe(os.Args[2:]))
	}
}

func runDeviceProbe(args []string) int {
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "%s takes the engine directory and the backend\n", deviceProbeArg)
		return 2
	}
	devices, err := readDevices(args[0], args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	out, err := json.Marshal(devices)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(string(out))
	return 0
}

func (r *Runtime) probeDevices(ctx context.Context, engineDir, backend string) []Device {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	runCtx, cancel := context.WithTimeout(ctx, deviceProbeTimeout)
	defer cancel()
	cmd := exec.Command(exe, deviceProbeArg, engineDir, backend)
	proc.Hide(cmd)
	res, err := proc.Run(runCtx, cmd)
	if err != nil || !res.Exit.Success() {
		return nil
	}
	var listed []Device
	if json.Unmarshal(res.Stdout, &listed) != nil {
		return nil
	}
	var out []Device
	for _, d := range listed {
		if (d.Type == ggmlDeviceGPU || d.Type == ggmlDeviceIGPU) && d.Total > 0 {
			out = append(out, d)
		}
	}
	return out
}

func ggmlLibrary(dir, name string) string {
	return filepath.Join(dir, libraryFileName(name))
}

func readDevices(dir, backend string) ([]Device, error) {
	release, err := addLibraryDir(dir)
	if err != nil {
		return nil, err
	}
	defer release()
	base, err := dl.Open(ggmlLibrary(dir, "ggml-base"), dl.Global)
	if err != nil {
		return nil, err
	}
	core, err := dl.Open(ggmlLibrary(dir, "ggml"), dl.Global)
	if err != nil {
		return nil, err
	}
	var load func(path string) uintptr
	if err := core.Bind(&load, "ggml_backend_load"); err != nil {
		return nil, err
	}
	var (
		count       func(reg uintptr) uintptr
		get         func(reg, index uintptr) uintptr
		description func(dev uintptr) string
		devType     func(dev uintptr) int32
		memory      func(dev uintptr, free, total *uintptr)
	)
	if err := base.BindAll([]dl.Binding{
		{Fn: &count, Name: "ggml_backend_reg_dev_count"},
		{Fn: &get, Name: "ggml_backend_reg_dev_get"},
		{Fn: &description, Name: "ggml_backend_dev_description"},
		{Fn: &devType, Name: "ggml_backend_dev_type"},
		{Fn: &memory, Name: "ggml_backend_dev_memory"},
	}); err != nil {
		return nil, err
	}
	registry := load(ggmlLibrary(dir, "ggml-"+backend))
	devices := []Device{}
	if registry == 0 {
		return devices, nil
	}
	n := min(count(registry), maxProbedDevices)
	for i := uintptr(0); i < n; i++ {
		dev := get(registry, i)
		if dev == 0 {
			continue
		}
		var free, total uintptr
		memory(dev, &free, &total)
		devices = append(devices, Device{
			Description: strings.ToValidUTF8(description(dev), "�"), Type: int(devType(dev)),
			Free: int64(min(free, total)), Total: int64(total),
		})
	}
	return devices, nil
}
