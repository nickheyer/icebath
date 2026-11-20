package tools

import (
	"context"
	"sync"
	"testing"
)

func TestDelegatedChildActiveReadsTheContextAndTheCheckEnv(t *testing.T) {
	env := &Env{Values: &sync.Map{}}
	if DelegatedChildActive(t.Context(), env) || DelegatedChildActive(nil, nil) {
		t.Fatal("a plain context counted as a delegate child")
	}
	child := WithDelegatedChildContext(t.Context(), true)
	if !DelegatedChildActive(child, env) || !DelegatedChildActive(child, nil) {
		t.Fatal("the child context was not seen")
	}
	if !DelegatedChildActive(t.Context(), checkEnvFor(child, env)) {
		t.Fatal("the env bound for a child's checks did not carry the mark")
	}
	if DelegatedChildActive(t.Context(), env) {
		t.Fatal("binding a check env marked the parent env")
	}
}

func TestCheckEnvForCopiesOnlyForAChild(t *testing.T) {
	env := &Env{Values: &sync.Map{}, Home: "/h"}
	if got := checkEnvFor(t.Context(), env); got != env {
		t.Fatal("a plain context copied the env")
	}
	if got := checkEnvFor(t.Context(), nil); got != nil {
		t.Fatal("a plain context synthesized an env")
	}
	child := WithDelegatedChildContext(t.Context(), true)
	bound := checkEnvFor(child, env)
	if bound == env || !bound.delegatedChild || bound.Values != env.Values || bound.Home != "/h" || bound.unscopedChecks {
		t.Fatalf("child copy %+v", bound)
	}
	if again := checkEnvFor(child, bound); again != bound {
		t.Fatal("an already marked env was copied again")
	}
	synthesized := checkEnvFor(child, nil)
	if synthesized == nil || !synthesized.delegatedChild || !synthesized.unscopedChecks {
		t.Fatalf("nil env under a child %+v", synthesized)
	}
}

func TestDelegatedChildProcessMarkerIsAnyNonEmptyValue(t *testing.T) {
	env := &Env{Values: &sync.Map{}}
	t.Setenv(delegatedChildEnvMarker, "")
	if isDelegatedChildProcessContext(env) || isDelegatedChildProcessContext(nil) || DelegatedChildProcessActive(t.Context(), env) {
		t.Fatal("an empty marker counted as a child process")
	}
	if !DispatcherOwnedWorkerActive(t.Context(), env) {
		t.Fatal("a plain execution was not dispatcher owned")
	}
	for _, marker := range []string{"1", "0", "false", "/tmp/kanban-root"} {
		t.Setenv(delegatedChildEnvMarker, marker)
		if !isDelegatedChildProcessContext(env) || !isDelegatedChildProcessContext(nil) {
			t.Fatalf("marker %q did not count as a child process", marker)
		}
		if isDispatcherOwnedWorkerContext(env) || DispatcherOwnedWorkerActive(t.Context(), env) {
			t.Fatalf("marker %q still dispatcher owned", marker)
		}
	}
	env.Values.Store("session.delegated_child", false)
	if !isDelegatedChildProcessContext(env) {
		t.Fatal("a false session flag hid the process marker")
	}
	t.Setenv(delegatedChildEnvMarker, "")
	if isDelegatedChildProcessContext(env) {
		t.Fatal("a false session flag counted as a child")
	}
	env.Values.Store("session.delegated_child", true)
	if !isDelegatedChildProcessContext(env) {
		t.Fatal("the session flag was not read")
	}
	env.Values.Delete("session.delegated_child")
	child := WithDelegatedChildContext(t.Context(), true)
	if !DelegatedChildProcessActive(child, env) || DispatcherOwnedWorkerActive(child, env) {
		t.Fatal("the child context was not a child process or stayed dispatcher owned")
	}
	if sel := definitionSelectionContext(child, env); !sel.DelegatedChild || sel.DispatcherOwnedWorker {
		t.Fatalf("selection context under a child %+v", sel)
	}
	if sel := definitionSelectionContext(t.Context(), env); sel.DelegatedChild || !sel.DispatcherOwnedWorker {
		t.Fatalf("selection context of the parent %+v", sel)
	}
}

func TestAvailabilityChecksSeeTheDelegateChild(t *testing.T) {
	r := NewRegistry()
	var seen []bool
	check := NoCacheCheck(func(env *Env) bool {
		seen = append(seen, DelegatedChildActive(context.Background(), env))
		return true
	})
	env := &Env{Values: &sync.Map{}}
	r.checkCached(t.Context(), env, check)
	r.checkCached(WithDelegatedChildContext(t.Context(), true), env, check)
	r.checkCached(WithDelegatedChildContext(t.Context(), true), nil, check)
	if len(seen) != 3 || seen[0] || !seen[1] || !seen[2] {
		t.Fatalf("checks saw %v", seen)
	}
}

func TestPyContainsKanbanMatchesPythonMembership(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want bool
	}{{nil, false}, {[]any{"web", "kanban"}, true}, {[]any{"kanbans"}, false}, {[]string{"kanban"}, true}, {"my-kanban-set", true}, {"", false},
		{map[string]any{"kanban": false}, true}, {map[string]any{"web": true}, false}, {int64(0), false}} {
		if got := PyContainsKanban(tc.v); got != tc.want {
			t.Fatalf("PyContainsKanban(%#v) = %v", tc.v, got)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a truthy non iterable did not raise")
		}
	}()
	PyContainsKanban(int64(5))
}
