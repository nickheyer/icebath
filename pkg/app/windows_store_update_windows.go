//go:build windows

package app

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/saltosystems/winrt-go"
	"github.com/saltosystems/winrt-go/windows/foundation"
	"github.com/saltosystems/winrt-go/windows/foundation/collections"
	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	storeContextClass         = "Windows.Services.Store.StoreContext"
	guidIStoreContextStatics  = "9c06ee5f-15c0-4e72-9330-d6191cebd19c"
	guidIStorePackageUpdate   = "140fa150-3cbf-4a35-b91f-48271c31b072"
	guidIInitializeWithWindow = "3e68d4bd-7135-4d10-8018-9fb6d9f33fa1"
	guidIIterable             = "faa585ea-6214-4217-afda-7f46de5869b3"
	storePackageUpdateClass   = "Windows.Services.Store.StorePackageUpdate"
)

// Vtable slots from the Windows metadata, the six IInspectable slots first
const (
	slotStoreContextStaticsGetDefault        = 6
	slotStoreContextGetUpdates               = 23
	slotStoreContextRequestDownload          = 24
	slotStoreContextRequestDownloadInstall   = 25
	slotStorePackageUpdatePackage            = 6
	slotPackageID                            = 6
	slotPackageIDFullName                    = 12
	slotStorePackageUpdateResultOverallState = 6
	slotAsyncOperationWithProgressGetResults = 10
	slotInitializeWithWindowInitialize       = 3
	slotUnknownRelease                       = 2
)

const (
	storeCOINITApartmentThreaded = 0x2
	storeQSAllInput              = 0x04FF
	storeWaitFailed              = 0xFFFFFFFF
	storePMRemove                = 0x0001
	storeWMQuit                  = 0x0012
)

var (
	storeOle32                         = windows.NewLazySystemDLL("ole32.dll")
	storeProcCoInitializeEx            = storeOle32.NewProc("CoInitializeEx")
	storeProcCoUninitialize            = storeOle32.NewProc("CoUninitialize")
	storeCombase                       = windows.NewLazySystemDLL("combase.dll")
	storeProcRoGetActivationFactory    = storeCombase.NewProc("RoGetActivationFactory")
	storeUser32                        = windows.NewLazySystemDLL("user32.dll")
	storeProcMsgWaitForMultipleObjects = storeUser32.NewProc("MsgWaitForMultipleObjects")
	storeProcPeekMessageW              = storeUser32.NewProc("PeekMessageW")
	storeProcTranslateMessage          = storeUser32.NewProc("TranslateMessage")
	storeProcDispatchMessageW          = storeUser32.NewProc("DispatchMessageW")
)

// OSError the projection raises for a failing HRESULT, its signed code and trimmed system message
type storeOSError int32

func (e storeOSError) Error() string {
	buf := make([]uint16, 2048)
	n, err := windows.FormatMessage(windows.FORMAT_MESSAGE_FROM_SYSTEM|windows.FORMAT_MESSAGE_IGNORE_INSERTS, 0, uint32(e), 0x400, buf, nil)
	if err != nil || n == 0 {
		return fmt.Sprintf("[WinError %d] Windows Error 0x%x", int32(e), uint32(e))
	}
	msg := strings.TrimRightFunc(windows.UTF16ToString(buf[:n]), func(r rune) bool { return r <= ' ' || r == '.' })
	return fmt.Sprintf("[WinError %d] %s", int32(e), msg)
}

// Error of a failing HRESULT, nil for a success code
func storeHR(hr uintptr) error {
	if int32(hr) < 0 {
		return storeOSError(int32(hr))
	}
	return nil
}

// Error of a winrt-go or go-ole call as the OSError the projection raises
func storeOleError(err error) error {
	var oe *ole.OleError
	if errors.As(err, &oe) {
		return storeOSError(int32(oe.Code()))
	}
	return err
}

