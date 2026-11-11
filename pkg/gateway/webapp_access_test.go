package gateway

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Root with a standalone profile solo and a sibling other claiming 9122, 9121 held by something else
type accessMachine struct {
	root, solo string
	mu         sync.Mutex
	listening  map[int]bool
	probed     [][2]any
}

// Writes a config yaml into the dir, creating it
func writeAccessConfig(t *testing.T, dir, text string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newAccessMachine(t *testing.T) *accessMachine {
	t.Helper()
	tmp := t.TempDir()
	root := writeAccessConfig(t, filepath.Join(tmp, ".icebath"), "")
	m := &accessMachine{root: root, listening: map[int]bool{9121: true}}
	m.solo = writeAccessConfig(t, filepath.Join(root, "profiles", "solo"), "gateway:\n  standalone: true\n")
	writeAccessConfig(t, filepath.Join(root, "profiles", "other"), "gateway:\n  standalone: true\nwebapp:\n  port: 9122\n")
	swapSeam(t, &webappAccessPortFree, func(host string, port int) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.probed = append(m.probed, [2]any{host, port})
		return !m.listening[port]
	})
	swapSeam(t, &webappAccessSignedIn, func(context.Context) bool { return true })
	swapSeam(t, &wl.lanAddress, func() string { return "192.168.1.50" })
	t.Setenv("ICEBATH_HOME", m.solo)
	config.InvalidateManagedCache()
	return m
}

func (m *accessMachine) probes() [][2]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][2]any(nil), m.probed...)
}

func (m *accessMachine) clearProbes() {
	m.mu.Lock()
	m.probed = nil
	m.mu.Unlock()
}

