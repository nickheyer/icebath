package hostinfo

// IsNvidiaN1XPCIID recognizes the N1X device range packed above the NVIDIA vendor id
func IsNvidiaN1XPCIID(pciID int) bool {
	return pciID&0xFFFF == 0x10DE && 0x2E00 <= pciID>>16 && pciID>>16 <= 0x2E3F
}
