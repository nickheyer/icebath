package websocketutil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// URL Connect refuses before dialing: one that does not parse, has a scheme other than ws, wss, http or https, or names no host
var ErrInvalidURL = errors.New("invalid websocket URL")

// Message over the size limit, after which the connection is closed with CloseMessageTooBig
var ErrMessageTooBig = errors.New("websocket message exceeds the size limit")

// Text message whose payload is not UTF-8, after which the connection is closed with CloseInvalidPayload
var ErrInvalidUTF8 = errors.New("websocket text message is not valid UTF-8")

// Keepalive ping after which nothing came from the peer within the ping timeout, or that could not be written within it, after which the connection is closed with CloseInternalError
var ErrKeepaliveTimeout error = &timeoutError{msg: "websocket peer did not answer a keepalive ping in time"}

// Close frame the peer did not answer within the close timeout
var ErrCloseTimeout error = &timeoutError{msg: "websocket peer did not answer the close frame in time"}

// Sentinel that satisfies net.Error as a timeout
type timeoutError struct{ msg string }

func (e *timeoutError) Error() string { return e.msg }

func (e *timeoutError) Timeout() bool { return true }

func (e *timeoutError) Temporary() bool { return true }

// Upgrade request the server refused, or answered with a handshake that does not check out
type HandshakeError struct {
	// HTTP status of the response, 101 when the server switched protocols but the handshake was invalid
	StatusCode int
	// Status line text such as "403 Forbidden", the standard text of the code when empty
	Status string
	// Response headers
	Header http.Header
	// Start of the response body, at most 1024 bytes
	Body []byte
	// URL the upgrade went to, without its user info, query and fragment
	URL string
	// What was wrong with the response: websocket.ErrBadHandshake, the compression the server negotiated or the subprotocol it chose; websocket.ErrBadHandshake when nil
	Err error
}

// The URL and the status the server refused with, such as "websocket upgrade to wss://example.com/ws refused: 403 Forbidden"
func (e *HandshakeError) Error() string {
	var b strings.Builder
	b.WriteString("websocket upgrade")
	if e.URL != "" {
		b.WriteString(" to ")
		b.WriteString(e.URL)
	}
	if e.StatusCode == http.StatusSwitchingProtocols {
		b.WriteString(" failed: ")
		b.WriteString(strings.TrimPrefix(e.Unwrap().Error(), "websocket: "))
		return b.String()
	}
	b.WriteString(" refused: ")
	switch {
	case e.Status != "":
		b.WriteString(e.Status)
	default:
		b.WriteString(strconv.Itoa(e.StatusCode))
		if text := http.StatusText(e.StatusCode); text != "" {
			b.WriteByte(' ')
			b.WriteString(text)
		}
	}
	return b.String()
}

// What was wrong with the response, websocket.ErrBadHandshake unless a more precise cause is known
func (e *HandshakeError) Unwrap() error {
	if e.Err != nil {
		return e.Err
	}
	return websocket.ErrBadHandshake
}

// Whether target is websocket.ErrBadHandshake, which every handshake error matches whatever its precise cause
func (e *HandshakeError) Is(target error) bool { return target == websocket.ErrBadHandshake }

// Error of a gorilla/websocket dial: a *HandshakeError carrying the response when the server refused the upgrade, any other error unchanged and nil for nil
func WrapDialError(resp *http.Response, err error) error {
	if err == nil || resp == nil || !errors.Is(err, websocket.ErrBadHandshake) {
		return err
	}
	shown := ""
	if resp.Request != nil && resp.Request.URL != nil {
		shown = displayURL(resp.Request.URL)
	}
	return newHandshakeError(resp, err, shown)
}

// Handshake error of a response, its body read up to the 1024 bytes the dialer keeps and put back in front of the rest so the response still reads whole
func newHandshakeError(resp *http.Response, err error, shown string) *HandshakeError {
	e := &HandshakeError{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header, URL: shown, Err: err}
	if resp.Body != nil {
		e.Body, _ = io.ReadAll(io.LimitReader(resp.Body, maxHandshakeBody))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(e.Body), resp.Body), resp.Body}
	}
	return e
}

// Most of a refused upgrade's body a HandshakeError keeps, what gorilla/websocket reads of it
const maxHandshakeBody = 1024

// Opening handshake that outlasted the open timeout, or whose dial or name lookup timed out first
type OpenTimeoutError struct {
	// URL the handshake went to, without its user info, query and fragment
	URL string
	// Open timeout that ran out, zero when the operating system or a name server timed out first
	Limit time.Duration
	// Timeout the operating system or a name server reported, nil when the open timeout ran out
	Err error
}

// The URL and the limit or the cause, such as "websocket opening handshake with wss://example.com/ws timed out after 30s"
func (e *OpenTimeoutError) Error() string {
	var b strings.Builder
	b.WriteString("websocket opening handshake")
	if e.URL != "" {
		b.WriteString(" with ")
		b.WriteString(e.URL)
	}
	b.WriteString(" timed out")
	if e.Limit > 0 {
		b.WriteString(" after ")
		b.WriteString(e.Limit.String())
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// The timeout the operating system or a name server reported, nil when the open timeout ran out
func (e *OpenTimeoutError) Unwrap() error { return e.Err }

// Always true: the error is a timeout
func (e *OpenTimeoutError) Timeout() bool { return true }

// Always true: the same handshake may succeed when tried again
func (e *OpenTimeoutError) Temporary() bool { return true }

// Whether target is context.DeadlineExceeded or os.ErrDeadlineExceeded, the deadline errors the open timeout stands for
func (e *OpenTimeoutError) Is(target error) bool {
	return target == context.DeadlineExceeded || target == os.ErrDeadlineExceeded
}

// Connection attempt that failed before the server answered the upgrade: the name lookup, the dial, the proxy, the TLS handshake or the exchange of the upgrade itself
type ConnectError struct {
	// URL being connected to, without its user info, query and fragment
	URL string
	Err error
}

// The URL and the cause, such as "websocket connect to wss://example.com/ws: dial tcp 192.0.2.1:443: connect: connection refused"
func (e *ConnectError) Error() string {
	text := "websocket connect to " + e.URL
	if e.Err != nil {
		text += ": " + e.Err.Error()
	}
	return text
}

// The cause
func (e *ConnectError) Unwrap() error { return e.Err }

// URL safe to show in an error: scheme, host and path, the user info, query and fragment left out since they can carry credentials or tickets
func displayURL(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http":
		scheme = "ws"
	case "https":
		scheme = "wss"
	}
	return (&url.URL{Scheme: scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}).String()
}
