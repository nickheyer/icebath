package tools

// Physical memory of the host in bytes, false when the platform probe fails
func PhysicalMemoryBytes() (int64, bool) { return physicalMemoryBytes() }
