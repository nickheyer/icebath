//go:build !windows

package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
)

// Started control server for home with the given verbs, stopped when the test ends
func startControlServer(t *testing.T, h string, handlers map[string]ControlVerbHandler) *GatewayControlServer {
	t.Helper()
	server, err := NewGatewayControlServer(h, handlers)
	if err != nil {
		t.Fatal(err)
	}
	if !server.Start() {
		t.Fatal("control server did not start")
	}
	t.Cleanup(server.Stop)
	return server
}

// Handler answering a fixed result
func fixedVerb(result map[string]any) ControlVerbHandler {
	return func(map[string]any) (any, error) { return result, nil }
}

// Plain JSON form of a decoded value for comparisons
func controlJSON(t *testing.T, v any) string {
	t.Helper()
	text, err := json.Marshal(pyjson.Plain(v))
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

// Raw request line to the unix socket of home and its decoded answer
func controlRoundTrip(t *testing.T, h string, line string) map[string]any {
	t.Helper()
	path, err := ResolveClientSocketPath(h)
	if err != nil || path == "" {
		t.Fatalf("no socket %v", err)
	}
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(reply), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Connected unix socket of a socketpair, the peer end closed with the test
func unixSocketPair(t *testing.T) net.Conn {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	ours, theirs := os.NewFile(uintptr(fds[0]), "ours"), os.NewFile(uintptr(fds[1]), "theirs")
	conn, err := net.FileConn(ours)
	ours.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); theirs.Close() })
	return conn
}

// Home whose name is long enough that its socket uses the temp dir fallback and pointer
func longControlHome(t *testing.T, n int) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), strings.Repeat("p", n), ".icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestServerAnswersIdentifyAndStatus(t *testing.T) {
	h := shortControlHome(t)
	startControlServer(t, h, map[string]ControlVerbHandler{
		"identify": fixedVerb(map[string]any{"pid": 4242, "code_sha": "abc123", "protocol": 1}),
		"status":   fixedVerb(map[string]any{"gateway_state": "running"}),
	})
	ident := QueryGatewayControl(h, "identify", nil, DefaultControlClientTimeout)
	status := QueryGatewayControl(h, "status", nil, DefaultControlClientTimeout)
	if got := controlJSON(t, ident); got != `{"code_sha":"abc123","pid":4242,"protocol":1}` {
		t.Fatalf("identify %s", got)
	}
	if got := controlJSON(t, status); got != `{"gateway_state":"running"}` {
		t.Fatalf("status %s", got)
	}
}

func TestUnknownVerbAndMalformedRequest(t *testing.T) {
	h := shortControlHome(t)
	startControlServer(t, h, map[string]ControlVerbHandler{"identify": fixedVerb(map[string]any{"pid": 1})})
	if unknown := QueryGatewayControl(h, "restart", nil, DefaultControlClientTimeout); unknown != nil {
		t.Fatalf("unknown verb answered %v", unknown)
	}
	payload := controlRoundTrip(t, h, "this is not json")
	if payload["ok"] != false || payload["protocol"] != float64(ControlProtocolVersion) {
		t.Fatalf("garbage reply %v", payload)
	}
}

func TestVerbHandlerReceivesParams(t *testing.T) {
	h := shortControlHome(t)
	received := map[string]any{}
	startControlServer(t, h, map[string]ControlVerbHandler{
		"with-params": func(params map[string]any) (any, error) {
			for k, v := range params {
				received[k] = v
			}
			return map[string]any{"echo": params}, nil
		},
		"bare": fixedVerb(map[string]any{"ok": 1}),
	})
	got := QueryGatewayControl(h, "with-params", pyjson.ObjectOf("old", "a", "new", "b"), DefaultControlClientTimeout)
	bare := QueryGatewayControl(h, "bare", nil, DefaultControlClientTimeout)
	if controlJSON(t, got) != `{"echo":{"new":"b","old":"a"}}` {
		t.Fatalf("with params %s", controlJSON(t, got))
	}
	if controlJSON(t, received) != `{"new":"b","old":"a"}` {
		t.Fatalf("received %v", received)
	}
	if controlJSON(t, bare) != `{"ok":1}` {
		t.Fatalf("bare %s", controlJSON(t, bare))
	}
}

