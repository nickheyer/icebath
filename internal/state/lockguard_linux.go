package state

import "golang.org/x/sys/unix"

// Open file description lock command of the Linux kernel
const ofdSetLK = unix.F_OFD_SETLK
