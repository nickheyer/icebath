package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/browser"
	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/proc"
)

const (
	// Debugging port of a browser started for /browser connect
	DefaultPort = 9222
	DefaultURL  = "http://127.0.0.1:9222"
	stderrLog   = "launch-stderr.log"
	stderrTail  = 2000
)

// Every Chromium program of the machine that could be started with a debugging port, in the order tried
func debugCandidates() []string {
	var out []string
	which := func(name string) string {
		found, err := exec.LookPath(name)
		if err != nil {
			return ""
		}
		return found
	}
	bases := []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")}
	for _, b := range products {
		switch runtime.GOOS {
		case "darwin":
			out = append(out, b.macApp)
		case "windows":
			for _, name := range b.winBins {
				out = append(out, which(name))
			}
			for _, base := range bases {
				if base == "" {
					continue
				}
				for _, parts := range b.winInstall {
					out = append(out, filepath.Join(append([]string{base}, parts...)...))
				}
			}
		default:
			for _, name := range b.linuxBins {
				out = append(out, which(name))
			}
			out = append(out, b.linuxPaths...)
		}
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		for _, b := range products {
			for _, base := range []string{"/mnt/c/Program Files", "/mnt/c/Program Files (x86)"} {
				for _, parts := range b.winInstall {
					out = append(out, filepath.Join(append([]string{base}, parts...)...))
				}
			}
		}
	}
	seen := map[string]bool{}
	var found []string
	for _, p := range out {
		if p == "" {
			continue
		}
		key := fold(p)
		if seen[key] || !fsx.IsFile(p) {
			continue
		}
		seen[key] = true
		found = append(found, p)
	}
	return found
}

// Profile folder of the debug browser, kept apart from the user's own
func debugDir(cfg *config.Config) string { return filepath.Join(profileDir(cfg), "chrome-debug") }

func debugArgs(cfg *config.Config, port int) []string {
	return append([]string{fmt.Sprintf("--remote-debugging-port=%d", port), "--user-data-dir=" + debugDir(cfg)}, browser.QuietArgs()...)
}

