package backend

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	gatewayEnsureProtocolDeadline = 60 * time.Second
	gatewayEnsureClientGrace      = 15 * time.Second
	ensureStdoutLimit             = 65536
	ensureStderrTail              = 4096
	// StaleRestartPoll is how often a restarting owner is re-ensured until it is replaced
	StaleRestartPoll  = time.Second
	restartWaitReason = "restart_wait"
	// GatewayTicketHeader carries a native grant on desktop requests to the local gateway
	GatewayTicketHeader = "X-Icebath-Gateway-Ticket"
)

// BackendCommand is the resolved command line the desktop runs icebath verbs through
type BackendCommand struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Shell   bool              `json:"shell"`
}

// EnsureResult is the exit code and captured output of one bounded gateway client
type EnsureResult struct {
	Code   int
	Stdout string
	Stderr string
}

// EnsureRunOptions bound one gateway client run, zero values take the ensure deadline and label
type EnsureRunOptions struct {
	Timeout time.Duration
	Label   string
}

// GatewayEndpoint is the credential free endpoint a gateway ensure verdict names
type GatewayEndpoint struct {
	ProfileID       string      `json:"profile_id"`
	InstanceID      string      `json:"instance_id"`
	AuthorityEpoch  json.Number `json:"authority_epoch"`
	RuntimeProtocol int         `json:"runtime_protocol"`
	APIOrigin       string      `json:"api_origin"`
	Capabilities    []string    `json:"capabilities"`
	Supervisor      string      `json:"supervisor"`
	ControlHome     *string     `json:"control_home,omitempty"`
	CodeSHA         *string     `json:"code_sha,omitempty"`
}

func (e GatewayEndpoint) controlHome() string {
	if e.ControlHome != nil && *e.ControlHome != "" {
		return *e.ControlHome
	}
	return e.ProfileID
}

// Wraps command in quotes when cmd.exe would cut a spaced path of a shell delegated Windows spawn
func WindowsShellCommand(command string, shell, isWindows bool) string {
	if !isWindows || !shell || command == "" || strings.HasPrefix(command, `"`) {
		return command
	}
	return `"` + command + `"`
}

// Runs one bounded icebath gateway client and captures its exit code, stdout and stderr tail
func RunGatewayEnsure(backend BackendCommand, cwd, home string, parentEnv map[string]string, opts EnsureRunOptions) (EnsureResult, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = gatewayEnsureProtocolDeadline + gatewayEnsureClientGrace
	}
	if opts.Label == "" {
		opts.Label = "icebath gateway ensure"
	}
	env := make(map[string]string, len(parentEnv)+len(backend.Env)+1)
	for k, v := range parentEnv {
		env[k] = v
	}
	env["ICEBATH_HOME"] = home
	for k, v := range backend.Env {
		env[k] = v
	}
	delete(env, "ICEBATH_DESKTOP_DEV_SERVER")
	cmd := spawnCommand(WindowsShellCommand(backend.Command, backend.Shell, runtime.GOOS == "windows"), backend.Args, backend.Shell)
	cmd.Dir = cwd
	cmd.Env = envList(env)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return EnsureResult{}, fmt.Errorf("Could not run %s", opts.Label)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return EnsureResult{}, fmt.Errorf("Could not run %s", opts.Label)
	}
	if err := cmd.Start(); err != nil {
		return EnsureResult{}, fmt.Errorf("Could not run %s", opts.Label)
	}
	var (
		mu       sync.Mutex
		stdout   strings.Builder
		stderr   string
		expired  error
		forced   = make(chan struct{})
		finished = make(chan struct{})
	)
	expire := func(message string) {
		mu.Lock()
		defer mu.Unlock()
		if expired != nil {
			return
		}
		expired = errors.New(message)
		terminateProcess(cmd.Process)
		go func() {
			select {
			case <-time.After(time.Second):
				cmd.Process.Kill()
				close(forced)
			case <-finished:
			}
		}()
	}
	var reads sync.WaitGroup
	reads.Add(2)
	go func() {
		defer reads.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				mu.Lock()
				stdout.Write(buf[:n])
				over := stdout.Len() > ensureStdoutLimit
				if over {
					kept := stdout.String()[:ensureStdoutLimit]
					stdout.Reset()
					stdout.WriteString(kept)
				}
				mu.Unlock()
				if over {
					expire(opts.Label + " output exceeded limit")
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer reads.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				mu.Lock()
				stderr = tailString(stderr+string(buf[:n]), ensureStderrTail)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	timer := time.AfterFunc(opts.Timeout, func() { expire(opts.Label + " timed out") })
	go func() {
		reads.Wait()
		cmd.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-forced:
		timer.Stop()
		mu.Lock()
		defer mu.Unlock()
		return EnsureResult{}, expired
	}
	timer.Stop()
	mu.Lock()
	defer mu.Unlock()
	code, exited := exitCode(cmd)
	if expired != nil {
		if !exited || code == 0 {
			code = 7
		}
		return EnsureResult{Code: code, Stdout: stdout.String(), Stderr: strings.TrimSpace(stderr + "\n" + expired.Error())}, nil
	}
	if !exited {
		code = 7
	}
	return EnsureResult{Code: code, Stdout: stdout.String(), Stderr: stderr}, nil
}

func exitCode(cmd *exec.Cmd) (int, bool) {
	if cmd.ProcessState == nil {
		return 0, false
	}
	if code := cmd.ProcessState.ExitCode(); code >= 0 {
		return code, true
	}
	return 0, false
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// Decodes the ensure verdict on stdout or diagnoses the stderr of a client that never reached the protocol
func ParseGatewayEnsureOutput(result EnsureResult) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(result.Stdout))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err == nil && !dec.More() {
		if obj, ok := payload.(map[string]any); ok {
			return obj, nil
		}
	}
	var reason string
	for _, line := range regexp.MustCompile(`\r?\n`).Split(strings.TrimSpace(result.Stderr), -1) {
		if line != "" {
			reason = line
		}
	}
	suffix := ""
	if reason != "" {
		suffix = ": " + reason
	}
	return nil, fmt.Errorf("icebath gateway ensure produced no result (exit %d)%s. Update Icebath or check the profile, then retry.", result.Code, suffix)
}

