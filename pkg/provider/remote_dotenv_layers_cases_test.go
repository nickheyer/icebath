package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	remoteBootChildEnv   = "ICEBATH_TEST_REMOTE_BOOT_CHILD"
	remoteBootProjectEnv = "ICEBATH_TEST_REMOTE_BOOT_PROJECT"
)

// Child mode that loads the dotenv layers in a fresh process and prints the backend name and the loaded personality
func dispatchRemoteBootChildEntry() {
	if os.Getenv(remoteBootChildEnv) == "" {
		return
	}
	ctx := context.Background()
	opts := DotenvLoadOptions{}
	if project := os.Getenv(remoteBootProjectEnv); project != "" {
		opts.ProjectEnv, opts.SkipExternalSecrets = project, true
	}
	if _, err := LoadIcebathDotenv(ctx, opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	backend, err := config.GetConfigBackend(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tree, err := config.LoadContext(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, _ := json.Marshal([]any{backend.Name(), tree.Get("display.personality")})
	fmt.Println("RESULT=" + string(out))
	os.Exit(0)
}

// Runs this test binary as a fresh process in child mode and returns the RESULT pair it printed
func runRemoteBootChild(t *testing.T, dir string, env map[string]string) []any {
	t.Helper()
	child := []string{remoteBootChildEnv + "=1"}
	for _, k := range []string{"PATH", "LANG", "TMPDIR", "SYSTEMROOT"} {
		if v, ok := os.LookupEnv(k); ok {
			child = append(child, k+"="+v)
		}
	}
	for k, v := range env {
		child = append(child, k+"="+v)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env, cmd.Dir = child, dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child %v: %s", err, out)
	}
	var result []any
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "RESULT="); ok {
			result = nil
			if err := json.Unmarshal([]byte(rest), &result); err != nil {
				t.Fatal(err)
			}
		}
	}
	return result
}

// Moves the deployment out of the process env into dotenv files and returns the managed dir
func dropProcessDeployment(t *testing.T, p *configplane.Plane) string {
	t.Helper()
	for name := range bootPlaneEnv(p, true) {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	managed := filepath.Join(t.TempDir(), "managed")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_MANAGED_DIR", managed)
	return managed
}

func dotenvLines(env map[string]string, keep func(string) bool) string {
	var b strings.Builder
	for k, v := range env {
		if keep(k) {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
	}
	return b.String()
}

// The deployment in the home dotenv and the instance id only in the managed dotenv boots the fetch
func TestInstanceIdFromTheManagedDotenvBoots(t *testing.T) {
	p := setupBootPlane(t, true)
	managed := dropProcessDeployment(t, p)
	env := bootPlaneEnv(p, true)
	if err := os.WriteFile(filepath.Join(managed, ".env"), []byte(remote.InstanceEnv+"="+env[remote.InstanceEnv]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	delete(env, remote.InstanceEnv)
	if err := os.WriteFile(filepath.Join(p.Home, ".env"), []byte(dotenvLines(env, func(string) bool { return true })), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Upper = map[string]any{"display": map[string]any{"personality": "managed"}}
	if err := loadDotenv(p); err != nil {
		t.Fatal(err)
	}
	gets := p.Gets()
	if len(gets) == 0 || gets[0].Instance != configplane.Instance {
		t.Fatalf("gets %+v", gets)
	}
	if got := os.Getenv(remote.InstanceEnv); got != configplane.Instance {
		t.Fatalf("instance env %q", got)
	}
}

// The user dotenv selects remote, the project dotenv holds the IdP credential, a fresh process boots and fetches
func TestPlaneCredentialFromTheProjectDotenvBoots(t *testing.T) {
	p := setupBootPlane(t, true)
	env := bootPlaneEnv(p, true)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(p.Home, ".env"), []byte(dotenvLines(env, func(k string) bool { return strings.HasPrefix(k, "ICEBATH_CONFIG_") })), 0o600); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(dir, "project.env")
	if err := os.WriteFile(project, []byte(dotenvLines(env, func(k string) bool { return strings.HasPrefix(k, "GATEWAY_RELAY_IDP_") })), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Home, "config.yaml"), []byte("display:\n  personality: forbidden-local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Profile("default").Values = map[string]any{"display": map[string]any{"personality": "remote"}}
	p.Profile("default").Version = 1
	result := runRemoteBootChild(t, dir, map[string]string{"ICEBATH_HOME": p.Home, "HOME": dir, remoteBootProjectEnv: project})
	if !reflect.DeepEqual(result, []any{"remote", "remote"}) {
		t.Fatalf("result %v", result)
	}
	if len(p.Gets()) == 0 {
		t.Fatal("the fresh process never fetched")
	}
}

// The bootstrap composes the layers the way the dotenv load does
func TestBootstrapKeepsTheDotenvPrecedence(t *testing.T) {
	dir := t.TempDir()
	h := filepath.Join(dir, "home")
	managed := filepath.Join(dir, "managed")
	for _, d := range []string{h, managed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_MANAGED_DIR", managed)
	t.Setenv("CC_A", "process")
	t.Setenv("CC_D", "process")
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "")
	os.Unsetenv("OP_SERVICE_ACCOUNT_TOKEN")
	write := func(path, text string) {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(h, ".env"), "CC_A=user\nCC_B=user\n")
	write(filepath.Join(h, ".op.env"), "CC_B=op\nCC_E=op\n")
	project := filepath.Join(dir, "project.env")
	write(project, "CC_B=project\nCC_C=project\nCC_D=project\n")
	write(filepath.Join(managed, ".env"), "CC_C=managed\n")
	env := bootstrapEnv(h, project)
	got := []string{env["CC_A"], env["CC_B"], env["CC_C"], env["CC_D"], env["CC_E"]}
	if !reflect.DeepEqual(got, []string{"user", "user", "managed", "process", "op"}) {
		t.Fatalf("layers %v", got)
	}
	if err := os.Remove(filepath.Join(h, ".env")); err != nil {
		t.Fatal(err)
	}
	if got := bootstrapEnv(h, project)["CC_D"]; got != "project" {
		t.Fatalf("no user dotenv CC_D %q", got)
	}
}

// A child built for another profile still reads that profile remotely and never carries another IdP credential
func TestChildForAnotherProfileKeepsTheRemoteDeployment(t *testing.T) {
	p := setupBootPlane(t, true)
	dropProcessDeployment(t, p)
	env := bootPlaneEnv(p, true)
	if err := os.WriteFile(filepath.Join(p.Home, ".env"), []byte(dotenvLines(env, func(string) bool { return true })), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotenv(p); err != nil {
		t.Fatal(err)
	}
	beta := filepath.Join(p.Home, "profiles", "beta")
	if err := os.MkdirAll(beta, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beta, "config.yaml"), []byte("display:\n  personality: local-child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idp := dotenvLines(env, func(k string) bool { return strings.HasPrefix(k, "GATEWAY_RELAY_IDP_") })
	if err := os.WriteFile(filepath.Join(beta, ".env"), []byte(idp), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Mu.Lock()
	p.Profile("beta").Values = map[string]any{"display": map[string]any{"personality": "remote-child"}}
	p.Mu.Unlock()
	saved := tools.ProfileSecrets
	tools.ProfileSecrets = ProfileSecretSource()
	t.Cleanup(func() { tools.ProfileSecrets = saved })
	childEnv, err := tools.ServedProfileChildEnv(context.Background(), &tools.Env{Home: p.Home, Values: &sync.Map{}}, nil, beta, true)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if result := runRemoteBootChild(t, dir, childEnv); !reflect.DeepEqual(result, []any{"remote", "remote-child"}) {
		t.Fatalf("result %v", result)
	}
	sawBeta := false
	for _, g := range p.Gets() {
		sawBeta = sawBeta || g.Profile == "beta"
	}
	if !sawBeta {
		t.Fatal("no fetch for profile beta")
	}
	for k, v := range childEnv {
		if strings.HasPrefix(k, "GATEWAY_RELAY_IDP_") && v != env[k] {
			t.Fatalf("child carries %s=%q", k, v)
		}
	}
	if got := os.Getenv(config.BackendEnv); got != "remote" {
		t.Fatalf("parent selector %q", got)
	}
}
