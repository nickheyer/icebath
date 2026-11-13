package connections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/git"
)

const (
	sshDefaultConnectTimeout = 15 * time.Second
	sshDefaultExecTimeout    = 20 * time.Second
	sshDefaultForwardTimeout = 15 * time.Second
	// Remote watchdog seconds for probe commands, under the local exec timeout
	RemoteProbeTimeoutSecs = 15
	sshTunnelRestartLimit  = 5
	sshTunnelRestartDelay  = time.Second
	// ControlPersist seconds of every mux master
	ControlPersistSeconds = 300
	// Interval that refreshes a mux master while it carries forwards
	ControlForwardKeepalive = 60 * time.Second
	tunnelStopTimeout       = 5 * time.Second
	tunnelStderrCap         = 16384
)

// Classified ssh failure kinds the renderer shows its own copy for
const (
	SSHErrorUnreachable     = "unreachable"
	SSHErrorAuthFailed      = "auth-failed"
	SSHErrorInteractiveAuth = "interactive-auth"
	SSHErrorHostKeyChanged  = "host-key-changed"
	SSHErrorTimeout         = "timeout"
	SSHErrorUnknown         = "unknown"
	SSHErrorSuperseded      = "superseded"
)

// SSHError is an ssh failure with its kind, the close signal and the tunnel stderr it came from
type SSHError struct {
	Message      string
	ErrKind      string
	StderrKind   string
	Signal       string
	TunnelStderr string
	Code         string
	Profile      string
	Cause        error
	CleanupCause error
}

func (e *SSHError) Error() string { return e.Message }

// Cause and the cleanup failure that followed it, both reachable for diagnostics
func (e *SSHError) Unwrap() []error {
	var out []error
	for _, c := range []error{e.Cause, e.CleanupCause} {
		if c != nil {
			out = append(out, c)
		}
	}
	return out
}

// Kind is the classified failure kind
func (e *SSHError) Kind() string { return e.ErrKind }

func supersededSSHError(message string) *SSHError {
	return &SSHError{Message: message, ErrKind: SSHErrorSuperseded}
}

// Kind of an error that carries one directly, empty otherwise
func errKind(err error) string {
	if k, ok := err.(interface{ Kind() string }); ok {
		return k.Kind()
	}
	return ""
}

// Spawn failure text and code the way Node child_process reports them
func nodeSpawnError(command string, err error) *SSHError {
	code := ""
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		code = "ENOENT"
	case errors.Is(err, fs.ErrPermission):
		code = "EACCES"
	}
	if code == "" {
		return &SSHError{Message: err.Error(), Cause: err}
	}
	return &SSHError{Message: "spawn " + command + " " + code, Code: code, Cause: err}
}

const jsSpaceClass = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

var (
	sshControlCharRE = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	sshHostnameRE    = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	sshIPv6RE        = regexp.MustCompile(`^[0-9A-Fa-f:.]+(?:%[A-Za-z0-9._-]+)?$`)
	jsSpaceRE        = regexp.MustCompile(`[` + jsSpaceClass + `]`)
	sshUserBadRE     = regexp.MustCompile(`[` + jsSpaceClass + `@]`)
)

// Refuses a host, user or port that could inject ssh options or leak a pasted secret
func ValidateSSHTarget(host, user string, port float64) error {
	if host == "" {
		return errors.New("Unsafe SSH target: host is required.")
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("Unsafe SSH target: host must not start with a dash (\"%s\").", host)
	}
	if sshControlCharRE.MatchString(host) {
		return errors.New("Unsafe SSH target: host contains control characters.")
	}
	if jsSpaceRE.MatchString(host) {
		return errors.New(`Invalid SSH host: contains whitespace. Enter only the destination (user@host or host) — no "ssh " prefix or extra options.`)
	}
	if strings.Contains(host, ",") {
		return fmt.Errorf("Invalid SSH host \"%s\": commas are not valid in a hostname or IP (use dots, e.g. 192.168.1.10).", host)
	}
	if strings.Contains(host, ":") {
		if strings.Count(host, ":") < 2 || !sshIPv6RE.MatchString(host) {
			return fmt.Errorf("Invalid SSH host \"%s:<hidden>\": unexpected \":\" segment. Use host or host:port — and never put a password in the host field; Desktop SSH authenticates with keys.", strings.Split(host, ":")[0])
		}
	} else if !sshHostnameRE.MatchString(host) {
		return fmt.Errorf("Invalid SSH host \"%s\": not a valid hostname or IP address.", RedactSecrets(host))
	}
	if user != "" && sshControlCharRE.MatchString(user) {
		return errors.New("Unsafe SSH target: user contains control characters.")
	}
	if user != "" && strings.HasPrefix(user, "-") {
		return fmt.Errorf("Unsafe SSH target: user must not start with a dash (\"%s\").", user)
	}
	if user != "" && sshUserBadRE.MatchString(user) {
		return fmt.Errorf("Invalid SSH user \"%s\": contains whitespace or \"@\". Enter only the destination (user@host) — no \"ssh \" prefix.", user)
	}
	if port != float64(int64(port)) || port < 1 || port > 65535 {
		return fmt.Errorf("Unsafe SSH port: %s (must be 1-65535).", jsNumber(port))
	}
	return nil
}

// Refuses a key path that could inject ssh options
func ValidateKeyPath(keyPath string) error {
	if keyPath == "" {
		return nil
	}
	if sshControlCharRE.MatchString(keyPath) {
		return errors.New("Unsafe SSH key path: contains control characters.")
	}
	if strings.HasPrefix(keyPath, "-") {
		return fmt.Errorf("Unsafe SSH key path: must not start with a dash (\"%s\").", keyPath)
	}
	return nil
}

var sshRedactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(ICEBATH_DASHBOARD_SESSION_TOKEN=)([^` + jsSpaceClass + `]+)`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)(X-(?:Icebath|Hermes)-Session-Token["']?[` + jsSpaceClass + `]*[:=][` + jsSpaceClass + `]*["']?)([^` + jsSpaceClass + `"'&]+)`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)(Authorization["']?[` + jsSpaceClass + `]*:[` + jsSpaceClass + `]*Bearer[` + jsSpaceClass + `]+)([^` + jsSpaceClass + `]+)`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)([?&](?:token|ticket)=)([^` + jsSpaceClass + `&"']+)`), "${1}<redacted>"},
}

// Masks session tokens, bearer headers, URL tickets and a password typed where an SSH port belongs
func RedactSecrets(text string) string {
	out := text
	for _, r := range sshRedactions {
		out = r.re.ReplaceAllString(out, r.repl)
	}
	return redactTargetSecret(out)
}

// Digit run followed by a non word character or the end, the lookahead that keeps a numeric port visible
func numericPortAhead(rs []rune, i int) bool {
	j := i
	for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
		j++
	}
	if j == i {
		return false
	}
	if j == len(rs) {
		return true
	}
	c := rs[j]
	word := c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	return !word
}

// Masks a password typed after an SSH host where the port belongs, backtracking as a JavaScript regex does
func redactTargetSecret(text string) string {
	rs := []rune(text)
	var out strings.Builder
	pos := 0
	for start := 0; start < len(rs); {
		matchEnd, groupEnd := -1, -1
		if !jsSpace(rs[start]) {
			runEnd := start
			for runEnd < len(rs) && !jsSpace(rs[runEnd]) {
				runEnd++
			}
			for at := runEnd - 1; at > start && matchEnd < 0; at-- {
				if rs[at] != '@' {
					continue
				}
				h := at + 1
				for h < len(rs) && rs[h] != ':' && !jsSpace(rs[h]) {
					h++
				}
				if h == at+1 || h >= len(rs) || rs[h] != ':' {
					continue
				}
				if numericPortAhead(rs, h+1) {
					continue
				}
				e := h + 1
				for e < len(rs) && rs[e] != ':' && !jsSpace(rs[e]) {
					e++
				}
				if e == h+1 {
					continue
				}
				matchEnd, groupEnd = e, h
			}
		}
		if matchEnd < 0 {
			start++
			continue
		}
		out.WriteString(string(rs[pos:start]))
		out.WriteString(string(rs[start:groupEnd]))
		out.WriteString(":<redacted>")
		pos, start = matchEnd, matchEnd
	}
	out.WriteString(string(rs[pos:]))
	return out.String()
}

// ControlIdentity is what separates one control master socket from another for the same host
type ControlIdentity struct {
	KeyPath                    string
	OwnershipID                string
	Scope                      string
	EffectiveConfigFingerprint string
}

// Hashed socket path stable across reconnects and short enough for sun_path
func ControlSocketPath(user, host string, port int, baseDir string, identity ControlIdentity) string {
	dir := baseDir
	if dir == "" {
		dir = defaultControlDir()
	}
	parts := []any{identity.OwnershipID, identity.Scope, user, host, port, git.NodeNormalize(identity.KeyPath), identity.EffectiveConfigFingerprint}
	text, _ := jsonMarshal(parts)
	sum := sha256.Sum256([]byte(text))
	return git.NodeJoin(dir, hex.EncodeToString(sum[:])[:16]+".sock")
}

func shortControlDir() string {
	return "/tmp/icebath-ssh-" + strconv.Itoa(os.Getuid())
}

func defaultControlDir() string {
	if runtime.GOOS == "windows" {
		return git.NodeJoin(os.TempDir(), "icebath-desktop-ssh")
	}
	home, _ := os.UserHomeDir()
	homeDir := git.NodeJoin(backend.PlatformDefaultHermesHome(home, backend.ProcessEnv(), "linux"), "desktop-ssh")
	if len(git.NodeJoin(homeDir, "0123456789abcdef.sock.0123456789abcdef")) <= 104 {
		return homeDir
	}
	return shortControlDir()
}

// Mux options sharing one authenticated master, or per call options when there is no control path
func BaseSSHOptions(controlPath string, connectTimeout time.Duration) []string {
	if connectTimeout <= 0 {
		connectTimeout = sshDefaultConnectTimeout
	}
	secs := int64(jsRound(float64(connectTimeout.Milliseconds()) / 1000))
	if secs < 1 {
		secs = 1
	}
	var mux []string
	if controlPath != "" {
		mux = []string{"-o", "ControlPath=" + controlPath, "-o", "ControlMaster=auto", "-o", "ControlPersist=" + strconv.Itoa(ControlPersistSeconds)}
	}
	return append(mux,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ConnectTimeout="+strconv.FormatInt(secs, 10),
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "TCPKeepAlive=yes")
}

// Math.round of JavaScript, halves toward positive infinity
func jsRound(f float64) float64 {
	return float64(int64(f + 0.5))
}

// SSHEndpoint is the destination half every ssh invocation of a connection shares
type SSHEndpoint struct {
	Host        string
	User        string
	Port        int
	KeyPath     string
	ControlPath string
}

// Non default port and explicit identity file
func HostArgs(port int, keyPath string) ([]string, error) {
	var args []string
	if port != 0 && port != 22 {
		args = append(args, "-p", strconv.Itoa(port))
	}
	if keyPath != "" {
		if err := ValidateKeyPath(keyPath); err != nil {
			return nil, err
		}
		args = append(args, "-i", keyPath)
	}
	return args, nil
}

// SSHTarget is user@host, or host without a user
func SSHTarget(user, host string) string {
	if user != "" {
		return user + "@" + host
	}
	return host
}

