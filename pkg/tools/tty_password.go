package tools

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Hidden line reader of the controlling terminal, swapped by tests that hold the read open
var readHiddenLineFn = readHiddenLine

// Prints the sudo password box and reads one hidden line from the controlling terminal, false when the wait timed out
func readHiddenPasswordFromTTY(timeout time.Duration) (string, bool) {
	os.Setenv("ICEBATH_SPINNER_PAUSE", "1")
	defer os.Unsetenv("ICEBATH_SPINNER_PAUSE")
	time.Sleep(200 * time.Millisecond)
	seconds := int(timeout.Seconds())
	wait := fmt.Sprintf("\u2502    \u2022 Wait %ds to auto-skip", seconds)
	fmt.Println(strings.Join([]string{
		"",
		"\u250c" + strings.Repeat("\u2500", 58) + "\u2510",
		"\u2502  \U0001F510 SUDO PASSWORD REQUIRED" + strings.Repeat(" ", 30) + "\u2502",
		"\u251c" + strings.Repeat("\u2500", 58) + "\u2524",
		"\u2502  Enter password below (input is hidden), or:            \u2502",
		"\u2502    \u2022 Press Enter to skip (command fails gracefully)     \u2502",
		wait + strings.Repeat(" ", 27) + "\u2502",
		"\u2514" + strings.Repeat("\u2500", 58) + "\u2518",
		"",
	}, "\n"))
	fmt.Print("  Password (hidden): ")
	flushStdout()
	done := make(chan string, 1)
	read := readHiddenLineFn
	go func() { done <- read() }()
	closeWindow := humanWaitWindow(nil, "")
	var password string
	select {
	case password = <-done:
		closeWindow()
	case <-time.After(timeout):
		closeWindow()
		fmt.Print("\n  \u23f1 Timeout - continuing without sudo\n    (Press Enter to dismiss)\n\n")
		flushStdout()
		return "", false
	}
	if password != "" {
		fmt.Print("\n  \u2713 Password received (cached for this session)\n\n")
	} else {
		fmt.Print("\n  \u23ed Skipped - continuing without sudo\n\n")
	}
	flushStdout()
	return password, true
}
