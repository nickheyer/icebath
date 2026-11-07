package vault

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// OS credential store the vault key lives in, the Go side of a python keyring backend
type keyringBackend interface {
	keyringModule() string
	keyringClass() string
	GetPassword(service, account string) (string, bool, error)
	SetPassword(service, account, password string) error
	DeletePassword(service, account string) error
}

// A keyring that writes through an ordered list of children, keyring's ChainerBackend
type keyringChainer interface {
	keyringChildren() []keyringBackend
}

// Exception a keyring backend raises, its python class name kept for the messages hermes formats
type keyringError struct {
	Class string
	Msg   string
}

func (e *keyringError) Error() string { return e.Msg }

// Python exception class name of an error for the type(exc).__name__ hermes reports
func pyErrorClass(err error) string {
	var ke *keyringError
	var de *dbusError
	var errno syscall.Errno
	switch {
	case errors.As(err, &ke):
		return ke.Class
	case errors.As(err, &de):
		return "DBusErrorResponse"
	case errors.Is(err, os.ErrNotExist):
		return "FileNotFoundError"
	case errors.Is(err, os.ErrPermission):
		return "PermissionError"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "TimeoutError"
	case errors.As(err, &errno) && errno == syscall.ECONNREFUSED:
		return "ConnectionRefusedError"
	}
	return "OSError"
}

// Backend name as keyring's name classproperty spells it, module tail then class
func keyringName(b keyringBackend) string {
	mod := b.keyringModule()
	if i := strings.LastIndex(mod, "."); i >= 0 {
		mod = mod[i+1:]
	}
	return strings.ReplaceAll(mod, "_", " ") + " " + b.keyringClass()
}

const noKeyringMessage = "No recommended backend was available. Install a recommended 3rd party backend package; or, " +
	"install the keyrings.alt package if you want to use the non-recommended backends. See " +
	"https://pypi.org/project/keyring for details."

// What keyring resolves to when no OS store is viable, every operation raising NoKeyringError
type failKeyring struct{}

func (failKeyring) keyringModule() string { return "keyring.backends.fail" }
func (failKeyring) keyringClass() string  { return "Keyring" }
func (failKeyring) GetPassword(string, string) (string, bool, error) {
	return "", false, &keyringError{"NoKeyringError", noKeyringMessage}
}
func (failKeyring) SetPassword(string, string, string) error {
	return &keyringError{"NoKeyringError", noKeyringMessage}
}
func (failKeyring) DeletePassword(string, string) error {
	return &keyringError{"NoKeyringError", noKeyringMessage}
}

// Backend keyring.get_keyring returns on this host, swapped by tests for an in memory keyring
var keyringGetKeyring = nativeKeyring

// macOS login keychain through the security tool, the secret passed on stdin and never in argv
type macOSKeyring struct{ tool string }

func (macOSKeyring) keyringModule() string { return "keyring.backends.macOS" }
func (macOSKeyring) keyringClass() string  { return "Keyring" }

func (k macOSKeyring) run(stdin string, args ...string) (string, int, error) {
	cmd := exec.Command(k.tool, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), exit.ExitCode(), fmt.Errorf("%s", strings.TrimSpace(errb.String()))
	}
	if err == nil && strings.Contains(errb.String(), "security: ") {
		return out.String(), 1, fmt.Errorf("%s", strings.TrimSpace(errb.String()))
	}
	return out.String(), 0, err
}

// Item not found in the keychain, errSecItemNotFound as the security tool exits
const macOSItemNotFound = 44

func (k macOSKeyring) GetPassword(service, account string) (string, bool, error) {
	out, code, err := k.run("", "find-generic-password", "-s", service, "-a", account, "-w")
	if code == macOSItemNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, &keyringError{"KeyringError", err.Error()}
	}
	return strings.TrimSuffix(out, "\n"), true, nil
}

func (k macOSKeyring) SetPassword(service, account, password string) error {
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", securityQuote(service), securityQuote(account), securityQuote(password))
	if _, _, err := k.run(line, "-i"); err != nil {
		return &keyringError{"PasswordSetError", "Can't store password on keychain: " + err.Error()}
	}
	return nil
}

func (k macOSKeyring) DeletePassword(service, account string) error {
	_, code, err := k.run("", "delete-generic-password", "-s", service, "-a", account)
	if code == macOSItemNotFound {
		return &keyringError{"PasswordDeleteError", "Item not found"}
	}
	if err != nil {
		return &keyringError{"PasswordDeleteError", "Can't delete password in keychain: " + err.Error()}
	}
	return nil
}

// Double quoted word for the security interactive parser
func securityQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
