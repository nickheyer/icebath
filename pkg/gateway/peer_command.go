package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	term "github.com/charmbracelet/x/term"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	peerBotChatTitle          = "Bot Chat"
	peerDefaultProfileHandle  = "icebath"
	peerUnknownProfileError   = "Unknown or unconfigured profile"
	peerListTimeout           = 30 * time.Second
	peerChatQueuedObject      = "hermes.session.chat.queued"
	peerRedirectLoopPrefix    = "The HTTP server returned a redirect error that would lead to an infinite loop.\nThe last 30x error message was:\n"
	peerRemoteDisconnectedMsg = "Remote end closed connection without response"
)

var (
	peerNameRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	peerProfileRe     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	peerURLTypeRe     = regexp.MustCompile(`(?s)^([^/:]+):(.*)$`)
	peerURLHostRe     = regexp.MustCompile(`(?s)^//([^/#?]*)(.*)$`)
	peerHeaderValueRe = regexp.MustCompile("\n[^ \t]|\n$|\r[^ \t\n]|\r$")
)

// One synchronous agent turn can legitimately take minutes
var peerDMTimeout = 600 * time.Second

// Process streams a peer invocation prints to and reads its message from
var (
	peerStderr      io.Writer = os.Stderr
	peerStdin       io.Reader = os.Stdin
	peerStdinIsatty           = func() bool { return term.IsTerminal(os.Stdin.Fd()) }
)

// Home, context and streams one peer command runs against
type peerCLI struct {
	ctx         context.Context
	home        string
	out         io.Writer
	errOut      io.Writer
	stdin       io.Reader
	stdinIsatty func() bool
}

func (c *peerCLI) println(s string)  { fmt.Fprintln(c.out, s) }
func (c *peerCLI) eprintln(s string) { fmt.Fprintln(c.errOut, s) }

// Python urllib HTTPError, a non success status whose body reads once like the response file it wraps
type peerHTTPError struct {
	code    int
	msg     string
	body    []byte
	bodyErr error
	drained bool
}

func (e *peerHTTPError) Error() string { return fmt.Sprintf("HTTP Error %d: %s", e.code, e.msg) }

// Body bytes on the first read and nothing afterwards, a failed read reporting its exception
func (e *peerHTTPError) read() ([]byte, error) {
	if e.drained {
		return nil, nil
	}
	e.drained = true
	return e.body, e.bodyErr
}

// Python urllib URLError, the reason printed inside the urlopen error brackets
type peerURLError struct{ reason error }

func (e *peerURLError) Error() string { return "<urlopen error " + e.reason.Error() + ">" }

// Python OSError or one of its subclasses with the str the exception prints
type peerOSError struct {
	class string
	text  string
}

func (e *peerOSError) Error() string { return e.text }

// Python exceptions the peer command raises itself, sorted by class for its except clauses
type (
	peerRuntimeError    struct{ msg string }
	peerLookupError     struct{ msg string }
	peerValueError      struct{ msg string }
	peerPermissionError struct{ msg string }
	peerAttributeError  struct{ msg string }
)

func (e *peerRuntimeError) Error() string    { return e.msg }
func (e *peerLookupError) Error() string     { return e.msg }
func (e *peerValueError) Error() string      { return e.msg }
func (e *peerPermissionError) Error() string { return e.msg }
func (e *peerAttributeError) Error() string  { return e.msg }

// True for URLError and its HTTPError subclass
func peerIsURLError(err error) bool {
	var ue *peerURLError
	var he *peerHTTPError
	return errors.As(err, &ue) || errors.As(err, &he)
}

// True for a bare TimeoutError, never one wrapped as a URLError reason
func peerIsTimeoutError(err error) bool {
	var oe *peerOSError
	return errors.As(err, &oe) && oe.class == "TimeoutError"
}

// True for OSError and every subclass the request stack raises
func peerIsOSError(err error) bool {
	var oe *peerOSError
	var pe *peerPermissionError
	return peerIsURLError(err) || errors.As(err, &oe) || errors.As(err, &pe)
}

// True for an error the transport except clauses catch, OSError family or RuntimeError
func peerTransportFailure(err error) bool {
	var re *peerRuntimeError
	return peerIsOSError(err) || errors.As(err, &re)
}

// Environment variable holding a peer's API key
func peerKeyEnv(name string) string {
	return "ICEBATH_PEER_" + strings.ReplaceAll(pystr.Upper(name), "-", "_") + "_KEY"
}

