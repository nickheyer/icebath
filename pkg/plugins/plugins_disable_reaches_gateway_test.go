package plugins

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// World of one disable or enable against a running gateway double, its home, calls and the pid it reports
type disableGatewayWorld struct {
	ctx   context.Context
	home  string
	root  string
	calls []gatewayCall
}

// Probe plugin enabled in a PM admitted home, a profile under a shared root when shared, with the gateway answers given
func disableGatewayHome(t *testing.T, sharedRoot bool, answer, shared map[string]any, gatewayPid int) *disableGatewayWorld {
	t.Helper()
	tmp, h := pubWorld(t)
	w := &disableGatewayWorld{home: h, root: h}
	if sharedRoot {
		w.root = filepath.Join(tmp, "shared-root")
		w.home = filepath.Join(w.root, "profiles", "work")
		mustMkdir(t, w.home)
		t.Setenv("ICEBATH_HOME", w.home)
		config.ResetCache()
	}
	pubPluginTree(t, filepath.Join(w.home, "plugins", "probe"), "name: probe\nversion: '1.0'\n")
	writeFile(t, filepath.Join(w.home, "config.yaml"), "plugins:\n  enabled: [probe]\n  disabled: []\n")
	w.ctx = homeCtx(w.home)
	txOAuthSurface(t)
	next := *services.Load()
	next.DefinesMemoryProvider = func(string) bool { return false }
	next.RequiresAuthNotice = func(map[string]any) string { return "" }
	next.DeclaresModelProviderKind = func(string) bool { return false }
	next.PluginOAuthKey = func(string) string { return "" }
	next.ForgetPluginGrants = func(string) error { return nil }
	next.OAuthAdmissionProblems = func(map[string]any, string) []string { return nil }
	next.OAuthAdmissionWarnings = func(map[string]any, string) []string { return nil }
	next.GatewayRunningPid = func(string, bool) (int, bool, error) { return gatewayPid, gatewayPid != 0, nil }
	next.InstalledGatewayServiceKind = func(context.Context) (string, bool, error) { return "", false, nil }
	next.RefreshPluginSessions = func(context.Context, string, string, string) error { return nil }
	next.HostRendezvous = &fakeRendezvous{}
	next.ReloadGatewayPlugins = fakeGateway(answer, shared, &w.calls)
	InstallServices(&next)
	return w
}

// Output of icebath plugins disable probe with runs of whitespace collapsed
func (w *disableGatewayWorld) disable(t *testing.T) (string, string) {
	t.Helper()
	var out bytes.Buffer
	if err := CmdDisable(w.ctx, "probe", &out); err != nil {
		t.Fatalf("disable: %v\n%s", err, out.String())
	}
	return out.String(), strings.Join(strings.Fields(out.String()), " ")
}

func resolvedDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

var reloadedAnswer = map[string]any{"reloaded": true, "plugins": []any{}}

func TestDisableUnloadsThePluginFromTheRunningGateway(t *testing.T) {
	w := disableGatewayHome(t, false, reloadedAnswer, nil, 0)
	out, _ := w.disable(t)
	if len(w.calls) == 0 || resolvedDir(t, w.calls[0].target) != resolvedDir(t, w.home) {
		t.Fatalf("gateway calls %v", w.calls)
	}
	if strings.Contains(out, "next session") || !strings.Contains(out, "running gateway reloaded") {
		t.Fatalf("out %q", out)
	}
}

func TestAnExplicitRefusalIsReportedNotCalledSilence(t *testing.T) {
	w := disableGatewayHome(t, false, unservedAnswer, nil, 0)
	_, out := w.disable(t)
	if !strings.Contains(out, "refused") || !strings.Contains(out, "home is not served by this gateway") ||
		!strings.Contains(out, "saved config") || strings.Contains(out, "No running gateway answered") {
		t.Fatalf("out %q", out)
	}
}

func TestAProfileWithoutItsOwnGatewayReachesTheSharedOne(t *testing.T) {
	w := disableGatewayHome(t, true, nil, reloadedAnswer, 0)
	out, _ := w.disable(t)
	var got []gatewayCall
	for _, call := range w.calls {
		resolved := gatewayCall{target: resolvedDir(t, call.target)}
		if call.profileHome != "" {
			resolved.profileHome = resolvedDir(t, call.profileHome)
		}
		got = append(got, resolved)
	}
	want := []gatewayCall{{resolvedDir(t, w.home), ""}, {resolvedDir(t, w.root), resolvedDir(t, w.home)}}
	if !reflect.DeepEqual(got, want) || !strings.Contains(out, "running gateway reloaded") {
		t.Fatalf("calls %v want %v out %q", got, want, out)
	}
}

func TestASharedGatewayRefusingAnUnservedHomeIsReported(t *testing.T) {
	w := disableGatewayHome(t, true, nil, unservedAnswer, 0)
	_, out := w.disable(t)
	if len(w.calls) != 2 || !strings.Contains(out, "home is not served by this gateway") ||
		!strings.Contains(out, "saved config") || strings.Contains(out, "No running gateway answered") {
		t.Fatalf("calls %v out %q", w.calls, out)
	}
}

func TestDisableWithoutAnAnsweringGatewaySaysARestartIsNeeded(t *testing.T) {
	w := disableGatewayHome(t, false, nil, nil, 0)
	out, _ := w.disable(t)
	if strings.Contains(out, "next session") || !strings.Contains(out, "icebath gateway restart") {
		t.Fatalf("out %q", out)
	}
}

func TestDisableSaysTheRunningGatewayUnloadedIt(t *testing.T) {
	w := disableGatewayHome(t, false, reloadedAnswer, nil, 0)
	_, out := w.disable(t)
	if !strings.Contains(out, "running gateway reloaded its plugins and unloaded probe") || strings.Contains(out, "new sessions") ||
		!strings.Contains(out, "platform hooks it already wired up stay until") || !strings.Contains(out, "icebath gateway restart") {
		t.Fatalf("out %q", out)
	}
}

func TestEnablePrintsOnlyTheOutcomeThatApplies(t *testing.T) {
	for _, reloaded := range []bool{true, false} {
		name := "not_reloaded"
		if reloaded {
			name = "reloaded"
		}
		t.Run(name, func(t *testing.T) {
			var answer map[string]any
			if reloaded {
				answer = map[string]any{"reloaded": true, "plugins": []any{"probe"}, "activations": []any{
					map[string]any{"name": "probe", "key": "probe", "activated_now": map[string]any{"hooks": 1}, "deferred": map[string]any{}}}}
			}
			w := disableGatewayHome(t, false, answer, nil, 4242)
			writeFile(t, filepath.Join(w.home, "config.yaml"), "plugins:\n  enabled: []\n  disabled: [probe]\n")
			config.ResetCache()
			var buf bytes.Buffer
			no := false
			if err := CmdEnable(w.ctx, "probe", &no, &buf); err != nil {
				t.Fatalf("enable: %v\n%s", err, buf.String())
			}
			out := strings.Join(strings.Fields(buf.String()), " ")
			if !strings.Contains(out, "Plugin probe enabled.") || strings.Contains(out, "next session") {
				t.Fatalf("out %q", out)
			}
			if strings.Contains(out, "active in the running gateway now") != reloaded || strings.Contains(out, "icebath gateway restart") == reloaded {
				t.Fatalf("reloaded=%v out %q", reloaded, out)
			}
		})
	}
}