func payloadEndpoint(payload map[string]any) map[string]any {
	ep, _ := payload["endpoint"].(map[string]any)
	return ep
}

func payloadString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// StaleCode names the gateway and client code of a ready owner running other code than this client
type StaleCode struct {
	Gateway string
	Client  string
}

// Stale code pair when the ready owner and this client name different commits, nil otherwise
func StaleGatewayCode(payload map[string]any) *StaleCode {
	gw := payloadString(payloadEndpoint(payload), "code_sha")
	client := payloadString(payload, "client_code_sha")
	if gw == "" || client == "" || strings.EqualFold(gw, client) {
		return nil
	}
	return &StaleCode{Gateway: gw, Client: client}
}

func decodeEndpoint(raw map[string]any) (GatewayEndpoint, error) {
	var ep GatewayEndpoint
	text, err := json.Marshal(raw)
	if err != nil {
		return ep, err
	}
	dec := json.NewDecoder(strings.NewReader(string(text)))
	dec.UseNumber()
	if err := dec.Decode(&ep); err != nil {
		return ep, errors.New("Invalid local gateway endpoint")
	}
	return ep, nil
}

// whatwgOrigin is the subset of a WHATWG URL the endpoint check reads
type whatwgOrigin struct {
	protocol, hostname, port, pathname, search, hash string
	userinfo                                         bool
}

func parseWhatwg(raw string) (whatwgOrigin, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || (u.Host == "" && u.Opaque == "") {
		return whatwgOrigin{}, errors.New("Invalid URL")
	}
	o := whatwgOrigin{protocol: strings.ToLower(u.Scheme) + ":", port: u.Port(), pathname: u.EscapedPath(), userinfo: u.User != nil}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	o.hostname = host
	if ((o.protocol == "http:" || o.protocol == "ws:") && o.port == "80") || ((o.protocol == "https:" || o.protocol == "wss:") && o.port == "443") {
		o.port = ""
	}
	if o.pathname == "" {
		o.pathname = "/"
	}
	if u.RawQuery != "" {
		o.search = "?" + u.RawQuery
	}
	if u.Fragment != "" {
		o.hash = "#" + u.Fragment
	}
	return o, nil
}

func assertLocalGatewayEndpoint(ep GatewayEndpoint) error {
	o, err := parseWhatwg(ep.APIOrigin)
	if err != nil {
		return err
	}
	if (o.protocol != "http:" && o.protocol != "https:") || (o.hostname != "127.0.0.1" && o.hostname != "[::1]") ||
		o.userinfo || o.pathname != "/" || o.search != "" || o.hash != "" || o.port == "" ||
		ep.RuntimeProtocol != 1 || ep.InstanceID == "" || ep.ProfileID == "" || !slices.Contains(ep.Capabilities, "session-authority-v1") {
		return errors.New("Invalid local gateway endpoint")
	}
	return nil
}

