package provider

// Budget with lazy reads on and every optional field at its default
func NewHardwareBudget(usableVRAM, totalDevice, ramAvailable int64) HardwareBudget {
	return HardwareBudget{UsableVRAMBytes: usableVRAM, TotalDeviceBytes: totalDevice,
		RAMAvailableBytes: ramAvailable, LazyReads: true}
}
