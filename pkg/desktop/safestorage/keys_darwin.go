//go:build darwin

package safestorage

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os/exec"
	"strings"
)

// Exit status of the security tool when the keychain holds no such item
const errSecItemNotFoundExit = 44

type platformKeys struct {
	service string
	account string
	loaded  bool
	key     []byte
}

func newPlatformKeys(o Options) *platformKeys {
	return &platformKeys{service: o.AppName + " Safe Storage", account: o.AppName}
}

// Derives the v10 key from the login keychain password of the app once per process
func (k *platformKeys) load() {
	if k.loaded {
		return
	}
	k.loaded = true
	if password, ok := keychainPassword(k.service, k.account); ok {
		k.key, _ = deriveCBCKey(password, 1003)
	}
}

func (k *platformKeys) available(bool) bool {
	k.load()
	return k.key != nil
}

func (k *platformKeys) encrypt(plaintext []byte) ([]byte, error) {
	return cbcSeal(k.key, "v10", plaintext)
}

func (k *platformKeys) decrypt(data []byte) ([]byte, error) {
	if bytes.HasPrefix(data, []byte("v10")) {
		return cbcOpen(k.key, data[3:])
	}
	return data, nil
}

// Generic password of service and account, a random one added when the keychain has none
func keychainPassword(service, account string) (string, bool) {
	out, err := exec.Command("/usr/bin/security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err == nil {
		password := strings.TrimSuffix(string(out), "\n")
		return password, password != ""
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != errSecItemNotFoundExit {
		return "", false
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", false
	}
	password := base64.StdEncoding.EncodeToString(raw)
	cmd := exec.Command("/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader("add-generic-password -s " + quoteSecurityArg(service) + " -a " + quoteSecurityArg(account) + " -w " + quoteSecurityArg(password) + "\n")
	if cmd.Run() != nil {
		return "", false
	}
	return password, true
}

// Double quoted argument for the security tool command reader with quotes and backslashes escaped
func quoteSecurityArg(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
