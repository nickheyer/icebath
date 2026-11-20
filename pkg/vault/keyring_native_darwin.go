package vault

// The login keychain through the security tool every macOS install ships
func nativeKeyring() (keyringBackend, error) {
	return macOSKeyring{tool: "/usr/bin/security"}, nil
}
