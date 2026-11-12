package provider

import "testing"

func TestFriendlyWaitIsARoundedSpokenDuration(t *testing.T) {
	for seconds, want := range map[float64]string{
		-5: "a few seconds", 0: "a few seconds", 15: "a few seconds", 15.5: "about a minute", 42: "about a minute", 89.9: "about a minute",
		90: "about 2 minutes", 150: "about 2 minutes", 300: "about 5 minutes", 600: "about 10 minutes", 3599: "about 60 minutes",
		3600: "about an hour", 5400: "about 2 hours", 9000: "about 2 hours", 12600: "about 4 hours", 86400: "about 24 hours",
	} {
		if got := FriendlyWait(seconds); got != want {
			t.Errorf("FriendlyWait(%v) = %q, want %q", seconds, got, want)
		}
	}
}
