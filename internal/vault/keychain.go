package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Kinds a keychain failure matches through errors.Is
var (
	// The entry does not exist
	ErrNotFound = errors.New("keychain entry not found")
	// The keychain is locked or the user or system refused access
	ErrDenied = errors.New("keychain locked or access denied")
	// No OS keychain is reachable from this process
	ErrUnavailable = errors.New("no OS keychain is available")
)

// Error for a service or account name the OS stores could not file an entry under
var errBadName = errors.New("keychain name is not NUL free UTF-8 text")

// Error for a secret the OS stores would alter because it is not UTF-8 text
var errBadSecret = errors.New("keychain secret is not UTF-8 text")

// Longest a keychain operation waits on an OS prompt the caller gave no deadline, the surfaces' unlock deadline
const keychainPromptWait = UnlockPromptTimeout

// Keychain failure with the kind it matches and the reason the store gave, never a secret value
type KeychainError struct {
	Kind   error
	Reason string
}

// Reason the store gave
func (e *KeychainError) Error() string { return e.Reason }

// Kind for errors.Is
func (e *KeychainError) Unwrap() error { return e.Kind }

// Keychain error of kind with a formatted reason
func keychainError(kind error, format string, args ...any) error {
	return &KeychainError{Kind: kind, Reason: fmt.Sprintf(format, args...)}
}

// Unavailable error naming why no keychain is reachable
func noKeychain(reason string) error {
	return keychainError(ErrUnavailable, "no OS keychain is reachable from this session (%s)", reason)
}

// OS credential store of this platform, macOS login items, Windows Credential Manager or the Secret Service
type Keychain struct {
	state keychainState
}

// Keychain whose platform bindings load on first use
func NewKeychain() *Keychain { return &Keychain{} }

// Error as a KeychainError, kept unchanged when it already is one
func asKeychainError(err error) error {
	var ke *KeychainError
	if err == nil || errors.As(err, &ke) {
		return err
	}
	return &KeychainError{Kind: err, Reason: err.Error()}
}

// Refuses a service or account an OS store would truncate at a NUL or mangle as invalid UTF-8
func checkNames(names ...string) error {
	for _, name := range names {
		if !utf8.ValidString(name) || strings.IndexByte(name, 0) >= 0 {
			return keychainError(errBadName, "the keychain name %s is not valid text", quote(name))
		}
	}
	return nil
}

// Context bounding a wait on an OS prompt, the caller's deadline or else the prompt wait
func promptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, keychainPromptWait)
}

// Denied error for a prompt nobody answered before the wait ended, a caller's cancellation kept as it is
func promptUnanswered(parent context.Context, what string) error {
	if errors.Is(parent.Err(), context.Canceled) {
		return parent.Err()
	}
	return &KeychainError{Kind: errors.Join(ErrDenied, context.DeadlineExceeded),
		Reason: what + " was not answered in time, so the keychain stays locked"}
}

// Secret stored for service and account, ErrNotFound when there is none
func (k *Keychain) Get(ctx context.Context, service, account string) (string, error) {
	if err := checkNames(service, account); err != nil {
		return "", err
	}
	secret, err := k.get(ctx, service, account)
	return secret, asKeychainError(err)
}

// Stores secret for service and account, replacing an earlier one, refusing bytes no store returns unchanged
func (k *Keychain) Set(ctx context.Context, service, account, secret string) error {
	if err := checkNames(service, account); err != nil {
		return err
	}
	if !utf8.ValidString(secret) {
		return keychainError(errBadSecret, "the secret for %s / %s is not UTF-8 text, which the keychain cannot return unchanged", quote(service), quote(account))
	}
	return asKeychainError(k.set(ctx, service, account, secret))
}

// Deletes the secret for service and account, ErrNotFound when there is none
func (k *Keychain) Delete(ctx context.Context, service, account string) error {
	if err := checkNames(service, account); err != nil {
		return err
	}
	return asKeychainError(k.del(ctx, service, account))
}

// Nil when this process can reach the OS keychain, an ErrUnavailable error naming why not otherwise
func (k *Keychain) Check(ctx context.Context) error { return asKeychainError(k.check(ctx)) }

// Name of the store this process uses, or why none is reachable
func (k *Keychain) Label(ctx context.Context) string {
	if err := k.Check(ctx); err != nil {
		return "unavailable: " + err.Error()
	}
	return keychainName
}

// Account and secret of the login keychain item filed under service, read through the security tool
func (k *Keychain) FindForeign(ctx context.Context, service string) (account, secret string, err error) {
	if err := checkNames(service); err != nil {
		return "", "", err
	}
	account, secret, err = k.findForeign(ctx, service)
	return account, secret, asKeychainError(err)
}

// Secret of the login keychain item filed under service, read through the security tool
func (k *Keychain) ReadForeign(ctx context.Context, service string) (string, error) {
	if err := checkNames(service); err != nil {
		return "", err
	}
	secret, err := k.readForeign(ctx, service)
	return secret, asKeychainError(err)
}

// Rewrites the secret of the item for service and account in place, hex encoded on stdin
func (k *Keychain) UpdateForeign(ctx context.Context, service, account string, secret []byte) error {
	if err := checkNames(service, account); err != nil {
		return err
	}
	return asKeychainError(k.updateForeign(ctx, service, account, secret))
}

// Secret of the login keychain item filed under service and account, read through the security tool
func (k *Keychain) GetForeign(ctx context.Context, service, account string) (string, error) {
	if err := checkNames(service, account); err != nil {
		return "", err
	}
	secret, err := k.getForeign(ctx, service, account)
	return secret, asKeychainError(err)
}

// Secret of the first Secret Service item whose attributes include every given pair, unlocking it when locked
func (k *Keychain) LookupAttributes(ctx context.Context, attributes map[string]string) (string, error) {
	if err := checkAttributes(attributes); err != nil {
		return "", err
	}
	secret, err := k.lookupAttributes(ctx, attributes)
	return secret, asKeychainError(err)
}

// Stores secret under label and attributes in the default Secret Service collection, replacing an item with the same attributes
func (k *Keychain) StoreAttributes(ctx context.Context, label string, attributes map[string]string, secret string) error {
	if err := checkAttributes(attributes); err != nil {
		return err
	}
	if err := checkNames(label); err != nil {
		return err
	}
	if !utf8.ValidString(secret) {
		return keychainError(errBadSecret, "the secret for the item %s is not UTF-8 text, which the keychain cannot return unchanged", quote(label))
	}
	return asKeychainError(k.storeAttributes(ctx, label, attributes, secret))
}

// Deletes every Secret Service item whose attributes include every given pair
func (k *Keychain) ClearAttributes(ctx context.Context, attributes map[string]string) error {
	if err := checkAttributes(attributes); err != nil {
		return err
	}
	return asKeychainError(k.clearAttributes(ctx, attributes))
}

// Refuses attribute names and values an OS store would truncate or mangle
func checkAttributes(attributes map[string]string) error {
	for name, value := range attributes {
		if err := checkNames(name, value); err != nil {
			return err
		}
	}
	return nil
}
