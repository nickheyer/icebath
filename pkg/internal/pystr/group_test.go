package pystr

import "testing"

func TestGroupThousands(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 12: "12", 999: "999", 1000: "1,000", 123456: "123,456", 1234567: "1,234,567", -1234: "-1,234", -999: "-999"} {
		if got := GroupThousands(n); got != want {
			t.Errorf("GroupThousands(%d) = %q, want %q", n, got, want)
		}
	}
}