// Registered peers from config bot_peers, an empty mapping when the key is missing or malformed
var loadPeers = func(ctx context.Context, h string) (map[string]any, error) {
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, err
	}
	peers, ok := cfg["bot_peers"].(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return peers, nil
}

// Writes the peer registry back as config bot_peers
var savePeers = func(ctx context.Context, h string, peers map[string]any) error {
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	cfg["bot_peers"] = peers
	return config.SaveConfig(ctx, h, map[string]any(cfg), config.SaveOptions{})
}

// API key of a peer from the profile secret scope, the raw environment when the scope refuses
var peerSecret = func(ctx context.Context, name string) string {
	envName := peerKeyEnv(name)
	value, _, err := provider.GetSecret(ctx, envName)
	if err != nil {
		return pystr.Strip(os.Getenv(envName))
	}
	return pystr.Strip(value)
}

// Method, JSON body, timeout and extra headers of one peer API request
type peerRequestOptions struct {
	method  string
	body    *pyjson.Object
	timeout time.Duration
	headers [][2]string
}

// JSON object a peer answers, refusing cross origin credential forwarding and non object replies
var peerRequest = func(ctx context.Context, rawURL, key string, o peerRequestOptions) (*pyjson.Object, error) {
	method := o.method
	if method == "" {
		method = http.MethodGet
	}
	timeout := o.timeout
	if timeout == 0 {
		timeout = peerListTimeout
	}
	var data io.Reader
	if o.body != nil {
		text, err := pyjson.Dumps(o.body)
		if err != nil {
			return nil, err
		}
		data = strings.NewReader(text)
	}
	target, err := peerRequestURL(rawURL)
	if err != nil {
		return nil, err
	}
	headers := [][2]string{{"Authorization", "Bearer " + key}, {"Content-Type", "application/json"}, {"User-Agent", "icebath-peer-dm"}}
	headers = append(headers, o.headers...)
	for _, h := range headers {
		if peerHeaderValueRe.MatchString(h[1]) {
			return nil, fmt.Errorf("ValueError: Invalid header value %s", pyrepr.BytesRepr([]byte(h[1])))
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, data)
	if err != nil {
		return nil, err
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	raw, err := peerOpen(req, timeout)
	if err != nil {
		return nil, err
	}
	payload := pystr.DecodeUTF8Replace(raw)
	parsed, err := pyjson.Loads(payload)
	if err != nil {
		var de *pyjson.DecodeError
		if errors.As(err, &de) {
			return nil, &peerRuntimeError{"Peer returned non-JSON response: " + pystr.Slice(payload, 0, 200)}
		}
		return nil, err
	}
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil, &peerRuntimeError{"Peer returned a non-object JSON response"}
	}
	return obj, nil
}

// URL urllib would open, unknown schemes and missing hosts refused the way Request and do_open refuse them
func peerRequestURL(raw string) (string, error) {
	full := pystr.Strip(raw)
	if strings.HasPrefix(full, "<") && strings.HasSuffix(full, ">") {
		full = pystr.Strip(full[1 : len(full)-1])
	}
	if strings.HasPrefix(full, "URL:") {
		full = pystr.Strip(full[4:])
	}
	if i := strings.IndexByte(full, '#'); i >= 0 {
		full = full[:i]
	}
	m := peerURLTypeRe.FindStringSubmatch(full)
	if m == nil {
		return "", fmt.Errorf("ValueError: unknown url type: %s", pyrepr.Repr(full))
	}
	scheme := pystr.Lower(m[1])
	if scheme != "http" && scheme != "https" {
		return "", &peerURLError{errors.New("unknown url type: " + scheme)}
	}
	host := peerURLHostRe.FindStringSubmatch(m[2])
	if host == nil || host[1] == "" {
		return "", &peerURLError{errors.New("no host given")}
	}
	return full, nil
}

// Body of a successful response, urllib HTTPError for any other status and its exception classes for failures
func peerOpen(req *http.Request, timeout time.Duration) ([]byte, error) {
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{
		GetConn:      func(string) { wrote.Store(false) },
		WroteRequest: func(info httptrace.WroteRequestInfo) { wrote.Store(info.Err == nil) },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := provider.OpenCredentialedURL(req, timeout, nil, nil)
	if err != nil {
		return nil, peerTransportError(err, wrote.Load())
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &peerHTTPError{code: resp.StatusCode, msg: peerReason(resp), body: raw,
			bodyErr: peerReadError(readErr, len(raw), resp.ContentLength)}
	}
	if readErr != nil {
		return nil, peerReadError(readErr, len(raw), resp.ContentLength)
	}
	return raw, nil
}

// Reason phrase urllib puts in the HTTPError, the infinite loop text for a redirect the handler refused as a loop
func peerReason(resp *http.Response) string {
	reason := http.StatusText(resp.StatusCode)
	if _, text, ok := strings.Cut(resp.Status, " "); ok {
		reason = text
	}
	method := resp.Request.Method
	code := resp.StatusCode
	readable := method == http.MethodGet || method == http.MethodHead
	followed := (readable && (code == 301 || code == 302 || code == 303 || code == 307 || code == 308)) ||
		(method == http.MethodPost && (code == 301 || code == 302 || code == 303))
	if followed && resp.Header.Get("Location") != "" {
		return peerRedirectLoopPrefix + reason
	}
	return reason
}

// Exception urllib raises for a failed exchange, URLError before the request is sent and the bare error after
func peerTransportError(err error, wrote bool) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	inner := ue.Err
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		timedOut := &peerOSError{class: "TimeoutError", text: "timed out"}
		if wrote {
			return timedOut
		}
		return &peerURLError{timedOut}
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if !wrote {
		return &peerURLError{&peerOSError{class: pypath.OSErrorClass(inner), text: pyhttpx.OSErrorText(inner)}}
	}
	if errors.Is(inner, io.EOF) {
		return &peerOSError{class: "RemoteDisconnected", text: peerRemoteDisconnectedMsg}
	}
	var errno syscall.Errno
	if errors.As(inner, &errno) {
		return &peerOSError{class: pypath.OSErrorClass(inner), text: pyhttpx.OSErrorText(inner)}
	}
	return fmt.Errorf("http.client.HTTPException: %w", inner)
}

// Exception a response body read raises, IncompleteRead for a short body and the socket error otherwise
func peerReadError(err error, got int, want int64) error {
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return &peerOSError{class: "TimeoutError", text: "timed out"}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		if want > int64(got) {
			return fmt.Errorf("http.client.IncompleteRead: IncompleteRead(%d bytes read, %d more expected)", got, want-int64(got))
		}
		return fmt.Errorf("http.client.IncompleteRead: IncompleteRead(%d bytes read)", got)
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return &peerOSError{class: pypath.OSErrorClass(err), text: pyhttpx.OSErrorText(err)}
	}
	return err
}

