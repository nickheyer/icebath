package browserutil

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	shellutil "github.com/nickheyer/icebath/pkg/utils/shell"
)

// One way Open can show a URL, a command line or, with no Args, the operating system's own URL handler
type Launcher struct {
	Args       []string // program and arguments, each %s standing for the URL: inside a shell's -c command string a quoted reference to the BROWSER_URL variable set for it, inside PowerShell code the URL as a quoted literal, elsewhere the URL itself within that one argument
	Terminal   bool     // a text browser running in this terminal: Open waits for it and its exit status decides
	Background bool     // a BROWSER entry ending in &: started and left running without waiting for an early failure
}

var (
	// Refusal of anything but an absolute http or https URL with a host, which could otherwise reach a command line as an option or make an opener run a local program
	ErrUnsafeURL = errors.New("not an absolute http or https url")
	// Failure of every launcher, or no launcher at all, to take the URL
	ErrNoBrowser = errors.New("no browser opened the url")
)

// How long a started browser command may take to fail before it counts as having taken the URL
const settle = time.Second

// Text browsers that need this terminal, named by program
var textBrowsers = map[string]bool{"w3m": true, "lynx": true, "links": true, "links2": true, "elinks": true,
	"www-browser": true, "browsh": true, "carbonyl": true}

// Shows an absolute http or https URL in the user's browser, trying each BROWSER entry and then the openers of this platform until one takes it; ErrUnsafeURL for anything else
func Open(ctx context.Context, target string) error {
	if !webURL(target) {
		return fmt.Errorf("%w: %q", ErrUnsafeURL, target)
	}
	var failures []string
	for _, l := range Launchers() {
		err := l.open(ctx, target)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		failures = append(failures, err.Error())
	}
	if len(failures) == 0 {
		return ErrNoBrowser
	}
	return fmt.Errorf("%w: %s", ErrNoBrowser, strings.Join(failures, "; "))
}

// Whether target is an absolute http or https URL with a host and no surrounding white space or control characters
func webURL(target string) bool {
	if target == "" || strings.TrimSpace(target) != target {
		return false
	}
	u, err := url.Parse(target)
	return err == nil && u.Host != "" && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https"))
}

// Launchers Open tries, in order: each entry of BROWSER, then the openers of this platform found on this host
func Launchers() []Launcher {
	var out []Launcher
	for entry := range strings.SplitSeq(os.Getenv("BROWSER"), string(os.PathListSeparator)) {
		if l, ok := parseEntry(entry); ok {
			out = append(out, l)
		}
	}
	return append(out, platformLaunchers()...)
}

// Launcher of one BROWSER entry: a program path named whole, else the words of a command line of this host (POSIX shell words, Windows command line words on Windows); the URL goes where %s is or last, a trailing & runs it in the background
func parseEntry(entry string) (Launcher, bool) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return Launcher{}, false
	}
	var args []string
	if st, err := os.Stat(entry); err == nil && st.Mode().IsRegular() {
		args = []string{entry}
	} else {
		words, err := shellutil.SplitCommandLine(entry)
		if err != nil {
			return Launcher{}, false
		}
		args = words
	}
	background := false
	if n := len(args); n > 0 && args[n-1] == "&" {
		background, args = true, args[:n-1]
	}
	if len(args) == 0 {
		return Launcher{}, false
	}
	if !slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "%s") }) {
		args = append(args, "%s")
	}
	return Launcher{Args: args, Terminal: !background && isTextBrowser(args[0]), Background: background}, true
}

// Whether a program is a text browser that takes over the terminal
func isTextBrowser(program string) bool {
	name := strings.ToLower(filepath.Base(program))
	return textBrowsers[strings.TrimSuffix(name, ".exe")]
}

// Runs the launcher for target: a terminal browser waited for, any other command started detached and watched briefly for an early failure
func (l Launcher) open(ctx context.Context, target string) error {
	if len(l.Args) == 0 {
		return systemOpen(target)
	}
	argv, env, err := l.command(target)
	if err != nil {
		return fmt.Errorf("%s: %w", l.Args[0], err)
	}
	var cmd *exec.Cmd
	if l.Terminal {
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	} else {
		cmd = exec.Command(argv[0], argv[1:]...)
		cmd.SysProcAttr = detachAttr()
	}
	if interpreterOf(cmd.Path) == cmdShell && strings.ContainsAny(target, cmdSyntax) {
		return fmt.Errorf("%s: %w", argv[0], errCmdSyntax)
	}
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	if l.Terminal {
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", argv[0], err)
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", argv[0], err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if l.Background {
		return nil
	}
	timer := time.NewTimer(settle)
	defer timer.Stop()
	select {
	case err := <-exited:
		if err != nil {
			return fmt.Errorf("%s: %w", argv[0], err)
		}
		return nil
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