func isRestartWait(payload map[string]any) bool {
	return payload["state"] == "draining" && payload["reason_code"] == restartWaitReason && payloadEndpoint(payload) != nil
}

// StaleGatewayRestart requests the restart of a stale ready owner, done closes when the restart client exits
type StaleGatewayRestart func(endpoint map[string]any, stale StaleCode) (done <-chan struct{}, requested bool)

func reensureDuringRestart(run func() (EnsureResult, error), done <-chan struct{}, staleInstance string, poll time.Duration) (EnsureResult, map[string]any, error) {
	for {
		last := false
		select {
		case <-done:
			last = true
		default:
		}
		result, err := run()
		if err != nil {
			return result, nil, err
		}
		payload, err := ParseGatewayEnsureOutput(result)
		if err != nil {
			return result, nil, err
		}
		replaced := payload["state"] == "ready" && payloadString(payloadEndpoint(payload), "instance_id") != staleInstance
		if last || isRestartWait(payload) || replaced {
			return result, payload, nil
		}
		select {
		case <-done:
		case <-time.After(poll):
		}
	}
}

// Connection is one backend descriptor the renderer receives from getConnection
type Connection map[string]any

// Endpoint is the local gateway endpoint of a native descriptor, nil for remote ones
func (c Connection) Endpoint() *GatewayEndpoint {
	switch ep := c["gatewayEndpoint"].(type) {
	case GatewayEndpoint:
		return &ep
	case *GatewayEndpoint:
		return ep
	}
	return nil
}

// Clone copies the descriptor so overlays never write into a cached one
func (c Connection) Clone() Connection {
	out := make(Connection, len(c)+8)
	for k, v := range c {
		out[k] = v
	}
	return out
}

func (c Connection) str(key string) string {
	s, _ := c[key].(string)
	return s
}

// Runs the canonical ensure, restarts a stale owner once, and returns the native local descriptor
func EnsureLocalGateway(run func() (EnsureResult, error), beforeEnsure func() error, restartStale StaleGatewayRestart) (Connection, error) {
	return ensureLocalGateway(run, beforeEnsure, restartStale, StaleRestartPoll)
}

func ensureLocalGateway(run func() (EnsureResult, error), beforeEnsure func() error, restartStale StaleGatewayRestart, poll time.Duration) (Connection, error) {
	if beforeEnsure != nil {
		if err := beforeEnsure(); err != nil {
			return nil, err
		}
	}
	result, err := run()
	if err != nil {
		return nil, err
	}
	payload, err := ParseGatewayEnsureOutput(result)
	if err != nil {
		return nil, err
	}
	var stale *StaleCode
	if restartStale != nil && result.Code == 0 && payload["state"] == "ready" {
		stale = StaleGatewayCode(payload)
	}
	if stale != nil {
		if done, requested := restartStale(payloadEndpoint(payload), *stale); requested {
			result, payload, err = reensureDuringRestart(run, done, payloadString(payloadEndpoint(payload), "instance_id"), poll)
			if err != nil {
				return nil, err
			}
		}
	}
	if !isRestartWait(payload) && (result.Code != 0 || payload["state"] != "ready") {
		detail := ""
		if d, ok := payload["detail"].(string); ok && strings.TrimSpace(d) != "" {
			detail = " " + regexp.MustCompile(`\r?\n`).Split(strings.TrimSpace(d), 2)[0] + "."
		}
		recovery := "status"
		if payload["reason_code"] == "update_paused" {
			recovery = "restart"
		}
		state := payloadString(payload, "state")
		if state == "" {
			state = "inaccessible"
		}
		reason := payloadString(payload, "reason_code")
		if reason == "" {
			reason = "ensure_failed"
		}
		return nil, fmt.Errorf("Gateway %s (%s).%s Use icebath gateway %s for recovery.", state, reason, detail, recovery)
	}
	endpoint, err := decodeEndpoint(payloadEndpoint(payload))
	if err != nil {
		return nil, err
	}
	if err := assertLocalGatewayEndpoint(endpoint); err != nil {
		return nil, err
	}
	return Connection{
		"baseUrl":         endpoint.APIOrigin,
		"wsUrl":           "ws" + strings.TrimPrefix(endpoint.APIOrigin, "http") + "/api/ws?native_dial=unminted",
		"mode":            "local",
		"source":          "local",
		"authMode":        "native",
		"token":           "",
		"gatewayEndpoint": endpoint,
	}, nil
}

