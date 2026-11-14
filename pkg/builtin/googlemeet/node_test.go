package googlemeet

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/spf13/cobra"
)

func TestProtocolEncodeDecodeRoundtrip(t *testing.T) {
	req, err := makeRequest("status", "tok", pyjson.ObjectOf("a", 1), "")
	if err != nil {
		t.Fatal(err)
	}
	text, _ := encodeEnvelope(req)
	back, err := decodeEnvelope([]byte(text))
	if err != nil || !pyjson.Equal(back, req) {
		t.Fatal(back, err)
	}
	if ok, reason := validateRequest(back, "tok"); !ok || reason != "" {
		t.Fatal(reason)
	}
	if ok, reason := validateRequest(back, "other"); ok || reason != "token mismatch" {
		t.Fatal(reason)
	}
	if _, err := makeRequest("bogus", "tok", pyjson.NewObject(), ""); err == nil || err.Error() != "unknown request type: 'bogus'" {
		t.Fatal(err)
	}
	if _, err := decodeEnvelope([]byte("[1]")); err == nil || err.Error() != "envelope must be a JSON object" {
		t.Fatal(err)
	}
	if _, err := decodeEnvelope([]byte(`{"type": "x"}`)); err == nil || err.Error() != "envelope missing string 'id'" {
		t.Fatal(err)
	}
}

func TestRegistryAddGetRoundtripPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.json")
	reg := newNodeRegistry(context.Background(), path)
	if err := reg.add(context.Background(), "mac", "ws://mac.local:18789", "tok"); err != nil {
		t.Fatal(err)
	}
	entry, err := newNodeRegistry(context.Background(), path).get("mac")
	if err != nil || entry == nil || entry.Vals["name"] != "mac" || entry.Vals["url"] != "ws://mac.local:18789" || entry.Vals["token"] != "tok" {
		t.Fatal(entry, err)
	}
	if r, _ := reg.resolve(""); r == nil || r.Vals["name"] != "mac" {
		t.Fatal(r)
	}
	if err := reg.add(context.Background(), "", "u", "t"); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestServerEnsureTokenGeneratesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meetings", "node_token.json")
	old := umask(0o022)
	defer umask(old)
	tok, err := newNodeServer(context.Background(), "127.0.0.1", 0, path, "n").ensureToken(context.Background())
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(tok) {
		t.Fatal(tok, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
	again, _ := newNodeServer(context.Background(), "127.0.0.1", 0, path, "n").ensureToken(context.Background())
	if again != tok {
		t.Fatal(again, tok)
	}
}

// Websocket stand in answering each sent request through reply
type replyWS struct {
	sent  []string
	reply func(string) string
}

func (w *replyWS) Send(text string) error { w.sent = append(w.sent, text); return nil }
func (w *replyWS) Recv(time.Duration) (string, error) {
	return w.reply(w.sent[len(w.sent)-1]), nil
}
func (w *replyWS) Close() {}

func TestClientRPCSendsCorrectEnvelopeAndParsesResponse(t *testing.T) {
	ws := &replyWS{reply: func(raw string) string {
		req, _ := decodeEnvelope([]byte(raw))
		resp, _ := makeResponse(req.Vals["id"], pyjson.ObjectOf("ok", true, "echo", req.Vals["type"]))
		text, _ := encodeEnvelope(resp)
		return text
	}}
	var url string
	restore := dialSyncWS
	defer func() { dialSyncWS = restore }()
	dialSyncWS = func(u string, _ http.Header, _ time.Duration) (syncWS, error) {
		url = u
		return ws, nil
	}
	c, _ := newNodeClient("ws://remote:1", "tok123", 10*time.Second)
	out, err := c.rpc("ping", pyjson.ObjectOf("hello", 1))
	if err != nil || !pyjson.Equal(out, pyjson.ObjectOf("ok", true, "echo", "ping")) {
		t.Fatal(out, err)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(ws.sent[0]), &sent)
	if sent["type"] != "ping" || sent["token"] != "tok123" || sent["payload"].(map[string]any)["hello"] != 1.0 || sent["id"] == "" || url != "ws://remote:1" {
		t.Fatal(sent, url)
	}
}

func TestNodeServerAnswersARealWebsocketClient(t *testing.T) {
	isolateHome(t)
	server := newNodeServer(context.Background(), "127.0.0.1", 0, filepath.Join(t.TempDir(), "node_token.json"), "icebath-meet-node")
	tok, _ := server.ensureToken(context.Background())
	ts := httptest.NewServer(server.handler(context.Background()))
	defer ts.Close()
	c, _ := newNodeClient("ws"+strings.TrimPrefix(ts.URL, "http"), tok, 10*time.Second)
	pong, err := c.ping()
	if err != nil || pong.Vals["display_name"] != "icebath-meet-node" {
		t.Fatal(pong, err)
	}
	st, err := c.status()
	if err != nil || st.Vals["ok"] != false || st.Vals["reason"] != "no active meeting" {
		t.Fatal(st, err)
	}
	bad, _ := newNodeClient("ws"+strings.TrimPrefix(ts.URL, "http"), "wrong", 10*time.Second)
	if _, err := bad.ping(); err == nil || err.Error() != "node error: token mismatch" {
		t.Fatal(err)
	}
}

// Runs the node command tree on argv capturing stdout
func runNodeCLI(t *testing.T, argv ...string) (int, string) {
	t.Helper()
	root := &cobra.Command{Use: "meet-node-test"}
	registerNodeCLI(root)
	var out bytes.Buffer
	restore := cliOut
	cliOut = &out
	defer func() { cliOut = restore }()
	code := 0
	rd := nodeDispatch
	defer func() { nodeDispatch = rd }()
	nodeDispatch = func(ns map[string]any) int { code = nodeCommand(ns); return code }
	root.SetArgs(argv)
	root.SilenceErrors, root.SilenceUsage = true, true
	_ = root.Execute()
	return code, out.String()
}

func TestCLIApproveListRemove(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	if rc, _ := runNodeCLI(t, "approve", "mac", "ws://mac:1", "tok"); rc != 0 {
		t.Fatal(rc)
	}
	if e, _ := newNodeRegistry(ctx, "").get("mac"); e == nil {
		t.Fatal("not approved")
	}
	rc, out := runNodeCLI(t, "list")
	if rc != 0 || !strings.Contains(out, "mac") || !strings.Contains(out, "ws://mac:1") {
		t.Fatal(rc, out)
	}
	if rc, _ := runNodeCLI(t, "remove", "mac"); rc != 0 {
		t.Fatal(rc)
	}
	if e, _ := newNodeRegistry(ctx, "").get("mac"); e != nil {
		t.Fatal("not removed")
	}
}
