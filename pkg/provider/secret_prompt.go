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
)

// ErrSecretPromptInterrupted is the Ctrl+C a masked secret prompt raises as KeyboardInterrupt
var ErrSecretPromptInterrupted = errors.New("KeyboardInterrupt")

// Streams and the getpass fallback the masked prompt reads and writes, swapped by tests
var (
	secretPromptStdin  = func() *os.File { return os.Stdin }
	secretPromptStdout = func() *os.File { return os.Stdout }
	secretGetpass      = getpass
)

// Reads one secret line writing a mask per typed char, Ctrl+C interrupting and EOF raising io.EOF
func collectMaskedInput(readChar func() string, write func(string), prompt, mask string) (string, error) {
	var value []string
	write(prompt)
	for {
		ch := readChar()
		switch ch {
		case "\r", "\n":
			write("\r\n")
			return strings.Join(value, ""), nil
		case "\x03":
			write("\r\n")
			return "", ErrSecretPromptInterrupted
		case "\x04", "\x1a", "":
			write("\r\n")
			return "", io.EOF
		case "\b", "\x7f":
			if len(value) > 0 {
				value = value[:len(value)-1]
				write("\b \b")
			}
			continue
		case "\x1b":
			continue
		}
		value = append(value, ch)
		if mask != "" {
			write(mask)
		}
	}
}

// Prompts for a secret with asterisk feedback, getpass when either stream is not a terminal
func maskedSecretPrompt(prompt string) (string, error) {
	return maskedSecretPromptMask(prompt, "*")
}

// Masked secret prompt with a chosen mask, raw terminal failures falling back to getpass
func maskedSecretPromptMask(prompt, mask string) (string, error) {
	stdin, stdout := secretPromptStdin(), secretPromptStdout()
	if !streamIsTTY(stdin) || !streamIsTTY(stdout) {
		return secretGetpass(prompt)
	}
	value, err := maskedSecretPromptRaw(stdin, stdout, prompt, mask)
	if err == nil || errors.Is(err, ErrSecretPromptInterrupted) || errors.Is(err, io.EOF) {
		return value, err
	}
	return secretGetpass(prompt)
}

// True when the stream is an open terminal
func streamIsTTY(f *os.File) bool {
	return f != nil && xterm.IsTerminal(f.Fd())
}

// Raw mode masked read restoring the terminal state, special keys swallowed whole on Windows consoles
func maskedSecretPromptRaw(stdin, stdout *os.File, prompt, mask string) (string, error) {
	fd := stdin.Fd()
	oldState, err := xterm.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer func() { _ = xterm.Restore(fd, oldState) }()
	reader := bufio.NewReader(stdin)
	write := func(text string) { _, _ = io.WriteString(stdout, text); _ = stdout.Sync() }
	readChar := func() string { return readSecretRune(reader) }
	if runtime.GOOS == "windows" {
		readChar = func() string {
			ch := readSecretRune(reader)
			if ch == "\x1b" || ch == "\x00" || ch == "\u00e0" {
				skipKeySequence(reader, ch)
				return "\x1b"
			}
			return ch
		}
	}
	return collectMaskedInput(readChar, write, prompt, mask)
}

// One decoded character from the stream, empty once it is closed
func readSecretRune(reader *bufio.Reader) string {
	r, _, err := reader.ReadRune()
	if err != nil {
		return ""
	}
	return string(r)
}

// Consumes the rest of a special key, its scan code or the CSI or SS3 escape tail
func skipKeySequence(reader *bufio.Reader, lead string) {
	if lead != "\x1b" {
		_, _, _ = reader.ReadRune()
		return
	}
	if reader.Buffered() == 0 {
		return
	}
	next, _, err := reader.ReadRune()
	if err != nil || (next != '[' && next != 'O') {
		return
	}
	for reader.Buffered() > 0 {
		b, err := reader.ReadByte()
		if err != nil || (b >= 0x40 && b <= 0x7e) {
			return
		}
	}
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
