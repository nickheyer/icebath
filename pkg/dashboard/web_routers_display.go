package dashboard

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	displayActivityStamp = 60 * time.Second
	displayReadChunk     = 64 * 1024
	closeControlTaken    = 4000
	closeDesktopGone     = 4001
	closeBadTicket       = 4401
	closeNotAllowed      = 4403
	closeProtocol        = 1003
	displayLeaseRefresh  = 250 * time.Millisecond
)

// Constructor of the client stream filter every bridge runs its viewer input through
var newRfbClientFilter = botdesktop.NewRfbClientFilter

// Router of the raw RFB over WebSocket bridge for the Bot Desktop viewer
func displayRouter() *APIRouter {
	r := NewAPIRouter("")
	r.WebSocket("/api/display/ws", &WSEndpoint{
		Name:   "display_ws",
		Handle: func(ctx context.Context, c *Call) error { return displayWS(ctx, c.WebSocket) },
	})
	return r
}

// Per connection memory of whether this viewer ever held control
type displayHeld struct {
	mu   sync.Mutex
	ever bool
}

// Whether a lease change evicts this viewer, true only when another human took control it once held
func displayShouldEvict(held *displayHeld, lease botdesktop.BotDesktopLease, viewerID string) bool {
	held.mu.Lock()
	defer held.mu.Unlock()
	if lease.Holder != botdesktop.LeaseHuman {
		held.ever = false
		return false
	}
	if lease.ViewerID == viewerID {
		held.ever = true
		return false
	}
	return held.ever
}

// Bot desktop ticket the socket query carries, nil when missing, spent, foreign or homeless
func consumeDisplayTicket(ws *WebSocket) *pyjson.Object {
	ticket := ws.QueryParams().Get("display_ticket")
	if ticket == "" {
		return nil
	}
	info, err := ConsumeWSTicket(ticket)
	if err != nil {
		return nil
	}
	if v, _ := info.Get("provider"); v != "bot-desktop" {
		return nil
	}
	if v, _ := info.Get("hermes_home"); !pyrepr.Truthy(v) {
		return nil
	}
	return info
}

// Accepts an allowed viewer socket and bridges it to the profile RFB stream its ticket names
func displayWS(ctx context.Context, ws *WebSocket) error {
	if !wsRequestIsAllowed(ws) {
		return ws.Close(ctx, closeNotAllowed, "")
	}
	if err := ws.Accept(ctx, nil, nil); err != nil {
		return err
	}
	info := consumeDisplayTicket(ws)
	if info == nil {
		return ws.Close(ctx, closeBadTicket, "display ticket missing, expired or used")
	}
	if displaySignedOut(ctx, ws, info) {
		return ws.Close(ctx, closeBadTicket, "Signed out")
	}
	return displayBridgeRun(ctx, ws, info)
}

// Viewer bridge of an admitted display socket, the real bridge swapped only where a hermes case installs a fake
var displayBridgeRun = displayBridge

// Whether the browser tab that asked for this viewer signed out since, tracking the socket with that tab otherwise
func displaySignedOut(ctx context.Context, ws *WebSocket, info *pyjson.Object) bool {
	v, ok := info.Get("admitted_with")
	if !ok || v == nil {
		return false
	}
	admitted := pyrepr.Str(v)
	if !BrowserCredentialIsCurrent(&admitted) {
		return true
	}
	trackBrowserSocket(ctx, ws, admitted)
	return false
}

// Reader, writer and relay process of the profile Xvnc, a socket or a sandbox exec stream
func openRFB(profileHome string) (io.Reader, io.WriteCloser, *tools.StreamProcess, error) {
	env := &tools.Env{Home: profileHome}
	running, err := botdesktop.SandboxScreenRunning(env)
	if err != nil {
		return nil, nil, nil, err
	}
	if running {
		relay, err := botdesktop.OpenRFBStream(env)
		if err != nil {
			return nil, nil, nil, err
		}
		return relay.Stdout, relay.Stdin, relay, nil
	}
	sock := filepath.Join(profileHome, "bot-desktop", "rfb.sock")
	if fi, _ := pypath.PathlibStat(sock); fi == nil {
		return nil, nil, nil, &os.PathError{Op: "open", Path: sock, Err: errors.New("rfb.sock missing")}
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil, nil, err
	}
	return conn, conn, nil, nil
}

// Whether an RFB open failure is an OS error the viewer hears as the desktop being gone
func rfbOpenOSError(err error) bool {
	var pathErr *os.PathError
	var opErr *net.OpError
	return errors.As(err, &pathErr) || errors.As(err, &opErr) || pypath.IsOSError(err)
}

