package provider

import "testing"

func TestBackendRetirementReservesOnTheCurrentProcessFence(t *testing.T) {
	prev := Retirement
	t.Cleanup(func() { Retirement = prev })
	Retirement = NewRetirementFence()
	work := BackendRetirement()
	if !work.Acquire() || Retirement.ActiveCount() != 1 {
		t.Fatalf("acquire missed the process fence, active %d", Retirement.ActiveCount())
	}
	work.Release()
	if Retirement.ActiveCount() != 0 {
		t.Fatalf("release left %d admitted", Retirement.ActiveCount())
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
