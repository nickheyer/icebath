package mcp

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
)

// A remote server address that is not an http or https URL with a host
type InvalidURLError struct{ Msg string }

func (e *InvalidURLError) Error() string { return e.Msg }

// A remote server address answering with something other than MCP, such as a web page
type NonMCPError struct{ Msg string }

func (e *NonMCPError) Error() string { return e.Msg }

// Address of a remote server stripped, or why it is unusable
func ValidURL(server string, raw any) (string, error) {
	bad := func(detail string) error {
		return &InvalidURLError{Msg: fmt.Sprintf("The address of the MCP server %s cannot be used, because %s.", server, detail)}
	}
	text, ok := raw.(string)
	if !ok {
		return "", bad("it is not text")
	}
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return "", bad("it is empty")
	}
	u, err := url.Parse(stripped)
	switch {
	case err != nil:
		return "", bad(fmt.Sprintf("“%s” cannot be read as an address", stripped))
	case u.Scheme != "http" && u.Scheme != "https":
		return "", bad(fmt.Sprintf("“%s” does not start with http:// or https://", stripped))
	case u.Host == "":
		return "", bad(fmt.Sprintf("“%s” names no computer", stripped))
	case u.Hostname() == "":
		return "", bad(fmt.Sprintf("“%s” names no computer", stripped))
	}
	return stripped, nil
}

// Lower cased words meaning the server side session ended while the credentials still hold
var expiredMarkers = []string{"invalid or expired session", "expired session", "session expired", "session not found", "unknown session",
	"session terminated", "closedresourceerror", "closed resource", "transport is closed", "connection closed", "broken pipe", "end of file"}

// Whether the failure is the server's session or the transport under it ending, so a fresh session may succeed
func SessionExpired(err error) bool {
	if err == nil {
		return false
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		switch current {
		case io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, net.ErrClosed, os.ErrClosed, syscall.EPIPE, ErrSessionExpired:
			return true
		}
	}
	text := strings.ToLower(err.Error())
	for _, marker := range expiredMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return errors.Is(err, ErrSessionExpired)
}

// HTTP statuses a server rejecting the Streamable HTTP transport answers its first request with
var streamableRejections = map[int]bool{400: true, 405: true, 406: true, 411: true}

// Whether a failed first connect is a Streamable HTTP rejection, so the legacy SSE transport may work
func StreamableRejected(err error) bool {
	if status, ok := errors.AsType[*HTTPStatusError](err); ok && streamableRejections[status.Status] {
		return true
	}
	rpc, ok := errors.AsType[*RPCError](err)
	return ok && rpc.Code == CodeInternalError && strings.Contains(strings.ToLower(rpc.Message), "server returned an error response")
}
