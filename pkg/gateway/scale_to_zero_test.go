package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestEnabledTruthyValues(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", "on", " On "} {
		if !ScaleToZeroEnabled(map[string]string{ScaleToZeroEnv: value}) {
			t.Fatalf("%q should enable scale to zero", value)
		}
	}
}

func TestTimeoutParsesMinutesToSeconds(t *testing.T) {
	for _, c := range []struct {
		in   any
		want float64
	}{{5, 300.0}, {10, 600.0}, {"5", 300.0}} {
		if got := ParseIdleTimeoutSeconds(c.in, DefaultIdleTimeoutMinutes); got != c.want {
			t.Fatalf("%v: got %v want %v", c.in, got, c.want)
		}
	}
}

func TestTimeoutInvalidValuesDegradeToDefault(t *testing.T) {
	for _, bad := range []any{nil, "", "nope", 0, -3} {
		if got := ParseIdleTimeoutSeconds(bad, DefaultIdleTimeoutMinutes); got != DefaultIdleTimeoutMinutes*60.0 {
			t.Fatalf("%v: got %v", bad, got)
		}
	}
}

func TestRelayOnlyIsTrue(t *testing.T) {
	if !MessagingIsRelayOnlyOrAbsent([]string{"relay"}) {
		t.Fatal("relay only should be true")
	}
}

func TestNoPlatformIsTrue(t *testing.T) {
	if !MessagingIsRelayOnlyOrAbsent(nil) {
		t.Fatal("no platform should be true")
	}
}

func TestArmBlockedWithoutWakeURL(t *testing.T) {
	empty := ""
	if ShouldArm(true, true, nil) || ShouldArm(true, true, &empty) {
		t.Fatal("arming without a wake URL must be refused")
	}
}

func TestNotIdleWithRunningAgent(t *testing.T) {
	if IsIdle(1, 600.0, 300.0, false) {
		t.Fatal("a running agent is not idle")
	}
}

func TestIdleExactlyAtThreshold(t *testing.T) {
	if !IsIdle(0, 300.0, 300.0, false) {
		t.Fatal("the timeout boundary is idle")
	}
}

var flyTestEnv = map[string]string{FlyAppNameEnv: "icebath-agent-stg-test", FlyMachineIDEnv: "d891234f"}

