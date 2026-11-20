package pydict

import (
	"reflect"
	"testing"
)

func TestDictKeepsPythonInsertionOrder(t *testing.T) {
	d := New[string, int]()
	for i, k := range []string{"zeta", "alpha", "mid"} {
		d.Set(k, i)
	}
	d.Set("zeta", 9)
	if got := d.Keys(); !reflect.DeepEqual(got, []string{"zeta", "alpha", "mid"}) {
		t.Fatalf("reassignment moved a key %v", got)
	}
	if !d.Delete("alpha") || d.Delete("alpha") || d.Has("alpha") {
		t.Fatal("delete")
	}
	d.Set("alpha", 1)
	if got := d.Values(); !reflect.DeepEqual(got, []int{9, 2, 1}) {
		t.Fatalf("reinserted key must go last %v", got)
	}
	other := New[string, int]()
	other.Set("new", 5)
	other.Set("mid", 7)
	m := Merge(d, other)
	if !reflect.DeepEqual(m.Keys(), []string{"zeta", "mid", "alpha", "new"}) || !reflect.DeepEqual(m.Values(), []int{9, 7, 1, 5}) {
		t.Fatalf("merge %v %v", m.Keys(), m.Values())
	}
	var empty *Dict[string, int]
	if empty.Len() != 0 || empty.Has("x") || len(Merge(empty, other).Keys()) != 2 {
		t.Fatal("nil dict must read as empty")
	}
}
