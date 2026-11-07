package vault

import (
	"syscall"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

var (
	advapi32       = syscall.NewLazyDLL("advapi32.dll")
	procCredReadW  = advapi32.NewProc("CredReadW")
	procCredWriteW = advapi32.NewProc("CredWriteW")
	procCredDelete = advapi32.NewProc("CredDeleteW")
	procCredFree   = advapi32.NewProc("CredFree")
)

const (
	credTypeGeneric       = 1
	credPersistEnterprise = 3
	errorNotFound         = 1168
)

type winCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        [2]uint32
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// Windows Credential Manager generic credentials laid out as keyring's WinVaultKeyring stores them
type winVaultKeyring struct{}

func nativeKeyring() (keyringBackend, error) { return winVaultKeyring{}, nil }

func (winVaultKeyring) keyringModule() string { return "keyring.backends.Windows" }
func (winVaultKeyring) keyringClass() string  { return "WinVaultKeyring" }

type winCred struct {
	user string
	blob []byte
}

func utf16Ptr(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func utf16String(p *uint16) string {
	if p == nil {
		return ""
	}
	var units []uint16
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0; ptr = unsafe.Add(ptr, 2) {
		units = append(units, *(*uint16)(ptr))
	}
	return string(utf16.Decode(units))
}

// Credential under a target name, nil when Windows reports ERROR_NOT_FOUND
func winRead(target string) (*winCred, error) {
	var out *winCredential
	r, _, e := procCredReadW.Call(uintptr(unsafe.Pointer(utf16Ptr(target))), credTypeGeneric, 0, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		if errno, ok := e.(syscall.Errno); ok && errno == errorNotFound {
			return nil, nil
		}
		return nil, &keyringError{"error", e.Error()}
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(out)))
	blob := unsafe.Slice(out.CredentialBlob, out.CredentialBlobSize)
	return &winCred{user: utf16String(out.UserName), blob: append([]byte{}, blob...)}, nil
}

// Blob decoded as UTF-16 with the UTF-8 retry keyring's DecodingCredential applies
func (c *winCred) value() string {
	if len(c.blob)%2 == 0 {
		units := make([]uint16, len(c.blob)/2)
		for i := range units {
			units[i] = uint16(c.blob[2*i]) | uint16(c.blob[2*i+1])<<8
		}
		valid := true
		for i := 0; i < len(units); i++ {
			if utf16.IsSurrogate(rune(units[i])) {
				if i+1 >= len(units) || utf16.DecodeRune(rune(units[i]), rune(units[i+1])) == utf8.RuneError {
					valid = false
					break
				}
				i++
			}
		}
		if valid {
			return string(utf16.Decode(units))
		}
	}
	return string(c.blob)
}

func winWrite(target, user, password string) error {
	units := utf16.Encode([]rune(password))
	blob := make([]byte, 2*len(units))
	for i, u := range units {
		blob[2*i], blob[2*i+1] = byte(u), byte(u>>8)
	}
	cred := winCredential{Type: credTypeGeneric, TargetName: utf16Ptr(target), Comment: utf16Ptr("Stored using python-keyring"),
		CredentialBlobSize: uint32(len(blob)), Persist: credPersistEnterprise, UserName: utf16Ptr(user)}
	if len(blob) > 0 {
		cred.CredentialBlob = &blob[0]
	}
	if r, _, e := procCredWriteW.Call(uintptr(unsafe.Pointer(&cred)), 0); r == 0 {
		return &keyringError{"error", e.Error()}
	}
	return nil
}

func winDelete(target string) error {
	r, _, e := procCredDelete.Call(uintptr(unsafe.Pointer(utf16Ptr(target))), credTypeGeneric, 0)
	if r == 0 {
		if errno, ok := e.(syscall.Errno); ok && errno == errorNotFound {
			return nil
		}
		return &keyringError{"error", e.Error()}
	}
	return nil
}

func compoundName(user, service string) string { return user + "@" + service }

func (winVaultKeyring) GetPassword(service, account string) (string, bool, error) {
	res, err := winRead(service)
	if err != nil {
		return "", false, err
	}
	if res == nil || (account != "" && res.user != account) {
		if res, err = winRead(compoundName(account, service)); err != nil || res == nil {
			return "", false, err
		}
	}
	return res.value(), true, nil
}

func (winVaultKeyring) SetPassword(service, account, password string) error {
	existing, err := winRead(service)
	if err != nil {
		return err
	}
	if existing != nil {
		if err := winWrite(compoundName(existing.user, service), existing.user, existing.value()); err != nil {
			return err
		}
	}
	return winWrite(service, account, password)
}

func (winVaultKeyring) DeletePassword(service, account string) error {
	deleted := false
	for _, target := range []string{service, compoundName(account, service)} {
		existing, err := winRead(target)
		if err != nil {
			return err
		}
		if existing != nil && existing.user == account {
			deleted = true
			if err := winDelete(target); err != nil {
				return err
			}
		}
	}
	if !deleted {
		return &keyringError{"PasswordDeleteError", service}
	}
	return nil
}