// One shot unix socket HTTP server standing in for flaps, returning its socket path and captured request
func fakeFlaps(t *testing.T, statusLine string) (string, <-chan string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "s2z-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "fly-api.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	captured := make(chan string, 1)
	go func() {
		defer listener.Close()
		conn, err := listener.Accept()
		if err != nil {
			captured <- ""
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		var data strings.Builder
		for !strings.Contains(data.String(), "\r\n\r\n") {
			line, err := reader.ReadString('\n')
			data.WriteString(line)
			if err != nil {
				break
			}
		}
		captured <- data.String()
		_, _ = io.WriteString(conn, "HTTP/1.1 "+statusLine+"\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
	}()
	return sockPath, captured
}

func TestSuspendSelfPostsSuspendForThisMachine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	sockPath, captured := fakeFlaps(t, "200 OK")
	if !SuspendSelf(flyTestEnv, sockPath, scaleToZeroSuspendSelfTimeoutS) {
		t.Fatal("a 200 from flaps should be accepted")
	}
	request := <-captured
	if !strings.HasPrefix(request, "POST /v1/apps/icebath-agent-stg-test/machines/d891234f/suspend HTTP/1.1\r\n") {
		t.Fatalf("request = %q", request)
	}
	if !strings.Contains(request, "Host: flaps\r\n") {
		t.Fatalf("request = %q", request)
	}
}

func TestSuspendSelfNon2xxIsFalseNotRaise(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	sockPath, captured := fakeFlaps(t, "412 Precondition Failed")
	if SuspendSelf(flyTestEnv, sockPath, scaleToZeroSuspendSelfTimeoutS) {
		t.Fatal("a 412 must not be accepted")
	}
	<-captured
}

func TestSuspendSelfMissingSocketIsFalseNotRaise(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	if SuspendSelf(flyTestEnv, filepath.Join(t.TempDir(), "nope.sock"), scaleToZeroSuspendSelfTimeoutS) {
		t.Fatal("a missing socket must fail awake")
	}
}

func TestSuspendSelfRequiresMachineIdentity(t *testing.T) {
	if SuspendSelf(map[string]string{}, filepath.Join(t.TempDir(), "unused.sock"), scaleToZeroSuspendSelfTimeoutS) {
		t.Fatal("no identity must not suspend")
	}
}

func TestSelfSuspendAvailableNeedsIdentityAndSocket(t *testing.T) {
	if _, err := os.Stat(FlyAPISocket); err != nil && SelfSuspendAvailable(flyTestEnv) {
		t.Fatal("no socket means unavailable")
	}
	if SelfSuspendAvailable(map[string]string{}) {
		t.Fatal("no identity means unavailable")
	}
}

const testSleepURL = "https://portal.example.com/api/agents/inst-1/sleep?t=sig"

func TestBrokeredSleepURLReadsTheStamp(t *testing.T) {
	if got := BrokeredSleepURL(map[string]string{SleepURLEnv: testSleepURL}); got == nil || *got != testSleepURL {
		t.Fatalf("got %v", got)
	}
	if BrokeredSleepURL(map[string]string{}) != nil || BrokeredSleepURL(map[string]string{SleepURLEnv: "   "}) != nil {
		t.Fatal("unset or blank stamp must be nil")
	}
}

func TestMalformedSleepURLWarnsOnceNotEveryIdleTick(t *testing.T) {
	malformedSleepURLLoggedMu.Lock()
	malformedSleepURLLogged = false
	malformedSleepURLLoggedMu.Unlock()
	t.Setenv(SleepURLEnv, "http://portal.example.com/x")
	logs := captureLogs(t)
	for range 5 {
		if BrokeredSleepURL(nil) != nil {
			t.Fatal("malformed stamp must be nil")
		}
	}
	if n := strings.Count(logs.String(), "ignoring malformed"); n != 1 {
		t.Fatalf("warned %d times", n)
	}
}

func TestBrokeredSleepURLRejectsAMalformedStamp(t *testing.T) {
	for _, bad := range []string{"not-a-url", "http://portal.example.com/x", "/api/agents/i/sleep"} {
		t.Setenv(SleepURLEnv, bad)
		if BrokeredSleepURL(nil) != nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	t.Setenv(SleepURLEnv, "https://portal.example.com/x?t=s")
	if got := BrokeredSleepURL(nil); got == nil || *got != "https://portal.example.com/x?t=s" {
		t.Fatalf("got %v", got)
	}
}

// Round tripper standing in for the urllib opener the Python cases pass
type brokerOpener func(req *http.Request) (*http.Response, error)

func (f brokerOpener) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Response with the status and an empty body
func brokerResponse(req *http.Request, status int, reason string) *http.Response {
	return &http.Response{StatusCode: status, Status: reason, Body: io.NopCloser(strings.NewReader("")), Request: req}
}

func TestRequestBrokeredSuspendPostsTheSignedURL(t *testing.T) {
	seen := map[string]string{}
	client := &http.Client{Transport: brokerOpener(func(req *http.Request) (*http.Response, error) {
		seen["url"], seen["method"] = req.URL.String(), req.Method
		return brokerResponse(req, 200, "200 OK"), nil
	})}
	if !RequestBrokeredSuspend(context.Background(), testSleepURL, BrokeredSuspendTimeoutS, client) {
		t.Fatal("a 200 from the broker should be accepted")
	}
	if seen["url"] != testSleepURL || seen["method"] != "POST" {
		t.Fatalf("seen = %v", seen)
	}
}

func TestRequestBrokeredSuspendFailsAwake(t *testing.T) {
	openers := []brokerOpener{
		func(req *http.Request) (*http.Response, error) { return brokerResponse(req, 409, "409 Conflict"), nil },
		func(req *http.Request) (*http.Response, error) { return nil, errors.New("connection refused") },
		func(req *http.Request) (*http.Response, error) {
			return brokerResponse(req, 500, "500 Internal Server Error"), nil
		},
	}
	for i, opener := range openers {
		if RequestBrokeredSuspend(context.Background(), testSleepURL, BrokeredSuspendTimeoutS, &http.Client{Transport: opener}) {
			t.Fatalf("opener %d accepted", i)
		}
	}
}

// Temp process home for the dashboard client marker
func dashboardHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(home.EnvHome, dir)
	return dir
}

// Marker made unreadable by removing search permission from its directory
func denyMarkerStat(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("permission bits do not deny stat here")
	}
	dir := filepath.Dir(DashboardClientHeartbeatPath(""))
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

func TestHeartbeatPathLivesUnderHermesHomeState(t *testing.T) {
	dir := dashboardHome(t)
	if got := DashboardClientHeartbeatPath(""); got != filepath.Join(dir, "state", "dashboard_clients.heartbeat") {
		t.Fatalf("path = %s", got)
	}
}

func TestLastSeenMissingMarkerIsNoneNotFailAwake(t *testing.T) {
	dashboardHome(t)
	if seen := DashboardClientLastSeen("", nil); seen != nil {
		t.Fatalf("seen = %v", *seen)
	}
}

func TestTouchCreatesStateDirAndMarker(t *testing.T) {
	dashboardHome(t)
	if !TouchDashboardClientHeartbeat("") {
		t.Fatal("touch failed")
	}
	if _, err := os.Stat(DashboardClientHeartbeatPath("")); err != nil {
		t.Fatal(err)
	}
	seen := DashboardClientLastSeen("", nil)
	if seen == nil || math.Abs(*seen-float64(time.Now().UnixNano())/1e9) >= 5 {
		t.Fatalf("seen = %v", seen)
	}
}

func TestLastSeenReturnsRawMtimeWithoutStalenessCutoff(t *testing.T) {
	dashboardHome(t)
	TouchDashboardClientHeartbeat("")
	info, err := os.Stat(DashboardClientHeartbeatPath(""))
	if err != nil {
		t.Fatal(err)
	}
	mtime := float64(info.ModTime().UnixNano()) / 1e9
	for _, offset := range []float64{10, 3600} {
		now := mtime + offset
		if seen := DashboardClientLastSeen("", &now); seen == nil || *seen != mtime {
			t.Fatalf("seen = %v want %v", seen, mtime)
		}
	}
}

func TestLastSeenFutureMtimeIsClampedToNow(t *testing.T) {
	dashboardHome(t)
	TouchDashboardClientHeartbeat("")
	future := time.Now().Add(600 * time.Second)
	if err := os.Chtimes(DashboardClientHeartbeatPath(""), future, future); err != nil {
		t.Fatal(err)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	if seen := DashboardClientLastSeen("", &now); seen == nil || *seen != now {
		t.Fatalf("seen = %v want %v", seen, now)
	}
}

func TestLastSeenUnreadableMarkerFailsAwake(t *testing.T) {
	dashboardHome(t)
	TouchDashboardClientHeartbeat("")
	denyMarkerStat(t)
	now := 1_000_000.0
	if seen := DashboardClientLastSeen("", &now); seen == nil || *seen != now {
		t.Fatalf("seen = %v", seen)
	}
}

func TestTouchNeverRaises(t *testing.T) {
	dashboardHome(t)
	if err := os.MkdirAll(DashboardClientHeartbeatPath(""), 0o755); err != nil {
		t.Fatal(err)
	}
	if TouchDashboardClientHeartbeat("") {
		t.Fatal("a failed touch must report false")
	}
}
