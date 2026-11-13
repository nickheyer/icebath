//go:build linux || darwin

package hostinfo

import (
	"fmt"

	"github.com/nickheyer/icebath/internal/dl"
)

var cudaLibraries = []string{"libcuda.so.1", "libcuda.so"}

// First CUDA device through the driver library, kept loaded once opened, ErrAbsent without a driver or device
func probeCUDA() (CUDADevice, error) {
	for _, name := range cudaLibraries {
		lib, err := dl.Open(name, dl.Local)
		if err != nil {
			continue
		}
		return probeCUDADevice(func(symbol string) uintptr {
			addr, err := lib.Symbol(symbol)
			if err != nil {
				return 0
			}
			return addr
		})
	}
	return CUDADevice{}, fmt.Errorf("cuda driver: %w", ErrAbsent)
}

//go:uintptrescapes
func cudaCall(fn uintptr, args ...uintptr) int32 {
	r, _ := dl.Call(fn, args...)
	return int32(r)
}
