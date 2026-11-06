package pyint

import (
	"math/big"
	"testing"
)

func TestInstanceMatchesPythonIsinstanceInt(t *testing.T) {
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	cases := []struct {
		in   any
		want string
		ok   bool
	}{
		{int64(7), "7", true}, {7, "7", true}, {true, "1", true}, {false, "0", true}, {huge, huge.String(), true},
		{3.0, "", false}, {"3", "", false}, {nil, "", false},
	}
	for _, c := range cases {
		got, ok := Instance(c.in)
		if ok != c.ok || (ok && got.String() != c.want) {
			t.Fatalf("Instance(%#v) = %v, %v", c.in, got, ok)
		}
	}
}
