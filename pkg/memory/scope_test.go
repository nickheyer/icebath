package memory

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Two named profile homes under a temp root
func profileHomes(t *testing.T) (string, string) {
	root := filepath.Join(t.TempDir(), ".icebath")
	alpha, beta := filepath.Join(root, "profiles", "alpha"), filepath.Join(root, "profiles", "beta")
	for _, h := range []string{alpha, beta} {
		if err := os.MkdirAll(h, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return alpha, beta
}

func multiplexed(ctx context.Context) context.Context {
	return provider.WithMultiplexContext(ctx, true)
}

func TestCallFromAnotherProfilesScopeIsRefused(t *testing.T) {
	alpha, beta := profileHomes(t)
	bound := NewBoundScope(alpha, "")
	var scope ProviderScope
	var allowed bool
	logs := captureDefaultLog(func() {
		scope, allowed = CallerScope(home.WithOverride(t.Context(), beta), bound, "tool pw_remember")
	})
	if allowed || scope != (ProviderScope{}) {
		t.Fatal(scope)
	}
	if !strings.Contains(logs, CallerProfileRefusal) || !strings.Contains(logs, alpha) || !strings.Contains(logs, beta) {
		t.Fatal(logs)
	}
}

func TestMultiplexedCallWithNoKnownProfileIsRefused(t *testing.T) {
	alpha, _ := profileHomes(t)
	logs := captureDefaultLog(func() {
		if _, allowed := CallerScope(home.WithOverride(multiplexed(t.Context()), alpha), nil, "tool pw_remember"); allowed {
			t.Fatal("allowed")
		}
	})
	if !strings.Contains(logs, "the memory manager recorded no profile") || !strings.Contains(logs, alpha) {
		t.Fatal(logs)
	}
}

func TestMultiplexedCallWithoutACallerScopeIsRefusedEvenForTheProcessProfile(t *testing.T) {
	alpha, _ := profileHomes(t)
	t.Setenv("ICEBATH_HOME", alpha)
	logs := captureDefaultLog(func() {
		if _, allowed := CallerScope(multiplexed(t.Context()), NewBoundScope(alpha, ""), "tool pw_remember"); allowed {
			t.Fatal("allowed")
		}
	})
	if !strings.Contains(logs, "the caller names no profile on a process serving several") || !strings.Contains(logs, alpha) {
		t.Fatal(logs)
	}
}

func TestSingleProfileProcessKeepsWorkingWithoutInitialize(t *testing.T) {
	alpha, _ := profileHomes(t)
	scope, allowed := CallerScope(home.WithOverride(t.Context(), alpha), nil, "tool pw_remember")
	if !allowed || scope.Home != alpha || scope.AgentIdentity != "" {
		t.Fatal(scope, allowed)
	}
	t.Setenv("ICEBATH_HOME", alpha)
	scope, allowed = CallerScope(t.Context(), nil, "tool pw_remember")
	if !allowed || scope.Home != home.Dir(t.Context()) {
		t.Fatal(scope, allowed)
	}
}

func TestMatchingCallerScopeCarriesTheBoundHomeAndIdentity(t *testing.T) {
	alpha, _ := profileHomes(t)
	bound := NewBoundScope(alpha, "alpha")
	for _, ctx := range []context.Context{
		home.WithOverride(multiplexed(t.Context()), alpha),
		provider.WithSecretScope(multiplexed(t.Context()), map[string]string{}, alpha),
	} {
		scope, allowed := CallerScope(ctx, bound, "hook sync_turn")
		if !allowed || scope.Home != alpha || scope.AgentIdentity != "alpha" {
			t.Fatal(scope, allowed)
		}
	}
}

func TestToolCallsReuseTheKeyInitializeBound(t *testing.T) {
	alpha, beta := profileHomes(t)
	bound := &BoundScope{Home: beta, HomeKey: home.Key(alpha)}
	if _, allowed := CallerScope(home.WithOverride(t.Context(), alpha), bound, "tool x"); !allowed {
		t.Fatal("bound key ignored")
	}
}

func TestBackgroundHookRunsUnderTheOwningProfileExplicitly(t *testing.T) {
	alpha, _ := profileHomes(t)
	run := InOwnerHome(NewBoundScope(alpha, ""), func(ctx context.Context) string { return home.Override(ctx) })
	if got := run(multiplexed(t.Context())); got != alpha {
		t.Fatal(got)
	}
	plain := InOwnerHome(nil, func(ctx context.Context) string { return home.Override(ctx) })
	if got := plain(t.Context()); got != "" {
		t.Fatal(got)
	}
}

type instanceProvider struct {
	name string
	tags map[string]bool
}

func (p *instanceProvider) Name() string { return p.name }

func TestOneProviderInstanceIsNeverServedToASecondProfile(t *testing.T) {
	alpha, beta := profileHomes(t)
	shared := &instanceProvider{name: "instancescoped", tags: map[string]bool{}}
	ctx := multiplexed(t.Context())
	if !ClaimInstance(ctx, shared, alpha) {
		t.Fatal("first claim refused")
	}
	var second bool
	logs := captureDefaultLog(func() { second = ClaimInstance(ctx, shared, beta) })
	if second || !strings.Contains(logs, SharedInstanceRefusal) || !strings.Contains(logs, beta) || !strings.Contains(logs, "instancescoped") {
		t.Fatal(second, logs)
	}
	if !ClaimInstance(ctx, shared, alpha) {
		t.Fatal("owner reclaim refused")
	}
	runtime.KeepAlive(shared)
}

func TestValueEqualDistinctInstancesAreEachServed(t *testing.T) {
	alpha, beta := profileHomes(t)
	ctx := multiplexed(t.Context())
	first, second := &instanceProvider{name: "same"}, &instanceProvider{name: "same"}
	if !ClaimInstance(ctx, first, alpha) || !ClaimInstance(ctx, second, beta) {
		t.Fatal("distinct instances refused")
	}
	runtime.KeepAlive(first)
	runtime.KeepAlive(second)
}

func TestSharedInstanceRebindsFreelyWhenNotMultiplexed(t *testing.T) {
	alpha, beta := profileHomes(t)
	shared := &instanceProvider{name: "single"}
	if !ClaimInstance(t.Context(), shared, alpha) || !ClaimInstance(t.Context(), shared, beta) {
		t.Fatal("single profile rebind refused")
	}
	runtime.KeepAlive(shared)
}

type valueProvider struct{ name string }

func (p valueProvider) Name() string { return p.name }

func TestUntrackableProviderInstanceFailsClosedOnlyWhenMultiplexed(t *testing.T) {
	alpha, _ := profileHomes(t)
	if !ClaimInstance(t.Context(), valueProvider{"plain"}, alpha) {
		t.Fatal("single profile refused")
	}
	if ClaimInstance(multiplexed(t.Context()), valueProvider{"plain"}, alpha) {
		t.Fatal("multiplexed untrackable allowed")
	}
}

func TestCollectedInstanceReleasesItsClaim(t *testing.T) {
	alpha, beta := profileHomes(t)
	ctx := multiplexed(t.Context())
	func() {
		gone := &instanceProvider{name: "gone"}
		ClaimInstance(ctx, gone, alpha)
	}()
	runtime.GC()
	runtime.GC()
	claimsMu.Lock()
	sweepDeadClaims()
	left := len(claims)
	claimsMu.Unlock()
	fresh := &instanceProvider{name: "fresh"}
	if !ClaimInstance(ctx, fresh, beta) {
		t.Fatal("fresh instance refused")
	}
	runtime.KeepAlive(fresh)
	if left > 0 {
		for _, c := range claims {
			if c.ref.Value() == nil {
				t.Fatal("dead claim survived a sweep")
			}
		}
	}
}

func TestRefusedToolMessageNamesNoHomes(t *testing.T) {
	if got := RefusedToolMessage("pw_remember"); got != "Memory tool 'pw_remember' refused. Ask the operator to check `icebath logs errors`." {
		t.Fatal(got)
	}
}
