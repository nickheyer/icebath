package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Bare runner whose config multiplexes as given
func handoffScopeRunner(multiplex bool) *GatewayRunner {
	r := allocGatewayRunner()
	r.config = &gateway.GatewayConfig{MultiplexProfiles: new(multiplex)}
	return r
}

func TestScopesSingleProfileGatewayIsRootOnly(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	scopes := handoffWatchScopes(context.Background(), handoffScopeRunner(false))
	if len(scopes) != 1 || scopes[0].name != nil || scopes[0].home != "" {
		t.Fatal(scopes)
	}
}

func TestScopesIncludeRootFirstThenEverySecondaryHome(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	for _, name := range []string{"bala", "medicina"} {
		dir := filepath.Join(h, "profiles", name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0o644)
	}
	scopes := handoffWatchScopes(context.Background(), handoffScopeRunner(true))
	if scopes[0].name != nil || scopes[0].home != "" {
		t.Fatal("root store must still be polled first")
	}
	var got []string
	for _, s := range scopes[1:] {
		if s.name == nil || *s.name == "default" {
			t.Fatal("default profile must not be polled twice per tick")
		}
		got = append(got, *s.name+"="+s.home)
	}
	want := []string{"bala=" + filepath.Join(h, "profiles", "bala"), "medicina=" + filepath.Join(h, "profiles", "medicina")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("%v want %v", got, want)
	}
}

func TestScopesDegradeToRootWhenResolutionRaises(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	profilesDir := filepath.Join(h, "profiles")
	os.MkdirAll(filepath.Join(profilesDir, "bala"), 0o755)
	os.WriteFile(filepath.Join(profilesDir, "bala", "config.yaml"), []byte("{}\n"), 0o644)
	os.Chmod(profilesDir, 0)
	t.Cleanup(func() { os.Chmod(profilesDir, 0o755) })
	scopes := handoffWatchScopes(context.Background(), handoffScopeRunner(true))
	if len(scopes) != 1 || scopes[0].name != nil {
		t.Fatal(scopes)
	}
}
