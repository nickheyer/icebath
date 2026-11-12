package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Writer safe for the bridge goroutines and the test to share
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// Request against the bridge with an explicit Host header
func bridgeHTTP(t *testing.T, port int, method, path, host, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://127.0.0.1:"+strconv.Itoa(port)+path, strings.NewReader(body))
	if host != "" {
		req.Host = host
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestBridgeServesTheHTTPAPIBeforeWhatsAppConnects(t *testing.T) {
	hermeticHome(t)
	session := filepath.Join(t.TempDir(), "session")
	port := freePort(t)
	out := &lockedBuffer{}
	b := startBridge(context.Background(), bridgeArgs{Port: port, Session: session, Mode: "bot"}, out,
		map[string]string{"WHATSAPP_SEND_READ_RECEIPTS": "true", "WHATSAPP_DM_POLICY": "pairing"})
	defer b.Kill()
	if err := eventually(10*time.Second, func() bool { return !portIsFree(port) }); err != nil {
		t.Fatalf("bridge never listened: %s", out.String())
	}
	status, health := bridgeHTTP(t, port, "GET", "/health", "", "")
	abs, _ := filepath.Abs(session)
	if status != 200 || health["status"] != "disconnected" || health["session"] != abs || health["sendReadReceipts"] != true ||
		health["capabilities"].(map[string]any)["outboundMentions"] != true || health["scriptHash"] == "" {
		t.Fatalf("health = %d %v", status, health)
	}
	if status, body := bridgeHTTP(t, port, "GET", "/health", "evil.example:"+strconv.Itoa(port), ""); status != 400 ||
		body["error"] != "Invalid Host header. Bridge accepts loopback hosts only." {
		t.Fatalf("rebinding host = %d %v", status, body)
	}
	if status, body := bridgeHTTP(t, port, "POST", "/send", "", `{"chatId":"1@s.whatsapp.net","message":"hi"}`); status != 503 || body["error"] != "Not connected to WhatsApp" {
		t.Fatalf("send = %d %v", status, body)
	}
	if status, body := bridgeHTTP(t, port, "POST", "/typing", "", `{"chatId":"1@s.whatsapp.net"}`); status != 503 || body["error"] != "Not connected" {
		t.Fatalf("typing = %d %v", status, body)
	}
	if status, body := bridgeHTTP(t, port, "GET", "/chat/15551234567@s.whatsapp.net", "", ""); status != 200 || body["name"] != "15551234567" || body["isGroup"] != false {
		t.Fatalf("chat = %d %v", status, body)
	}
	for _, want := range []string{"WhatsApp bridge listening on port " + strconv.Itoa(port) + " (mode: bot)",
		"WHATSAPP_DM_POLICY=pairing — unknown DMs are forwarded for gateway pairing."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("console lacks %q:\n%s", want, out.String())
		}
	}
	b.Terminate()
	if code, done := b.Wait(5 * time.Second); !done || code != -15 {
		t.Fatalf("terminate = %d %v", code, done)
	}
	if err := eventually(5*time.Second, func() bool { return portIsFree(port) }); err != nil {
		t.Fatal("port held after the bridge stopped")
	}
}

func TestBridgeExitsOnAStaleSessionLikeALoggedOutSocket(t *testing.T) {
	hermeticHome(t)
	session := filepath.Join(t.TempDir(), "session")
	if err := os.MkdirAll(session, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "creds.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}
	b := startBridge(context.Background(), bridgeArgs{Port: freePort(t), Session: session}, out, map[string]string{})
	code, done := b.Wait(60 * time.Second)
	if !done || code != 1 {
		t.Fatalf("exit = %d %v:\n%s", code, done, out.String())
	}
	if !strings.Contains(out.String(), "Logged out. Delete session and restart to re-authenticate.") {
		t.Fatalf("console = %s", out.String())
	}
}

func TestBridgeExitsWhenItsPortIsTaken(t *testing.T) {
	hermeticHome(t)
	ln, port := holdPort(t)
	defer ln.Close()
	out := &lockedBuffer{}
	b := startBridge(context.Background(), bridgeArgs{Port: port, Session: t.TempDir()}, out, map[string]string{})
	if code, done := b.Wait(10 * time.Second); !done || code != 1 {
		t.Fatalf("exit = %d %v", code, done)
	}
	if !strings.Contains(out.String(), "FATAL: port "+strconv.Itoa(port)+" already in use on 127.0.0.1") {
		t.Fatalf("console = %s", out.String())
	}
}

func TestBridgeHelpersMatchTheNodeBridge(t *testing.T) {
	if got := splitLongMessage("aaaa bbbb\ncccc dddd", 10); strings.Join(got, "|") != "aaaa bbbb|cccc dddd" {
		t.Fatalf("split = %q", got)
	}
	if got := bridgeNormalizeID("447999674698:14@s.whatsapp.net"); got != "447999674698@s.whatsapp.net" {
		t.Fatalf("normalize = %q", got)
	}
	if got := normalizeWhatsAppIdentifier("+351912345678:3@s.whatsapp.net"); got != "351912345678" {
		t.Fatalf("identifier = %q", got)
	}
	if got := redactWhatsAppID("15551234567:2@s.whatsapp.net"); got != "…4567@s.whatsapp.net" {
		t.Fatalf("redact = %q", got)
	}
	if _, _, _, err := buildPollPayload("q", []any{"only"}, nil); err == nil || err.Error() != "at least two poll options are required" {
		t.Fatalf("poll err = %v", err)
	}
	if _, _, err := buildLocationPayload(float64(91), float64(0)); err == nil || err.Error() != "latitude/longitude out of range" {
		t.Fatalf("location err = %v", err)
	}
	if got := classifyOwnerMessageGate(true, true, newOutboundIDTracker(4), func(string) bool { return false }, "id", "chat"); got != "drop_allowlist" {
		t.Fatalf("gate = %q", got)
	}
}

func TestRunPairingEmitsJSONEventsAndFailsWithoutAReachableServer(t *testing.T) {
	hermeticHome(t)
	session := filepath.Join(t.TempDir(), "session")
	out := &lockedBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if code := RunPairing(ctx, session, "self-chat", out, true); code != 1 {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	var events []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["ts"] != nil {
			events = append(events, ev["event"].(string))
		}
	}
	if len(events) < 2 || events[0] != "started" || events[len(events)-1] != "error" {
		t.Fatalf("events = %v:\n%s", events, out.String())
	}
}
