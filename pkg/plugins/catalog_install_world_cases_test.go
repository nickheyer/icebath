package plugins

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// URL install of a fresh one commit repo carrying the extra files, returning the installed tree
func catInstallURL(t *testing.T, w *catWorldT, repo, name string, files map[string]string) string {
	t.Helper()
	c6Write(t, filepath.Join(repo, "plugin.yaml"), "name: "+name+"\nversion: 1.0.0\ndescription: d\n")
	c6Write(t, filepath.Join(repo, "__init__.py"), "def register(ctx):\n    pass\n")
	for rel, text := range files {
		c6Write(t, filepath.Join(repo, rel), text)
	}
	c6Git(t, repo, "init", "-q")
	c6Commit(t, repo, "v1")
	target, _, _, err := installPluginCore(w.ctx, refURI(repo), installCoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestCustomInstallRecordsAnAnonymousExtensionInstallAndReinstallNone(t *testing.T) {
	w := catWorld(t)
	calls := []map[string]any{}
	saved := recordExtensionInstall
	t.Cleanup(func() { recordExtensionInstall = saved })
	recordExtensionInstall = func(_ context.Context, kind, source string, name any, outcome string, _, _ any, _ error) {
		calls = append(calls, map[string]any{"kind": kind, "source": source, "name": name, "outcome": outcome})
	}
	if res, err := DashboardInstallPlugin(w.ctx, DashboardInstallOptions{Identifier: refURI(w.repo)}); err != nil || res["ok"] != true {
		t.Fatalf("url install %v %v", res, err)
	}
	if res, err := DashboardInstallPlugin(w.ctx, DashboardInstallOptions{Force: true, CatalogName: "cat-plugin"}); err != nil || res["ok"] != true {
		t.Fatalf("catalog reinstall %v %v", res, err)
	}
	want := []map[string]any{{"kind": "plugin", "source": "local", "name": nil, "outcome": "success"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("recorded %v", calls)
	}
}

func TestKillListCoversUpdateEnableAndLoadOfAnInstalledPlugin(t *testing.T) {
	w := catWorld(t)
	repo := filepath.Join(w.root, "later-killed")
	target := catInstallURL(t, w, repo, "killed", nil)
	dir := t.TempDir()
	c6Write(t, filepath.Join(dir, "removed.yaml"), "removed:\n- name: killed\n  reason: backdoor\n")
	t.Cleanup(SwapCatalogDir(dir))
	if res, err := DashboardUpdateUserPlugin(w.ctx, "killed", false, false); err != nil || !strings.Contains(pyStrOr(res["error"]), "backdoor") {
		t.Fatalf("update of a killed plugin %v %v", res, err)
	}
	var exit *CommandExit
	if err := CmdEnable(w.ctx, "killed", nil, io.Discard); !errors.As(err, &exit) {
		t.Fatalf("enable of a killed plugin = %v, want SystemExit", err)
	}
	if getEnabledSet(w.h)["killed"] {
		t.Fatal("killed plugin enabled")
	}
	manifest := &Manifest{Name: "killed", Source: "user", Path: target, Key: "killed"}
	if gate := GateManifest(w.h, manifest, map[string]bool{}, map[string]bool{"killed": true}, true); gate.Action == "load" {
		t.Fatalf("killed plugin gate %+v", gate)
	}
	disable := false
	if err := CmdInstall(w.ctx, InstallCommandOptions{Identifier: refURI(repo), Force: true, Enable: &disable, AllowRemoved: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if gate := GateManifest(w.h, manifest, map[string]bool{}, map[string]bool{"killed": true}, true); gate.Action != "load" {
		t.Fatalf("allow removed install gate %+v", gate)
	}
}

func TestBriefInstallPrintsThePinAndOutcomeAndARestartHintOnlyForAGateway(t *testing.T) {
	for _, gatewayPID := range []int{0, 4242} {
		t.Run(map[int]string{0: "no-gateway", 4242: "gateway-running"}[gatewayPID], func(t *testing.T) {
			w := catWorld(t)
			next := *installedServices()
			next.GatewayRunningPid = func(string, bool) (int, bool, error) { return gatewayPID, gatewayPID != 0, nil }
			InstallServices(&next)
			var out bytes.Buffer
			enable := true
			if err := CmdInstall(w.ctx, InstallCommandOptions{Identifier: "cat-plugin", Enable: &enable, Brief: true}, &out); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if !strings.Contains(text, "pinned @") || !strings.Contains(text, "Plugin cat-plugin enabled") {
				t.Fatalf("brief install output %q", text)
			}
			if strings.ContainsAny(text, "╭┌") || strings.Contains(text, "Plugin installed:") {
				t.Fatalf("brief install printed a panel %q", text)
			}
			if strings.Contains(text, "maintained by") || strings.Contains(text, "Cloning") {
				t.Fatalf("brief install printed the description or clone line %q", text)
			}
			if strings.Contains(text, "Restart the gateway") != (gatewayPID != 0) {
				t.Fatalf("restart hint with gateway pid %d: %q", gatewayPID, text)
			}
		})
	}
}
