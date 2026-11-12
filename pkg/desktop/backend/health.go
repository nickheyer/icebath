package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBackendReadyTimeout  = 180 * time.Second
	DefaultBackendReadyPoll     = 500 * time.Millisecond
	DefaultHealthProbeTimeout   = 5 * time.Second
	RemoteSessionExpiredMessage = "Your remote gateway session has expired. Open Settings → Gateway and click \"Sign in\" again."
	RemoteUnsignedOauthMessage  = "Remote Icebath gateway uses OAuth, but you are not signed in. Open Settings → Gateway and click \"Sign in\", or switch back to Local."
)

// HTTPError is a failed request carrying its status code.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string { return e.Message }

// ConnRefusedError marks a connection refused by the backend port.
type ConnRefusedError struct{ Err error }

func (e *ConnRefusedError) Error() string { return e.Err.Error() }
func (e *ConnRefusedError) Unwrap() error { return e.Err }

// IsConnectionRefusedError reports an ECONNREFUSED failure
func IsConnectionRefusedError(err error) bool {
	var c *ConnRefusedError
	return errors.As(err, &c)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var statusPrefix = regexp.MustCompile(`^(\d{3}):`)

// ServerSideError is a 502, 503 or 504 with its detail.
type ServerSideError struct {
	StatusCode int
	Detail     string
}

// IsServerSideHTTPError classifies 502, 503 and 504 from a structured status or a leading status prefix
func IsServerSideHTTPError(err error) *ServerSideError {
	if err == nil {
		return nil
	}
	var h *HTTPError
	if errors.As(err, &h) {
		if h.StatusCode == 502 || h.StatusCode == 503 || h.StatusCode == 504 {
			return &ServerSideError{StatusCode: h.StatusCode, Detail: err.Error()}
		}
	}
	m := statusPrefix.FindStringSubmatch(err.Error())
	if m == nil {
		return nil
	}
	code, _ := strconv.Atoi(m[1])
	if code == 502 || code == 503 || code == 504 {
		return &ServerSideError{StatusCode: code, Detail: err.Error()}
	}
	return nil
}

// CloudBackendDownError explains a Nous Cloud agent answering with a server side fault.
type CloudBackendDownError struct {
	Message    string
	StatusCode int
	Detail     string
	Cause      error
}

func (e *CloudBackendDownError) Error() string { return e.Message }
func (e *CloudBackendDownError) Unwrap() error { return e.Cause }

// IsNousCloudAgentURL reports a hosted Nous agent URL
func IsNousCloudAgentURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.HasSuffix(u.Hostname(), ".agents.nousresearch.com")
}

// MakeNousCloudBackendDownError builds the actionable cloud outage error for a server side fault of a cloud agent
func MakeNousCloudBackendDownError(baseURL string, err error) error {
	if !IsNousCloudAgentURL(baseURL) {
		return nil
	}
	se := IsServerSideHTTPError(err)
	if se == nil {
		return nil
	}
	host := baseURL
	if u, perr := url.Parse(baseURL); perr == nil {
		host = u.Hostname()
	}
	detail := errText(err)
	return &CloudBackendDownError{
		Message:    fmt.Sprintf("Nous Cloud agent %s is down (HTTP %d: server-side fault). Check https://portal.nousresearch.com for backend status, or switch to Local mode in Settings → Gateway. You can also reach out on Discord at discord.gg/NousResearch for immediate assistance. Original detail: %s", host, se.StatusCode, detail),
		StatusCode: se.StatusCode, Detail: detail, Cause: err,
	}
}

var authRejection = regexp.MustCompile(`^40[13]:`)

// IsMissingHealthEndpointError reports a 404 or a missing endpoint hint
func IsMissingHealthEndpointError(err error) bool {
	m := errText(err)
	return strings.HasPrefix(m, "404:") || strings.Contains(m, "endpoint is likely missing")
}

// IsAuthRejectionError reports a 401 or 403
func IsAuthRejectionError(err error) bool { return authRejection.MatchString(errText(err)) }

