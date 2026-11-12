//go:build windows

package files

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32                        = windows.NewLazySystemDLL("shell32.dll")
	ole32                          = windows.NewLazySystemDLL("ole32.dll")
	procSHFileOperationW           = shell32.NewProc("SHFileOperationW")
	procILCreateFromPathW          = shell32.NewProc("ILCreateFromPathW")
	procILFree                     = shell32.NewProc("ILFree")
	procSHOpenFolderAndSelectItems = shell32.NewProc("SHOpenFolderAndSelectItems")
	procCoInitializeEx             = ole32.NewProc("CoInitializeEx")
)

const (
	foDelete          = 0x0003
	fofSilent         = 0x0004
	fofNoConfirmation = 0x0010
	fofAllowUndo      = 0x0040
	fofNoErrorUI      = 0x0400
)

type shFileOpStruct struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

// Selects the item in an Explorer window through SHOpenFolderAndSelectItems
func ShowItemInFolder(fullPath string) {
	procCoInitializeEx.Call(0, 0x2)
	p, err := windows.UTF16PtrFromString(resolvePath(fullPath))
	if err != nil {
		return
	}
	pidl, _, _ := procILCreateFromPathW.Call(uintptr(unsafe.Pointer(p)))
	if pidl == 0 {
		return
	}
	defer procILFree.Call(pidl)
	procSHOpenFolderAndSelectItems.Call(pidl, 0, 0, 0)
}

// Opens the path in its default application through ShellExecute, the error text on failure and empty on success
func OpenPath(fullPath string) string {
	verb, _ := windows.UTF16PtrFromString("open")
	file, err := windows.UTF16PtrFromString(resolvePath(fullPath))
	if err != nil {
		return err.Error()
	}
	if err := windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		return "Failed to open path"
	}
	return ""
}

// Moves the item to the Recycle Bin with an undoable silent delete
func TrashItem(fullPath string) error {
	abs := resolvePath(fullPath)
	from, err := windows.UTF16FromString(abs)
	if err != nil {
		return err
	}
	from = append(from, 0)
	op := shFileOpStruct{wFunc: foDelete, pFrom: &from[0], fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI}
	ret, _, _ := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	if ret != 0 {
		return fmt.Errorf("Failed to move %s to the Recycle Bin (error 0x%x)", abs, ret)
	}
	if op.fAnyOperationsAborted != 0 {
		return errors.New("Failed to move item to trash")
	}
	return nil
}
