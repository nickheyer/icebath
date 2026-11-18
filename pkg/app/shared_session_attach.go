package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyipaddress"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Largest owner handshake reply read before the attachment is refused
const attachReplyLimit = 65536

// Host and port of an explicit loopback origin of the given scheme, an error for any other URL
func attachLocalOrigin(raw, scheme string) (string, int, error) {
	parts, err := pyurl.Split(raw)
	if err != nil {
		return "", 0, err
	}
	host := parts.Hostname()
	addr, ok := pyipaddress.IPAddress(host)
	local := ok && pyipaddress.IsLoopback(addr)
	refused := errors.New("Shared runtime endpoint must be an explicit loopback address and port.")
	if parts.Scheme != scheme || !local || strings.Contains(parts.Netloc, "@") || parts.Fragment != "" {
		return "", 0, refused
	}
	port, present, err := parts.Port()
	if err != nil {
		return "", 0, err
	}
	if !present || port == 0 {
		return "", 0, refused
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

// Transport and JSON decode failures of the handshake, the ones reported as a failed attachment
type attachTransportError struct{ err error }

// Message of the underlying failure
func (e *attachTransportError) Error() string { return e.err.Error() }

// Decoded owner handshake reply, an error past 64 KiB or on a non 2xx status
func attachHandshake(ctx context.Context, endpoint string) (any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &attachTransportError{err}
	}
	request.Header.Set("Accept", "*/*")
	resp, err := attachHandshakeClient().Do(request)
	if err != nil {
		return nil, &attachTransportError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &attachTransportError{fmt.Errorf("handshake status %d", resp.StatusCode)}
	}
	var body []byte
	chunk := make([]byte, attachReplyLimit)
	for {
		n, rerr := resp.Body.Read(chunk)
		body = append(body, chunk[:n]...)
		if len(body) > attachReplyLimit {
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

// Authenticated socket URL of the runtime owning a session in this profile, false when nothing owns it
func discoverAttachURL(ctx context.Context, sessionID, registryHome string) (string, bool, error) {
	if registryHome == "" {
		registryHome = home.Dir(ctx)
	}
	profileHome, err := pypath.Resolve(registryHome)
	if err != nil {
		return "", false, err
	}
	entries, err := activesessions.RegistrySnapshot(ctx, profileHome, true)
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
	parts, err := pyurl.Split(endpointText)
	if err != nil {
		return "", false, err
	}
	if (parts.Path != "" && parts.Path != "/") || parts.Query != "" {
		return "", false, errors.New("Shared runtime endpoint must be an origin without a path or query.")
	}
	leaseID := owner.GetOr("lease_id", nil)
	query := pyurl.URLEncode([][2]string{{"session_id", sessionID}, {"lease_id", pyrepr.Str(leaseID)}, {"profile_home", profileHome}})
	reply, err := attachHandshake(ctx, strings.TrimRight(endpointText, "/")+"/api/session-attach?"+query)
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
	differentEndpoint := errors.New("Shared runtime handshake returned a different endpoint.")
	websocketURL, ok := replyObj.GetOr("websocket_url", nil).(string)
	if !ok {
		return "", false, differentEndpoint
	}
	wsHost, wsPort, err := attachLocalOrigin(websocketURL, "ws")
	if err != nil {
		return "", false, err
	}
	if wsHost != originHost || wsPort != originPort {
		return "", false, differentEndpoint
	}
	wsParts, err := pyurl.Split(websocketURL)
	if err != nil {
		return "", false, err
	}
	if wsParts.Path != "/api/ws" {
		return "", false, differentEndpoint
	}
	return websocketURL, true, nil
}

// Points the TUI at a resumed session's live owner unless an explicit gateway URL is already set
func configureTUIAttachment(ctx context.Context, env map[string]string, sessionID, registryHome string) error {
	if sessionID == "" || strings.TrimSpace(env["ICEBATH_TUI_GATEWAY_URL"]) != "" {
		return nil
	}
	attachURL, found, err := discoverAttachURL(ctx, sessionID, registryHome)
	if err != nil {
		return err
	}
	if found {
		env["ICEBATH_TUI_GATEWAY_URL"] = attachURL
	}
	return nil
}
