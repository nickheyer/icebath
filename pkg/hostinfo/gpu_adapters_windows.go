package hostinfo

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	dxgiSoftwareFlag = 0x2
	dxgiMaxAdapters  = 16
	dxgiSlotRelease  = 2
	dxgiSlotEnum1    = 12
	dxgiSlotGetDesc1 = 10
)

var iidDXGIFactory1 = windows.GUID{Data1: 0x770AAE78, Data2: 0xF26F, Data3: 0x4DBA,
	Data4: [8]byte{0xA8, 0x29, 0x25, 0x3C, 0x83, 0xD1, 0xB3, 0x87}}

type dxgiAdapterDesc1 struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	AdapterLuidLow        uint32
	AdapterLuidHigh       int32
	Flags                 uint32
}

type dxgiObject struct{ vtbl *[32]uintptr }

// Calls the COM method in vtable slot of obj and returns its HRESULT
func dxgiCall(obj *dxgiObject, slot int, args ...uintptr) int32 {
	r, _, _ := syscall.SyscallN(obj.vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(obj))}, args...)...)
	return int32(r)
}

// Adapters from CreateDXGIFactory1 and EnumAdapters1, nil when dxgi cannot be loaded or answers an error
func windowsGPUAdaptersImpl() []GPUAdapter {
	dxgi := windows.NewLazySystemDLL("dxgi.dll")
	create := dxgi.NewProc("CreateDXGIFactory1")
	if create.Find() != nil {
		return nil
	}
	var factory *dxgiObject
	if r, _, _ := create.Call(uintptr(unsafe.Pointer(&iidDXGIFactory1)), uintptr(unsafe.Pointer(&factory))); int32(r) != 0 {
		return nil
	}
	defer dxgiCall(factory, dxgiSlotRelease)
	found := []GPUAdapter{}
	for index := 0; index < dxgiMaxAdapters; index++ {
		var adapter *dxgiObject
		if dxgiCall(factory, dxgiSlotEnum1, uintptr(index), uintptr(unsafe.Pointer(&adapter))) != 0 {
			break
		}
		var desc dxgiAdapterDesc1
		if dxgiCall(adapter, dxgiSlotGetDesc1, uintptr(unsafe.Pointer(&desc))) == 0 {
			found = append(found, GPUAdapter{
				Description:    windows.UTF16ToString(desc.Description[:]),
				VendorID:       desc.VendorID,
				DedicatedBytes: int64(desc.DedicatedVideoMemory),
				SharedBytes:    int64(desc.SharedSystemMemory),
				Software:       desc.Flags&dxgiSoftwareFlag != 0,
			})
		}
		dxgiCall(adapter, dxgiSlotRelease)
	}
	return found
}
