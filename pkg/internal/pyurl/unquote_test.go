package pyurl

import "testing"

func TestUnquoteMatchesUrllib(t *testing.T) {
	for in, want := range map[string]string{
		"references/my%20guide.md": "references/my guide.md", "references/foo%23bar.md": "references/foo#bar.md",
		"%2A.md": "*.md", "100%": "100%", "%zz%4": "%zz%4", "%E2%82%AC": "€", "%E2%82": "�",
		"café%20x": "café x", "%ff%41": "�A", "plain": "plain",
	} {
		if got := Unquote(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

func TestUnquoteLatin1MatchesUrllib(t *testing.T) {
	for in, want := range map[string]string{
		"r%C3%A9sum%C3%A9.pdf": "rÃ©sumÃ©.pdf", "100%": "100%", "%zz%4": "%zz%4", "%ff%41": "ÿA", "café%20x": "café x", "plain": "plain",
	} {
		if got := UnquoteLatin1(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}
