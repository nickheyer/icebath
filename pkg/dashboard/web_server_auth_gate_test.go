package dashboard

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestShouldRequireAuthTruthTable(t *testing.T) {
	for _, c := range []struct {
		host     string
		expected bool
	}{
		{"127.0.0.1", false}, {"127.0.0.1", false}, {"localhost", false}, {"::1", false},
		{"0.0.0.0", true}, {"0.0.0.0", true}, {"192.168.1.5", true}, {"10.0.0.1", true},
		{"100.64.0.1", true}, {"hermes-agent-prod-abc.fly.dev", true},
	} {
		if got := gateway.ShouldRequireAuth(c.host); got != c.expected {
			t.Errorf("ShouldRequireAuth(%q) = %v", c.host, got)
		}
	}
}

func TestPublicURLAwareGateRequiresAuthForLoopbackProxy(t *testing.T) {
	t.Setenv("ICEBATH_DASHBOARD_PUBLIC_URL", "https://dashboard.example.test:9443")
	if !ShouldRequireDashboardAuth(context.Background(), "127.0.0.1", nil) {
		t.Fatal("public URL on another host must engage the gate")
	}
}

func TestPublicURLAwareGatePreservesLocalOnlyMode(t *testing.T) {
	t.Setenv("ICEBATH_DASHBOARD_PUBLIC_URL", "http://localhost:9119")
	if ShouldRequireDashboardAuth(context.Background(), "127.0.0.1", nil) {
		t.Fatal("loopback public URL must keep local token mode")
	}
}

func TestExemptWithDesktopEnvAndSessionTokenOnLoopback(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "desktop-minted")
	if !gateway.DesktopLoopbackAuthExempt("127.0.0.1", "", "") || !gateway.DesktopLoopbackAuthExempt("::1", "", "") {
		t.Fatal("desktop loopback backend must be exempt")
	}
}

func TestExemptViaSSHSpawnCredentialsWithoutEnvToken(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "")
	os.Unsetenv("ICEBATH_DASHBOARD_SESSION_TOKEN")
	if !gateway.DesktopLoopbackAuthExempt("127.0.0.1", "tok", "") || !gateway.DesktopLoopbackAuthExempt("127.0.0.1", "", "nonce") {
		t.Fatal("SSH spawn credentials must exempt")
	}
}

func TestNotExemptWithoutDesktopEnv(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "")
	os.Unsetenv("ICEBATH_DESKTOP")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "tok")
	if gateway.DesktopLoopbackAuthExempt("127.0.0.1", "", "") {
		t.Fatal("no desktop env must not exempt")
	}
}

func TestNotExemptWithoutAnyCredential(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "")
	os.Unsetenv("ICEBATH_DASHBOARD_SESSION_TOKEN")
	if gateway.DesktopLoopbackAuthExempt("127.0.0.1", "", "") {
		t.Fatal("no credential must not exempt")
	}
}

func TestNotExemptOnNonLoopbackBind(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "tok")
	if gateway.DesktopLoopbackAuthExempt("0.0.0.0", "", "") || gateway.DesktopLoopbackAuthExempt("192.168.1.10", "", "") {
		t.Fatal("non-loopback bind must not exempt")
	}
}

func TestPublicURLEngagesGateForNonDesktopLoopback(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "")
	os.Unsetenv("ICEBATH_DESKTOP")
	if !ShouldRequireDashboardAuth(context.Background(), "127.0.0.1", map[string]bool{"dash.example.com": true}) {
		t.Fatal("public host must engage the gate")
	}
}

func TestHonorsInjectedToken(t *testing.T) {
	if err := ImportWebServer(); err != nil {
		t.Fatal(err)
	}
	original := SessionToken()
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "desktop-seeded-token")
	if got, err := gateway.ResolveSessionToken(); err != nil || got != "desktop-seeded-token" {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := ImportWebServer(); err != nil || SessionToken() != original {
		t.Fatal("the loaded module token must stay untouched")
	}
}
