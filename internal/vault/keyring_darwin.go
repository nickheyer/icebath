package vault

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Item not found, as the security tool exits for errSecItemNotFound
const macItemNotFound = 44

// The login keychain through the security tool every Mac ships, the secret written on its stdin and never in its arguments
type macKeyring struct{ tool string }

func nativeKeyring() (keyring, error) {
	return macKeyring{tool: "/usr/bin/security"}, nil
}

func (macKeyring) name() string { return "macOS Keychain" }

func (k macKeyring) run(stdin string, args ...string) (string, int, error) {
	cmd := exec.Command(k.tool, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return out.String(), exit.ExitCode(), errors.New(strings.TrimSpace(errb.String()))
	}
	if err == nil && strings.Contains(errb.String(), "security: ") {
		return out.String(), 1, errors.New(strings.TrimSpace(errb.String()))
	}
	return out.String(), 0, err
}

func (k macKeyring) get(service, account string) (string, bool, error) {
	out, code, err := k.run("", "find-generic-password", "-s", service, "-a", account, "-w")
	if code == macItemNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, &keyringError{"KeyringError", err.Error()}
	}
	return strings.TrimSuffix(out, "\n"), true, nil
}

// Double quoted word of the security tool's interactive parser
func securityQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func (k macKeyring) set(service, account, secret string) error {
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", securityQuote(service), securityQuote(account), securityQuote(secret))
	if _, _, err := k.run(line, "-i"); err != nil {
		return &keyringError{"PasswordSetError", "The keychain did not keep the key: " + strs.Full(err.Error())}
	}
	return nil
}

func (k macKeyring) remove(service, account string) error {
	_, code, err := k.run("", "delete-generic-password", "-s", service, "-a", account)
	if code == macItemNotFound {
		return &keyringError{"PasswordDeleteError", "The keychain has no such entry."}
	}
	if err != nil {
		return &keyringError{"PasswordDeleteError", "The keychain did not delete the entry: " + strs.Full(err.Error())}
	}
	return nil
}
