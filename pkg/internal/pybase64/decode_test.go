package pybase64

import "testing"

func TestB64DecodeMatchesCPython(t *testing.T) {
	cases := []struct {
		in, want, err string
	}{
		{"aGVsbG8=", "hello", ""},
		{"aGVs\nbG8=", "hello", ""},
		{"aGVsbG8=junk", "hello", ""},
		{"QQ==QQ==", "A", ""},
		{"=QQ==", "A", ""},
		{"Q=Q==", "A", ""},
		{"aGVsbG8", "", "Incorrect padding"},
		{"aGVsb", "", "Invalid base64-encoded string: number of data characters (5) cannot be 1 more than a multiple of 4"},
		{"a", "", "Invalid base64-encoded string: number of data characters (1) cannot be 1 more than a multiple of 4"},
		{"", "", ""},
		{"!!!!", "", ""},
		{"héllo", "", "string argument should contain only ASCII characters"},
	}
	for _, c := range cases {
		got, err := B64Decode(c.in)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Fatalf("%q: error %v, want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || string(got) != c.want {
			t.Fatalf("%q: got %q %v, want %q", c.in, got, err, c.want)
		}
	}
}

func TestURLSafeB64DecodeMatchesCPython(t *testing.T) {
	for in, want := range map[string]string{"-_8=": "\xfb\xff", "+/8=": "\xfb\xff", "aGVsbG8": "", "aGVsbG8=": "hello"} {
		got, err := URLSafeB64Decode(in)
		if want == "" {
			if err == nil {
				t.Errorf("%q decoded to %q, want an error", in, got)
			}
			continue
		}
		if err != nil || string(got) != want {
			t.Errorf("%q = %q, %v want %q", in, got, err, want)
		}
	}
	if _, err := URLSafeB64Decode("caf\u00e9"); err != ErrNonASCII {
		t.Fatal(err)
	}
}