// pywintypes error text of a failed Win32 call, its code, function name and message without the trailing line break
func storeAPIError(fn string, errno error) error {
	code := uint32(0)
	var en syscall.Errno
	if errors.As(errno, &en) {
		code = uint32(en)
	}
	msg := "No error message is available"
	buf := make([]uint16, 2048)
	if n, err := windows.FormatMessage(windows.FORMAT_MESSAGE_FROM_SYSTEM|windows.FORMAT_MESSAGE_IGNORE_INSERTS, 0, code, 0, buf, nil); err == nil && n > 0 {
		msg = windows.UTF16ToString(buf[:n])
	}
	if end := len(msg) - 1; end > 1 && (msg[end-1] == '\n' || msg[end-1] == '\r') {
		msg = msg[:end-1]
	} else if end > 0 && (msg[end] == '\n' || msg[end] == '\r') {
		msg = msg[:end]
	}
	return fmt.Errorf("(%d, %s, %s)", int32(code), pyrepr.StrRepr(fn), pyrepr.StrRepr(msg))
}

// Address of one vtable method of a COM object
func storeMethod(obj unsafe.Pointer, slot int) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	return *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
}

// COM references one Store request holds, all released before the apartment shuts down
type storeRefs struct{ held []unsafe.Pointer }

func (r *storeRefs) own(obj unsafe.Pointer) unsafe.Pointer {
	r.held = append(r.held, obj)
	return obj
}

func (r *storeRefs) releaseAll() {
	for i := len(r.held) - 1; i >= 0; i-- {
		obj := r.held[i]
		syscall.SyscallN(storeMethod(obj, slotUnknownRelease), uintptr(obj))
	}
	r.held = nil
}

// Interface iid of a COM object as a new held reference
func (r *storeRefs) query(obj unsafe.Pointer, iid string) (unsafe.Pointer, error) {
	got, err := (*ole.IUnknown)(obj).QueryInterface(ole.NewGUID(iid))
	if err != nil {
		return nil, storeOleError(err)
	}
	return r.own(unsafe.Pointer(got)), nil
}

// Joins a single threaded apartment the way the projection's init_apartment does
func storeInitApartmentNative() error {
	hr, _, _ := storeProcCoInitializeEx.Call(0, storeCOINITApartmentThreaded)
	return storeHR(hr)
}

func storeUninitApartmentNative() { storeProcCoUninitialize.Call() }

// Store request on the default StoreContext, consent UI owned by the desktop window when one is given
func storeExecuteNative(mode string, hwnd *big.Int) (*pyjson.Object, error) {
	handle := hwnd != nil && hwnd.Sign() != 0
	if mode != "check" && !handle {
		return nil, errors.New("A desktop window is required for Microsoft Store consent")
	}
	refs := &storeRefs{}
	defer refs.releaseAll()
	context, err := storeDefaultContext(refs)
	if err != nil {
		return nil, err
	}
	if handle {
		window, err := storeWindowHandle(hwnd)
		if err != nil {
			return nil, err
		}
		if err := storeInitializeWithWindow(refs, context.obj, window); err != nil {
			return nil, err
		}
	}
	return storePerform(context, mode, storeWait, storeUpdateCompleted)
}

// Window handle value of the hwnd argument, a value no pointer holds failing as the pointer conversion does
func storeWindowHandle(n *big.Int) (uintptr, error) {
	bits := uint(strconv.IntSize)
	if n.Sign() < 0 {
		if n.BitLen() < int(bits) || (n.BitLen() == int(bits) && n.TrailingZeroBits() == bits-1) {
			return uintptr(n.Int64()), nil
		}
		if bits == 32 {
			return 0, errors.New("Python int too large to convert to C long")
		}
		return 0, errors.New("int too big to convert")
	}
	if n.BitLen() <= int(bits) {
		return uintptr(n.Uint64()), nil
	}
	if bits == 32 {
		return 0, errors.New("Python int too large to convert to C unsigned long")
	}
	return 0, errors.New("int too big to convert")
}

// Owns the Store consent and progress UI with the desktop window
func storeInitializeWithWindow(refs *storeRefs, obj unsafe.Pointer, window uintptr) error {
	iw, err := refs.query(obj, guidIInitializeWithWindow)
	if err != nil {
		return err
	}
	hr, _, _ := syscall.SyscallN(storeMethod(iw, slotInitializeWithWindowInitialize), uintptr(iw), window)
	return storeHR(hr)
}

