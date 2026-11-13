// Encrypted autofill vault, password manager login backends, page fill classification and OS keychain access
package vault

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/internal/home"
)

// Kinds a vault failure matches through errors.Is
var (
	// A kind, label, origin, identifier, payload field or authenticator key was refused
	ErrInvalid = errors.New("invalid vault item")
	// The vault file does not decrypt under the key in use
	ErrUndecryptable = errors.New("vault file could not be decrypted")
	// The decrypted vault is not the document the store writes
	ErrCorrupt = errors.New("vault file is corrupted")
	// The vault key is not 32 url-safe base64 bytes
	ErrBadKey = errors.New("vault key is malformed")
	// Ciphertext exists but no key that opens it was found
	ErrKeyMissing = errors.New("vault key could not be found")
	// The key file present does not decrypt the vault
	ErrKeyConflict = errors.New("vault key file does not decrypt the vault")
	// The keychain pointer beside the vault is unreadable or names no entry
	ErrKeyRef = errors.New("vault key pointer is unusable")
	// Reading, writing or moving the key through the OS keychain failed
	ErrKeyStorage = errors.New("vault key storage failed")
	// No item carries the requested id
	ErrNoItem = errors.New("no such vault item")
	// The vault file declares a format version newer than the one this build writes
	ErrNewerFormat = errors.New("vault file uses a newer format")
)

// Vault failure whose text is safe to surface, never carrying a secret value
type Error struct {
	Kind error
	msg  string
}

// Message safe to show the user
func (e *Error) Error() string { return e.msg }

// Kind sentinel for errors.Is
func (e *Error) Unwrap() error { return e.Kind }

// Failure of kind with a formatted message
func failure(kind error, format string, args ...any) *Error {
	return &Error{Kind: kind, msg: fmt.Sprintf(format, args...)}
}

// Profile scoped secret value of a variable name, empty when the active profile has none
type SecretLookup func(ctx context.Context, name string) string

// Settings of the vault section of config.yaml that the caller reads and fills
type Options struct {
	KeyStorage  string
	OnePassword OnePasswordOptions
	Bitwarden   BitwardenOptions
	Plugins     map[string]bool
}

// Settings of vault.onepassword, Disabled meaning enabled is set to false
type OnePasswordOptions struct {
	Disabled               bool
	Account                string
	BinaryPath             string
	ServiceAccountTokenEnv string
}

// Settings of vault.bitwarden, Disabled meaning enabled is set to false
type BitwardenOptions struct {
	Disabled   bool
	BinaryPath string
}

// Process wide vault service holding the keychain, manager sessions and plugin login backends
type Vault struct {
	resolver *home.Resolver
	keychain *Keychain
	secrets  SecretLookup
	sessions *sessions
	plugins  *pluginRegistry
}

// Vault resolving homes through resolver, keeping keys in keychain and reading manager tokens through secrets
func New(resolver *home.Resolver, keychain *Keychain, secrets SecretLookup) *Vault {
	return &Vault{resolver: resolver, keychain: keychain, secrets: secrets, sessions: newSessions(), plugins: newPluginRegistry()}
}

// Normalized vault.key_storage, file when unset
func KeyStorage(setting string) string {
	if setting == "" {
		return "file"
	}
	return lowerFull(trimSpace(setting))
}
