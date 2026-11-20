package tools

import (
	"errors"
	"testing"
)

func TestDelegationRouteExports(t *testing.T) {
	if msg := DelegationParamError(0, "tier", "x", map[string]any{"enum": []any{"a", "b"}}); msg != "Task 0: 'tier' must be one of a, b (got 'x')." {
		t.Fatalf("msg %q", msg)
	}
	route, err := CleanDelegationRoute("b", map[string]any{"model": "m", "junk": 1})
	if err != nil || len(route) != 1 || route["model"] != "m" {
		t.Fatalf("route %v err %v", route, err)
	}
	var routeErr *DelegationRouteError
	if _, err := CleanDelegationRoute("b", nil); !errors.As(err, &routeErr) {
		t.Fatalf("err %v", err)
	}
	var valueErr *DelegationValueError
	if err := RequirePinnedCommand("icebath-test-missing-binary", "missing"); !errors.As(err, &valueErr) || err.Error() != "missing" {
		t.Fatalf("err %v", err)
	}
	if err := RequirePinnedCommand("", "missing"); err != nil {
		t.Fatal(err)
	}
	if b := CredentialBundle("m", "p", nil, "k", nil, map[string]any{}, map[string]any{"command": nil}); len(b) != 7 || b["model"] != "m" {
		t.Fatalf("bundle %v", b)
	}
}
