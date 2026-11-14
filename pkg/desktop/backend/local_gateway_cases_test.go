//go:build !windows

package backend

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

func shortSocketTmpDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(real, 0o700); os.RemoveAll(real) })
	return real
}

func testEndpoint(profileID, instance string) GatewayEndpoint {
	return GatewayEndpoint{ProfileID: profileID, InstanceID: instance, AuthorityEpoch: "1", RuntimeProtocol: 1, APIOrigin: "http://127.0.0.1:1234", Capabilities: []string{"session-authority-v1"}, Supervisor: "none"}
}

func endpointMap(ep GatewayEndpoint) map[string]any {
	text, _ := json.Marshal(ep)
	var m map[string]any
	json.Unmarshal(text, &m)
	return m
}

func ensureJSON(t *testing.T, v any) string {
	t.Helper()
	text, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

// controlServer answers each control request with reply(params) on a 0600 socket at path
func controlServer(t *testing.T, path string, reply func(params map[string]any, n int) any) (*[]map[string]any, func()) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0o600)
	var mu sync.Mutex
	requests := []map[string]any{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					Params map[string]any `json:"params"`
				}
				json.Unmarshal(line, &req)
				mu.Lock()
				requests = append(requests, req.Params)
				n := len(requests)
				mu.Unlock()
				if answer := reply(req.Params, n); answer != nil {
					text, _ := json.Marshal(answer)
					c.Write(append(text, '\n'))
				}
			}(conn)
		}
	}()
	stop := func() { ln.Close(); os.Remove(path) }
	t.Cleanup(stop)
	return &requests, stop
}

func grantReply(profile, instance, ticket string) map[string]any {
	return map[string]any{"protocol": 1, "id": 1, "ok": true, "result": map[string]any{"profile_id": profile, "instance_id": instance, "runtime_protocol": 1, "ticket": ticket}}
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheEnsureClientInheritsTheCallerScrubbedParentEnvNotTheRawDesktopEnv(t *testing.T) {
	t.Setenv("LEAK", "raw desktop secret")
	script := writeScript(t, t.TempDir(), "print-env", `printf '{"leak":"%s","home":"%s","own":"%s"}' "${LEAK:-null}" "$ICEBATH_HOME" "$OWN"`)
	result, err := RunGatewayEnsure(BackendCommand{Command: script, Env: map[string]string{"OWN": "1"}}, t.TempDir(), "/home/x/.icebath", map[string]string{"PATH": os.Getenv("PATH"), "OWN": "0"}, EnsureRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != `{"leak":"null","home":"/home/x/.icebath","own":"1"}` {
		t.Fatalf("stdout %q", result.Stdout)
	}
}

func TestTheEnsureClientNeverHandsADevRendererOriginToTheLongLivedLocalGateway(t *testing.T) {
	script := writeScript(t, t.TempDir(), "print-dev", `printf '%s' "${ICEBATH_DESKTOP_DEV_SERVER:-null}"`)
	result, err := RunGatewayEnsure(BackendCommand{Command: script, Env: map[string]string{"ICEBATH_DESKTOP_DEV_SERVER": "http://127.0.0.1:5174"}}, t.TempDir(), "/home/x/.icebath",
		map[string]string{"PATH": os.Getenv("PATH"), "ICEBATH_DESKTOP_DEV_SERVER": "http://127.0.0.1:5174"}, EnsureRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "null" {
		t.Fatalf("stdout %q", result.Stdout)
	}
}

func TestAShellDelegatedEnsureQuotesASpacedWindowsInstallPath74064(t *testing.T) {
	command := `C:\Users\John Doe\AppData\Local\hermes\icebath.cmd`
	got := []string{WindowsShellCommand(command, true, true), WindowsShellCommand(command, false, true)}
	if !reflect.DeepEqual(got, []string{`"` + command + `"`, command}) {
		t.Fatalf("spawned %q", got)
	}
}

func TestCanonicalEnsureCannotCrossARejectedUpdateOrProfileLifecycleGate(t *testing.T) {
	ran := false
	_, err := EnsureLocalGateway(func() (EnsureResult, error) { ran = true; return EnsureResult{Stdout: "{}"}, nil },
		func() error { return errors.New("profile retired during update") }, nil)
	if err == nil || !strings.Contains(err.Error(), "profile retired during update") || ran {
		t.Fatalf("err %v ran %v", err, ran)
	}
}

func TestNativeHTTPRespectsSupportedHomeModeAndMintsFreshPurposeBoundGrants(t *testing.T) {
	for _, mode := range []os.FileMode{0o700, 0o750, 0o701} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			home := shortSocketTmpDir(t, "desktop-http-")
			os.Chmod(home, mode)
			endpoint := testEndpoint(home, "owner")
			requests, _ := controlServer(t, filepath.Join(home, "gateway.sock"), func(params map[string]any, n int) any {
				profile, _ := params["profile_id"].(string)
				if profile == "" {
					profile = home
				}
				return grantReply(profile, "owner", fmt.Sprintf("grant-%d", n))
			})
			headers := func(route, method string) (map[string]string, error) {
				return NativeGatewayHTTPHeaders(endpoint, endpoint.APIOrigin, endpoint.APIOrigin+route, home, method, nil)
			}
			a, err := headers("/api/config", "")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := headers("/api/config", "")
			if a[GatewayTicketHeader] != "grant-1" || b[GatewayTicketHeader] != "grant-2" {
				t.Fatalf("grants %v %v", a, b)
			}
			for _, r := range *requests {
				if !reflect.DeepEqual(r, map[string]any{"profile_id": home, "instance_id": "owner", "purpose": "native-http"}) {
					t.Fatalf("params %v", r)
				}
			}
			if _, err := NativeGatewayHTTPHeaders(endpoint, endpoint.APIOrigin, "http://127.0.0.1:4567/api/config", home, "", nil); err == nil || !strings.Contains(err.Error(), "origin") {
				t.Fatalf("origin mismatch %v", err)
			}
			if len(*requests) != 2 {
				t.Fatalf("requests %d", len(*requests))
			}
			if _, err := MintLocalGatewayTicket(endpoint, ""); err != nil {
				t.Fatal(err)
			}
			last := func() map[string]any { return (*requests)[len(*requests)-1] }
			if !reflect.DeepEqual(last(), map[string]any{"profile_id": home, "instance_id": "owner", "purpose": "interactive", "scope": "host"}) {
				t.Fatalf("interactive params %v", last())
			}
			for _, c := range [][3]string{
				{"/api/profiles", "POST", "native-profile-create"},
				{"/api/profiles?unused=1", "post", "native-profile-create"},
				{"/api/profiles", "GET", "native-http"},
				{"/api/profiles/import", "POST", "native-http"},
				{"/api/config", "POST", "native-http"},
				{"/api/profiles/default", "PATCH", "native-profile-registry"},
				{"/api/profiles/default", "delete", "native-profile-registry"},
				{"/api/profiles/default/soul", "PUT", "native-http"},
				{"/api/profiles/active", "GET", "native-http"},
				{"/api/profiles/active", "POST", "native-http"},
				{"/api/profiles/sessions?profile=default", "GET", "native-http"},
			} {
				if _, err := headers(c[0], c[1]); err != nil {
					t.Fatalf("%v: %v", c, err)
				}
				if !reflect.DeepEqual(last(), map[string]any{"profile_id": home, "instance_id": "owner", "purpose": c[2]}) {
					t.Fatalf("%v params %v", c, last())
				}
			}
			for _, c := range [][2]string{{"active", "PATCH"}, {"import", "DELETE"}, {"sessions", "DELETE"}} {
				dir := filepath.Join(home, "profiles", c[0])
				os.MkdirAll(dir, 0o700)
				if _, err := headers("/api/profiles/"+c[0], c[1]); err != nil {
					t.Fatalf("%v: %v", c, err)
				}
				real, _ := filepath.EvalSymlinks(dir)
				if !reflect.DeepEqual(last(), map[string]any{"profile_id": real, "instance_id": "owner", "purpose": "native-profile-registry"}) {
					t.Fatalf("%v params %v", c, last())
				}
			}
		})
	}
}

