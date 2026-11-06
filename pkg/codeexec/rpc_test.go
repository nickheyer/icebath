package codeexec

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestWatchPatternsBlocked(t *testing.T) {
	if !slices.Contains(terminalBlockedParams, "watch_patterns") {
		t.Fatal("watch_patterns is not blocked")
	}
}

type recordedCall struct {
	name string
	args map[string]any
}

// Dispatch fake echoing the call back as JSON and recording every call
func echoDispatch(mu *sync.Mutex, seen *[]recordedCall) dispatchFunc {
	return func(ctx context.Context, name string, args any) string {
		plain, _ := pyjson.Plain(args).(map[string]any)
		mu.Lock()
		*seen = append(*seen, recordedCall{name, plain})
		mu.Unlock()
		b, _ := json.Marshal(map[string]any{"name": name, "args": plain})
		return string(b)
	}
}

func writeRequest(t *testing.T, path string, req map[string]any) {
	t.Helper()
	b, _ := json.Marshal(req)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was never consumed", path)
}

func readResult(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return out
}

func TestFilePollLoopAuthorityAllowListAndBudget(t *testing.T) {
	env := testEnv(t)
	rpc := filepath.Join(t.TempDir(), "rpc with spaces")
	if err := os.MkdirAll(rpc, 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []recordedCall
	budget := newRPCBudget([]string{"terminal", "read_file"}, 2)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		rpcPollLoop(t.Context(), env, newShellEnv(t, rpc), rpc, "owner", budget, stop, "right-token", echoDispatch(&mu, &seen))
		close(done)
	}()
	defer func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("poller never stopped")
		}
	}()
	for _, c := range []struct {
		seq   int
		token any
	}{{9001, nil}, {9002, "wrong-token"}} {
		req := map[string]any{"seq": c.seq, "tool": "terminal", "args": map[string]any{"command": "forbidden"}}
		if c.token != nil {
			req["token"] = c.token
		}
		path := filepath.Join(rpc, "req_"+itoa(c.seq))
		writeRequest(t, path, req)
		waitGone(t, path)
		if _, err := os.Stat(filepath.Join(rpc, "res_00"+itoa(c.seq))); err == nil {
			t.Fatal("unauthorized request was answered")
		}
	}
	mu.Lock()
	if len(seen) != 0 || budget.callCount() != 0 {
		t.Fatalf("unauthorized dispatch %v", seen)
	}
	mu.Unlock()
	writeRequest(t, filepath.Join(rpc, "req_000001"), map[string]any{"seq": 1, "token": "right-token", "tool": "terminal",
		"args": map[string]any{"command": "echo fixture", "background": true, "watch_patterns": []string{"x"}, "pty": true}})
	waitGone(t, filepath.Join(rpc, "req_000001"))
	res := readResult(t, filepath.Join(rpc, "res_000001"))
	if res["name"] != "terminal" || len(res["args"].(map[string]any)) != 1 {
		t.Fatalf("blocked params reached dispatch %v", res)
	}
	writeRequest(t, filepath.Join(rpc, "req_000002"), map[string]any{"seq": 2, "token": "right-token", "tool": "unauthorized-tool", "args": map[string]any{}})
	waitGone(t, filepath.Join(rpc, "req_000002"))
	if msg, _ := readResult(t, filepath.Join(rpc, "res_000002"))["error"].(string); !strings.Contains(msg, "not available") ||
		!strings.Contains(msg, "Available: read_file, terminal") {
		t.Fatalf("allow list message %q", msg)
	}
	for seq := 3; seq <= 4; seq++ {
		writeRequest(t, filepath.Join(rpc, "req_00000"+itoa(seq)), map[string]any{"seq": seq, "token": "right-token", "tool": "read_file", "args": map[string]any{"path": "x"}})
		waitGone(t, filepath.Join(rpc, "req_00000"+itoa(seq)))
	}
	if msg, _ := readResult(t, filepath.Join(rpc, "res_000004"))["error"].(string); !strings.Contains(msg, "limit reached (2)") {
		t.Fatalf("budget message %q", msg)
	}
	if budget.callCount() != 2 || len(budget.entries()) != 2 {
		t.Fatalf("budget %d log %v", budget.callCount(), budget.entries())
	}
	if st, err := os.Stat(filepath.Join(rpc, "res_000001")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("response file mode %v %v", st, err)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestSocketServerLoopTokenDecodeAndDispatch(t *testing.T) {
	env := testEnv(t)
	sock := filepath.Join(t.TempDir(), "rpc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var seen []recordedCall
	budget := newRPCBudget([]string{"read_file"}, 5)
	done := make(chan struct{})
	go func() {
		rpcServerLoop(t.Context(), env, ln, "owner", budget, make(chan struct{}), "tok", echoDispatch(&mu, &seen))
		close(done)
	}()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	exchange := func(line string) map[string]any {
		if _, err := conn.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		resp, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(resp), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := exchange("not json"); got["error"] != "Invalid RPC request: Expecting value: line 1 column 1 (char 0)" {
		t.Fatalf("decode error %v", got)
	}
	if got := exchange("\xff\xfe"); !strings.HasPrefix(got["error"].(string), "Invalid RPC request: 'utf-8' codec can't decode byte 0xff in position 0") {
		t.Fatalf("utf-8 error %v", got)
	}
	if got := exchange(`{"tool":"read_file","args":{},"token":"nope"}`); got["error"] != "Unauthorized RPC request" {
		t.Fatalf("token check %v", got)
	}
	if got := exchange(`{"tool":"read_file","args":{"path":"é"},"token":"tok"}`); got["name"] != "read_file" {
		t.Fatalf("dispatch %v", got)
	}
	conn.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server loop outlived its client")
	}
	if len(seen) != 1 || seen[0].args["path"] != "é" || budget.callCount() != 1 {
		t.Fatalf("seen %v", seen)
	}
}

func TestSocketServerLoopStopsBeforeAnyClient(t *testing.T) {
	env := testEnv(t)
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		rpcServerLoop(t.Context(), env, ln, "owner", newRPCBudget(nil, 1), stop, "tok", nil)
		close(done)
	}()
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("accept loop ignored stop")
	}
}