// IsGatedMissingHealthError reports an auth rejection caused by a missing cookie
func IsGatedMissingHealthError(err error) bool {
	return IsAuthRejectionError(err) && strings.Contains(errText(err), "no_cookie")
}

// ReauthError asks the user to sign in to the remote gateway again.
type ReauthError struct {
	Message string
	Detail  string
}

func (e *ReauthError) Error() string { return e.Message }

// NeedsOauthLogin is always true for a reauth error
func (e *ReauthError) NeedsOauthLogin() bool { return true }

// MakeReauthRequiredError is the expired session error with an optional detail
func MakeReauthRequiredError(detail string) error {
	return &ReauthError{Message: RemoteSessionExpiredMessage, Detail: detail}
}

// MakeUnsignedOauthError is the never signed in error
func MakeUnsignedOauthError() error { return &ReauthError{Message: RemoteUnsignedOauthMessage} }

// IsReauthRequiredError reports a reauth error
func IsReauthRequiredError(err error) bool {
	var r *ReauthError
	return errors.As(err, &r)
}

// SupersededError marks an SSH bootstrap a newer connection setting replaced.
type SupersededError struct{}

func (SupersededError) Error() string {
	return "SSH bootstrap was superseded by newer connection settings."
}

// Kind is superseded
func (SupersededError) Kind() string { return "superseded" }

// ReadyOptions configures the readiness poll.
type ReadyOptions struct {
	FetchJSON           func(ctx context.Context, url, token string) error
	ProbeHealth         func(ctx context.Context, url string, timeout time.Duration) error
	Token               string
	Ctx                 context.Context
	Timeout             time.Duration
	Poll                time.Duration
	HealthProbeTimeout  time.Duration
	Sleep               func(ctx context.Context, d time.Duration) error
	Now                 func() time.Time
	ProbeIsCredentialed bool
	AlreadyBound        bool
}

// WaitForHermesReady polls /api/health, falling back to /api/status, until ready, a terminal error or the deadline
func WaitForHermesReady(baseURL string, o ReadyOptions) error {
	ctx := o.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	timeout, poll, probeTimeout := o.Timeout, o.Poll, o.HealthProbeTimeout
	if timeout == 0 {
		timeout = DefaultBackendReadyTimeout
	}
	if poll == 0 {
		poll = DefaultBackendReadyPoll
	}
	if probeTimeout == 0 {
		probeTimeout = DefaultHealthProbeTimeout
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return SupersededError{}
			}
		}
	}
	base := strings.TrimRight(baseURL, "/")
	deadline := now().Add(timeout)
	var lastErr error
	statusFallback := false
	for now().Before(deadline) {
		if ctx.Err() != nil {
			return SupersededError{}
		}
		var err error
		if statusFallback {
			err = o.FetchJSON(ctx, base+"/api/status", o.Token)
		} else {
			err = o.ProbeHealth(ctx, base+"/api/health", probeTimeout)
		}
		if err == nil {
			return nil
		}
		lastErr = err
		if o.ProbeIsCredentialed && IsAuthRejectionError(err) {
			return MakeReauthRequiredError(err.Error())
		}
		if o.AlreadyBound && IsConnectionRefusedError(err) {
			return fmt.Errorf("Icebath backend did not become ready: %s", err.Error())
		}
		if !statusFallback && (IsMissingHealthEndpointError(err) || IsGatedMissingHealthError(err)) {
			statusFallback = true
			continue
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
	detail := "timeout"
	if lastErr != nil {
		detail = lastErr.Error()
	}
	if cloud := MakeNousCloudBackendDownError(baseURL, lastErr); cloud != nil {
		return cloud
	}
	return fmt.Errorf("Icebath backend did not become ready: %s", detail)
}

// OwnershipEntry is one backend the desktop owns in backend-ownership.json.
type OwnershipEntry struct {
	Nonce             string  `json:"nonce"`
	Pid               int     `json:"pid"`
	Profile           string  `json:"profile"`
	StartMarker       string  `json:"startMarker"`
	Command           *string `json:"command,omitempty"`
	ParentPid         int     `json:"parentPid,omitempty"`
	ParentStartMarker string  `json:"parentStartMarker,omitempty"`
}

func (e OwnershipEntry) sameIdentity(o OwnershipEntry) bool {
	return e.Pid == o.Pid && e.StartMarker == o.StartMarker && e.Nonce == o.Nonce && e.Profile == o.Profile
}

func (e OwnershipEntry) complete() bool {
	return e.Pid > 0 && e.StartMarker != "" && e.Nonce != "" && e.Profile != ""
}

func entryFrom(v any) (OwnershipEntry, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return OwnershipEntry{}, false
	}
	pid, okPid := jsonInteger(m["pid"])
	str := func(k string) string { s, _ := m[k].(string); return s }
	e := OwnershipEntry{Nonce: str("nonce"), Pid: pid, Profile: str("profile"), StartMarker: str("startMarker")}
	if !okPid || !e.complete() {
		return OwnershipEntry{}, false
	}
	if c, ok := m["command"].(string); ok {
		e.Command = &c
	}
	if pp, ok := jsonInteger(m["parentPid"]); ok && pp > 0 {
		e.ParentPid = pp
	}
	e.ParentStartMarker = str("parentStartMarker")
	return e, true
}