// Profile whose gateway restart replaces the process behind endpoint, the multiplexer root for a served secondary
func GatewayOwnerProfile(endpoint map[string]any) string {
	home := payloadString(endpoint, "control_home")
	if home == "" {
		home = payloadString(endpoint, "profile_id")
	}
	if filepath.Base(filepath.Dir(home)) == "profiles" {
		return filepath.Base(home)
	}
	return "default"
}

var unrecognizedDetach = regexp.MustCompile(`unrecognized arguments:.*--detach`)

// Runs gateway restart detached for owner, retrying without detach when an older runtime rejects the flag
func RunGatewayOwnerRestart(run func(args []string) (EnsureResult, error), ownerProfile string) (EnsureResult, error) {
	args := []string{"--profile", ownerProfile, "gateway", "restart"}
	result, err := run(append(slices.Clone(args), "--detach"))
	if err != nil {
		return result, err
	}
	if result.Code == 2 && unrecognizedDetach.MatchString(result.Stderr) {
		return run(args)
	}
	return result, nil
}

// Restarter with one restart per owner process and code pair, concurrent dials sharing it and later ones attaching
func CreateStaleGatewayRestarter(restart func(ownerProfile string) <-chan struct{}, log func(string)) StaleGatewayRestart {
	var mu sync.Mutex
	restarts := map[string]<-chan struct{}{}
	return func(endpoint map[string]any, stale StaleCode) (<-chan struct{}, bool) {
		owner := GatewayOwnerProfile(endpoint)
		key := strings.ToLower(owner + "\n" + stale.Gateway + "\n" + stale.Client)
		mu.Lock()
		defer mu.Unlock()
		if pending, ok := restarts[key]; ok {
			select {
			case <-pending:
				log(fmt.Sprintf("[gateway] owner still runs %s (this Icebath is %s) after a restart; attaching as-is", stale.Gateway, stale.Client))
				return nil, false
			default:
				return pending, true
			}
		}
		log(fmt.Sprintf("[gateway] owner runs %s but this Icebath is %s; restarting it", stale.Gateway, stale.Client))
		running := restart(owner)
		restarts[key] = running
		return running, true
	}
}

var staleControlErrors = map[string]bool{"Invalid gateway ticket response": true, "Invalid gateway control pointer": true, "Noncanonical gateway profile": true}

// Whether a dial failed because the cached endpoint names an owner that is gone or replaced
func IsStaleLocalGatewayError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.HasPrefix(message, "Gateway ticket") || staleControlErrors[message]
}

// Uses the ensured endpoint, and after one stale owner failure forgets it and re-ensures once
func RedialLocalGateway[E, R any](ensure func() (E, error), forget func() error, use func(E) (R, error)) (R, error) {
	var zero R
	first, err := ensure()
	if err != nil {
		return zero, err
	}
	out, err := use(first)
	if err == nil || !IsStaleLocalGatewayError(err) {
		return out, err
	}
	if err := forget(); err != nil {
		return zero, err
	}
	next, err := ensure()
	if err != nil {
		return zero, err
	}
	return use(next)
}

// LocalDial carries what a native socket dial needs from the ensured descriptor
type LocalDial struct {
	Ensure     func() (Connection, error)
	Forget     func() error
	LaunchHome string
	Mint       func(GatewayEndpoint, string) (string, error)
	Prepare    func(baseURL, ticket string) (string, error)
	Profile    string
	Purpose    string
}

// Mints a private grant for the ensured endpoint and prepares the socket URL, re-ensuring a stale owner once
func LocalGatewayDial(d LocalDial) (string, error) {
	mint := d.Mint
	if mint == nil {
		mint = MintLocalGatewayTicket
	}
	purpose := d.Purpose
	if purpose == "" {
		purpose = "interactive"
	}
	return RedialLocalGateway(d.Ensure, d.Forget, func(current Connection) (string, error) {
		ep := current.Endpoint()
		if ep == nil {
			return "", errors.New("Local gateway connection has no gateway endpoint")
		}
		routed, err := RoutedTicketEndpoint(*ep, d.Profile, d.LaunchHome, "")
		if err != nil {
			return "", err
		}
		ticket, err := mint(routed, purpose)
		if err != nil {
			return "", err
		}
		return d.Prepare(current.str("baseUrl"), ticket)
	})
}

