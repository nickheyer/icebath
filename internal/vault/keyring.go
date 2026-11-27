package vault

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// Keychain service every vault key is filed under
const keychainService = "Icebath Agent vault key"

// OS credential store a vault key can live in
type keyring interface {
	// Name of the store as a person reads it
	name() string
	// Value of the entry, false when there is none
	get(service, account string) (string, bool, error)
	set(service, account, secret string) error
	remove(service, account string) error
}

// Failure of a keyring, of a class its messages name
type keyringError struct {
	class string
	text  string
}

func (e *keyringError) Error() string { return e.text }

// Class of a keyring failure as its messages name it
func errorClass(err error) string {
	var ke *keyringError
	var errno syscall.Errno
	switch {
	case errors.As(err, &ke):
		return ke.class
	case isBusError(err):
		return "DBusErrorResponse"
	case errors.Is(err, os.ErrNotExist):
		return "FileNotFoundError"
	case errors.Is(err, os.ErrPermission):
		return "PermissionError"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "TimeoutError"
	case errors.As(err, &errno) && errno == syscall.ECONNREFUSED:
		return "ConnectionRefusedError"
	}
	return "OSError"
}

// No OS keychain this process can reach: a session with no graphical login, or a store that is only another file
type KeychainUnavailable struct{ Msg string }

func (e *KeychainUnavailable) Error() string { return e.Msg }

// The keychain this process uses, KeychainUnavailable when there is none
func osKeyring() (keyring, error) {
	k, err := nativeKeyring()
	if err != nil {
		return nil, &KeychainUnavailable{Msg: err.Error()}
	}
	return k, nil
}

// Keychain of this computer as a person reads its name, or why there is none
func KeychainLabel() string {
	k, err := osKeyring()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return k.name()
}

// Line of a keyring failure: what went wrong and its class
func refused(what string, err error) string {
	return fmt.Sprintf("%s (%s)", what, strings.TrimSpace(errorClass(err)))
}
