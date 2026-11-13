//go:build linux || darwin

package state

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// One descriptor of this process with the identity and size of the file it references
type ownFD struct {
	fd   int
	id   fileIdentity
	size int64
}

// Descriptors of this process from the first listable descriptor table, false when neither table lists
func ownDescriptors() ([]ownFD, bool) {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		out := make([]ownFD, 0, len(entries))
		for _, e := range entries {
			fd, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			var st unix.Stat_t
			if unix.Fstat(fd, &st) != nil {
				continue
			}
			out = append(out, ownFD{fd: fd, id: fileIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}, size: st.Size})
		}
		return out, true
	}
	return nil, false
}

// Descriptor this process holds on the exact inode id, never chosen by pathname, false when none or unlistable
func ownDescriptorFor(id fileIdentity) (ownFD, bool) {
	fds, _ := ownDescriptors()
	for _, d := range fds {
		if d.id == id {
			return d, true
		}
	}
	return ownFD{}, false
}
