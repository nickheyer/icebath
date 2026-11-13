package vault

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/dl"
)

// Name the macOS store is reported by
const keychainName = "macOS Keychain"

// Framework paths the keychain binds and the CFString encoding it passes text in
const (
	securityFramework       = "/System/Library/Frameworks/Security.framework/Security"
	coreFoundationFramework = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	cfStringEncodingUTF8    = 0x08000100
)

// Security framework status codes the keychain tells apart
const (
	errSecItemNotFound          = -25300
	errSecUserCanceled          = -128
	errSecAuthFailed            = -25293
	errSecInteractionNotAllowed = -25308
	errSecMissingEntitlement    = -34018
	errSecCSInfoPlistFailed     = -67030
	errSecNotAvailable          = -25291
	errSecNoSuchKeychain        = -25294
)

// Security framework binding loaded on first use and kept for the keychain
type keychainState struct {
	once sync.Once
	api  *securityAPI
	err  error
}

// Security and CoreFoundation entry points and constants a generic password query needs
type securityAPI struct {
	copyMatching    func(query uintptr, result *uintptr) int32
	add             func(attributes uintptr, result uintptr) int32
	remove          func(query uintptr) int32
	errorMessage    func(status int32, reserved uintptr) uintptr
	release         func(ref uintptr)
	dictCreate      func(alloc uintptr, keys, values *uintptr, count int, keyCallbacks, valueCallbacks uintptr) uintptr
	stringCreate    func(alloc uintptr, text string, encoding uint32) uintptr
	stringLength    func(ref uintptr) int
	stringMaxSize   func(length int, encoding uint32) int
	stringGet       func(ref uintptr, buf []byte, size int, encoding uint32) uint8
	dataCreate      func(alloc uintptr, data []byte, length int) uintptr
	dataLength      func(ref uintptr) int
	dataBytes       func(ref uintptr) uintptr
	keyCallbacks    uintptr
	valueCallbacks  uintptr
	class           uintptr
	genericPassword uintptr
	attrService     uintptr
	attrAccount     uintptr
	returnData      uintptr
	matchLimit      uintptr
	matchLimitOne   uintptr
	valueData       uintptr
	booleanTrue     uintptr
}

// Bound framework entry points, an ErrUnavailable error when the frameworks cannot load
func (k *Keychain) bind() (*securityAPI, error) {
	k.state.once.Do(func() {
		api, err := loadSecurity()
		if err != nil {
			k.state.err = noKeychain(fmt.Sprintf("the Security framework could not load: %v", err))
			return
		}
		k.state.api = api
	})
	return k.state.api, k.state.err
}

// Opens Security and CoreFoundation and binds every function and constant the queries use
func loadSecurity() (*securityAPI, error) {
	security, err := dl.Open(securityFramework, dl.Global)
	if err != nil {
		return nil, err
	}
	foundation, err := dl.Open(coreFoundationFramework, dl.Global)
	if err != nil {
		return nil, err
	}
	a := &securityAPI{}
	if err := security.BindAll([]dl.Binding{
		{Fn: &a.copyMatching, Name: "SecItemCopyMatching"},
		{Fn: &a.add, Name: "SecItemAdd"},
		{Fn: &a.remove, Name: "SecItemDelete"},
		{Fn: &a.errorMessage, Name: "SecCopyErrorMessageString"},
	}); err != nil {
		return nil, err
	}
	if err := foundation.BindAll([]dl.Binding{
		{Fn: &a.release, Name: "CFRelease"},
		{Fn: &a.dictCreate, Name: "CFDictionaryCreate"},
		{Fn: &a.stringCreate, Name: "CFStringCreateWithCString"},
		{Fn: &a.stringLength, Name: "CFStringGetLength"},
		{Fn: &a.stringMaxSize, Name: "CFStringGetMaximumSizeForEncoding"},
		{Fn: &a.stringGet, Name: "CFStringGetCString"},
		{Fn: &a.dataCreate, Name: "CFDataCreate"},
		{Fn: &a.dataLength, Name: "CFDataGetLength"},
		{Fn: &a.dataBytes, Name: "CFDataGetBytePtr"},
	}); err != nil {
		return nil, err
	}
	for _, sym := range []struct {
		lib    *dl.Library
		name   string
		target *uintptr
		deref  bool
	}{
		{foundation, "kCFTypeDictionaryKeyCallBacks", &a.keyCallbacks, false},
		{foundation, "kCFTypeDictionaryValueCallBacks", &a.valueCallbacks, false},
		{foundation, "kCFBooleanTrue", &a.booleanTrue, true},
		{security, "kSecClass", &a.class, true},
		{security, "kSecClassGenericPassword", &a.genericPassword, true},
		{security, "kSecAttrService", &a.attrService, true},
		{security, "kSecAttrAccount", &a.attrAccount, true},
		{security, "kSecReturnData", &a.returnData, true},
		{security, "kSecMatchLimit", &a.matchLimit, true},
		{security, "kSecMatchLimitOne", &a.matchLimitOne, true},
		{security, "kSecValueData", &a.valueData, true},
	} {
		addr, err := sym.lib.Symbol(sym.name)
		if err != nil {
			return nil, err
		}
		if sym.deref {
			addr = *dl.At[uintptr](addr)
		}
		*sym.target = addr
	}
	return a, nil
}

