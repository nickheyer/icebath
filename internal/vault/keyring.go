package vault

import "github.com/nickheyer/icebath/pkg/strs"

// Keychain service every vault key is filed under
const keychainService = "icebath vault key"

// OS credential store a vault key can live in
type keyring interface {
	// Name of the store as a person reads it
	name() string
	// Value of the entry, false when there is none
	get(service, account string) (string, bool, error)
	set(service, account, secret string) error
	remove(service, account string) error
}

// Failure of a keyring, of a class the code tells failures apart by and a sentence a person reads
type keyringError struct {
	class string
	text  string
}

func (e *keyringError) Error() string { return e.text }

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
		return strs.Full(err.Error())
	}
	return k.name()
}

// Line of a keyring failure: what went wrong, then what the keychain said
func refused(what string, err error) string {
	return what + " " + strs.Full(err.Error())
}