// ParseBackendOwnershipDetailed reads the ownership file, flagging unparseable JSON as corrupt
func ParseBackendOwnershipDetailed(contents string) (entries []OwnershipEntry, corrupt bool) {
	entries = []OwnershipEntry{}
	if strings.TrimSpace(contents) == "" {
		return entries, false
	}
	var parsed any
	if json.Unmarshal([]byte(contents), &parsed) != nil {
		return entries, true
	}
	var values []any
	switch t := parsed.(type) {
	case []any:
		values = t
	case map[string]any:
		values, _ = t["backends"].([]any)
	}
	for _, v := range values {
		e, ok := entryFrom(v)
		if !ok {
			continue
		}
		dup := false
		for _, x := range entries {
			if x.sameIdentity(e) {
				dup = true
			}
		}
		if !dup {
			entries = append(entries, e)
		}
	}
	return entries, false
}

// ParseBackendOwnership returns the complete unique entries of the ownership file
func ParseBackendOwnership(contents string) []OwnershipEntry {
	e, _ := ParseBackendOwnershipDetailed(contents)
	return e
}

// SerializeBackendOwnership writes the backends object with two space indent and a newline
func SerializeBackendOwnership(entries []OwnershipEntry) string {
	if entries == nil {
		entries = []OwnershipEntry{}
	}
	b, _ := json.MarshalIndent(map[string]any{"backends": entries}, "", "  ")
	return string(b) + "\n"
}

// OwnershipStore persists the ownership file.
type OwnershipStore interface {
	Read() (string, error)
	Write(contents string) error
	Quarantine() error
}

// OwnershipDeps verify and stop owned backends.
type OwnershipDeps struct {
	MatchesIdentity func(OwnershipEntry) (*bool, error)
	MatchesParent   func(OwnershipEntry) (*bool, error)
	Stop            func(OwnershipEntry) error
	Store           OwnershipStore
	ReapDeadline    time.Duration
	Now             func() time.Time
}

const ReapOrphansDeadline = 5 * time.Second

// Ownership records the backends this desktop spawned so a crashed run can reap them.
type Ownership struct {
	d  OwnershipDeps
	mu sync.Mutex
}

// NewOwnership binds ownership to its store and probes
func NewOwnership(d OwnershipDeps) *Ownership { return &Ownership{d: d} }

func (o *Ownership) read() ([]OwnershipEntry, bool) {
	raw, _ := o.d.Store.Read()
	return ParseBackendOwnershipDetailed(raw)
}

