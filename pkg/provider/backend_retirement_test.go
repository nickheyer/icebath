package provider

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// Swaps the registered idle proof for the test and restores it afterwards
func withRetirementIdleProof(t *testing.T, probe func() (map[string]any, error)) {
	prev := RetirementIdleProof
	RetirementIdleProof = probe
	t.Cleanup(func() { RetirementIdleProof = prev })
}

func idleTrueProof() (map[string]any, error) { return map[string]any{"idle": true, "reason": nil}, nil }

func TestOnlyUncommittedPermitsExpireAndUnreadableWorkNeverGrants(t *testing.T) {
	withRetirementIdleProof(t, idleTrueProof)
	fence := NewRetirementFence()
	now := 100.0
	fence.now = func() float64 { return now }
	eq := func(got, want map[string]any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
	}
	first, _ := fence.Prepare()["token"].(string)
	now += 30.0
	eq(fence.Commit(first), map[string]any{"ok": false})
	eq(fence.Cancel(first), map[string]any{"ok": false})
	admitted, done := fence.Work()
	if !admitted {
		t.Fatal("work refused after the permit expired")
	}
	eq(fence.Prepare(), map[string]any{"ok": false, "idle": false})
	done()
	second, _ := fence.Prepare()["token"].(string)
	if first == second {
		t.Fatal("expired permit reissued")
	}
	eq(NewRetirementFence().Commit(second), map[string]any{"ok": false})
	now += 29.0
	eq(fence.Commit(second), map[string]any{"ok": true})
	now += 3600.0
	eq(fence.Commit(second), map[string]any{"ok": true})
	eq(fence.Prepare(), map[string]any{"ok": true, "idle": true, "token": second})
	eq(fence.Cancel(second), map[string]any{"ok": false})
	admitted, done = fence.Work()
	done()
	if admitted {
		t.Fatal("committed fence admitted work")
	}
	withRetirementIdleProof(t, func() (map[string]any, error) { return map[string]any{"idle": nil}, nil })
	eq(NewRetirementFence().Prepare(), map[string]any{"ok": false, "idle": nil})
}

func TestRetirementPermitBlocksAdmissionUntilCancelAndCommitIsPermanent(t *testing.T) {
	withRetirementIdleProof(t, idleTrueProof)
	fence := NewRetirementFence()
	if !fence.Acquire() || fence.ActiveCount() != 1 {
		t.Fatal("open fence refused work")
	}
	fence.Release()
	permit := fence.Prepare()
	token, _ := permit["token"].(string)
	if len(token) != 43 || !reflect.DeepEqual(permit, map[string]any{"ok": true, "idle": true, "token": token}) {
		t.Fatalf("permit %#v", permit)
	}
	if fence.Acquire() {
		t.Fatal("work admitted under a permit")
	}
	if got := fence.Prepare(); !reflect.DeepEqual(got, map[string]any{"ok": false, "idle": false}) {
		t.Fatalf("second prepare %#v", got)
	}
	if got := fence.Cancel("wrong-token"); got["ok"] != false || fence.Acquire() {
		t.Fatal("wrong token cancelled the permit")
	}
	if got := fence.Cancel(token); got["ok"] != true || !fence.Acquire() {
		t.Fatal("cancel left admission closed")
	}
	fence.Release()
	next, _ := fence.Prepare()["token"].(string)
	if next == token || fence.Commit(token)["ok"] != false || fence.Commit(next)["ok"] != true || fence.Commit(next)["ok"] != true {
		t.Fatal("commit accepted a stale token or refused the live one")
	}
	if fence.Cancel(next)["ok"] != false || fence.Acquire() {
		t.Fatal("committed fence reopened")
	}
	if got := fence.Prepare(); !reflect.DeepEqual(got, map[string]any{"ok": true, "idle": true, "token": next}) {
		t.Fatalf("lost commit recovery %#v", got)
	}
}

