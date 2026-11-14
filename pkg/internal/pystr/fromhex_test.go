package pystr

import (
	"bytes"
	"testing"
)

func TestBytesFromHexMatchesCPython(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []byte
		err  string
	}{
		{"", []byte{}, ""},
		{"0aFf", []byte{0x0a, 0xff}, ""},
		{" 0a \t\nff\x0b\x0c", []byte{0x0a, 0xff}, ""},
		{"0 a", nil, "non-hexadecimal number found in fromhex() arg at position 1"},
		{"abc", nil, "non-hexadecimal number found in fromhex() arg at position 3"},
		{"zz", nil, "non-hexadecimal number found in fromhex() arg at position 0"},
		{"é0a", nil, "non-hexadecimal number found in fromhex() arg at position 0"},
		{"0aé", nil, "non-hexadecimal number found in fromhex() arg at position 2"},
	} {
		got, err := BytesFromHex(tc.in)
		if tc.err != "" {
			if err == nil || err.Error() != tc.err {
				t.Fatalf("%q error %v, want %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, tc.want) {
			t.Fatalf("%q = %x, %v", tc.in, got, err)
		}
	}
}
