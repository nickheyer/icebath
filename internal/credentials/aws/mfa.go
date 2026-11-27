package aws

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"

	"github.com/charmbracelet/x/term"
)

// MFA code typed at this process's terminal without echo, given up once ctx ends
func readMFA(ctx context.Context, prompt string) (string, error) {
	path := "/dev/tty"
	if runtime.GOOS == "windows" {
		path = "CONIN$"
	}
	tty, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("This AWS profile needs an MFA code, and there is no terminal to type it in. Sign in with `aws sts get-session-token`, or use a profile without mfa_serial.")
	}
	defer tty.Close()
	if _, err := tty.WriteString(prompt); err != nil {
		return "", err
	}
	type typed struct {
		code string
		err  error
	}
	answer := make(chan typed, 1)
	go func() {
		code, err := term.ReadPassword(tty.Fd())
		tty.WriteString("\n")
		answer <- typed{strings.TrimSpace(string(code)), err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case got := <-answer:
		return got.code, got.err
	}
}