func TestResultErrorAndToolErrorsSince(t *testing.T) {
	cases := map[string]string{
		`{"error": "boom"}`:      "boom",
		`{"error": ""}`:          "",
		`{"ok": true}`:           "",
		`["error"]`:              "",
		`{"error": {"code": 1}}`: "{'code': 1}",
		`{"error": "` + strings.Repeat("x", 400) + `"}`: strings.Repeat("x", 300),
		`{"error" broken`: "",
	}
	for in, want := range cases {
		if got := resultError(in); got != want {
			t.Fatalf("%s -> %q want %q", in, got, want)
		}
	}
	var log []toolCallEntry
	for i := 0; i < 8; i++ {
		log = append(log, toolCallEntry{Tool: "t" + itoa(i), Error: map[bool]string{true: "bad"}[i%2 == 0 || i > 3]})
	}
	got := toolErrorsSince(log, 1)
	if len(got) != 5 || got[0]["tool"] != "t2" || got[4]["tool"] != "t7" {
		t.Fatalf("errors since %v", got)
	}
}

func TestPrivateDirsAndCheckedRemoteWrite(t *testing.T) {
	if got := privateDirsCmd("/r o", "/r o/a", "/r o/b"); got != "umask 077 && mkdir -p '/r o/a' '/r o/b' && chmod 700 '/r o' '/r o/a' '/r o/b'" {
		t.Fatalf("dirs cmd %q", got)
	}
	if got := privateDirsCmd("/r"); got != "umask 077 && mkdir -p /r && chmod 700 /r" {
		t.Fatalf("root only %q", got)
	}
	root := t.TempDir()
	sh := newShellEnv(t, root)
	target := filepath.Join(root, "with space", "payload.txt")
	if _, err := executeChecked(t.Context(), sh, privateDirsCmd(filepath.Dir(target)), "mkdir", 10, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := remoteWrite(t.Context(), sh, target, "café ✓\n", true, 30, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	st, _ := os.Stat(target)
	if string(b) != "café ✓\n" || st.Mode().Perm() != 0o600 {
		t.Fatalf("payload %q mode %v", b, st.Mode())
	}
	_, err := remoteWrite(t.Context(), sh, filepath.Join(root, "missing", "x"), "data", false, 30, true)
	if err == nil || !strings.HasPrefix(err.Error(), "remote file ship for '"+filepath.Join(root, "missing", "x")+"' failed: '") {
		t.Fatalf("checked failure %v", err)
	}
}

func TestMissingTokenRejected(t *testing.T) {
	env := testEnv(t)
	sock := filepath.Join(t.TempDir(), "rpc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var seen []recordedCall
	done := make(chan struct{})
	go func() {
		rpcServerLoop(t.Context(), env, ln, "owner", newRPCBudget([]string{"terminal"}, 5), make(chan struct{}), "secret-token", echoDispatch(&mu, &seen))
		close(done)
	}()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte(`{"tool":"terminal","args":{"command":"echo hi"}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	var out map[string]any
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatal(err)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "Unauthorized") {
		t.Fatalf("response %v", out)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server loop outlived its client")
	}
	if len(seen) != 0 {
		t.Fatalf("unauthorized request dispatched %v", seen)
	}
}
