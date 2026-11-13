package hostinfo

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/windows"
)

var nvcuda = windows.NewLazySystemDLL("nvcuda.dll")

// First CUDA device through the NVIDIA driver library, ErrAbsent without a driver or device
func probeCUDA() (CUDADevice, error) {
	if err := nvcuda.Load(); errors.Is(err, windows.ERROR_MOD_NOT_FOUND) {
		return CUDADevice{}, fmt.Errorf("cuda driver: %w", ErrAbsent)
	} else if err != nil {
		return CUDADevice{}, fmt.Errorf("cuda driver: %w", err)
	}
	return probeCUDADevice(func(symbol string) uintptr {
		proc := nvcuda.NewProc(symbol)
		if proc.Find() != nil {
			return 0
		}
		return proc.Addr()
	})
}

//go:uintptrescapes
func cudaCall(fn uintptr, args ...uintptr) int32 {
	r, _, _ := syscall.SyscallN(fn, args...)
	return int32(r)
}
