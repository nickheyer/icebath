package hostinfo

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/nickheyer/icebath/internal/dl"
)

const (
	hostCPULoadInfo      = 3
	hostCPULoadInfoCount = 4
	hostVMInfo64         = 4
	hostVMInfo64Count    = 38
)

// Words a mach host statistics call fills for flavor, through a host port released afterwards
func hostStatistics(symbol string, flavor, count uint32) ([]byte, error) {
	names := []string{"mach_host_self", symbol, "mach_port_deallocate", "mach_task_self_"}
	addrs := make([]uintptr, len(names))
	for i, name := range names {
		addr, err := dl.Self().Symbol(name)
		if err != nil {
			return nil, err
		}
		addrs[i] = addr
	}
	port, _ := dl.Call(addrs[0])
	if uint32(port) == 0 {
		return nil, fmt.Errorf("%s: no host port", symbol)
	}
	task := *dl.At[uint32](addrs[3])
	defer dl.Call(addrs[2], uintptr(task), port)
	buf := make([]byte, 4*count)
	filled := count
	ret, _ := dl.Call(addrs[1], port, uintptr(flavor), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&filled)))
	runtime.KeepAlive(buf)
	if int32(ret) != 0 {
		return nil, fmt.Errorf("%s: kern_return %d", symbol, int32(ret))
	}
	if filled > count {
		filled = count
	}
	return buf[:4*filled], nil
}

// Page size the kernel counts host statistics pages in, true kernel pages even under Rosetta
func kernelPageSize() (uint64, error) {
	addr, err := dl.Self().Symbol("vm_kernel_page_size")
	if err != nil {
		return 0, err
	}
	size := uint64(*dl.At[uintptr](addr))
	if size == 0 {
		return 0, errors.New("vm_kernel_page_size: zero")
	}
	return size, nil
}
