package auth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/shell"
)

var consoleBrowserNames = []string{"w3m", "lynx", "links", "links2", "elinks", "www-browser", "browsh"}

const (
	browserWaitLimit   = 300 * time.Second
	browserRemoteWait  = 5 * time.Second
	xdgSettingsTimeout = 10 * time.Second
)

type launchMode int

const (
	launchGeneric launchMode = iota
	launchBackground
	launchRemote
	launchTTYRemote
	launchKonqueror
	launchWindowsDefault
	launchOSAScript
	launchUnparsable
)

type browserCandidate struct {
	key        string
	name       string
	argv       []string
	remoteArgs []string
	mode       launchMode
}

type waitResult int

const (
	waitSucceeded waitResult = iota
	waitFailed
	waitTimedOut
	waitCancelled
)

func (s *Store) isRemoteSession(ctx context.Context) bool {
	session := hostinfo.ReadSession(func(name string) string { return s.lookupEnv(ctx, name) })
	return session.SSHClient || session.SSHTTY || session.RemoteConsole != ""
}

func (s *Store) IsRemoteSession(ctx context.Context) bool { return s.isRemoteSession(s.bind(ctx)) }

func namesConsoleBrowser(value string) bool {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return false
	}
	return slices.Contains(consoleBrowserNames, strings.ToLower(filepath.Base(fields[0])))
}

func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func namedBrowser(name string, mode launchMode, remoteArgs ...string) browserCandidate {
	return browserCandidate{key: name, name: name, argv: []string{name, "%s"}, remoteArgs: remoteArgs, mode: mode}
}

func (s *Store) unixBrowsers(ctx context.Context, env func(string) string) []browserCandidate {
	out := []browserCandidate{}
	display := env("DISPLAY") != "" || env("WAYLAND_DISPLAY") != ""
	preferred := ""
	if display {
		preferred = s.xdgDefaultBrowser(ctx)
	}
	register := func(c browserCandidate) {
		if preferred != "" && c.key+".desktop" == preferred {
			out = slices.Insert(out, 0, c)
			return
		}
		out = append(out, c)
	}
	if display {
		if onPath("xdg-open") {
			register(namedBrowser("xdg-open", launchBackground))
		}
		if onPath("gio") {
			register(browserCandidate{key: "gio", name: "gio", argv: []string{"gio", "open", "--", "%s"}, mode: launchBackground})
		}
		desktops := strings.Split(env("XDG_CURRENT_DESKTOP"), ":")
		if (slices.Contains(desktops, "GNOME") || env("GNOME_DESKTOP_SESSION_ID") != "") && onPath("gvfs-open") {
			register(namedBrowser("gvfs-open", launchBackground))
		}
		if (slices.Contains(desktops, "KDE") || env("KDE_FULL_SESSION") != "") && onPath("kfmclient") {
			register(namedBrowser("kfmclient", launchKonqueror))
		}
		if onPath("x-www-browser") {
			register(namedBrowser("x-www-browser", launchBackground))
		}
		for _, name := range []string{"firefox", "iceweasel", "seamonkey", "mozilla-firefox", "mozilla"} {
			if onPath(name) {
				register(namedBrowser(name, launchRemote, "%s"))
			}
		}
		if onPath("kfm") {
			register(namedBrowser("kfm", launchKonqueror))
		} else if onPath("konqueror") {
			register(namedBrowser("konqueror", launchKonqueror))
		}
		if onPath("epiphany") {
			register(namedBrowser("epiphany", launchRemote, "-n", "%s"))
		}
		for _, name := range []string{"google-chrome", "chrome", "chromium", "chromium-browser"} {
			if onPath(name) {
				register(namedBrowser(name, launchRemote, "%s"))
			}
		}
		if onPath("opera") {
			register(namedBrowser("opera", launchRemote, "%s"))
		}
		if onPath("microsoft-edge") {
			register(namedBrowser("microsoft-edge", launchRemote, "%s"))
		}
	}
	if env("TERM") != "" {
		for _, name := range []string{"www-browser", "links"} {
			if onPath(name) {
				register(namedBrowser(name, launchGeneric))
			}
		}
		if onPath("elinks") {
			register(namedBrowser("elinks", launchTTYRemote, "-remote", "openURL(%s)"))
		}
		for _, name := range []string{"lynx", "w3m"} {
			if onPath(name) {
				register(namedBrowser(name, launchGeneric))
			}
		}
	}
	return out
}

