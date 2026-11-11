package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// Whether something accepts connections on host and port, any failure but a refusal reading as live
func HasLiveListener(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), time.Second)
	if err == nil {
		conn.Close()
		return true
	}
	return !errors.Is(err, syscall.ECONNREFUSED)
}

// Starts the app on host and port, exclusive on macOS with one reuse retry past a dead TIME_WAIT socket
func StartTCPSite(ctx context.Context, app *WebApplication, host *string, port int, logTag string, accessLog AccessLog) (*WebAppRunner, error) {
	exclusive := runtime.GOOS == "darwin"
	opts := ListenOptions{AccessLog: accessLog}
	if exclusive {
		reuse := false
		opts.ReuseAddress = &reuse
	}
	runner, err := StartWebApp(ctx, app, host, port, opts)
	if err == nil {
		return runner, nil
	}
	if !exclusive || !errors.Is(err, syscall.EADDRINUSE) || IsWildcardHost(host) {
		return nil, err
	}
	if HasLiveListener(*host, port) {
		return nil, err
	}
	slog.Default().With("logger", "gateway.platforms.tcp_site").Info(fmt.Sprintf(
		"[%s] %s:%d busy without a live listener (TIME_WAIT from the previous gateway); rebinding with SO_REUSEADDR",
		logTag, *host, port))
	reuse := true
	opts.ReuseAddress = &reuse
	return StartWebApp(ctx, app, host, port, opts)
}
