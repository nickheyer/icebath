package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Points Relay's user and system file lookups at empty directories and blanks every plugin variable, returning the home
func isolateRelayDemand(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv(RelayPluginsConfigEnv, "")
	for _, name := range LegacyRelayExportEnvVars {
		t.Setenv(name, "")
	}
	system := t.TempDir()
	prev := relaySystemConfigDir
	relaySystemConfigDir = func() string { return system }
	t.Cleanup(func() { relaySystemConfigDir = prev })
	return home
}

// Writes the plugins.toml Relay reads under dir and returns its path
func writeRelayPluginsFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "nemo-relay", "plugins.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version = 1\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRelayDiscoveredPluginFilesFollowRelaysUserAndSystemRules(t *testing.T) {
	home := isolateRelayDemand(t)
	if got := RelayDiscoveredPluginFiles(); len(got) != 0 {
		t.Fatalf("empty homes %v", got)
	}
	homeFile := writeRelayPluginsFile(t, filepath.Join(home, ".config"))
	if got := RelayDiscoveredPluginFiles(); !reflect.DeepEqual(got, []string{homeFile}) {
		t.Fatalf("home file %v", got)
	}
	xdg := t.TempDir()
	xdgFile := writeRelayPluginsFile(t, xdg)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got := RelayDiscoveredPluginFiles(); !reflect.DeepEqual(got, []string{xdgFile}) {
		t.Fatalf("absolute XDG_CONFIG_HOME %v", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if got := RelayDiscoveredPluginFiles(); !reflect.DeepEqual(got, []string{homeFile}) {
		t.Fatalf("relative XDG_CONFIG_HOME %v", got)
	}
	profile := t.TempDir()
	profileFile := writeRelayPluginsFile(t, filepath.Join(profile, ".config"))
	t.Setenv("USERPROFILE", profile)
	t.Setenv("HOME", "")
	if got := RelayDiscoveredPluginFiles(); len(got) != 0 {
		t.Fatalf("empty HOME %v", got)
	}
	os.Unsetenv("HOME")
	if got := RelayDiscoveredPluginFiles(); !reflect.DeepEqual(got, []string{profileFile}) {
		t.Fatalf("unset HOME %v", got)
	}
	systemFile := writeRelayPluginsFile(t, relaySystemConfigDir())
	if got := RelayDiscoveredPluginFiles(); !reflect.DeepEqual(got, []string{profileFile, systemFile}) {
		t.Fatalf("user and system files %v", got)
	}
}

func TestRelayDemandedByPluginFilesAndPluginVariables(t *testing.T) {
	home := isolateRelayDemand(t)
	ctx := context.Background()
	if RelayDemanded(ctx) {
		t.Fatal("demand without any consumer")
	}
	t.Setenv(RelayPluginsConfigEnv, "/nowhere/plugins.toml")
	if !RelayDemanded(ctx) {
		t.Fatal("explicit plugins file variable")
	}
	t.Setenv(RelayPluginsConfigEnv, " ")
	t.Setenv(LegacyRelayExportEnvVars[0], "1")
	if !RelayDemanded(ctx) {
		t.Fatal("legacy exporter variable")
	}
	t.Setenv(LegacyRelayExportEnvVars[0], "")
	file := writeRelayPluginsFile(t, filepath.Join(home, ".config"))
	if !RelayDemanded(ctx) {
		t.Fatal("discovered plugins file")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if RelayDemanded(ctx) {
		t.Fatal("removed plugins file")
	}
}

// Binding over a fake relay that fails with failure while the runtime is not ready, counting its loads
func relayProvisioningBinding(t *testing.T, failure error) (ready *bool, loads *int) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "profile"))
	f := newRelayFake(nil, nil)
	ready, loads = new(bool), new(int)
	RegisterNemoRelayBinding(func() (*NemoRelay, error) {
		*loads++
		if !*ready {
			return nil, failure
		}
		return f.relay, nil
	})
	RegisterNemoRelayReadiness(func() bool { return *ready })
	ResetRelayRuntimeForTests()
	t.Cleanup(func() {
		ResetRelayRuntimeForTests()
		RegisterNemoRelayBinding(nil)
	})
	return ready, loads
}

