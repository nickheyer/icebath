//go:build windows

package vsc

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Opens the named pipe for overlapped reading and writing, waiting while every instance of it is busy
func dial(ctx context.Context, path string) (io.ReadWriteCloser, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	for {
		h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
		if err == nil {
			return os.NewFile(uintptr(h), path), nil
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
