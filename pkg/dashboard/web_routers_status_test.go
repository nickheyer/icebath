package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/installidentity"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

func TestHealthReportsTheBootCommitFromVersionInfo(t *testing.T) {
	commit := "0fbe50a9d88aa7e1c0b5f3d2e4a6b8c9d0e1f2a3"
	for _, want := range []*string{&commit, nil} {
		app := dashboardTestApp(t)
		saved := statusVersionInfo
		distance, branch := int64(0), "main"
		statusVersionInfo = func() (updaterecord.VersionInfo, error) {
			return updaterecord.VersionInfo{BaseVersion: "2026.10.1", DerivedVersion: "2026.10.1", Distance: &distance,
				Commit: want, Branch: &branch, Source: "git"}, nil
		}
		r := newTestClient(t, app).get("/api/health")
		statusVersionInfo = saved
		if r.Status != 200 {
			t.Fatalf("status %d %s", r.Status, r.Body)
		}
		body := jsonObject(t, r)
		got, present := body["commit"]
		if body["ok"] != true || !present {
			t.Fatalf("body %s", r.Body)
		}
		if (want == nil && got != nil) || (want != nil && got != *want) {
			t.Fatalf("commit %v, want %v", got, want)
		}
	}
}

func TestStatusConfigStampReadRunsOffTheEventLoop(t *testing.T) {
	c := newSessionClient(t)
	stop := errors.New("stop")
	seen := 0
	saved := statusCheckConfigVersion
	statusCheckConfigVersion = func(context.Context, string, bool) (int, int, error) {
		seen++
		return 0, 0, stop
	}
	t.Cleanup(func() { statusCheckConfigVersion = saved })
	r := c.get("/api/status")
	if seen == 0 {
		t.Fatal("the config stamp was never read")
	}
	if !errors.Is(r.AppErr, stop) && r.Status != 500 {
		t.Fatalf("the read failure did not end the probe: %d %s %v", r.Status, r.Body, r.AppErr)
	}
}

// Pins the session token and the SSH owner state for one case
func pinSSHOwnerState(t *testing.T, token, nonce string) {
	t.Helper()
	savedToken := SessionToken()
	webStateMu.Lock()
	savedNonce, savedMarker, savedPurelib := sshOwnerNonce, sshRuntimeMarker, sshRuntimePurelib
	sshOwnerNonce, sshRuntimeMarker, sshRuntimePurelib = nonce, "", nil
	webStateMu.Unlock()
	if token != "" {
		setSessionToken(token)
	}
	t.Cleanup(func() {
		setSessionToken(savedToken)
		webStateMu.Lock()
		sshOwnerNonce, sshRuntimeMarker, sshRuntimePurelib = savedNonce, savedMarker, savedPurelib
		webStateMu.Unlock()
	})
}

func TestSSHOwnershipEndpointRequiresTokenAndReturnsExactNonce(t *testing.T) {
	token, nonce := strings.Repeat("t", 64), "0123456789abcdef"
	app := dashboardTestApp(t)
	pinSSHOwnerState(t, token, nonce)
	app.State.Set("auth_required", false)
	client := newTestClient(t, app)
	if r := client.get("/api/ssh/ownership"); r.Status != 401 {
		t.Fatalf("without a token %d %s", r.Status, r.Body)
	}
	client.get("/api/ssh/ownership", sessionHeaderName, token).expect(t, 200,
		`{"ok":true,"sshOwnerNonce":"0123456789abcdef","protocolVersion":1,"runtimeIntact":true}`)
}