// Base URL of a peer, the multiplex mirror of a named profile when one is given
func peerBaseURL(peer map[string]any, profile string) string {
	base := ""
	if v := peer["url"]; pyrepr.Truthy(v) {
		base = pyrepr.Str(v)
	}
	base = pystr.RstripChars(base, "/")
	if profile != "" {
		return base + "/p/" + pyurl.Quote(profile, "")
	}
	return base
}

// True only for the profile prefix 404 a peer answers before any handler, never a handler 404 or transport failure
func peerLacksProfile(err error) (bool, error) {
	var he *peerHTTPError
	if !errors.As(err, &he) || he.code != 404 {
		return false, nil
	}
	raw, readErr := he.read()
	if readErr != nil {
		if peerIsOSError(readErr) {
			return false, nil
		}
		return false, readErr
	}
	parsed, loadErr := pyjson.Loads(pystr.DecodeUTF8Replace(raw))
	if loadErr != nil {
		var de *pyjson.DecodeError
		if errors.As(loadErr, &de) {
			return false, nil
		}
		return false, loadErr
	}
	body, ok := parsed.(*pyjson.Object)
	if !ok {
		return false, nil
	}
	refusal, _ := body.Get("error")
	text, isText := refusal.(string)
	return isText && text == peerUnknownProfileError, nil
}

// Mirror a target addresses, an ambiguous refusal of the default handle reported naming the explicit default address
func peerResolveBase(ctx context.Context, peer map[string]any, profile, key, peerName string) (string, error) {
	base := peerBaseURL(peer, profile)
	if profile != peerDefaultProfileHandle {
		return base, nil
	}
	if _, err := peerRequest(ctx, base+"/health", key, peerRequestOptions{}); err != nil {
		if !peerTransportFailure(err) {
			return "", err
		}
		lacks, lerr := peerLacksProfile(err)
		if lerr != nil {
			return "", lerr
		}
		if lacks {
			name := peerName
			if name == "" {
				name = "<peer>"
			}
			return "", &peerLookupError{fmt.Sprintf("Peer '%s' answered that it serves no '%s' profile. That answer "+
				"is ambiguous: an up-to-date gateway gives it when it does not serve its default profile, "+
				"and an older one also when it has a '%s' profile it does not serve. "+
				"If the peer serves its default profile, address that bot as %s/default.",
				name, peerDefaultProfileHandle, peerDefaultProfileHandle, name)}
		}
	}
	return base, nil
}