// Claim records a complete identity, stopping the child when the record cannot be written
func (o *Ownership) Claim(c OwnershipEntry) (OwnershipEntry, error) {
	if !c.complete() {
		return OwnershipEntry{}, errors.New("Cannot own a backend without a complete process identity.")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	cur, _ := o.read()
	next := []OwnershipEntry{}
	for _, e := range cur {
		if e.Pid != c.Pid {
			next = append(next, e)
		}
	}
	if err := o.d.Store.Write(SerializeBackendOwnership(append(next, c))); err != nil {
		o.d.Stop(c)
		return OwnershipEntry{}, err
	}
	return c, nil
}

// Release forgets an identity, writing only when it was recorded
func (o *Ownership) Release(id OwnershipEntry) error {
	if !id.complete() {
		return errors.New("Cannot release a backend without a complete process identity.")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	cur, _ := o.read()
	next := []OwnershipEntry{}
	for _, e := range cur {
		if !e.sameIdentity(id) {
			next = append(next, e)
		}
	}
	if len(next) != len(cur) {
		return o.d.Store.Write(SerializeBackendOwnership(next))
	}
	return nil
}

// ReapOrphans stops recorded backends whose parent desktop is gone and whose identity still matches, within a deadline
func (o *Ownership) ReapOrphans() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	entries, corrupt := o.read()
	if corrupt {
		o.d.Store.Quarantine()
		return []int{}
	}
	now := o.d.Now
	if now == nil {
		now = time.Now
	}
	limit := o.d.ReapDeadline
	if limit == 0 {
		limit = ReapOrphansDeadline
	}
	deadline := now().Add(limit)
	survivors, reaped := []OwnershipEntry{}, []int{}
	for i, e := range entries {
		if !now().Before(deadline) {
			survivors = append(survivors, entries[i:]...)
			break
		}
		parent, err := o.d.MatchesParent(e)
		if err != nil || (parent != nil && *parent) {
			survivors = append(survivors, e)
			continue
		}
		matches, err := o.d.MatchesIdentity(e)
		if err != nil {
			survivors = append(survivors, e)
			continue
		}
		if matches != nil && !*matches {
			continue
		}
		if matches == nil {
			survivors = append(survivors, e)
			continue
		}
		if err := o.d.Stop(e); err != nil {
			survivors = append(survivors, e)
			continue
		}
		reaped = append(reaped, e.Pid)
	}
	o.d.Store.Write(SerializeBackendOwnership(survivors))
	return reaped
}

// Clear empties the ownership file
func (o *Ownership) Clear() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.d.Store.Write(SerializeBackendOwnership(nil))
}

var backendCommand = regexp.MustCompile(`(?i)(?:^|[\s/\\"])(?:icebath(?:\.exe)?)"?(?:\s+(?:--profile|-p)\s+\S+)?\s+(?:serve|dashboard)(?:\s|$)`)

// BackendCommandMatches recognizes an icebath serve or dashboard command line
func BackendCommandMatches(command string) bool { return backendCommand.MatchString(command) }

// ShutdownCoordinator runs a teardown once and lets every caller wait on it.
type ShutdownCoordinator struct {
	mu       sync.Mutex
	teardown func() error
	done     chan struct{}
	err      error
}

// NewShutdownCoordinator wraps teardown
func NewShutdownCoordinator(teardown func() error) *ShutdownCoordinator {
	return &ShutdownCoordinator{teardown: teardown}
}

// Run starts the teardown once and waits for it
func (c *ShutdownCoordinator) Run() error {
	c.mu.Lock()
	if c.done == nil {
		c.done = make(chan struct{})
		go func() {
			defer close(c.done)
			defer func() {
				if r := recover(); r != nil {
					c.err = fmt.Errorf("%v", r)
				}
			}()
			c.err = c.teardown()
		}()
	}
	done := c.done
	c.mu.Unlock()
	<-done
	return c.err
}

// HasStarted reports a started teardown
func (c *ShutdownCoordinator) HasStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done != nil
}