func TestSSHOwnershipReportsReplacedRuntime(t *testing.T) {
	token := strings.Repeat("t", 64)
	app := dashboardTestApp(t)
	pinSSHOwnerState(t, token, "0123456789abcdef")
	purelib := filepath.Join(t.TempDir(), "site-packages")
	if err := os.Mkdir(purelib, 0o755); err != nil {
		t.Fatal(err)
	}
	dev, ino, err := pyfs.LstatIdentity(purelib)
	if err != nil {
		t.Fatal(err)
	}
	webStateMu.Lock()
	sshRuntimePurelib = &sshRuntimeIdentity{path: purelib, dev: dev, ino: ino + 1}
	webStateMu.Unlock()
	r := newTestClient(t, app).get("/api/ssh/ownership", sessionHeaderName, token)
	if r.Status != 200 || jsonObject(t, r)["runtimeIntact"] != false {
		t.Fatalf("answer %d %s", r.Status, r.Body)
	}
}

func venvPurelib(t *testing.T) (string, string) {
	t.Helper()
	venv := filepath.Join(t.TempDir(), "venv")
	purelib := filepath.Join(venv, "lib", "site-packages")
	if err := os.MkdirAll(purelib, 0o755); err != nil {
		t.Fatal(err)
	}
	return venv, purelib
}

func TestSSHRuntimeMarkerDetectsRecreatedVenvEvenWithReusedInode(t *testing.T) {
	pinSSHOwnerState(t, "", "")
	venv, purelib := venvPurelib(t)
	applySSHOwnerNonceAt("0123456789abcdef", purelib, nil)
	defer applySSHOwnerNonceAt("", purelib, nil)
	if !sshRuntimeIntact() {
		t.Fatal("fresh runtime reads as replaced")
	}
	if err := os.RemoveAll(venv); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(purelib, 0o755); err != nil {
		t.Fatal(err)
	}
	if sshRuntimeIntact() {
		t.Fatal("marker tier must catch a recreated venv regardless of inode reuse")
	}
}