// Pumps RFB bytes between the accepted viewer socket and the profile Xvnc, input gated by the lease
func displayBridge(ctx context.Context, ws *WebSocket, info *pyjson.Object) error {
	rawHome, _ := info.Get("hermes_home")
	profileHome := pyrepr.Str(rawHome)
	profileKey := home.Key(profileHome)
	viewerID := "viewer"
	for _, key := range []string{"viewer_id", "user_id"} {
		if v, _ := info.Get(key); pyrepr.Truthy(v) {
			viewerID = pyrepr.Str(v)
			break
		}
	}
	reader, writer, relay, err := openRFB(profileHome)
	if err != nil {
		if !rfbOpenOSError(err) {
			return err
		}
		slog.Warn("display ws: cannot reach RFB endpoint", "home", profileHome, "error", err)
		return ws.Close(ctx, closeDesktopGone, "Bot Desktop is not running")
	}
	held := &displayHeld{ever: botdesktop.LeaseViewerMaySendInput(viewerID, profileHome)}
	var allowedMu sync.Mutex
	allowed, allowedAt := held.ever, time.Now()
	refreshAllowed := func(lease *botdesktop.BotDesktopLease) {
		if lease == nil {
			current := botdesktop.LeaseGet(profileHome)
			lease = &current
		}
		allowedMu.Lock()
		allowed = lease.Holder == botdesktop.LeaseHuman && lease.ViewerID == viewerID
		allowedAt = time.Now()
		allowedMu.Unlock()
	}
	mayInput := func() bool {
		allowedMu.Lock()
		stale := time.Since(allowedAt) > displayLeaseRefresh
		allowedMu.Unlock()
		if stale {
			refreshAllowed(nil)
		}
		allowedMu.Lock()
		defer allowedMu.Unlock()
		return allowed
	}
	evicted := make(chan struct{})
	var evictOnce sync.Once
	unsubscribe := botdesktop.LeaseOnChange(func(key string, lease botdesktop.BotDesktopLease) {
		if key != profileKey {
			return
		}
		refreshAllowed(&lease)
		if displayShouldEvict(held, lease, viewerID) {
			evictOnce.Do(func() { close(evicted) })
		}
	})
	filter := newRfbClientFilter(mayInput)
	activityFile := filepath.Join(profileHome, "bot-desktop", "activity")
	var stamped time.Time
	stampActivity := func() {
		if time.Since(stamped) < displayActivityStamp {
			return
		}
		stamped = time.Now()
		touchFile(activityFile)
	}
	viewerClosed := false
	cctx, cancel := context.WithCancel(ctx)
	results := make(chan error, 3)
	go func() {
		buf := make([]byte, displayReadChunk)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				stampActivity()
				if serr := ws.SendBytes(cctx, append([]byte(nil), buf[:n]...)); serr != nil {
					results <- serr
					return
				}
			}
			if err == io.EOF {
				results <- nil
				return
			}
			if err != nil {
				results <- err
				return
			}
		}
	}()
	go func() {
		results <- func() error {
			for {
				message, err := ws.Receive(cctx)
				if err != nil {
					return err
				}
				if message.Type == "websocket.disconnect" {
					if message.Code == 1000 || message.Code == 1001 {
						viewerClosed = true
					}
					return nil
				}
				if message.Bytes == nil {
					return ws.Close(cctx, closeProtocol, "RFB is binary")
				}
				out, err := filter.Feed(message.Bytes)
				if err != nil {
					reason := []rune(err.Error())
					return ws.Close(cctx, closeProtocol, string(reason[:min(len(reason), 100)]))
				}
				if len(out) > 0 {
					if _, err := writer.Write(out); err != nil {
						return err
					}
				}
			}
		}()
	}()
	go func() {
		select {
		case <-evicted:
			results <- ws.Close(cctx, closeControlTaken, "control-taken")
		case <-cctx.Done():
			results <- nil
		}
	}()
	first := <-results
	cancel()
	_ = writer.Close()
	if closer, ok := reader.(io.Closer); ok {
		_ = closer.Close()
	}
	if relay != nil {
		_ = relay.Kill()
	}
	for range 2 {
		<-results
	}
	var disconnect *WebSocketDisconnect
	if first != nil && !errors.As(first, &disconnect) && !errors.Is(first, net.ErrClosed) && !errors.Is(first, context.Canceled) {
		slog.Debug("display ws ended", "error", first)
	}
	unsubscribe()
	if viewerClosed && botdesktop.LeaseViewerMaySendInput(viewerID, profileHome) {
		_, _ = botdesktop.LeaseRelease(botdesktop.LeaseReleaseOptions{ViewerID: &viewerID, ProfileKey: profileHome})
	}
	_ = ws.Close(context.Background(), 1000, "")
	return nil
}

// Creates path when missing and stamps its modification time, ignoring OS errors
func touchFile(path string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return
	}
	_ = f.Close()
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}