// Quotes for the login shell sshd runs, decoding the same under POSIX sh and fish
func LoginShellQuote(value string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range value {
		switch r {
		case '\'':
			b.WriteString(`'\''`)
		case '\\':
			b.WriteString(`'\\'`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// Arguments of a one shot remote command
func BuildExecArgs(c SSHEndpoint, remoteCommand string, connectTimeout time.Duration) ([]string, error) {
	host, err := HostArgs(c.Port, c.KeyPath)
	if err != nil {
		return nil, err
	}
	args := append(BaseSSHOptions(c.ControlPath, connectTimeout), host...)
	return append(args, "--", SSHTarget(c.User, c.Host), remoteCommand), nil
}

// Arguments of a control command against the master socket
func BuildControlArgs(c SSHEndpoint, op string, extra []string, connectTimeout time.Duration) ([]string, error) {
	host, err := HostArgs(c.Port, c.KeyPath)
	if err != nil {
		return nil, err
	}
	args := append([]string{"-O", op}, extra...)
	args = append(args, BaseSSHOptions(c.ControlPath, connectTimeout)...)
	args = append(args, host...)
	return append(args, "--", SSHTarget(c.User, c.Host)), nil
}

// Arguments that open the master and background ssh once it is up
func BuildMasterArgs(c SSHEndpoint, connectTimeout time.Duration) ([]string, error) {
	host, err := HostArgs(c.Port, c.KeyPath)
	if err != nil {
		return nil, err
	}
	args := append([]string{"-M", "-N", "-f"}, BaseSSHOptions(c.ControlPath, connectTimeout)...)
	args = append(args, host...)
	return append(args, "--", SSHTarget(c.User, c.Host)), nil
}

// Arguments of an interactive ssh -tt session over the existing master, in remoteCwd or running remoteCommand
func BuildInteractiveSSHArgs(c SSHEndpoint, remoteCwd string, connectTimeout time.Duration, remoteCommand string) ([]string, error) {
	host, err := HostArgs(c.Port, c.KeyPath)
	if err != nil {
		return nil, err
	}
	args := append([]string{"-tt"}, BaseSSHOptions(c.ControlPath, connectTimeout)...)
	args = append(args, host...)
	args = append(args, "--", SSHTarget(c.User, c.Host))
	if remoteCommand != "" {
		return append(args, remoteCommand), nil
	}
	cwd := strings.TrimSpace(remoteCwd)
	if cwd != "" {
		q := "'" + strings.ReplaceAll(cwd, "'", `'\''`) + "'"
		return append(args, "cd "+q+` 2>/dev/null; exec "$SHELL" -l`), nil
	}
	return append(args, `exec "$SHELL" -l`), nil
}

// Wraps a single remote command in a POSIX watchdog that kills it remotely after secs
func WithRemoteTimeout(remoteCommand string, timeoutSecs float64) string {
	secs := int64(RemoteProbeTimeoutSecs)
	if timeoutSecs > 0 && timeoutSecs < 1e18 {
		secs = int64(timeoutSecs)
	}
	return `[ -n "${ZSH_VERSION-}" ] || set -m 2>/dev/null; (` + remoteCommand + `) </dev/null & __htp=$!; set +m 2>/dev/null; ` +
		`(sleep ` + strconv.FormatInt(secs, 10) + ` </dev/null >/dev/null 2>&1; kill -9 -- -$__htp 2>/dev/null; kill -9 $__htp 2>/dev/null) & __htw=$!; ` +
		`wait $__htp; __htrc=$?; ` +
		`kill $__htw 2>/dev/null; wait $__htw 2>/dev/null; ` +
		`exit $__htrc`
}

// Local forward bound to loopback only
func ForwardSpec(localPort, remotePort int, remoteHost string) string {
	if remoteHost == "" {
		remoteHost = "127.0.0.1"
	}
	return fmt.Sprintf("127.0.0.1:%d:%s:%d", localPort, remoteHost, remotePort)
}

var (
	sshHostKeyRE     = regexp.MustCompile(`(?i)REMOTE HOST IDENTIFICATION HAS CHANGED|Host key verification failed|Offending (?:key|ECDSA|RSA|ED25519)`)
	sshInteractiveRE = regexp.MustCompile(`(?i)Tailscale SSH requires an additional check|To authenticate, visit:[` + jsSpaceClass + `]*https://login\.tailscale\.com/`)
	sshAuthRE        = regexp.MustCompile(`(?i)Permission denied|Too many authentication failures|no matching host key|publickey|password|keyboard-interactive`)
	sshUnreachableRE = regexp.MustCompile(`(?i)Could not resolve hostname|Connection refused|Connection timed out|No route to host|Network is unreachable|Operation timed out|port \d+: Connection`)
)

// Kind of an ssh stderr, host key change ahead of auth because its banner also names keys
func ClassifySSHError(stderr string) string {
	switch {
	case sshHostKeyRE.MatchString(stderr):
		return SSHErrorHostKeyChanged
	case sshInteractiveRE.MatchString(stderr):
		return SSHErrorInteractiveAuth
	case sshAuthRE.MatchString(stderr):
		return SSHErrorAuthFailed
	case sshUnreachableRE.MatchString(stderr):
		return SSHErrorUnreachable
	}
	return SSHErrorUnknown
}

// User facing copy of a classified ssh failure
func SSHErrorMessage(kind string, user, host string, port int, stderr string) string {
	t := SSHTarget(user, host)
	detail := strings.TrimSpace(stderr)
	switch kind {
	case SSHErrorHostKeyChanged:
		return "The host key for " + t + " has CHANGED since you last connected. " +
			"This could be a man-in-the-middle attack, or the server was reinstalled. " +
			"SSH refused to connect. Verify the change is expected, then remove the old key " +
			"with `ssh-keygen -R " + host + "` and reconnect.\n\n" + detail
	case SSHErrorAuthFailed:
		return "SSH authentication to " + t + " failed. Desktop runs ssh non-interactively " +
			"(BatchMode), so a key requiring a passphrase or 2FA must be loaded into your " +
			"ssh-agent first (e.g. `ssh-add ~/.ssh/id_ed25519`), or set an IdentityFile in " +
			"~/.ssh/config. Original error: " + detail
	case SSHErrorInteractiveAuth:
		portArg := ""
		if port != 0 && port != 22 {
			portArg = " -p " + strconv.Itoa(port)
		}
		return "Tailscale SSH requires an interactive browser check for " + t + ". " +
			"Icebath Desktop runs SSH non-interactively. In Terminal, run " +
			"`ssh" + portArg + " " + t + " true`, complete the browser check, then retry. " +
			"If checks recur, use a key-authenticated OpenSSH route or adjust the tailnet SSH check policy."
	case SSHErrorUnreachable:
		return "Could not reach " + t + " over SSH. Check the host, port, and your network. Original error: " + detail
	case SSHErrorTimeout:
		return "SSH operation to " + t + " timed out. The connection may be half-open (e.g. after sleep); reconnecting."
	}
	if detail == "" {
		detail = "unknown failure"
	}
	return "SSH error connecting to " + t + ": " + detail
}

// SSHRunResult is one finished ssh child, Code nil when it died from a signal
type SSHRunResult struct {
	Code   *int
	Signal string
	Stdout string
	Stderr string
}

// SSHRunOptions bound one ssh child by time and a cancel context
type SSHRunOptions struct {
	Timeout   time.Duration
	Command   string
	StdinData *string
	Signal    context.Context
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

func ctxAborted(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil
}

// Runs one ssh invocation, killing it on the deadline or on cancel
func RunSSH(args []string, o SSHRunOptions) (SSHRunResult, error) {
	if ctxAborted(o.Signal) {
		return SSHRunResult{}, supersededSSHError("SSH operation was cancelled.")
	}
	command := o.Command
	if command == "" {
		command = "ssh"
	}
	cmd := exec.Command(command, args...)
	var stdout, stderr lockedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if o.StdinData != nil {
		cmd.Stdin = strings.NewReader(*o.StdinData)
	}
	if err := cmd.Start(); err != nil {
		return SSHRunResult{}, nodeSpawnError(command, err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	timer := time.NewTimer(o.Timeout)
	defer timer.Stop()
	select {
	case <-done:
		code, signal := processExit(cmd.ProcessState)
		return SSHRunResult{Code: code, Signal: signal, Stdout: stdout.String(), Stderr: stderr.String()}, nil
	case <-timer.C:
		_ = cmd.Process.Kill()
		return SSHRunResult{}, &SSHError{
			Message:    fmt.Sprintf("ssh timed out after %dms", o.Timeout.Milliseconds()),
			ErrKind:    SSHErrorTimeout,
			StderrKind: ClassifySSHError(stderr.String()),
		}
	case <-ctxDone(o.Signal):
		_ = cmd.Process.Kill()
		return SSHRunResult{}, supersededSSHError("SSH operation was cancelled.")
	}
}

// Normal exit is code zero and no close signal
func sshCloseOK(r SSHRunResult) bool {
	return r.Signal == "" && r.Code != nil && *r.Code == 0
}

func codeText(code *int) string {
	if code == nil {
		return "null"
	}
	return strconv.Itoa(*code)
}

// ControlMasterHolders counts the connections attached to each control socket
type ControlMasterHolders struct {
	mu      sync.Mutex
	holders map[string]map[any]bool
}

// NewControlMasterHolders is an empty holder registry
func NewControlMasterHolders() *ControlMasterHolders {
	return &ControlMasterHolders{holders: map[string]map[any]bool{}}
}

// Exits the master only when the closing connection was its last holder
func ControlMasterCloseAction(otherHolders int) string {
	if otherHolders > 0 {
		return "release-only"
	}
	return "exit-master"
}

// Registers holder on a control socket
func (h *ControlMasterHolders) Acquire(controlPath string, holder any) {
	if controlPath == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.holders[controlPath]
	if set == nil {
		set = map[any]bool{}
		h.holders[controlPath] = set
	}
	set[holder] = true
}

// Drops holder and answers what its close does with the master
func (h *ControlMasterHolders) Release(controlPath string, holder any) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.holders[controlPath]
	if set != nil {
		delete(set, holder)
		if len(set) == 0 {
			delete(h.holders, controlPath)
		}
	}
	return ControlMasterCloseAction(len(set))
}

// Holders still attached to a control socket
func (h *ControlMasterHolders) Count(controlPath string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.holders[controlPath])
}

// Process wide holder view every connection shares
var SharedControlMasterHolders = NewControlMasterHolders()

// SSHConnectionOptions name the client binary, log and identity of one connection
type SSHConnectionOptions struct {
	// Per invocation ssh with one tunnel child per forward, the Windows OpenSSH form
	NoMux                      bool
	SSHBinary                  string
	RememberLog                func(string)
	OwnershipID                string
	Scope                      string
	EffectiveConfigFingerprint string
}

type tunnelChild struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

type sshTunnel struct {
	alive        bool
	child        *tunnelChild
	restarts     int
	restartTimer *time.Timer
	stopping     bool
}

// SSHConnection is one OpenSSH ControlMaster connection, or per call ssh with tunnel children without mux
type SSHConnection struct {
	SSHEndpoint
	SSHBinary string

	log            func(string)
	mux            bool
	controlMasters *ControlMasterHolders

	mu             sync.Mutex
	opened         bool
	posixRemote    *bool
	tunnels        map[string]*sshTunnel
	forwardedSpecs map[string]bool
	keepaliveStop  chan struct{}
}

// SSHConnectionConfig is the destination of a connection
type SSHConnectionConfig struct {
	Host    string
	User    string
	Port    int
	KeyPath string
}

// NewSSHConnection validates the destination and derives the control socket
func NewSSHConnection(cfg SSHConnectionConfig, o SSHConnectionOptions) (*SSHConnection, error) {
	if cfg.Host == "" {
		return nil, errors.New("SshConnection requires a host.")
	}
	port := cfg.Port
	if port == 0 {
		port = 22
	}
	if err := ValidateSSHTarget(cfg.Host, cfg.User, float64(port)); err != nil {
		return nil, err
	}
	if err := ValidateKeyPath(cfg.KeyPath); err != nil {
		return nil, err
	}
	c := &SSHConnection{
		SSHEndpoint:    SSHEndpoint{Host: cfg.Host, User: cfg.User, Port: port, KeyPath: cfg.KeyPath},
		mux:            !o.NoMux && runtime.GOOS != "windows",
		controlMasters: SharedControlMasterHolders,
		tunnels:        map[string]*sshTunnel{},
		forwardedSpecs: map[string]bool{},
		SSHBinary:      o.SSHBinary,
		log:            o.RememberLog,
	}
	if c.mux {
		c.ControlPath = ControlSocketPath(c.User, c.Host, c.Port, "", ControlIdentity{
			KeyPath: c.KeyPath, OwnershipID: o.OwnershipID, Scope: o.Scope, EffectiveConfigFingerprint: o.EffectiveConfigFingerprint,
		})
	}
	if c.SSHBinary == "" {
		c.SSHBinary = "ssh"
	}
	if c.log == nil {
		c.log = func(string) {}
	}
	return c, nil
}

// Probe connection without mux, one authenticated ssh per operation
func NewSSHProbeConnection(cfg SSHConnectionConfig, o SSHConnectionOptions) (*SSHConnection, error) {
	o.NoMux = true
	return NewSSHConnection(cfg, o)
}

func (c *SSHConnection) logLine(msg string) {
	c.log(RedactSecrets("[ssh] " + msg))
}

// Classifies a failed run, a tunnel stderr or an error into the user facing kind and copy
func (c *SSHConnection) fail(v any, fallbackKind string) error {
	var signal, stderr string
	switch x := v.(type) {
	case error:
		if errKind(x) == SSHErrorSuperseded {
			return x
		}
		if se, ok := x.(*SSHError); ok && se.ErrKind == SSHErrorTimeout {
			kind := SSHErrorTimeout
			if se.StderrKind == SSHErrorInteractiveAuth {
				kind = SSHErrorInteractiveAuth
			}
			return &SSHError{Message: SSHErrorMessage(kind, c.User, c.Host, c.Port, ""), ErrKind: kind}
		}
		if se, ok := x.(*SSHError); ok {
			signal = se.Signal
		}
		stderr = x.Error()
	case SSHRunResult:
		signal, stderr = x.Signal, x.Stderr
	case string:
		stderr = x
	}
	if signal != "" && strings.TrimSpace(stderr) == "" {
		detail := "ssh process exited from signal " + signal
		return &SSHError{Message: SSHErrorMessage(SSHErrorUnknown, c.User, c.Host, c.Port, detail), ErrKind: SSHErrorUnknown, Signal: signal}
	}
	kind := fallbackKind
	if stderr != "" {
		kind = ClassifySSHError(stderr)
	}
	return &SSHError{Message: SSHErrorMessage(kind, c.User, c.Host, c.Port, stderr), ErrKind: kind, Signal: signal}
}

// Classifies a failed connect and records what ssh did in desktop.log
func (c *SSHConnection) connectFailed(raw any) error {
	err := c.fail(raw, SSHErrorUnreachable)
	if errKind(err) == SSHErrorSuperseded {
		return err
	}
	code, signal, stderr := "?", "", ""
	switch x := raw.(type) {
	case SSHRunResult:
		code, signal, stderr = codeText(x.Code), x.Signal, x.Stderr
	case *SSHError:
		if x.Code != "" {
			code = x.Code
		}
		signal, stderr = x.Signal, x.Message
	case error:
		stderr = x.Error()
	}
	stderr = strings.TrimSpace(stderr)
	if n := utf8.RuneCountInString(stderr); n > 500 {
		stderr = string([]rune(stderr)[n-500:])
	}
	if stderr == "" {
		stderr = "(empty)"
	}
	if signal == "" {
		signal = "none"
	}
	c.logLine(fmt.Sprintf("connect to %s:%d failed (kind=%s, exit=%s, signal=%s): %s", SSHTarget(c.User, c.Host), c.Port, errKind(err), code, signal, stderr))
	return err
}

func (c *SSHConnection) run(args []string, timeout time.Duration, signal context.Context, stdin *string) (SSHRunResult, error) {
	return RunSSH(args, SSHRunOptions{Timeout: timeout, Command: c.SSHBinary, Signal: signal, StdinData: stdin})
}

// Opens the master, a live verified master being reused, or verifies auth with one exec without mux
func (c *SSHConnection) Open(signal context.Context) error {
	if !c.mux {
		return c.open(signal)
	}
	c.controlMasters.Acquire(c.ControlPath, c)
	if err := c.open(signal); err != nil {
		c.controlMasters.Release(c.ControlPath, c)
		return err
	}
	return nil
}

func (c *SSHConnection) open(signal context.Context) error {
	if c.mux {
		controlDir := filepath.Dir(c.ControlPath)
		if runtime.GOOS != "windows" && controlDir == shortControlDir() {
			if err := checkShortControlParent(); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(controlDir, 0o700); err != nil {
			return err
		}
		if err := secureControlDir(controlDir); err != nil {
			return err
		}
	}
	alive, err := c.IsAlive(signal)
	if err != nil {
		return err
	}
	if alive {
		if !c.mux {
			c.setOpened(true)
			return nil
		}
		verified, err := c.verifyMuxChannel(signal)
		if err != nil {
			return err
		}
		if verified {
			c.setOpened(true)
			return nil
		}
		c.logLine("existing control master failed exec verification; evicting stale master")
		c.evictStaleMaster()
	}
	if !c.mux {
		c.logLine(fmt.Sprintf("connecting (no-mux) to %s:%d", SSHTarget(c.User, c.Host), c.Port))
		args, err := BuildExecArgs(c.SSHEndpoint, "exit 0", sshDefaultConnectTimeout)
		if err != nil {
			return err
		}
		result, err := c.run(args, sshDefaultConnectTimeout, signal, nil)
		if err != nil {
			return c.connectFailed(err)
		}
		if !sshCloseOK(result) {
			return c.connectFailed(result)
		}
		c.setOpened(true)
		c.logLine("connection verified (no-mux; per-operation ssh)")
		return nil
	}
	args, err := BuildMasterArgs(c.SSHEndpoint, sshDefaultConnectTimeout)
	if err != nil {
		return err
	}
	c.logLine(fmt.Sprintf("opening control master to %s:%d", SSHTarget(c.User, c.Host), c.Port))
	result, err := c.run(args, sshDefaultConnectTimeout, signal, nil)
	if err != nil {
		return c.connectFailed(err)
	}
	if !sshCloseOK(result) {
		return c.connectFailed(result)
	}
	c.setOpened(true)
	c.logLine("control master established")
	return nil
}

func (c *SSHConnection) setOpened(v bool) {
	c.mu.Lock()
	c.opened = v
	c.mu.Unlock()
}

// Whether the master answers a check, or one exec succeeds without mux, a dead tunnel failing at once
func (c *SSHConnection) IsAlive(signal context.Context) (bool, error) {
	c.mu.Lock()
	for _, t := range c.tunnels {
		if !t.alive {
			c.mu.Unlock()
			return false, nil
		}
	}
	c.mu.Unlock()
	var args []string
	var err error
	if c.mux {
		args, err = BuildControlArgs(c.SSHEndpoint, "check", nil, sshDefaultConnectTimeout)
	} else {
		args, err = BuildExecArgs(c.SSHEndpoint, "exit 0", sshDefaultConnectTimeout)
	}
	if err != nil {
		return false, err
	}
	result, err := c.run(args, sshDefaultConnectTimeout, signal, nil)
	if err != nil {
		if errKind(err) == SSHErrorSuperseded {
			return false, err
		}
		return false, nil
	}
	return sshCloseOK(result), nil
}

// Refreshes the mux while it carries forwards so ControlPersist cannot drop their listeners
func (c *SSHConnection) startControlKeepalive() {
	c.mu.Lock()
	if !c.mux || c.keepaliveStop != nil || len(c.forwardedSpecs) == 0 {
		c.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	c.keepaliveStop = stop
	c.mu.Unlock()
	go func() {
		ticker := time.NewTicker(ControlForwardKeepalive)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_, _ = c.IsAlive(nil)
			}
		}
	}()
}

func (c *SSHConnection) stopControlKeepalive() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keepaliveStop == nil {
		return
	}
	close(c.keepaliveStop)
	c.keepaliveStop = nil
}

// A real exec through the master, a wedged mux hanging to the timeout
func (c *SSHConnection) verifyMuxChannel(signal context.Context) (bool, error) {
	args, err := BuildExecArgs(c.SSHEndpoint, "exit 0", sshDefaultConnectTimeout)
	if err != nil {
		return false, nil
	}
	result, err := c.run(args, sshDefaultConnectTimeout, signal, nil)
	if err != nil {
		if errKind(err) == SSHErrorSuperseded {
			return false, err
		}
		return false, nil
	}
	return sshCloseOK(result), nil
}

// Exits the master and drops its socket so ControlMaster auto cannot attach to the corpse
func (c *SSHConnection) evictStaleMaster() {
	if args, err := BuildControlArgs(c.SSHEndpoint, "exit", nil, sshDefaultConnectTimeout); err == nil {
		_, _ = c.run(args, sshDefaultConnectTimeout, nil, nil)
	}
	if err := os.Remove(c.ControlPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.logLine(fmt.Sprintf("could not remove stale control socket (%s); a fresh master may not dial", fileErrorCode(err)))
	}
}

// Node error code of a failed file call
func fileErrorCode(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, fs.ErrPermission):
		return "EACCES"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return errnoCode(pe.Err)
	}
	return "undefined"
}

// SSHExecOptions bound one remote command and feed its stdin
type SSHExecOptions struct {
	Timeout   time.Duration
	StdinData *string
}

// Runs one remote command, under sh -c on Linux and Darwin remotes, answering its stdout
func (c *SSHConnection) Exec(remoteCommand string, o SSHExecOptions) (string, error) {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = sshDefaultExecTimeout
	}
	c.mu.Lock()
	known := c.posixRemote != nil
	c.mu.Unlock()
	if !known {
		if args, err := BuildExecArgs(c.SSHEndpoint, "uname -s", sshDefaultConnectTimeout); err == nil {
			if probe, err := c.run(args, timeout, nil, nil); err == nil && (probe.Code == nil || *probe.Code != 255) {
				name := strings.TrimSpace(probe.Stdout)
				posix := probe.Code != nil && *probe.Code == 0 && (name == "Linux" || name == "Darwin")
				c.mu.Lock()
				c.posixRemote = &posix
				c.mu.Unlock()
			}
		}
	}
	c.mu.Lock()
	posix := c.posixRemote != nil && *c.posixRemote
	c.mu.Unlock()
	command := remoteCommand
	if posix {
		command = "sh -c " + LoginShellQuote(remoteCommand)
	}
	args, err := BuildExecArgs(c.SSHEndpoint, command, sshDefaultConnectTimeout)
	if err != nil {
		return "", err
	}
	result, err := c.run(args, timeout, nil, o.StdinData)
	if err != nil {
		return "", c.fail(err, SSHErrorUnknown)
	}
	if !sshCloseOK(result) {
		return "", c.fail(result, SSHErrorUnknown)
	}
	return result.Stdout, nil
}

// Spawns one ssh -N -L child and waits for its local forwarding line, a later death routed to the flap handler
func (c *SSHConnection) startNoMuxTunnelChild(t *sshTunnel, spec string, args []string, localPort int) error {
	cmd := exec.Command(c.SSHBinary, args...)
	child := &tunnelChild{cmd: cmd, exited: make(chan struct{})}
	c.mu.Lock()
	t.child = child
	c.mu.Unlock()
	result := make(chan error, 1)
	var stateMu sync.Mutex
	var stderr string
	readyConfirmed, settled, downHandled := false, false, false
	var readyTimer *time.Timer
	finishOK := func() {
		if settled {
			return
		}
		settled = true
		readyTimer.Stop()
		result <- nil
	}
	finishFail := func(err *SSHError) {
		if settled {
			return
		}
		settled = true
		readyTimer.Stop()
		err.TunnelStderr = stderr
		result <- err
	}
	onDown := func(cause string, err *SSHError) {
		stateMu.Lock()
		if !readyConfirmed {
			c.mu.Lock()
			if t.child == child {
				t.alive = false
			}
			c.mu.Unlock()
			finishFail(err)
			stateMu.Unlock()
			return
		}
		c.mu.Lock()
		current := t.child == child
		c.mu.Unlock()
		if downHandled || !current {
			stateMu.Unlock()
			return
		}
		downHandled = true
		stateMu.Unlock()
		c.handleNoMuxTunnelFlap(t, spec, args, localPort, cause)
	}
	stateMu.Lock()
	readyTimer = time.AfterFunc(sshDefaultForwardTimeout, func() {
		stateMu.Lock()
		defer stateMu.Unlock()
		finishFail(&SSHError{Message: "tunnel did not confirm local forwarding"})
	})
	stateMu.Unlock()
	pr, pw, err := os.Pipe()
	if err != nil {
		close(child.exited)
		se := &SSHError{Message: err.Error(), Cause: err}
		onDown("tunnel process failed ("+se.Message+")", se)
		return <-result
	}
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		close(child.exited)
		se := nodeSpawnError(c.SSHBinary, err)
		onDown("tunnel process failed ("+se.Message+")", se)
		return <-result
	}
	_ = pw.Close()
	ready := regexp.MustCompile(`Local forwarding listening on .* port ` + strconv.Itoa(localPort) + `\b`)
	go func() {
		defer pr.Close()
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				stateMu.Lock()
				if !readyConfirmed {
					stderr += string(buf[:n])
					if len(stderr) > tunnelStderrCap {
						stderr = stderr[len(stderr)-tunnelStderrCap:]
					}
					if ready.MatchString(stderr) {
						readyConfirmed = true
						finishOK()
					}
				}
				stateMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(child.exited)
		code, _ := processExit(cmd.ProcessState)
		text := codeText(code)
		onDown("tunnel process exited with code "+text, &SSHError{Message: "tunnel process exited with code " + text})
	}()
	return <-result
}

