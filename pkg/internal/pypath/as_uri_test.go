package pypath

import "testing"

func TestAsURIQuotesLikePathlib(t *testing.T) {
	cases := map[string]string{
		"/tmp/a b/kanban.db": "file:///tmp/a%20b/kanban.db",
		"/x/%?#\u00e9":       "file:///x/%25%3F%23%C3%A9",
		"/a//b/":             "file:///a/b",
	}
	for in, want := range cases {
		got, err := posixAsURI(in)
		if err != nil || got != want {
			t.Fatalf("%q: %q %v", in, got, err)
		}
	}
	if _, err := posixAsURI("rel/x"); err == nil {
		t.Fatal("relative path accepted")
	}
	for in, want := range map[string]string{`C:\a b\k.db`: "file:///C:/a%20b/k.db", `\\srv\share\k.db`: "file://srv/share/k.db"} {
		got, err := ntAsURI(in)
		if err != nil || got != want {
			t.Fatalf("%q: %q %v", in, got, err)
		}
	}
	if _, err := ntAsURI(`a\b`); err == nil {
		t.Fatal("relative nt path accepted")
	}
}
