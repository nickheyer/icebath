package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// Replaces one setup seam for the test
func patchSetup[T any](t *testing.T, seam *T, v T) {
	t.Helper()
	old := *seam
	*seam = v
	t.Cleanup(func() { *seam = old })
}

// Host probes of the setup flow as the gateway module patches set them, the home a throwaway directory
func patchServiceHost(t *testing.T, container, systemd, macos, windows bool) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	patchSetup(t, &setupIsContainer, func() bool { return container })
	patchSetup(t, &setupSupportsSystemd, func(context.Context) bool { return systemd })
	patchSetup(t, &setupIsMacOS, func() bool { return macos })
	patchSetup(t, &setupIsWindows, func() bool { return windows })
}

// Records install and start calls of the systemd backend
func recordSystemdCalls(t *testing.T) *[]string {
	calls := &[]string{}
	patchSetup(t, &setupSystemdInstall, func(context.Context, io.Writer, bool, bool, string, bool, bool, bool) error {
		*calls = append(*calls, "install")
		return nil
	})
	patchSetup(t, &setupSystemdStart, func(context.Context, bool) error {
		*calls = append(*calls, "start")
		return nil
	})
	return calls
}

func ensureService(t *testing.T) (bool, string) {
	t.Helper()
	var out bytes.Buffer
	ok, err := EnsureGatewayService(context.Background(), &out, ServiceSetupOptions{Context: "setup", Install: true})
	if err != nil {
		t.Fatal(err)
	}
	return ok, out.String()
}

func stoppedAndInstalled(t *testing.T, installed bool) {
	patchSetup(t, &setupServiceRunning, func(context.Context) (bool, error) { return false, nil })
	patchSetup(t, &setupServiceInstalled, func(context.Context) (bool, error) { return installed, nil })
}

func TestEnsureGatewayServiceContainerIsNoop(t *testing.T) {
	patchServiceHost(t, true, true, false, false)
	calls := recordSystemdCalls(t)
	ok, out := ensureService(t)
	if ok || len(*calls) != 0 || !strings.Contains(out, "restart policy") {
		t.Fatalf("%v %v %q", ok, *calls, out)
	}
}

func TestEnsureGatewayServiceNoServiceManagerIsNoop(t *testing.T) {
	patchServiceHost(t, false, false, false, false)
	ok, out := ensureService(t)
	if ok || !strings.Contains(out, "icebath gateway") {
		t.Fatalf("%v %q", ok, out)
	}
}

func TestEnsureGatewayServiceAlreadyRunningShortCircuits(t *testing.T) {
	patchServiceHost(t, false, true, false, false)
	patchSetup(t, &setupServiceRunning, func(context.Context) (bool, error) { return true, nil })
	calls := recordSystemdCalls(t)
	if ok, _ := ensureService(t); !ok || len(*calls) != 0 {
		t.Fatalf("%v %v", ok, *calls)
	}
}

func TestEnsureGatewayServiceInstalledButStoppedStartsWithoutReinstall(t *testing.T) {
	patchServiceHost(t, false, true, false, false)
	stoppedAndInstalled(t, true)
	calls := recordSystemdCalls(t)
	if ok, out := ensureService(t); !ok || strings.Join(*calls, ",") != "start" {
		t.Fatalf("%v %v %q", ok, *calls, out)
	}
}

func TestEnsureGatewayServiceConflictingUnitsBlockInstall(t *testing.T) {
	patchServiceHost(t, false, true, false, false)
	stoppedAndInstalled(t, false)
	patchSetup(t, &setupConflictingUnits, func(context.Context) (bool, error) { return true, nil })
	warned := false
	patchSetup(t, &setupConflictWarning, func(context.Context, io.Writer) error { warned = true; return nil })
	calls := recordSystemdCalls(t)
	if ok, out := ensureService(t); ok || !warned || len(*calls) != 0 {
		t.Fatalf("%v %v %v %q", ok, warned, *calls, out)
	}
}

func TestEnsureGatewayServiceNeverRaisesOnInstallFailure(t *testing.T) {
	patchServiceHost(t, false, true, false, false)
	stoppedAndInstalled(t, false)
	patchSetup(t, &setupConflictingUnits, func(context.Context) (bool, error) { return false, nil })
	patchSetup(t, &setupSystemdInstall, func(context.Context, io.Writer, bool, bool, string, bool, bool, bool) error {
		return errors.New("dbus fell over")
	})
	if ok, out := ensureService(t); ok || !strings.Contains(out, "icebath gateway install") {
		t.Fatalf("%v %q", ok, out)
	}
}

func TestEnsureGatewayServiceNeverRaisesOnSysExit(t *testing.T) {
	patchServiceHost(t, false, true, false, false)
	stoppedAndInstalled(t, false)
	patchSetup(t, &setupConflictingUnits, func(context.Context) (bool, error) { return false, nil })
	patchSetup(t, &setupSystemdInstall, func(context.Context, io.Writer, bool, bool, string, bool, bool, bool) error {
		return &SystemExitError{Code: 1}
	})
	if ok, out := ensureService(t); ok || !strings.Contains(out, "icebath gateway install") {
		t.Fatalf("%v %q", ok, out)
	}
}

func TestEnsureGatewayServiceUserSystemdUnreachableReportsRemediation(t *testing.T) {
	patchServiceHost(t, false, true, false, false)
	stoppedAndInstalled(t, true)
	patchSetup(t, &setupSystemdStart, func(context.Context, bool) error {
		return &UserSystemdUnavailableError{Msg: "no D-Bus session\nenable linger"}
	})
	if ok, out := ensureService(t); ok || !strings.Contains(out, "enable linger") {
		t.Fatalf("%v %q", ok, out)
	}
}
