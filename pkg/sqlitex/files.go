package sqlitex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Kinds of file a database is moved aside as: one that starts some other way, one that holds only zero bytes, one of another layout
const (
	NotADatabase = "notadb"
	Zeroed       = "zeroed"
	OtherVersion = "other-version"
)

// Journals that live beside a database file and belong to it
var sidecars = []string{"-wal", "-shm", "-journal"}

// How a file meant to hold a database is damaged: Zeroed when it holds only zero bytes, or nothing while a journal of it is left;
// NotADatabase when it starts some other way; empty when it starts as a database, is empty with no journal, as SQLite starts a new one
// in, or is not there. Its size in bytes
func Damaged(path string) (string, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	header := make([]byte, 16)
	n, _ := io.ReadFull(f, header)
	switch {
	case n == 16 && string(header) == "SQLite format 3\x00":
		return "", info.Size(), nil
	case info.Size() == 0 && !journaled(path):
		return "", 0, nil
	case strings.Trim(string(header[:n]), "\x00") == "" && zeros(f):
		return Zeroed, info.Size(), nil
	}
	return NotADatabase, info.Size(), nil
}

// Whether nothing but zero bytes is left to read, false when the rest can't be read
func zeros(r io.Reader) bool {
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return false
			}
		}
		if err != nil {
			return err == io.EOF
		}
	}
}

// Whether a journal of the database is left beside it
func journaled(path string) bool {
	for _, sidecar := range []string{"-wal", "-journal"} {
		if info, err := os.Stat(path + sidecar); err == nil && info.Size() > 0 {
			return true
		}
	}
	return false
}

// Moves the database file and its journals to a name beside it that says what it is and when: path.kind-YYYYMMDD-HHMMSS-pid.bak, numbered
// when that is taken. The new path; the file stays where it was when it or a journal can't be moved
func Aside(path, kind string) (string, error) {
	base := fmt.Sprintf("%s.%s-%s-%d", path, kind, time.Now().Format("20060102-150405"), os.Getpid())
	backup := base + ".bak"
	for i := 1; fsx.Lexists(backup); i++ {
		backup = fmt.Sprintf("%s-%d.bak", base, i)
	}
	if err := os.Rename(path, backup); err != nil {
		return "", err
	}
	moved := []string{""}
	for _, sidecar := range sidecars {
		err := os.Rename(path+sidecar, backup+sidecar)
		if err == nil {
			moved = append(moved, sidecar)
			continue
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		// A journal left beside a new database would be played into it, so the whole set goes back
		for _, back := range moved {
			os.Rename(backup+back, path+back)
		}
		return "", err
	}
	return backup, nil
}

// Runs the function holding the lock file, waiting for it until the timeout; told whether it got the lock
func Locked(lock string, timeout time.Duration, fn func(locked bool) error) error {
	release, err := fsx.Lock(context.Background(), lock, timeout)
	if err != nil {
		return fn(false)
	}
	defer release()
	return fn(true)
}

// Runs the function holding the lock of the database's first open, path.quarantine.lock, under which the file is moved aside or its shape
// changed one process at a time
func FirstOpen(path string, timeout time.Duration, fn func(locked bool) error) error {
	return Locked(path+".quarantine.lock", timeout, fn)
}