var voiceDialPaths = map[string]bool{"/api/audio/speak-stream": true, "/api/audio/transcribe-stream": true}

type dialGrant struct {
	ticket   string
	windowID string
	expires  time.Time
	origin   string
	pathname string
	header   bool
}

// LocalGatewayDials reserves one use socket grants bound to the window that asked for them
type LocalGatewayDials struct {
	mu      sync.Mutex
	pending map[string]dialGrant
	now     func() time.Time
}

// Empty dial reservation table on the wall clock
func NewLocalGatewayDials() *LocalGatewayDials {
	return &LocalGatewayDials{pending: map[string]dialGrant{}, now: time.Now}
}

func randomUUID() string {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (d *LocalGatewayDials) reserve(u *url.URL, ticket, windowID string, header bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	for key, v := range d.pending {
		if !v.expires.After(now) {
			delete(d.pending, key)
		}
	}
	if len(d.pending) >= 100 {
		return "", errors.New("Too many pending gateway dials")
	}
	nonce := randomUUID()
	u.RawQuery = setSearchParam(u.RawQuery, "native_dial", nonce)
	o, err := parseWhatwg(u.String())
	if err != nil {
		return "", err
	}
	d.pending[nonce] = dialGrant{ticket: ticket, windowID: windowID, expires: now.Add(25 * time.Second), origin: whatwgOriginString(u), pathname: o.pathname, header: header}
	return u.String(), nil
}

func whatwgOriginString(u *url.URL) string {
	o, err := parseWhatwg(u.String())
	if err != nil {
		return ""
	}
	if o.port == "" {
		return strings.ToLower(u.Scheme) + "://" + o.hostname
	}
	return strings.ToLower(u.Scheme) + "://" + o.hostname + ":" + o.port
}

// Socket URL of the main gateway dial carrying its nonce and ticket
func (d *LocalGatewayDials) Prepare(origin, ticket, windowID string) (string, error) {
	u, err := url.Parse("ws" + strings.TrimPrefix(origin, "http") + "/api/ws")
	if err != nil {
		return "", err
	}
	reserved, err := d.reserve(u, ticket, windowID, false)
	if err != nil {
		return "", err
	}
	return reserved + "&ticket=" + EncodeURIComponent(ticket), nil
}

// Socket URL of a plugin dial whose ticket travels in a header
func (d *LocalGatewayDials) PreparePlugin(origin, path, ticket, windowID string) (string, error) {
	base, err := url.Parse("ws" + strings.TrimPrefix(origin, "http"))
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	return d.reserve(base.ResolveReference(ref), ticket, windowID, true)
}

// DialRequest is an outgoing socket handshake the shell offers for header rewriting
type DialRequest struct {
	URL            string
	WindowID       string
	ResourceType   string
	RequestHeaders map[string]string
}

// Handshake headers for a reserved dial, Origin dropped and the plugin ticket added, nil when not reserved
func (d *LocalGatewayDials) Headers(req DialRequest) map[string]string {
	u, err := url.Parse(req.URL)
	if err != nil {
		return nil
	}
	nonce := u.Query().Get("native_dial")
	if nonce == "" {
		return nil
	}
	o, err := parseWhatwg(req.URL)
	if err != nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	grant, ok := d.pending[nonce]
	if !ok {
		return nil
	}
	voice := !grant.header && grant.pathname == "/api/ws" && voiceDialPaths[o.pathname]
	if (grant.pathname != o.pathname && !voice) || grant.windowID != req.WindowID || req.ResourceType != "webSocket" ||
		(grant.header && grant.origin != whatwgOriginString(u)) {
		return nil
	}
	delete(d.pending, nonce)
	if !grant.expires.After(d.now()) {
		return nil
	}
	headers := map[string]string{}
	for k, v := range req.RequestHeaders {
		lower := strings.ToLower(k)
		if lower == "origin" || lower == strings.ToLower(GatewayTicketHeader) {
			continue
		}
		headers[k] = v
	}
	if grant.header {
		headers[GatewayTicketHeader] = grant.ticket
	}
	return headers
}

var profileCollectionRoutes = map[string][]string{
	"active":                 {"GET", "POST"},
	"import":                 {"POST"},
	"projects/tree":          {"GET"},
	"sessions":               {"GET"},
	"sessions/pull-requests": {"POST"},
	"sessions/sidebar":       {"GET"},
}

// Part of a profiles API path after /api/profiles/, empty for any other path
func ProfileRouteSuffix(pathname string) string {
	trimmed := strings.TrimRight(pathname, "/")
	if strings.HasPrefix(trimmed, "/api/profiles/") {
		return trimmed[len("/api/profiles/"):]
	}
	return ""
}

// Collection route a profiles path names for method, empty when it names a profile
func ProfileCollectionRoute(pathname, method string) string {
	suffix := ProfileRouteSuffix(pathname)
	methods, ok := profileCollectionRoutes[suffix]
	if suffix == "" || !ok {
		return ""
	}
	if method == "" || slices.Contains(methods, strings.ToUpper(method)) {
		return suffix
	}
	return ""
}

func scopedProfileOf(u *url.URL, method string) string {
	pathname := u.EscapedPath()
	collection := ProfileCollectionRoute(pathname, method)
	if collection == "sessions/sidebar" {
		return u.Query().Get("recents_profile")
	}
	segment, _, _ := strings.Cut(ProfileRouteSuffix(pathname), "/")
	if segment != "" && collection == "" {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return ""
		}
		return decoded
	}
	return u.Query().Get("profile")
}

