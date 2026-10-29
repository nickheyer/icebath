package term

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Opens a pseudo terminal pair through the multiplexer
func openPty() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := master.Fd()
	if err := ioctl(fd, unix.TIOCPTYGRANT, 0); err != nil {
		master.Close()
		return nil, nil, err
	}
	if err := ioctl(fd, unix.TIOCPTYUNLK, 0); err != nil {
		master.Close()
		return nil, nil, err
	}
	var name [128]byte
	if err := ioctl(fd, unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		master.Close()
		return nil, nil, err
	}
	n := 0
	for n < len(name) && name[n] != 0 {
		n++
	}
	slave, err := os.OpenFile(string(name[:n]), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func ioctl(fd, req, arg uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, req, arg)
	if errno != 0 {
		return errno
	}
	return nil
}