// Immutable CFDictionary of alternating keys and values, owned by the caller
func (a *securityAPI) dictionary(pairs ...uintptr) uintptr {
	n := len(pairs) / 2
	keys, values := make([]uintptr, n), make([]uintptr, n)
	for i := range n {
		keys[i], values[i] = pairs[2*i], pairs[2*i+1]
	}
	dict := a.dictCreate(0, &keys[0], &values[0], n, a.keyCallbacks, a.valueCallbacks)
	runtime.KeepAlive(keys)
	runtime.KeepAlive(values)
	return dict
}

// Releases a CoreFoundation object unless it is null
func (a *securityAPI) free(ref uintptr) {
	if ref != 0 {
		a.release(ref)
	}
}

// UTF-8 contents of a CFString, empty for null
func (a *securityAPI) text(ref uintptr) string {
	if ref == 0 {
		return ""
	}
	size := a.stringMaxSize(a.stringLength(ref), cfStringEncodingUTF8) + 1
	if size <= 1 {
		return ""
	}
	buf := make([]byte, size)
	if a.stringGet(ref, buf, size, cfStringEncodingUTF8) == 0 {
		return ""
	}
	for i, c := range buf {
		if c == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

// Keychain error of a failed status with the framework's own description
func (a *securityAPI) statusError(status int32, op string) error {
	message := a.errorMessage(status, 0)
	description := a.text(message)
	a.free(message)
	var kind error
	switch status {
	case errSecItemNotFound:
		kind = ErrNotFound
	case errSecUserCanceled, errSecAuthFailed, errSecInteractionNotAllowed, errSecMissingEntitlement, errSecCSInfoPlistFailed:
		kind = ErrDenied
	case errSecNotAvailable, errSecNoSuchKeychain:
		kind = ErrUnavailable
	default:
		kind = fmt.Errorf("OSStatus %d", status)
	}
	return &KeychainError{Kind: kind, Reason: fmt.Sprintf("the macOS Keychain could not %s (%s, status %d)", op, description, status)}
}

// Generic password dictionary for service and account plus any extra pairs
func (a *securityAPI) query(service, account uintptr, extra ...uintptr) uintptr {
	return a.dictionary(append([]uintptr{a.class, a.genericPassword, a.attrService, service, a.attrAccount, account}, extra...)...)
}

// CFStrings of service and account, released by the returned function, an error when either cannot be made
func (a *securityAPI) names(service, account string) (uintptr, uintptr, func(), error) {
	s := a.stringCreate(0, service, cfStringEncodingUTF8)
	c := a.stringCreate(0, account, cfStringEncodingUTF8)
	release := func() { a.free(s); a.free(c) }
	if s == 0 || c == 0 {
		release()
		return 0, 0, nil, keychainError(errBadName, "the macOS Keychain could not take %s / %s as text", quote(service), quote(account))
	}
	return s, c, release, nil
}

// Outcome of a keychain read run on a goroutine of its own
type readResult struct {
	secret string
	err    error
}

// Reports whether the Security framework loads
func (k *Keychain) check(context.Context) error {
	_, err := k.bind()
	return err
}

// Data of the generic password for service and account, an access prompt nobody answers ending as denied
func (k *Keychain) get(ctx context.Context, service, account string) (string, error) {
	a, err := k.bind()
	if err != nil {
		return "", err
	}
	wait, cancel := promptContext(ctx)
	defer cancel()
	done := make(chan readResult, 1)
	go func() {
		secret, err := a.read(service, account)
		done <- readResult{secret, err}
	}()
	select {
	case r := <-done:
		return r.secret, r.err
	case <-wait.Done():
		return "", promptUnanswered(ctx, "the macOS Keychain access prompt")
	}
}

// Data of the generic password for service and account, blocking while the system shows an access prompt
func (a *securityAPI) read(service, account string) (string, error) {
	s, c, done, err := a.names(service, account)
	if err != nil {
		return "", err
	}
	defer done()
	q := a.query(s, c, a.returnData, a.booleanTrue, a.matchLimit, a.matchLimitOne)
	defer a.free(q)
	var result uintptr
	if status := a.copyMatching(q, &result); status != 0 {
		return "", a.statusError(status, "read the password")
	}
	defer a.free(result)
	data := dl.Copy[byte](a.dataBytes(result), a.dataLength(result))
	if !utf8.Valid(data) {
		return "", keychainError(errors.New("secret is not UTF-8"), "the macOS Keychain item holds a secret that is not UTF-8 text")
	}
	return string(data), nil
}

// Replaces the password for service and account, refused once ctx ended and otherwise run to a definite outcome
func (k *Keychain) set(ctx context.Context, service, account, secret string) error {
	a, err := k.bind()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s, c, done, err := a.names(service, account)
	if err != nil {
		return err
	}
	defer done()
	q := a.query(s, c)
	defer a.free(q)
	if status := a.remove(q); status != 0 && status != errSecItemNotFound {
		return a.statusError(status, "replace the password")
	}
	value := a.dataCreate(0, []byte(secret), len(secret))
	if value == 0 {
		return keychainError(errors.New("CFDataCreate failed"), "the macOS Keychain could not take the password")
	}
	defer a.free(value)
	attrs := a.query(s, c, a.valueData, value)
	defer a.free(attrs)
	if status := a.add(attrs, 0); status != 0 {
		return a.statusError(status, "store the password")
	}
	return nil
}

// Deletes the password for service and account, refused once ctx ended and otherwise run to a definite outcome
func (k *Keychain) del(ctx context.Context, service, account string) error {
	a, err := k.bind()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s, c, done, err := a.names(service, account)
	if err != nil {
		return err
	}
	defer done()
	q := a.query(s, c)
	defer a.free(q)
	if status := a.remove(q); status != 0 {
		return a.statusError(status, "delete the password")
	}
	return nil
}
