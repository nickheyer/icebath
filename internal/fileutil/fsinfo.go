package fileutil

import "io/fs"

// Size of a volume and how much of it is used and free, in bytes
type DiskSpace struct {
	Total uint64
	Used  uint64
	Free  uint64
}

// Set of permissions Accessible tests
type Access uint32

// Execute, write and read permission, combinable with bitwise or
const (
	AccessExec Access = 1 << iota
	AccessWrite
	AccessRead
)

// Identity of a file by device and inode, or by volume serial and file index on Windows
type FileID struct {
	Device uint64
	Inode  uint64
}

// Reports whether path is an NTFS directory junction, which exists only on Windows
func IsJunction(path string) bool { return isMountPoint(path) }

// Ten character ls style rendering of a mode, type letter first and setuid, setgid and sticky in the execute slots
func ModeString(m fs.FileMode) string {
	b := []byte("----------")
	switch {
	case m&fs.ModeDir != 0:
		b[0] = 'd'
	case m&fs.ModeSymlink != 0:
		b[0] = 'l'
	case m&fs.ModeNamedPipe != 0:
		b[0] = 'p'
	case m&fs.ModeSocket != 0:
		b[0] = 's'
	case m&fs.ModeCharDevice != 0:
		b[0] = 'c'
	case m&fs.ModeDevice != 0:
		b[0] = 'b'
	case m&fs.ModeIrregular != 0:
		b[0] = '?'
	}
	const rwx = "rwx"
	for i := range 9 {
		if m&(1<<uint(8-i)) != 0 {
			b[1+i] = rwx[i%3]
		}
	}
	special := func(slot int, set bool, lower, upper byte) {
		switch {
		case set && b[slot] == 'x':
			b[slot] = lower
		case set:
			b[slot] = upper
		}
	}
	special(3, m&fs.ModeSetuid != 0, 's', 'S')
	special(6, m&fs.ModeSetgid != 0, 's', 'S')
	special(9, m&fs.ModeSticky != 0, 't', 'T')
	return string(b)
}
