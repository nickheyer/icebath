//go:build !windows

package browser

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
	"golang.org/x/sys/unix"
)

func init() { extraChildModes["lightpanda-serve"] = runLightpandaServeChild }

// Real spawn of a fake serve binary that checks its stdio, session and env before answering json/version
func TestLightpandaRealSpawnServesAndStops(t *testing.T) {
	env, state := isolateLightpanda(t)
	pin(t, &lightpandaStartTime, safeStartTime)
	exe := fakeExe(t, "lightpanda-serve")
	pin(t, &lightpandaFindBinary, func(*tools.Env) string { return exe })
	pin(t, &lightpandaBrowserEnv, func(*tools.Env) map[string]string {
		return map[string]string{"PATH": os.Getenv("PATH"), "LP_MARK": "composed", "ICEBATH_HOME": env.Home}
	})
	server, err := launchLightpanda(env, "lp_real", false)
	if err != "" {
		t.Fatal(err)
	}
	pid := server.Pid()
	if !server.IsAlive() || server.StartTime == nil {
		t.Fatalf("server %+v", server)
	}
	if tail := readLightpandaLogTail(server.LogPath); tail != "info: listening" {
		t.Fatalf("log tail %q", tail)
	}
	stopLightpanda(env, "lp_real")
	if server.IsAlive() || getLightpandaServer("lp_real") != nil {
		t.Fatal("server survived stop")
	}
	if _, err := os.Stat(filepath.Join(state, "lp_real.json")); err == nil {
		t.Fatal("record kept")
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("pid %d still running", pid)
	}
}

// Fake lightpanda serve, fails unless stdin is empty, stdout is the null device, it leads a session and LP_MARK came through
func runLightpandaServeChild() int {
	port := ""
	for i, a := range os.Args {
		if a == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	if os.Args[1] != "serve" || port == "" {
		fmt.Fprintln(os.Stderr, "FATAL bad argv", os.Args)
		return 2
	}
	buf := make([]byte, 1)
	if n, _ := os.Stdin.Read(buf); n != 0 {
		fmt.Fprintln(os.Stderr, "FATAL stdin not empty")
		return 3
	}
	var out, null syscall.Stat_t
	if syscall.Fstat(1, &out) != nil || syscall.Stat(os.DevNull, &null) != nil || out.Rdev != null.Rdev || out.Ino != null.Ino {
		fmt.Fprintln(os.Stderr, "FATAL stdout not the null device")
		return 4
	}
	if sid, _ := unix.Getsid(0); sid != os.Getpid() {
		fmt.Fprintln(os.Stderr, "FATAL not a session leader")
		return 5
	}
	if os.Getenv("LP_MARK") != "composed" {
		fmt.Fprintln(os.Stderr, "FATAL env not passed")
		return 6
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL", err)
		return 7
	}
	fmt.Fprintln(os.Stderr, "info: listening")
	http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Browser": "Lightpanda/1.0"}`))
	}))
	return 0
}

func TestLightpandaRealSpawnFailureCarriesStderr(t *testing.T) {
	env, _ := isolateLightpanda(t)
	exe := fakeExe(t, "lightpanda-serve")
	pin(t, &lightpandaFindBinary, func(*tools.Env) string { return exe })
	pin(t, &lightpandaBrowserEnv, func(*tools.Env) map[string]string { return map[string]string{"PATH": os.Getenv("PATH")} })
	server, err := launchLightpanda(env, "lp_noenv", false)
	if server != nil || !strings.Contains(err, "exited with code 6") || !strings.Contains(err, "FATAL env not passed") {
		t.Fatalf("err %q", err)
	}
}
