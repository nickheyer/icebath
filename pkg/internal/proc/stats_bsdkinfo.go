//go:build openbsd || netbsd

package proc

// Descriptor counts are read through the kern file sysctl by pid
const NumFDsAvailable = true

// Count of descriptors pid holds open, the kinfo_file records of its descriptor table
func NumFDs(pid int) (int, error) {
	raw, err := sysctlFiles(pid)
	if err != nil {
		return 0, kinfoError(pid, "kinfo_getfile()", err)
	}
	return len(raw) / kinfoFileSize, nil
}
