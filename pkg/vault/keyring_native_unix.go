//go:build !darwin && !windows

package vault

// Secret Service when its daemon is reachable, else the fail keyring as keyring resolves on Linux
func nativeKeyring() (keyringBackend, error) {
	ss := newSecretServiceKeyring()
	if ss.viable() == nil {
		return ss, nil
	}
	return failKeyring{}, nil
}
