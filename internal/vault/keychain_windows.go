package vault

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Name the Windows store is reported by
const keychainName = "Windows Credential Manager"

// Credential Manager constants and the comment every written credential carries
const (
	credTypeGeneric       = 1
	credPersistEnterprise = 3
	credComment           = "Stored by icebath"
)

// Credential functions of advapi32, loaded from the system directory on first call
var (
	advapi32       = windows.NewLazySystemDLL("advapi32.dll")
	procCredRead   = advapi32.NewProc("CredReadW")
	procCredWrite  = advapi32.NewProc("CredWriteW")
	procCredDelete = advapi32.NewProc("CredDeleteW")
	procCredFree   = advapi32.NewProc("CredFree")
)

// Credential Manager keeps no state of its own beyond the lazily loaded procedures
type keychainState struct{}

// CREDENTIALW with its pointers to UTF-16 text and blob bytes, the layout of wincred.h on 64 bit Windows
type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// Generic secret and user name read back from one target name
type credential struct {
	user string
	blob []byte
}

// Error number a failed credential call left behind
func lastError(err error) syscall.Errno {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	return windows.ERROR_GEN_FAILURE
}

// Keychain error of a failed credential call, missing entries, denials and logon sessions told apart
func credentialError(code syscall.Errno, op, target string) error {
	reason := fmt.Sprintf("Windows Credential Manager could not %s %s (%v)", op, quote(target), code)
	switch code {
	case windows.ERROR_NOT_FOUND:
		return &KeychainError{Kind: ErrNotFound, Reason: reason}
	case windows.ERROR_ACCESS_DENIED:
		return &KeychainError{Kind: ErrDenied, Reason: reason}
	case windows.ERROR_NO_SUCH_LOGON_SESSION:
		return noKeychain(reason)
	}
	return &KeychainError{Kind: code, Reason: reason}
}

// Copy of a NUL terminated UTF-16 string Windows owns, empty for null
func wideString(p *uint16) string {
	if p == nil {
		return ""
	}
	return windows.UTF16PtrToString(p)
}

// Generic credential stored under target and whether there is one
func readTarget(target string) (credential, bool, error) {
	name, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return credential{}, false, keychainError(errBadName, "the credential name %s is not valid", quote(target))
	}
	var out *credentialW
	r, _, callErr := procCredRead.Call(uintptr(unsafe.Pointer(name)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		if code := lastError(callErr); code != windows.ERROR_NOT_FOUND {
			return credential{}, false, credentialError(code, "read", target)
		}
		return credential{}, false, nil
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(out)))
	c := credential{user: wideString(out.UserName)}
	if out.CredentialBlob != nil && out.CredentialBlobSize > 0 {
		c.blob = append([]byte(nil), unsafe.Slice(out.CredentialBlob, out.CredentialBlobSize)...)
	}
	return c, true, nil
}

// Writes a generic credential under target for user with the password UTF-16 little endian encoded
func writeTarget(target, user, password string) error {
	name, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return keychainError(errBadName, "the credential name %s is not valid", quote(target))
	}
	comment, err := windows.UTF16PtrFromString(credComment)
	if err != nil {
		return err
	}
	userName, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return keychainError(errBadName, "the credential user %s is not valid", quote(user))
	}
	units := utf16.Encode([]rune(password))
	blob := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(blob[2*i:], u)
	}
	cred := credentialW{Type: credTypeGeneric, TargetName: name, Comment: comment, CredentialBlobSize: uint32(len(blob)),
		Persist: credPersistEnterprise, UserName: userName}
	if len(blob) > 0 {
		cred.CredentialBlob = &blob[0]
	}
	r, _, callErr := procCredWrite.Call(uintptr(unsafe.Pointer(&cred)), 0)
	runtime.KeepAlive(&cred)
	runtime.KeepAlive(blob)
	if r == 0 {
		return credentialError(lastError(callErr), "write", target)
	}
	return nil
}

