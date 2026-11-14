package whatsapp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

// Health endpoint on the adapter port that answers once, then releases the port, delaying headers or body on request
func oneShotHealth(t *testing.T, a *WhatsAppAdapter, body string, delay string) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a.bridgePort = ln.Addr().(*net.TCPAddr).Port
	var once sync.Once
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer once.Do(func() { go ln.Close() })
		switch delay {
		case "headers":
			time.Sleep(2500 * time.Millisecond)
		case "body":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			time.Sleep(2500 * time.Millisecond)
		}
		w.Write([]byte(body))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// Bridge build and paired session under a temp root, the adapter built bare over them
func staleSetup(t *testing.T) (*WhatsAppAdapter, string) {
	t.Helper()
	hermeticHome(t)
	root := t.TempDir()
	script := filepath.Join(root, "icebath-bridge")
	if err := os.WriteFile(script, []byte("current bridge build\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, "session")
	if err := os.MkdirAll(session, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "creds.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return newBareAdapter(t, script, session, 19876), root
}

func TestStaleBridgeHandshakeRestartsBridgeWhenReadReceiptConfigChanged(t *testing.T) {
	noSleep(t)
	a, _ := staleSetup(t)
	a.sendReadReceipts = true
	oneShotHealth(t, a, fmt.Sprintf(`{"status":"connected","scriptHash":%q,"sendReadReceipts":false,"session":%q}`,
		fileContentHash(a.bridgeScript), a.sessionPath), "")
	spawns := 0
	fakeSpawn(t, func(args bridgeArgs, fh *os.File, env map[string]string) bridgeProc {
		spawns++
		return exitedProc(1)
	})
	_, _ = a.Connect(context.Background(), false)
	if spawns != 1 {
		t.Fatalf("spawns = %d", spawns)
	}
}

func TestStaleBridgeHandshakeBridgeIsAdoptedOrLeftRunningByItsSession(t *testing.T) {
	for _, tc := range []struct {
		id, holder, status, fatal string
	}{
		{"own-session-adopted", "own", "connected", ""},
		{"other-session-connected", "other", "connected", "whatsapp_bridge_foreign_session"},
		{"other-session-starting", "other", "disconnected", "whatsapp_bridge_foreign_session"},
		{"health-headers-timeout", "headers-timeout", "", "whatsapp_bridge_unresponsive"},
		{"health-body-timeout", "body-timeout", "", "whatsapp_bridge_unresponsive"},
		{"health-body-timeout-windows", "body-timeout-windows", "", "whatsapp_bridge_unresponsive"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			noSleep(t)
			a, root := staleSetup(t)
			reported := filepath.Join(root, "other-profile-session")
			if tc.holder == "own" {
				reported = a.sessionPath
			}
			status := "null"
			if tc.status != "" {
				status = strconv.Quote(tc.status)
			}
			delay := ""
			if strings.Contains(tc.holder, "timeout") {
				delay, _, _ = strings.Cut(tc.holder, "-")
			}
			oneShotHealth(t, a, fmt.Sprintf(`{"status":%s,"scriptHash":%q,"sendReadReceipts":false,"session":%q}`,
				status, fileContentHash(a.bridgeScript), reported), delay)
			windows := strings.HasSuffix(tc.holder, "-windows")
			onWindows(t, windows)
			savedListeners := listenerPidsOnPort
			listenerPidsOnPort = func(int) ([]int, error) {
				if windows {
					return []int{}, nil
				}
				return []int{4242}, nil
			}
			t.Cleanup(func() { listenerPidsOnPort = savedListeners })
			netstat := ""
			if windows {
				netstat = fmt.Sprintf("  TCP    127.0.0.1:%d           0.0.0.0:0              LISTENING       4242\n", a.bridgePort)
			}
			fakeCommands(t, netstat)
			forbidKills(t)
			if err := os.WriteFile(filepath.Join(a.sessionPath, "bridge.pid"), []byte("4190000"), 0o644); err != nil {
				t.Fatal(err)
			}
			spawns := 0
			fakeSpawn(t, func(args bridgeArgs, fh *os.File, env map[string]string) bridgeProc {
				spawns++
				return &fakeProc{}
			})
			ok, _ := a.Connect(context.Background(), false)
			defer a.Disconnect(context.Background())
			if ok != (tc.fatal == "") {
				t.Fatalf("connect = %v", ok)
			}
			if spawns != 0 {
				t.Fatal("a bridge was spawned")
			}
			_, pidfileErr := os.Stat(filepath.Join(a.sessionPath, "bridge.pid"))
			if reaped := pidfileErr != nil; reaped != (tc.fatal == "whatsapp_bridge_unresponsive") {
				t.Fatalf("pidfile reap ran = %v", reaped)
			}
			if tc.fatal != "" {
				code, msg := a.FatalErrorCode(), a.FatalErrorMessage()
				if code == nil || *code != tc.fatal || a.FatalErrorRetryable() != (tc.fatal == "whatsapp_bridge_unresponsive") {
					t.Fatalf("fatal = %v retryable=%v", code, a.FatalErrorRetryable())
				}
				if !strings.Contains(*msg, fmt.Sprintf("nothing on port %d was", a.bridgePort)) || !strings.Contains(*msg, "bridge_port") {
					t.Fatalf("message = %q", *msg)
				}
			}
		})
	}
}

func TestCacheDirEnvPassthroughBridgeSpawnEnvHasCacheDirs(t *testing.T) {
	noSleep(t)
	a, _ := staleSetup(t)
	a.sendReadReceipts = true
	a.bridgePort = freePort(t)
	var spawnEnv map[string]string
	fakeSpawn(t, func(args bridgeArgs, fh *os.File, env map[string]string) bridgeProc {
		spawnEnv = env
		return exitedProc(1)
	})
	_, _ = a.Connect(context.Background(), false)
	ctx := context.Background()
	h := home.Dir(ctx)
	img, _ := mediacache.ImageCacheDir(ctx, h)
	audio, _ := mediacache.AudioCacheDir(ctx, h)
	doc, _ := mediacache.DocumentCacheDir(ctx, h)
	if spawnEnv["ICEBATH_IMAGE_CACHE_DIR"] != img || spawnEnv["ICEBATH_AUDIO_CACHE_DIR"] != audio || spawnEnv["ICEBATH_DOCUMENT_CACHE_DIR"] != doc {
		t.Fatalf("cache env = %q %q %q", spawnEnv["ICEBATH_IMAGE_CACHE_DIR"], spawnEnv["ICEBATH_AUDIO_CACHE_DIR"], spawnEnv["ICEBATH_DOCUMENT_CACHE_DIR"])
	}
	if spawnEnv["WHATSAPP_SEND_READ_RECEIPTS"] != "true" {
		t.Fatalf("read receipts env = %q", spawnEnv["WHATSAPP_SEND_READ_RECEIPTS"])
	}
}
