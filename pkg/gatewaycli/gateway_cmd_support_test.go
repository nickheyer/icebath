package gatewaycli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Fresh profile home with the managed dir pointed away from the host
func supportHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_MANAGED_DIR", filepath.Join(h, "no-managed"))
	return h
}

func TestRestartRefusesOnThePublishedActiveAgentsOfAnOlderGateway(t *testing.T) {
	supportHome(t)
	timeout := 5 * time.Second
	if _, err := gateway.WriteRuntimeStatus(context.Background(), gateway.RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "running", "active_agents": 1}}, false, &timeout); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := RefuseWhileSessionsLive(context.Background(), &out, "restart", map[string]any{"all": false, "system": false, "force": false, "interrupt": false}, "", "")
	var exit *gateway.SystemExitError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
	if !strings.Contains(out.String(), "1 active agent run(s)") {
		t.Fatalf("output %q", out.String())
	}
}

func TestInterruptFlagProceedsAndNamesTheInterruptedWork(t *testing.T) {
	supportHome(t)
	timeout := 5 * time.Second
	if _, err := gateway.WriteRuntimeStatus(context.Background(), gateway.RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "running", "active_agents": 2}}, false, &timeout); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RefuseWhileSessionsLive(context.Background(), &out, "stop", map[string]any{"interrupt": true}, "", ""); err != nil {
		t.Fatalf("err=%v", err)
	}
	text := out.String()
	if !strings.Contains(text, "--yes: stop proceeds: 2 live session(s) would be interrupted.") || !strings.Contains(text, "2 active agent run(s)") {
		t.Fatalf("output %q", text)
	}
}

func TestNoLiveWorkLetsTheVerbProceedSilently(t *testing.T) {
	supportHome(t)
	var out bytes.Buffer
	if err := RefuseWhileSessionsLive(context.Background(), &out, "stop", map[string]any{}, "", ""); err != nil || out.Len() != 0 {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
}

func TestExplainServiceFailureCoversSystemctlFailures(t *testing.T) {
	lines := ExplainServiceFailure(gateway.SystemctlUnavailableError{})
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "This system has no systemd") {
		t.Fatalf("lines %q", lines)
	}
	failed := &gateway.CalledProcessError{Cmd: []string{"systemctl", "--user", "restart", "icebath-gateway"}, ReturnCode: 1}
	lines = ExplainServiceFailure(failed)
	if len(lines) != 4 || lines[0] != "Could not restart the gateway service; systemd reported an error." ||
		lines[3] != "Details: Command '['systemctl', '--user', 'restart', 'icebath-gateway']' returned non-zero exit status 1." {
		t.Fatalf("lines %q", lines)
	}
	if lines := ExplainServiceFailure(&pysubprocess.CalledProcessError{Cmd: []string{"journalctl"}, Returncode: 2}); lines[0] != "Could not start the gateway service; systemd reported an error." {
		t.Fatalf("default verb %q", lines[0])
	}
	if ExplainServiceFailure(errors.New("other")) != nil {
		t.Fatal("an unknown failure must not be explained")
	}
}

func TestGatewayCommandExitTurnsFailuresIntoGuidance(t *testing.T) {
	var out bytes.Buffer
	if code, err := gatewayCommandExit(&out, &gateway.SystemExitError{Code: 78}); code != 78 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	out.Reset()
	if code, err := gatewayCommandExit(&out, &gateway.CalledProcessError{Cmd: []string{"systemctl", "stop", "x"}, ReturnCode: 1}); code != 1 || err != nil ||
		!strings.Contains(out.String(), "Could not stop the gateway service") || !strings.Contains(out.String(), "  See why with") {
		t.Fatalf("code=%d err=%v out=%q", code, err, out.String())
	}
	out.Reset()
	if code, err := gatewayCommandExit(&out, &gateway.SystemScopeRequiresRootError{Msg: "needs root"}); code != 1 || err != nil || out.String() != "needs root\n" {
		t.Fatalf("code=%d err=%v out=%q", code, err, out.String())
	}
	boom := errors.New("boom")
	if code, err := gatewayCommandExit(&out, boom); code != 1 || err != boom {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestEnsureExitCodeMapsReasonsThenStates(t *testing.T) {
	cases := []struct {
		state, reason string
		want          int
	}{
		{"inaccessible", "gateway_other_session", 4}, {"inaccessible", "gateway_elevated", 4}, {"inaccessible", "authorization", 4},
		{"starting", "deadline", 5}, {"ready", "", 0}, {"incompatible", "runtime_protocol", 3}, {"draining", "", 6},
		{"inaccessible", "", 7}, {"conflict", "", 7}, {"starting", "", 5}, {"absent", "", 5},
	}
	for _, tc := range cases {
		if got := EnsureExitCode(gateway.ObservedEndpoint{State: tc.state, ReasonCode: tc.reason}); got != tc.want {
			t.Fatalf("%s/%s: %d want %d", tc.state, tc.reason, got, tc.want)
		}
	}
}

func TestEnsureRejectsAnInvalidTimeoutWithTheProtocolLine(t *testing.T) {
	supportHome(t)
	for _, raw := range []string{"soon", "0", "-1", "nan", "inf"} {
		var out bytes.Buffer
		code, err := CmdGatewayEnsure(context.Background(), map[string]any{"timeout": raw}, &out)
		if err != nil || code != 2 || out.String() != `{"endpoint":null,"reason_code":"invalid_invocation","state":"inaccessible"}`+"\n" {
			t.Fatalf("%s: code=%d err=%v out=%q", raw, code, err, out.String())
		}
	}
}

func TestNoBackendExitPrintsTheTableAndItsCode(t *testing.T) {
	var out bytes.Buffer
	err := noBackendExit(&out, "install", "container")
	var exit *gateway.SystemExitError
	if !errors.As(err, &exit) || exit.Code != 0 || !strings.HasPrefix(out.String(), "Service installation is not needed inside a Docker container.") {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
	out.Reset()
	if err := noBackendExit(&out, "uninstall", "s6"); err != nil || !strings.Contains(out.String(), "icebath profile delete <name>") {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
}

func TestStatusNotesStaySilentWithoutAnIncompatibleOwner(t *testing.T) {
	supportHome(t)
	var out bytes.Buffer
	if ReportIncompatibleOwner(context.Background(), &out, []int{os.Getpid()}) || out.Len() != 0 {
		t.Fatalf("output %q", out.String())
	}
	if err := PrintStandaloneByConfig(context.Background(), &out); err != nil || out.Len() != 0 {
		t.Fatalf("err=%v output %q", err, out.String())
	}
}
