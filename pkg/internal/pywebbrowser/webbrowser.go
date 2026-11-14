// Package pywebbrowser opens URLs the way the CPython 3.14 webbrowser module does
package pywebbrowser

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Failure webbrowser.Error reports, no runnable browser or a bad new value
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Python exception class of the failure
func (e *Error) PyTypeName() string { return "Error" }

// Refusal of a url whose first non blank character is a dash
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// Python exception class of the refusal
func (e *ValueError) PyTypeName() string { return "ValueError" }

// Failure of a blank BROWSER entry whose first word is read
type IndexError struct{ Msg string }

func (e *IndexError) Error() string { return e.Msg }

// Python exception class of the failure
func (e *IndexError) PyTypeName() string { return "IndexError" }

// Launch strategy of one browser controller
type kind int

const (
	kindGeneric kind = iota
	kindBackground
	kindUnix
	kindKonqueror
	kindPlatform
)

// One browser controller with the class attributes of its Python launcher class
type browser struct {
	kind               kind
	name               string
	basename           string
	args               []string
	raiseOpts          []string
	background         bool
	redirectStdout     bool
	remoteArgs         []string
	remoteAction       string
	remoteActionNewwin string
	remoteActionNewtab string
	hasNewtab          bool
	// Launcher of a controller that drives the operating system instead of a command line
	launch func(b *browser, url string) (bool, error)
}

// Registered class and instance of one browser name
type entry struct {
	klass    func() *browser
	instance *browser
}

var (
	lock        sync.Mutex
	browsers    = map[string]entry{}
	tryorder    []string
	registered  bool
	osPreferred string
)

// Name and basename of a resolved controller
type Controller struct {
	Name     string
	Basename string
}

// Controller started with a command line and waited on
func newGeneric(name string) *browser {
	return &browser{kind: kindGeneric, name: name, args: []string{"%s"}, basename: pypath.Basename(name)}
}

// Controller started with a split command line whose first word is the program
func newGenericList(kind kind, cmdline []string) *browser {
	return &browser{kind: kind, name: cmdline[0], args: cmdline[1:], basename: pypath.Basename(cmdline[0])}
}

// Controller started detached and reported open while it is still running
func newBackground(name string) *browser {
	b := newGeneric(name)
	b.kind = kindBackground
	return b
}

// Remote capable controller with the defaults of the Unix parent class
func newUnix(name string) *browser {
	return &browser{kind: kindUnix, name: name, basename: name, args: []string{"%s"}, redirectStdout: true,
		remoteArgs: []string{"%action", "%s"}}
}

// Chrome, Chromium, Opera and Edge launcher
func newChrome(name string) *browser {
	b := newUnix(name)
	b.remoteActionNewwin, b.hasNewtab, b.background = "--new-window", true, true
	return b
}

// Records a controller under its lowered name, preferred ones and the desktop default first in the try order
func register(name string, klass func() *browser, instance *browser, preferred bool) {
	browsers[pystr.Lower(name)] = entry{klass: klass, instance: instance}
	if preferred || (osPreferred != "" && name+".desktop" == osPreferred) {
		tryorder = append([]string{name}, tryorder...)
		return
	}
	tryorder = append(tryorder, name)
}

// Clone of a registered controller renamed to a BROWSER entry that names its executable
func synthesize(name string, preferred bool) (entry, error) {
	words := pystr.SplitWhitespace(name, -1)
	if len(words) == 0 {
		return entry{}, &IndexError{Msg: "list index out of range"}
	}
	cmd := words[0]
	if _, ok := pyshutil.Which(cmd, nil); !ok {
		return entry{}, nil
	}
	base := pypath.Basename(cmd)
	command, ok := browsers[pystr.Lower(base)]
	if !ok {
		return entry{}, nil
	}
	if command.instance != nil && pystr.Lower(base) == command.instance.basename {
		controller := *command.instance
		controller.name, controller.basename = name, pypath.Basename(name)
		register(name, nil, &controller, preferred)
		return entry{instance: &controller}, nil
	}
	return entry{}, nil
}

// Controller of one try order name, a command line with a url slot split into its own launcher
func get(name string) (*browser, error) {
	if strings.Contains(name, "%s") {
		parts, err := pyshlex.SplitPOSIX(name, false)
		if err != nil {
			return nil, err
		}
		if parts[len(parts)-1] == "&" {
			return newGenericList(kindBackground, parts[:len(parts)-1]), nil
		}
		return newGenericList(kindGeneric, parts), nil
	}
	command, ok := browsers[pystr.Lower(name)]
	if !ok {
		var err error
		if command, err = synthesize(name, false); err != nil {
			return nil, err
		}
	}
	if command.instance != nil {
		return command.instance, nil
	}
	if command.klass != nil {
		return command.klass(), nil
	}
	return nil, nil
}

// Fills the registry from the platform browsers and then the BROWSER variable on first use
func ensureRegistered() error {
	if registered {
		return nil
	}
	registered = true
	registerPlatformBrowsers()
	value, ok := os.LookupEnv("BROWSER")
	if !ok {
		return nil
	}
	choices := strings.Split(value, string(os.PathListSeparator))
	for i := len(choices) - 1; i >= 0; i-- {
		cmdline := choices[i]
		if !strings.ContainsAny(cmdline, " \t") {
			if command, known := browsers[pystr.Lower(cmdline)]; known {
				if command.instance == nil || (command.instance.kind != kindGeneric && command.instance.kind != kindBackground) {
					tryorder = append([]string{pystr.Lower(cmdline)}, tryorder...)
					continue
				}
			}
		}
		if cmdline == "" {
			continue
		}
		cmd, err := synthesize(cmdline, true)
		if err != nil {
			return err
		}
		if cmd.instance == nil {
			register(cmdline, nil, newGeneric(cmdline), true)
		}
	}
	return nil
}

