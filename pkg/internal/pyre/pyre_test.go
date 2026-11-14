package pyre

import "testing"

func TestCompileErrorsMatchCPython(t *testing.T) {
	cases := map[string]string{
		`nvapi-[unclosed`:  "unterminated character set at position 6",
		`*abc`:             "nothing to repeat at position 0",
		`a**`:              "multiple repeat at position 2",
		`(abc`:             "missing ), unterminated subpattern at position 0",
		`abc)`:             "unbalanced parenthesis at position 3",
		`\q`:               `bad escape \q at position 0`,
		`a\`:               "bad escape (end of pattern) at position 1",
		`(?<=a+)b`:         "look-behind requires fixed-width pattern",
		`(?<=a|bc)d`:       "look-behind requires fixed-width pattern",
		`a(?i)b`:           "global flags not at the start of the expression at position 1",
		`(a)\2`:            "invalid group reference 2 at position 4",
		`(a\1)`:            "cannot refer to an open group at position 2",
		`a{3,2}`:           "min repeat greater than max repeat at position 2",
		`[z-a]`:            "bad character range z-a at position 1",
		`(?P<1a>x)`:        "bad character in group name '1a' at position 4",
		`(?P<a>x)(?P<a>y)`: "redefinition of group name 'a' as group 2; was group 1 at position 12",
		`(?P=b)`:           "unknown group name 'b' at position 4",
		`(?Q)`:             "unknown extension ?Q at position 1",
		`(?<x>a)`:          "unknown extension ?<x at position 1",
		`(?(2)a|b)(c)`:     "invalid group reference 2 at position 3",
		`(?(1)a|b|c)(x)`:   "conditional backref with more than two branches at position 8",
		`(?L)a`:            "bad inline flag: cannot use 'L' flag with a str pattern at position 3",
		`\x4`:              `incomplete escape \x4 at position 0`,
		`\777`:             `octal escape value \777 outside of range 0-0o377 at position 0`,
		"ab\n(":            "missing ), unterminated subpattern at position 3 (line 2, column 1)",
	}
	for pattern, want := range cases {
		_, err := Compile(pattern)
		if err == nil || err.Error() != want {
			t.Errorf("Compile(%q) error %v, want %q", pattern, err, want)
		}
	}
}

func TestTranslatedPatternsMatchLikePython(t *testing.T) {
	cases := []struct {
		pattern, text, want string
	}{
		{`(?P<word>ab)(?P=word)`, "xabab", "abab"},
		{`(?P<n>x)(y)\2`, "xyy", "xyy"},
		{`a{,2}b`, "aaab", "aab"},
		{`ab\Z`, "ab\n", ""},
		{`ab$`, "ab\n", "ab"},
		{`[a-z-[x]]+`, "q-[x]]", "x]]"},
		{`(?a)\w+`, "éabc", "abc"},
		{`\w+`, "éabc", "éabc"},
		{`(?a:[^\d])+`, "éx1", "éx"},
		{`[\D5]+`, "a5b1", "a5b"},
		{`a++a`, "aaa", ""},
		{`x(?#c\)omment)y`, "xy", "xy"},
		{`\N{LATIN SMALL LETTER E WITH ACUTE}t`, "ét", "ét"},
		{`(?x) a b # comment`, "ab", "ab"},
		{`(?i)AB`, "xab", "ab"},
		{`(?i)(?m)^ab`, "x\nAB", "AB"},
		{`(a)?(?(1)b|c)`, "c", "c"},
		{`x{}`, "x{}", "x{}"},
		{`x{a}`, "x{a}", "x{a}"},
	}
	for _, c := range cases {
		re, err := Compile(c.pattern)
		if err != nil {
			t.Errorf("Compile(%q): %v", c.pattern, err)
			continue
		}
		m, err := re.FindStringMatch(c.text)
		got := ""
		if err == nil && m != nil {
			got = m.String()
		}
		if got != c.want {
			t.Errorf("%q on %q matched %q, want %q", c.pattern, c.text, got, c.want)
		}
	}
}

func TestGroupIndexMatchesPythonGroupindex(t *testing.T) {
	pattern := `(a)(?P<owner>[^/]+)(?:x)(?P<repo>b)(c)(?P<skill>d)?`
	idx, err := GroupIndex(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 3 || idx["owner"] != 2 || idx["repo"] != 3 || idx["skill"] != 5 {
		t.Fatalf("groupindex %v", idx)
	}
	re, err := Compile(pattern)
	if err != nil {
		t.Fatal(err)
	}
	m, err := re.FindStringMatch("aOxbc")
	if err != nil || m == nil {
		t.Fatal(m, err)
	}
	if g := m.GroupByNumber(idx["owner"]).String(); g != "O" {
		t.Fatalf("owner %q", g)
	}
	if g := m.GroupByNumber(idx["skill"]); g == nil || len(g.Captures) != 0 {
		t.Fatalf("skill took part %v", g)
	}
	if _, err := GroupIndex(`(?P<a>x)(?P<a>y)`); err == nil || err.Error() != "redefinition of group name 'a' as group 2; was group 1 at position 12" {
		t.Fatalf("duplicate name: %v", err)
	}
}