// Session id of the remote canonical Bot Chat through the exact title hidden aware lookup, empty when none exists
func peerFindBotChat(ctx context.Context, base, key string) (string, error) {
	query := pyurl.URLEncode([][2]string{{"limit", "200"}, {"title", peerBotChatTitle}, {"include_hidden", "1"}})
	listing, err := peerRequest(ctx, base+"/api/sessions?"+query, key, peerRequestOptions{})
	if err != nil {
		return "", err
	}
	data, _ := listing.Get("data")
	if !pyrepr.Truthy(data) {
		return "", nil
	}
	rows, isList := data.([]any)
	if !isList {
		switch data.(type) {
		case string, *pyjson.Object:
			return "", nil
		}
		return "", fmt.Errorf("TypeError: '%s' object is not iterable", pyrepr.TypeName(data))
	}
	for _, row := range rows {
		session, ok := row.(*pyjson.Object)
		if !ok {
			continue
		}
		title, _ := session.Get("title")
		text := ""
		if pyrepr.Truthy(title) {
			s, isText := title.(string)
			if !isText {
				return "", &peerAttributeError{fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(title))}
			}
			text = s
		}
		if pystr.Strip(text) == peerBotChatTitle {
			id, _ := session.Get("id")
			if !pyrepr.Truthy(id) {
				return "", nil
			}
			return pyrepr.Str(id), nil
		}
	}
	return "", nil
}

// Canonical Bot Chat session id, created when missing, an old peer's hidden duplicate refusal explained
var peerEnsureBotChat = func(ctx context.Context, base, key string) (string, error) {
	existing, err := peerFindBotChat(ctx, base, key)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}
	created, err := peerRequest(ctx, base+"/api/sessions", key, peerRequestOptions{method: http.MethodPost,
		body: pyjson.ObjectOf("title", peerBotChatTitle, "source", "bot_peer_dm")})
	if err != nil {
		var he *peerHTTPError
		if errors.As(err, &he) {
			detail := peerHTTPErrorDetail(he)
			if he.code == 400 {
				text, isText := detail.(string)
				if !isText {
					return "", &peerAttributeError{fmt.Sprintf("'%s' object has no attribute 'lower'", pyrepr.TypeName(detail))}
				}
				if strings.Contains(pystr.Lower(text), "title") {
					return "", &peerRuntimeError{fmt.Sprintf("Peer already has a '%s' session but it is hidden and the "+
						"peer's gateway is too old to expose hidden sessions to this lookup "+
						"(HTTP 400: %s). Update the peer's icebath-agent, or unhide the "+
						"session there: PATCH /api/sessions/<id> {\"hidden\": false}.", peerBotChatTitle, text)}
				}
			}
		}
		return "", err
	}
	session := created
	if wrapped, _ := created.Get("session"); wrapped != nil {
		if row, ok := wrapped.(*pyjson.Object); ok {
			session = row
		}
	}
	id, _ := session.Get("id")
	if !pyrepr.Truthy(id) {
		id, _ = session.Get("session_id")
	}
	sessionID := ""
	if pyrepr.Truthy(id) {
		sessionID = pyrepr.Str(id)
	}
	if sessionID == "" {
		return "", &peerRuntimeError{"Peer did not return a session id for the new Bot Chat"}
	}
	return sessionID, nil
}

// Peer name and optional profile of a target, the profile validated and lowercased
func peerParseTarget(target any) (string, string, error) {
	raw, err := peerText(target)
	if err != nil {
		return "", "", err
	}
	peer, profile, _ := strings.Cut(pystr.Strip(raw), "/")
	peer = pystr.Strip(peer)
	profile = pystr.Strip(profile)
	if peer == "" {
		return "", "", &peerValueError{"Peer name required (icebath peer dm <peer>[/<agent>] ...)"}
	}
	if profile != "" && !peerProfileRe.MatchString(profile) {
		return "", "", &peerValueError{"Invalid agent/profile name: " + pyrepr.Repr(profile)}
	}
	if profile != "" {
		if profile, err = profiles.NormalizeProfileName(profile); err != nil {
			return "", "", &peerValueError{err.Error()}
		}
	}
	return peer, profile, nil
}