// First controller of the try order that resolves, an Error when none does
func Get() (Controller, error) {
	lock.Lock()
	defer lock.Unlock()
	if err := ensureRegistered(); err != nil {
		return Controller{}, err
	}
	for _, name := range tryorder {
		b, err := get(name)
		if err != nil {
			return Controller{}, err
		}
		if b != nil {
			return Controller{Name: b.name, Basename: b.basename}, nil
		}
	}
	return Controller{}, &Error{Msg: "could not locate runnable browser"}
}

// Shows the url in the first browser of the try order that accepts it, new 0 same window, 1 window, 2 tab
func Open(url string, new int) (bool, error) {
	lock.Lock()
	if err := ensureRegistered(); err != nil {
		lock.Unlock()
		return false, err
	}
	order := append([]string(nil), tryorder...)
	lock.Unlock()
	for _, name := range order {
		lock.Lock()
		b, err := get(name)
		lock.Unlock()
		if err != nil {
			return false, err
		}
		if b == nil {
			return false, &Error{Msg: "could not locate runnable browser"}
		}
		opened, err := b.open(url, new, true)
		if err != nil {
			return false, err
		}
		if opened {
			return true, nil
		}
	}
	return false, nil
}

// Refuses a url that a browser command line would read as an option
func checkURL(url string) error {
	if url != "" && strings.HasPrefix(pystr.Lstrip(url), "-") {
		return &ValueError{Msg: "Invalid URL (leading dash disallowed): " + pyrepr.StrRepr(url)}
	}
	return nil
}

// Program followed by its arguments with every url slot filled
func (b *browser) cmdline(url string) []string {
	out := []string{b.name}
	for _, arg := range b.args {
		out = append(out, strings.ReplaceAll(arg, "%s", url))
	}
	return out
}

// True while a started process has not exited, the process reaped once it does
func stillRunning(cmd *exec.Cmd) bool {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// Command over the inherited terminal or the null device
func command(cmdline []string, inherit, inheritStdout, session bool) *exec.Cmd {
	cmd := exec.Command(cmdline[0], cmdline[1:]...)
	if inherit {
		cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	}
	if inheritStdout {
		cmd.Stdout = os.Stdout
	}
	if session {
		cmd.SysProcAttr = sessionAttr
	}
	return cmd
}

// Hands the url to this controller, true when its browser took it
func (b *browser) open(url string, new int, autoraise bool) (bool, error) {
	if b.kind == kindUnix {
		return b.openUnix(url, new, autoraise)
	}
	cmdline := b.cmdline(url)
	if err := checkURL(url); err != nil {
		return false, err
	}
	switch b.kind {
	case kindGeneric:
		return command(cmdline, true, true, false).Run() == nil, nil
	case kindBackground:
		cmd := command(cmdline, true, true, true)
		if cmd.Start() != nil {
			return false, nil
		}
		return stillRunning(cmd), nil
	case kindKonqueror:
		return openKonqueror(url, new), nil
	}
	return b.launch(b, url)
}

// Remote invocation of a running browser first, a direct launch when that fails
func (b *browser) openUnix(url string, new int, autoraise bool) (bool, error) {
	var action string
	switch new {
	case 0:
		action = b.remoteAction
	case 1:
		action = b.remoteActionNewwin
	case 2:
		action = b.remoteActionNewwin
		if b.hasNewtab {
			action = b.remoteActionNewtab
		}
	default:
		return false, &Error{Msg: fmt.Sprintf("Bad 'new' parameter to open(); expected 0, 1, or 2, got %d", new)}
	}
	if err := checkURL(strings.ReplaceAll(url, "%action", action)); err != nil {
		return false, err
	}
	var args []string
	for _, arg := range b.remoteArgs {
		if arg = strings.ReplaceAll(strings.ReplaceAll(arg, "%action", action), "%s", url); arg != "" {
			args = append(args, arg)
		}
	}
	success, err := b.invoke(args, true, autoraise)
	if err != nil || success {
		return success, err
	}
	return b.invoke(b.cmdline(url)[1:], false, false)
}

// Runs the browser, a remote call given five seconds and a background one only checked for a live process
func (b *browser) invoke(args []string, remote, autoraise bool) (bool, error) {
	cmdline := []string{b.name}
	if remote && len(b.raiseOpts) > 0 {
		index := 0
		if autoraise {
			index = 1
		}
		if opt := b.raiseOpts[index]; opt != "" {
			cmdline = append(cmdline, opt)
		}
	}
	cmdline = append(cmdline, args...)
	inherit := !(remote || b.background)
	cmd := command(cmdline, inherit, inherit || !b.redirectStdout, true)
	if err := cmd.Start(); err != nil {
		return false, err
	}
	if remote {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			return err == nil, nil
		case <-time.After(5 * time.Second):
			return true, nil
		}
	}
	if b.background {
		return stillRunning(cmd), nil
	}
	return cmd.Wait() == nil, nil
}

// kfmclient first, then konqueror, then kfm, each over the null device
func openKonqueror(url string, new int) bool {
	action := "openURL"
	if new == 2 {
		action = "newTab"
	}
	if cmd := command([]string{"kfmclient", action, url}, false, false, false); cmd.Start() == nil {
		_ = cmd.Wait()
		return true
	}
	if cmd := command([]string{"konqueror", "--silent", url}, false, false, true); cmd.Start() == nil {
		if stillRunning(cmd) {
			return true
		}
	}
	cmd := command([]string{"kfm", "-d", url}, false, false, true)
	if cmd.Start() != nil {
		return false
	}
	return stillRunning(cmd)
}
