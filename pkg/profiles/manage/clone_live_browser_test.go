package manage

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/nickheyer/icebath/pkg/browser"
)

func TestCreateProfileCloneAllDoesNotAttachToTheSourceProfilesLiveBrowser(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "model: test")
	browserProfile := filepath.Join(def, "bot-desktop", "browser-profile")
	if err := os.MkdirAll(browserProfile, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/browser/source"}`)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	writeFile(t, filepath.Join(browserProfile, "DevToolsActivePort"), fmt.Sprintf("%d\n/devtools/browser/source\n", port))
	if err := os.Symlink(fmt.Sprintf("host-%d", os.Getpid()), filepath.Join(browserProfile, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	if got, _, _, ok := browser.RunningInstance(browserProfile); !ok || got != port {
		t.Fatalf("source browser %d %v want %d", got, ok, port)
	}
	dir, err := CreateProfile(context.Background(), "coder", CreateOptions{CloneAll: true})
	if err != nil {
		t.Fatal(err)
	}
	cloned := filepath.Join(dir, "bot-desktop", "browser-profile")
	if got, _, _, ok := browser.RunningInstance(cloned); ok {
		t.Fatalf("clone attached to port %d", got)
	}
	if got, _, _, ok := browser.RunningInstance(browserProfile); !ok || got != port {
		t.Fatalf("source browser after clone %d %v", got, ok)
	}
}
