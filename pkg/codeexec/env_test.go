package codeexec

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestNativeEssentialsAndPassthroughPriority(t *testing.T) {
	essentials := map[string]string{
		"SYSTEMROOT": `C:\Windows`, "SystemRoot": `C:\Windows`, "SystemDrive": "C:", "WINDIR": `C:\Windows`,
		"ComSpec": `C:\Windows\System32\cmd.exe`, "comspec": `C:\Windows\System32\cmd.exe`,
		"APPDATA": `C:\Users\alice\AppData\Roaming`, "LOCALAPPDATA": `C:\Users\alice\AppData\Local`,
	}
	safe := map[string]string{"PATH": `C:\Windows\System32;C:\Python`, "HOME": `C:\Users\alice`,
		"PATHEXT": ".COM;.EXE;.BAT;.CMD;.PY", "USERPROFILE": `C:\Users\alice`, "TEMP": `C:\Users\alice\Temp`}
	secret := map[string]string{"OPENAI_API_KEY": "fake-provider", "GITHUB_TOKEN": "fake-github",
		"MY_PASSWORD": "fake-password", "TENOR_API_KEY": "fake-third-party", "RANDOM_UNKNOWN_VAR": "unknown"}
	for _, windows := range []bool{false, true} {
		for _, passthrough := range []bool{false, true} {
			env := testEnv(t)
			source := map[string]string{}
			for _, m := range []map[string]string{essentials, safe, secret} {
				maps.Copy(source, m)
			}
			w := windows
			p := passthrough
			got, err := scrubChildEnv(env, source, scrubOptions{isWindows: &w,
				isPassthrough: func(k string) bool { return p && k == "TENOR_API_KEY" }})
			if err != nil {
				t.Fatal(err)
			}
			want := maps.Clone(safe)
			if windows {
				maps.Copy(want, essentials)
			}
			if passthrough {
				want["TENOR_API_KEY"] = "fake-third-party"
			}
			if !maps.Equal(got, want) {
				t.Fatalf("windows=%v passthrough=%v got %v want %v", windows, passthrough, got, want)
			}
		}
	}
}

func scopedSecretEnv(t *testing.T, dotenv string) *tools.Env {
	t.Helper()
	env := testEnv(t)
	tools.RegisterEnvPassthrough(env, []string{"SERVICE_TOKEN"})
	t.Setenv("SERVICE_TOKEN", "token-for-default")
	if err := os.WriteFile(filepath.Join(env.Home, ".env"), []byte(dotenv), 0o600); err != nil {
		t.Fatal(err)
	}
	env.Secrets = config.NewSecrets(env.Home)
	tools.SetMultiplexActive(true)
	t.Cleanup(func() { tools.SetMultiplexActive(false) })
	return env
}

