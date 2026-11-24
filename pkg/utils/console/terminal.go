package consoleutil

import (
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/charmbracelet/colorprofile"
	termutil "github.com/nickheyer/icebath/pkg/utils/term"
)

// Environment variables color detection reads, the key one detection is kept under
var profileEnv = [...]string{"TERM", "COLORTERM", "NO_COLOR", "FORCE_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "TMUX",
	"WT_SESSION", "GOOGLE_CLOUD_SHELL", "ConEmuANSI", "ANSICON", "ANSICON_VER"}

// Detected terminal profiles by environment, detection loading terminfo and asking tmux only once per environment
var terminalProfiles sync.Map

// Color profile of a terminal in the current environment, detected once per environment
func terminalColors() colorprofile.Profile {
	var key strings.Builder
	for _, name := range profileEnv {
		key.WriteString(os.Getenv(name))
		key.WriteByte(0)
	}
	if p, ok := terminalProfiles.Load(key.String()); ok {
		return p.(colorprofile.Profile)
	}
	p := detectTerminalColors()
	terminalProfiles.Store(key.String(), p)
	return p
}

// Color profile a terminal shows: none on a dumb or unknown terminal unless color is forced, the sixteen standard colors (all of them under COLORTERM=truecolor) when TERM is unset or empty outside Windows, else what the environment, terminfo, tmux and the Windows console report; attributes without colors under a non empty NO_COLOR
func detectTerminalColors() colorprofile.Profile {
	var p colorprofile.Profile
	switch term := os.Getenv("TERM"); {
	case dumbTerminal():
		p = colorprofile.NoTTY
	case term == "" && runtime.GOOS != "windows":
		p = colorprofile.ANSI
		if ct := strings.ToLower(os.Getenv("COLORTERM")); ct == "truecolor" || ct == "24bit" {
			p = colorprofile.TrueColor
		}
	default:
		p = colorprofile.Detect(nil, append(os.Environ(), "TTY_FORCE=1"))
	}
	if forcedColor() {
		p = max(p, colorprofile.ANSI)
	}
	if p > colorprofile.ASCII && os.Getenv("NO_COLOR") != "" {
		p = colorprofile.ASCII
	}
	return p
}

// Whether TERM names a terminal that takes no escape sequences
func dumbTerminal() bool {
	term := os.Getenv("TERM")
	return term == "dumb" || term == "unknown"
}

// Whether the environment asks for color on any output: a non empty FORCE_COLOR, or CLICOLOR_FORCE set to anything but 0
func forcedColor() bool {
	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	v := os.Getenv("CLICOLOR_FORCE")
	return v != "" && v != "0"
}

// Color profile of the terminal the program runs in, for output the program has chosen to style: what the terminal shows, at least the sixteen standard colors, attributes without colors under a non empty NO_COLOR
func TerminalProfile() colorprofile.Profile {
	if os.Getenv("NO_COLOR") != "" {
		return colorprofile.ASCII
	}
	return max(terminalColors(), colorprofile.ANSI)
}

// Color profile output to a writer can use: the terminal's when the writer is one, TerminalProfile when the environment forces color, else none
func detectProfile(w io.Writer) colorprofile.Profile {
	switch {
	case isTerminal(w):
		return terminalColors()
	case forcedColor():
		return TerminalProfile()
	}
	return colorprofile.NoTTY
}

// Whether w is a terminal, a standard descriptor a live display has pointed at a pipe counting as the terminal it pointed at
func isTerminal(w io.Writer) bool {
	if termutil.IsTerminal(w) {
		return true
	}
	l := liveActive.Load()
	if l == nil || len(l.streams) == 0 {
		return false
	}
	fd, ok := termutil.Fd(w)
	return ok && l.captures(fd)
}

// Width in cells output wraps at: COLUMNS, else the terminal on stdout, the terminals a live display captured, stderr or stdin, else 80
func DetectWidth() int {
	if cols, _ := termutil.Size(0, 0); cols > 0 {
		return cols
	}
	fds := make([]uintptr, 0, 4)
	if l := liveActive.Load(); l != nil {
		for _, s := range l.streams {
			fds = append(fds, s.termFd)
		}
	}
	for _, fd := range append(fds, uintptr(syscall.Stderr), uintptr(syscall.Stdin)) {
		if cols, _, err := termutil.SizeOf(fd); err == nil {
			return cols
		}
	}
	return 80
}
