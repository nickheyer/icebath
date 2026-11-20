package gateway

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"syscall"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// DiscoveryError is a bounded local control refusal, Reason the stable code and Detail what the user reads
type DiscoveryError struct {
	Reason, Cause, Fix, Detail string
}

func newDiscoveryError(reason, cause, fix string) *DiscoveryError {
	e := &DiscoveryError{Reason: reason, Cause: cause, Fix: fix, Detail: cause}
	if cause != "" && fix != "" {
		e.Detail = cause + "; " + fix
	}
	return e
}

func (e *DiscoveryError) Error() string {
	if e.Detail != "" {
		return e.Reason + ": " + e.Detail
	}
	return e.Reason
}

// Detail within limit characters, an unfitting repair command dropped with a notice and a long cause cut visibly
func (e *DiscoveryError) BoundedDetail(limit int) string {
	if e.Detail == "" || utf8.RuneCountInString(e.Detail) <= limit {
		return e.Detail
	}
	notice := ""
	if e.Fix != "" {
		notice = "; repair command omitted (path too long to print)"
	}
	room, marker, cause := limit-len(notice), "… [truncated]", []rune(e.Cause)
	if len(cause) > room {
		cause = append(cause[:room-utf8.RuneCountInString(marker)], []rune(marker)...)
	}
	return string(cause) + notice
}

// ErrControlTimeout is the TimeoutError of a control query that ran out of its budget
var ErrControlTimeout = errors.New("timed out")

var identifyRequest = []byte(`{"protocol":1,"verb":"identify","id":1}` + "\n")

// Swappable identify query and uid source of discovery
var (
	queryIdentifySeam = QueryIdentify
	discoveryGetuid   = os.Getuid
)

// Result object of the first response line, the invalid_control_response refusal for any other shape
func identifyResponse(data []byte) (*pyjson.Object, error) {
	line := data
	for i, c := range data {
		if c == '\n' {
			line = data[:i]
			break
		}
	}
	decoded, err := pyjson.LoadsBytes(line)
	if err != nil {
		return nil, err
	}
	response, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, newDiscoveryError("invalid_control_response", "", "")
	}
	okValue, _ := response.Get("ok")
	protocol, _ := response.Get("protocol")
	id, _ := response.Get("id")
	result, _ := response.Get("result")
	resultObj, isObj := result.(*pyjson.Object)
	if okValue != true || !pyEq(protocol, 1) || !pyEq(id, 1) || !isObj {
		return nil, newDiscoveryError("invalid_control_response", "", "")
	}
	return resultObj, nil
}

// Python exception class a discovery failure maps to, in the order discovery catches them
func discoveryErrorKind(err error) string {
	var de *DiscoveryError
	var ne net.Error
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED):
		return "missing"
	case errors.Is(err, ErrControlTimeout) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, syscall.ETIMEDOUT) ||
		(errors.As(err, &ne) && ne.Timeout()):
		return "timeout"
	case errors.As(err, &de):
		return "discovery"
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	}
	return "other"
}
