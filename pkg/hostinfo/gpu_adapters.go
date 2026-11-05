package hostinfo

// One graphics adapter as DXGI enumerates it
type GPUAdapter struct {
	Description    string
	VendorID       uint32
	DedicatedBytes int64
	SharedBytes    int64
	Software       bool
}

// What Windows lets this adapter allocate, dedicated plus shared memory as Task Manager shows it
func (a GPUAdapter) MemoryBytes() int64 { return a.DedicatedBytes + a.SharedBytes }

var windowsGPUAdaptersOnce = cached(func() []GPUAdapter { return windowsGPUAdaptersImpl() })

// Every adapter DXGI enumerates in its order, empty off Windows or when DXGI cannot answer
func WindowsGPUAdapters() []GPUAdapter { return windowsGPUAdaptersOnce() }
