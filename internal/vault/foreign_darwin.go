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

// Account attribute and password lines the security tool prints for find-generic-password -g
var (
	securityAccountLine  = regexp.MustCompile(securityAccountLinePattern)
	securityPasswordLine = regexp.MustCompile(securityPasswordPattern)
)

// Output of one security tool run within timeout, stdin empty unless input is given
func runSecurity(ctx context.Context, timeout time.Duration, input string, args ...string) (proc.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command(securityTool, args...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	res, err := proc.Run(ctx, cmd)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return res, keychainError(context.DeadlineExceeded, "the security tool did not answer within %s", timeout)
	}
	if err != nil {
		return res, keychainError(err, "the security tool could not run: %v", err)
	}
	return res, nil
}

// Keychain error for a security tool exit, its status mapped to the not found and denied kinds
func securityExit(res proc.Result) error {
	detail := head(trimSpace(outputText(res.Stderr)), 200)
	reason := fmt.Sprintf("the security tool exited %d (%s)", res.Exit.Status(), detail)
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
func (k *Keychain) findForeign(ctx context.Context, service string) (ForeignItem, error) {
	res, err := runSecurity(ctx, foreignLookupTimeout, "", "find-generic-password", "-s", service, "-g")
	if err != nil {
		return ForeignItem{}, err
	}
	if !res.Exit.Success() {
		return ForeignItem{}, securityExit(res)
	}
	item := ForeignItem{
		Account: securityAttr(securityAccountLine.FindStringSubmatch(outputText(res.Stdout))),
		Secret:  securityAttr(securityPasswordLine.FindStringSubmatch(outputText(res.Stderr))),
	}
	if item.Account == "" || item.Secret == "" {
		return ForeignItem{}, keychainError(ErrNotFound, "the keychain item %s carries no account or no password", quote(service))
	}
	return item, nil
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

// Updates the item for account and service through security -i so the secret never reaches argv, then reads it back
func (k *Keychain) updateForeign(ctx context.Context, service, account string, secret []byte) error {
	for _, name := range []string{account, service} {
		if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return keychainError(errBadName, "the keychain name %s holds a control character the security tool would read as a new command", quote(name))
		}
	}
	line := fmt.Sprintf("add-generic-password -U -a %s -s %s -X %s\n", securityQuote(account), securityQuote(service), hex.EncodeToString(secret))
	res, err := runSecurity(ctx, foreignUpdateTimeout, line, "-i")
	if err != nil {
		return err
	}
	if !res.Exit.Success() {
		return securityExit(res)
	}
	back, err := runSecurity(ctx, foreignLookupTimeout, "", "find-generic-password", "-a", account, "-s", service, "-w")
	if err != nil {
		return err
	}
	if !back.Exit.Success() {
		return securityExit(back)
	}
	if trimSpace(outputText(back.Stdout)) != trimSpace(string(secret)) {
		detail := head(scrub(trimSpace(outputText(res.Stderr)), hex.EncodeToString(secret), string(secret)), 200)
		return keychainError(errors.New("security tool update not applied"), "the security tool did not store the new secret for %s (%s)", quote(service), detail)
	}
	return nil
}
