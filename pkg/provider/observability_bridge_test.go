package provider

import "testing"

// Enum construction accepts every reason value and refuses anything else
func TestParseFailoverReason(t *testing.T) {
	if r, ok := ParseFailoverReason("rate_limit"); !ok || r != ReasonRateLimit {
		t.Fatalf("rate_limit = %v %v", r, ok)
	}
	if r, ok := ParseFailoverReason(ReasonAuth); !ok || r != ReasonAuth {
		t.Fatalf("enum member = %v %v", r, ok)
	}
	for _, bad := range []any{"nope", 3, nil} {
		if _, ok := ParseFailoverReason(bad); ok {
			t.Fatalf("%v accepted", bad)
		}
	}
}
