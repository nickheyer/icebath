//go:build windows

package pm

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// Captures what the standard output and error handles receive while run executes, the counterpart of pytest capfd
func ecCaptureFDs(t *testing.T, run func()) (string, string) {
	t.Helper()
	dir := t.TempDir()
	outFile, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	savedOutHandle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		t.Fatal(err)
	}
	savedErrHandle, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil {
		t.Fatal(err)
	}
	savedOut, savedErr := os.Stdout, os.Stderr
	if err := windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(outFile.Fd())); err != nil {
		t.Fatal(err)
	}
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(errFile.Fd())); err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outFile, errFile
	func() {
		defer func() {
			_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, savedOutHandle)
			_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, savedErrHandle)
			os.Stdout, os.Stderr = savedOut, savedErr
		}()
		run()
	}()
	outFile.Close()
	errFile.Close()
	return peRead(t, outFile.Name()), peRead(t, errFile.Name())
}

// Writes straight to the standard output handle the way a child process inheriting it prints
func ecWriteStdoutFD(text string) {
	handle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return
	}
	var written uint32
	_ = windows.WriteFile(handle, []byte(text), &written, nil)
}
