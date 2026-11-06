//go:build !windows

package gateway

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Short directory under /tmp so unix socket paths fit sun_path
func shortControlHome(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "hgw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	h := filepath.Join(root, ".icebath")
	if err := os.Mkdir(h, 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestShortHomeBindsInHome(t *testing.T) {
	h := shortControlHome(t)
	bind, pointer, err := ResolveServerSocketPath(h)
	if err != nil || bind != filepath.Join(h, "gateway.sock") || pointer != "" {
		t.Fatalf("got %q %q %v", bind, pointer, err)
	}
}

func TestLongHomeUsesPointerFallback(t *testing.T) {
	deep := filepath.Join(t.TempDir(), strings.Repeat("x", 120), ".icebath")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	bind, pointer, err := ResolveServerSocketPath(deep)
	if err != nil || bind == filepath.Join(deep, "gateway.sock") || len(bind) > 100 || pointer != filepath.Join(deep, "gateway.sock.path") {
		t.Fatalf("got %q %q %v", bind, pointer, err)
	}
	if !strings.HasSuffix(bind, "/control.sock") || !strings.Contains(bind, "icebath-gw-") {
		t.Fatalf("fallback leaf %q", bind)
	}
}

func TestClientResolutionPrefersDirectThenPointer(t *testing.T) {
	h, tmp := t.TempDir(), t.TempDir()
	if p, err := ResolveClientSocketPath(h); p != "" || err != nil {
		t.Fatalf("empty home resolved %q %v", p, err)
	}
	target := filepath.Join(tmp, "elsewhere.sock")
	os.WriteFile(target, nil, 0o600)
	os.WriteFile(filepath.Join(h, "gateway.sock.path"), []byte("\ufeff "+target+"\n"), 0o600)
	if p, _ := ResolveClientSocketPath(h); p != target {
		t.Fatalf("pointer resolved %q", p)
	}
	direct := filepath.Join(h, "gateway.sock")
	os.WriteFile(direct, nil, 0o600)
	if p, _ := ResolveClientSocketPath(h); p != direct {
		t.Fatalf("direct resolved %q", p)
	}
}

func TestPointerToMissingOrUndecodableTarget(t *testing.T) {
	h := t.TempDir()
	os.WriteFile(filepath.Join(h, "gateway.sock.path"), []byte(filepath.Join(h, "gone.sock")), 0o600)
	if p, err := ResolveClientSocketPath(h); p != "" || err != nil {
		t.Fatalf("missing target resolved %q %v", p, err)
	}
	os.WriteFile(filepath.Join(h, "gateway.sock.path"), []byte{0xff, 0xfe, 'x'}, 0o600)
	if _, err := ResolveClientSocketPath(h); err == nil {
		t.Fatal("undecodable pointer must raise")
	}
	if IdentifyGateway(h, time.Second) != nil {
		t.Fatal("undecodable pointer must answer nil at the query level")
	}
}

func TestWindowsPipeNameIsStableAndHomeScoped(t *testing.T) {
	tmp := t.TempDir()
	a, _ := WindowsPipeName(filepath.Join(tmp, "a"))
	b, _ := WindowsPipeName(filepath.Join(tmp, "b"))
	again, _ := WindowsPipeName(filepath.Join(tmp, "a"))
	if !strings.HasPrefix(a, `\\.\pipe\icebath-gateway-`) || a == b || a != again || len(a) != len(`\\.\pipe\icebath-gateway-`)+16 {
		t.Fatalf("pipe names %q %q %q", a, b, again)
	}
}

func TestNoSocketReturnsNoneFast(t *testing.T) {
	h := t.TempDir()
	start := time.Now()
	if IdentifyGateway(h, DefaultControlClientTimeout) != nil || QueryGatewayControl(h, "status", nil, DefaultControlClientTimeout) != nil {
		t.Fatal("no socket must answer nil")
	}
	if time.Since(start) > time.Second {
		t.Fatal("no socket must answer fast")
	}
}

// Unix control listener answering each request line with reply(request)
func serveControl(t *testing.T, h string, reply func(req string) string) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(h, "gateway.sock"))
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
			line, _ := bufio.NewReader(conn).ReadString('\n')
			conn.Write([]byte(reply(line)))
			conn.Close()
		}
	}()
}

func TestQueryGatewayControlAnswers(t *testing.T) {
	h := shortControlHome(t)
	var got string
	answer := `{"ok": true, "result": {"pid": 7, "kind": "gw"}}` + "\n"
	serveControl(t, h, func(req string) string { got = req; return answer })
	r := MigrateGatewayProfileIdentity(h, "old", "new", time.Second)
	if r == nil || strings.Join(r.Keys, ",") != "pid,kind" {
		t.Fatalf("answer %#v", r)
	}
	if want := `{"verb": "migrate-profile-identity", "id": 1, "protocol": 1, "params": {"old": "old", "new": "new"}}` + "\n"; got != want {
		t.Fatalf("request %q", got)
	}
	if IdentifyGateway(h, time.Second) == nil || !strings.HasPrefix(got, `{"verb": "identify", "id": 1, "protocol": 1}`) {
		t.Fatalf("identify request %q", got)
	}
	for _, bad := range []string{`{"ok": false, "result": {"a": 1}}`, `{"ok": 1, "result": {"a": 1}}`,
		`{"ok": true, "result": [1]}`, `[1]`, `not json`, "", "\n{\"ok\": true, \"result\": {}}"} {
		answer = bad + "\n"
		if r := PurgeGatewayProfileIdentity(h, "x", time.Second); r != nil {
			t.Fatalf("answer %q gave %#v", bad, r)
		}
	}
	answer = strings.Repeat("y", maxControlResponseBytes+70000)
	if r := RescanGatewayProfiles(h, time.Second); r != nil {
		t.Fatalf("oversized answer gave %#v", r)
	}
	answer = `{"ok": true, "result": {"served_profiles": ["a"]}}`
	r = RescanGatewayProfiles(h, time.Second)
	if v, _ := r.Get("served_profiles"); len(v.([]any)) != 1 {
		t.Fatalf("eof terminated answer %#v", r)
	}
}

func TestReloadPluginsDefaultsProfileHomeToHome(t *testing.T) {
	h := shortControlHome(t)
	var got string
	serveControl(t, h, func(req string) string { got = req; return `{"ok": true, "result": {}}` + "\n" })
	if r := ReloadGatewayPlugins(h, "", time.Second); r == nil || r.Len() != 0 {
		t.Fatalf("answer %#v", r)
	}
	want, _ := pyjson.Dumps(h)
	if !strings.Contains(got, `"params": {"home": `+want+`}`) {
		t.Fatalf("request %q", got)
	}
}

func TestReadResponseLineStopsAtDeadline(t *testing.T) {
	calls := 0
	line, err := readResponseLine(func() ([]byte, error) { calls++; return []byte("ab"), nil }, time.Now().Add(-time.Second))
	if line != nil || err != nil || calls != 0 {
		t.Fatalf("expired deadline %q %v %d", line, err, calls)
	}
}
