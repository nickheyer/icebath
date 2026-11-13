//go:build windows

package selfupdate

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/saltosystems/winrt-go/windows/foundation"
	"golang.org/x/sys/windows"
)

const (
	appInstallerPackageClass = "Windows.ApplicationModel.Package"
	guidIPackageStatics      = "4e534bdf-2960-4878-97a4-9624deb72f2d"
	guidIPackage6            = "8b1ad942-12d7-4754-ae4e-638cbc0e3a2e"
)

// Vtable slots from the Windows metadata, the six IInspectable slots first
const (
	slotPackageStaticsCurrent                = 6
	slotPackage6GetAppInstallerInfo          = 6
	slotPackage6CheckUpdateAvailability      = 7
	slotAppInstallerInfoURI                  = 6
	slotURIAbsoluteURI                       = 6
	slotUpdateAvailabilityResultAvailability = 6
	slotAppInstallerUnknownRelease           = 2
)

const (
	roInitMultithreaded      = 1
	asyncStatusStarted       = 0
	asyncStatusCanceled      = 2
	hresultOperationCanceled = -2147023673
)

var (
	aiCombase            = windows.NewLazySystemDLL("combase.dll")
	aiProcRoInitialize   = aiCombase.NewProc("RoInitialize")
	aiProcRoUninitialize = aiCombase.NewProc("RoUninitialize")
)

// OSError the projection raises for a failing HRESULT, its signed code and the system message without trailing dots
type winRTError int32

func (e winRTError) WinError() int32 { return int32(e) }

func (e winRTError) Error() string {
	buf := make([]uint16, 2048)
	n, err := windows.FormatMessage(windows.FORMAT_MESSAGE_FROM_SYSTEM|windows.FORMAT_MESSAGE_IGNORE_INSERTS, 0, uint32(e), 0x400, buf, nil)
	if err != nil || n == 0 {
		return fmt.Sprintf("[WinError %d] Windows Error 0x%x", int32(e), uint32(e))
	}
	msg := strings.TrimRightFunc(windows.UTF16ToString(buf[:n]), func(r rune) bool { return r <= ' ' || r == '.' })
	return fmt.Sprintf("[WinError %d] %s", int32(e), msg)
}

// Error of a failing HRESULT, nil for a success code
func winRTHR(hr uintptr) error {
	if int32(hr) < 0 {
		return winRTError(int32(hr))
	}
	return nil
}

// Error of a go-ole call as the OSError the projection raises
func winRTOleError(err error) error {
	var oe *ole.OleError
	if errors.As(err, &oe) {
		return winRTError(int32(oe.Code()))
	}
	return err
}

// Address of one vtable method of a COM object
func winRTMethod(obj unsafe.Pointer, slot int) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	return *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
}

// Calls a getter slot of a COM object that writes one out value
func winRTGet(obj unsafe.Pointer, slot int, out unsafe.Pointer) error {
	hr, _, _ := syscall.SyscallN(winRTMethod(obj, slot), uintptr(obj), uintptr(out))
	return winRTHR(hr)
}

// Multithreaded apartment and the COM references the check holds, all released on close
type appInstallerWinProjection struct{ held []unsafe.Pointer }

// Joins the multithreaded apartment the projection runs in
func loadAppInstallerProjection() (appInstallerProjection, error) {
	hr, _, _ := aiProcRoInitialize.Call(roInitMultithreaded)
	if err := winRTHR(hr); err != nil {
		return nil, err
	}
	return &appInstallerWinProjection{}, nil
}

func (p *appInstallerWinProjection) own(obj unsafe.Pointer) unsafe.Pointer {
	p.held = append(p.held, obj)
	return obj
}

// Releases every held reference before the apartment shuts down
func (p *appInstallerWinProjection) Close() {
	for i := len(p.held) - 1; i >= 0; i-- {
		obj := p.held[i]
		syscall.SyscallN(winRTMethod(obj, slotAppInstallerUnknownRelease), uintptr(obj))
	}
	p.held = nil
	aiProcRoUninitialize.Call()
}

