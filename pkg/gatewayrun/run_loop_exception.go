package gatewayrun

import (
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Python class names of the transient network errors the loop logs and swallows
var transientNetworkErrorClassNames = map[string]bool{
	"TimedOut": true, "NetworkError": true, "ReadError": true, "WriteError": true, "ConnectError": true,
	"ConnectTimeout": true, "ReadTimeout": true, "WriteTimeout": true, "PoolTimeout": true,
	"RemoteProtocolError": true, "ServerDisconnectedError": true, "ClientConnectorError": true, "ClientOSError": true}

// Python class name of one link of an error chain, never looking past the link itself
func ownExceptionName(err error) string {
	if named, ok := err.(interface{ ErrorKind() string }); ok && named.ErrorKind() != "" {
		return named.ErrorKind()
	}
	name := strings.TrimLeft(fmt.Sprintf("%T", err), "*")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// Whether one link of an error chain is a Go network failure a retry can outlive: a timeout, a socket operation that failed, a refused, reset or aborted connection, or a connection cut mid read
func isGoTransientNetworkLink(err error) bool {
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	if _, ok := err.(*net.OpError); ok {
		return true
	}
	if errno, ok := err.(syscall.Errno); ok {
		switch errno {
		case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH:
			return true
		}
	}
	return err == io.ErrUnexpectedEOF
}

// Next link of an error chain, the first of a joined error
func causeOf(err error) error {
	if next := errors.Unwrap(err); next != nil {
		return next
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		if errs := joined.Unwrap(); len(errs) > 0 {
			return errs[0]
		}
	}
	return nil
}

// True for a transient network error, a known class name or a Go network failure, anywhere in the first twelve links of its cause chain
func isTransientNetworkError(err error) bool {
	seen := map[error]bool{}
	cur := err
	for depth := 0; cur != nil && depth < 12; depth++ {
		if reflect.TypeOf(cur).Comparable() {
			if seen[cur] {
				break
			}
			seen[cur] = true
		}
		if transientNetworkErrorClassNames[ownExceptionName(cur)] || isGoTransientNetworkLink(cur) {
			return true
		}
		cur = causeOf(cur)
	}
	return false
}

// Loop safety net logging and swallowing transient network errors, every other failure to the default handler
func gatewayLoopExceptionHandler(loop *gateway.CallbackLoop, c gateway.LoopExceptionContext) {
	if c.Exception != nil && isTransientNetworkError(c.Exception) {
		task := c.Task
		if task == "" {
			task = "<unknown task>"
		}
		text := c.Exception.Error()
		if label := ownExceptionName(c.Exception); transientNetworkErrorClassNames[label] {
			text = label + ": " + text
		}
		pkgLog().Warn(fmt.Sprintf("Gateway swallowed transient network error from %s: %s", task, text), "exc_info", c.Exception)
		return
	}
	loop.DefaultExceptionHandler(c)
}
