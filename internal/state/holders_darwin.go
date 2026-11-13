package state

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/internal/proc"
)

// proc_info call numbers, flavors and record layout of the vnode path record the kernel returns per descriptor
const (
	procInfoPIDInfo      = 2
	procInfoPIDFDInfo    = 3
	procPIDListFDs       = 1
	procPIDFDVnodePath   = 2
	procFDInfoSize       = 8
	proxFDTypeVnode      = 1
	vnodeFDRecordSize    = 1200
	vnodeFDDevOffset     = 24
	vnodeFDInoOffset     = 32
	vnodeFDPathOffset    = 176
	darwinFDScanDeadline = 5 * time.Second
)

// One vnode descriptor of some process with the last path and identity of the file it references
type vnodeFD struct {
	pid  int
	path string
	id   fileIdentity
}

// Raw proc_info call returning the byte count, zero when the kernel refuses
func procInfo(call, pid, flavor int, arg uint64, buf []byte) int {
	var p unsafe.Pointer
	if len(buf) > 0 {
		p = unsafe.Pointer(&buf[0])
	}
	r, _, e := unix.Syscall6(unix.SYS_PROC_INFO, uintptr(call), uintptr(pid), uintptr(flavor), uintptr(arg), uintptr(p), uintptr(len(buf)))
	if e != 0 || int32(r) <= 0 {
		return 0
	}
	return int(int32(r))
}

// Descriptor records of pid, nil when the process is gone or not ours to inspect
func listFDRecords(pid int) []byte {
	for size := 4096; ; size *= 2 {
		buf := make([]byte, size)
		used := procInfo(procInfoPIDInfo, pid, procPIDListFDs, 0, buf)
		if used <= 0 {
			return nil
		}
		if used < size {
			return buf[:used-used%procFDInfoSize]
		}
	}
}

// Every vnode descriptor libproc reports, an unlinked file keeping its last path and identity
func vnodeDescriptors() ([]vnodeFD, error) {
	table, err := proc.ReadTable()
	if err != nil {
		return nil, err
	}
	var out []vnodeFD
	record := make([]byte, vnodeFDRecordSize)
	for _, entry := range table {
		fds := listFDRecords(entry.PID)
		for off := 0; off+procFDInfoSize <= len(fds); off += procFDInfoSize {
			fd := int(int32(binary.LittleEndian.Uint32(fds[off:])))
			if binary.LittleEndian.Uint32(fds[off+4:]) != proxFDTypeVnode {
				continue
			}
			if procInfo(procInfoPIDFDInfo, entry.PID, procPIDFDVnodePath, uint64(fd), record) <= 0 {
				continue
			}
			path, _, _ := strings.Cut(string(record[vnodeFDPathOffset:]), "\x00")
			out = append(out, vnodeFD{
				pid:  entry.PID,
				path: strings.ToValidUTF8(path, "�"),
				id:   fileIdentity{dev: uint64(binary.LittleEndian.Uint32(record[vnodeFDDevOffset:])), ino: binary.LittleEndian.Uint64(record[vnodeFDInoOffset:])},
			})
		}
	}
	return out, nil
}

// Foreign holders from each process's open regular files, their resolved paths matched against the store family
func foreignHolders(path string) []Holder {
	db := resolvePath(path)
	watched := map[string]bool{}
	var holders []Holder
	for _, candidate := range sqliteFamily(db) {
		watched[canonicalSQLitePath(candidate)] = true
		if _, err := identityOf(candidate); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ESRCH) {
			holders = append(holders, Holder{Target: fmt.Sprintf("watched-file stat failed: %s: %v", candidate, err)})
		}
	}
	table, err := proc.ReadTable()
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not prove state.db has no foreign holders; deferring structural maintenance: %v", err))
		return append(holders, Holder{Target: fmt.Sprintf("open-file scan failed: %v", err)})
	}
	own := os.Getpid()
	for _, entry := range table {
		if entry.PID == own {
			continue
		}
		files, err := proc.OpenFiles(entry.PID)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.Path != "" && watched[canonicalSQLitePath(resolvePath(f.Path))] {
				holders = append(holders, Holder{PID: entry.PID, Target: f.Path})
			}
		}
	}
	return holders
}

// Distinct processes holding the resolved store file, matched by identity since libproc reports the path as opened
func countHolders(path string) (int, bool) {
	id, err := identityOf(resolvePath(path))
	if err != nil {
		return 0, false
	}
	fds, err := vnodeDescriptors()
	if err != nil {
		return 0, false
	}
	pids := map[int]bool{}
	for _, d := range fds {
		if d.id == id {
			pids[d.pid] = true
		}
	}
	return len(pids), true
}

// Holders of a deleted sidecar generation through libproc, abandoned after five seconds since a call can block
func deletedSidecarHolders(path string) []Holder {
	base := resolvePath(absPath(path))
	watched := map[string]string{}
	for _, p := range []string{base + walSuffix, base + shmSuffix} {
		watched[strings.ToLower(p)] = p
	}
	type result struct {
		holders []Holder
		err     error
	}
	done := make(chan result, 1)
	go func() {
		fds, err := vnodeDescriptors()
		var holders []Holder
		for _, d := range fds {
			if literal, ok := watched[strings.ToLower(d.path)]; ok && identityUnlinked(d.id, literal) {
				holders = append(holders, Holder{PID: d.pid, Target: d.path})
			}
		}
		done <- result{holders, err}
	}()
	timer := time.NewTimer(darwinFDScanDeadline)
	defer timer.Stop()
	select {
	case r := <-done:
		if r.err != nil {
			slog.Debug(fmt.Sprintf("deleted-WAL holder scan failed for %s: %v", path, r.err))
		}
		return r.holders
	case <-timer.C:
		slog.Warn(fmt.Sprintf("deleted-WAL holder scan timed out after %.1fs for %s; continuing fail-open",
			darwinFDScanDeadline.Seconds(), path))
		return nil
	}
}

// macOS reports no deleted suffix, so a WAL generation that appeared after open is adopted without a scan
func ownDeletedSidecar(string) (bool, error) { return false, errors.ErrUnsupported }
