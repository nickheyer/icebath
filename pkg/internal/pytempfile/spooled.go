package pytempfile

import (
	"errors"
	"io"
	"os"
	"runtime"
	"strconv"
)

// In memory file that rolls over to an anonymous temporary file once its position passes the max size
type SpooledTemporaryFile struct {
	maxSize int64
	buf     []byte
	pos     int64
	file    *os.File
	rolled  bool
	closed  bool
}

// Spooled file rolling to disk past maxSize bytes, zero never rolling on its own
func NewSpooledTemporaryFile(maxSize int64) *SpooledTemporaryFile {
	return &SpooledTemporaryFile{maxSize: maxSize}
}

var errClosed = errors.New("I/O operation on closed file.")

// Anonymous temporary file, unlinked at once on POSIX and deleted at close on Windows
func TemporaryFile() (*os.File, error) {
	f, err := os.CreateTemp("", "tmp")
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if err := os.Remove(f.Name()); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

// Maximum in memory size given at construction
func (f *SpooledTemporaryFile) MaxSize() int64 { return f.maxSize }

// Whether the file has been closed
func (f *SpooledTemporaryFile) Closed() bool { return f.closed }

// Whether the contents have moved to a disk file
func (f *SpooledTemporaryFile) Rolled() bool { return f.rolled }

// Moves the contents to a temporary file keeping the position
func (f *SpooledTemporaryFile) Rollover() error {
	if f.rolled {
		return nil
	}
	nf, err := TemporaryFile()
	if err != nil {
		return err
	}
	if _, err := nf.Write(f.buf); err != nil {
		nf.Close()
		return err
	}
	if _, err := nf.Seek(f.pos, io.SeekStart); err != nil {
		nf.Close()
		return err
	}
	f.file, f.buf, f.rolled = nf, nil, true
	return nil
}

// Writes at the position, rolling over once the position passes the max size
func (f *SpooledTemporaryFile) Write(p []byte) (int, error) {
	if f.closed {
		return 0, errClosed
	}
	if f.rolled {
		return f.file.Write(p)
	}
	end := f.pos + int64(len(p))
	if end > int64(len(f.buf)) {
		grown := make([]byte, end)
		copy(grown, f.buf)
		f.buf = grown
	}
	copy(f.buf[f.pos:end], p)
	f.pos = end
	if f.maxSize > 0 && f.pos > f.maxSize {
		if err := f.Rollover(); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// Reads from the position
func (f *SpooledTemporaryFile) Read(p []byte) (int, error) {
	if f.closed {
		return 0, errClosed
	}
	if f.rolled {
		return f.file.Read(p)
	}
	if f.pos >= int64(len(f.buf)) {
		return 0, io.EOF
	}
	n := copy(p, f.buf[f.pos:])
	f.pos += int64(n)
	return n, nil
}

// Moves the position, returning the new absolute position
func (f *SpooledTemporaryFile) Seek(offset int64, whence int) (int64, error) {
	if f.closed {
		return 0, errClosed
	}
	if f.rolled {
		return f.file.Seek(offset, whence)
	}
	var next int64
	switch whence {
	case io.SeekStart:
		if offset < 0 {
			return 0, errors.New("negative seek value " + strconv.FormatInt(offset, 10))
		}
		next = offset
	case io.SeekCurrent:
		next = max(f.pos+offset, 0)
	case io.SeekEnd:
		next = max(int64(len(f.buf))+offset, 0)
	default:
		return 0, errors.New("invalid whence (" + strconv.Itoa(whence) + ", should be 0, 1 or 2)")
	}
	f.pos = next
	return next, nil
}

// Current position
func (f *SpooledTemporaryFile) Tell() (int64, error) { return f.Seek(0, io.SeekCurrent) }

// Closes the memory buffer or the disk file, a second close doing nothing
func (f *SpooledTemporaryFile) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	if f.rolled {
		name := f.file.Name()
		err := f.file.Close()
		if runtime.GOOS == "windows" {
			if rerr := os.Remove(name); err == nil && rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				err = rerr
			}
		}
		return err
	}
	f.buf = nil
	return nil
}
