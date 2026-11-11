package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Launch namespace of an attach probe with the defaults of the serve command
func attachArgs(pairs ...any) map[string]any {
	ns := map[string]any{"host": "127.0.0.1", "port": 9200, "no_open": true, "isolated": false, "open_profile": ""}
	for i := 0; i+1 < len(pairs); i += 2 {
		ns[pairs[i].(string)] = pairs[i+1]
	}
	return ns
}

// Scratch lock directory with the Desktop marker cleared and a bare serve argv
func attachHostDir(t *testing.T) {
	t.Helper()
	hostDir(t)
	t.Setenv("ICEBATH_DESKTOP", "")
	swapSeam(t, &dashboardTypedArgv, func() []string { return []string{"serve"} })
}

// Loopback owner that answers the host identity path, its SPA flag settable per test
type attachOwner struct {
	port      int
	servesSpa bool
}

func startAttachOwner(t *testing.T) *attachOwner {
	t.Helper()
	owner := &attachOwner{servesSpa: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != HostIdentityPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": os.Getpid(), "role": RoleServe, "servesSpa": owner.servesSpa})
	}))
	t.Cleanup(server.Close)
	owner.port = server.Listener.Addr().(*net.TCPAddr).Port
	return owner
}

// Port nothing listens on
func deadPort(t *testing.T) int {
	t.Helper()
	spare, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer spare.Close()
	return spare.Addr().(*net.TCPAddr).Port
}

func publishServe(t *testing.T, createTime *float64, pid, port int) {
	t.Helper()
	writeHostRecord(t, &HostRecord{Role: RoleServe, PID: pid, CreateTime: createTime, Host: "127.0.0.1", Port: &port,
		ProtocolVersion: HostProtocolVersion, Profiles: []string{"default", "alpha"}, UpdatedAt: "2026-01-01T00:00:00+00:00"})
}

// Runs the attach step and returns its exit, nil when the launch proceeds to the bind, with the printed text
func attach(t *testing.T, ns map[string]any, headless bool) (*DashboardExit, string) {
	t.Helper()
	var out bytes.Buffer
	err := attachToHostBackend(context.Background(), DashboardLaunch{Namespace: ns, Stdout: &out, Stderr: &out}, headless)
	if err == nil {
		return nil, out.String()
	}
	var exit *DashboardExit
	if !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return exit, out.String()
}

func TestSecondServeAttachesToTheLiveHostBackend(t *testing.T) {
	attachHostDir(t)
	owner := startAttachOwner(t)
	publishServe(t, HostProcessCreateTime(os.Getpid()), os.Getpid(), owner.port)
	exit, out := attach(t, attachArgs(), true)
	if exit == nil || exit.Code != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	if !strings.Contains(out, fmt.Sprintf("port %d", owner.port)) {
		t.Fatalf("out = %q", out)
	}
}

func TestInheritedDesktopFlagWithoutSpawnCredentialStillAttaches(t *testing.T) {
	attachHostDir(t)
	owner := startAttachOwner(t)
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "")
	publishServe(t, HostProcessCreateTime(os.Getpid()), os.Getpid(), owner.port)
	if exit, _ := attach(t, attachArgs(), true); exit == nil || exit.Code != 0 {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestDesktopOwnedBackendKeepsItsSeparateLifecycle(t *testing.T) {
	attachHostDir(t)
	owner := startAttachOwner(t)
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "desktop-spawn-token")
	publishServe(t, HostProcessCreateTime(os.Getpid()), os.Getpid(), owner.port)
	if exit, _ := attach(t, attachArgs(), true); exit != nil {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestStaleRecordIsIgnoredAndTheLaunchProceeds(t *testing.T) {
	attachHostDir(t)
	stale := 1.0
	publishServe(t, &stale, os.Getpid(), 9119)
	if exit, _ := attach(t, attachArgs(), true); exit != nil {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestIsolatedNeverAttaches(t *testing.T) {
	attachHostDir(t)
	owner := startAttachOwner(t)
	publishServe(t, HostProcessCreateTime(os.Getpid()), os.Getpid(), owner.port)
	if exit, _ := attach(t, attachArgs("isolated", true), true); exit != nil {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestARecordWhoseOwnerDoesNotAnswerFallsThroughToTheBind(t *testing.T) {
	attachHostDir(t)
	publishServe(t, HostProcessCreateTime(os.Getpid()), os.Getpid(), deadPort(t))
	if exit, _ := attach(t, attachArgs(), true); exit != nil {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestAnExplicitEndpointTheOwnerCannotServeIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		argv []string
		over []any
	}{
		{"explicit-port", []string{"serve", "--port", "8899"}, []any{"port", 8899}},
		{"explicit-host", []string{"serve", "--host", "0.0.0.0"}, []any{"host", "0.0.0.0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			attachHostDir(t)
			owner := startAttachOwner(t)
			swapSeam(t, &dashboardTypedArgv, func() []string { return c.argv })
			publishServe(t, HostProcessCreateTime(os.Getpid()), os.Getpid(), owner.port)
			exit, out := attach(t, attachArgs(c.over...), true)
			if exit == nil || exit.Code != GatewayFatalConfigExitCode {
				t.Fatalf("exit = %+v", exit)
			}
			if !strings.Contains(out, fmt.Sprintf("PID %d", os.Getpid())) {
				t.Fatalf("out = %q", out)
			}
		})
	}
}

func TestDashboardIsNeverRoutedToAHeadlessBackend(t *testing.T) {
	attachHostDir(t)
	owner := startAttachOwner(t)
	owner.servesSpa = false
	publishServe(t, HostProcessCreateTime(os.Getpid()), os.Getpid(), owner.port)
	exit, out := attach(t, attachArgs(), false)
	if exit == nil || exit.Code != GatewayFatalConfigExitCode {
		t.Fatalf("exit = %+v", exit)
	}
	if !strings.Contains(out, "no dashboard UI") {
		t.Fatalf("out = %q", out)
	}
}
