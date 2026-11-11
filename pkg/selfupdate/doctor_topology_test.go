package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// A host gateway owned by this process serving default and served, with served as the active profile
func servedHost(t *testing.T) (*Doctor, *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "icebath")
	servedHome := filepath.Join(root, "profiles", "served")
	locks := filepath.Join(tmp, "locks")
	for _, dir := range []string{servedHome, locks} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", servedHome)
	t.Setenv("ICEBATH_PROFILE", "served")
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", locks)
	t.Setenv("HOME", tmp)
	t.Cleanup(home.PatchUserHome(tmp))
	profiles.ResetFileCache()
	profiles.ResetStandaloneMemo()
	home.ResetKeyCache()
	ctx := context.Background()
	outcome, err := gateway.ClaimHostLock(ctx, gateway.RoleGateway)
	if err != nil || outcome != gateway.HostLockAcquired {
		t.Fatalf("host lock outcome = %q, err = %v", outcome, err)
	}
	t.Cleanup(func() { gateway.ReleaseHostLock(gateway.RoleGateway) })
	if record := gateway.PublishHostRecord(ctx, gateway.RoleGateway, gateway.HostPublishOptions{Profiles: []string{"default", "served"}}); record == nil {
		t.Fatal("host record was not published")
	}
	out := &bytes.Buffer{}
	return &Doctor{W: out, Home: servedHome, ProjectRoot: t.TempDir(), DisplayHome: servedHome}, out
}

// Supervision stand in answering a fixed set of running services and profile slots
type fakeS6Manager struct {
	running map[string]bool
	slots   []string
}

func (m fakeS6Manager) IsRunning(name string) (bool, error)    { return m.running[name], nil }
func (m fakeS6Manager) ListProfileGateways() ([]string, error) { return m.slots, nil }

func TestDoctorReportsTheSingleHostGatewayNotPerProfileSlots(t *testing.T) {
	d, out := servedHost(t)
	toolsSeam(t, &doctorDetectServiceManager, func() (string, error) { return "s6", nil })
	toolsSeam(t, &doctorNewS6Manager, func() doctorS6Manager {
		return fakeS6Manager{map[string]bool{"main-icebath": true, "gateway-legacy": true}, []string{"default", "served", "legacy"}}
	})
	issues := []string{}
	if err := doctorCheckS6Supervision(context.Background(), d, &issues); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "Per-profile gateways:") || !strings.Contains(text, fmt.Sprintf("PID %d", os.Getpid())) || !strings.Contains(text, "served") ||
		!strings.Contains(text, "LEGACY per-profile gateway slots still supervised: legacy") {
		t.Fatalf("output:\n%s", text)
	}
	found := false
	for _, issue := range issues {
		found = found || strings.Contains(issue, "migrate --multiplex")
	}
	if !found {
		t.Fatalf("issues = %q", issues)
	}
}

func TestDoctorRaisesAnIssueWhenNothingOwnsTheGatewayRole(t *testing.T) {
	d, out := servedHost(t)
	toolsSeam(t, &doctorHostGatewayTopology, func() (*gateway.HostGatewayTopology, error) { return nil, nil })
	toolsSeam(t, &doctorDetectServiceManager, func() (string, error) { return "s6", nil })
	toolsSeam(t, &doctorNewS6Manager, func() doctorS6Manager {
		return fakeS6Manager{map[string]bool{"main-icebath": true}, []string{"default", "served", "legacy"}}
	})
	issues := []string{}
	if err := doctorCheckS6Supervision(context.Background(), d, &issues); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No host gateway owns the gateway role") {
		t.Fatalf("output:\n%s", out.String())
	}
	found := false
	for _, issue := range issues {
		found = found || strings.Contains(issue, "gateway start")
	}
	if !found {
		t.Fatalf("the outage state produced no remediation: %q", issues)
	}
}

