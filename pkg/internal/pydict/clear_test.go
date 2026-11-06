package pydict

import "testing"

// Clearing empties a dict in place so every holder of the pointer sees it and later inserts start fresh
func TestClearKeepsIdentity(t *testing.T) {
	d := New[string, int]()
	d.Set("a", 1)
	d.Set("b", 2)
	alias := d
	d.Clear()
	if alias.Len() != 0 || alias.Has("a") || len(alias.Keys()) != 0 {
		t.Fatalf("clear left %v", alias.Keys())
	}
	d.Set("c", 3)
	d.Set("a", 4)
	if got := alias.Keys(); len(got) != 2 || got[0] != "c" || got[1] != "a" {
		t.Fatalf("keys after clear %v", got)
	}
}
