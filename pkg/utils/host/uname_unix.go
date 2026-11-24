//go:build unix

package hostutil

import (
	"runtime"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

type unameFields struct{ sysname, release, machine string }

// Fields of uname for this process, empty when the kernel refuses
var uname = sync.OnceValue(func() unameFields {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return unameFields{}
	}
	return unameFields{
		sysname: unix.ByteSliceToString(u.Sysname[:]),
		release: unix.ByteSliceToString(u.Release[:]),
		machine: unix.ByteSliceToString(u.Machine[:]),
	}
})

func readOSName() string {
	if name := uname().sysname; name != "" {
		return name
	}
	return goosName()
}

func readMachine() string {
	if name := uname().machine; name != "" {
		return name
	}
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
			return "arm64"
		}
		return "aarch64"
	case "386":
		return "i686"
	case "arm":
		return "armv7l"
	case "loong64":
		return "loongarch64"
	}
	return runtime.GOARCH
}

func readRelease() string { return uname().release }

// Label of a Unix host: the product release on Apple systems, the C library on Linux, pointer width and binary format elsewhere
func readPlatform() string {
	name, rel := OSName(), Release()
	if product, version := productRelease(); version != "" {
		name, rel = product, version
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "android" {
		if lib := libcLabel(); lib != "" {
			return joinLabel(name, rel, Machine(), "with", lib)
		}
		return joinLabel(name, rel, Machine())
	}
	return joinLabel(name, rel, Machine(), strconv.Itoa(strconv.IntSize)+"bit", binaryFormat())
}

// Executable format of programs on this system
func binaryFormat() string {
	switch runtime.GOOS {
	case "darwin", "ios":
		return "Mach-O"
	case "aix":
		return "XCOFF"
	}
	return "ELF"
}