// IsPending reports a teardown that started and has not finished
func (c *ShutdownCoordinator) IsPending() bool {
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

const (
	DefaultProbeTimeout        = 15 * time.Second
	DefaultPortAnnounceTimeout = 90 * time.Second
	MinPortAnnounceTimeout     = 45 * time.Second
	sourceCompletionGrace      = 5 * time.Minute
	sourceCompletionMaxTotal   = 30 * time.Minute
)

// ResolveProbeTimeout reads ICEBATH_PROBE_TIMEOUT_MS, positive values capped at 120 s, else the default
func ResolveProbeTimeout(env func(string) string) time.Duration {
	raw := env("ICEBATH_PROBE_TIMEOUT_MS")
	if raw == "" {
		return DefaultProbeTimeout
	}
	n, ok := leadingInt(raw)
	if !ok || n <= 0 {
		return DefaultProbeTimeout
	}
	return time.Duration(min(n, 120_000)) * time.Millisecond
}

func leadingInt(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\n\r")
	end := 0
	if end < len(s) && (s[end] == '-' || s[end] == '+') {
		end++
	}
	start := end
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == start {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	return n, err == nil
}

// ProbeError is a failed runtime probe.
type ProbeError struct {
	Command  string
	Code     int
	Signal   string
	Killed   bool
	TimedOut bool
}

func (e *ProbeError) Error() string {
	reason := e.Signal
	if reason == "" {
		reason = strconv.Itoa(e.Code)
	}
	return fmt.Sprintf("Runtime probe failed: %s (%s)", e.Command, reason)
}

// IsTimeoutError reports a probe killed by its timeout
func IsTimeoutError(err error) bool {
	var p *ProbeError
	return errors.As(err, &p) && (p.Killed || p.TimedOut || p.Signal == "SIGTERM")
}

// ExecProbe runs command once more after a timeout, failing on a non zero exit
func ExecProbe(command string, args []string, timeout time.Duration) error {
	run := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, command, args...)
		hideWindow(cmd)
		err := cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			return &ProbeError{Command: command, Signal: "SIGTERM", Killed: true, TimedOut: true}
		}
		if err == nil {
			return nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &ProbeError{Command: command, Code: ee.ExitCode()}
		}
		return err
	}
	err := run()
	if err == nil || !IsTimeoutError(err) {
		return err
	}
	return run()
}

// ShouldTrustHermesOverride trusts a non blank override path
func ShouldTrustHermesOverride(override string) bool { return strings.TrimSpace(override) != "" }

// VerifyHermesCli reports whether command answers --version
func VerifyHermesCli(command string, timeout time.Duration) bool {
	if command == "" {
		return false
	}
	return ExecProbe(command, []string{"--version"}, timeout) == nil
}

var readyLine = regexp.MustCompile(`^ICEBATH_(?:BACKEND|DASHBOARD)_READY port=(\d+)`)
var readyMerged = regexp.MustCompile(`ICEBATH_(?:BACKEND|DASHBOARD)_READY port=(\d+)`)
var sourceCompletionBanner = regexp.MustCompile(`finishing an interrupted source update|completing source-update dependencies`)

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// ReadyPortInMergedOutput finds a ready sentinel not glued to a preceding word character
func ReadyPortInMergedOutput(out string) (int, bool) {
	for _, loc := range readyMerged.FindAllStringSubmatchIndex(out, -1) {
		if loc[0] > 0 && isWordByte(out[loc[0]-1]) {
			continue
		}
		n, _ := strconv.Atoi(out[loc[2]:loc[3]])
		return n, true
	}
	return 0, false
}

// ResolvePortAnnounceTimeout reads ICEBATH_DESKTOP_PORT_ANNOUNCE_TIMEOUT_MS with a 45 s floor
func ResolvePortAnnounceTimeout(env func(string) string) time.Duration {
	v, err := strconv.ParseFloat(strings.TrimSpace(env("ICEBATH_DESKTOP_PORT_ANNOUNCE_TIMEOUT_MS")), 64)
	if err == nil && v > 0 {
		ms := max(float64(MinPortAnnounceTimeout.Milliseconds()), float64(int64(v+0.5)))
		return time.Duration(ms) * time.Millisecond
	}
	return DefaultPortAnnounceTimeout
}

