package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// A client connected to the launch authority of a fresh home, over a native ticket when native
func newIdentityDesktop(t *testing.T, native bool, config string) *onbDesktop {
	t.Helper()
	root := onboardingLaunchRoot(t)
	if config != "" {
		writeFile(t, filepath.Join(root, "config.yaml"), config)
	}
	for _, name := range []string{"ICEBATH_DESKTOP", "ICEBATH_DASHBOARD_SESSION_TOKEN"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Cleanup(func() {
		provider.WaitForSetupRecord(provider.HomePoolEnv(root), 20*time.Second)
		tools.OnboardingDueRead(func() tools.OnboardingDue { return tools.OnboardingDue{} }, nil, 20*time.Second)
	})
	d := authorityDesktop(t, root, false)
	identity := map[string]any{"user_id": "uid:1", "provider": "local", "profile_id": root, "instance_id": "owner"}
	if native {
		identity["native_bootstrap"] = true
	}
	d.sock = newWSTestSocket()
	d.transport = tuigateway.NewWSTransport(d.sock, "test", identity)
	conn, err := gateway.NewAuthorityConnection(d.authority, d.transport, identity, false)
	if err != nil {
		t.Fatal(err)
	}
	d.conn = conn
	gateway.BindNativeTransport(d.authority, conn.Actor(), identity)
	return d
}

// Result object of a reply, failing the case when the reply carries none
func replyResult(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("reply %v", reply)
	}
	return result
}

func TestAFreshInstallIsOfferedOnboardingOnceTheDesktopSaysItIsLocal(t *testing.T) {
	d := newIdentityDesktop(t, true, "")
	before := replyResult(t, d.request(t, 1, "onboarding.state", pyjson.ObjectOf("assume_fresh", true)))
	if before["eligible"] != false {
		t.Fatalf("eligible before the declaration %v", before)
	}
	d.request(t, 2, "client.capabilities", pyjson.ObjectOf("server_requests", true))
	declared := d.request(t, 3, "client.capabilities", pyjson.ObjectOf("desktop_local", true))
	if _, ok := declared["result"]; !ok {
		t.Fatalf("declaration answered %v", declared)
	}
	if !tuigateway.AnswersRequests(d.transport) {
		t.Fatal("the transport does not answer server requests")
	}
	after := replyResult(t, d.request(t, 4, "onboarding.state", pyjson.ObjectOf("assume_fresh", true)))
	if after["run"] != true || after["eligible"] != true {
		t.Fatalf("state after the declaration %v", after)
	}
}

func TestTheDeclarationCountsOnlyOnANativeTicket(t *testing.T) {
	d := newIdentityDesktop(t, false, "")
	d.request(t, 1, "client.capabilities", pyjson.ObjectOf("server_requests", true, "desktop_local", true))
	state := replyResult(t, d.request(t, 2, "onboarding.state", pyjson.ObjectOf("assume_fresh", true)))
	if state["eligible"] != false {
		t.Fatalf("a non native client was offered onboarding %v", state)
	}
}

func TestAFreshLaunchClaimsTheTrialAndOpensOnboardingOnIt(t *testing.T) {
	var mu sync.Mutex
	held := map[string]any{}
	guest := map[string]any{"auth_method": provider.AnonAuthMethod, provider.WelcomeClaimFlag: true}
	t.Cleanup(provider.SwapCurrentNousStateForTests(func(provider.PoolEnv) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		if len(held) == 0 {
			return nil
		}
		out := map[string]any{}
		for k, v := range held {
			out[k] = v
		}
		return out
	}))
	t.Cleanup(provider.SwapWelcomeHasNousIdentityForTests(func(provider.PoolEnv) bool {
		mu.Lock()
		defer mu.Unlock()
		return len(held) > 0
	}))
	prevClaim := provider.ClaimDesktopFreeTier
	provider.ClaimDesktopFreeTier = func(env provider.PoolEnv, assumeFresh, _ bool) (map[string]any, error) {
		blocker, err := provider.ClaimBlocker(env, assumeFresh)
		if err != nil {
			return nil, err
		}
		if blocker != "" {
			return nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		out := map[string]any{}
		for k, v := range guest {
			held[k] = v
			out[k] = v
		}
		return out, nil
	}
	t.Cleanup(func() { provider.ClaimDesktopFreeTier = prevClaim })
	d := newIdentityDesktop(t, true, "")
	d.request(t, 1, "client.capabilities", pyjson.ObjectOf("desktop_local", true))
	started := d.request(t, 2, "onboarding.start", pyjson.ObjectOf("fresh_launch", true, "assume_fresh", true,
		"facts_source", "local", "locale", "en"))
	result := replyResult(t, started)
	resumed := d.request(t, 3, "session.resume", pyjson.ObjectOf("session_id", result["stored_session_id"]))
	if _, failed := resumed["error"]; failed {
		t.Fatalf("resume answered %v", resumed)
	}
}

func TestTheDesktopBootWorkRunsForTheLaunchProfileWhicheverProfileConnectsFirst(t *testing.T) {
	onboardingLaunchRoot(t)
	launch := home.Process()
	secondary := filepath.Join(t.TempDir(), "profiles", "work")
	mkdirAll(t, secondary)
	var mu sync.Mutex
	var seen []string
	prevLatch := tools.OnboardingLatchFirstInstall
	tools.OnboardingLatchFirstInstall = func(ctx context.Context) (bool, error) {
		mu.Lock()
		seen = append(seen, home.Dir(ctx))
		mu.Unlock()
		return false, nil
	}
	t.Cleanup(func() { tools.OnboardingLatchFirstInstall = prevLatch })
	localDesktopBootMu.Lock()
	prevStarted := localDesktopBootStarted
	localDesktopBootStarted = false
	localDesktopBootMu.Unlock()
	t.Cleanup(func() {
		localDesktopBootMu.Lock()
		localDesktopBootStarted = prevStarted
		localDesktopBootMu.Unlock()
	})
	provider.SetMultiplexActive(true)
	provider.ResetFreeTierBootstrapForTests()
	t.Cleanup(provider.ResetFreeTierBootstrapForTests)
	t.Cleanup(func() {
		tools.OnboardingDueRead(func() tools.OnboardingDue { return tools.OnboardingDue{} }, nil, 20*time.Second)
	})
	scoped := home.WithOverride(context.Background(), secondary)
	beginLocalDesktopBoot(scoped)
	if provider.WaitForSetupRecord(provider.HomePoolEnv(launch), 20*time.Second) == nil {
		t.Fatal("no setup record was built")
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{launch}) {
		t.Fatalf("latched in %v, want [%s]", got, launch)
	}
}