var (
	urlLike       = regexp.MustCompile(`^[a-z]+://`)
	profileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// Endpoint a grant for urlOrProfile mints against, a sibling profile home under the same multiplexer root
func RoutedGatewayEndpoint(endpoint GatewayEndpoint, urlOrProfile, launchHome, method string) (GatewayEndpoint, error) {
	profile := urlOrProfile
	if urlLike.MatchString(urlOrProfile) {
		u, err := url.Parse(urlOrProfile)
		if err != nil {
			return endpoint, errors.New("Invalid URL")
		}
		profile = scopedProfileOf(u, method)
	}
	profile = strings.TrimSpace(profile)
	if profile == "" || profile == "current" {
		return endpoint, nil
	}
	underProfiles := filepath.Base(filepath.Dir(endpoint.ProfileID)) == "profiles"
	ownName := "default"
	if underProfiles {
		ownName = filepath.Base(endpoint.ProfileID)
	}
	if profile == ownName {
		return endpoint, nil
	}
	if !profileNameRe.MatchString(profile) {
		return endpoint, errors.New("Invalid profile route")
	}
	root := endpoint.ProfileID
	if underProfiles {
		root = filepath.Dir(filepath.Dir(endpoint.ProfileID))
	}
	home := root
	if profile != "default" {
		home = filepath.Join(root, "profiles", profile)
	}
	control := endpoint.controlHome()
	routed := endpoint
	routed.ProfileID = home
	routed.ControlHome = &control
	return routed, nil
}

// Routed endpoint with a sibling profile home canonicalised, naming a profile that does not exist
func RoutedTicketEndpoint(endpoint GatewayEndpoint, urlOrProfile, launchHome, method string) (GatewayEndpoint, error) {
	routed, err := RoutedGatewayEndpoint(endpoint, urlOrProfile, launchHome, method)
	if err != nil {
		return routed, err
	}
	if routed.ProfileID == endpoint.ProfileID || (routed.ControlHome != nil && routed.ProfileID == *routed.ControlHome) {
		return routed, nil
	}
	home, err := ReservedHome(routed.ProfileID)
	if err != nil {
		if isMissingNode(err) {
			return routed, fmt.Errorf("Profile '%s' does not exist: no directory at %s", filepath.Base(routed.ProfileID), routed.ProfileID)
		}
		return routed, err
	}
	routed.ProfileID = home
	return routed, nil
}

// Grant purpose of a native HTTP request, the profile create and registry writes named apart
func NativeGrantPurpose(rawURL, method string) (string, error) {
	if method == "" {
		method = "GET"
	}
	verb := strings.ToUpper(method)
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", errors.New("Invalid URL")
	}
	pathname := strings.TrimRight(u.EscapedPath(), "/")
	if verb == "POST" && pathname == "/api/profiles" {
		return "native-profile-create", nil
	}
	suffix := ProfileRouteSuffix(pathname)
	if (verb == "PATCH" || verb == "DELETE") && suffix != "" && !strings.Contains(suffix, "/") && ProfileCollectionRoute(pathname, verb) == "" {
		return "native-profile-registry", nil
	}
	return "native-http", nil
}