func TestTurnWithoutARelayConsumerPassesSilentlyWhileTheRuntimeIsUnprovisioned(t *testing.T) {
	isolateRelayDemand(t)
	ready, loads := relayProvisioningBinding(t, errors.New("the binding must not load before the runtime is provisioned"))
	logs := captureRegistryLog(t)
	ctx := context.Background()
	host, ok := RelayHosts.ForProfile(ctx, "", true).(NoopRelayRuntime)
	if !ok || !host.Retry || host.Reason != relayUnconfiguredReason || *loads != 0 {
		t.Fatalf("host %#v after %d loads", host, *loads)
	}
	if cached := RelayHosts.ForProfile(ctx, "", false); cached != nil {
		t.Fatalf("passing host was kept %#v", cached)
	}
	lease := RelayCoordinator.AcquireConversation(ctx, RelayConversation{ProfileKey: CurrentRelayProfileKey(ctx), SessionID: "s1", Platform: "cli"})
	if _, ok := lease.Host.(NoopRelayRuntime); !ok || lease.Session != nil || *loads != 0 {
		t.Fatalf("lease %#v after %d loads", lease, *loads)
	}
	RelayCoordinator.ReleaseConversation(lease)
	if got := logs.String(); strings.Contains(got, "Relay") || strings.Contains(got, "relay") {
		t.Fatalf("a session without a Relay consumer logged %q", got)
	}
	*ready = true
	if rt := GetRelayRuntime(ctx, true, ""); rt == nil || *loads != 1 {
		t.Fatalf("runtime %v after %d loads once provisioned", rt, *loads)
	}
}

func TestTurnWithARelayConsumerUsesTheRuntimeOnceItIsProvisioned(t *testing.T) {
	isolateRelayDemand(t)
	t.Setenv(LegacyRelayExportEnvVars[0], "1")
	failure := errors.New("the runtime environment /venv is not provisioned: download failed")
	ready, loads := relayProvisioningBinding(t, failure)
	logs := captureRegistryLog(t)
	ctx := context.Background()
	host, ok := RelayHosts.ForProfile(ctx, "", true).(NoopRelayRuntime)
	if !ok || !host.Retry || host.Reason != failure.Error() || *loads != 1 {
		t.Fatalf("host %#v after %d loads", host, *loads)
	}
	if again := RelayHosts.ForProfile(ctx, "", true); again != host || *loads != 1 {
		t.Fatalf("host %#v after %d loads while still provisioning", again, *loads)
	}
	if got := logs.String(); strings.Count(got, "Icebath Relay runtime initialization failed") != 1 {
		t.Fatalf("log %q", got)
	}
	*ready = true
	if rt := GetRelayRuntime(ctx, true, ""); rt == nil || *loads != 2 {
		t.Fatalf("runtime %v after %d loads once ready", rt, *loads)
	}
}

func TestBindingThatFailsWhileReadyKeepsItsReducedHost(t *testing.T) {
	isolateRelayDemand(t)
	ready, loads := relayProvisioningBinding(t, errors.New("No module named 'nemo_relay'"))
	RegisterNemoRelayReadiness(func() bool { return true })
	*ready = false
	ctx := context.Background()
	host, ok := RelayHosts.ForProfile(ctx, "", true).(NoopRelayRuntime)
	if !ok || host.Retry || *loads != 1 {
		t.Fatalf("host %#v after %d loads", host, *loads)
	}
	if again := RelayHosts.ForProfile(ctx, "", true); again != host || *loads != 1 {
		t.Fatalf("host %#v after %d loads", again, *loads)
	}
}

func TestReplacingTheBindingDropsItsReadinessCheck(t *testing.T) {
	RegisterNemoRelayReadiness(func() bool { return false })
	RegisterNemoRelayBinding(nil)
	t.Cleanup(func() { RegisterNemoRelayBinding(nil) })
	if !relayBindingReady() {
		t.Fatal("the readiness of the replaced binding survived")
	}
}