func TestSSHRuntimeMarkerSurvivesInPlaceInstalls(t *testing.T) {
	pinSSHOwnerState(t, "", "")
	_, purelib := venvPurelib(t)
	applySSHOwnerNonceAt("0123456789abcdef", purelib, nil)
	defer applySSHOwnerNonceAt("", purelib, nil)
	if err := os.Mkdir(filepath.Join(purelib, "newpkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !sshRuntimeIntact() {
		t.Fatal("an in place install read as a replacement")
	}
}

func TestSSHRuntimeReadonlyPurelibFallsBackToStat(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	if os.Geteuid() == 0 {
		t.Skip("directory write bits are not enforced for root")
	}
	pinSSHOwnerState(t, "", "")
	_, purelib := venvPurelib(t)
	if err := os.Chmod(purelib, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(purelib, 0o755)
	applySSHOwnerNonceAt("0123456789abcdef", purelib, nil)
	defer applySSHOwnerNonceAt("", purelib, nil)
	webStateMu.RLock()
	marker, ident := sshRuntimeMarker, sshRuntimePurelib
	webStateMu.RUnlock()
	if marker != "" || ident == nil || !sshRuntimeIntact() {
		t.Fatalf("marker %q identity %v intact %v", marker, ident, sshRuntimeIntact())
	}
}

func TestSSHOwnershipEndpointIsAbsentWithoutOwnerNonce(t *testing.T) {
	token := strings.Repeat("t", 64)
	app := dashboardTestApp(t)
	pinSSHOwnerState(t, token, "")
	app.State.Set("auth_required", false)
	if r := newTestClient(t, app).get("/api/ssh/ownership", sessionHeaderName, token); r.Status != 404 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
}

func isoAge(secondsAgo float64) string {
	return time.Now().UTC().Add(-time.Duration(secondsAgo * float64(time.Second))).Format("2006-01-02T15:04:05.000000+00:00")
}

// Replaces the pid probe and runtime record of the status route for one case
func pinGatewayRuntime(t *testing.T, pid int, running bool, record *pyjson.Object) {
	t.Helper()
	savedProbe, savedRead := statusPidProbe, statusReadRuntimeStatus
	statusPidProbe = func(string) (int, bool, error) { return pid, running, nil }
	statusReadRuntimeStatus = func(string) *pyjson.Object { return record }
	t.Cleanup(func() { statusPidProbe, statusReadRuntimeStatus = savedProbe, savedRead })
}

func TestStatusReportsStaleHeartbeatWhenPidAlive(t *testing.T) {
	c := newSessionClient(t)
	record := pyjson.ObjectOf("gateway_state", "running", "pid", int64(1234), "start_time", 111.0,
		"updated_at", isoAge(900), "platforms", pyjson.NewObject(), "active_agents", int64(0))
	pinGatewayRuntime(t, 1234, true, record)
	data := jsonObject(t, c.get("/api/status"))
	if data["gateway_running"] != true || data["gateway_state"] != "running" {
		t.Fatalf("status %v", data)
	}
	stale, ok := data["gateway_heartbeat_stale_s"].(int64)
	if !ok || stale < 900 || stale > 930 {
		t.Fatalf("stale seconds %v", data["gateway_heartbeat_stale_s"])
	}
	record.Set("updated_at", isoAge(5))
	if got := jsonObject(t, c.get("/api/status"))["gateway_heartbeat_stale_s"]; got != nil {
		t.Fatalf("fresh heartbeat reported stale %v", got)
	}
}

func TestStatusKeepsWatchdogDegradedVerdictAndReasonForDeadPid(t *testing.T) {
	c := newSessionClient(t)
	record := pyjson.ObjectOf("gateway_state", "degraded", "exit_reason", "loop_liveness_watchdog",
		"pid", int64(999_999_999), "start_time", 1.0, "updated_at", isoAge(30), "platforms", pyjson.NewObject())
	pinGatewayRuntime(t, 0, false, record)
	data := jsonObject(t, c.get("/api/status"))
	if data["gateway_running"] != false || data["gateway_state"] != "degraded" ||
		data["gateway_exit_reason"] != "loop_liveness_watchdog" || data["gateway_heartbeat_stale_s"] != nil {
		t.Fatalf("status %v", data)
	}
	record.Set("desired_state", "stopped")
	data = jsonObject(t, c.get("/api/status"))
	if data["gateway_state"] != "stopped" || data["gateway_exit_reason"] != nil {
		t.Fatalf("status after the operator stop %v", data)
	}
}

func TestGetLogsYieldsWhileReadingAndFiltering(t *testing.T) {
	c := newSessionClient(t)
	writeTestFile(t, filepath.Join(os.Getenv("ICEBATH_HOME"), "logs", "agent.log"), "fixture\n")
	saved := StatusCLI.ReadLogTail
	StatusCLI.ReadLogTail = func(string, int, bool, *string, []string, bool) ([]string, error) { return []string{"fixture"}, nil }
	t.Cleanup(func() { StatusCLI.ReadLogTail = saved })
	c.get("/api/logs?file=agent&lines=100").expect(t, 200, `{"file":"agent","lines":["fixture"]}`)
}

// Client of a gated listener with the fake provider registered, or of a loopback listener with none
func authStatusClient(t *testing.T, gated bool) *testClient {
	t.Helper()
	app := dashboardTestApp(t)
	gateway.ClearDashboardAuthProviders()
	t.Cleanup(gateway.ClearDashboardAuthProviders)
	if !gated {
		app.State.Set("bound_host", "127.0.0.1")
		app.State.Set("bound_port", int64(8080))
		app.State.Set("auth_required", false)
		return newTestClientAt(t, app, "http", "127.0.0.1", 8080)
	}
	if err := gateway.RegisterGlobalDashboardAuthProvider(fakeIDPProvider{}); err != nil {
		t.Fatal(err)
	}
	app.State.Set("bound_host", "fly-app.fly.dev")
	app.State.Set("bound_port", int64(443))
	app.State.Set("auth_required", true)
	return newTestClientAt(t, app, "https", "fly-app.fly.dev", 443)
}

func TestStatusReportsAuthRequiredInGatedMode(t *testing.T) {
	r := authStatusClient(t, true).get("/api/status")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	body := jsonObject(t, r)
	if body["auth_required"] != true || !slices.Equal(body["auth_providers"].([]any), []any{"stub"}) {
		t.Fatalf("body %s", r.Body)
	}
}

func TestStatusWithholdsHostDetailInGatedMode(t *testing.T) {
	r := authStatusClient(t, true).get("/api/status")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	body := jsonObject(t, r)
	for _, key := range []string{"version", "gateway_state", "auth_required", "auth_providers"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("liveness field %q must stay public", key)
		}
	}
	for _, key := range []string{"hermes_home", "config_path", "env_path", "gateway_pid", "gateway_health_url"} {
		if _, leaked := body[key]; leaked {
			t.Fatalf("/api/status leaked %q under the gate", key)
		}
	}
}

// Replaces the debug share upload with one that reports the redaction it was asked for, or fails
func pinDebugShare(t *testing.T, fail error) *[]int {
	t.Helper()
	var lines []int
	saved := StatusCLI.BuildDebugShare
	StatusCLI.BuildDebugShare = func(_ context.Context, logLines int, redact bool) (*pyjson.Object, error) {
		lines = append(lines, logLines)
		if fail != nil {
			return nil, fail
		}
		return pyjson.ObjectOf("urls", pyjson.ObjectOf("Report", "https://paste.rs/x"), "failures", []any{}, "redacted", redact,
			"auto_delete_seconds", int64(3600)), nil
	}
	t.Cleanup(func() { StatusCLI.BuildDebugShare = saved })
	return &lines
}

func TestRedactFalseIsHonored(t *testing.T) {
	c := newSessionClient(t)
	pinDebugShare(t, nil)
	r := c.send("POST", "/api/ops/debug-share", map[string]any{"redact": false})
	if r.Status != 200 || jsonObject(t, r)["redacted"] != false {
		t.Fatalf("answer %d %s", r.Status, r.Body)
	}
}

func TestDefaultBodyRedacts(t *testing.T) {
	c := newSessionClient(t)
	lines := pinDebugShare(t, nil)
	r := c.send("POST", "/api/ops/debug-share", nil)
	if r.Status != 200 || jsonObject(t, r)["redacted"] != true {
		t.Fatalf("answer %d %s", r.Status, r.Body)
	}
	if !slices.Equal(*lines, []int{200}) {
		t.Fatalf("log lines %v", *lines)
	}
}

func TestUploadFailureReturns502(t *testing.T) {
	c := newSessionClient(t)
	pinDebugShare(t, &pm.PyError{Class: "RuntimeError", Message: "down"})
	r := c.send("POST", "/api/ops/debug-share", map[string]any{"redact": true})
	if r.Status != 502 {
		t.Fatalf("answer %d %s", r.Status, r.Body)
	}
}

// Points the status route at a remote gateway health URL whose probe answers as given
func pinGatewayHealth(t *testing.T, url string, alive bool, body *pyjson.Object) {
	t.Helper()
	webStateMu.Lock()
	savedURL, savedSet := gatewayHealthURL, gatewayHealthURLSet
	gatewayHealthURL, gatewayHealthURLSet = url, true
	webStateMu.Unlock()
	savedProbe := statusProbeGatewayHealth
	statusProbeGatewayHealth = func(context.Context) (bool, any) { return alive, body }
	t.Cleanup(func() {
		statusProbeGatewayHealth = savedProbe
		webStateMu.Lock()
		gatewayHealthURL, gatewayHealthURLSet = savedURL, savedSet
		webStateMu.Unlock()
	})
}

func TestStatusFallsBackToRemoteProbe(t *testing.T) {
	c := newSessionClient(t)
	pinGatewayRuntime(t, 0, false, nil)
	pinGatewayHealth(t, "http://gw:8642", true, pyjson.ObjectOf("status", "ok", "gateway_state", "running",
		"platforms", pyjson.ObjectOf("telegram", pyjson.ObjectOf("state", "connected")), "pid", int64(999)))
	r := c.get("/api/status")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	if data["gateway_running"] != true || data["gateway_pid"] != int64(999) || data["gateway_state"] != "running" ||
		data["gateway_health_url"] != "http://gw:8642" {
		t.Fatalf("status %s", r.Body)
	}
}

func TestStatusRemoteRunningNullPid(t *testing.T) {
	c := newSessionClient(t)
	pinGatewayRuntime(t, 0, false, nil)
	pinGatewayHealth(t, "http://gw:8642", true, pyjson.ObjectOf("status", "ok"))
	r := c.get("/api/status")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	if data["gateway_running"] != true || data["gateway_pid"] != nil || data["gateway_state"] != "running" {
		t.Fatalf("status %s", r.Body)
	}
}

// Empties the process cache of the install id for one case
func resetInstallIDCache(t *testing.T) {
	t.Helper()
	saved := installIDCache
	installIDCache = &installidentity.Cache{}
	t.Cleanup(func() { installIDCache = saved })
}

var installIDRE = regexp.MustCompile(`\A[0-9a-f]{32}\z`)

func TestStatusReportsPersistentInstallID(t *testing.T) {
	c := newSessionClient(t)
	resetInstallIDCache(t)
	pinGatewayRuntime(t, 0, false, nil)
	first := c.get("/api/status")
	if first.Status != 200 {
		t.Fatalf("status %d %s", first.Status, first.Body)
	}
	installID, _ := jsonObject(t, first)["install_id"].(string)
	if !installIDRE.MatchString(installID) {
		t.Fatalf("install id %q", installID)
	}
	raw, err := os.ReadFile(filepath.Join(home.Root(), "install_id"))
	if err != nil || strings.TrimSpace(string(raw)) != installID {
		t.Fatalf("id file %q %v", raw, err)
	}
	if again := jsonObject(t, c.get("/api/status"))["install_id"]; again != installID {
		t.Fatalf("second read %v", again)
	}
}

func TestInstallIDSurvivesProcessCacheReset(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetInstallIDCache(t)
	ctx := context.Background()
	first, err := getInstallID(ctx)
	if err != nil || first == "" {
		t.Fatalf("first id %q %v", first, err)
	}
	installIDCache = &installidentity.Cache{}
	if again, err := getInstallID(ctx); err != nil || again != first {
		t.Fatalf("after a cache reset %q %v, want %q", again, err, first)
	}
}

func TestAllProfilesOfOneInstallShareTheID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "icebath-root")
	profileHome := filepath.Join(root, "profiles", "research")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	resetInstallIDCache(t)
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", root)
	rootID, err := getInstallID(ctx)
	if err != nil || rootID == "" {
		t.Fatalf("root id %q %v", rootID, err)
	}
	t.Setenv("ICEBATH_HOME", profileHome)
	installIDCache = &installidentity.Cache{}
	if profileID, err := getInstallID(ctx); err != nil || profileID != rootID {
		t.Fatalf("profile id %q %v, want %q", profileID, err, rootID)
	}
	if st, err := os.Stat(filepath.Join(root, "install_id")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("root id file %v", err)
	}
	if _, err := os.Stat(filepath.Join(profileHome, "install_id")); !os.IsNotExist(err) {
		t.Fatalf("profile home got its own id file: %v", err)
	}
}

