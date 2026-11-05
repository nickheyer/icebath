package state

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows proves holders through Restart Manager only
func foreignStateDBHolders(dbPath string) ([]Holder, error) {
	return restartManagerScan(dbPath, loadRestartManager), nil
}

// Restart Manager entry points resolved from rstrtmgr.dll
type rstrtmgr struct {
	start, register, getList, end *windows.LazyProc
}

// Loads rstrtmgr.dll on every scan and resolves its four entry points
func loadRestartManager() (rmAPI, error) {
	dll := windows.NewLazySystemDLL("rstrtmgr.dll")
	if err := dll.Load(); err != nil {
		return nil, err
	}
	api := &rstrtmgr{
		start:    dll.NewProc("RmStartSession"),
		register: dll.NewProc("RmRegisterResources"),
		getList:  dll.NewProc("RmGetList"),
		end:      dll.NewProc("RmEndSession"),
	}
	for _, p := range []*windows.LazyProc{api.start, api.register, api.getList, api.end} {
		if err := p.Find(); err != nil {
			return nil, err
		}
	}
	return api, nil
}

func (a *rstrtmgr) StartSession(session *uint32, flags uint32, key []uint16) uint32 {
	r, _, _ := a.start.Call(uintptr(unsafe.Pointer(session)), uintptr(flags), uintptr(unsafe.Pointer(&key[0])))
	return uint32(r)
}

func (a *rstrtmgr) RegisterResources(session uint32, filenames []string) uint32 {
	names := make([]*uint16, len(filenames))
	for i, name := range filenames {
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return uint32(windows.ERROR_INVALID_PARAMETER)
		}
		names[i] = p
	}
	r, _, _ := a.register.Call(uintptr(session), uintptr(len(names)), uintptr(unsafe.Pointer(&names[0])), 0, 0, 0, 0)
	return uint32(r)
}

func (a *rstrtmgr) GetList(session uint32, needed, count *uint32, apps []rmProcessInfo, reasons *uint32) uint32 {
	var buf uintptr
	if len(apps) > 0 {
		buf = uintptr(unsafe.Pointer(&apps[0]))
	}
	r, _, _ := a.getList.Call(uintptr(session), uintptr(unsafe.Pointer(needed)), uintptr(unsafe.Pointer(count)), buf, uintptr(unsafe.Pointer(reasons)))
	return uint32(r)
}

func (a *rstrtmgr) EndSession(session uint32) uint32 {
	r, _, _ := a.end.Call(uintptr(session))
	return uint32(r)
}
