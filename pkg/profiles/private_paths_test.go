package profiles

import "testing"

func TestCasefoldMatchesPythonForTheStoreNameFolds(t *testing.T) {
	for in, want := range map[string]string{"DEFAULT": "default", "Straße": "strasse", "ﬁle": "file", "Work Bot": "work bot", "ΣΊΣΥΦΟΣ": "σίσυφοσ"} {
		if got := Casefold(in); got != want {
			t.Errorf("Casefold(%q) = %q, want %q", in, got, want)
		}
	}
}
