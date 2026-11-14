package selfupdate

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

func wblClearEnv(t *testing.T, names ...string) {
	for _, name := range names {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func wblDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func TestWebBuildLimitsHeapAndThreadCapsApplied(t *testing.T) {
	wblClearEnv(t, "NODE_OPTIONS", "RAYON_NUM_THREADS", "ICEBATH_WEB_BUILD_MAX_OLD_SPACE_SIZE", "ICEBATH_WEB_BUILD_THREADS")
	env := WebBuildLimits(map[string]string{})
	if !strings.Contains(env["NODE_OPTIONS"], "--max-old-space-size=") {
		t.Fatalf("NODE_OPTIONS %q", env["NODE_OPTIONS"])
	}
	if n, err := strconv.Atoi(env["RAYON_NUM_THREADS"]); err != nil || n < 1 {
		t.Fatalf("RAYON_NUM_THREADS %q", env["RAYON_NUM_THREADS"])
	}
}

func TestWebBuildLimitsExistingUserNodeOptionsPreserved(t *testing.T) {
	wblClearEnv(t, "RAYON_NUM_THREADS")
	env := WebBuildLimits(map[string]string{"NODE_OPTIONS": "--max-old-space-size=8192"})
	if strings.Count(env["NODE_OPTIONS"], "--max-old-space-size=") != 1 || !strings.Contains(env["NODE_OPTIONS"], "8192") {
		t.Fatalf("NODE_OPTIONS %q", env["NODE_OPTIONS"])
	}
}

func TestWebBuildLimitsExistingUserRayonThreadsPreserved(t *testing.T) {
	wblClearEnv(t, "NODE_OPTIONS")
	env := WebBuildLimits(map[string]string{"RAYON_NUM_THREADS": "6"})
	if env["RAYON_NUM_THREADS"] != "6" || !strings.Contains(env["NODE_OPTIONS"], "--max-old-space-size=") {
		t.Fatalf("env %v", env)
	}
}

func TestWebBuildLimitsExplicitEnvOverridesWin(t *testing.T) {
	wblClearEnv(t, "NODE_OPTIONS", "RAYON_NUM_THREADS")
	env := WebBuildLimits(map[string]string{"ICEBATH_WEB_BUILD_MAX_OLD_SPACE_SIZE": "3072", "ICEBATH_WEB_BUILD_THREADS": "1"})
	if !strings.Contains(env["NODE_OPTIONS"], "--max-old-space-size=3072") || env["RAYON_NUM_THREADS"] != "1" {
		t.Fatalf("env %v", env)
	}
}

func TestWebBuildLimitsCgroupLimitShrinksHeapFloor(t *testing.T) {
	wblClearEnv(t, "NODE_OPTIONS")
	prev := webCgroupMemoryLimitMB
	webCgroupMemoryLimitMB = func() (int64, bool) { return 2048, true }
	t.Cleanup(func() { webCgroupMemoryLimitMB = prev })
	env := WebBuildLimits(map[string]string{})
	if !strings.Contains(env["NODE_OPTIONS"], "--max-old-space-size=1536") {
		t.Fatalf("NODE_OPTIONS %q", env["NODE_OPTIONS"])
	}
}

func TestWebBuildLimitsBoundedThreadsHalfCores(t *testing.T) {
	prev := webAvailableCores
	t.Cleanup(func() { webAvailableCores = prev })
	for cores, want := range map[int]string{16: "8", 4: "2", 1: "1"} {
		webAvailableCores = func() int { return cores }
		if got := WebBuildLimits(map[string]string{})["RAYON_NUM_THREADS"]; got != want {
			t.Fatalf("cores %d: %q want %q", cores, got, want)
		}
	}
}

func TestWebBuildLimitsApplyUpdatesEnvInPlace(t *testing.T) {
	wblClearEnv(t, "NODE_OPTIONS", "RAYON_NUM_THREADS")
	env := map[string]string{"PATH": "/bin"}
	ApplyWebBuildLimits(env)
	if !strings.Contains(env["NODE_OPTIONS"], "--max-old-space-size=") || !wblDigits(env["RAYON_NUM_THREADS"]) || env["PATH"] != "/bin" {
		t.Fatalf("env %v", env)
	}
}

func TestWebBuildLimitsLightModeTightensCaps(t *testing.T) {
	wblClearEnv(t, "NODE_OPTIONS", "RAYON_NUM_THREADS", "ICEBATH_WEB_BUILD_MAX_OLD_SPACE_SIZE", "ICEBATH_WEB_BUILD_THREADS")
	prev := webAvailableCores
	webAvailableCores = func() int { return 8 }
	t.Cleanup(func() { webAvailableCores = prev })
	env := WebBuildLimits(map[string]string{"ICEBATH_WEB_BUILD_LIGHT": "1"})
	if !strings.Contains(env["NODE_OPTIONS"], "--max-old-space-size=1024") || env["RAYON_NUM_THREADS"] != "1" {
		t.Fatalf("env %v", env)
	}
}

func TestBuildSourceWebAppliesLimits(t *testing.T) {
	captured := map[string]any{}
	prev := compileSourceWeb
	compileSourceWeb = func(_ context.Context, st *frontendStep, _, _, _ string, env map[string]string) error {
		captured["env"], captured["label"] = env, st.label
		return nil
	}
	t.Cleanup(func() { compileSourceWeb = prev })
	wblClearEnv(t, "NODE_OPTIONS", "RAYON_NUM_THREADS")
	if err := BuildSourceWeb(context.Background(), t.TempDir(), map[string]string{"PATH": "/bin"}, ""); err != nil {
		t.Fatal(err)
	}
	env := captured["env"].(map[string]string)
	if !strings.Contains(env["NODE_OPTIONS"], "--max-old-space-size=") || !wblDigits(env["RAYON_NUM_THREADS"]) || env["PATH"] != "/bin" {
		t.Fatalf("env %v", env)
	}
	if captured["label"] != "Building the web UI" {
		t.Fatalf("label %v", captured["label"])
	}
}