// Restarts a dead ready tunnel child within the cumulative budget, marking it dead once exhausted or stopping
func (c *SSHConnection) handleNoMuxTunnelFlap(t *sshTunnel, spec string, args []string, localPort int, cause string) {
	c.mu.Lock()
	if t.stopping || c.tunnels[spec] != t {
		t.alive = false
		c.mu.Unlock()
		return
	}
	if t.restarts >= sshTunnelRestartLimit {
		t.alive = false
		c.mu.Unlock()
		c.logLine(fmt.Sprintf("tunnel 127.0.0.1:%d down (%s); restart budget exhausted (%d)", localPort, cause, sshTunnelRestartLimit))
		return
	}
	t.restarts++
	restarts := t.restarts
	c.mu.Unlock()
	c.logLine(fmt.Sprintf("tunnel 127.0.0.1:%d flapped (%s); restarting (%d/%d) in %dms", localPort, cause, restarts, sshTunnelRestartLimit, sshTunnelRestartDelay.Milliseconds()))
	c.mu.Lock()
	t.restartTimer = time.AfterFunc(sshTunnelRestartDelay, func() {
		c.mu.Lock()
		t.restartTimer = nil
		if t.stopping || c.tunnels[spec] != t {
			t.alive = false
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		if err := c.startNoMuxTunnelChild(t, spec, args, localPort); err == nil {
			c.mu.Lock()
			t.alive = true
			c.mu.Unlock()
			c.logLine(fmt.Sprintf("tunnel 127.0.0.1:%d restarted", localPort))
		} else {
			c.mu.Lock()
			stale := t.child
			c.mu.Unlock()
			_ = stopTunnelChild(stale)
			c.handleNoMuxTunnelFlap(t, spec, args, localPort, "restart failed: "+err.Error())
		}
	})
	c.mu.Unlock()
}

// Terminates a tunnel child and waits for it to exit
func stopTunnelChild(child *tunnelChild) error {
	if child == nil {
		return nil
	}
	select {
	case <-child.exited:
		return nil
	default:
	}
	if child.cmd.Process == nil {
		return nil
	}
	if err := terminateChild(child.cmd.Process); err != nil {
		return errors.New("SSH tunnel termination was refused.")
	}
	timer := time.NewTimer(tunnelStopTimeout)
	defer timer.Stop()
	select {
	case <-child.exited:
		return nil
	case <-timer.C:
		return errors.New("SSH tunnel did not exit after termination.")
	}
}

// Establishes a loopback forward, a control forward with mux or a restartable tunnel child without
func (c *SSHConnection) Forward(localPort, remotePort int) error {
	spec := ForwardSpec(localPort, remotePort, "")
	c.logLine(fmt.Sprintf("forwarding 127.0.0.1:%d -> 127.0.0.1:%d", localPort, remotePort))
	if !c.mux {
		host, err := HostArgs(c.Port, c.KeyPath)
		if err != nil {
			return err
		}
		args := append(BaseSSHOptions("", sshDefaultConnectTimeout), host...)
		args = append(args, "-v", "-N", "-L", spec, "--", SSHTarget(c.User, c.Host))
		t := &sshTunnel{alive: true}
		c.mu.Lock()
		c.tunnels[spec] = t
		c.mu.Unlock()
		if err := c.startNoMuxTunnelChild(t, spec, args, localPort); err != nil {
			c.mu.Lock()
			child := t.child
			c.mu.Unlock()
			if stopErr := stopTunnelChild(child); stopErr != nil {
				return c.fail(stopErr, SSHErrorUnknown)
			}
			c.mu.Lock()
			delete(c.tunnels, spec)
			c.mu.Unlock()
			var se *SSHError
			if errors.As(err, &se) && se.TunnelStderr != "" {
				return c.fail(se.TunnelStderr, SSHErrorUnknown)
			}
			return c.fail(err, SSHErrorUnknown)
		}
		return nil
	}
	args, err := BuildControlArgs(c.SSHEndpoint, "forward", []string{"-L", spec}, sshDefaultConnectTimeout)
	if err != nil {
		return err
	}
	result, err := c.run(args, sshDefaultForwardTimeout, nil, nil)
	if err != nil {
		return c.fail(err, SSHErrorUnknown)
	}
	if !sshCloseOK(result) {
		return c.fail(result, SSHErrorUnknown)
	}
	c.mu.Lock()
	c.forwardedSpecs[spec] = true
	c.mu.Unlock()
	c.startControlKeepalive()
	return nil
}

// Cancels a forward, a failed control cancel logged and not returned
func (c *SSHConnection) CancelForward(localPort, remotePort int) error {
	spec := ForwardSpec(localPort, remotePort, "")
	if !c.mux {
		c.mu.Lock()
		t := c.tunnels[spec]
		if t == nil {
			c.mu.Unlock()
			return nil
		}
		t.stopping = true
		t.alive = false
		if t.restartTimer != nil {
			t.restartTimer.Stop()
			t.restartTimer = nil
		}
		child := t.child
		c.mu.Unlock()
		if err := stopTunnelChild(child); err != nil {
			return err
		}
		c.mu.Lock()
		delete(c.tunnels, spec)
		c.mu.Unlock()
		c.logLine(fmt.Sprintf("cancelled forward 127.0.0.1:%d", localPort))
		return nil
	}
	args, err := BuildControlArgs(c.SSHEndpoint, "cancel", []string{"-L", spec}, sshDefaultConnectTimeout)
	if err != nil {
		return err
	}
	defer func() {
		c.mu.Lock()
		delete(c.forwardedSpecs, spec)
		empty := len(c.forwardedSpecs) == 0
		c.mu.Unlock()
		if empty {
			c.stopControlKeepalive()
		}
	}()
	if _, err := c.run(args, sshDefaultForwardTimeout, nil, nil); err != nil {
		c.logLine("cancelForward failed (ignored): " + err.Error())
		return nil
	}
	c.logLine(fmt.Sprintf("cancelled forward 127.0.0.1:%d", localPort))
	return nil
}

// Exits the master unless another connection holds its socket, or kills the tunnel children without mux
func (c *SSHConnection) Close() error {
	c.stopControlKeepalive()
	c.mu.Lock()
	c.forwardedSpecs = map[string]bool{}
	c.mu.Unlock()
	action := "exit-master"
	if c.mux {
		action = c.controlMasters.Release(c.ControlPath, c)
	}
	c.mu.Lock()
	opened := c.opened
	c.mu.Unlock()
	if !opened {
		return nil
	}
	if !c.mux {
		for {
			c.mu.Lock()
			var spec string
			var t *sshTunnel
			for k, v := range c.tunnels {
				spec, t = k, v
				break
			}
			if t == nil {
				c.mu.Unlock()
				break
			}
			t.stopping = true
			t.alive = false
			if t.restartTimer != nil {
				t.restartTimer.Stop()
				t.restartTimer = nil
			}
			child := t.child
			c.mu.Unlock()
			if err := stopTunnelChild(child); err != nil {
				return err
			}
			c.mu.Lock()
			delete(c.tunnels, spec)
			c.mu.Unlock()
		}
		c.setOpened(false)
		c.logLine("connection closed (no-mux tunnels killed)")
		return nil
	}
	if action == "release-only" {
		c.setOpened(false)
		c.logLine(fmt.Sprintf("control master left running: %d other connection(s) still use it", c.controlMasters.Count(c.ControlPath)))
		return nil
	}
	args, err := BuildControlArgs(c.SSHEndpoint, "exit", nil, sshDefaultConnectTimeout)
	if err == nil {
		var result SSHRunResult
		result, err = c.run(args, sshDefaultConnectTimeout, nil, nil)
		if err != nil {
			err = c.fail(err, SSHErrorUnknown)
		} else if !sshCloseOK(result) {
			err = c.fail(result, SSHErrorUnknown)
		}
	}
	if err != nil {
		c.logLine("close failed; removing control socket: " + err.Error())
		_ = os.Remove(c.ControlPath)
	} else {
		c.logLine("control master closed")
	}
	c.setOpened(false)
	return nil
}

// Free loopback port for the local end of a tunnel
func PickLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// Refuses to keep polling once a newer bootstrap cancelled this one
func AssertBootstrapNotSuperseded(signal context.Context) error {
	if ctxAborted(signal) {
		return supersededSSHError("SSH bootstrap was cancelled.")
	}
	return nil
}