// Route a native grant mints for, the body profile when the URL names none
func NativeGrantRoute(rawURL string, body any, method string) string {
	if u, err := url.Parse(rawURL); err == nil && strings.TrimSpace(scopedProfileOf(u, method)) != "" {
		return rawURL
	}
	if obj, ok := body.(map[string]any); ok {
		if profile, ok := obj["profile"].(string); ok && strings.TrimSpace(profile) != "" {
			return profile
		}
	}
	return rawURL
}

// Ticket header for a native gateway HTTP call, refused when the URL is not this endpoint origin
func NativeGatewayHTTPHeaders(endpoint GatewayEndpoint, baseURL, rawURL, launchHome, method string, body any) (map[string]string, error) {
	if launchHome == "" {
		launchHome = endpoint.ProfileID
	}
	if method == "" {
		method = "GET"
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("Invalid URL")
	}
	if whatwgOriginString(u) != endpoint.APIOrigin || baseURL != endpoint.APIOrigin {
		return nil, errors.New("Native gateway HTTP origin mismatch")
	}
	purpose, err := NativeGrantPurpose(rawURL, method)
	if err != nil {
		return nil, err
	}
	routed, err := RoutedTicketEndpoint(endpoint, NativeGrantRoute(rawURL, body, method), launchHome, method)
	if err != nil {
		return nil, err
	}
	ticket, err := MintLocalGatewayTicket(routed, purpose)
	if err != nil {
		return nil, err
	}
	return map[string]string{GatewayTicketHeader: ticket}, nil
}

// Scope a grant of purpose asks for, host wide only for the interactive socket
func TicketScope(purpose string) string {
	if purpose == "interactive" {
		return "host"
	}
	return ""
}

// TicketReasonError carries the reason code a gateway ticket refusal names
type TicketReasonError interface {
	error
	TicketReason() string
}

var (
	ticketClientMu sync.Mutex
	ticketClient   func(endpoint GatewayEndpoint, purpose string) (string, error)
)

// Installs the gateway control client that mints where the socket checks cannot vouch for the home, nil removing it
func ConfigureGatewayTicketClient(client func(endpoint GatewayEndpoint, purpose string) (string, error)) {
	ticketClientMu.Lock()
	ticketClient = client
	ticketClientMu.Unlock()
}

func configuredTicketClient() func(GatewayEndpoint, string) (string, error) {
	ticketClientMu.Lock()
	defer ticketClientMu.Unlock()
	return ticketClient
}

func mintGroupWritableHome(endpoint GatewayEndpoint, purpose string, homes []string) (string, error) {
	quoted := make([]string, len(homes))
	for i, h := range homes {
		quoted[i] = shellQuote(h)
	}
	client := configuredTicketClient()
	if client == nil {
		return "", fmt.Errorf("Unsafe gateway control path: profile home is group-writable and this Desktop cannot verify the group is private: run chmod g-w %s", strings.Join(quoted, " "))
	}
	ticket, err := client(endpoint, purpose)
	if err == nil {
		return ticket, nil
	}
	var reason TicketReasonError
	if !errors.As(err, &reason) || reason.TicketReason() != "unsafe_control_permissions" {
		return "", err
	}
	return "", fmt.Errorf("Profile home is writable by a group other accounts share: run chmod g-w %s", strings.Join(quoted, " "))
}

func isMissingNode(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func shellQuote(file string) string {
	return "'" + strings.ReplaceAll(file, "'", `'\''`) + "'"
}

// Query string with key set once at its first position or appended, serialised like URLSearchParams
func setSearchParam(rawQuery, key, value string) string {
	var pairs [][2]string
	placed := false
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		k, _ = url.QueryUnescape(k)
		v, _ = url.QueryUnescape(v)
		if k == key {
			if placed {
				continue
			}
			v = value
			placed = true
		}
		pairs = append(pairs, [2]string{k, v})
	}
	if !placed {
		pairs = append(pairs, [2]string{key, value})
	}
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = url.QueryEscape(p[0]) + "=" + url.QueryEscape(p[1])
	}
	return strings.Join(out, "&")
}

// Percent encodes s exactly as JavaScript encodeURIComponent does
func EncodeURIComponent(s string) string {
	const keep = "-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte(keep, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
