package ziputil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
)

// Damage in a member's stored data: a bad checksum, a broken or cut short compressed stream, a bad local header, or data that runs into another member
var ErrCorrupt = errors.New("corrupt zip member")

// Member protected by encryption, which this package does not decrypt
var ErrEncrypted = errors.New("zip member is encrypted")

// Archive holding more members or declaring more uncompressed bytes than an extraction limit allows
var ErrLimit = errors.New("zip archive exceeds an extraction limit")

var (
	errOverlap      = errors.New("zip member data overlaps another member")
	errLZMAHeader   = errors.New("lzma: zip member header gives properties of the wrong size")
	errNotRegular   = errors.New("not a regular file or directory")
	errClosed       = fmt.Errorf("zip archive: %w", fs.ErrClosed)
	errMemberClosed = fmt.Errorf("zip member: %w", fs.ErrClosed)
	errDirData      = fmt.Errorf("zip directory member takes no data: %w", fs.ErrInvalid)
	errNoName       = fmt.Errorf("zip member name leaves no file name: %w", fs.ErrInvalid)
)

// Damaged member error reading as its cause and matching ErrCorrupt
type corruptError struct{ err error }

// Text of the cause
func (e *corruptError) Error() string { return e.err.Error() }

// The cause
func (e *corruptError) Unwrap() error { return e.err }

// Whether target is ErrCorrupt
func (e *corruptError) Is(target error) bool { return target == ErrCorrupt }

// err marked as member damage; nil, io.EOF, use after close, operating system failures and errors already marked are left as they are
func corrupt(err error) error {
	if err == nil || err == io.EOF || errors.Is(err, ErrCorrupt) || errors.Is(err, fs.ErrClosed) || errorutil.IsSystem(err) {
		return err
	}
	return &corruptError{err}
}