// AnnounceSource is the backend child whose stdout announces the port.
type AnnounceSource struct {
	Stdout     io.Reader
	Exited     <-chan string
	Buffered   func() string
	OutputTail func() string
}

// WaitForDashboardPort reads stdout lines for the ready sentinel, extending the deadline while a source update completes
func WaitForDashboardPort(src AnnounceSource, timeout time.Duration, now func() time.Time) (int, error) {
	if now == nil {
		now = time.Now
	}
	tail := func() string {
		if src.OutputTail == nil {
			return ""
		}
		return src.OutputTail()
	}
	started := now()
	completing := false
	deadline := func() (time.Time, bool) {
		if completing {
			a, b := started.Add(sourceCompletionMaxTotal), now().Add(sourceCompletionGrace)
			if a.Before(b) {
				return a, true
			}
			return b, true
		}
		return started.Add(timeout), false
	}
	if src.Buffered != nil {
		if out := src.Buffered(); out != "" {
			if port, ok := ReadyPortInMergedOutput(out); ok {
				return port, nil
			}
			if sourceCompletionBanner.MatchString(out) {
				completing = true
			}
		}
	}
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(src.Stdout)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for {
		end, extended := deadline()
		timer := time.NewTimer(max(0, end.Sub(now())))
		select {
		case line, ok := <-lines:
			timer.Stop()
			if !ok {
				lines = nil
				continue
			}
			if m := readyLine.FindStringSubmatch(line); m != nil {
				port, _ := strconv.Atoi(m[1])
				return port, nil
			}
			if sourceCompletionBanner.MatchString(line) {
				completing = true
			}
		case reason := <-src.Exited:
			timer.Stop()
			return 0, fmt.Errorf("Icebath backend: exited before port announcement (%s)%s", reason, tail())
		case <-timer.C:
			suffix := ""
			if extended {
				suffix = " while an update completion was in progress"
			}
			return 0, fmt.Errorf("Timed out waiting for Icebath backend port announcement (%dms)%s", timeout.Milliseconds(), suffix)
		}
	}
}

// ReadDashboardReadyFile returns the positive integer port of a ready file or zero
func ReadDashboardReadyFile(name string) int {
	if name == "" {
		return 0
	}
	raw, err := os.ReadFile(name)
	if err != nil {
		return 0
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return 0
	}
	if f, ok := v["port"].(float64); ok && f > 0 && f == float64(int64(f)) {
		return int(f)
	}
	if s, ok := v["port"].(string); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// WaitForDashboardReadyFile polls the ready file every 50 ms until a port, the child exit or the timeout
func WaitForDashboardReadyFile(name string, src AnnounceSource, timeout time.Duration) (int, error) {
	if port := ReadDashboardReadyFile(name); port > 0 {
		return port, nil
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-tick.C:
			if port := ReadDashboardReadyFile(name); port > 0 {
				return port, nil
			}
		case reason := <-src.Exited:
			tail := ""
			if src.OutputTail != nil {
				tail = src.OutputTail()
			}
			return 0, fmt.Errorf("Icebath backend: exited before port announcement (%s)%s", reason, tail)
		case <-deadline.C:
			return 0, fmt.Errorf("Timed out waiting for Icebath backend port announcement (%dms)", timeout.Milliseconds())
		}
	}
}

// WaitForDashboardPortAnnouncement waits on the ready file when one is given and on stdout otherwise
func WaitForDashboardPortAnnouncement(src AnnounceSource, readyFile string, timeout time.Duration) (int, error) {
	if readyFile != "" {
		return WaitForDashboardReadyFile(readyFile, src, timeout)
	}
	return WaitForDashboardPort(src, timeout, time.Now)
}

// BackendStartFailureContext describes a failed local start.
type BackendStartFailureContext struct {
	AttemptedRemote    bool
	SupervisorRecovery bool
}

