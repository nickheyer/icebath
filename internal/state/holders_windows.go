package state

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Restart Manager return code asking for a larger process buffer
const rmErrorMoreData = 234

// Length of the Restart Manager session key buffer including its terminator
const rmSessionKeyLen = 33

// RM_UNIQUE_PROCESS, a pid and its start time
type rmUniqueProcess struct {
	pid     uint32
	started windows.Filetime
}

// RM_PROCESS_INFO, one process Restart Manager reports as using a registered file
type rmProcessInfo struct {
	process     rmUniqueProcess
	appName     [256]uint16
	serviceName [64]uint16
	appType     uint32
	appStatus   uint32
	tsSessionID uint32
	restartable int32
}

// Foreign holders as Windows Restart Manager reports them, a failed scan reported as a PID zero holder
func foreignHolders(path string) []Holder {
	holders, err := restartManagerHolders(path)
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not prove state.db has no Windows holders; deferring structural maintenance: %v", err))
		return []Holder{{Target: fmt.Sprintf("Windows Restart Manager scan failed: %v", err)}}
	}
	return holders
}

// Processes other than this one Restart Manager names as users of the store or a sidecar
func restartManagerHolders(path string) ([]Holder, error) {
	dll := windows.NewLazySystemDLL("rstrtmgr.dll")
	start, register, getList, end := dll.NewProc("RmStartSession"), dll.NewProc("RmRegisterResources"),
		dll.NewProc("RmGetList"), dll.NewProc("RmEndSession")
	for _, p := range []*windows.LazyProc{start, register, getList, end} {
		if err := p.Find(); err != nil {
			return nil, err
		}
	}
	db := absPath(path)
	var names []*uint16
	for _, p := range sqliteFamily(db) {
		if !exists(p) {
			continue
		}
		u, err := windows.UTF16PtrFromString(p)
		if err != nil {
			return nil, err
		}
		names = append(names, u)
	}
	if len(names) == 0 {
		return nil, nil
	}
	var session uint32
	key := make([]uint16, rmSessionKeyLen)
	if rc, _, _ := start.Call(uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); rc != 0 {
		return nil, fmt.Errorf("RmStartSession failed: %w", windows.Errno(rc))
	}
	defer end.Call(uintptr(session))
	if rc, _, _ := register.Call(uintptr(session), uintptr(len(names)), uintptr(unsafe.Pointer(&names[0])), 0, 0, 0, 0); rc != 0 {
		return nil, fmt.Errorf("RmRegisterResources failed: %w", windows.Errno(rc))
	}
	var needed, reasons uint32
	for range 4 {
		apps := make([]rmProcessInfo, needed)
		count := needed
		buf := uintptr(0)
		if needed > 0 {
			buf = uintptr(unsafe.Pointer(&apps[0]))
		}
		rc, _, _ := getList.Call(uintptr(session), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)), buf,
			uintptr(unsafe.Pointer(&reasons)))
		if rc == 0 {
			own := uint32(os.Getpid())
			var holders []Holder
			for _, app := range apps[:count] {
				if app.process.pid != own {
					holders = append(holders, Holder{PID: int(app.process.pid), Target: db})
				}
			}
			return holders, nil
		}
		if rc != rmErrorMoreData {
			return nil, fmt.Errorf("RmGetList failed: %w", windows.Errno(rc))
		}
	}
	return nil, errors.New("Restart Manager holder set kept changing")
}

// Windows exposes no descriptor table to count holders from
func countHolders(string) (int, bool) { return 0, false }

// Windows cannot unlink a held sidecar, so no retired generation can exist
func deletedSidecarHolders(string) []Holder { return nil }

// Windows cannot unlink a held sidecar, so a WAL that appeared after open is adopted without a scan
func ownDeletedSidecar(string) (bool, error) { return false, errors.ErrUnsupported }