func bindable(network, host string, port int) bool {
	lc := net.ListenConfig{Control: reuseAddr}
	ln, err := lc.Listen(context.Background(), network, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// First port after the one wanted that both loopbacks can bind, the next one when none can
func freeDebugPort(preferred, attempts int) int {
	for p := preferred + 1; p < preferred+1+attempts; p++ {
		if bindable("tcp4", "127.0.0.1", p) && bindable("tcp6", "::1", p) {
			return p
		}
	}
	return preferred + 1
}

// Command a person runs to start a debuggable browser themselves, empty when there is no browser to name
func manualCommand(cfg *config.Config, port int) string {
	if candidates := debugCandidates(); len(candidates) > 0 {
		argv := append([]string{candidates[0]}, debugArgs(cfg, port)...)
		if runtime.GOOS == "windows" {
			quoted := make([]string, len(argv))
			for i, a := range argv {
				quoted[i] = windowsArg(a)
			}
			return strings.Join(quoted, " ")
		}
		return terminal.Join(argv)
	}
	if runtime.GOOS == "darwin" {
		return `open -a "Google Chrome" --args ` + terminal.Join(debugArgs(cfg, port))
	}
	return ""
}

// One argument quoted the way a Windows program reads its command line
func windowsArg(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\n\v\"") {
		return a
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, c := range a {
		switch c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes*2+1))
			b.WriteRune(c)
			slashes = 0
			continue
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			slashes = 0
			b.WriteRune(c)
			continue
		}
	}
	b.WriteString(strings.Repeat(`\`, slashes*2))
	b.WriteByte('"')
	return b.String()
}

// One launch tried: the program, how it ended up, its exit code and what it last wrote
type attempt struct {
	binary, state string
	code          *int
	tail          string
}

// Why the launch left no debuggable browser, empty when nothing more is known
func hint(attempts []*attempt) string {
	for _, a := range attempts {
		if a.state == "exited" && a.code != nil && *a.code == 0 {
			name := filepath.Base(a.binary)
			return name + " closed at once without opening its debugging port, most likely because a copy of it was already running. Close every " +
				name + " window, including any left running in the background, then connect again."
		}
	}
	for _, a := range attempts {
		if a.state == "exited" && a.tail != "" {
			lines := strings.Split(a.tail, "\n")
			return filepath.Base(a.binary) + " closed before its debugging port opened. It last printed: " + strings.TrimSpace(lines[len(lines)-1])
		}
	}
	return ""
}

func readTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > stderrTail {
		data = data[len(data)-stderrTail:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(data), "�"))
}

// Starts each Chromium of the machine with the debugging port until one keeps running; whether one did, and every attempt
func launchDebug(ctx context.Context, cfg *config.Config, port int) (bool, []*attempt) {
	dir := debugDir(cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Info(fmt.Sprintf("browser debug launch: could not create %s: %s", dir, err))
		return false, nil
	}
	candidates := debugCandidates()
	if len(candidates) == 0 {
		slog.Info(fmt.Sprintf("browser debug launch: no Chromium-family binary found (system=%s)", runtime.GOOS))
		return false, nil
	}
	logPath := filepath.Join(dir, stderrLog)
	var attempts []*attempt
	for _, candidate := range candidates {
		cmd, err := spawnDebug(cfg, candidate, port, logPath)
		if err != nil {
			attempts = append(attempts, &attempt{binary: candidate, state: "spawn-failed"})
			slog.Info(fmt.Sprintf("browser debug launch: failed to spawn %s: %s", candidate, err))
			continue
		}
		slog.Info(fmt.Sprintf("browser debug launch: spawned %s (pid=%d) with --remote-debugging-port=%d", candidate, cmd.Process.Pid, port))
		exited := make(chan struct{})
		var code int
		go func() {
			cmd.Wait()
			code = proc.ExitCode(cmd.ProcessState)
			close(exited)
		}()
		state := "starting"
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
			if discoverLocal(ctx, port, 100*time.Millisecond) != "" {
				state = "ready"
				break
			}
			select {
			case <-exited:
				state = "exited"
			case <-time.After(100 * time.Millisecond):
			}
			if state == "exited" {
				break
			}
		}
		a := &attempt{binary: candidate, state: state}
		attempts = append(attempts, a)
		if state != "exited" {
			return true, attempts
		}
		<-exited
		a.code, a.tail = &code, readTail(logPath)
		tail := ""
		if a.tail != "" {
			tail = "; stderr tail: " + a.tail
		}
		slog.Warn(fmt.Sprintf("browser debug launch: %s exited (code=%d) before port %d opened%s", candidate, code, port, tail))
	}
	return false, attempts
}

// Starts one program detached, its output dropped and its errors in the launch log
func spawnDebug(cfg *config.Config, program string, port int, logPath string) (*exec.Cmd, error) {
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cmd := exec.Command(program, debugArgs(cfg, port)...)
	cmd.Stderr = f
	cmd.SysProcAttr = debugDetached()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// Whether the address is the local default, found over discovery, never a concrete websocket
func defaultLocal(u *url.URL) bool {
	port := u.Port()
	if port == "" {
		port = "80"
	}
	host := strings.ToLower(u.Hostname())
	return (u.Scheme == "http" || u.Scheme == "ws") && (host == "127.0.0.1" || host == "localhost") && port == "9222" &&
		(u.Path == "" || u.Path == "/" || u.Path == "/json" || u.Path == "/json/version")
}

// Whether /json/version or /json of the host answers
func httpReachable(u *url.URL, timeout time.Duration) bool {
	scheme := map[string]string{"ws": "http", "wss": "https"}[u.Scheme]
	if scheme == "" {
		scheme = u.Scheme
	}
	root := (&url.URL{Scheme: scheme, User: u.User, Host: u.Host}).String()
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: loopbackProxy}}
	for _, target := range []string{root + "/json/version", root + "/json"} {
		resp, err := client.Get(target)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return true
		}
	}
	return false
}

// Finds or launches the default local debug browser, telling each step; empty when none answers
func (b *Browser) localDefault(ctx context.Context, cfg *config.Config, port int, say func(message, level string)) string {
	if found := discoverLocal(ctx, port, 2*time.Second); found != "" {
		say("Found a browser with remote debugging at "+found+".", "info")
		return found
	}
	launch := port
	if portInUse(port, 500*time.Millisecond) {
		launch = freeDebugPort(port, 10)
		say(fmt.Sprintf("Another program uses port %d, such as a code editor's debugger or a development server. Starting a browser with remote "+
			"debugging on port %d instead.", port, launch), "info")
	} else {
		say("No browser with remote debugging is running, so icebath is starting one.", "info")
	}
	launched, attempts := launchDebug(ctx, cfg, launch)
	found := ""
	if launched {
		for deadline := time.Now().Add(10 * time.Second); found == "" && time.Now().Before(deadline); {
			if found = discoverLocal(ctx, launch, time.Second); found == "" {
				select {
				case <-ctx.Done():
					return ""
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
	}
	if found != "" {
		say(fmt.Sprintf("Started a browser with remote debugging on port %d.", launch), "info")
		return found
	}
	if h := hint(attempts); h != "" {
		say(h, "error")
	}
	var lines []string
	if command := manualCommand(cfg, launch); command != "" {
		lines = []string{"Start a browser with remote debugging with this command, then connect again:", command}
	} else {
		lines = []string{"No Chrome, Edge, Brave or other Chromium browser was found on this computer.",
			fmt.Sprintf("Install one, or start one with --remote-debugging-port=%d, then connect again.", launch)}
	}
	lines = append(lines, "The browser is not connected yet.")
	for _, line := range lines {
		say(line, "error")
	}
	return ""
}

// Error of an address /browser connect refuses, for the person to fix
type BadAddress struct{ Message string }

func (e *BadAddress) Error() string { return e.Message }

// Checks, reaches or launches the browser at the address and attaches the profile's browser tools to it, telling each step; whether they
// are attached and the endpoint, or the one tried when no browser answered
func (b *Browser) Connect(ctx context.Context, cfg *config.Config, address string, say func(message, level string)) (bool, string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		address = DefaultURL
	}
	target := address
	if !strings.Contains(address, "://") {
		target = "http://" + address
	}
	u, err := url.Parse(target)
	if err != nil {
		return false, "", &BadAddress{"“" + address + "” is not an address. Give one such as " + DefaultURL + "."}
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
	default:
		return false, "", &BadAddress{"“" + address + "” is not a browser address. It must start with http://, https://, ws:// or wss://."}
	}
	if u.Hostname() == "" {
		return false, "", &BadAddress{"“" + address + "” names no computer. Give one such as " + DefaultURL + "."}
	}
	port := 80
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil || port <= 0 || port > 65535 {
			return false, "", &BadAddress{"The port in “" + address + "” is not a number from 1 to 65,535."}
		}
	} else if u.Scheme == "https" || u.Scheme == "wss" {
		port = 443
	}
	local := defaultLocal(u)
	if local {
		address, port = DefaultURL, DefaultPort
		u, _ = url.Parse(address)
	}
	switch {
	case (u.Scheme == "ws" || u.Scheme == "wss") && strings.HasPrefix(u.Path, "/devtools/browser/"):
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(u.Hostname(), strconv.Itoa(port)), 2*time.Second)
		if err != nil {
			slog.Debug("browser connect: dial failed", "address", redactEndpoint(address), "error", err)
			return false, "", fmt.Errorf("Could not reach a browser at %s. Check that it is running with remote debugging on and that the address is "+
				"right.", redactEndpoint(address))
		}
		conn.Close()
	case local:
		found := b.localDefault(ctx, cfg, port, say)
		if found == "" {
			return false, address, nil
		}
		address = found
		u, _ = url.Parse(found)
	case !httpReachable(u, 2*time.Second):
		return false, "", errors.New("Could not reach a browser at " + redactEndpoint(address) + ". Check that it is running with remote debugging " +
			"on and that the address is right.")
	}
	normalized := u.String()
	if !strings.HasPrefix(u.Path, "/devtools/browser/") {
		bare := *u
		bare.Path, bare.RawPath, bare.RawQuery, bare.ForceQuery, bare.Fragment, bare.RawFragment = "", "", "", false, "", ""
		normalized = bare.String()
	}
	b.Attach(ctx, cfg, normalized)
	if err := cfg.Set(browserconfig.CDPURL, normalized); err != nil {
		return true, normalized, fmt.Errorf("Connected for now, but the address was not kept for later sessions. %w", err)
	}
	return true, normalized, nil
}

// Detaches the profile's browser tools from the browser they were attached to and forgets its address, answering what happened; held when
// an environment variable still names an address, which wins
func (b *Browser) Disconnect(ctx context.Context, cfg *config.Config) (string, bool, error) {
	b.Attach(ctx, cfg, "")
	if err := cfg.Set(browserconfig.CDPURL, nil); err != nil {
		return "", false, err
	}
	if variable := b.Holder(cfg); variable != "" {
		return "Disconnected, but " + variable + " still names a browser, which the agent uses while it is set.", true, nil
	}
	return "Disconnected. The agent opens a browser of its own again.", false, nil
}

// Environment variable that names the address of the browser over the settings, empty when none does
func (b *Browser) Holder(cfg *config.Config) string {
	_, setting, ok := cfg.Setting(browserconfig.CDPURL)
	if !ok {
		return ""
	}
	if source, variable := cfg.Source(setting); source == config.FromEnvironment {
		return variable
	}
	return ""
}
