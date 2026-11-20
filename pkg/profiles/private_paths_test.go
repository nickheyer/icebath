package profiles

import "testing"

func TestCasefoldMatchesPythonForTheStoreNameFolds(t *testing.T) {
	for in, want := range map[string]string{
		"DEFAULT": "default", "Straße": "strasse", "ﬁle": "file", "Work Bot": "work bot", "ΣΊΣΥΦΟΣ": "σίσυφοσ",
		"İ": "i̇", "KEY": "key", "ᏸ": "Ᏸ", "Ꭰ": "Ꭰ", "ﬓ": "մն", "ᾀ": "ἀι", "ẞ": "ss", "ſ": "s", "µ": "μ",
	} {
		if got := Casefold(in); got != want {
			t.Errorf("Casefold(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStoreCopyMatchesPythonUnicodeDigitsAndFolds(t *testing.T) {
	cases := []struct {
		parts []string
		want  bool
	}{
		{[]string{"config.yaml.bak.١٢"}, true},
		{[]string{"CONFIG.YAML.BAK-20260101T000000Z.2"}, true},
		{[]string{"config.yaml.bak-my-note"}, false},
		{[]string{"auth.json.\n"}, false},
		{[]string{"Kuth.json.x"}, false},
		{[]string{"Platforms", "Pairing"}, true},
		{[]string{"platforms"}, false},
		{[]string{"ſlack_tokens.json"}, true},
	}
	for _, c := range cases {
		if got := ProfilePathIsPrivate(c.parts); got != c.want {
			t.Errorf("ProfilePathIsPrivate(%q) = %v, want %v", c.parts, got, c.want)
		}
	}
	if !ProfilePathContainsPrivateStore([]string{"PLATFORMS"}) || ProfilePathContainsPrivateStore([]string{"platforms", "pairing"}) {
		t.Error("ancestor check differs from the Python fold")
	}
}
