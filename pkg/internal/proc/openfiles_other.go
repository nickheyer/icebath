//go:build unix && !linux && !darwin && !freebsd && !solaris

package proc

// psutil reaches descriptors on this kernel through its own kinfo interface
const OpenFilesAvailable = true

// Regular files pid holds open through the kernel's kinfo_file interface for this OS
func OpenFiles(pid int) ([]OpenFile, error) {
	return kinfoOpenFilesForThisKernel(pid)
}
