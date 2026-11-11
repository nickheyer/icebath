package gateway

// Replaces the boot fingerprint and the disk fingerprint reader, the returned func putting the previous pair back
func SwapCodeSkewFingerprints(boot *string, disk func() (string, bool)) func() {
	bootFingerprintMu.Lock()
	savedBoot, savedDisk := bootFingerprint, codeSkewFingerprint
	bootFingerprint, codeSkewFingerprint = boot, disk
	bootFingerprintMu.Unlock()
	return func() {
		bootFingerprintMu.Lock()
		bootFingerprint, codeSkewFingerprint = savedBoot, savedDisk
		bootFingerprintMu.Unlock()
	}
}
