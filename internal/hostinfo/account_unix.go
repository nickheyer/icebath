//go:build linux || darwin

package hostinfo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/nickheyer/icebath/internal/dl"
)

const (
	passwdBufferLimit   = 1 << 20
	passwdBufferAtLeast = 1024
)

// Fields of one user record of the system account database
type passwdEntry struct {
	login string
	gecos string
	home  string
}

// Home directory of a login name through every configured name service, ErrAbsent without a record
func AccountHome(login string) (string, error) {
	if login == "" || strings.ContainsRune(login, 0) {
		return "", ErrAbsent
	}
	name := dl.CString(login)
	entry, err := passwdLookup("getpwnam_r", uintptr(unsafe.Pointer(name)))
	runtime.KeepAlive(name)
	if errors.Is(err, dl.ErrNoSymbol) || errors.Is(err, dl.ErrUnsupported) {
		u, lerr := user.Lookup(login)
		return fileAccountHome(u, lerr)
	}
	return entry.home, err
}

// Home directory of a numeric user id through every configured name service, ErrAbsent without a record
func AccountHomeByID(uid int) (string, error) {
	entry, err := passwdByID(uid)
	if errors.Is(err, dl.ErrNoSymbol) || errors.Is(err, dl.ErrUnsupported) {
		u, lerr := user.LookupId(strconv.Itoa(uid))
		return fileAccountHome(u, lerr)
	}
	return entry.home, err
}

// Home directory of a record read from the passwd file, ErrAbsent when the file has none
func fileAccountHome(u *user.User, err error) (string, error) {
	var noName user.UnknownUserError
	var noID user.UnknownUserIdError
	if errors.As(err, &noName) || errors.As(err, &noID) {
		return "", ErrAbsent
	}
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// Record of a numeric user id from the C library
func passwdByID(uid int) (passwdEntry, error) {
	if uid < 0 {
		return passwdEntry{}, ErrAbsent
	}
	return passwdLookup("getpwuid_r", uintptr(uint32(uid)))
}

// Runs one reentrant passwd lookup of the C library, growing its string buffer until the record fits
func passwdLookup(symbol string, key uintptr) (passwdEntry, error) {
	fn, err := dl.Self().Symbol(symbol)
	if err != nil {
		return passwdEntry{}, err
	}
	for size := passwdBufferAtLeast; size <= passwdBufferLimit; size *= 4 {
		record := make([]byte, passwdBytes)
		buf := make([]byte, size)
		var result uintptr
		r1, _ := dl.Call(fn, key, uintptr(unsafe.Pointer(&record[0])),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&result)))
		switch ret := syscall.Errno(uint32(r1)); {
		case ret == syscall.ERANGE:
			continue
		case ret == syscall.ENOENT, ret == syscall.ESRCH, ret == syscall.EBADF, ret == syscall.EPERM:
			return passwdEntry{}, ErrAbsent
		case ret != 0:
			return passwdEntry{}, fmt.Errorf("%s: %w", symbol, ret)
		case result == 0:
			return passwdEntry{}, ErrAbsent
		}
		base := uintptr(unsafe.Pointer(&buf[0]))
		field := func(offset int) (string, bool) {
			return cString(buf, base, uintptr(binary.NativeEndian.Uint64(record[offset:])))
		}
		login, okLogin := field(passwdNameOffset)
		gecos, okGecos := field(passwdGecosOffset)
		home, okHome := field(passwdDirOffset)
		if !okLogin || !okGecos || !okHome {
			return passwdEntry{}, fmt.Errorf("%s: record points outside its buffer", symbol)
		}
		return passwdEntry{login: login, gecos: gecos, home: home}, nil
	}
	return passwdEntry{}, fmt.Errorf("%s: record exceeds %d bytes", symbol, passwdBufferLimit)
}

// NUL terminated string at address ptr inside buf, which starts at base, false when ptr lies outside buf
func cString(buf []byte, base, ptr uintptr) (string, bool) {
	if ptr < base || ptr >= base+uintptr(len(buf)) {
		return "", false
	}
	rest := buf[ptr-base:]
	if end := bytes.IndexByte(rest, 0); end >= 0 {
		return string(rest[:end]), true
	}
	return "", false
}

// First comma separated field of a gecos entry
func gecosName(gecos string) string {
	name, _, _ := strings.Cut(gecos, ",")
	return strings.TrimSpace(name)
}

// Login and full name of this user from the C library user database, else the passwd file
func account() (string, string) {
	if entry, err := passwdByID(syscall.Getuid()); err == nil {
		return entry.login, gecosName(entry.gecos)
	}
	u, err := user.Current()
	if err != nil {
		return "", ""
	}
	return u.Username, gecosName(u.Name)
}