func TestCorruptIDFileIsReplacedNotPropagated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "icebath-root")
	writeTestFile(t, filepath.Join(root, "install_id"), "not-a-valid-id\n")
	t.Setenv("ICEBATH_HOME", root)
	resetInstallIDCache(t)
	value, err := getInstallID(context.Background())
	if err != nil || !installIDRE.MatchString(value) {
		t.Fatalf("id %q %v", value, err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, "install_id")); err != nil || strings.TrimSpace(string(raw)) != value {
		t.Fatalf("id file %q %v", raw, err)
	}
}

func TestDrainingStateIsNeitherBusyNorDrainable(t *testing.T) {
	c := newSessionClient(t)
	pinGatewayRuntime(t, 1234, true, pyjson.ObjectOf("gateway_state", "draining", "platforms", pyjson.NewObject(), "active_agents", int64(3)))
	data := jsonObject(t, c.get("/api/status"))
	if data["gateway_busy"] != false || data["gateway_drainable"] != false {
		t.Fatalf("status %v", data)
	}
}

func TestActiveAgentsUnparseableInFileDegradesToZero(t *testing.T) {
	c := newSessionClient(t)
	pinGatewayRuntime(t, 1234, true, pyjson.ObjectOf("gateway_state", "running", "platforms", pyjson.NewObject(), "active_agents", "garbage"))
	data := jsonObject(t, c.get("/api/status"))
	if data["active_agents"] != int64(0) || data["gateway_busy"] != false {
		t.Fatalf("status %v", data)
	}
}

