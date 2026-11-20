package pybase64

import "testing"

func TestB64DecodeValidateMatchesStrictMode(t *testing.T) {
	ok := map[string]string{"": "", "aGk=": "hi", "aGVsbG8=": "hello", "YWJj": "abc"}
	for in, want := range ok {
		got, err := B64DecodeValidate(in)
		if err != nil || string(got) != want {
			t.Errorf("B64DecodeValidate(%q) = %q, %v", in, got, err)
		}
	}
	bad := map[string]string{
		"=aGk":      "Leading padding not allowed",
		"aGk=a":     "Excess data after padding",
		"aG=a=":     "Discontinuous padding not allowed",
		"aGk=\n":    "Excess data after padding",
		"aG k=":     "Only base64 data is allowed",
		"aGVsbA=":   "Incorrect padding",
		"YWJjZ":     "Invalid base64-encoded string: number of data characters (5) cannot be 1 more than a multiple of 4",
		"YWJj=":     "Excess padding not allowed",
		"aGVsbG8=x": "Excess data after padding",
	}
	for in, want := range bad {
		if _, err := B64DecodeValidate(in); err == nil || err.Error() != want {
			t.Errorf("B64DecodeValidate(%q) error = %v want %q", in, err, want)
		}
	}
}