func (s *Store) xdgDefaultBrowser(ctx context.Context) string {
	path, err := exec.LookPath("xdg-settings")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, xdgSettingsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "get", "default-web-browser")
	cmd.Env = s.childEnv(ctx)
	raw, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func synthesizedBrowser(entry string, standard []browserCandidate) (browserCandidate, bool) {
	fields := strings.Fields(entry)
	if len(fields) == 0 || !onPath(fields[0]) {
		return browserCandidate{}, false
	}
	name := strings.ToLower(filepath.Base(fields[0]))
	for _, c := range standard {
		if strings.ToLower(c.key) == name && strings.ToLower(filepath.Base(c.name)) == name {
			clone := c
			clone.key, clone.name = entry, entry
			clone.argv = append([]string{entry}, c.argv[1:]...)
			return clone, true
		}
	}
	return browserCandidate{}, false
}

func envBrowser(entry string, standard []browserCandidate) browserCandidate {
	if strings.Contains(entry, "%s") {
		argv, err := shell.Split(entry)
		if err != nil || len(argv) == 0 {
			return browserCandidate{key: entry, name: entry, mode: launchUnparsable}
		}
		if argv[len(argv)-1] == "&" {
			return browserCandidate{key: entry, name: argv[0], argv: argv[:len(argv)-1], mode: launchBackground}
		}
		return browserCandidate{key: entry, name: argv[0], argv: argv, mode: launchGeneric}
	}
	if clone, ok := synthesizedBrowser(entry, standard); ok {
		return clone
	}
	return browserCandidate{key: entry, name: entry, argv: []string{entry, "%s"}, mode: launchGeneric}
}

func (s *Store) browserCandidates(ctx context.Context) []browserCandidate {
	env := func(name string) string { return s.lookupEnv(ctx, name) }
	standard := platformBrowsers(env)
	if runtime.GOOS != "windows" {
		standard = append(standard, s.unixBrowsers(ctx, env)...)
	}
	chosen := []browserCandidate{}
	if browserEnv := env("BROWSER"); browserEnv != "" {
		for _, entry := range strings.Split(browserEnv, string(os.PathListSeparator)) {
			if strings.TrimSpace(entry) != "" {
				chosen = append(chosen, envBrowser(entry, standard))
			}
		}
	}
	return append(chosen, standard...)
}

func (s *Store) canOpenGraphicalBrowser(ctx context.Context) bool {
	browserEnv := s.lookupEnv(ctx, "BROWSER")
	if browserEnv != "" && namesConsoleBrowser(browserEnv) {
		return false
	}
	if runtime.GOOS == "linux" {
		hasDisplay := s.lookupEnv(ctx, "DISPLAY") != "" || s.lookupEnv(ctx, "WAYLAND_DISPLAY") != ""
		if !hasDisplay && browserEnv == "" {
			return false
		}
	}
	candidates := s.browserCandidates(ctx)
	if len(candidates) == 0 || candidates[0].mode == launchUnparsable {
		return false
	}
	return !namesConsoleBrowser(candidates[0].name)
}

func (s *Store) CanOpenGraphicalBrowser(ctx context.Context) bool {
	return s.canOpenGraphicalBrowser(s.bind(ctx))
}

func substituteURL(argv []string, target string) []string {
	out := []string{argv[0]}
	for _, a := range argv[1:] {
		out = append(out, strings.ReplaceAll(a, "%s", target))
	}
	return out
}