func collectorExploded(context.Context, string) (*pyjson.Object, error) {
	return nil, &pm.PyError{Class: "RuntimeError", Message: "collector exploded"}
}

func TestMemoryBlockDegradesWhenCollectorRaises(t *testing.T) {
	c := newSessionClient(t)
	saved := statusCollectMemory
	statusCollectMemory = collectorExploded
	t.Cleanup(func() { statusCollectMemory = saved })
	r := c.get("/api/status")
	memory, _ := jsonObject(t, r)["memory"].(map[string]any)
	if r.Status != 200 || len(memory) != 1 || memory["pressure"] != "unknown" {
		t.Fatalf("answer %d memory %v", r.Status, memory)
	}
}

func TestDiskBlockDegradesWhenCollectorRaises(t *testing.T) {
	c := newSessionClient(t)
	saved := statusCollectDisk
	statusCollectDisk = collectorExploded
	t.Cleanup(func() { statusCollectDisk = saved })
	r := c.get("/api/status")
	disk, _ := jsonObject(t, r)["disk"].(map[string]any)
	if r.Status != 200 || len(disk) != 1 || disk["pressure"] != "unknown" {
		t.Fatalf("answer %d disk %v", r.Status, disk)
	}
}

func TestLocalRuntimeValidEpochBecomesISOString(t *testing.T) {
	c := newSessionClient(t)
	pinGatewayRuntime(t, 1234, true, pyjson.ObjectOf("gateway_state", "running", "platforms", pyjson.NewObject(),
		"active_agents", int64(0), "updated_at", int64(1750000000)))
	value, ok := jsonObject(t, c.get("/api/status"))["gateway_updated_at"].(string)
	if !ok {
		t.Fatal("gateway_updated_at is not a string")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !parsed.Equal(time.Unix(1750000000, 0)) {
		t.Fatalf("updated at %q %v", value, err)
	}
}

func TestRemoteHealthNumericUpdatedAtNormalized(t *testing.T) {
	c := newSessionClient(t)
	pinGatewayRuntime(t, 0, false, nil)
	pinGatewayHealth(t, "http://gw:8642", true, pyjson.ObjectOf("status", "ok", "gateway_state", "running",
		"platforms", pyjson.NewObject(), "updated_at", 1750000000.25, "pid", int64(999)))
	r := c.get("/api/status")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	value, ok := data["gateway_updated_at"].(string)
	if data["gateway_running"] != true || !ok {
		t.Fatalf("status %s", r.Body)
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		t.Fatalf("updated at %q is no zoned timestamp: %v", value, err)
	}
}

func TestPublicComponentPayloadCarriesNoSecretBearingFields(t *testing.T) {
	c := newSessionClient(t)
	saved := DashboardHealthState
	DashboardHealthState = NewDashboardHealth(dashboardHealthWindowSeconds)
	t.Cleanup(func() { DashboardHealthState = saved })
	DashboardHealthState.RecordError("RuntimeError", "/api/secret-route?token=abc")
	components, err := json.Marshal(jsonObject(t, c.get("/api/status"))["components"])
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"secret-route", "token=abc", "last_error_path", "last_error_type", "kaboom"} {
		if strings.Contains(string(components), banned) {
			t.Fatalf("components %s carry %q", components, banned)
		}
	}
}
