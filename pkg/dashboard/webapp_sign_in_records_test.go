package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Connection of an app whose state holds the given values
func webappConn(vals map[string]any) *HTTPConnection {
	return &HTTPConnection{Scope: &Scope{App: &FastAPI{State: NewState(vals)}}}
}

func TestWebappGatewayDigestsThePathlibAbsoluteHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix paths")
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	sum := sha256.Sum256([]byte("/srv/a/../gw"))
	want := hex.EncodeToString(sum[:])[:16]
	conn := webappConn(map[string]any{"webapp_home": "/srv/a/./../gw/", "ui_surface": "webapp"})
	if got := webappCookieGateway(conn); got != want {
		t.Fatalf("gateway %q want %q", got, want)
	}
	if got := webappCookieGateway(webappConn(map[string]any{"webapp_home": "/srv/gw"})); got != "" {
		t.Fatalf("off the webapp %q", got)
	}
	if got := webappGateway(webappConn(map[string]any{})); got != "" {
		t.Fatalf("no home %q", got)
	}
}

func TestWebappNativeSignInRecordsRoundTripThroughTheHomeFile(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	webappRecordsMu.Lock()
	clear(webappRecords)
	webappRecordsMu.Unlock()
	records := webappLoaded(h)
	records.Set(webappDigest("token"), int64(4102444800))
	webappWriteRecords(context.Background(), filepath.Join(h, webappRecordPath), records)
	text, err := os.ReadFile(filepath.Join(h, webappRecordPath))
	if err != nil || string(text) != `{"`+webappDigest("token")+`":4102444800}`+"\n" {
		t.Fatalf("record %q %v", text, err)
	}
	if err := os.WriteFile(filepath.Join(h, webappRecordPath), []byte(`{"a":1,"b":"x","c":2.5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	webappRecordsMu.Lock()
	clear(webappRecords)
	reloaded := webappLoaded(h)
	webappRecordsMu.Unlock()
	if len(reloaded.Keys) != 1 || reloaded.Vals["a"] != int64(1) {
		t.Fatalf("reloaded %v", reloaded.Vals)
	}
}
