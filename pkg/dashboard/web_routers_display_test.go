package dashboard

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Accepted socket double whose next receive is a disconnect with code once release fires
func displayTestSocket(code int, release <-chan struct{}) *WebSocket {
	receive := func(ctx context.Context) (Message, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
		return Message{Type: "websocket.disconnect", Code: code}, nil
	}
	send := func(context.Context, Message) error { return nil }
	ws := NewWebSocket(testScope("websocket", "GET", "/api/display/ws", nil), receive, send)
	ws.ClientState, ws.ApplicationState = WSConnected, WSConnected
	return ws
}

// Unix RFB listener under the profile home serving each connection with serve
func displayTestXvnc(t *testing.T, profileHome string, serve func(net.Conn)) {
	dir := filepath.Join(profileHome, "bot-desktop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "rfb.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
}

// Profile home with a path short enough for a unix socket under it
func displayShortHome(t *testing.T) string {
	dir, err := os.MkdirTemp("", "dsp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func displayTestInfo(profileHome string) *pyjson.Object {
	return pyjson.ObjectOf("hermes_home", profileHome, "viewer_id", "desk-1")
}

func TestOnlyACleanViewerCloseHandsTheScreenBack(t *testing.T) {
	for _, tc := range []struct {
		code       int
		humanKeeps bool
	}{{1006, true}, {1005, true}, {1000, false}} {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		profileHome := displayShortHome(t)
		if _, err := botdesktop.LeaseAcquire("desk-1", profileHome, ""); err != nil {
			t.Fatal(err)
		}
		displayTestXvnc(t, profileHome, func(conn net.Conn) {
			time.Sleep(time.Second)
			conn.Close()
		})
		release := make(chan struct{})
		time.AfterFunc(50*time.Millisecond, func() { close(release) })
		if err := displayBridge(context.Background(), displayTestSocket(tc.code, release), displayTestInfo(profileHome)); err != nil {
			t.Fatal(err)
		}
		after := botdesktop.LeaseGet(profileHome)
		if (after.Holder == botdesktop.LeaseHuman) != tc.humanKeeps {
			t.Fatalf("close %d: lease %+v", tc.code, after)
		}
	}
}

func TestExHolderWhoHandedBackIsNotEvictedByALaterTakeover(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	human := func(viewer string) botdesktop.BotDesktopLease {
		return botdesktop.BotDesktopLease{Holder: botdesktop.LeaseHuman, ViewerID: viewer}
	}
	held := &displayHeld{}
	if displayShouldEvict(held, human("desk-1"), "desk-1") {
		t.Fatal("own takeover evicted")
	}
	if displayShouldEvict(held, botdesktop.BotDesktopLease{Holder: botdesktop.LeaseAgent}, "desk-1") {
		t.Fatal("hand back evicted")
	}
	if displayShouldEvict(held, human("desk-2"), "desk-1") {
		t.Fatal("a viewer that handed back was evicted by a later takeover")
	}
	held = &displayHeld{}
	displayShouldEvict(held, human("desk-1"), "desk-1")
	if !displayShouldEvict(held, human("desk-2"), "desk-1") {
		t.Fatal("takeover while holding did not evict")
	}
}

func TestATakeoverMadeByAnotherProcessStopsInputWithinTheRefreshInterval(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	profileHome := displayShortHome(t)
	if _, err := botdesktop.LeaseAcquire("desk-1", profileHome, ""); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var allow func() bool
	prev := newRfbClientFilter
	t.Cleanup(func() { newRfbClientFilter = prev })
	newRfbClientFilter = func(allowInput func() bool) *botdesktop.RfbClientFilter {
		mu.Lock()
		allow = allowInput
		mu.Unlock()
		return prev(allowInput)
	}
	displayTestXvnc(t, profileHome, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- displayBridge(context.Background(), displayTestSocket(1000, finish), displayTestInfo(profileHome))
	}()
	captured := func() func() bool {
		mu.Lock()
		defer mu.Unlock()
		return allow
	}
	for captured() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if !captured()() {
		t.Fatal("holder may not send input")
	}
	text := `{"holder": "human", "viewer_id": "desk-2", "since": 0, "reason": "", "epoch": 2}`
	if err := os.WriteFile(filepath.Join(profileHome, "bot-desktop", "lease.json"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for captured()() && time.Since(start) < 2*time.Second {
		time.Sleep(20 * time.Millisecond)
	}
	elapsed := time.Since(start)
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("another process takeover seen after %v", elapsed)
	}
}

func TestNoBridgeTaskOrSocketOutlivesTheBridge(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	profileHome := displayShortHome(t)
	sawEOF := make(chan struct{})
	displayTestXvnc(t, profileHome, func(conn net.Conn) {
		_, _ = io.Copy(io.Discard, conn)
		close(sawEOF)
	})
	release := make(chan struct{})
	time.AfterFunc(50*time.Millisecond, func() { close(release) })
	if err := displayBridge(context.Background(), displayTestSocket(1000, release), displayTestInfo(profileHome)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sawEOF:
	case <-time.After(2 * time.Second):
		t.Fatal("Xvnc never saw EOF after the bridge returned")
	}
}

func TestDisplayTicketMustBeABotDesktopTicketPinnedToAProfileHome(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetWSTickets()
	t.Cleanup(resetWSTickets)
	socket := func(ticket string) *WebSocket {
		return NewWebSocket(testScope("websocket", "GET", "/api/display/ws?display_ticket="+ticket, nil), nil, nil)
	}
	gatewayTicket := MintWSTicket("u", "google", "", nil)
	if consumeDisplayTicket(socket(gatewayTicket)) != nil {
		t.Fatal("a gateway ticket opened the display")
	}
	unpinned := MintWSTicket("display:v", "bot-desktop", "", nil)
	if consumeDisplayTicket(socket(unpinned)) != nil {
		t.Fatal("a ticket without a profile home opened the display")
	}
	good := MintWSTicket("display:v", "bot-desktop", "", pyjson.ObjectOf("hermes_home", "/srv/icebath/bot-a", "viewer_id", "v"))
	info := consumeDisplayTicket(socket(good))
	if info == nil || info.GetOr("hermes_home", nil) != "/srv/icebath/bot-a" || info.GetOr("viewer_id", nil) != "v" {
		t.Fatalf("good ticket info %v", info)
	}
	if consumeDisplayTicket(socket(good)) != nil {
		t.Fatal("single use")
	}
}
