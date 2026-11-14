package selfupdate

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

func TestUpdatePathWaitsForAHeldLock(t *testing.T) {
	root := dblCheckout(t)
	t.Setenv("ICEBATH_RUNTIME_DIR", filepath.Join(t.TempDir(), "tools"))
	ctx := context.Background()
	holder := NewDesktopBuildLock(root)
	if !dblAcquire(t, holder, false) {
		t.Fatal("holder refused")
	}
	var releaseCalls []bool
	var built []bool
	prev := productSteps
	t.Cleanup(func() { productSteps = prev })
	productSteps.featureDeps = func(context.Context, string) error { return nil }
	productSteps.memoryMigrate = func(context.Context, func(string)) ([]string, error) { return nil, nil }
	productSteps.leftCoreMigrate = func(context.Context, func(string)) ([]string, error) { return nil, nil }
	productSteps.frontends = func(string) []string { return []string{"web"} }
	productSteps.buildEnv = func(context.Context, map[string]string, bool, bool) (map[string]string, error) {
		return map[string]string{"PATH": "/usr/bin"}, nil
	}
	productSteps.prepareDeps = func(context.Context, string, []string, map[string]string, bool) error { return nil }
	productSteps.buildWeb = func(context.Context, string, map[string]string, string) error { return nil }
	productSteps.refreshApps = func(context.Context, string) {}
	productSteps.publishStage = func(string) {}
	productSteps.desktopLock = func(ctx context.Context, projectRoot string, wait bool) (*DesktopBuildLock, error) {
		releaseCalls = append(releaseCalls, wait)
		holder.Release(ctx)
		lock := NewDesktopBuildLock(projectRoot)
		lock.Out = &strings.Builder{}
		_, err := lock.Acquire(ctx, wait)
		return lock, err
	}
	productSteps.buildDesktop = func(context.Context, string, bool, string, map[string]string, string) (string, error) {
		built = append(built, true)
		return "", nil
	}
	t.Cleanup(SwapSourceToolsForTests(func(_ context.Context, name string, opts pm.EnsureOptions) (*pm.Runner, error) {
		return &pm.Runner{Name: name, Env: opts.BaseEnv}, nil
	}, func(context.Context) (string, bool) { return "", false }))
	sbCapture(t)
	if err := BuildUpdateProducts(ctx, root, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(releaseCalls, []bool{true}) {
		t.Fatalf("update path must acquire the lock in wait mode, calls %v", releaseCalls)
	}
	if !reflect.DeepEqual(built, []bool{true}) {
		t.Fatalf("built %v", built)
	}
}