// Deletes the generic credential under target, one an earlier delete already removed counting as deleted
func deleteTarget(target string) error {
	name, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return keychainError(errBadName, "the credential name %s is not valid", quote(target))
	}
	r, _, callErr := procCredDelete.Call(uintptr(unsafe.Pointer(name)), credTypeGeneric, 0)
	if r == 0 {
		if code := lastError(callErr); code != windows.ERROR_NOT_FOUND {
			return credentialError(code, "delete", target)
		}
	}
	return nil
}

// Text of a credential blob, UTF-16 with an optional byte order mark first and UTF-8 second
func blobText(blob []byte) (string, error) {
	if s, ok := utf16Text(blob); ok {
		return s, nil
	}
	if utf8.Valid(blob) {
		return string(blob), nil
	}
	return "", keychainError(errors.New("undecodable credential"), "the stored credential is neither UTF-16 nor UTF-8 text")
}

// Strict UTF-16 decode honouring a leading byte order mark, little endian without one
func utf16Text(blob []byte) (string, bool) {
	if len(blob)%2 != 0 {
		return "", false
	}
	order := binary.ByteOrder(binary.LittleEndian)
	switch {
	case len(blob) >= 2 && blob[0] == 0xff && blob[1] == 0xfe:
		blob = blob[2:]
	case len(blob) >= 2 && blob[0] == 0xfe && blob[1] == 0xff:
		blob, order = blob[2:], binary.BigEndian
	}
	units := make([]uint16, len(blob)/2)
	for i := range units {
		units[i] = order.Uint16(blob[2*i:])
	}
	for i := 0; i < len(units); i++ {
		switch u := rune(units[i]); {
		case u >= 0xd800 && u <= 0xdbff:
			if i+1 >= len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
				return "", false
			}
			i++
		case u >= 0xdc00 && u <= 0xdfff:
			return "", false
		}
	}
	return string(utf16.Decode(units)), true
}

// Compound target a second account of one service is filed under
func compoundTarget(account, service string) string { return account + "@" + service }

// Reports whether advapi32 exports the credential functions
func (k *Keychain) check(context.Context) error {
	for _, proc := range []*windows.LazyProc{procCredRead, procCredWrite, procCredDelete, procCredFree} {
		if err := proc.Find(); err != nil {
			return noKeychain(fmt.Sprintf("advapi32 lacks %s: %v", proc.Name, err))
		}
	}
	return nil
}

// Password under the service target when it belongs to account or no account is named, else under the compound target
func (k *Keychain) get(ctx context.Context, service, account string) (string, error) {
	if err := k.check(ctx); err != nil {
		return "", err
	}
	res, found, err := readTarget(service)
	if err != nil {
		return "", err
	}
	if !found || account != "" && res.user != account {
		if res, found, err = readTarget(compoundTarget(account, service)); err != nil {
			return "", err
		}
		if !found {
			return "", keychainError(ErrNotFound, "no credential for %s / %s", quote(service), quote(account))
		}
	}
	return blobText(res.blob)
}

// Moves whatever account already holds the service target to its compound target, then writes this one there
func (k *Keychain) set(ctx context.Context, service, account, secret string) error {
	if err := k.check(ctx); err != nil {
		return err
	}
	existing, found, err := readTarget(service)
	if err != nil {
		return err
	}
	if found {
		value, err := blobText(existing.blob)
		if err != nil {
			return err
		}
		if err := writeTarget(compoundTarget(existing.user, service), existing.user, value); err != nil {
			return err
		}
	}
	return writeTarget(service, account, secret)
}

// Deletes the credentials of account under the service and compound targets, ErrNotFound when neither holds one
func (k *Keychain) del(ctx context.Context, service, account string) error {
	if err := k.check(ctx); err != nil {
		return err
	}
	deleted := false
	for _, target := range []string{service, compoundTarget(account, service)} {
		existing, found, err := readTarget(target)
		if err != nil {
			return err
		}
		if found && existing.user == account {
			deleted = true
			if err := deleteTarget(target); err != nil {
				return err
			}
		}
	}
	if !deleted {
		return keychainError(ErrNotFound, "no credential for %s / %s", quote(service), quote(account))
	}
	return nil
}
