//go:build windows

package tuiserver

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/pysignal"
)

// Signal number Windows gives a console Ctrl Break, the SIGBREAK of the C runtime
const sigBreak = syscall.Signal(21)

// Names the termination signals of the entry are logged under
var entrySignalNames = map[os.Signal]string{syscall.SIGTERM: "SIGTERM", syscall.SIGINT: "SIGINT", sigBreak: "SIGBREAK"}

var procSetConsoleCtrlHandler = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")

// Routes SIGTERM and Ctrl Break to the logged exit and swallows Ctrl C ahead of the runtime handler
func installEntrySignals(e *stdioEntry) {
	pysignal.Signal(syscall.SIGTERM, pysignal.Func(e.logSignal))
	pysignal.Signal(os.Interrupt, pysignal.Ignore)
	if procSetConsoleCtrlHandler.Find() != nil {
		return
	}
	handler := windows.NewCallback(func(ctrlType uint32) uintptr {
		switch ctrlType {
		case windows.CTRL_C_EVENT:
			return 1
		case windows.CTRL_BREAK_EVENT:
			go e.logSignal(sigBreak)
			return 1
		}
		return 0
	})
	_, _, _ = procSetConsoleCtrlHandler.Call(handler, 1)
}

// Moves RPC reads to a private uninheritable handle and gives the process a NUL standard input
func closeRPCStdinOnExec() {
	file, isFile := entryStdin.(*os.File)
	if !isFile {
		entryLogger().Debug("could not give the TUI gateway a private RPC stdin", "exc_info", "stdin has no handle")
		return
	}
	proc := windows.CurrentProcess()
	var private windows.Handle
	if err := windows.DuplicateHandle(proc, windows.Handle(file.Fd()), proc, &private, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		entryLogger().Debug("could not give the TUI gateway a private RPC stdin", "exc_info", err.Error())
		return
	}
	name, _ := windows.UTF16PtrFromString("NUL")
	null, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		windows.CloseHandle(private)
		entryLogger().Debug("could not give the TUI gateway a private RPC stdin", "exc_info", err.Error())
		return
	}
	_ = file.Close()
	_ = windows.SetStdHandle(windows.STD_INPUT_HANDLE, null)
	os.Stdin = os.NewFile(uintptr(null), "NUL")
	entryStdin = os.NewFile(uintptr(private), file.Name())
}