func TestDoctorChecksHostUnitLingerUnderAServedProfile(t *testing.T) {
	for _, xdg := range []bool{false, true} {
		t.Run(fmt.Sprint(xdg), func(t *testing.T) {
			d, out := servedHost(t)
			tmp := t.TempDir()
			fakeHome := filepath.Join(tmp, "fakehome")
			configHome := filepath.Join(fakeHome, ".config")
			if xdg {
				configHome = filepath.Join(tmp, "xdgconfig")
			}
			writeTestFile(t, filepath.Join(configHome, "systemd", "user", gateway.ServiceBase+".service"), "[Unit]\n", 0o644)
			t.Setenv("HOME", fakeHome)
			t.Cleanup(home.PatchUserHome(fakeHome))
			if xdg {
				t.Setenv("XDG_CONFIG_HOME", configHome)
			} else {
				t.Setenv("XDG_CONFIG_HOME", "")
				os.Unsetenv("XDG_CONFIG_HOME")
			}
			toolsSeam(t, &doctorIsLinux, func() bool { return true })
			toolsSeam(t, &doctorDetectServiceManager, func() (string, error) { return "systemd", nil })
			toolsSeam(t, &doctorSystemdLingerStatus, func(context.Context) (*bool, string) {
				enabled := false
				return &enabled, "off"
			})
			issues := []string{}
			if err := doctorCheckGatewayServiceLinger(context.Background(), d, &issues); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Systemd linger disabled") {
				t.Fatalf("output:\n%s", out.String())
			}
			found := false
			for _, issue := range issues {
				found = found || strings.Contains(issue, "enable-linger")
			}
			if !found {
				t.Fatalf("issues = %q", issues)
			}
		})
	}
}

func TestStateDbHolderLineNamesTheSharedHostGateway(t *testing.T) {
	servedHost(t)
	holders := 3
	rows := doctorRenderStateDBStats(map[string]any{"messages": int64(5)}, &holders, doctorHostGatewayNote())
	texts := []string{}
	for _, row := range rows {
		texts = append(texts, row.Text)
	}
	want := fmt.Sprintf("3 process(es) holding the DB open (the host gateway (PID %d) serving profiles default, served)", os.Getpid())
	if !strings.Contains(strings.Join(texts, " "), want) {
		t.Fatalf("rows = %q", texts)
	}
}

func TestReconcileWarnsWhenLegacyEntryCannotBeRemoved(t *testing.T) {
	d, _ := toolsDoctor(t)
	legacy := filepath.Join(t.TempDir(), "Icebath_Gateway_alice.cmd")
	toolsSeam(t, &doctorSysPlatform, "windows")
	toolsSeam(t, &doctorRedundantAutostartEntries, func(context.Context) ([]string, error) { return []string{legacy}, nil })
	toolsSeam(t, &doctorReconcileAutostartLaunchers, func(context.Context) ([]string, []string, error) {
		return []string{}, []string{"Could not remove " + legacy}, nil
	})
	f := &Finding{}
	if err := doctorCheckWindowsGatewayAutostart(context.Background(), d, true, f); err != nil {
		t.Fatal(err)
	}
	if f.Fixed != 0 || len(f.ManualIssues) != 1 {
		t.Fatalf("finding = %+v", f)
	}
}

func TestDoctorWriteProbeNeverTouchesAStoreALiveWriterHolds(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "state.db")
	conn, err := state.ConnectSQLite(stateDB, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "CREATE TABLE sessions (id TEXT)"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	var probed []string
	toolsSeam(t, &doctorDBOpensCleanly, func(_ context.Context, path string) (string, bool, error) {
		probed = append(probed, path)
		return "", false, nil
	})
	for _, held := range []bool{true, false} {
		toolsSeam(t, &doctorLiveWriterHoldsDB, func(context.Context, string) (bool, error) { return held, nil })
		if reason, unhealthy, err := doctorWriteHealthReason(context.Background(), &bytes.Buffer{}, stateDB, false); err != nil || unhealthy || reason != "" {
			t.Fatalf("held = %v: reason = %q, unhealthy = %v, err = %v", held, reason, unhealthy, err)
		}
	}
	if len(probed) != 2 || probed[0] == stateDB || filepath.Base(probed[0]) != "state.db" || probed[1] != stateDB {
		t.Fatalf("probed = %q", probed)
	}
}

func TestRecoveryCopyWithoutACatalogMemoryEntryKeepsTheGenericHint(t *testing.T) {
	d, out := toolsDoctor(t)
	ctx := context.Background()
	if hint := memory.CatalogInstallHint(ctx, "honcho", "memory"); hint != "icebath plugins install honcho" {
		t.Fatalf("memory hint = %q", hint)
	}
	if hint := memory.CatalogInstallHint(ctx, "honcho", "tools"); hint != "" {
		t.Fatalf("tools hint = %q", hint)
	}
	if hint := memory.CatalogInstallHint(ctx, "no-such-provider", ""); hint != "" {
		t.Fatalf("unknown provider hint = %q", hint)
	}
	doctorMemoryProviderGeneric(ctx, d, "no-such-provider")
	if !strings.Contains(out.String(), "run: icebath memory setup") {
		t.Fatalf("output:\n%s", out.String())
	}
}
