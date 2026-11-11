package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Fields of a urllib urlsplit result the attachment checks read
type attachSplit struct {
	Scheme, Netloc, Path, RawQuery, Fragment string
}

// urllib.parse.urlsplit of a URL, an error for an unbalanced IPv6 bracket
func attachURLSplit(raw string) (attachSplit, error) {
	text := strings.TrimLeftFunc(raw, func(r rune) bool { return r <= ' ' })
	text = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(text)
	var parts attachSplit
	if i := strings.IndexByte(text, ':'); i > 0 && isASCIILetter(text[0]) {
		valid := true
		for j := 0; j < i; j++ {
			c := text[j]
			if !isASCIILetter(c) && !(c >= '0' && c <= '9') && c != '+' && c != '-' && c != '.' {
				valid = false
				break
			}
		}
		if valid {
			parts.Scheme, text = strings.ToLower(text[:i]), text[i+1:]
		}
	}
	if strings.HasPrefix(text, "//") {
		end := len(text)
		if j := strings.IndexAny(text[2:], "/?#"); j >= 0 {
			end = j + 2
		}
		parts.Netloc, text = text[2:end], text[end:]
		if strings.Contains(parts.Netloc, "[") != strings.Contains(parts.Netloc, "]") {
			return attachSplit{}, errors.New("Invalid IPv6 URL")
		}
	}
	if i := strings.IndexByte(text, '#'); i >= 0 {
		text, parts.Fragment = text[:i], text[i+1:]
	}
	if i := strings.IndexByte(text, '?'); i >= 0 {
		text, parts.RawQuery = text[:i], text[i+1:]
	}
	parts.Path = text
	return parts, nil
}

// True for an ASCII letter
func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// Host and port text after any userinfo, the brackets of an IPv6 literal removed
func (s attachSplit) hostinfo() (string, string) {
	info := s.Netloc
	if i := strings.LastIndexByte(info, '@'); i >= 0 {
		info = info[i+1:]
	}
	if i := strings.IndexByte(info, '['); i >= 0 {
		bracketed := info[i+1:]
		host, port, _ := strings.Cut(bracketed, "]")
		_, port, _ = strings.Cut(port, ":")
		return host, port
	}
	host, port, _ := strings.Cut(info, ":")
	return host, port
}

// Lower cased host name keeping an IPv6 zone as written
func (s attachSplit) Hostname() string {
	host, _ := s.hostinfo()
	addr, zone, found := strings.Cut(host, "%")
	if found {
		return strings.ToLower(addr) + "%" + zone
	}
	return strings.ToLower(host)
}

// Port as an int, zero when absent and an error for text that is not an in range decimal
func (s attachSplit) Port() (int, error) {
	_, text := s.hostinfo()
	if text == "" {
		return 0, nil
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("Port could not be cast to integer value as '%s'", text)
		}
	}
	port, err := strconv.Atoi(text)
	if err != nil || port > 65535 {
		return 0, errors.New("Port out of range 0-65535")
	}
	return port, nil
}

// True when the netloc carries userinfo
func (s attachSplit) HasUser() bool { return strings.Contains(s.Netloc, "@") }

// Host and port of an explicit loopback origin of the given scheme, an error for any other URL
func attachLocalOrigin(raw, scheme string) (string, int, error) {
	parts, err := attachURLSplit(raw)
	if err != nil {
		return "", 0, err
	}
	host := parts.Hostname()
	local := false
	if addr, perr := netip.ParseAddr(host); perr == nil {
		local = addr.IsLoopback()
	}
	port, err := parts.Port()
	if err != nil {
		return "", 0, err
	}
	if parts.Scheme != scheme || !local || parts.HasUser() || parts.Fragment != "" || port == 0 {
		return "", 0, errors.New("Shared runtime endpoint must be an explicit loopback address and port.")
	}
	return host, port, nil
}

// Connection whose every read and write gets its own deadline, the per operation timeouts of an httpx client
type attachDeadlineConn struct {
	net.Conn
	timeout time.Duration
}

