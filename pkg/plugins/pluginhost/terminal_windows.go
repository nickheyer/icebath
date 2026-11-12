//go:build windows

package pluginhost

import (
	"strconv"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
	"golang.org/x/sys/windows"
)

var (
	terminalFDsMu sync.Mutex
	terminalFDs   = map[string]any{}
)

// C runtime descriptor over one inherited terminal handle, opened once and kept for later commands
func (rt *runtime) terminalFD(p py, i int, raw string) any {
	terminalFDsMu.Lock()
	defer terminalFDsMu.Unlock()
	if fd, ok := terminalFDs[raw]; ok {
		return fd
	}
	handle, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		throw(&pyhost.Exception{Type: "RuntimeError", Message: "the plugin host terminal stream " + raw + " is not a handle"})
	}
	osm := p.imp("os")
	flags := p.get(osm, "O_WRONLY")
	if i == 0 {
		flags = p.get(osm, "O_RDONLY")
	}
	fd := p.val(p.m(p.imp("msvcrt"), "open_osfhandle", int64(handle), flags))
	terminalFDs[raw] = fd
	return fd
}

// Points the process standard handles at the terminal so subprocesses inherit it, restored by the returned func
func swapStdHandles(parts []string) func() {
	ids := []uint32{windows.STD_INPUT_HANDLE, windows.STD_OUTPUT_HANDLE, windows.STD_ERROR_HANDLE}
	previous := make([]windows.Handle, len(ids))
	restore := func(upTo int) {
		for i := 0; i < upTo; i++ {
			_ = windows.SetStdHandle(ids[i], previous[i])
		}
	}
	for i, id := range ids {
		h, err := strconv.ParseUint(parts[i], 10, 64)
		if err == nil {
			previous[i], err = windows.GetStdHandle(id)
		}
		if err == nil {
			err = windows.SetStdHandle(id, windows.Handle(h))
		}
		if err != nil {
			restore(i)
			throw(&pyhost.Exception{Type: "OSError", Message: "the plugin host could not point its standard handles at the terminal: " + err.Error()})
		}
	}
	return func() { restore(len(ids)) }
}
