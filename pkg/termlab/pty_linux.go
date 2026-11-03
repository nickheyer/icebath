package termlab

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// Opens a pseudo terminal pair through the multiplexer
func openPty() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, nil, err
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

// Copies the window size of one terminal, pixels included, onto another
func copySize(from, to *os.File) (*unix.Winsize, error) {
	ws, err := unix.IoctlGetWinsize(int(from.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return nil, err
	}
	return ws, unix.IoctlSetWinsize(int(to.Fd()), unix.TIOCSWINSZ, ws)
}
