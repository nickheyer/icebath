package hostinfo

import (
	"errors"
	"fmt"
	"time"
	"unsafe"
)

const (
	cudaErrorNoDevice           = 100
	cuDeviceAttributeIntegrated = 18
	cudaRetry                   = time.Minute
)

// First device as the CUDA driver API sees it, Integrated nil when the driver gives no verdict
type CUDADevice struct {
	TotalBytes uint64
	Integrated *bool
}

// First device as the driver API sees it, kept once a device answers, a miss probed again after a minute
func (h *Host) CUDA() (CUDADevice, error) { return h.cuda.get(cudaRetry, probeCUDA) }

// Allocator pool and unified memory verdict of the first device through the driver library symbols
func probeCUDADevice(symbol func(string) uintptr) (CUDADevice, error) {
	cuInit, cuDeviceGet, cuDeviceGetAttribute := symbol("cuInit"), symbol("cuDeviceGet"), symbol("cuDeviceGetAttribute")
	cuDeviceTotalMem := symbol("cuDeviceTotalMem_v2")
	if cuDeviceTotalMem == 0 {
		cuDeviceTotalMem = symbol("cuDeviceTotalMem")
	}
	if cuInit == 0 || cuDeviceGet == 0 || cuDeviceGetAttribute == 0 || cuDeviceTotalMem == 0 {
		return CUDADevice{}, errors.New("cuda driver: library lacks the device query entry points")
	}
	switch result := cudaCall(cuInit, 0); result {
	case 0:
	case cudaErrorNoDevice:
		return CUDADevice{}, fmt.Errorf("cuda device: %w", ErrAbsent)
	default:
		return CUDADevice{}, fmt.Errorf("cuda driver: cuInit returned CUDA error %d", result)
	}
	var device int32
	if result := cudaCall(cuDeviceGet, uintptr(unsafe.Pointer(&device)), 0); result != 0 {
		return CUDADevice{}, fmt.Errorf("cuda driver: cuDeviceGet returned CUDA error %d", result)
	}
	var total uintptr
	if result := cudaCall(cuDeviceTotalMem, uintptr(unsafe.Pointer(&total)), uintptr(device)); result != 0 {
		return CUDADevice{}, fmt.Errorf("cuda driver: cuDeviceTotalMem returned CUDA error %d", result)
	}
	if total == 0 {
		return CUDADevice{}, errors.New("cuda driver: device reports no memory")
	}
	d := CUDADevice{TotalBytes: uint64(total)}
	var integrated int32
	if cudaCall(cuDeviceGetAttribute, uintptr(unsafe.Pointer(&integrated)), cuDeviceAttributeIntegrated, uintptr(device)) == 0 {
		verdict := integrated != 0
		d.Integrated = &verdict
	}
	return d, nil
}
