package agent

import "testing"

func TestAffectionFiresVibe(t *testing.T) {
	for _, text := range []string{"Good Bot!", "ily", "thank you", "ty", "you're the best <3", "❤️"} {
		if got := detectReaction(text); got != Vibe {
			t.Errorf("detectReaction(%q) = %q, want vibe", text, got)
		}
	}
}

func TestNeutralOrNegativeDoesNotFire(t *testing.T) {
	for _, text := range []any{"", nil, "run the tests", "this is great", "awesome work on the refactor", "it's broken </3", "the ferry departs at 3", "commit the changes"} {
		if got := detectReaction(text); got != "" {
			t.Errorf("detectReaction(%v) = %q, want none", text, got)
		}
	}
}

func TestCaseInsensitiveInvariant(t *testing.T) {
	want := detectReaction("ily")
	for _, text := range []string{"ILY", "iLy", "ily"} {
		if got := detectReaction(text); got != want {
			t.Errorf("detectReaction(%q) = %q, want %q", text, got, want)
		}
	}
}
