package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func writeRouteProfile(t *testing.T, dir, host, model, key, secret, effort string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "model:\n  provider: custom\n  base_url: https://" + host + "/v1\n  default: " + model + "\n  key_env: PROFILE_KEY\n" +
		"agent:\n  reasoning_effort: " + effort + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	env := "PROFILE_KEY=" + key + "\nPROBE_SECRET=" + secret + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Launch profile and a coder profile beneath it, the launch secrets loaded into the process env
func twoRouteProfiles(t *testing.T) (string, string) {
	tmp := t.TempDir()
	launch := filepath.Join(tmp, "os-home", ".icebath")
	writeRouteProfile(t, launch, "launch.invalid", "launch-model", "launch-key", "launch-secret", "medium")
	coder := filepath.Join(launch, "profiles", "coder")
	writeRouteProfile(t, coder, "coder.invalid", "coder-model", "coder-key", "coder-secret", "low")
	t.Setenv("HOME", filepath.Join(tmp, "os-home"))
	t.Setenv("ICEBATH_HOME", launch)
	t.Setenv("PROFILE_KEY", "launch-key")
	t.Setenv("PROBE_SECRET", "launch-secret")
	return launch, coder
}

func TestProfileRouteResolvesTheTargetProfilesModelAndKey(t *testing.T) {
	_, coder := twoRouteProfiles(t)
	route, err := routeToTaskRoute(context.Background(), map[string]any{"profile": "coder", "label": "coder"}, &AIAgent{},
		map[string]*tools.TaskRoute{}, map[string]any{}, tools.DefaultDelegationBackend)
	if err != nil {
		t.Fatal(err)
	}
	gotHome, _ := filepath.EvalSymlinks(route.ProfileHome)
	wantHome, _ := filepath.EvalSymlinks(coder)
	if gotHome != wantHome || route.Label != "coder" {
		t.Fatalf("home %q label %q", route.ProfileHome, route.Label)
	}
	if route.Creds["model"] != "coder-model" || route.Creds["base_url"] != "https://coder.invalid/v1" || route.Creds["api_key"] != "coder-key" {
		t.Fatalf("creds %v", route.Creds)
	}
	reasoning, ok := route.ReasoningEffort.(tools.ResolvedReasoning)
	if !ok || !reflect.DeepEqual(reasoning.Config, map[string]any{"enabled": true, "effort": "low"}) {
		t.Fatalf("reasoning %#v", route.ReasoningEffort)
	}
	if os.Getenv("PROBE_SECRET") != "launch-secret" || os.Getenv("PROFILE_KEY") != "launch-key" {
		t.Fatal("launch env changed")
	}
}

func TestUnknownProfileIsRefusedWithAnInstruction(t *testing.T) {
	twoRouteProfiles(t)
	_, err := routeToTaskRoute(context.Background(), map[string]any{"profile": "nobody"}, &AIAgent{},
		map[string]*tools.TaskRoute{}, map[string]any{}, tools.DefaultDelegationBackend)
	var routeErr *tools.DelegationRouteError
	if !errors.As(err, &routeErr) || !strings.Contains(err.Error(), "icebath profile list") {
		t.Fatalf("err %v", err)
	}
}
