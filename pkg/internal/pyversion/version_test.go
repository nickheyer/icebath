package pyversion

import "testing"

func TestCompareFollowsPackagingOrder(t *testing.T) {
	ordered := []string{"1.0.dev456", "1.0a1", "1.0a2.dev456", "1.0a12.dev456", "1.0a12", "1.0b1.dev456", "1.0b2",
		"1.0b2.post345.dev456", "1.0b2.post345", "1.0rc1.dev456", "1.0rc1", "1.0", "1.0+abc.5", "1.0+abc.7",
		"1.0+5", "1.0.post456.dev34", "1.0.post456", "1.1.dev1", "1.4.0", "1.10", "1!0.1"}
	for i := 0; i+1 < len(ordered); i++ {
		a, err := Parse(ordered[i])
		if err != nil {
			t.Fatal(err)
		}
		b, err := Parse(ordered[i+1])
		if err != nil {
			t.Fatal(err)
		}
		if Compare(a, b) != -1 || Compare(b, a) != 1 {
			t.Fatalf("%s should sort before %s", ordered[i], ordered[i+1])
		}
	}
	equal := [][2]string{{"1.0", "1.0.0"}, {"1.0alpha1", "1.0a1"}, {"1.0-1", "1.0.post1"}, {"V1.0C2", "1.0rc2"}, {" 2.0 ", "2.0"}}
	for _, pair := range equal {
		a, _ := Parse(pair[0])
		b, _ := Parse(pair[1])
		if a == nil || b == nil || Compare(a, b) != 0 {
			t.Fatalf("%q should equal %q", pair[0], pair[1])
		}
	}
	for _, bad := range []string{"1.4.0 beta", "", "latest", "1.0+", "1..0"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("%q should be invalid", bad)
		}
	}
	if _, err := Parse("nope"); err.Error() != "Invalid version: 'nope'" {
		t.Fatalf("message %q", err)
	}
}