func TestRetirementProbeFailuresAreIndeterminate(t *testing.T) {
	for name, probe := range map[string]func() (map[string]any, error){
		"unregistered": nil,
		"raises":       func() (map[string]any, error) { return nil, errors.New("ledger unreadable") },
		"panics":       func() (map[string]any, error) { panic("boom") },
		"no idle key":  func() (map[string]any, error) { return map[string]any{"reason": "x"}, nil },
		"busy":         func() (map[string]any, error) { return map[string]any{"idle": false}, nil },
		"truthy":       func() (map[string]any, error) { return map[string]any{"idle": 1}, nil },
	} {
		t.Run(name, func(t *testing.T) {
			withRetirementIdleProof(t, probe)
			fence := NewRetirementFence()
			got := fence.Prepare()
			want := map[string]any{"ok": false, "idle": nil}
			switch name {
			case "busy":
				want["idle"] = false
			case "truthy":
				want["idle"] = 1
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v want %#v", got, want)
			}
			if !fence.Acquire() {
				t.Fatal("refused prepare left admission frozen")
			}
		})
	}
}

func TestPrepareFreezesAdmissionWithoutBlockingOtherWorkLocks(t *testing.T) {
	fence := NewRetirementFence()
	probing, release, attempted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	withRetirementIdleProof(t, func() (map[string]any, error) {
		close(probing)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			panic("release never set")
		}
		return map[string]any{"idle": nil}, nil
	})
	preparing := make(chan map[string]any, 1)
	go func() { preparing <- fence.Prepare() }()
	arriving := make(chan bool, 1)
	func() {
		defer close(release)
		select {
		case <-probing:
		case <-time.After(10 * time.Second):
			t.Fatal("probe never started")
		}
		go func() {
			admitted, done := fence.Work()
			close(attempted)
			done()
			arriving <- admitted
		}()
		select {
		case <-attempted:
		case <-time.After(3 * time.Second):
			t.Fatal("admission must not wait on a ledger lock held by its own caller")
		}
		if <-arriving {
			t.Fatal("work admitted across the prepare freeze")
		}
	}()
	if got := <-preparing; !reflect.DeepEqual(got, map[string]any{"ok": false, "idle": nil}) {
		t.Fatalf("prepare %#v", got)
	}
	admitted, done := fence.Work()
	defer done()
	if !admitted {
		t.Fatal("unreadable probe left a fence behind")
	}
}

func TestProbeFailureRefusesThePermitWithoutWedgingAdmission(t *testing.T) {
	fence := NewRetirementFence()
	withRetirementIdleProof(t, func() (map[string]any, error) { return nil, errors.New("ledger unavailable") })
	if got := fence.Prepare(); !reflect.DeepEqual(got, map[string]any{"ok": false, "idle": nil}) {
		t.Fatalf("prepare %#v", got)
	}
	admitted, done := fence.Work()
	defer done()
	if !admitted {
		t.Fatal("probe failure wedged admission")
	}
}

func TestBackendRetirementReservesOnTheCurrentProcessFence(t *testing.T) {
	withRetirementIdleProof(t, idleTrueProof)
	prev := Retirement
	t.Cleanup(func() { Retirement = prev })
	Retirement = NewRetirementFence()
	work := BackendRetirement()
	if !work.Acquire() || Retirement.ActiveCount() != 1 {
		t.Fatalf("acquire missed the process fence, active %d", Retirement.ActiveCount())
	}
	if got := Retirement.Prepare(); !reflect.DeepEqual(got, map[string]any{"ok": false, "idle": false}) {
		t.Fatalf("prepare with work in flight %#v", got)
	}
	work.Release()
	token, _ := Retirement.Prepare()["token"].(string)
	if token == "" {
		t.Fatal("idle fence refused the permit")
	}
	if admitted, done := work.Work(); admitted {
		done()
		t.Fatal("work admitted while a permit is outstanding")
	}
	swapped := NewRetirementFence()
	Retirement = swapped
	admitted, done := work.Work()
	if !admitted || swapped.ActiveCount() != 1 {
		t.Fatal("reservation did not follow the swapped process fence")
	}
	done()
	if swapped.ActiveCount() != 0 {
		t.Fatalf("release left %d admitted", swapped.ActiveCount())
	}
}