// ShouldLatchBackendStartFailure latches a local start failure outside supervisor recovery
func ShouldLatchBackendStartFailure(c BackendStartFailureContext) bool {
	return !c.AttemptedRemote && !c.SupervisorRecovery
}

// RemoteBootRetryContext describes a failed remote boot.
type RemoteBootRetryContext struct {
	AttemptedRemote   bool
	IsReauth          bool
	IsHostKeyChanged  bool
	IsSSHAuthFailed   bool
	IsSSHClientFailed bool
}

// ShouldLatchRemoteReauthFailure latches a confirmed remote reauth rejection
func ShouldLatchRemoteReauthFailure(c RemoteBootRetryContext) bool {
	return c.AttemptedRemote && c.IsReauth
}

// KindError tags an error with a failure kind.
type KindError struct {
	Kind     string
	SSHError string
	Message  string
}

func (e *KindError) Error() string { return e.Message }

func kindOf(err error) (string, string) {
	var k *KindError
	if errors.As(err, &k) {
		return k.Kind, k.SSHError
	}
	return "", ""
}

var hostKeyChanged = regexp.MustCompile(`(?i)REMOTE HOST IDENTIFICATION HAS CHANGED|Host key verification failed|host key for .+ has CHANGED`)
var sshAuthFailed = regexp.MustCompile(`(?i)SSH authentication to .+ failed|Permission denied \((?:publickey|password|keyboard-interactive)`)

// IsHostKeyChangedBootFailure recognizes a changed SSH host key
func IsHostKeyChangedBootFailure(err error) bool {
	if k, _ := kindOf(err); k == "host-key-changed" {
		return true
	}
	return hostKeyChanged.MatchString(errText(err))
}

// IsSSHAuthFailedBootFailure recognizes rejected SSH credentials
func IsSSHAuthFailedBootFailure(err error) bool {
	if k, s := kindOf(err); k == "auth-failed" || s == "auth-failed" {
		return true
	}
	return sshAuthFailed.MatchString(errText(err))
}

const SSHClientFailed = "ssh-client-failed"

// SSHClientFailedError explains a local SSH client that could not resolve the connection config
func SSHClientFailedError(sshBinary string, cause error, platform string) error {
	hint := ""
	if platform == "win32" {
		hint = " Set desktop.ssh_path in config.yaml to a working ssh.exe (for example Git for Windows' usr\\bin\\ssh.exe) and retry."
	}
	return &KindError{Kind: SSHClientFailed, Message: fmt.Sprintf("The local SSH client (%s) failed to resolve the connection config: %s.%s", sshBinary, errText(cause), hint)}
}

// IsSSHClientFailedBootFailure recognizes the SSH client failure
func IsSSHClientFailedBootFailure(err error) bool {
	k, _ := kindOf(err)
	return k == SSHClientFailed
}

// ShouldLatchSSHClientFailure latches a remote SSH client failure
func ShouldLatchSSHClientFailure(c RemoteBootRetryContext) bool {
	return c.AttemptedRemote && c.IsSSHClientFailed
}

// ShouldLatchSSHAuthFailure latches a remote SSH auth failure
func ShouldLatchSSHAuthFailure(c RemoteBootRetryContext) bool {
	return c.AttemptedRemote && c.IsSSHAuthFailed
}

// ShouldLatchHostKeyChangedFailure latches a remote host key change
func ShouldLatchHostKeyChangedFailure(c RemoteBootRetryContext) bool {
	return c.AttemptedRemote && c.IsHostKeyChanged
}

// IsRetryableRemoteBootFailure retries a remote failure that is none of the terminal kinds
func IsRetryableRemoteBootFailure(c RemoteBootRetryContext) bool {
	return c.AttemptedRemote && !c.IsReauth && !c.IsHostKeyChanged && !c.IsSSHAuthFailed && !c.IsSSHClientFailed
}

// ShouldHoldBootProgressForReauth keeps a latched reauth message on screen against a different error update
func ShouldHoldBootProgressForReauth(latched string, updateError *string) bool {
	if latched == "" {
		return false
	}
	return updateError == nil || *updateError != latched
}
