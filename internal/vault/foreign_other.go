//go:build !darwin

package vault

import "context"

// Unavailable error for a login keychain item asked for off macOS
func noLoginKeychain() error {
	return noKeychain("the macOS login keychain exists only on macOS")
}

// Refuses because only macOS has a login keychain
func (k *Keychain) findForeign(context.Context, string) (account, secret string, err error) {
	return "", "", noLoginKeychain()
}

// Refuses because only macOS has a login keychain
func (k *Keychain) readForeign(context.Context, string) (string, error) { return "", noLoginKeychain() }

// Refuses because only macOS has a login keychain
func (k *Keychain) updateForeign(context.Context, string, string, []byte) error {
	return noLoginKeychain()
}

// Refuses because only macOS has a login keychain
func (k *Keychain) getForeign(context.Context, string, string) (string, error) {
	return "", noLoginKeychain()
}