// Webapp block of the home user config file, empty when absent
func accessWebappBlock(t *testing.T, h string) map[string]any {
	t.Helper()
	raw, err := config.ReadUserConfigRaw(context.Background(), filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := raw["webapp"].(map[string]any)
	if block == nil {
		return map[string]any{}
	}
	return block
}

// Plan of the mode in the process scope, failing the case on an error
func mustPlan(t *testing.T, mode, url string) (string, int, string, string) {
	t.Helper()
	host, port, pinned, callback, err := WebappAccessPlan(context.Background(), mode, url)
	if err != nil {
		t.Fatalf("plan %s: %v", mode, err)
	}
	return host, port, pinned, callback
}

func mustServe(t *testing.T, host, publicURL string, port int) {
	t.Helper()
	if err := WebappAccessServe(context.Background(), host, publicURL, port); err != nil {
		t.Fatal(err)
	}
}

func intOf(v any) int {
	n, _, _ := webappConfigInt(v)
	return n
}

func TestTheChosenPortStaysForThatProfile(t *testing.T) {
	m := newAccessMachine(t)
	host, port, public, _ := mustPlan(t, "lan", "")
	mustServe(t, host, public, port)
	if err := WebappAccessTurnOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.clearProbes()
	m.mu.Lock()
	m.listening[9123] = true
	m.mu.Unlock()
	for _, tc := range [][3]string{{"lan", "", "http://192.168.1.50:9123/auth/callback"},
		{"public", "https://solo.example.test", "https://solo.example.test/auth/callback"}} {
		host, port, public, callback := mustPlan(t, tc[0], tc[1])
		if port != 0 || callback != tc[2] {
			t.Fatalf("%s: port %d callback %q", tc[0], port, callback)
		}
		mustServe(t, host, public, port)
		if got := intOf(accessWebappBlock(t, m.solo)["port"]); got != 9123 {
			t.Fatalf("port %d", got)
		}
	}
	if len(m.probes()) != 0 {
		t.Fatalf("probed %v", m.probes())
	}
}

func TestTheOwnUrlModeAlsoGetsAFreePort(t *testing.T) {
	m := newAccessMachine(t)
	host, port, _, callback := mustPlan(t, "public", "https://solo.example.test")
	if host != "127.0.0.1" || port != 9123 || callback != "https://solo.example.test/auth/callback" {
		t.Fatalf("%s %d %s", host, port, callback)
	}
	probes := m.probes()
	if last := probes[len(probes)-1]; last != [2]any{"127.0.0.1", 9123} {
		t.Fatalf("last probe %v", last)
	}
}

func TestTheDefaultProfileKeepsTheDocumentedPortAndWritesNone(t *testing.T) {
	m := newAccessMachine(t)
	t.Setenv("ICEBATH_HOME", m.root)
	host, port, public, callback := mustPlan(t, "lan", "")
	if port != 0 || callback != "http://192.168.1.50:9120/auth/callback" {
		t.Fatalf("port %d callback %s", port, callback)
	}
	mustServe(t, host, public, port)
	if _, has := accessWebappBlock(t, m.root)["port"]; has {
		t.Fatal("port written")
	}
	if len(m.probes()) != 0 {
		t.Fatalf("probed %v", m.probes())
	}
}

func TestAPortTheUserSetIsNeverReplaced(t *testing.T) {
	for _, configured := range []string{"9555", "9120", "not-a-port"} {
		t.Run(configured, func(t *testing.T) {
			m := newAccessMachine(t)
			writeAccessConfig(t, m.solo, "gateway:\n  standalone: true\nwebapp:\n  port: "+configured+"\n")
			host, port, public, _ := mustPlan(t, "lan", "")
			if port != 0 || len(m.probes()) != 0 {
				t.Fatalf("port %d probes %v", port, m.probes())
			}
			mustServe(t, host, public, port)
			got := accessWebappBlock(t, m.solo)["port"]
			if configured == "not-a-port" {
				if got != "not-a-port" {
					t.Fatalf("port %#v", got)
				}
			} else if n, ok, _ := webappConfigInt(got); !ok || configured != strconv.Itoa(n) {
				t.Fatalf("port %#v", got)
			}
		})
	}
}

func TestWithNoFreePortStartRefusesAndNamesTheSetting(t *testing.T) {
	m := newAccessMachine(t)
	m.mu.Lock()
	for p := 9121; p < 9221; p++ {
		m.listening[p] = true
	}
	m.mu.Unlock()
	_, _, _, _, err := WebappAccessPlan(context.Background(), "lan", "")
	var refused *WebappAccessError
	if !errors.As(err, &refused) {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(refused.Error(), "webapp.port") || !strings.Contains(refused.Error(), filepath.Join(m.solo, "config.yaml")) {
		t.Fatalf("message %q", refused.Error())
	}
	if _, has := accessWebappBlock(t, m.solo)["port"]; has {
		t.Fatal("port written")
	}
}

func TestAStartRefusedForNoNousAccountSavesNoPort(t *testing.T) {
	for _, tc := range [][2]string{{"lan", ""}, {"public", "https://solo.example.test"}} {
		t.Run(tc[0], func(t *testing.T) {
			m := newAccessMachine(t)
			swapSeam(t, &webappAccessSignedIn, func(context.Context) bool { return false })
			_, _, _, _, err := WebappAccessPlan(context.Background(), tc[0], tc[1])
			if err == nil || !strings.Contains(err.Error(), "Sign in to your Nous account") {
				t.Fatalf("err %v", err)
			}
			if len(accessWebappBlock(t, m.solo)) != 0 || len(m.probes()) != 0 {
				t.Fatalf("block %v probes %v", accessWebappBlock(t, m.solo), m.probes())
			}
		})
	}
}

func TestAStartThatFailsAfterChoosingKeepsThePort(t *testing.T) {
	m := newAccessMachine(t)
	mustPlan(t, "lan", "")
	block := accessWebappBlock(t, m.solo)
	if len(block) != 1 || intOf(block["port"]) != 9123 {
		t.Fatalf("block %v", block)
	}
	m.clearProbes()
	_, port, _, callback := mustPlan(t, "lan", "")
	if port != 0 || callback != "http://192.168.1.50:9123/auth/callback" || len(m.probes()) != 0 {
		t.Fatalf("port %d callback %s probes %v", port, callback, m.probes())
	}
}

func TestTheProbeSeesAPortAListenerHolds(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := held.Addr().(*net.TCPAddr).Port
	if webappPortFree("127.0.0.1", port) {
		t.Fatal("held port reported free")
	}
	held.Close()
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	if !webappPortFree("127.0.0.1", free) {
		t.Fatalf("free port %d reported taken", free)
	}
}

func TestAStandaloneProfileWithoutAPortGetsAFreeOneThatEverySurfaceUses(t *testing.T) {
	m := newAccessMachine(t)
	swapSeam(t, &WebappRendererBuild.PackagedDist, func() string { return "" })
	gh, err := WebappGatewayHome(context.Background())
	if err != nil || gh != m.solo {
		t.Fatalf("gateway home %q %v", gh, err)
	}
	host, port, public, callback := mustPlan(t, "lan", "")
	if callback != "http://192.168.1.50:9123/auth/callback" || host != "0.0.0.0" || port != 9123 || public != "" {
		t.Fatalf("%s %d %q %s", host, port, public, callback)
	}
	mustServe(t, host, public, port)
	block := accessWebappBlock(t, m.solo)
	if len(block) != 3 || block["enabled"] != true || block["host"] != "0.0.0.0" || intOf(block["port"]) != 9123 {
		t.Fatalf("block %v", block)
	}
	bindHost, bindPort, mount, _ := WebappListenerPlan(context.Background(), m.solo)
	if bindHost != "0.0.0.0" || bindPort != 9123 || mount == nil {
		t.Fatalf("listener %s %d %v", bindHost, bindPort, mount)
	}
	if got := WebappDialOrigin(bindHost, bindPort); got != "http://127.0.0.1:9123" {
		t.Fatalf("dial origin %s", got)
	}
	status, err := WebappAccessStatus(context.Background())
	if err != nil || intOf(status.GetOr("port", nil)) != 9123 {
		t.Fatalf("status %v %v", status, err)
	}
	if len(accessWebappBlock(t, m.root)) != 0 {
		t.Fatalf("root block %v", accessWebappBlock(t, m.root))
	}
}

func TestAPortTheManagedScopeSetsIsTheOneTheCallbackNames(t *testing.T) {
	m := newAccessMachine(t)
	swapSeam(t, &WebappRendererBuild.PackagedDist, func() string { return "" })
	managed := writeAccessConfig(t, filepath.Join(t.TempDir(), "managed"), "webapp:\n  port: 9555\n")
	t.Setenv("ICEBATH_MANAGED_DIR", managed)
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	host, port, public, callback := mustPlan(t, "lan", "")
	if port != 0 || callback != "http://192.168.1.50:9555/auth/callback" || len(m.probes()) != 0 {
		t.Fatalf("port %d callback %s probes %v", port, callback, m.probes())
	}
	mustServe(t, host, public, port)
	if _, has := accessWebappBlock(t, m.solo)["port"]; has {
		t.Fatal("port written over the managed one")
	}
	bindHost, bindPort, mount, _ := WebappListenerPlan(context.Background(), m.solo)
	if bindHost != "0.0.0.0" || bindPort != 9555 || mount == nil {
		t.Fatalf("listener %s %d %v", bindHost, bindPort, mount)
	}
}

const accessSyncWait = 20 * time.Second

// Waits for a marker file another process creates, false after the sync wait
func awaitMarker(path string) bool {
	deadline := time.Now().Add(accessSyncWait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// Creates a marker file another process waits for
func setMarker(path string) { _ = os.WriteFile(path, nil, 0o644) }

// One standalone profile Start in a process of its own, run when the parent case re-executes this binary
func TestWebappAccessStartInAProcessOfItsOwn(t *testing.T) {
	h, role, syncDir := os.Getenv("ICEBATH_ACCESS_CHILD_HOME"), os.Getenv("ICEBATH_ACCESS_CHILD_ROLE"), os.Getenv("ICEBATH_ACCESS_CHILD_SYNC")
	if h == "" {
		return
	}
	firstHolds, secondSelecting := filepath.Join(syncDir, "first-holds"), filepath.Join(syncDir, "second-selecting")
	t.Setenv("ICEBATH_HOME", h)
	swapSeam(t, &webappAccessSignedIn, func(context.Context) bool { return true })
	swapSeam(t, &wl.lanAddress, func() string { return "192.168.1.50" })
	listening := map[int]bool{9121: true}
	if role == "first" {
		held := false
		swapSeam(t, &webappAccessPortFree, func(_ string, port int) bool {
			if !held {
				held = true
				setMarker(firstHolds)
				if !awaitMarker(secondSelecting) {
					panic("the second Start never reached the port claim")
				}
			}
			return !listening[port]
		})
	} else {
		lock := webappAccessFileLock
		swapSeam(t, &webappAccessFileLock, func(path string, timeout time.Duration, message string) (func(), error) {
			if filepath.Base(path) == "port-claim.lock" {
				setMarker(secondSelecting)
			}
			return lock(path, timeout, message)
		})
		swapSeam(t, &webappAccessPortFree, func(_ string, port int) bool {
			setMarker(secondSelecting)
			return !listening[port]
		})
		if !awaitMarker(firstHolds) {
			t.Fatal("the first Start never chose a port")
		}
	}
	host, port, public, _ := mustPlan(t, "lan", "")
	mustServe(t, host, public, port)
	if err := os.WriteFile(filepath.Join(syncDir, role+"-port"), []byte(strconv.Itoa(intOf(accessWebappBlock(t, h)["port"]))), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTwoProfilesStartingAtOnceNeverSaveTheSamePort(t *testing.T) {
	m := newAccessMachine(t)
	twin := writeAccessConfig(t, filepath.Join(m.root, "profiles", "twin"), "gateway:\n  standalone: true\n")
	syncDir := t.TempDir()
	homes := map[string]string{"first": m.solo, "second": twin}
	var wg sync.WaitGroup
	outputs := map[string][]byte{}
	var mu sync.Mutex
	for role, h := range homes {
		wg.Add(1)
		go func(role, h string) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestWebappAccessStartInAProcessOfItsOwn$", "-test.count=1")
			cmd.Env = append(os.Environ(), "ICEBATH_ACCESS_CHILD_HOME="+h, "ICEBATH_ACCESS_CHILD_ROLE="+role, "ICEBATH_ACCESS_CHILD_SYNC="+syncDir)
			out, _ := cmd.CombinedOutput()
			mu.Lock()
			outputs[role] = out
			mu.Unlock()
		}(role, h)
	}
	wg.Wait()
	saved := map[string]string{}
	for role, h := range homes {
		raw, err := os.ReadFile(filepath.Join(syncDir, role+"-port"))
		if err != nil {
			t.Fatalf("%s start reported nothing: %s", role, outputs[role])
		}
		saved[h] = string(raw)
	}
	if saved[m.solo] != "9123" || saved[twin] != "9124" {
		t.Fatalf("saved %v", saved)
	}
}