// Message the peer put in its JSON error body, else the first 200 body characters or the exception text
func peerHTTPErrorDetail(e *peerHTTPError) any {
	raw, err := e.read()
	if err != nil {
		return e.Error()
	}
	body := pystr.DecodeUTF8Replace(raw)
	parsed, err := pyjson.Loads(body)
	if err != nil {
		return e.Error()
	}
	var message any
	if obj, ok := parsed.(*pyjson.Object); ok {
		if refusal, has := obj.Get("error"); has {
			inner, isObj := refusal.(*pyjson.Object)
			if !isObj {
				return e.Error()
			}
			message, _ = inner.Get("message")
		}
	}
	if pyrepr.Truthy(message) {
		return message
	}
	return pystr.Slice(body, 0, 200)
}

// Name, profile, registry entry and key of a registered target
func resolvePeerTarget(c *peerCLI, target any) (string, string, map[string]any, string, error) {
	peerName, profile, err := peerParseTarget(target)
	if err != nil {
		return "", "", nil, "", err
	}
	peers, err := loadPeers(c.ctx, c.home)
	if err != nil {
		return "", "", nil, "", err
	}
	peer, ok := peers[peerName].(map[string]any)
	if !ok || !pyrepr.Truthy(peer["url"]) {
		return "", "", nil, "", &peerLookupError{fmt.Sprintf("No peer named '%s'. Run: icebath peer list", peerName)}
	}
	key := peerSecret(c.ctx, peerName)
	if key == "" {
		return "", "", nil, "", &peerPermissionError{fmt.Sprintf("No API key for peer '%s'. Set it: icebath peer add %s "+
			"--url <url> --key <key> (or add %s=<key> to ~/.icebath/.env)", peerName, peerName, peerKeyEnv(peerName))}
	}
	return peerName, profile, peer, key, nil
}

// Text of an argparse value after the or empty idiom, stripped, a non string value refusing strip
func peerText(v any) (string, error) {
	if !pyrepr.Truthy(v) {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", &peerAttributeError{fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(v))}
	}
	return pystr.Strip(s), nil
}

// Message argument, stdin when the argument is empty and stdin is not a terminal
func peerMessageFromArgs(c *peerCLI, ns map[string]any) (string, error) {
	message, err := peerText(ns["message"])
	if err != nil || message != "" || c.stdinIsatty() {
		return message, err
	}
	raw, err := io.ReadAll(c.stdin)
	if err != nil {
		return "", err
	}
	text, err := pystr.DecodeUTF8(raw)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		text = pystr.TranslateNewlines(text)
	}
	return pystr.Strip(text), nil
}

// Restart durable replay support a peer advertises, nil when an older peer cannot advertise it
func peerRunDurability(ctx context.Context, base, key string) (*bool, error) {
	capabilities, err := peerRequest(ctx, base+"/v1/capabilities", key, peerRequestOptions{})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, nil
	}
	features, ok := capabilities.GetOr("features", nil).(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	contract, ok := features.GetOr("runs_idempotency", nil).(*pyjson.Object)
	if !ok || !pyrepr.Truthy(contract.GetOr("supported", nil)) {
		return nil, nil
	}
	durable := pyrepr.Truthy(contract.GetOr("durable", nil))
	return &durable, nil
}

// Prints a peer HTTP rejection or transport failure to stderr and always exits 1
func peerFailure(c *peerCLI, peerName string, err error) int {
	var he *peerHTTPError
	if errors.As(err, &he) {
		c.eprintln(fmt.Sprintf("Peer '%s' rejected the request (HTTP %d): %s", peerName, he.code, pyrepr.Str(peerHTTPErrorDetail(he))))
		return 1
	}
	c.eprintln(fmt.Sprintf("Could not reach peer '%s': %s", peerName, err.Error()))
	return 1
}

// Prints the JSON payload under --json, the text lines otherwise
func peerEmit(c *peerCLI, ns map[string]any, payload *pyjson.Object, lines []string) (int, error) {
	if pyrepr.Truthy(ns["json"]) {
		text, err := pyjson.Dumps(payload)
		if err != nil {
			return 1, err
		}
		c.println(text)
		return 0, nil
	}
	for _, line := range lines {
		c.println(line)
	}
	return 0, nil
}

// None for an absent profile, the profile id otherwise
func peerProfileValue(profile string) any {
	if profile == "" {
		return nil
	}
	return profile
}

// First truthy value of the pair like Python or
func peerOr(a, b any) any {
	if pyrepr.Truthy(a) {
		return a
	}
	return b
}