// Reads with a fresh read deadline
func (c *attachDeadlineConn) Read(b []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

// Writes with a fresh write deadline
func (c *attachDeadlineConn) Write(b []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// HTTP client that ignores proxy environment and redirects with three second connect, read and write timeouts
func attachHandshakeClient() *http.Client {
	const timeout = 3 * time.Second
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &attachDeadlineConn{Conn: conn, timeout: timeout}, nil
		},
		DisableKeepAlives: true,
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Errors the handshake reports as a failed attachment, the httpx and JSON decode failures
type attachTransportError struct{ err error }

// Message of the underlying failure
func (e *attachTransportError) Error() string { return e.err.Error() }

// Body of the owner handshake reply, an error past 64 KiB or on a non 2xx status
func attachHandshake(endpoint string) (any, error) {
	resp, err := attachHandshakeClient().Get(endpoint)
	if err != nil {
		return nil, &attachTransportError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &attachTransportError{fmt.Errorf("handshake status %d", resp.StatusCode)}
	}
	var body []byte
	chunk := make([]byte, 65536)
	for {
		n, rerr := resp.Body.Read(chunk)
		body = append(body, chunk[:n]...)
		if len(body) > 65536 {
			return nil, errors.New("Shared runtime handshake response is too large.")
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, &attachTransportError{rerr}
		}
	}
	text, err := pyjson.DecodeBytes(body)
	if err != nil {
		return nil, err
	}
	reply, err := pyjson.Loads(text)
	if err != nil {
		return nil, &attachTransportError{err}
	}
	return reply, nil
}

// Authenticated socket URL of the runtime that owns a session in this profile, false when nothing owns it
func discoverAttachURL(sessionID, registryHome string) (string, bool, error) {
	if registryHome == "" {
		registryHome = home.Dir(context.Background())
	}
	profileHome, err := pypath.Resolve(registryHome)
	if err != nil {
		return "", false, err
	}
	entries, err := activesessions.RegistrySnapshot(profileHome, true)
	if err != nil {
		return "", false, err
	}
	var owners []*pyjson.Object
	for _, entry := range entries {
		if pyjson.Equal(entry.GetOr("session_id", nil), sessionID) {
			owners = append(owners, entry)
		}
	}
	if len(owners) == 0 {
		return "", false, nil
	}
	if len(owners) != 1 {
		return "", false, errors.New("Session owner identity is ambiguous; no attachment was attempted.")
	}
	owner := owners[0]
	var endpoint any
	if metadata, ok := owner.GetOr("metadata", nil).(*pyjson.Object); ok {
		endpoint = metadata.GetOr("shared_runtime_url", nil)
	}
	endpointText, _ := endpoint.(string)
	if endpointText == "" {
		return "", false, errors.New("This chat is open in another Icebath window/terminal, and attaching " +
			"this terminal to it is not available in this build. Close the chat " +
			"there and run icebath --resume " + sessionID + " here to take it over.\n" +
			activesessions.SessionOwnerDetails(sessionID, owner))
	}
	originHost, originPort, err := attachLocalOrigin(endpointText, "http")
	if err != nil {
		return "", false, err
	}
	parts, err := attachURLSplit(endpointText)
	if err != nil {
		return "", false, err
	}
	if (parts.Path != "" && parts.Path != "/") || parts.RawQuery != "" {
		return "", false, errors.New("Shared runtime endpoint must be an origin without a path or query.")
	}
	leaseID, _ := owner.GetOr("lease_id", nil).(string)
	query := "session_id=" + url.QueryEscape(sessionID) + "&lease_id=" + url.QueryEscape(leaseID) +
		"&profile_home=" + url.QueryEscape(profileHome)
	reply, err := attachHandshake(strings.TrimRight(endpointText, "/") + "/api/session-attach?" + query)
	var transport *attachTransportError
	if errors.As(err, &transport) {
		return "", false, errors.New("This chat is open in another Icebath window/terminal, and attaching " +
			"this terminal to it just failed. Use the chat where it is open, or " +
			"close it there and run icebath --resume " + sessionID + " here.\n" +
			activesessions.SessionOwnerDetails(sessionID, owner))
	}
	if err != nil {
		return "", false, err
	}
	replyObj, ok := reply.(*pyjson.Object)
	if !ok || !pyjson.Equal(replyObj.GetOr("session_id", nil), sessionID) ||
		!pyjson.Equal(replyObj.GetOr("lease_id", nil), leaseID) || !pyjson.Equal(replyObj.GetOr("profile_home", nil), profileHome) {
		return "", false, errors.New("Shared runtime handshake identity does not match the requested owner.")
	}
	websocketURL, ok := replyObj.GetOr("websocket_url", nil).(string)
	if !ok {
		return "", false, errors.New("Shared runtime handshake returned a different endpoint.")
	}
	wsHost, wsPort, err := attachLocalOrigin(websocketURL, "ws")
	if err != nil {
		return "", false, err
	}
	wsParts, err := attachURLSplit(websocketURL)
	if err != nil {
		return "", false, err
	}
	if wsHost != originHost || wsPort != originPort || wsParts.Path != "/api/ws" {
		return "", false, errors.New("Shared runtime handshake returned a different endpoint.")
	}
	return websocketURL, true, nil
}

// Points the TUI at a resumed session's live owner unless an explicit gateway URL is already set
func configureTUIAttachment(env map[string]string, sessionID, registryHome string) error {
	if sessionID == "" || strings.TrimSpace(env["ICEBATH_TUI_GATEWAY_URL"]) != "" {
		return nil
	}
	attachURL, found, err := discoverAttachURL(sessionID, registryHome)
	if err != nil {
		return err
	}
	if found {
		env["ICEBATH_TUI_GATEWAY_URL"] = attachURL
	}
	return nil
}
