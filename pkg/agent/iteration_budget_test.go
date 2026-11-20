package agent

import (
	"math"
	"sync"
	"testing"
)

func TestIterationBudgetUsedIsThreadSafe(t *testing.T) {
	budget := NewIterationBudget(1000)
	var wg sync.WaitGroup
	for i := range 10 {
		consume := i < 5
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 200 {
					if consume {
						budget.Consume()
					} else {
						budget.Refund()
					}
					_ = budget.Used()
				}
			}()
		}
	}
	wg.Wait()
	if u := budget.Used(); u < 0 || u > budget.MaxTotal {
		t.Fatalf("used %d outside 0..%d", u, budget.MaxTotal)
	}
}

func TestIterationBudgetConsumeReturnsFalseWhenExhausted(t *testing.T) {
	budget := NewIterationBudget(3)
	for i := range 3 {
		if !budget.Consume() {
			t.Fatalf("consume %d refused", i)
		}
	}
	if budget.Consume() {
		t.Fatal("consume past the cap allowed")
	}
}

func TestIterationBudgetRefundRestoresConsume(t *testing.T) {
	budget := NewIterationBudget(2)
	budget.Consume()
	budget.Consume()
	if budget.Consume() {
		t.Fatal("exhausted budget allowed a consume")
	}
	budget.Refund()
	if !budget.Consume() {
		t.Fatal("refund did not restore a consume")
	}
}

func TestIterationBudgetUsedReflectsConsumeAndRefund(t *testing.T) {
	budget := NewIterationBudget(10)
	steps := []struct {
		consume bool
		want    int
	}{{true, 1}, {true, 2}, {false, 1}, {false, 0}}
	if budget.Used() != 0 {
		t.Fatalf("fresh used %d", budget.Used())
	}
	for _, s := range steps {
		if s.consume {
			budget.Consume()
		} else {
			budget.Refund()
		}
		if budget.Used() != s.want {
			t.Fatalf("used %d want %d", budget.Used(), s.want)
		}
	}
}

func TestIterationBudgetRemaining(t *testing.T) {
	budget := NewIterationBudget(5)
	if budget.Remaining() != 5 {
		t.Fatalf("remaining %d", budget.Remaining())
	}
	budget.Consume()
	if budget.Remaining() != 4 {
		t.Fatalf("remaining %d", budget.Remaining())
	}
	budget.Consume()
	budget.Consume()
	if budget.Remaining() != 2 {
		t.Fatalf("remaining %d", budget.Remaining())
	}
	budget.Refund()
	if budget.Remaining() != 3 {
		t.Fatalf("remaining %d", budget.Remaining())
	}
}

func TestNormalizeBudgetWarningRatio(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{nil, 0, false}, {true, 0, false}, {false, 0, false}, {0.5, 0.5, true}, {"0.25", 0.25, true},
		{" 0.7 ", 0.7, true}, {"2_5e-2", 0.25, true}, {0, 0, false}, {1, 0, false}, {1.0, 0, false},
		{-0.2, 0, false}, {math.NaN(), 0, false}, {math.Inf(1), 0, false}, {"nan", 0, false},
		{"abc", 0, false}, {[]int{1}, 0, false},
	}
	for _, c := range cases {
		got, ok := NormalizeBudgetWarningRatio(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%#v -> %v %v want %v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
