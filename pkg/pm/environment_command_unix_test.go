//go:build !windows

package pm

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// Descriptor 2 as a file that lives for the whole run, since go test -json points os.Stderr at stdout
var ecStderrFD = os.NewFile(2, "/dev/stderr")

// Captures what descriptors 1 and 2 receive while run executes, the counterpart of pytest capfd
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
	savedOut, err := unix.Dup(1)
	if err != nil {
		t.Fatal(err)
	}
	savedErr, err := unix.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Dup2(int(outFile.Fd()), 1); err != nil {
		t.Fatal(err)
	}
	if err := unix.Dup2(int(errFile.Fd()), 2); err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = ecStderrFD
	func() {
		defer func() {
			os.Stderr = stderr
			_ = unix.Dup2(savedOut, 1)
			_ = unix.Dup2(savedErr, 2)
			_ = unix.Close(savedOut)
			_ = unix.Close(savedErr)
		}()
		run()
	}()
	outFile.Close()
	errFile.Close()
	return peRead(t, outFile.Name()), peRead(t, errFile.Name())
}

// Writes straight to descriptor 1 the way a child process inheriting it prints
func ecWriteStdoutFD(text string) { _, _ = unix.Write(1, []byte(text)) }