func TestStopRemovesSocketAndPointer(t *testing.T) {
	h := shortControlHome(t)
	server, err := NewGatewayControlServer(h, map[string]ControlVerbHandler{"identify": fixedVerb(map[string]any{"pid": 1})})
	if err != nil || !server.Start() {
		t.Fatalf("start %v", err)
	}
	bind, _, _ := ResolveServerSocketPath(h)
	if _, err := os.Stat(bind); err != nil {
		t.Fatalf("socket missing %v", err)
	}
	server.Stop()
	if _, err := os.Stat(bind); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind %v", err)
	}
	if path, _ := ResolveClientSocketPath(h); path != "" {
		t.Fatalf("client still resolves %s", path)
	}
	if QueryGatewayControl(h, "identify", nil, DefaultControlClientTimeout) != nil {
		t.Fatal("query after stop answered")
	}
}

func TestStaleSocketFileIsReplacedOnBind(t *testing.T) {
	h := shortControlHome(t)
	bind, _, _ := ResolveServerSocketPath(h)
	if err := os.MkdirAll(filepath.Dir(bind), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bind, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	startControlServer(t, h, map[string]ControlVerbHandler{"identify": fixedVerb(map[string]any{"pid": 7})})
	if got := controlJSON(t, IdentifyGateway(h, DefaultControlClientTimeout)); got != `{"pid":7}` {
		t.Fatalf("identify %s", got)
	}
}

// Listener seam refusing every path under refuseUnder with err, recording each attempt
func refusingListenUnix(t *testing.T, refuseUnder string, err syscall.Errno) *[]string {
	t.Helper()
	var attempts []string
	real := controlListenUnix
	controlListenUnix = func(path string) (net.Listener, error) {
		attempts = append(attempts, path)
		if rel, relErr := filepath.Rel(refuseUnder, path); relErr == nil && !strings.HasPrefix(rel, "..") {
			return nil, &net.OpError{Op: "listen", Net: "unix", Err: os.NewSyscallError("bind", err)}
		}
		return real(path)
	}
	t.Cleanup(func() { controlListenUnix = real })
	return &attempts
}

func TestHomeRefusingSocketsListensOnPrivateFallbackBothClientsReach(t *testing.T) {
	h, err := filepath.EvalSymlinks(shortControlHome(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(h, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(h, "gateway.sock")
	attempts := refusingListenUnix(t, h, syscall.EOPNOTSUPP)
	realLstat, realStat := controlLstat, controlStat
	controlLstat = func(path string) (os.FileInfo, error) {
		if path == stale {
			return nil, &os.PathError{Op: "lstat", Path: stale, Err: syscall.EOPNOTSUPP}
		}
		return realLstat(path)
	}
	controlStat = func(path string) (os.FileInfo, error) {
		if path == stale {
			return nil, &os.PathError{Op: "stat", Path: stale, Err: syscall.EOPNOTSUPP}
		}
		return realStat(path)
	}
	t.Cleanup(func() { controlLstat, controlStat = realLstat, realStat })
	server, err := NewGatewayControlServer(h, map[string]ControlVerbHandler{"identify": fixedVerb(map[string]any{"pid": 11})})
	if err != nil || !server.Start() {
		t.Fatalf("start %v", err)
	}
	pointer := filepath.Join(h, "gateway.sock.path")
	raw, err := os.ReadFile(pointer)
	if err != nil {
		server.Stop()
		t.Fatal(err)
	}
	target := string(raw)
	pointerInfo, _ := os.Lstat(pointer)
	dirInfo, _ := os.Lstat(filepath.Dir(target))
	discovered, discErr := QueryIdentify(h, 3*time.Second)
	legacy := IdentifyGateway(h, DefaultControlClientTimeout)
	server.Stop()
	if server.BindPath() != target || filepath.Base(target) != "control.sock" {
		t.Fatalf("bound %s pointer %s", server.BindPath(), target)
	}
	if pointerInfo.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("modes %v %v", pointerInfo.Mode(), dirInfo.Mode())
	}
	if discErr != nil || controlJSON(t, discovered) != `{"pid":11}` || controlJSON(t, legacy) != `{"pid":11}` {
		t.Fatalf("clients %v %v %v", discovered, discErr, legacy)
	}
	if len(*attempts) == 0 || !strings.HasSuffix((*attempts)[len(*attempts)-1], "/control.sock") {
		t.Fatalf("attempts %v", *attempts)
	}
	if _, err := os.Stat(pointer); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pointer left behind")
	}
}

func TestBindRefusedForOtherReasonsStillFails(t *testing.T) {
	h, err := filepath.EvalSymlinks(shortControlHome(t))
	if err != nil {
		t.Fatal(err)
	}
	attempts := refusingListenUnix(t, h, syscall.EADDRINUSE)
	server, err := NewGatewayControlServer(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if server.Start() {
		server.Stop()
		t.Fatal("a refused bind started")
	}
	if strings.Join(*attempts, ",") != filepath.Join(h, "gateway.sock") {
		t.Fatalf("attempts %v", *attempts)
	}
	if _, err := os.Stat(filepath.Join(h, "gateway.sock.path")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pointer written")
	}
}

func TestLongHomeEndToEndViaPointer(t *testing.T) {
	deep := longControlHome(t, 120)
	server, err := NewGatewayControlServer(deep, map[string]ControlVerbHandler{"identify": fixedVerb(map[string]any{"pid": 9})})
	if err != nil || !server.Start() {
		t.Fatalf("start %v", err)
	}
	info, err := os.Stat(filepath.Join(deep, "gateway.sock.path"))
	got := IdentifyGateway(deep, DefaultControlClientTimeout)
	server.Stop()
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("pointer %v", err)
	}
	if controlJSON(t, got) != `{"pid":9}` {
		t.Fatalf("identify %v", got)
	}
	if _, err := os.Stat(filepath.Join(deep, "gateway.sock.path")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pointer left behind")
	}
}

func TestDefaultIdentifyPayloadShape(t *testing.T) {
	h := shortControlHome(t)
	t.Setenv("ICEBATH_HOME", h)
	startControlServer(t, h, nil)
	ident := IdentifyGateway(h, DefaultControlClientTimeout)
	if ident == nil {
		t.Fatal("no identify answer")
	}
	if !pyEq(recGet(ident, "protocol"), int64(ControlProtocolVersion)) || !pyEq(recGet(ident, "pid"), int64(os.Getpid())) {
		t.Fatalf("identify %v", ident)
	}
	for _, key := range []string{"hermes_home", "supervisor", "kind", "start_time"} {
		if _, ok := ident.Get(key); !ok {
			t.Fatalf("missing %s in %v", key, ident.Keys)
		}
	}
	switch recGet(ident, "supervisor") {
	case "systemd", "launchd", "desktop", "external", "manual":
	default:
		t.Fatalf("supervisor %v", recGet(ident, "supervisor"))
	}
}

func TestPauseVerbDispatchesAndReturnsAck(t *testing.T) {
	var calls []int
	server, err := NewGatewayControlServer(t.TempDir(), map[string]ControlVerbHandler{"pause-for-update": func(map[string]any) (any, error) {
		calls = append(calls, 1)
		return map[string]any{"pausing": true, "already_stopping": false, "pid": 111, "drain_timeout": 30.0}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(server.HandleRequestLine([]byte(`{"verb": "pause-for-update", "id": 7}`), ""), &response); err != nil {
		t.Fatal(err)
	}
	result, _ := response["result"].(map[string]any)
	if response["ok"] != true || result["pausing"] != true || result["drain_timeout"] != 30.0 || response["id"] != float64(7) || len(calls) != 1 {
		t.Fatalf("response %v calls %v", response, calls)
	}
}

func TestUnknownVerbStillListsPause(t *testing.T) {
	server, err := NewGatewayControlServer(t.TempDir(), map[string]ControlVerbHandler{"pause-for-update": fixedVerb(map[string]any{})})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(server.HandleRequestLine([]byte(`{"verb": "nope"}`), ""), &response); err != nil {
		t.Fatal(err)
	}
	if response["ok"] != false || !strings.Contains(fmt.Sprint(response["supported_verbs"]), "pause-for-update") {
		t.Fatalf("response %v", response)
	}
}

func TestPauseClientRoundtripOverRealSocket(t *testing.T) {
	h := shortControlHome(t)
	var acks []int
	startControlServer(t, h, map[string]ControlVerbHandler{"pause-for-update": func(map[string]any) (any, error) {
		acks = append(acks, 1)
		return map[string]any{"pausing": true, "already_stopping": false, "pid": 4242, "drain_timeout": 12.5}, nil
	}})
	result := PauseGatewayForUpdate(h, 5*time.Second)
	if len(acks) != 1 || result == nil || recGet(result, "pausing") != true || !pyEq(recGet(result, "drain_timeout"), 12.5) {
		t.Fatalf("acks %v result %v", acks, result)
	}
}

func TestPauseClientNoneWhenGatewayLacksVerb(t *testing.T) {
	h := shortControlHome(t)
	startControlServer(t, h, nil)
	if result := PauseGatewayForUpdate(h, 5*time.Second); result != nil {
		t.Fatalf("result %v", result)
	}
}

func TestPauseClientNoneWhenNoSocket(t *testing.T) {
	if result := PauseGatewayForUpdate(t.TempDir(), 500*time.Millisecond); result != nil {
		t.Fatalf("result %v", result)
	}
}

func TestSymlinkedHomeStillAuthenticatesSameUidPeers(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	server, err := NewGatewayControlServer(filepath.Join(root, "link"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := server.posixPeerSubject(unixSocketPair(t)); got != fmt.Sprintf("uid:%d", os.Getuid()) {
		t.Fatalf("subject %q", got)
	}
}

func TestRestartHealsAWorldReadablePointerLeftByAnOlderGateway(t *testing.T) {
	h := longControlHome(t, 90)
	pointer := filepath.Join(h, "gateway.sock.path")
	if err := os.WriteFile(pointer, []byte("/old/control.sock"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chmod(pointer, 0o644)
	startControlServer(t, h, nil)
	path, err := controlSocketPath(h)
	if err != nil || filepath.Base(path) != "control.sock" {
		t.Fatalf("discovered %s %v", path, err)
	}
}

func TestAClientPollingDuringPointerPublicationNeverSeesAnInvalidPointer(t *testing.T) {
	h := longControlHome(t, 90)
	poll := func() string {
		path, err := controlSocketPath(h)
		var de *DiscoveryError
		switch {
		case errors.Is(err, os.ErrNotExist):
			return "absent"
		case errors.As(err, &de):
			return de.Reason
		case err != nil:
			return err.Error()
		}
		return filepath.Base(path)
	}
	seen := []string{poll()}
	var mu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			got := poll()
			mu.Lock()
			seen = append(seen, got)
			mu.Unlock()
		}
	}()
	server, err := NewGatewayControlServer(h, nil)
	started := err == nil && server.Start()
	close(stop)
	<-done
	if started {
		server.Stop()
	}
	if !started {
		t.Fatalf("start %v", err)
	}
	absent := false
	for _, s := range seen {
		if s != "absent" && s != "control.sock" {
			t.Fatalf("a poll saw %q", s)
		}
		absent = absent || s == "absent"
	}
	if !absent {
		t.Fatalf("seen %v", seen)
	}
}

func TestGroupBitsThatAreAnACLMaskNeverCountAsAPrivateGroup(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(h, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(h, 0o770)
	user, err := pypwd.Getpwuid(os.Getuid())
	if err != nil {
		t.Skip("no passwd entry for this uid")
	}
	realPwall, realGrgid, realXattr := discoveryGetpwall, discoveryGetgrgid, aclListxattr
	discoveryGetpwall = func() ([]pypwd.Passwd, error) { return []pypwd.Passwd{user}, nil }
	discoveryGetgrgid = func(int) (pypwd.Group, error) { return pypwd.Group{Name: user.Name, Passwd: "x", GID: user.GID}, nil }
	aclListxattr = func(path string, dest []byte) (int, error) {
		names := "system.posix_acl_access\x00"
		if dest == nil {
			return len(names), nil
		}
		return copy(dest, names), nil
	}
	t.Cleanup(func() { discoveryGetpwall, discoveryGetgrgid, aclListxattr = realPwall, realGrgid, realXattr })
	var de *DiscoveryError
	if _, err := controlSocketPath(h); !errors.As(err, &de) || de.Reason != "unsafe_control_permissions" {
		t.Fatalf("discovery %v", err)
	}
	server, err := NewGatewayControlServer(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := server.posixPeerSubject(unixSocketPair(t)); got != "" {
		t.Fatalf("subject %q", got)
	}
}

func TestClientsRefuseAControlListenerRunByAnotherUid(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(h, 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var answered []string
	startControlServer(t, h, map[string]ControlVerbHandler{"identify": func(map[string]any) (any, error) {
		mu.Lock()
		answered = append(answered, "identify")
		mu.Unlock()
		return map[string]any{"pid": 1}, nil
	}})
	path, err := controlSocketPath(h)
	if err != nil {
		t.Fatal(err)
	}
	realUID := os.Getuid()
	discoveryGetuid = func() int { return realUID + 1 }
	discoverySocketPath = func(string) (string, error) { return path, nil }
	t.Cleanup(func() { discoveryGetuid, discoverySocketPath = os.Getuid, controlSocketPath })
	var de *DiscoveryError
	if _, err := QueryIdentify(h, 2*time.Second); !errors.As(err, &de) || de.Reason != "unsafe_control_peer" {
		t.Fatalf("identify %v", err)
	}
	endpoint := &GatewayEndpoint{ProfileID: "default", InstanceID: "owner"}
	if _, err := sessionTicket(h, endpoint, "interactive", "", "", false); !errors.As(err, &de) || de.Reason != "unsafe_control_peer" {
		t.Fatalf("session ticket %v", err)
	}
	var unreachable *gatewayUnreachable
	if _, err := webappControl(h, "identify", nil); !errors.As(err, &unreachable) || !strings.Contains(err.Error(), "unsafe_control_peer") {
		t.Fatalf("webapp control %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(answered) != 0 {
		t.Fatalf("a refused client reached the listener %v", answered)
	}
}

func TestLongHomeSurvivesASquattedSharedFallbackDirectory(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	h := longControlHome(t, 90)
	bind, _, err := ResolveServerSocketPath(h)
	if err != nil {
		t.Fatal(err)
	}
	squatted := filepath.Dir(bind)
	if err := os.WriteFile(squatted, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(squatted) })
	startControlServer(t, h, map[string]ControlVerbHandler{"identify": fixedVerb(map[string]any{"pid": 1})})
	path, err := controlSocketPath(h)
	if err != nil || filepath.Dir(filepath.Dir(path)) == filepath.Dir(squatted) {
		t.Fatalf("socket %s %v", path, err)
	}
}

func TestLongHomePrefersThePrivateRuntimeDir(t *testing.T) {
	runtimeDir, err := os.MkdirTemp("", "xr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	h := longControlHome(t, 90)
	bind, _, _ := ResolveServerSocketPath(h)
	if filepath.Dir(filepath.Dir(bind)) != runtimeDir {
		t.Fatalf("bind %s", bind)
	}
	os.Chmod(runtimeDir, 0o755)
	bind, _, _ = ResolveServerSocketPath(h)
	if filepath.Dir(filepath.Dir(bind)) == runtimeDir {
		t.Fatalf("an open runtime dir was trusted %s", bind)
	}
}

func TestPrivateControlPeerMintsProfileBoundTicket(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(h, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(h)
	server, err := NewGatewayControlServer(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	server.TicketStore, err = NewTicketStore("instance", map[string]bool{resolved: true})
	if err != nil {
		t.Fatal(err)
	}
	if !server.Start() {
		t.Fatal("start")
	}
	t.Cleanup(server.Stop)
	request := func(params map[string]any, extra map[string]any) map[string]any {
		body := map[string]any{"protocol": 1, "verb": "session-ticket", "params": params}
		for k, v := range extra {
			body[k] = v
		}
		line, _ := json.Marshal(body)
		return controlRoundTrip(t, h, string(line))
	}
	params := map[string]any{"profile_id": resolved, "instance_id": "instance", "purpose": "interactive"}
	reply := request(params, nil)
	if reply["ok"] != true {
		t.Fatalf("reply %v", reply)
	}
	ticket := reply["result"].(map[string]any)["ticket"]
	grant, err := server.TicketStore.Redeem(ticket, &resolved, "interactive")
	if err != nil {
		t.Fatal(err)
	}
	capabilities, _ := grant["capabilities"].(map[string]bool)
	if grant["subject"] != fmt.Sprintf("uid:%d", os.Getuid()) || grant["instance_id"] != "instance" || !capabilities["session:create"] {
		t.Fatalf("grant %v", grant)
	}
	with := func(key string, value any) map[string]any {
		out := map[string]any{}
		for k, v := range params {
			out[k] = v
		}
		out[key] = value
		return out
	}
	for _, invalid := range []map[string]any{with("instance_id", "old"), with("profile_id", "/wrong"), with("subject", "admin")} {
		if request(invalid, nil)["ok"] != false {
			t.Fatalf("invalid params minted %v", invalid)
		}
	}
	if request(params, map[string]any{"origin": "http://browser"})["ok"] != false {
		t.Fatal("an extra envelope key minted")
	}
	line, _ := json.Marshal(map[string]any{"verb": "session-ticket", "params": params})
	var direct map[string]any
	json.Unmarshal(server.HandleRequestLine(line, ""), &direct)
	if direct["ok"] != false {
		t.Fatal("a direct dispatch minted without a peer")
	}
}

func TestOldSocketCleanupCannotUnlinkReplacement(t *testing.T) {
	h := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(h, 0o700); err != nil {
		t.Fatal(err)
	}
	old, err := NewGatewayControlServer(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := NewGatewayControlServer(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !old.Start() {
		t.Fatal("old start")
	}
	old.Stop()
	if !replacement.Start() {
		t.Fatal("replacement start")
	}
	t.Cleanup(replacement.Stop)
	old.CleanupFiles()
	if path, _ := ResolveClientSocketPath(h); path == "" {
		t.Fatal("the replacement socket was unlinked")
	}
	if reply := controlRoundTrip(t, h, `{"verb":"identify"}`); reply["ok"] != true {
		t.Fatalf("reply %v", reply)
	}
}