// Registers or updates a peer gateway and stores its key in the home env file
func peerAdd(c *peerCLI, ns map[string]any) (int, error) {
	rawName, err := peerText(ns["name"])
	if err != nil {
		return 1, err
	}
	name := pystr.Lower(rawName)
	if !peerNameRe.MatchString(name) {
		c.eprintln(fmt.Sprintf("Invalid peer name: %s (lowercase, digits, -, _; max 64)", pyrepr.Repr(name)))
		return 2, nil
	}
	peerURL, err := peerText(ns["url"])
	if err != nil {
		return 1, err
	}
	lowered := pystr.Lower(peerURL)
	if !strings.HasPrefix(lowered, "http://") && !strings.HasPrefix(lowered, "https://") {
		c.eprintln("Peer --url must be an http(s) gateway base URL, e.g. http://spark.lan:8377")
		return 2, nil
	}
	peers, err := loadPeers(c.ctx, c.home)
	if err != nil {
		return 1, err
	}
	entry := map[string]any{"url": pystr.RstripChars(peerURL, "/")}
	if note := ns["note"]; pyrepr.Truthy(note) {
		text, err := peerText(note)
		if err != nil {
			return 1, err
		}
		entry["note"] = text
	}
	peers[name] = entry
	if err := savePeers(c.ctx, c.home, peers); err != nil {
		return 1, err
	}
	key, err := peerText(ns["key"])
	if err != nil {
		return 1, err
	}
	envName := peerKeyEnv(name)
	if key != "" {
		if err := config.SaveEnvValueContext(c.ctx, c.home, envName, key); err != nil {
			return 1, err
		}
		c.println(fmt.Sprintf("Peer '%s' saved (%s) — key stored as %s in ~/.icebath/.env", name, peerURL, envName))
		return 0, nil
	}
	c.println(fmt.Sprintf("Peer '%s' saved (%s). No key given — set the peer's API_SERVER_KEY with:\n"+
		"  icebath peer add %s --url %s --key <key>\n"+
		"  (or add %s=<key> to ~/.icebath/.env)", name, peerURL, name, peerURL, envName))
	return 0, nil
}

// Removes a registered peer and keeps its env file key
func peerRemove(c *peerCLI, ns map[string]any) (int, error) {
	rawName, err := peerText(ns["name"])
	if err != nil {
		return 1, err
	}
	name := pystr.Lower(rawName)
	peers, err := loadPeers(c.ctx, c.home)
	if err != nil {
		return 1, err
	}
	if _, ok := peers[name]; !ok {
		c.eprintln(fmt.Sprintf("No peer named '%s'.", name))
		return 1, nil
	}
	delete(peers, name)
	if err := savePeers(c.ctx, c.home, peers); err != nil {
		return 1, err
	}
	c.println(fmt.Sprintf("Peer '%s' removed (its %s entry in .env is kept; delete it manually if unused).", name, peerKeyEnv(name)))
	return 0, nil
}

// Prints every registered peer with its URL, key presence and note
func peerList(c *peerCLI, _ map[string]any) (int, error) {
	peers, err := loadPeers(c.ctx, c.home)
	if err != nil {
		return 1, err
	}
	if len(peers) == 0 {
		c.println("No peers registered. Add one: icebath peer add <name> --url http://host:port --key <API_SERVER_KEY>")
		return 0, nil
	}
	names := make([]string, 0, len(peers))
	for name := range peers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, _ := peers[name].(map[string]any)
		hasKey := fmt.Sprintf("NO KEY (%s unset)", peerKeyEnv(name))
		if peerSecret(c.ctx, name) != "" {
			hasKey = "key set"
		}
		note := ""
		if v := entry["note"]; pyrepr.Truthy(v) {
			note = " — " + pyrepr.Str(v)
		}
		peerURL, ok := entry["url"]
		if !ok {
			peerURL = "?"
		}
		c.println(fmt.Sprintf("%s\t%s\t[%s]%s", name, pyrepr.Str(peerURL), hasKey, note))
	}
	return 0, nil
}

