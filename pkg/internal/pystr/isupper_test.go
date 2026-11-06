package pystr

import "testing"

// Uppercase detection follows CPython over cased, uncased, titlecase and Other_Lowercase characters
func TestIsUpper(t *testing.T) {
	cases := map[string]bool{
		"": false, "TTS": true, "A1 B": true, "123": false, " ": false, "Tts": false, "tts": false,
		"ǅ": false, "AǅB": false, "Ⅷ": true, "AªB": false, "ÀÉ": true, "ΣΑ": true, "A-B_C": true, "Image gen": false,
	}
	for in, want := range cases {
		if got := IsUpper(in); got != want {
			t.Errorf("IsUpper(%q) = %v, want %v", in, got, want)
		}
	}
}
