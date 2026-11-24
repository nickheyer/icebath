package websocketutil

import (
	"errors"
	"strconv"
	"strings"
)

// Close codes of the RFC 6455 registry
const (
	// The purpose of the connection was fulfilled
	CloseNormal = 1000
	// The endpoint is going away, a server shutting down or a client leaving
	CloseGoingAway = 1001
	// The peer broke the protocol
	CloseProtocolError = 1002
	// The peer sent a kind of data the endpoint cannot accept
	CloseUnsupportedData = 1003
	// The close frame carried no code; never sent with a code on the wire
	CloseNoStatus = 1005
	// The connection ended without a close frame; never sent on the wire
	CloseAbnormal = 1006
	// A message carried data inconsistent with its type, such as text that is not UTF-8
	CloseInvalidPayload = 1007
	// A message broke the endpoint's policy
	ClosePolicyViolation = 1008
	// A message was too big to process
	CloseMessageTooBig = 1009
	// The server did not negotiate an extension the client requires
	CloseMandatoryExtension = 1010
	// The endpoint hit an unexpected condition
	CloseInternalError = 1011
	// The server is restarting
	CloseServiceRestart = 1012
	// The server is overloaded, the client should come back later
	CloseTryAgainLater = 1013
	// A gateway got an invalid response from the server behind it
	CloseBadGateway = 1014
	// The TLS handshake failed; never sent on the wire
	CloseTLSHandshake = 1015
)

// Short meaning of a close code, such as "normal closure" for 1000, "registered" for 3000 to 3999, "private use" for 4000 to 4999 and "unknown" otherwise
func CloseCodeText(code int) string {
	switch code {
	case CloseNormal:
		return "normal closure"
	case CloseGoingAway:
		return "going away"
	case CloseProtocolError:
		return "protocol error"
	case CloseUnsupportedData:
		return "unsupported data"
	case CloseNoStatus:
		return "no status"
	case CloseAbnormal:
		return "abnormal closure"
	case CloseInvalidPayload:
		return "invalid payload data"
	case ClosePolicyViolation:
		return "policy violation"
	case CloseMessageTooBig:
		return "message too big"
	case CloseMandatoryExtension:
		return "mandatory extension"
	case CloseInternalError:
		return "internal error"
	case CloseServiceRestart:
		return "service restart"
	case CloseTryAgainLater:
		return "try again later"
	case CloseBadGateway:
		return "bad gateway"
	case CloseTLSHandshake:
		return "TLS handshake failure"
	}
	switch {
	case code >= 3000 && code <= 3999:
		return "registered"
	case code >= 4000 && code <= 4999:
		return "private use"
	}
	return "unknown"
}

// Whether a code is one an endpoint may put in its close frame, CloseNoStatus standing for a frame without a code
func sendableCloseCode(code int) bool {
	switch {
	case code >= CloseNormal && code <= CloseUnsupportedData, code == CloseNoStatus:
		return true
	case code >= CloseInvalidPayload && code <= CloseBadGateway:
		return true
	}
	return code >= 3000 && code <= 4999
}

// Whether a close code ends a connection normally: CloseNormal, CloseGoingAway or CloseNoStatus
func normalCloseCode(code int) bool {
	return code == CloseNormal || code == CloseGoingAway || code == CloseNoStatus
}

// End of a connection: the close frames the two sides exchanged and, when the closing handshake did not complete, what ended it
type CloseError struct {
	// Code of the peer's close frame, CloseNoStatus when that frame carried none, CloseAbnormal when no close frame arrived
	Code int
	// Reason text of the peer's close frame
	Reason string
	// Code of the close frame this side sent, zero when it sent none
	SentCode int
	// Reason text of the close frame this side sent
	SentReason string
	// Whether this side sent its close frame before the peer's arrived
	SentFirst bool
	// What ended the connection when no close frame arrived: a network error, io.ErrUnexpectedEOF, ErrMessageTooBig, ErrInvalidUTF8, ErrKeepaliveTimeout, ErrCloseTimeout, the context error that cut a close short, a protocol violation of the peer or a compressed message that does not decompress (a flate.CorruptInputError)
	Err error
}

// Frames sent and received in the order they went, then the cause, such as "websocket closed: received 1001 (going away) bye; then sent 1001 (going away)"
func (e *CloseError) Error() string {
	var b strings.Builder
	b.WriteString("websocket closed: ")
	received := e.Received()
	switch {
	case received && e.SentCode != 0 && e.SentFirst:
		b.WriteString("sent ")
		writeCloseFrame(&b, e.SentCode, e.SentReason)
		b.WriteString("; then received ")
		writeCloseFrame(&b, e.Code, e.Reason)
	case received && e.SentCode != 0:
		b.WriteString("received ")
		writeCloseFrame(&b, e.Code, e.Reason)
		b.WriteString("; then sent ")
		writeCloseFrame(&b, e.SentCode, e.SentReason)
	case received:
		b.WriteString("received ")
		writeCloseFrame(&b, e.Code, e.Reason)
		b.WriteString("; no close frame sent")
	case e.SentCode != 0:
		b.WriteString("sent ")
		writeCloseFrame(&b, e.SentCode, e.SentReason)
		b.WriteString("; no close frame received")
	default:
		b.WriteString("no close frame received or sent")
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// The cause of an end without the peer's close frame
func (e *CloseError) Unwrap() error { return e.Err }

// Whether the peer sent a close frame
func (e *CloseError) Received() bool { return e.Code != 0 && e.Code != CloseAbnormal }

// Whether both sides exchanged close frames with CloseNormal, CloseGoingAway or CloseNoStatus
func (e *CloseError) Clean() bool {
	return e.Received() && e.SentCode != 0 && normalCloseCode(e.Code) && normalCloseCode(e.SentCode)
}

// Whether err holds a *CloseError of a clean closing handshake, the end a read loop treats as a normal finish
func IsCleanClose(err error) bool {
	var closed *CloseError
	return errors.As(err, &closed) && closed.Clean()
}

func writeCloseFrame(b *strings.Builder, code int, reason string) {
	b.WriteString(strconv.Itoa(code))
	b.WriteString(" (")
	b.WriteString(CloseCodeText(code))
	b.WriteByte(')')
	if reason != "" {
		b.WriteByte(' ')
		b.WriteString(reason)
	}
}