// Reads or stops one asynchronous run on the peer
func peerRunCtl(c *peerCLI, ns map[string]any, action, peerName, profile, base, key string) (int, error) {
	runID, err := peerText(ns["run_id"])
	if err != nil {
		return 1, err
	}
	if runID == "" {
		c.eprintln("Run ID required.")
		return 2, nil
	}
	stop := action == "stop"
	target := base + "/v1/runs/" + pyurl.Quote(runID, "")
	opts := peerRequestOptions{}
	if stop {
		target += "/stop"
		opts = peerRequestOptions{method: http.MethodPost, body: pyjson.NewObject()}
	}
	result, err := peerRequest(c.ctx, target, key, opts)
	if err != nil {
		if peerTransportFailure(err) {
			return peerFailure(c, peerName, err), nil
		}
		return 1, err
	}
	if pyrepr.Truthy(ns["json"]) {
		merged := pyjson.ObjectOf("peer", peerName, "profile", peerProfileValue(profile))
		merged.Update(result)
		text, err := pyjson.Dumps(merged)
		if err != nil {
			return 1, err
		}
		c.println(text)
		return 0, nil
	}
	c.println(fmt.Sprintf("%s: %s", runID, pyrepr.Str(result.GetOr("status", "unknown"))))
	if output := result.GetOr("output", nil); !stop && pyrepr.Truthy(output) {
		c.println(pyrepr.Str(output))
	} else if failure := result.GetOr("error", nil); !stop && pyrepr.Truthy(failure) {
		c.eprintln(pyrepr.Str(failure))
	}
	return 0, nil
}

// Request body of one turn, the author added only when a dispatcher set the turn author variable
func peerTurnBody(message, messageKey string, extra ...any) *pyjson.Object {
	body := pyjson.ObjectOf(append([]any{messageKey, message}, extra...)...)
	if author := provider.TurnAuthorFromEnv(nil); author != nil {
		body.Set("author", author)
	}
	return body
}