// StoreContext of the current user from the class statics
func storeDefaultContext(refs *storeRefs) (*storeWinContext, error) {
	class, err := ole.NewHString(storeContextClass)
	if err != nil {
		return nil, storeOleError(err)
	}
	defer ole.DeleteHString(class)
	var factory unsafe.Pointer
	hr, _, _ := storeProcRoGetActivationFactory.Call(uintptr(class), uintptr(unsafe.Pointer(ole.NewGUID(guidIStoreContextStatics))), uintptr(unsafe.Pointer(&factory)))
	if err := storeHR(hr); err != nil {
		return nil, err
	}
	refs.own(factory)
	var context unsafe.Pointer
	hr, _, _ = syscall.SyscallN(storeMethod(factory, slotStoreContextStaticsGetDefault), uintptr(factory), uintptr(unsafe.Pointer(&context)))
	if err := storeHR(hr); err != nil {
		return nil, err
	}
	return &storeWinContext{obj: refs.own(context), refs: refs}, nil
}

// Windows Runtime StoreContext the update flow drives
type storeWinContext struct {
	obj  unsafe.Pointer
	refs *storeRefs
}

func (c *storeWinContext) GetAppAndOptionalStorePackageUpdatesAsync() (storeUpdatesOperation, error) {
	var op unsafe.Pointer
	hr, _, _ := syscall.SyscallN(storeMethod(c.obj, slotStoreContextGetUpdates), uintptr(c.obj), uintptr(unsafe.Pointer(&op)))
	if err := storeHR(hr); err != nil {
		return nil, err
	}
	info, err := c.refs.query(c.refs.own(op), foundation.GUIDIAsyncInfo)
	if err != nil {
		return nil, err
	}
	return &storeWinUpdatesOperation{storeWinOperation{info: info}, op, c.refs}, nil
}

func (c *storeWinContext) RequestDownloadStorePackageUpdatesAsync(updates storeUpdateList) (storeOutcomeOperation, error) {
	return c.request(slotStoreContextRequestDownload, updates)
}

func (c *storeWinContext) RequestDownloadAndInstallStorePackageUpdatesAsync(updates storeUpdateList) (storeOutcomeOperation, error) {
	return c.request(slotStoreContextRequestDownloadInstall, updates)
}

// Starts a download or install request on the update sequence the query returned
func (c *storeWinContext) request(slot int, updates storeUpdateList) (storeOutcomeOperation, error) {
	list, ok := updates.(*storeWinUpdates)
	if !ok {
		return nil, fmt.Errorf("%T is not an update sequence of this StoreContext", updates)
	}
	iid := winrt.ParameterizedInstanceGUID(guidIIterable, "rc("+storePackageUpdateClass+";{"+guidIStorePackageUpdate+"})")
	iterable, err := c.refs.query(unsafe.Pointer(list.view), iid)
	if err != nil {
		return nil, err
	}
	var op unsafe.Pointer
	hr, _, _ := syscall.SyscallN(storeMethod(c.obj, slot), uintptr(c.obj), uintptr(iterable), uintptr(unsafe.Pointer(&op)))
	if err := storeHR(hr); err != nil {
		return nil, err
	}
	info, err := c.refs.query(c.refs.own(op), foundation.GUIDIAsyncInfo)
	if err != nil {
		return nil, err
	}
	return &storeWinOutcomeOperation{storeWinOperation{info: info}, op, c.refs}, nil
}

// Status and cancellation of a running Store operation through its IAsyncInfo
type storeWinOperation struct{ info unsafe.Pointer }

func (o storeWinOperation) Status() (storeAsyncStatus, error) {
	status, err := (*foundation.IAsyncInfo)(o.info).GetStatus()
	if err != nil {
		return 0, storeOleError(err)
	}
	return storeAsyncStatus(status), nil
}

func (o storeWinOperation) Cancel() error {
	return storeOleError((*foundation.IAsyncInfo)(o.info).Cancel())
}

// Store update query whose results are the vector view of package updates
type storeWinUpdatesOperation struct {
	storeWinOperation
	op   unsafe.Pointer
	refs *storeRefs
}

func (o *storeWinUpdatesOperation) GetResults() (storeUpdateList, error) {
	view, err := (*foundation.IAsyncOperation)(o.op).GetResults()
	if err != nil {
		return nil, storeOleError(err)
	}
	return &storeWinUpdates{view: (*collections.IVectorView)(o.refs.own(view)), refs: o.refs}, nil
}

