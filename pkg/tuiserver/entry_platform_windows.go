//go:build windows

package tuiserver

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// Signal number Windows gives a console Ctrl Break, the SIGBREAK of the C runtime
const sigBreak = syscall.Signal(21)

// Names the termination signals of the entry are logged under
var entrySignalNames = map[os.Signal]string{syscall.SIGTERM: "SIGTERM", syscall.SIGINT: "SIGINT", sigBreak: "SIGBREAK"}

// Moves RPC reads to a private uninheritable handle and gives the process a NUL standard input
func closeRPCStdinOnExec() {
	file, isFile := entryStdin.(*os.File)
	if !isFile {
		pkgLog().Debug("could not give the TUI gateway a private RPC stdin", "exc_info", "stdin has no handle")
		return
	}
	proc := windows.CurrentProcess()
	var private windows.Handle
	if err := windows.DuplicateHandle(proc, windows.Handle(file.Fd()), proc, &private, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		pkgLog().Debug("could not give the TUI gateway a private RPC stdin", "exc_info", err.Error())
		return
	}
	name, _ := windows.UTF16PtrFromString("NUL")
	null, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		windows.CloseHandle(private)
		pkgLog().Debug("could not give the TUI gateway a private RPC stdin", "exc_info", err.Error())
		return
	}
	_ = file.Close()
	_ = windows.SetStdHandle(windows.STD_INPUT_HANDLE, null)
	os.Stdin = os.NewFile(uintptr(null), "NUL")
	entryStdin = os.NewFile(uintptr(private), file.Name())
}
