package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNestedPluginLoadRunsInlineOnDeadlineWorker(t *testing.T) {
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  load_timeout_seconds: 0.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(h)
	named := func(name string) *Context { return newContext(&Manifest{Name: name, Key: name}, m) }
	outer, done, hung := named("outer"), named("done"), named("hung")
	var observed []*loadScope
	release := make(chan struct{})
	outerLoad := func(ctx context.Context) (bool, error) {
		scope, _ := ctx.Value(loadScopeKey{}).(*loadScope)
		observed = append(observed, scope)
		if _, err := runWithLoadDeadline(ctx, "done", done, func(ctx context.Context) (bool, error) {
			scope, _ := ctx.Value(loadScopeKey{}).(*loadScope)
			observed = append(observed, scope)
			return true, nil
		}); err != nil {
			return false, err
		}
		return runWithLoadDeadline(ctx, "hung", hung, func(context.Context) (bool, error) {
			<-release
			return true, nil
		})
	}
	_, err := runWithLoadDeadline(homeCtx(h), "outer", outer, outerLoad)
	var timeout *PluginLoadTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("outer load returned %v", err)
	}
	abandoned := []string{}
	for _, c := range []*Context{outer, done, hung} {
		if c.loadAbandoned.Load() {
			abandoned = append(abandoned, c.manifest.Name)
		}
	}
	close(release)
	if len(observed) != 2 || observed[0] == nil || observed[0] != observed[1] {
		t.Fatalf("nested load left the deadline worker: %v", observed)
	}
	if !reflect.DeepEqual(abandoned, []string{"outer", "hung"}) {
		t.Fatalf("abandoned %v", abandoned)
	}
}
