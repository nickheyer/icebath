package provider

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	xterm "github.com/charmbracelet/x/term"
	"github.com/nickheyer/icebath/pkg/cliui"
)

// ErrSecretPromptInterrupted is the Ctrl+C a masked secret prompt raises as KeyboardInterrupt
var ErrSecretPromptInterrupted = errors.New("KeyboardInterrupt")

// Streams and the getpass fallback the masked prompt reads and writes, swapped by tests
var (
	secretPromptStdin  = func() *os.File { return os.Stdin }
	secretPromptStdout = func() *os.File { return os.Stdout }
	secretGetpass      = getpass
)

// Prompts for a secret with one mask mark per character, getpass when either stream is not a terminal
func maskedSecretPrompt(prompt string) (string, error) {
	return maskedSecretPromptMask(prompt, "*")
}

// Masked terminal prompt for a secret value, the exported form plugins reach through Services
func MaskedSecretPrompt(prompt string) (string, error) { return maskedSecretPrompt(prompt) }

// Masked secret prompt with a chosen mask in an inline field, getpass when the terminal cannot draw one
func maskedSecretPromptMask(prompt, mask string) (string, error) {
	stdin, stdout := secretPromptStdin(), secretPromptStdout()
	if !streamIsTTY(stdin) || !streamIsTTY(stdout) {
		return secretGetpass(prompt)
	}
	value, err := secretFieldFn(prompt, mask)
	switch {
	case err == nil:
		return value, nil
	case errors.Is(err, cliui.ErrInterrupt):
		return "", ErrSecretPromptInterrupted
	case errors.Is(err, cliui.ErrEOF):
		return "", io.EOF
	}
	return secretGetpass(prompt)
}

// Inline masked field on the process console, swapped by tests
var secretFieldFn = func(prompt, mask string) (string, error) { return cliui.Default().SecretLine(prompt, mask) }

// True when the stream is an open terminal
func streamIsTTY(f *os.File) bool {
	return f != nil && xterm.IsTerminal(f.Fd())
}

// Echo free line read on the controlling terminal, stdin with an echo warning when there is none
func getpass(prompt string) (string, error) {
	if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		defer tty.Close()
		if xterm.IsTerminal(tty.Fd()) {
			_, _ = io.WriteString(tty, prompt)
			line, err := xterm.ReadPassword(tty.Fd())
			_, _ = io.WriteString(tty, "\n")
			if err != nil {
				return "", err
			}
			return string(line), nil
		}
	}
	if stdin := secretPromptStdin(); streamIsTTY(stdin) && runtime.GOOS == "windows" {
		_, _ = io.WriteString(os.Stderr, prompt)
		line, err := xterm.ReadPassword(stdin.Fd())
		_, _ = io.WriteString(os.Stderr, "\r\n")
		if err != nil {
			return "", err
		}
		return string(line), nil
	}
	fmt.Fprintln(os.Stderr, "Warning: Password input may be echoed.")
	_, _ = io.WriteString(os.Stderr, prompt)
	line, err := bufio.NewReader(secretPromptStdin()).ReadString('\n')
	if err != nil && line == "" {
		return "", io.EOF
	}
	return strings.TrimSuffix(line, "\n"), nil
}

// Getpass is the echo free terminal line read with its stdin fallback
func Getpass(prompt string) (string, error) { return getpass(prompt) }
