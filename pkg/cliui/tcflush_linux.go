package cliui

import "golang.org/x/sys/unix"

// Discards terminal input received but not yet read
func tcflushInput(fd int) { unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH) }
