package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32GetOEMCP            = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetOEMCP")
	kernel32WideCharToMultiByte = windows.NewLazySystemDLL("kernel32.dll").NewProc("WideCharToMultiByte")
)

// Flags making the code page conversions fail rather than substitute a best fit or default character
const (
	wcNoBestFitChars  = 0x00000400
	mbErrInvalidChars = 0x00000008
)

// Scripts this OS user's gateway Scheduled Tasks start, the install probe's view of task installs
func registeredTaskLaunchers() []string {
	return taskLaunchersFrom(execSchtasks)
}

// Runs schtasks.exe under the fifteen second gateway timeout
func ExecSchtasks(args []string) (int, string, string) {
	return execSchtasks(args, 15*time.Second)
}

// Runs schtasks.exe hidden under a hard timeout, a wedge answering code 124 with a synthetic stderr
func execSchtasks(args []string, timeout time.Duration) (int, string, string) {
	schtasks, ok := shutilWhich("schtasks", nil)
	if !ok {
		return 1, "", "schtasks.exe not found on PATH"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, schtasks, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return 124, "", "schtasks timed out after " + strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64) + "s"
	}
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return 1, "", "schtasks invocation failed: " + windowsErrorText(err)
		}
		code = exitErr.ExitCode()
	}
	pages := windowsConsoleCodePages()
	out, err := decodeSchtasksOutput(stdout.Bytes(), pages, consoleCodePageDecoder)
	if err != nil {
		return 1, "", "schtasks invocation failed: " + err.Error()
	}
	errText, err := decodeSchtasksOutput(stderr.Bytes(), pages, consoleCodePageDecoder)
	if err != nil {
		return 1, "", "schtasks invocation failed: " + err.Error()
	}
	return code, out, errText
}

// Console output, OEM and ANSI code pages in that order without repeats, UTF-8 when none answers
func windowsConsoleCodePages() []int {
	var pages []int
	add := func(cp uint32) {
		if cp == 0 {
			return
		}
		for _, p := range pages {
			if p == int(cp) {
				return
			}
		}
		pages = append(pages, int(cp))
	}
	if cp, err := windows.GetConsoleOutputCP(); err == nil {
		add(cp)
	}
	if r, _, _ := kernel32GetOEMCP.Call(); r != 0 {
		add(uint32(r))
	}
	add(windows.GetACP())
	if len(pages) == 0 {
		return []int{65001}
	}
	return pages
}

// Text of captured schtasks or whoami bytes decoded through this console's code pages
func DecodeSchtasksBytes(data []byte) (string, error) {
	return decodeSchtasksOutput(data, windowsConsoleCodePages(), consoleCodePageDecoder)
}

// CPython codec for a code page, else the Windows code page codec when the OS can encode with it
func consoleCodePageDecoder(cp int) codePageDecoder {
	if dec := pyCodePageDecoder(cp); dec != nil {
		return dec
	}
	if !windowsCodePageEncodes(cp) {
		return nil
	}
	return func(data []byte) (string, bool, error) {
		if len(data) == 0 {
			return "", true, nil
		}
		flags := uint32(mbErrInvalidChars)
		if cp == 65000 {
			flags = 0
		}
		n, err := windows.MultiByteToWideChar(uint32(cp), flags, &data[0], int32(len(data)), nil, 0)
		if err == nil && n > 0 {
			buf := make([]uint16, n)
			n, err = windows.MultiByteToWideChar(uint32(cp), flags, &data[0], int32(len(data)), &buf[0], n)
			if err == nil && n > 0 {
				return string(utf16.Decode(buf[:n])), true, nil
			}
		}
		if errors.Is(err, windows.ERROR_NO_UNICODE_TRANSLATION) {
			return "", false, nil
		}
		return "", false, errors.New(windowsErrorText(err))
	}
}

// OSError text CPython gives a Windows API failure, the WinError number with the system message
func windowsErrorText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("[WinError %d] %s", uint32(errno), errno.Error())
	}
	return err.Error()
}

// Reports whether the OS encodes an x in the code page, the probe CPython uses to accept a cp name
func windowsCodePageEncodes(cp int) bool {
	x := []uint16{'x'}
	if cp == 65001 || cp == 65000 {
		r, _, _ := kernel32WideCharToMultiByte.Call(uintptr(cp), 0, uintptr(unsafe.Pointer(&x[0])), 1, 0, 0, 0, 0)
		return r != 0
	}
	var usedDefault int32
	r, _, _ := kernel32WideCharToMultiByte.Call(uintptr(cp), wcNoBestFitChars, uintptr(unsafe.Pointer(&x[0])), 1, 0, 0, 0,
		uintptr(unsafe.Pointer(&usedDefault)))
	return r != 0 && usedDefault == 0
}