// Download or install request whose results are the StorePackageUpdateResult
type storeWinOutcomeOperation struct {
	storeWinOperation
	op   unsafe.Pointer
	refs *storeRefs
}

func (o *storeWinOutcomeOperation) GetResults() (storeUpdateOutcome, error) {
	var result unsafe.Pointer
	hr, _, _ := syscall.SyscallN(storeMethod(o.op, slotAsyncOperationWithProgressGetResults), uintptr(o.op), uintptr(unsafe.Pointer(&result)))
	if err := storeHR(hr); err != nil {
		return nil, err
	}
	return storeWinOutcome{obj: o.refs.own(result)}, nil
}

// Vector view of the StorePackageUpdate objects a query found
type storeWinUpdates struct {
	view *collections.IVectorView
	refs *storeRefs
}

func (u *storeWinUpdates) Len() (int, error) {
	size, err := u.view.GetSize()
	if err != nil {
		return 0, storeOleError(err)
	}
	return int(size), nil
}

// Full name of the package the update at index i installs
func (u *storeWinUpdates) FullName(i int) (string, error) {
	update, err := u.view.GetAt(uint32(i))
	if err != nil {
		return "", storeOleError(err)
	}
	u.refs.own(update)
	var pkg, id unsafe.Pointer
	hr, _, _ := syscall.SyscallN(storeMethod(update, slotStorePackageUpdatePackage), uintptr(update), uintptr(unsafe.Pointer(&pkg)))
	if err := storeHR(hr); err != nil {
		return "", err
	}
	u.refs.own(pkg)
	hr, _, _ = syscall.SyscallN(storeMethod(pkg, slotPackageID), uintptr(pkg), uintptr(unsafe.Pointer(&id)))
	if err := storeHR(hr); err != nil {
		return "", err
	}
	u.refs.own(id)
	var name ole.HString
	hr, _, _ = syscall.SyscallN(storeMethod(id, slotPackageIDFullName), uintptr(id), uintptr(unsafe.Pointer(&name)))
	if err := storeHR(hr); err != nil {
		return "", err
	}
	defer ole.DeleteHString(name)
	return name.String(), nil
}

// StorePackageUpdateResult of a finished request
type storeWinOutcome struct{ obj unsafe.Pointer }

func (o storeWinOutcome) OverallState() (storeUpdateState, error) {
	var state int32
	hr, _, _ := syscall.SyscallN(storeMethod(o.obj, slotStorePackageUpdateResultOverallState), uintptr(o.obj), uintptr(unsafe.Pointer(&state)))
	if err := storeHR(hr); err != nil {
		return 0, err
	}
	return storeUpdateStateOf(state)
}

// Polls the operation while pumping this thread's messages so the Store consent and progress UI stays responsive
func storeWait(op storeOperation) error {
	deadline := time.Now().Add(storeRequestTimeoutS * time.Second)
	for {
		status, err := op.Status()
		if err != nil {
			return err
		}
		if status != storeAsyncStarted {
			return nil
		}
		if !time.Now().Before(deadline) {
			if err := op.Cancel(); err != nil {
				return err
			}
			return errors.New("Microsoft Store request timed out")
		}
		r, _, lastErr := storeProcMsgWaitForMultipleObjects.Call(0, 0, 0, 50, storeQSAllInput)
		if uint32(r) == storeWaitFailed {
			return storeAPIError("MsgWaitForMultipleObjects", lastErr)
		}
		if storePumpWaitingMessages() {
			if err := op.Cancel(); err != nil {
				return err
			}
			return errors.New("Microsoft Store request was interrupted")
		}
	}
}

// Window message of the thread queue
type storeMSG struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       [2]int32
	lPrivate uint32
}

// Dispatches every waiting message of this thread, true once a quit message arrives
func storePumpWaitingMessages() bool {
	var msg storeMSG
	for {
		r, _, _ := storeProcPeekMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, storePMRemove)
		if r == 0 {
			return false
		}
		if msg.message == storeWMQuit {
			return true
		}
		storeProcTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		storeProcDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}
