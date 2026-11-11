package pyemail

import "testing"

func TestGetParamsCollapseMatchesEmailMessage(t *testing.T) {
	for header, want := range map[string]string{
		`attachment; filename="a b.pdf"`:                         "a b.pdf",
		`attachment; filename*=UTF-8''r%C3%A9sum%C3%A9.pdf`:      "résumé.pdf",
		`attachment; filename*0="par"; filename*1="t.txt"`:       "part.txt",
		`attachment; filename*=bogus''x%41.txt`:                  "xA.txt",
		`inline; FileName = "q\"uote.txt"`:                       `q"uote.txt`,
		`attachment; filename*=''plain.txt`:                      "plain.txt",
		`attachment; filename*0*=UTF-8''%E2%82; filename*1*=%AC`: "€",
	} {
		params, err := GetParams(header)
		if err != nil {
			t.Fatalf("%s: %v", header, err)
		}
		got := ""
		for _, p := range params[1:] {
			if p.Name == "filename" {
				got = CollapseRFC2231Value(p)
			}
		}
		if got != want {
			t.Errorf("%s -> %q want %q", header, got, want)
		}
	}
	if _, err := GetParams(`attachment; filename*="a"; filename*1="b"`); err != ErrUnorderable {
		t.Fatalf("mixed numbering err %v", err)
	}
}