// Starts one asynchronous peer turn on the canonical Bot Chat under an idempotency key
func peerRun(c *peerCLI, ns map[string]any, message, peerName, profile, base, key string) (int, error) {
	rawKey := ns["idempotency_key"]
	if !pyrepr.Truthy(rawKey) {
		rawKey = "peer-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	idempotencyKey, err := peerText(rawKey)
	if err != nil {
		return 1, err
	}
	if idempotencyKey == "" || pystr.Len(idempotencyKey) > 255 || strings.ContainsAny(idempotencyKey, "\r\n\x00") {
		c.eprintln("Idempotency key must be 1-255 characters without control newlines.")
		return 2, nil
	}
	var sessionID string
	var result *pyjson.Object
	err = func() error {
		durable, err := peerRunDurability(c.ctx, base, key)
		if err != nil {
			return err
		}
		if durable == nil || !*durable {
			c.eprintln("Warning: this peer does not advertise restart-durable " +
				"run replay; keep the run ID and avoid blind retries " +
				"after a gateway restart.")
		}
		if sessionID, err = peerEnsureBotChat(c.ctx, base, key); err != nil {
			return err
		}
		result, err = peerRequest(c.ctx, base+"/v1/runs", key, peerRequestOptions{method: http.MethodPost,
			body:    peerTurnBody(message, "input", "session_id", sessionID),
			headers: [][2]string{{"Idempotency-Key", idempotencyKey}}})
		return err
	}()
	if err != nil {
		if peerTransportFailure(err) {
			return peerFailure(c, peerName, err), nil
		}
		return 1, err
	}
	runID := ""
	if v := result.GetOr("run_id", nil); pyrepr.Truthy(v) {
		runID = pyrepr.Str(v)
	}
	if runID == "" {
		c.eprintln(fmt.Sprintf("Peer '%s' did not return a run ID.", peerName))
		return 1, nil
	}
	status := peerOr(result.GetOr("status", nil), "started")
	replayed := pyrepr.Truthy(result.GetOr("replayed", false))
	payload := pyjson.ObjectOf("peer", peerName, "profile", peerProfileValue(profile), "session_id", sessionID, "run_id", runID,
		"status", status, "idempotency_key", idempotencyKey, "replayed", replayed)
	replay := ""
	if replayed {
		replay = " (replayed)"
	}
	return peerEmit(c, ns, payload, []string{fmt.Sprintf("%s: %s%s", runID, pyrepr.Str(status), replay),
		"session_id: " + sessionID, "idempotency_key: " + idempotencyKey})
}

// Seconds of a duration the way Python prints an int or float timeout
func peerSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

// Runs one synchronous turn in the peer's canonical Bot Chat and prints the reply
func peerDM(c *peerCLI, ns map[string]any, message, peerName, profile, base, key string) (int, error) {
	sessionID := ""
	var result *pyjson.Object
	err := func() error {
		resolved, err := peerEnsureBotChat(c.ctx, base, key)
		if err != nil {
			return err
		}
		sessionID = resolved
		result, err = peerRequest(c.ctx, base+"/api/sessions/"+pyurl.Quote(sessionID, "")+"/chat", key,
			peerRequestOptions{method: http.MethodPost, body: peerTurnBody(message, "message"), timeout: peerDMTimeout})
		return err
	}()
	if err != nil {
		var re *peerRuntimeError
		if errors.As(err, &re) {
			c.eprintln(fmt.Sprintf("Peer '%s': %s", peerName, re.Error()))
			return 1, nil
		}
		if !peerIsOSError(err) {
			return 1, err
		}
		if sessionID != "" && peerIsTimeoutError(err) {
			c.eprintln(fmt.Sprintf("Peer '%s' accepted the message but its turn is still running after "+
				"%ss: the message is already in its Bot Chat (session %s) "+
				"and will be answered there. The reply cannot come back on this call. Do NOT resend.",
				peerName, peerSeconds(peerDMTimeout), sessionID))
			return 1, nil
		}
		return peerFailure(c, peerName, err), nil
	}
	if object, ok := result.GetOr("object", nil).(string); ok && object == peerChatQueuedObject {
		queuedIn := peerOr(result.GetOr("session_id", nil), sessionID)
		payload := pyjson.ObjectOf("peer", peerName, "profile", peerProfileValue(profile), "session_id", queuedIn,
			"status", peerOr(result.GetOr("status", nil), "queued"), "delivery_id", result.GetOr("delivery_id", nil))
		return peerEmit(c, ns, payload, []string{fmt.Sprintf("Peer '%s' has its Bot Chat open, so the message went into that chat (session "+
			"%s) and is answered there. The reply cannot come back on this call. Do NOT resend.", peerName, pyrepr.Str(queuedIn))})
	}
	reply := ""
	if msg, ok := result.GetOr("message", nil).(*pyjson.Object); ok {
		if content := msg.GetOr("content", nil); pyrepr.Truthy(content) {
			reply = pyrepr.Str(content)
		}
	}
	if IsIntentionalSilenceResponse(reply) {
		reply = ""
	} else {
		reply = StripTrailingSilenceMarker(reply).(string)
	}
	payload := pyjson.ObjectOf("peer", peerName, "profile", peerProfileValue(profile),
		"session_id", peerOr(result.GetOr("session_id", nil), sessionID), "reply", reply)
	line := reply
	if line == "" {
		line = "(no reply)"
	}
	return peerEmit(c, ns, payload, []string{line})
}

// Registry subcommands by the name or alias argparse stores in peer_action
var peerRegistryActions = map[string]func(*peerCLI, map[string]any) (int, error){
	"add": peerAdd, "set": peerAdd, "remove": peerRemove, "rm": peerRemove, "list": peerList, "ls": peerList,
}

// Dispatches the peer subcommand, registry actions locally and remote actions to the resolved peer mirror
func cmdPeer(c *peerCLI, ns map[string]any) (int, error) {
	action := ns["peer_action"]
	if action == nil {
		return peerList(c, ns)
	}
	name, _ := action.(string)
	if handler, ok := peerRegistryActions[name]; ok {
		return handler(c, ns)
	}
	if name != "dm" && name != "run" && name != "status" && name != "stop" {
		c.eprintln("Unknown peer action. See: icebath peer --help")
		return 2, nil
	}
	peerName, profile, peer, key, err := resolvePeerTarget(c, ns["target"])
	if err != nil {
		var ve *peerValueError
		var le *peerLookupError
		var pe *peerPermissionError
		switch {
		case errors.As(err, &ve):
			c.eprintln(err.Error())
			return 2, nil
		case errors.As(err, &le), errors.As(err, &pe):
			c.eprintln(err.Error())
			return 1, nil
		case errors.Is(err, fs.ErrPermission):
			c.eprintln(pypath.OSErrorText(err))
			return 1, nil
		}
		return 1, err
	}
	base, err := peerResolveBase(c.ctx, peer, profile, key, peerName)
	if err != nil {
		var le *peerLookupError
		if errors.As(err, &le) {
			c.eprintln(err.Error())
			return 1, nil
		}
		return 1, err
	}
	if name == "status" || name == "stop" {
		return peerRunCtl(c, ns, name, peerName, profile, base, key)
	}
	message, err := peerMessageFromArgs(c, ns)
	if err != nil {
		return 1, err
	}
	if message == "" {
		c.eprintln("Message required (argument or stdin).")
		return 2, nil
	}
	if name == "run" {
		return peerRun(c, ns, message, peerName, profile, base, key)
	}
	return peerDM(c, ns, message, peerName, profile, base, key)
}

// Runs icebath peer from the parsed namespace, stdout to w and diagnostics to stderr
func PeerCommand(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	return cmdPeer(&peerCLI{ctx: ctx, home: h, out: w, errOut: peerStderr, stdin: peerStdin, stdinIsatty: peerStdinIsatty}, ns)
}
