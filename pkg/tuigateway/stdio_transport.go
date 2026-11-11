package tuigateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Stream flushes are skipped when the no flush variable is set to a true word
var disableFlush = func() bool {
	switch strings.ToLower(pystr.Strip(os.Getenv("ICEBATH_TUI_GATEWAY_NO_FLUSH"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}()

func transportLog() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.transport")
}

type currentTransportKey struct{}

// Transport bound to the request context, nil when none is bound
func CurrentTransport(ctx context.Context) Transport {
	t, _ := ctx.Value(currentTransportKey{}).(Transport)
	return t
}

// Context whose handlers write to the given transport
func BindTransport(ctx context.Context, transport Transport) context.Context {
	return context.WithValue(ctx, currentTransportKey{}, transport)
}

// Returns when a stream error means the peer is gone and panics with any other error
func raiseUnlessPeerGone(err error, what string) {
	if errors.Is(err, syscall.EPIPE) {
		return
	}
	if errors.Is(err, fs.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) || !peerGoneErrno(errno) {
		panic(err)
	}
	transportLog().Debug(fmt.Sprintf("StdioTransport %s peer gone: %s", what, err))
}

// Writes JSON frames as lines to a stream resolved on every write
type StdioTransport struct {
	streamGetter func() io.Writer
	lock         *sync.Mutex
}

func NewStdioTransport(streamGetter func() io.Writer, lock *sync.Mutex) *StdioTransport {
	return &StdioTransport{streamGetter: streamGetter, lock: lock}
}

// Writes one frame line and flushes it, false only when the peer is gone
func (t *StdioTransport) Write(obj *pyjson.Object) bool {
	line := SerializeFrame(obj, "stdio", transportLog()) + "\n"
	t.lock.Lock()
	defer t.lock.Unlock()
	stream := t.streamGetter()
	if _, err := io.WriteString(stream, line); err != nil {
		raiseUnlessPeerGone(err, "write")
		return false
	}
	if disableFlush {
		return true
	}
	if flusher, ok := stream.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			raiseUnlessPeerGone(err, "flush")
			return false
		}
	}
	return true
}

func (t *StdioTransport) Close() {}
