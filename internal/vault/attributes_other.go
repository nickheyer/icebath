//go:build !linux

package vault

import "context"

func noSecretService() error {
	return noKeychain("the Secret Service exists only on Linux")
}

func (k *Keychain) lookupAttributes(context.Context, map[string]string) (string, error) {
	return "", noSecretService()
}

func (k *Keychain) storeAttributes(context.Context, string, map[string]string, string) error {
	return noSecretService()
}

func (k *Keychain) clearAttributes(context.Context, map[string]string) error {
	return noSecretService()
}
