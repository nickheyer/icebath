package vault

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/proc"
)

// Time the security tool gets for a lookup and for an update
const (
	foreignLookupTimeout = 5 * time.Second
	foreignUpdateTimeout = 10 * time.Second
)

// Security tool every macOS install ships, run by path so no other program on PATH receives a credential
const securityTool = "/usr/bin/security"

// Exit statuses of the security tool, the low byte of the Security framework status it failed with
const (
	securityExitNotFound      = 44
	securityExitNoInteraction = 36
	securityExitAuthFailed    = 51
	securityExitUserCanceled  = 128
)

// Attribute value, whitespace and line patterns of the security tool printout, a text value running to the last quote
const (
	securityAttrValue          = `(?:0x([0-9A-Fa-f]+)\b.*|"(.*)")`
	securitySpaces             = `[\t\n\v\f\r\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]*`
	securityAccountLinePattern = `(?m)^` + securitySpaces + `"acct"<blob>=` + securityAttrValue + securitySpaces + `$`
	securityPasswordPattern    = `(?m)^password: ` + securityAttrValue + securitySpaces + `$`
)

// Account attribute and password lines of find-generic-password -g, and any line carrying a password
var (
	securityAccountLine  = regexp.MustCompile(securityAccountLinePattern)
	securityPasswordLine = regexp.MustCompile(securityPasswordPattern)
	securityPasswordEcho = regexp.MustCompile(`(?m)^password:.*$`)
)

// Output of one security tool run within timeout, stdin empty unless input is given
func runSecurity(parent context.Context, timeout time.Duration, input string, args ...string) (proc.Result, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.Command(securityTool, args...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	res, err := proc.Run(ctx, cmd)
	switch {
	case err == nil:
		return res, nil
	case parent.Err() != nil:
		return res, parent.Err()
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return res, keychainError(context.DeadlineExceeded, "the security tool did not answer within %s", timeout)
	}
	return res, keychainError(err, "the security tool could not run: %v", err)
}

// Keychain error for a security tool exit, password lines and the given secrets kept out of its text
func securityExit(res proc.Result, secrets ...string) error {
	text := withoutSecrets(securityPasswordEcho.ReplaceAllString(outputText(res.Stderr), ""), secrets...)
	reason := fmt.Sprintf("the security tool exited %d (%s)", res.Exit.Status(), head(trimSpace(text), 200))
	switch res.Exit.Code {
	case securityExitNotFound:
		return &KeychainError{Kind: ErrNotFound, Reason: reason}
	case securityExitNoInteraction, securityExitAuthFailed, securityExitUserCanceled:
		return &KeychainError{Kind: ErrDenied, Reason: reason}
	}
	return &KeychainError{Kind: errors.New("security tool failed"), Reason: reason}
}

// Value of a printed attribute, hex decoded when the tool printed it as hex, empty when undecodable
func securityAttr(m []string) string {
	if m == nil {
		return ""
	}
	if m[1] != "" {
		raw, err := hex.DecodeString(m[1])
		if err != nil || !utf8.Valid(raw) {
			return ""
		}
		return string(raw)
	}
	return m[2]
}

// Double quoted word for the security interactive tokenizer, backslashes and quotes escaped
func securityQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Account and password of the item under service from one find-generic-password -g call
func (k *Keychain) findForeign(ctx context.Context, service string) (account, secret string, err error) {
	res, err := runSecurity(ctx, foreignLookupTimeout, "", "find-generic-password", "-s", service, "-g")
	if err != nil {
		return "", "", err
	}
	if !res.Exit.Success() {
		return "", "", securityExit(res)
	}
	account = securityAttr(securityAccountLine.FindStringSubmatch(outputText(res.Stdout)))
	secret = securityAttr(securityPasswordLine.FindStringSubmatch(outputText(res.Stderr)))
	if account == "" || secret == "" {
		return "", "", keychainError(ErrNotFound, "the keychain item %s carries no account or no password", quote(service))
	}
	return account, secret, nil
}

// Password of the item under service and account from one find-generic-password -g call, hex printouts decoded
func (k *Keychain) getForeign(ctx context.Context, service, account string) (string, error) {
	res, err := runSecurity(ctx, foreignLookupTimeout, "", "find-generic-password", "-s", service, "-a", account, "-g")
	if err != nil {
		return "", err
	}
	if !res.Exit.Success() {
		return "", securityExit(res)
	}
	secret := securityAttr(securityPasswordLine.FindStringSubmatch(outputText(res.Stderr)))
	if secret == "" {
		return "", keychainError(ErrNotFound, "the keychain item %s / %s carries no readable password", quote(service), quote(account))
	}
	return secret, nil
}

// Password of the item under service from one find-generic-password -w call
func (k *Keychain) readForeign(ctx context.Context, service string) (string, error) {
	res, err := runSecurity(ctx, foreignLookupTimeout, "", "find-generic-password", "-s", service, "-w")
	if err != nil {
		return "", err
	}
	if !res.Exit.Success() {
		return "", securityExit(res)
	}
	secret := trimSpace(outputText(res.Stdout))
	if secret == "" {
		return "", keychainError(ErrNotFound, "the keychain item %s holds an empty password", quote(service))
	}
	return secret, nil
}

// Updates the item for account and service through one security -i line so the secret never reaches argv
func (k *Keychain) updateForeign(ctx context.Context, service, account string, secret []byte) error {
	for _, name := range []string{account, service} {
		if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return keychainError(errBadName, "the keychain name %s holds a control character the security tool would read as a new command", quote(name))
		}
	}
	encoded := hex.EncodeToString(secret)
	line := fmt.Sprintf("add-generic-password -U -a %s -s %s -X %s\n", securityQuote(account), securityQuote(service), encoded)
	res, err := runSecurity(ctx, foreignUpdateTimeout, line, "-i")
	if err != nil {
		return err
	}
	if !res.Exit.Success() {
		return securityExit(res, encoded, string(secret))
	}
	return nil
}