func TestAServedSecondaryMintsItsTicketThroughTheMultiplexerControlSocketBoundToItsOwnProfile(t *testing.T) {
	root := shortSocketTmpDir(t, "desktop-mux-")
	secondary := filepath.Join(root, "profiles", "cold")
	os.MkdirAll(secondary, 0o700)
	os.Chmod(root, 0o700)
	requests, _ := controlServer(t, filepath.Join(root, "gateway.sock"), func(map[string]any, int) any {
		return grantReply(secondary, "mux", "served-grant")
	})
	endpoint := testEndpoint(secondary, "mux")
	if _, err := MintLocalGatewayTicket(endpoint, "native-http"); err == nil || err.Error() != "Gateway ticket control socket missing" {
		t.Fatalf("own socket %v", err)
	}
	endpoint.ControlHome = &root
	ticket, err := MintLocalGatewayTicket(endpoint, "native-http")
	if err != nil || ticket != "served-grant" {
		t.Fatalf("ticket %q %v", ticket, err)
	}
	if !reflect.DeepEqual(*requests, []map[string]any{{"profile_id": secondary, "instance_id": "mux", "purpose": "native-http"}}) {
		t.Fatalf("requests %v", *requests)
	}
}

func TestEnsureConsumesStructuredReadinessWithoutAcquiringAChildOwner(t *testing.T) {
	endpoint := testEndpoint("/private/profile", "owner")
	conn, err := EnsureLocalGateway(func() (EnsureResult, error) {
		return EnsureResult{Stdout: ensureJSON(t, map[string]any{"state": "ready", "endpoint": endpoint})}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if conn["baseUrl"] != endpoint.APIOrigin || !reflect.DeepEqual(*conn.Endpoint(), endpoint) || conn["token"] != "" {
		t.Fatalf("descriptor %v", conn)
	}
	if _, ok := conn["process"]; ok {
		t.Fatal("descriptor owns a process")
	}
	_, err = EnsureLocalGateway(func() (EnsureResult, error) {
		return EnsureResult{Code: 5, Stdout: `{"state":"starting","reason_code":"deadline"}`}, nil
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "starting") {
		t.Fatalf("starting %v", err)
	}
}

func TestARestartWaitOwnerIsAttachedToAnswerAParkedPromptAPlainDrainStaysRefused139041(t *testing.T) {
	endpoint := testEndpoint("/private/profile", "owner")
	old := "old"
	endpoint.CodeSHA = &old
	calls := 0
	restart := func(map[string]any, StaleCode) (<-chan struct{}, bool) {
		calls++
		done := make(chan struct{})
		close(done)
		return done, true
	}
	wait := EnsureResult{Code: 6, Stdout: ensureJSON(t, map[string]any{"state": "draining", "reason_code": "restart_wait", "endpoint": endpoint, "client_code_sha": "new"})}
	conn, err := EnsureLocalGateway(func() (EnsureResult, error) { return wait, nil }, nil, restart)
	if err != nil || !reflect.DeepEqual(*conn.Endpoint(), endpoint) || calls != 0 {
		t.Fatalf("conn %v err %v calls %d", conn, err, calls)
	}
	_, err = EnsureLocalGateway(func() (EnsureResult, error) {
		return EnsureResult{Code: 6, Stdout: `{"state":"draining","reason_code":"update_paused","endpoint":null}`}, nil
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "Gateway draining (update_paused)") {
		t.Fatalf("drain %v", err)
	}
}

func TestAStaleOwnerThisWindowRestartsIsAttachedInItsRestartWaitBeforeTheRestartFinishes139041(t *testing.T) {
	endpoint := testEndpoint("/h", "old")
	old := "old"
	endpoint.CodeSHA = &old
	finish := make(chan struct{})
	var mu sync.Mutex
	var restarted []string
	restartStale := CreateStaleGatewayRestarter(func(owner string) <-chan struct{} {
		mu.Lock()
		restarted = append(restarted, owner)
		mu.Unlock()
		return finish
	}, func(string) {})
	answers := []EnsureResult{
		{Code: 0, Stdout: ensureJSON(t, map[string]any{"state": "ready", "endpoint": endpoint, "client_code_sha": "new"})},
		{Code: 6, Stdout: ensureJSON(t, map[string]any{"state": "draining", "reason_code": "restart_wait", "endpoint": endpoint, "client_code_sha": "new"})},
	}
	type outcome struct {
		conn Connection
		err  error
	}
	got := make(chan outcome, 1)
	go func() {
		conn, err := EnsureLocalGateway(func() (EnsureResult, error) { a := answers[0]; answers = answers[1:]; return a, nil }, nil, restartStale)
		got <- outcome{conn, err}
	}()
	select {
	case o := <-got:
		close(finish)
		if o.err != nil || o.conn.Endpoint().InstanceID != "old" {
			t.Fatalf("outcome %v %v", o.conn, o.err)
		}
	case <-time.After(500 * time.Millisecond):
		close(finish)
		t.Fatal("blocked on the restart")
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(restarted, []string{"default"}) {
		t.Fatalf("restarted %v", restarted)
	}
}

func TestAnEnsureChildThatNeverReachedTheProtocolBoundaryIsDiagnosedFromStderrNotAsAJSONParseError(t *testing.T) {
	run := func(r EnsureResult) error {
		_, err := EnsureLocalGateway(func() (EnsureResult, error) { return r, nil }, nil, nil)
		return err
	}
	legacy := EnsureResult{Code: 2, Stderr: "usage: icebath gateway [-h] ...\nhermes gateway: 'ensure' is not a `icebath gateway` command.\nRun `icebath gateway --help` to see all commands.\n"}
	if err := run(legacy); err == nil || !strings.Contains(err.Error(), "produced no result (exit 2): Run `icebath gateway --help`") {
		t.Fatalf("legacy %v", err)
	}
	if err := run(EnsureResult{Code: 1, Stderr: "Error: Profile 'gone' does not exist.\n"}); err == nil || !strings.Contains(err.Error(), "exit 1): Error: Profile 'gone' does not exist.") {
		t.Fatalf("missing profile %v", err)
	}
	if err := run(EnsureResult{Code: 3, Stdout: `{"endpoint":null,"reason_code":"runtime_protocol","state":"incompatible"}`, Stderr: "noise"}); err == nil || !strings.Contains(err.Error(), "Gateway incompatible (runtime_protocol)") {
		t.Fatalf("incompatible %v", err)
	}
	if err := run(EnsureResult{}); err == nil || !strings.Contains(err.Error(), "produced no result (exit 0). Update Icebath") {
		t.Fatalf("empty %v", err)
	}
}

func TestPrivateDialCredentialIsOneUseAndBoundToTheRequestingNativeWindow(t *testing.T) {
	dials := NewLocalGatewayDials()
	prepared, err := dials.Prepare("http://127.0.0.1:1234", "private-ticket", "7")
	if err != nil {
		t.Fatal(err)
	}
	dial, _ := url.Parse(prepared)
	if dial.Query().Get("ticket") != "private-ticket" {
		t.Fatalf("ticket missing in %s", prepared)
	}
	q := dial.Query()
	q.Del("ticket")
	dial.RawQuery = q.Encode()
	raw := dial.String()
	if strings.Contains(raw, "private-ticket") {
		t.Fatal("ticket left in url")
	}
	details := DialRequest{URL: raw, WindowID: "8", ResourceType: "webSocket", RequestHeaders: map[string]string{"Origin": "http://renderer", "Sec-WebSocket-Protocol": "icebath-gateway-v1, icebath-gateway-ticket.private-ticket"}}
	if dials.Headers(details) != nil {
		t.Fatal("another window used the dial")
	}
	proxied := details
	proxied.URL = strings.Replace(raw, "127.0.0.1:1234", "127.0.0.1:4321", 1)
	proxied.WindowID = "7"
	headers := dials.Headers(proxied)
	if headers == nil {
		t.Fatal("reserved dial refused")
	}
	if _, ok := headers["Origin"]; ok || headers["Sec-WebSocket-Protocol"] != "icebath-gateway-v1, icebath-gateway-ticket.private-ticket" {
		t.Fatalf("headers %v", headers)
	}
	details.WindowID = "7"
	if dials.Headers(details) != nil {
		t.Fatal("dial used twice")
	}
}

func TestAVoiceSocketDialedOnItsOwnAPIWsReservationDropsOriginOtherPathsDoNot(t *testing.T) {
	dial := func(path string) map[string]string {
		dials := NewLocalGatewayDials()
		prepared, _ := dials.Prepare("http://127.0.0.1:1234", "voice-ticket", "7")
		u, _ := url.Parse(prepared)
		q := u.Query()
		q.Del("ticket")
		u.RawQuery = q.Encode()
		u.Path = path
		return dials.Headers(DialRequest{URL: u.String(), WindowID: "7", ResourceType: "webSocket", RequestHeaders: map[string]string{"Origin": "http://renderer"}})
	}
	for _, p := range []string{"/api/audio/speak-stream", "/api/audio/transcribe-stream"} {
		if got := dial(p); got == nil || len(got) != 0 {
			t.Fatalf("%s headers %v", p, got)
		}
	}
	if dial("/api/pty") != nil {
		t.Fatal("pty path accepted")
	}
}

func TestADialAgainstAReplacedLocalGatewayForgetsTheCachedEndpointOnceAndReEnsures(t *testing.T) {
	endpoints := []string{"dead", "alive"}
	var forgotten []string
	ensures := 0
	result, err := RedialLocalGateway(func() (string, error) {
		e := endpoints[min(ensures, 1)]
		ensures++
		return e, nil
	}, func() error { forgotten = append(forgotten, "primary"); return nil }, func(e string) (string, error) {
		if e == "dead" {
			return "", errors.New("Gateway ticket bootstrap failed")
		}
		return "ticket-for-" + e, nil
	})
	if err != nil || result != "ticket-for-alive" || !reflect.DeepEqual(forgotten, []string{"primary"}) || ensures != 2 {
		t.Fatalf("%q %v %v %d", result, err, forgotten, ensures)
	}
}

func TestARedialWhoseReEnsureComesBackWithoutAGatewayEndpointNamesThatNotATypeError(t *testing.T) {
	endpoint := testEndpoint("/h", "dead")
	mints := 0
	answers := []Connection{{"baseUrl": "http://127.0.0.1:1234", "gatewayEndpoint": endpoint}, {"baseUrl": "http://127.0.0.1:1234"}}
	_, err := LocalGatewayDial(LocalDial{
		Ensure:     func() (Connection, error) { a := answers[0]; answers = answers[1:]; return a, nil },
		Forget:     func() error { return nil },
		LaunchHome: "/h",
		Mint: func(GatewayEndpoint, string) (string, error) {
			mints++
			return "", errors.New("Gateway ticket control socket missing")
		},
		Prepare: func(string, string) (string, error) { return "unused", nil },
	})
	if err == nil || err.Error() != "Local gateway connection has no gateway endpoint" || mints != 1 {
		t.Fatalf("err %v mints %d", err, mints)
	}
}

func TestAReadyOwnerRunningOtherCodeThanThisIcebathIsRestartedOnceThenReEnsured(t *testing.T) {
	base := testEndpoint("/h/profiles/w", "old")
	root := "/h"
	base.ControlHome = &root
	with := func(instance, sha string) GatewayEndpoint {
		e := base
		e.InstanceID = instance
		e.CodeSHA = &sha
		return e
	}
	answers := []GatewayEndpoint{with("old", "old"), with("new", "new"), with("old", "old"), with("old", "old")}
	var restarted []string
	restartStale := CreateStaleGatewayRestarter(func(owner string) <-chan struct{} {
		restarted = append(restarted, owner)
		done := make(chan struct{})
		close(done)
		return done
	}, func(string) {})
	ensure := func() Connection {
		conn, err := ensureLocalGateway(func() (EnsureResult, error) {
			a := answers[0]
			answers = answers[1:]
			return EnsureResult{Stdout: ensureJSON(t, map[string]any{"state": "ready", "endpoint": a, "client_code_sha": "new"})}, nil
		}, nil, restartStale, 10*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	if got := ensure().Endpoint().InstanceID; got != "new" {
		t.Fatalf("first %s", got)
	}
	if !reflect.DeepEqual(restarted, []string{"default"}) {
		t.Fatalf("restarted %v", restarted)
	}
	if got := ensure().Endpoint().InstanceID; got != "old" {
		t.Fatalf("second %s", got)
	}
	if !reflect.DeepEqual(restarted, []string{"default"}) {
		t.Fatalf("restarted again %v", restarted)
	}
}

func TestADialThatKeepsFailingAfterOneReEnsureSurfacesTheErrorInsteadOfLooping(t *testing.T) {
	ensures, forgets := 0, 0
	_, err := RedialLocalGateway(func() (string, error) { ensures++; return "still-dead", nil },
		func() error { forgets++; return nil },
		func(string) (string, error) { return "", errors.New("Invalid gateway ticket response") })
	if err == nil || err.Error() != "Invalid gateway ticket response" || ensures != 2 || forgets != 1 {
		t.Fatalf("%v %d %d", err, ensures, forgets)
	}
}

func TestAStoppedGatewayThatUnlinkedItsControlSocketIsAStaleOwnerNotARawFilesystemError(t *testing.T) {
	home := shortSocketTmpDir(t, "desktop-redial-")
	socketPath := filepath.Join(home, "gateway.sock")
	endpoint := testEndpoint(home, "owner")
	serve := func(ticket string) func() {
		_, stop := controlServer(t, socketPath, func(map[string]any, int) any { return grantReply(home, "owner", ticket) })
		return stop
	}
	stop := serve("grant-original")
	if got, err := MintLocalGatewayTicket(endpoint, ""); err != nil || got != "grant-original" {
		t.Fatalf("%q %v", got, err)
	}
	stop()
	ensures, forgets := 0, 0
	result, err := RedialLocalGateway(func() (GatewayEndpoint, error) {
		ensures++
		if ensures == 2 {
			serve("grant-replacement")
		}
		return endpoint, nil
	}, func() error { forgets++; return nil }, func(e GatewayEndpoint) (string, error) { return MintLocalGatewayTicket(e, "") })
	if err != nil || result != "grant-replacement" || forgets != 1 || ensures != 2 {
		t.Fatalf("%q %v %d %d", result, err, forgets, ensures)
	}
}

func TestACorruptControlPointerIsAStaleOwnerTheRedialReEnsuresAndAttachesToTheRepublishedSocket(t *testing.T) {
	home := shortSocketTmpDir(t, "desktop-pointer-")
	runtimeDir := shortSocketTmpDir(t, "desktop-gw-")
	sum := sha256.Sum256([]byte(home))
	socketPath := filepath.Join(runtimeDir, "icebath-gw-"+hex.EncodeToString(sum[:])[:16], "control.sock")
	pointer := filepath.Join(home, "gateway.sock.path")
	endpoint := testEndpoint(home, "owner")
	os.Mkdir(filepath.Dir(socketPath), 0o700)
	controlServer(t, socketPath, func(map[string]any, int) any {
		return map[string]any{"protocol": 1, "id": 1, "ok": true, "result": map[string]any{"profile_id": home, "instance_id": "owner", "ticket": "grant-republished"}}
	})
	os.WriteFile(pointer, nil, 0o600)
	_, failure := MintLocalGatewayTicket(endpoint, "")
	if failure == nil || failure.Error() != "Invalid gateway control pointer" || !IsStaleLocalGatewayError(failure) {
		t.Fatalf("failure %v", failure)
	}
	if !IsStaleLocalGatewayError(errors.New("Noncanonical gateway profile")) {
		t.Fatal("noncanonical not stale")
	}
	ensures, forgets := 0, 0
	ticket, err := RedialLocalGateway(func() (GatewayEndpoint, error) {
		ensures++
		if ensures == 2 {
			os.WriteFile(pointer, []byte(socketPath), 0o600)
		}
		return endpoint, nil
	}, func() error { forgets++; return nil }, func(e GatewayEndpoint) (string, error) { return MintLocalGatewayTicket(e, "") })
	if err != nil || ticket != "grant-republished" || ensures != 2 || forgets != 1 {
		t.Fatalf("%q %v %d %d", ticket, err, ensures, forgets)
	}
}

func TestAGroupAccessibleControlSocketIsRefusedAsUnsafeAndUnrelatedErrorsAreNeverStale(t *testing.T) {
	home := shortSocketTmpDir(t, "desktop-unsafe-")
	socketPath := filepath.Join(home, "gateway.sock")
	controlServer(t, socketPath, func(map[string]any, int) any { return nil })
	os.Chmod(socketPath, 0o660)
	_, failure := MintLocalGatewayTicket(testEndpoint(home, "owner"), "")
	want := fmt.Sprintf("Unsafe gateway control path: '%s' is accessible to other users (mode 660): run chmod go-rwx '%s'", socketPath, socketPath)
	if failure == nil || failure.Error() != want || IsStaleLocalGatewayError(failure) {
		t.Fatalf("failure %v", failure)
	}
	if IsStaleLocalGatewayError(errors.New("EACCES: permission denied")) {
		t.Fatal("unrelated error stale")
	}
}

func TestASupportedHomeModeMintsItsTicketAHomeOthersCanWriteIsRefused(t *testing.T) {
	home := shortSocketTmpDir(t, "desktop-home-mode-")
	controlServer(t, filepath.Join(home, "gateway.sock"), func(map[string]any, int) any {
		return map[string]any{"protocol": 1, "id": 1, "ok": true, "result": map[string]any{"profile_id": home, "instance_id": "owner", "ticket": "grant"}}
	})
	endpoint := testEndpoint(home, "owner")
	for _, mode := range []os.FileMode{0o700, 0o755, 0o750, 0o701} {
		os.Chmod(home, mode)
		if got, err := MintLocalGatewayTicket(endpoint, ""); err != nil || got != "grant" {
			t.Fatalf("mode %o %q %v", mode, got, err)
		}
	}
	for _, mode := range []os.FileMode{0o757, 0o703, 0o775} {
		os.Chmod(home, mode)
		if _, err := MintLocalGatewayTicket(endpoint, ""); err == nil || !strings.Contains(err.Error(), "Unsafe gateway control path") {
			t.Fatalf("mode %o %v", mode, err)
		}
	}
}

func TestAQueryProfileRequestOnTheSharedHostDescriptorMintsForTheSiblingProfileHome(t *testing.T) {
	home, _ := filepath.Abs("hermes-home")
	endpoint := GatewayEndpoint{ProfileID: home, InstanceID: "i", AuthorityEpoch: "1", RuntimeProtocol: 1, APIOrigin: "http://127.0.0.1:1", Capabilities: []string{}, Supervisor: "none"}
	routed, err := RoutedGatewayEndpoint(endpoint, "http://127.0.0.1:1/api/sessions?profile=p2", home, "")
	if err != nil || routed.ProfileID != filepath.Join(home, "profiles", "p2") || *routed.ControlHome != home {
		t.Fatalf("p2 %+v %v", routed, err)
	}
	if same, _ := RoutedGatewayEndpoint(endpoint, "http://127.0.0.1:1/api/sessions?profile=default", home, ""); !reflect.DeepEqual(same, endpoint) {
		t.Fatalf("default %+v", same)
	}
	sibling := endpoint
	sibling.ProfileID = filepath.Join(home, "profiles", "p2")
	sibling.ControlHome = &home
	if back, _ := RoutedGatewayEndpoint(sibling, "http://127.0.0.1:1/x?profile=default", home, ""); back.ProfileID != home {
		t.Fatalf("back %+v", back)
	}
	if _, err := RoutedGatewayEndpoint(endpoint, "http://127.0.0.1:1/x?profile=../evil", home, ""); err == nil || err.Error() != "Invalid profile route" {
		t.Fatalf("evil %v", err)
	}
}

func TestAProfileSOULRouteMintsForTheProfileNamedInItsPath(t *testing.T) {
	home, _ := filepath.Abs("hermes-home")
	endpoint := GatewayEndpoint{ProfileID: home, InstanceID: "i", RuntimeProtocol: 1, APIOrigin: "http://127.0.0.1:1", Supervisor: "none"}
	routed, err := RoutedGatewayEndpoint(endpoint, "http://127.0.0.1:1/api/profiles/p2/soul", home, "")
	if err != nil || routed.ProfileID != filepath.Join(home, "profiles", "p2") || *routed.ControlHome != home {
		t.Fatalf("%+v %v", routed, err)
	}
}

func TestEveryNamedProfilesScreenRouteMintsForItsProfileCollectionRoutesDoNot(t *testing.T) {
	home, _ := filepath.Abs("hermes-home")
	endpoint := GatewayEndpoint{ProfileID: home, InstanceID: "i", RuntimeProtocol: 1, APIOrigin: "http://127.0.0.1:1", Supervisor: "none"}
	route := func(p string) string {
		r, err := RoutedGatewayEndpoint(endpoint, "http://127.0.0.1:1"+p, home, "")
		if err != nil {
			t.Fatal(err)
		}
		return r.ProfileID
	}
	for _, p := range []string{"/api/profiles/p2", "/api/profiles/p2/soul", "/api/profiles/p2/setup-command", "/api/profiles/p2/export"} {
		if route(p) != filepath.Join(home, "profiles", "p2") {
			t.Fatalf("%s %s", p, route(p))
		}
	}
	if route("/api/profiles/default/soul") != home {
		t.Fatal("default soul")
	}
	for _, p := range []string{"/api/profiles/active", "/api/profiles/import", "/api/profiles/projects/tree", "/api/profiles/sessions"} {
		if route(p) != home {
			t.Fatalf("%s %s", p, route(p))
		}
	}
	if route("/api/profiles/sessions?profile=p2") != filepath.Join(home, "profiles", "p2") {
		t.Fatal("sessions profile query")
	}
}

func TestAProfileNamedLikeACollectionRouteStillGetsItsOwnTicket(t *testing.T) {
	home, _ := filepath.Abs("hermes-home")
	endpoint := GatewayEndpoint{ProfileID: home, InstanceID: "i", RuntimeProtocol: 1, APIOrigin: "http://127.0.0.1:1", Supervisor: "none"}
	route := func(p string) string {
		r, _ := RoutedGatewayEndpoint(endpoint, "http://127.0.0.1:1"+p, home, "")
		return r.ProfileID
	}
	for _, name := range []string{"projects", "sessions", "active", "import"} {
		if route("/api/profiles/"+name+"/soul") != filepath.Join(home, "profiles", name) {
			t.Fatalf("%s", name)
		}
	}
	if route("/api/profiles/sessions/sidebar?recents_profile=p2") != filepath.Join(home, "profiles", "p2") ||
		route("/api/profiles/sessions/pull-requests?profile=p2") != filepath.Join(home, "profiles", "p2") {
		t.Fatal("collection queries")
	}
}

func TestASidebarReadMintsForTheProfileItsRecentsProfileNames(t *testing.T) {
	home, _ := filepath.Abs("hermes-home")
	endpoint := GatewayEndpoint{ProfileID: filepath.Join(home, "profiles", "p2"), InstanceID: "i", RuntimeProtocol: 1, APIOrigin: "http://127.0.0.1:1", Supervisor: "none", ControlHome: &home}
	if r, _ := RoutedGatewayEndpoint(endpoint, "http://127.0.0.1:1/api/profiles/sessions/sidebar?recents_profile=default&recents_limit=40", home, ""); r.ProfileID != home {
		t.Fatalf("default %+v", r)
	}
	if r, _ := RoutedGatewayEndpoint(endpoint, "http://127.0.0.1:1/api/profiles/sessions/sidebar?recents_profile=p2", home, ""); !reflect.DeepEqual(r, endpoint) {
		t.Fatalf("own %+v", r)
	}
	if r, _ := RoutedGatewayEndpoint(endpoint, "http://127.0.0.1:1/api/profiles/sessions/sidebar?profile=default&recents_profile=p2", home, ""); !reflect.DeepEqual(r, endpoint) {
		t.Fatalf("recents wins %+v", r)
	}
}

func TestAGroupWritableHomeMintsThroughThePythonPolicyASharedGroupNamesTheFix(t *testing.T) {
	home := shortSocketTmpDir(t, "desktop-group-home-")
	endpoint := testEndpoint(home, "owner")
	var calls []string
	verdict := "grant"
	t.Cleanup(func() { ConfigureGatewayTicketClient(nil) })
	ConfigureGatewayTicketClient(func(ep GatewayEndpoint, purpose string) (string, error) {
		calls = append(calls, ep.ProfileID+":"+purpose)
		if verdict == "shared" {
			return "", ticketRefusal{"unsafe_control_permissions"}
		}
		return "python-grant", nil
	})
	os.Chmod(home, 0o775)
	if got, err := MintLocalGatewayTicket(endpoint, "native-http"); err != nil || got != "python-grant" || !reflect.DeepEqual(calls, []string{home + ":native-http"}) {
		t.Fatalf("%q %v %v", got, err, calls)
	}
	verdict = "shared"
	_, failure := MintLocalGatewayTicket(endpoint, "")
	if failure == nil || failure.Error() != fmt.Sprintf("Profile home is writable by a group other accounts share: run chmod g-w '%s'", home) || IsStaleLocalGatewayError(failure) {
		t.Fatalf("shared %v", failure)
	}
	os.Chmod(home, 0o757)
	if _, err := MintLocalGatewayTicket(endpoint, ""); err == nil || !strings.Contains(err.Error(), "Unsafe gateway control path") || len(calls) != 2 {
		t.Fatalf("other writable %v %v", err, calls)
	}
}

func TestASessionWriteThatNamesItsOwnerOnlyInTheBodyGetsAGrantForThatProfile(t *testing.T) {
	u := "http://127.0.0.1:1/api/sessions/s1"
	checks := []struct {
		got, want string
	}{
		{NativeGrantRoute(u, map[string]any{"hidden": true, "profile": "p2"}, ""), "p2"},
		{NativeGrantRoute(u+"?profile=p3", map[string]any{"profile": "p2"}, ""), u + "?profile=p3"},
		{NativeGrantRoute("http://127.0.0.1:1/api/profiles/active", map[string]any{"profile": "p2"}, "PATCH"), "http://127.0.0.1:1/api/profiles/active"},
		{NativeGrantRoute("http://127.0.0.1:1/api/profiles/active", map[string]any{"profile": "p2"}, "GET"), "p2"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Fatalf("got %q want %q", c.got, c.want)
		}
	}
	if !strings.Contains(NativeGrantRoute("http://127.0.0.1:1/api/profiles/sessions/sidebar?recents_profile=p3", map[string]any{"profile": "p2"}, ""), "recents_profile=p3") {
		t.Fatal("sidebar")
	}
	for _, body := range []any{nil, map[string]any{}, map[string]any{"profile": ""}, map[string]any{"profile": 7}, []any{map[string]any{"profile": "p2"}}} {
		if NativeGrantRoute(u, body, "") != u {
			t.Fatalf("body %v", body)
		}
	}
}

func TestAnEnsureRefusalNamesItsRemedyInsteadOfOnlyAStatusPointer(t *testing.T) {
	detail := "another Icebath install's gateway owns this profile; stop it there: icebath gateway stop"
	_, err := EnsureLocalGateway(func() (EnsureResult, error) {
		return EnsureResult{Code: 3, Stdout: ensureJSON(t, map[string]any{"state": "incompatible", "reason_code": "runtime_protocol", "detail": detail, "endpoint": nil})}, nil
	}, nil, nil)
	if err == nil || err.Error() != "Gateway incompatible (runtime_protocol). "+detail+". Use icebath gateway status for recovery." {
		t.Fatalf("%v", err)
	}
}

func TestAnElevatedOrOtherSessionGatewayIsNamedWithItsRemedyNotAsAuthorization139845140252(t *testing.T) {
	cases := [][2]string{
		{"gateway_elevated", "The running gateway (PID 23460) was started as administrator, so this non-elevated app cannot reach it. Stop it from an administrator prompt (icebath gateway stop), then start Icebath without elevation"},
		{"gateway_other_session", "The running gateway (PID 23460) runs in another Windows logon session (session 0, e.g. a boot-time scheduled task), so this app cannot reach it"},
	}
	for _, c := range cases {
		_, err := EnsureLocalGateway(func() (EnsureResult, error) {
			return EnsureResult{Code: 4, Stdout: ensureJSON(t, map[string]any{"state": "inaccessible", "reason_code": c[0], "detail": c[1], "endpoint": nil})}, nil
		}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "Gateway inaccessible ("+c[0]+"). "+c[1]+".") || strings.Contains(err.Error(), "authorization") {
			t.Fatalf("%v", err)
		}
	}
}

func TestAnUpdatePausedEnsureRefusalPointsToTheRestartThatCompletesItsTail(t *testing.T) {
	_, err := EnsureLocalGateway(func() (EnsureResult, error) {
		return EnsureResult{Code: 6, Stdout: `{"state":"draining","reason_code":"update_paused","endpoint":null}`}, nil
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "Gateway draining (update_paused). Use icebath gateway restart for recovery.") {
		t.Fatalf("%v", err)
	}
}

func TestAnEnsureClientThatIgnoresTerminationIsKilledAndItsPromiseRejectsWithinASecondDeadline(t *testing.T) {
	script := writeScript(t, t.TempDir(), "owned-client", "trap '' TERM\nwhile :; do sleep 0.05; done\n")
	start := time.Now()
	_, err := RunGatewayEnsure(BackendCommand{Command: script}, t.TempDir(), "profile", map[string]string{"PATH": os.Getenv("PATH")}, EnsureRunOptions{Timeout: 10 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("%v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second || elapsed > 3*time.Second {
		t.Fatalf("rejected after %v", elapsed)
	}
}

func TestAStaleOwnerRestartClientThatTimesOutButObeysSIGTERMIsLoggedAndTheReEnsureAttaches(t *testing.T) {
	script := writeScript(t, t.TempDir(), "owned-client", "exec sleep 5\n")
	var logs []string
	restartDone := make(chan struct{})
	cwd := t.TempDir()
	restartOwner := func(owner string) <-chan struct{} {
		go func() {
			defer close(restartDone)
			result, err := RunGatewayEnsure(BackendCommand{Command: script}, cwd, owner, map[string]string{"PATH": os.Getenv("PATH")}, EnsureRunOptions{Timeout: 10 * time.Millisecond, Label: "icebath gateway restart"})
			if err != nil {
				logs = append(logs, err.Error())
				return
			}
			reason := ""
			for _, line := range strings.Split(strings.TrimSpace(result.Stdout+"\n"+result.Stderr), "\n") {
				if line != "" {
					reason = line
				}
			}
			logs = append(logs, fmt.Sprintf("gateway restart exited %d: %s", result.Code, reason))
		}()
		return restartDone
	}
	endpoint := testEndpoint("/h", "old")
	old := "old"
	endpoint.CodeSHA = &old
	fresh := endpoint
	fresh.InstanceID = "new"
	newSha := "new"
	fresh.CodeSHA = &newSha
	answers := []GatewayEndpoint{endpoint, fresh}
	ensures := 0
	conn, err := EnsureLocalGateway(func() (EnsureResult, error) {
		ensures++
		a := answers[0]
		if len(answers) > 1 {
			answers = answers[1:]
		}
		return EnsureResult{Stdout: ensureJSON(t, map[string]any{"state": "ready", "endpoint": a, "client_code_sha": "new"})}, nil
	}, nil, CreateStaleGatewayRestarter(restartOwner, func(string) {}))
	if err != nil || conn.Endpoint().InstanceID != "new" {
		t.Fatalf("%v %v", conn, err)
	}
	<-restartDone
	if ensures != 2 || !reflect.DeepEqual(logs, []string{"gateway restart exited 7: icebath gateway restart timed out"}) {
		t.Fatalf("ensures %d logs %v", ensures, logs)
	}
}

func TestTheDefaultEnsureClientOutlivesTheProtocolDeadlineAndReadsALateVerdict(t *testing.T) {
	if gatewayEnsureProtocolDeadline+gatewayEnsureClientGrace <= 60*time.Second {
		t.Fatal("default client deadline does not outlive the protocol deadline")
	}
	script := writeScript(t, t.TempDir(), "owned-client", `sleep 0.2; printf '%s' '{"state":"starting","reason_code":"deadline"}'; exit 5`)
	result, err := RunGatewayEnsure(BackendCommand{Command: script}, t.TempDir(), "profile", map[string]string{"PATH": os.Getenv("PATH")}, EnsureRunOptions{})
	if err != nil || !reflect.DeepEqual(result, EnsureResult{Code: 5, Stdout: `{"state":"starting","reason_code":"deadline"}`}) {
		t.Fatalf("%+v %v", result, err)
	}
}

func fakeIcebath(t *testing.T, old bool) (func([]string) (EnsureResult, error), func() []string) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	reject := ""
	if old {
		reject = `case " $* " in *" --detach "*) echo "icebath: error: unrecognized arguments: --detach" >&2; exit 2;; esac` + "\n"
	}
	bin := writeScript(t, dir, "icebath", fmt.Sprintf("echo \"$*\" >> %q\n%secho \"restarted\"\n", calls, reject))
	run := func(args []string) (EnsureResult, error) {
		return RunGatewayEnsure(BackendCommand{Command: bin, Args: args}, dir, dir, map[string]string{"PATH": os.Getenv("PATH")}, EnsureRunOptions{Timeout: 5 * time.Second, Label: "icebath gateway restart"})
	}
	read := func() []string {
		raw, _ := os.ReadFile(calls)
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	return run, read
}

func TestAStaleOwnerRestartAsksForADetachedReplacement(t *testing.T) {
	run, calls := fakeIcebath(t, false)
	result, err := RunGatewayOwnerRestart(run, "work")
	if err != nil || result.Code != 0 || !reflect.DeepEqual(calls(), []string{"--profile work gateway restart --detach"}) {
		t.Fatalf("%+v %v %v", result, err, calls())
	}
}

func TestARuntimeThatPredatesDetachStillRestartsTheOwner(t *testing.T) {
	run, calls := fakeIcebath(t, true)
	result, err := RunGatewayOwnerRestart(run, "default")
	if err != nil || result.Code != 0 || !reflect.DeepEqual(calls(), []string{"--profile default gateway restart --detach", "--profile default gateway restart"}) {
		t.Fatalf("%+v %v %v", result, err, calls())
	}
}

func TestASecondOwnerOnTheSameStaleCodeIsRestartedWhileAServedSiblingSharesItsMultiplexerRestart(t *testing.T) {
	var mu sync.Mutex
	var restarted []string
	restart := CreateStaleGatewayRestarter(func(owner string) <-chan struct{} {
		mu.Lock()
		restarted = append(restarted, owner)
		mu.Unlock()
		done := make(chan struct{})
		close(done)
		return done
	}, func(string) {})
	stale := StaleCode{Gateway: "old", Client: "new"}
	owner := func(profile string, control any) map[string]any {
		return map[string]any{"profile_id": profile, "control_home": control, "instance_id": "i"}
	}
	settle := func(done <-chan struct{}) { <-done }
	first, ok1 := restart(owner("/h", nil), stale)
	settle(first)
	second, ok2 := restart(owner("/h/profiles/work", nil), stale)
	settle(second)
	_, ok3 := restart(owner("/h/profiles/ops", "/h"), stale)
	mu.Lock()
	defer mu.Unlock()
	if !ok1 || !ok2 || ok3 || !reflect.DeepEqual(restarted, []string{"default", "work"}) {
		t.Fatalf("%v %v %v %v", ok1, ok2, ok3, restarted)
	}
}

func TestASiblingRouteUnderASymlinkedLaunchHomeIsCanonical(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	real := filepath.Join(base, "real")
	os.MkdirAll(filepath.Join(real, "profiles", "work"), 0o755)
	os.Symlink(real, filepath.Join(base, "link"))
	endpoint := GatewayEndpoint{ProfileID: real, InstanceID: "i", RuntimeProtocol: 1, APIOrigin: "http://127.0.0.1:1", Supervisor: "none"}
	routed, err := RoutedGatewayEndpoint(endpoint, "work", filepath.Join(base, "link"), "")
	if err != nil || routed.ProfileID != filepath.Join(real, "profiles", "work") {
		t.Fatalf("%+v %v", routed, err)
	}
	if canonical, _ := filepath.EvalSymlinks(routed.ProfileID); canonical != routed.ProfileID {
		t.Fatalf("not canonical %s", canonical)
	}
	if same, _ := RoutedGatewayEndpoint(endpoint, "default", filepath.Join(base, "link"), ""); !reflect.DeepEqual(same, endpoint) {
		t.Fatalf("default %+v", same)
	}
}

func TestAProfileHomeResolvesToExactlyTheHomeTheGatewayReservesForIt(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(base, "home")
	os.MkdirAll(filepath.Join(home, "profiles"), 0o755)
	os.Mkdir(filepath.Join(base, "other"), 0o755)
	os.Mkdir(filepath.Join(home, "work-data"), 0o755)
	os.Symlink("../work-data", filepath.Join(home, "profiles", "work"))
	os.Symlink(filepath.Join(base, "other"), filepath.Join(base, "link"))
	for _, h := range []string{home, filepath.Join(home, "profiles", "work"), filepath.Join(base, "link")} {
		got, err := ReservedHome(h)
		want, werr := pypath.Resolve(h)
		if err != nil || werr != nil || got != want {
			t.Fatalf("%s: %q %v want %q %v", h, got, err, want, werr)
		}
	}
}

func TestALinkedProfileMintsForTheHomeTheGatewayReservedAnAdvertisedHomeThatLeadsElsewhereIsRefused(t *testing.T) {
	root := shortSocketTmpDir(t, "gw-root-")
	target := shortSocketTmpDir(t, "gw-target-")
	links := shortSocketTmpDir(t, "gw-links-")
	os.Mkdir(filepath.Join(root, "profiles"), 0o700)
	os.Symlink(target, filepath.Join(root, "profiles", "work"))
	os.Symlink(root, filepath.Join(links, "home"))
	served := map[string]bool{root: true, target: true}
	requests, _ := controlServer(t, filepath.Join(root, "gateway.sock"), func(params map[string]any, n int) any {
		profile, _ := params["profile_id"].(string)
		if served[profile] {
			return grantReply(profile, "owner", fmt.Sprintf("grant-%d", n))
		}
		return map[string]any{"protocol": 1, "id": 1, "ok": false, "error": "PermissionError: bootstrap binding rejected"}
	})
	endpoint := testEndpoint(root, "owner")
	endpoint.APIOrigin = "http://127.0.0.1:1234"
	headers, err := NativeGatewayHTTPHeaders(endpoint, endpoint.APIOrigin, endpoint.APIOrigin+"/api/sessions?profile=work", "", "", nil)
	if err != nil || headers[GatewayTicketHeader] != "grant-1" {
		t.Fatalf("%v %v", headers, err)
	}
	if _, err := MintLocalGatewayTicket(testEndpoint(filepath.Join(links, "home"), "owner"), "native-http"); err == nil || err.Error() != "Noncanonical gateway profile" {
		t.Fatalf("linked home %v", err)
	}
	linked := filepath.Join(links, "home")
	viaLink := testEndpoint(root, "owner")
	viaLink.ControlHome = &linked
	if _, err := MintLocalGatewayTicket(viaLink, "native-http"); err == nil || err.Error() != "Noncanonical gateway profile" {
		t.Fatalf("linked control home %v", err)
	}
	_, gone := NativeGatewayHTTPHeaders(endpoint, endpoint.APIOrigin, endpoint.APIOrigin+"/api/sessions?profile=gone", "", "", nil)
	if gone == nil || !strings.Contains(gone.Error(), "Profile 'gone' does not exist") || IsStaleLocalGatewayError(gone) {
		t.Fatalf("gone %v", gone)
	}
	if !reflect.DeepEqual(*requests, []map[string]any{{"profile_id": target, "instance_id": "owner", "purpose": "native-http"}}) {
		t.Fatalf("requests %v", *requests)
	}
}

func TestAnUnsafeControlPathSurfacesWithoutForgettingAndReEnsuringTheSameOwner(t *testing.T) {
	failure := errors.New("Unsafe gateway control path")
	ensures, uses, forgets := 0, 0, 0
	_, err := RedialLocalGateway(func() (string, error) { ensures++; return "owner", nil },
		func() error { forgets++; return nil },
		func(string) (string, error) { uses++; return "", failure })
	if err != failure || ensures != 1 || uses != 1 || forgets != 0 {
		t.Fatalf("%v %d %d %d", err, ensures, uses, forgets)
	}
}

func TestAStaleMessageStillRefreshesItsEndpointOnce(t *testing.T) {
	for _, message := range []string{"Gateway ticket control socket missing", "Invalid gateway control pointer", "Noncanonical gateway profile"} {
		t.Run(message, func(t *testing.T) {
			ensures, forgets, uses := 0, 0, 0
			got, err := RedialLocalGateway(func() (string, error) { ensures++; return "owner", nil },
				func() error { forgets++; return nil },
				func(string) (string, error) {
					uses++
					if uses == 1 {
						return "", errors.New(message)
					}
					return "fresh ticket", nil
				})
			if err != nil || got != "fresh ticket" || ensures != 2 || forgets != 1 {
				t.Fatalf("%q %v %d %d", got, err, ensures, forgets)
			}
		})
	}
}

func TestEachUnsafeControlPathRefusalNamesThePathAndTheCommandThatRepairsIt(t *testing.T) {
	home := shortSocketTmpDir(t, "desktop-unsafe-msg-")
	failure := func(profile string) string {
		_, err := MintLocalGatewayTicket(testEndpoint(profile, "owner"), "")
		if err == nil || IsStaleLocalGatewayError(err) {
			t.Fatalf("err %v", err)
		}
		return err.Error()
	}
	sock := filepath.Join(home, "gateway.sock")
	os.Chmod(home, 0o757)
	if got := failure(home); got != fmt.Sprintf("Unsafe gateway control path: profile home '%s' is writable by other users (mode 757): run chmod o-w '%s'", home, home) {
		t.Fatal(got)
	}
	os.Chmod(home, 0o775)
	if got := failure(home); got != fmt.Sprintf("Unsafe gateway control path: profile home is group-writable and this Desktop cannot verify the group is private: run chmod g-w '%s'", home) {
		t.Fatal(got)
	}
	os.Chmod(home, 0o700)
	os.WriteFile(sock, nil, 0o600)
	if got := failure(home); got != fmt.Sprintf("Unsafe gateway control path: '%s' should be a socket but is a regular file: remove it, then run icebath gateway restart", sock) {
		t.Fatal(got)
	}
	os.Remove(sock)
	pointer := filepath.Join(home, "gateway.sock.path")
	os.WriteFile(pointer, []byte("/nowhere/control.sock\n"), 0o644)
	os.Chmod(pointer, 0o644)
	if got := failure(home); got != fmt.Sprintf("Unsafe gateway control path: '%s' is accessible to other users (mode 644): run chmod go-rwx '%s'", pointer, pointer) {
		t.Fatal(got)
	}
	root, _ := os.Lstat("/")
	if uid, owner := os.Getuid(), int(root.Sys().(*syscall.Stat_t).Uid); uid != owner {
		if got := failure("/"); got != fmt.Sprintf("Unsafe gateway control path: '/' is owned by uid %d, not this user (uid %d): run sudo chown %d '/'", owner, uid, uid) {
			t.Fatal(got)
		}
	}
}

type ticketRefusal struct{ reason string }

func (r ticketRefusal) Error() string        { return "Gateway ticket bootstrap failed" }
func (r ticketRefusal) TicketReason() string { return r.reason }