func TestExecuteCodeUsesActiveProfileForPassthrough(t *testing.T) {
	env := scopedSecretEnv(t, "SERVICE_TOKEN=token-for-routed-profile\n")
	child, err := scrubChildEnv(env, map[string]string{"SERVICE_TOKEN": "token-for-default"}, scrubOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if child["SERVICE_TOKEN"] != "token-for-routed-profile" {
		t.Fatalf("child %v", child)
	}
}

func TestExecuteCodeOmitsMissingScopedPassthrough(t *testing.T) {
	env := scopedSecretEnv(t, "")
	child, err := scrubChildEnv(env, map[string]string{"SERVICE_TOKEN": "token-for-default"}, scrubOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := child["SERVICE_TOKEN"]; ok {
		t.Fatalf("missing routed secret leaked %v", child)
	}
}

func TestExecuteCodeStripsBuzzVars(t *testing.T) {
	env := testEnv(t)
	child, err := scrubChildEnv(env, map[string]string{
		"BUZZ_PRIVATE_KEY": "nsec1fake", "BUZZ_AUTH_TAG": `["tag","data","kind","sig"]`,
		"BUZZ_RELAY_URL": "https://mycommunity.communities.buzz.xyz", "PATH": "/usr/bin", "HOME": "/home/user",
	}, scrubOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"BUZZ_PRIVATE_KEY", "BUZZ_AUTH_TAG", "BUZZ_RELAY_URL"} {
		if _, ok := child[k]; ok {
			t.Fatalf("%s leaked", k)
		}
	}
	if child["PATH"] != "/usr/bin" || child["HOME"] != "/home/user" {
		t.Fatalf("child %v", child)
	}
}

func TestCredentialAndControlNamesAreRefusedBySecretPolicy(t *testing.T) {
	for _, name := range []string{"NODE_BEARER", "NODE_CREDS", "CONDA_DSN", "DEPLOY_WEBHOOK", "session_cookie", "my_apikey"} {
		if !IsSecretEnvName(name) {
			t.Fatalf("%s not refused", name)
		}
	}
	for _, name := range []string{"NVM_DIR", "MY_TOOLCHAIN_HOME", "PATH"} {
		if IsSecretEnvName(name) {
			t.Fatalf("%s refused", name)
		}
	}
	env := testEnv(t)
	no := false
	child, err := scrubChildEnv(env, map[string]string{"ICEBATH_KANBAN_DB": "/board.db", "ICEBATH_SECRET_THING": "x",
		"ICEBATH_PROFILE": "work", "NVM_DIR": "/client/.nvm"}, scrubOptions{isWindows: &no, toolchain: map[string]bool{"NVM_DIR": true}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ICEBATH_PROFILE": "work", "NVM_DIR": "/client/.nvm"}
	if !maps.Equal(child, want) {
		t.Fatalf("child %v", child)
	}
}

func TestDelegateChildExecuteCodeEnvBridgesContextvarAndScrubsKanban(t *testing.T) {
	env := testEnv(t)
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_parent")
	t.Setenv("ICEBATH_KANBAN_RUN_ID", "123")
	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(h, "kanban.db"))
	t.Setenv("ICEBATH_KANBAN_WORKSPACE", filepath.Join(h, "parent-workspace"))
	t.Setenv("ICEBATH_KANBAN_CLAIM_LOCK", "lock")
	os.Unsetenv("ICEBATH_DELEGATED_CHILD_CONTEXT")
	env.Values.Store("session.delegated_child", true)
	no := false
	child, err := scrubChildEnv(env, environMap(), scrubOptions{isWindows: &no,
		isPassthrough: func(k string) bool { return len(k) > 15 && k[:15] == "ICEBATH_KANBAN_" }})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := os.LookupEnv("ICEBATH_DELEGATED_CHILD_CONTEXT"); ok {
		t.Fatal("marker leaked into the parent process env")
	}
	if child["ICEBATH_HOME"] != h || child["ICEBATH_DELEGATED_CHILD_CONTEXT"] == "" {
		t.Fatalf("child %v", child)
	}
	for _, k := range []string{"ICEBATH_KANBAN_TASK", "ICEBATH_KANBAN_RUN_ID", "ICEBATH_KANBAN_CLAIM_LOCK"} {
		if _, ok := child[k]; ok {
			t.Fatalf("%s survived", k)
		}
	}
	if child["ICEBATH_KANBAN_DB"] != filepath.Join(h, "kanban.db") {
		t.Fatalf("board location dropped %v", child)
	}
}

func homeOverrideChildEnv(t *testing.T, override string) map[string]string {
	t.Helper()
	rt := hostRuntime(t)
	ctx := t.Context()
	if override != "" {
		ctx = home.WithOverride(ctx, override)
	}
	child, err := buildChildEnv(ctx, testEnvKeepHome(t), childEnvSpec{RPCEndpoint: "sock", RPCToken: "tok",
		TmpDir: "/tmp/icebath-test", ChildPython: rt.Python})
	if err != nil {
		t.Fatal(err)
	}
	return child
}

// Tool env that leaves the process ICEBATH_HOME as the test set it
func testEnvKeepHome(t *testing.T) *tools.Env {
	t.Helper()
	keep := os.Getenv("ICEBATH_HOME")
	env := testEnv(t)
	t.Setenv("ICEBATH_HOME", keep)
	return env
}

func TestOverrideRewritesStaleServerDefaultPerTurn(t *testing.T) {
	withTimezone(t, "")
	t.Setenv("ICEBATH_HOME", "/machine/default/.icebath")
	base := t.TempDir()
	alpha := filepath.Join(base, "profiles", "alpha")
	if got := homeOverrideChildEnv(t, alpha)["ICEBATH_HOME"]; got != alpha {
		t.Fatalf("alpha turn saw %q", got)
	}
	beta := filepath.Join(base, "profiles", "beta")
	if got := homeOverrideChildEnv(t, beta)["ICEBATH_HOME"]; got != beta {
		t.Fatalf("beta turn saw %q", got)
	}
}

func TestNoOverrideLeavesInheritedValueUntouched(t *testing.T) {
	withTimezone(t, "")
	if home.Override(t.Context()) != "" {
		t.Fatal("override bound outside a turn")
	}
	t.Setenv("ICEBATH_HOME", "/machine/default/.icebath")
	if got := homeOverrideChildEnv(t, "")["ICEBATH_HOME"]; got != "/machine/default/.icebath" {
		t.Fatalf("inherited home rewritten to %q", got)
	}
}

func TestConfiguredTimezoneTravelsAsTZOffWindows(t *testing.T) {
	withTimezone(t, "America/Los_Angeles")
	t.Setenv("ICEBATH_TIMEZONE", "America/Los_Angeles")
	child := homeOverrideChildEnv(t, "")
	if _, ok := child["ICEBATH_TIMEZONE"]; ok {
		t.Fatal("ICEBATH_TIMEZONE reached the child")
	}
	if isWindows {
		if _, ok := child["TZ"]; ok {
			t.Fatal("IANA TZ handed to a Windows child")
		}
	} else if child["TZ"] != "America/Los_Angeles" {
		t.Fatalf("TZ %q", child["TZ"])
	}
}

// A damaged runtime dependency record raises out of the child env build instead of keeping owned entries
func TestDamagedRuntimeRecordRaisesFromChildEnvStrip(t *testing.T) {
	hostRuntime(t)
	env := testEnv(t)
	facts := pm.EnvironmentRuntimeFactsPath(context.Background(), pm.RepoRoot())
	if err := os.MkdirAll(filepath.Dir(facts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(facts, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PYTHONPATH", "/user/libs")
	child, err := buildChildEnv(t.Context(), env, childEnvSpec{RPCEndpoint: "sock", RPCToken: "tok",
		TmpDir: "/tmp/icebath-test", ChildPython: "/opt/project/bin/python"})
	if want := "invalid dependency environment record: " + facts; err == nil || err.Error() != want {
		t.Fatalf("child=%v err=%v, want %q", child, err, want)
	}
}
