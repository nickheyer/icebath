package provider

import (
	"errors"
	"io"
	"os"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// Points the masked prompt at a real pseudo terminal and scripts the inline field and getpass under it
func secretPromptOnPty(t *testing.T, field func(prompt, mask string) (string, error)) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut, oldGetpass, oldField := secretPromptStdin, secretPromptStdout, secretGetpass, secretFieldFn
	t.Cleanup(func() {
		secretPromptStdin, secretPromptStdout, secretGetpass, secretFieldFn = oldIn, oldOut, oldGetpass, oldField
		slave.Close()
		master.Close()
	})
	secretPromptStdin = func() *os.File { return slave }
	secretPromptStdout = func() *os.File { return slave }
	secretGetpass = func(prompt string) (string, error) { return "getpass:" + prompt, nil }
	secretFieldFn = field
}

func TestMaskedSecretPromptReadsThroughTheInlineFieldOnATerminal(t *testing.T) {
	var seen [2]string
	secretPromptOnPty(t, func(prompt, mask string) (string, error) {
		seen = [2]string{prompt, mask}
		return "sk-typed", nil
	})
	if got, err := maskedSecretPrompt("API key: "); err != nil || got != "sk-typed" || seen != [2]string{"API key: ", "*"} {
		t.Fatalf("got %q err %v field saw %q", got, err, seen)
	}
	if _, err := maskedSecretPromptMask("Token: ", ""); err != nil || seen != [2]string{"Token: ", ""} {
		t.Fatalf("the chosen mask did not reach the field, saw %q err %v", seen, err)
	}
}

func TestMaskedSecretPromptMapsInterruptAndEndOfInput(t *testing.T) {
	secretPromptOnPty(t, func(string, string) (string, error) { return "", cliui.ErrInterrupt })
	if _, err := maskedSecretPrompt("API key: "); !errors.Is(err, ErrSecretPromptInterrupted) {
		t.Fatalf("interrupt gave %v", err)
	}
	secretFieldFn = func(string, string) (string, error) { return "", cliui.ErrEOF }
	if _, err := maskedSecretPrompt("API key: "); !errors.Is(err, io.EOF) {
		t.Fatalf("end of input gave %v", err)
	}
}

func TestMaskedSecretPromptUsesGetpassWhenTheTerminalCannotDrawTheField(t *testing.T) {
	secretPromptOnPty(t, func(string, string) (string, error) { return "", cliui.ErrNoEditor })
	if got, err := maskedSecretPrompt("API key: "); err != nil || got != "getpass:API key: " {
		t.Fatalf("got %q err %v", got, err)
	}
}
