package tools

import (
	"sync"
	"testing"
)

// Fresh env carrying only the per session values map
func contextEnv() *Env { return &Env{Values: &sync.Map{}} }

func TestFalseInsideNonDispatcherContext(t *testing.T) {
	env := contextEnv()
	exit := EnterNonDispatcherOwnedContext(env)
	if IsDispatcherOwnedWorkerContext(env) {
		t.Fatal("dispatcher owned inside the non dispatcher scope")
	}
	exit()
	if !IsDispatcherOwnedWorkerContext(env) {
		t.Fatal("not dispatcher owned after the scope ended")
	}
}

func TestTokenFormRestores(t *testing.T) {
	env := contextEnv()
	restore := EnterNonDispatcherOwnedContext(env)
	if IsDispatcherOwnedWorkerContext(env) {
		t.Fatal("dispatcher owned while the token is held")
	}
	restore()
	if !IsDispatcherOwnedWorkerContext(env) {
		t.Fatal("the token restore did not clear the flag")
	}
}

func TestNestingRestoresOuterValue(t *testing.T) {
	env := contextEnv()
	outer := EnterNonDispatcherOwnedContext(env)
	inner := EnterNonDispatcherOwnedContext(env)
	if IsDispatcherOwnedWorkerContext(env) {
		t.Fatal("dispatcher owned inside the nested scope")
	}
	inner()
	if IsDispatcherOwnedWorkerContext(env) {
		t.Fatal("the inner exit cleared the outer scope")
	}
	outer()
	if !IsDispatcherOwnedWorkerContext(env) {
		t.Fatal("not dispatcher owned after both scopes ended")
	}
}

func TestDelegatedChildStillNotDispatcherOwned(t *testing.T) {
	env := contextEnv()
	env.Values.Store("session.delegated_child", true)
	if IsDispatcherOwnedWorkerContext(env) {
		t.Fatal("a delegated child counted as dispatcher owned")
	}
}

func TestThreadIsolation(t *testing.T) {
	t.Setenv("ICEBATH_KANBAN_TASK", "t_worker_real_task")
	t.Setenv("ICEBATH_KANBAN_WORKSPACE", "/tmp/ws")
	t.Setenv("ICEBATH_KANBAN_RUN_ID", "42")
	t.Setenv("ICEBATH_KANBAN_CLAIM_LOCK", "lock-abc")
	t.Setenv("ICEBATH_KANBAN_BOARD", "team-alpha")
	seen := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		job := contextEnv()
		exit := EnterNonDispatcherOwnedContext(job)
		defer exit()
		mu.Lock()
		seen["job"] = IsDispatcherOwnedWorkerContext(job)
		mu.Unlock()
		var inner sync.WaitGroup
		inner.Add(1)
		go func() {
			defer inner.Done()
			sibling := contextEnv()
			mu.Lock()
			seen["sibling"] = IsDispatcherOwnedWorkerContext(sibling)
			mu.Unlock()
		}()
		inner.Wait()
	}()
	wg.Wait()
	if seen["job"] {
		t.Fatal("job must be marked non-dispatcher")
	}
	if !seen["sibling"] {
		t.Fatal("sibling must be unaffected")
	}
}
