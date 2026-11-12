package pywebsockets

import "fmt"

// Close codes whose exchange ends a connection cleanly
var okCloseCodes = map[int]bool{1000: true, 1001: true, 1005: true}

// Explanation of each close code websockets names
var closeCodeExplanations = map[int]string{
	1000: "OK", 1001: "going away", 1002: "protocol error", 1003: "unsupported data",
	1005: "no status received [internal]", 1006: "abnormal closure [internal]", 1007: "invalid frame payload data",
	1008: "policy violation", 1009: "message too big", 1010: "mandatory extension", 1011: "internal error",
	1012: "service restart", 1013: "try again later", 1014: "bad gateway", 1015: "TLS handshake failure [internal]",
}

// Close frame of one side of a closing handshake
type Close struct {
	Code   int
	Reason string
}

// Code with its explanation and the reason when present, as websockets prints a close frame
func (c Close) String() string {
	explanation := closeCodeExplanations[c.Code]
	switch {
	case c.Code >= 3000 && c.Code < 4000:
		explanation = "registered"
	case c.Code >= 4000 && c.Code < 5000:
		explanation = "private use"
	case explanation == "":
		explanation = "unknown"
	}
	result := fmt.Sprintf("%d (%s)", c.Code, explanation)
	if c.Reason != "" {
		result += " " + c.Reason
	}
	return result
}

// Interaction with a closed connection, carrying the close frames received and sent and their order
type ConnectionClosed struct {
	Rcvd, Sent   *Close
	RcvdThenSent bool
}

func (e *ConnectionClosed) Error() string {
	switch {
	case e.Rcvd == nil && e.Sent == nil:
		return "no close frame received or sent"
	case e.Rcvd == nil:
		return fmt.Sprintf("sent %s; no close frame received", e.Sent)
	case e.Sent == nil:
		return fmt.Sprintf("received %s; no close frame sent", e.Rcvd)
	case e.RcvdThenSent:
		return fmt.Sprintf("received %s; then sent %s", e.Rcvd, e.Sent)
	}
	return fmt.Sprintf("sent %s; then received %s", e.Sent, e.Rcvd)
}

// ConnectionClosedOK when both frames carry a clean code, ConnectionClosedError otherwise
func (e *ConnectionClosed) PyTypeName() string {
	if e.Rcvd != nil && e.Sent != nil && okCloseCodes[e.Rcvd.Code] && okCloseCodes[e.Sent.Code] {
		return "ConnectionClosedOK"
	}
	return "ConnectionClosedError"
}
