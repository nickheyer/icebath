package packages

import "golang.org/x/sys/unix"

// Whether this process runs under Rosetta on Apple silicon
func emulatedOnArm64() bool {
	v, err := unix.SysctlUint32("sysctl.proc_translated")
	return err == nil && v == 1
}
