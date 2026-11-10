package pyshutil

import "golang.org/x/sys/unix"

// Sets the file flags of a symlink itself through a descriptor opened on the link
func lchflags(path string, flags uint32) error {
	fd, err := unix.Open(path, unix.O_SYMLINK|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Fchflags(fd, int(flags))
}
