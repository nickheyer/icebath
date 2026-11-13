package backend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	DefaultWsConnectTimeout        = 10 * time.Second
	DefaultWsReadyGrace            = 750 * time.Millisecond
	DefaultWsProgressCheckInterval = time.Second
	SpawnedBackendMaxConnectWait   = 90 * time.Second
	wsInvalidURLReason             = "TypeError [ERR_INVALID_URL]: Invalid URL"
	wsConnectionFailedReason       = "WebSocket connection failed."
)

// WsProbeOptions tune a socket probe, zero picks the default and a negative connect timeout or grace means none
type WsProbeOptions struct {
	ConnectTimeout        time.Duration
	ReadyGrace            time.Duration
	KeepWaitingWhile      func() bool
	ProgressCheckInterval time.Duration
	MaxConnectWait        time.Duration
	Headers               map[string]string
}

// WsProbeResult is the verdict of a live socket probe with the reason it failed
type WsProbeResult struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// wsProbeConn is the opened socket a probe reads one frame from and closes
type wsProbeConn interface {
	ReadMessage() (int, []byte, error)
	Close() error
}

// Opens the upgrade with extra headers, the gorilla dialer without a proxy as the Node runtime dials
var dialGatewayWebSocket = func(ctx context.Context, target string, header http.Header) (wsProbeConn, error) {
	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, target, header)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

type wsProbeEvent struct {
	kind   string
	conn   wsProbeConn
	code   int
	reason string
}

// Opens the socket URL and reports whether the gateway kept the upgrade past the ready grace
func ProbeGatewayWebSocket(wsURL string, opts WsProbeOptions) WsProbeResult {
	connectTimeout := opts.ConnectTimeout
	if connectTimeout == 0 {
		connectTimeout = DefaultWsConnectTimeout
	}
	readyGrace := opts.ReadyGrace
	if readyGrace == 0 {
		readyGrace = DefaultWsReadyGrace
	} else if readyGrace < 0 {
		readyGrace = 0
	}
	interval := opts.ProgressCheckInterval
	if interval == 0 {
		interval = DefaultWsProgressCheckInterval
	}
	maxWait := opts.MaxConnectWait
	if maxWait == 0 {
		maxWait = connectTimeout
	}
	var header http.Header
	if len(opts.Headers) > 0 {
		header = http.Header{}
		for k, v := range opts.Headers {
			header.Set(k, v)
		}
	}
	target, reason := websocketURLRecord(wsURL)
	if reason != "" {
		return WsProbeResult{Reason: reason}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan wsProbeEvent, 3)
	go func() {
		conn, err := dialGatewayWebSocket(ctx, target, header)
		if err != nil {
			events <- wsProbeEvent{kind: "error"}
			return
		}
		go func() {
			<-ctx.Done()
			_ = conn.Close()
		}()
		events <- wsProbeEvent{kind: "open", conn: conn}
		if _, _, err := conn.ReadMessage(); err == nil {
			events <- wsProbeEvent{kind: "message"}
		} else if closeErr := (*websocket.CloseError)(nil); errors.As(err, &closeErr) {
			events <- wsProbeEvent{kind: "close", code: closeErr.Code, reason: closeErr.Text}
		} else {
			events <- wsProbeEvent{kind: "error"}
		}
	}()

	var socket wsProbeConn
	finish := func(result WsProbeResult) WsProbeResult {
		cancel()
		if socket != nil {
			_ = socket.Close()
		}
		return result
	}
	startedAt := time.Now()
	hardCap := max(connectTimeout, maxWait)
	var connectC, graceC <-chan time.Time
	if connectTimeout > 0 {
		timer := time.NewTimer(connectTimeout)
		defer timer.Stop()
		connectC = timer.C
	}
	for {
		select {
		case ev := <-events:
			switch ev.kind {
			case "open":
				socket = ev.conn
				grace := time.NewTimer(readyGrace)
				defer grace.Stop()
				graceC = grace.C
			case "message":
				return finish(WsProbeResult{OK: true})
			case "close":
				return finish(WsProbeResult{Reason: wsCloseReason(ev.code, ev.reason, "The gateway accepted the connection then closed it (credential rejected?).")})
			default:
				return finish(WsProbeResult{Reason: wsConnectionFailedReason})
			}
		case <-graceC:
			return finish(WsProbeResult{OK: true})
		case <-connectC:
			elapsed := time.Since(startedAt)
			if !keepWaiting(opts.KeepWaitingWhile) {
				if elapsed > connectTimeout {
					return finish(WsProbeResult{Reason: fmt.Sprintf("Timed out after %dms waiting for the WebSocket to open (backend stopped after the %dms budget).", elapsed.Milliseconds(), connectTimeout.Milliseconds())})
				}
				return finish(WsProbeResult{Reason: fmt.Sprintf("Timed out after %dms waiting for the WebSocket to open.", connectTimeout.Milliseconds())})
			}
			if elapsed >= hardCap {
				return finish(WsProbeResult{Reason: fmt.Sprintf("Timed out after %dms waiting for the WebSocket to open (backend still running at the %dms cap).", elapsed.Milliseconds(), hardCap.Milliseconds())})
			}
			timer := time.NewTimer(min(interval, hardCap-elapsed))
			defer timer.Stop()
			connectC = timer.C
		}
	}
}

// Asks the keep waiting callback, a missing or panicking one failing closed
func keepWaiting(check func() bool) (alive bool) {
	if check == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			alive = false
		}
	}()
	return check()
}

// Validates a socket URL as the WebSocket constructor does, mapping http and https to ws and wss
func websocketURLRecord(raw string) (string, string) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return "", wsInvalidURLReason
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme == "http" || scheme == "https" || scheme == "ws" || scheme == "wss") && u.Host == "" {
		return "", wsInvalidURLReason
	}
	switch scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		u.Scheme = scheme
	default:
		return "", "expected a ws: or wss: url"
	}
	if u.Fragment != "" || strings.HasSuffix(raw, "#") {
		return "", "hash"
	}
	return u.String(), ""
}

// Appends the close code and reason to the fallback text
func wsCloseReason(code int, reason, fallback string) string {
	reason = strings.TrimSpace(reason)
	switch {
	case code != 0 && reason != "":
		return fmt.Sprintf("%s (code %d: %s)", fallback, code, reason)
	case code != 0:
		return fmt.Sprintf("%s (code %d)", fallback, code)
	case reason != "":
		return fmt.Sprintf("%s (%s)", fallback, reason)
	}
	return fallback
}

// Probe policy of a spawned child, waiting past the base budget while it lives up to the cold start cap
func SpawnedBackendProbeOptions(isChildAlive func() bool) WsProbeOptions {
	return WsProbeOptions{ConnectTimeout: DefaultWsConnectTimeout, MaxConnectWait: SpawnedBackendMaxConnectWait, KeepWaitingWhile: isChildAlive}
}