// Interface iid of a COM object as a new held reference
func (p *appInstallerWinProjection) query(obj unsafe.Pointer, iid string) (unsafe.Pointer, error) {
	got, err := (*ole.IUnknown)(obj).QueryInterface(ole.NewGUID(iid))
	if err != nil {
		return nil, winRTOleError(err)
	}
	return p.own(unsafe.Pointer(got)), nil
}

// Package of this process from the Package statics, a non packaged run failing with the package identity HRESULT
func (p *appInstallerWinProjection) Current() (appInstallerPackage, error) {
	factory, err := ole.RoGetActivationFactory(appInstallerPackageClass, ole.NewGUID(guidIPackageStatics))
	if err != nil {
		return nil, winRTOleError(err)
	}
	statics := p.own(unsafe.Pointer(factory))
	var pkg unsafe.Pointer
	if err := winRTGet(statics, slotPackageStaticsCurrent, unsafe.Pointer(&pkg)); err != nil {
		return nil, err
	}
	if pkg != nil {
		p.own(pkg)
	}
	return &appInstallerWinPackage{obj: pkg, p: p}, nil
}

// Running Package the check queries through its IPackage6 interface
type appInstallerWinPackage struct {
	obj unsafe.Pointer
	p   *appInstallerWinProjection
}

// IPackage6 of the package, a null package failing the attribute lookup the projection fails on None
func (k *appInstallerWinPackage) package6(attribute string) (unsafe.Pointer, error) {
	if k.obj == nil {
		return nil, fmt.Errorf("'NoneType' object has no attribute '%s'", attribute)
	}
	return k.p.query(k.obj, guidIPackage6)
}

// Absolute URI of the registered App Installer source, registered false when the package has none
func (k *appInstallerWinPackage) AppInstallerSourceURI() (string, bool, error) {
	p6, err := k.package6("get_app_installer_info")
	if err != nil {
		return "", false, err
	}
	var info unsafe.Pointer
	if err := winRTGet(p6, slotPackage6GetAppInstallerInfo, unsafe.Pointer(&info)); err != nil {
		return "", false, err
	}
	if info == nil {
		return "", false, nil
	}
	k.p.own(info)
	var uri unsafe.Pointer
	if err := winRTGet(info, slotAppInstallerInfoURI, unsafe.Pointer(&uri)); err != nil {
		return "", true, err
	}
	if uri == nil {
		return "", true, errors.New("'NoneType' object has no attribute 'absolute_uri'")
	}
	k.p.own(uri)
	var text ole.HString
	if err := winRTGet(uri, slotURIAbsoluteURI, unsafe.Pointer(&text)); err != nil {
		return "", true, err
	}
	defer ole.DeleteHString(text)
	return text.String(), true, nil
}

// Raw availability of the update check, waited on until the operation leaves the started state
func (k *appInstallerWinPackage) CheckUpdateAvailability() (int32, error) {
	p6, err := k.package6("check_update_availability_async")
	if err != nil {
		return 0, err
	}
	var op unsafe.Pointer
	if err := winRTGet(p6, slotPackage6CheckUpdateAvailability, unsafe.Pointer(&op)); err != nil {
		return 0, err
	}
	if op == nil {
		return 0, errors.New("'NoneType' object has no attribute 'get'")
	}
	k.p.own(op)
	info, err := k.p.query(op, foundation.GUIDIAsyncInfo)
	if err != nil {
		return 0, err
	}
	for {
		status, err := (*foundation.IAsyncInfo)(info).GetStatus()
		if err != nil {
			return 0, winRTOleError(err)
		}
		if status == asyncStatusCanceled {
			return 0, winRTError(hresultOperationCanceled)
		}
		if status != asyncStatusStarted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	result, err := (*foundation.IAsyncOperation)(op).GetResults()
	if err != nil {
		return 0, winRTOleError(err)
	}
	if result == nil {
		return 0, errors.New("'NoneType' object has no attribute 'availability'")
	}
	k.p.own(result)
	var availability int32
	if err := winRTGet(result, slotUpdateAvailabilityResultAvailability, unsafe.Pointer(&availability)); err != nil {
		return 0, err
	}
	return availability, nil
}
