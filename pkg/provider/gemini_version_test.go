package provider

import "testing"

func TestGeminiVersionAnchorsAtTheModelIDStart(t *testing.T) {
	cases := []struct {
		model        string
		major, minor int
	}{
		{"gemini-4-argon", 4, 0},
		{"google/gemini-3.8-flash", 3, 8},
		{"gemini/Gemini-2.5-pro", 2, 5},
		{"gemini-flash-latest", 0, 0},
		{"gemini-exp-1206", 0, 0},
		{"x-gemini-3", 0, 0},
		{"openai/gpt-gemini-3", 0, 0},
		{"", 0, 0},
	}
	for _, c := range cases {
		major, minor := geminiVersion(c.model)
		if major != c.major || minor != c.minor {
			t.Errorf("%q: got (%d, %d) want (%d, %d)", c.model, major, minor, c.major, c.minor)
		}
	}
}