func (s *Store) browserCommand(ctx context.Context, argv []string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = s.childEnv(ctx)
	return cmd
}

func waitBounded(ctx context.Context, cmd *exec.Cmd, limit time.Duration) waitResult {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return waitFailed
		}
		return waitSucceeded
	case <-timer.C:
		return waitTimedOut
	case <-ctx.Done():
		return waitCancelled
	}
}

func (s *Store) runAttached(ctx context.Context, argv []string) bool {
	cmd := s.browserCommand(ctx, argv)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return false
	}
	result := waitBounded(ctx, cmd, browserWaitLimit)
	return result == waitSucceeded || result == waitTimedOut
}

func (s *Store) startInBackground(ctx context.Context, argv []string, inheritStdio bool) bool {
	cmd := s.browserCommand(ctx, argv)
	if inheritStdio {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	}
	return proc.StartDetached(cmd) == nil
}

func (s *Store) invokeRemote(ctx context.Context, c browserCandidate, target string) bool {
	cmd := s.browserCommand(ctx, substituteURL(append([]string{c.argv[0]}, c.remoteArgs...), target))
	if c.mode == launchTTYRemote {
		cmd.Stdout = os.Stdout
	}
	browserSession(cmd)
	if err := cmd.Start(); err == nil {
		switch waitBounded(ctx, cmd, browserRemoteWait) {
		case waitSucceeded, waitTimedOut:
			return true
		case waitCancelled:
			return false
		}
	}
	straight := substituteURL(c.argv, target)
	if c.mode == launchTTYRemote {
		return s.runAttached(ctx, straight)
	}
	return s.startInBackground(ctx, straight, false)
}

func (s *Store) openKonqueror(ctx context.Context, target string) bool {
	if cmd := s.browserCommand(ctx, []string{"kfmclient", "openURL", target}); cmd.Start() == nil {
		return waitBounded(ctx, cmd, browserWaitLimit) != waitCancelled
	}
	if onPath("konqueror") && s.startInBackground(ctx, []string{"konqueror", "--silent", target}, false) {
		return true
	}
	return onPath("kfm") && s.startInBackground(ctx, []string{"kfm", "-d", target}, false)
}

func (s *Store) runOSAScript(ctx context.Context, app, target string) bool {
	target = strings.ReplaceAll(target, `"`, "%22")
	script := `open location "` + target + `"`
	if app != "default" {
		script = "\n   tell application \"" + app + "\"\n       activate\n       open location \"" + target + "\"\n   end\n   "
	}
	cmd := s.browserCommand(ctx, []string{"osascript"})
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		return false
	}
	result := waitBounded(ctx, cmd, browserWaitLimit)
	return result == waitSucceeded || result == waitTimedOut
}

func (s *Store) launchBrowser(ctx context.Context, c browserCandidate, target string) bool {
	switch c.mode {
	case launchWindowsDefault:
		return openNative(target)
	case launchOSAScript:
		return s.runOSAScript(ctx, c.argv[0], target)
	case launchKonqueror:
		return s.openKonqueror(ctx, target)
	case launchRemote, launchTTYRemote:
		return s.invokeRemote(ctx, c, target)
	case launchBackground:
		return s.startInBackground(ctx, substituteURL(c.argv, target), true)
	}
	return s.runAttached(ctx, substituteURL(c.argv, target))
}

func (s *Store) tryOpenBrowser(ctx context.Context, target string) (bool, error) {
	for _, c := range s.browserCandidates(ctx) {
		if c.mode == launchUnparsable {
			return false, errors.New("auth: the BROWSER command line cannot be parsed")
		}
		if s.launchBrowser(ctx, c, target) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) openBrowser(ctx context.Context, target string) bool {
	opened, err := s.tryOpenBrowser(ctx, target)
	return opened && err == nil
}

func (s *Store) OpenBrowser(ctx context.Context, target string) bool {
	return s.openBrowser(s.bind(ctx), target)
}
