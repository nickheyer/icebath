package hostinfo

import (
	"encoding/binary"
	"io/fs"
	"runtime"
	"unsafe"

	"github.com/nickheyer/icebath/internal/dl"
	"golang.org/x/sys/unix"
)

// Space of the volume holding path, with the space the volume alone uses where getattrlist reports it
func DiskUsage(path string) (Disk, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Disk{}, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	block := uint64(st.Bsize)
	return Disk{Total: st.Blocks * block, Used: (st.Blocks - st.Bfree) * block, Free: st.Bavail * block,
		VolumeUsed: volumeSpaceUsed(path)}, nil
}

// Volume space used attribute getattrlist reports for path, nil when the call fails or omits the attribute
func volumeSpaceUsed(path string) *uint64 {
	getattrlist, err := dl.Self().Symbol("getattrlist")
	if err != nil {
		return nil
	}
	name, err := unix.BytePtrFromString(path)
	if err != nil {
		return nil
	}
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Volattr: unix.ATTR_VOL_INFO | unix.ATTR_VOL_SPACEUSED}
	var buf [12]byte
	ret, _ := dl.Call(getattrlist, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&attrs)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	runtime.KeepAlive(name)
	if int32(ret) != 0 || binary.LittleEndian.Uint32(buf[0:]) < uint32(len(buf)) {
		return nil
	}
	used := binary.LittleEndian.Uint64(buf[4:])
	return &used
}
