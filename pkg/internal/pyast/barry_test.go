package pyast

import "testing"

// Checks the lexer turns <> on exactly where CPython's pass one ran the barry_as_FLUFL import
func TestBarryLexingFollowsPassOne(t *testing.T) {
	cases := []struct {
		src    string
		relex  bool
		splits bool
	}{
		{"x = lambda: from __future__ import barry_as_FLUFL\n1 <> 2\n", true, true},
		{"from __future__ import ｂarry_as_FLUFL\n1 <> 2\n", false, false},
		{"from __future__ import barry_as_FLUFL <> 1\n", false, true},
		{"from __future__ import (barry_as_FLUFL) <> 1\n", false, false},
		{"1 < > 2\n", false, true},
	}
	for _, c := range cases {
		p, _, _ := parseLexed([]byte(c.src), Options{Optimize: -1}, latestMinor)
		if relexed := p.lx.barryAt != -1; relexed != c.relex {
			t.Errorf("%q relexed %v want %v", c.src, relexed, c.relex)
		}
		ne, split := false, false
		for i, tk := range p.lx.toks {
			ne = ne || tk.val == "<>"
			split = split || tk.val == "<" && i+1 < len(p.lx.toks) && p.lx.toks[i+1].val == ">"
		}
		if split != c.splits || ne == c.splits {
			t.Errorf("%q split %v one token %v want split %v", c.src, split, ne, c.splits)
		}
	}
}

// Str sources holding surrogates fail the strict utf-8 encode of compile before the null byte check
func TestParseSurrogateSourceMatchesCPython(t *testing.T) {
	rows := []struct{ src, want string }{
		{"x = \"\xed\xa0\x80\"", "'utf-8' codec can't encode character '\\ud800' in position 5: surrogates not allowed"},
		{"\xed\xb3\xbf", "'utf-8' codec can't encode character '\\udcff' in position 0: surrogates not allowed"},
		{"a\nb = 1 +\xed\xa0\xbd", "'utf-8' codec can't encode character '\\ud83d' in position 9: surrogates not allowed"},
		{"# \xed\xa0\x80\n(1 2)", "'utf-8' codec can't encode character '\\ud800' in position 2: surrogates not allowed"},
		{"\xc3\xa9\xed\xa0\x80\xed\xa0\x81\xed\xa0\x82x", "'utf-8' codec can't encode characters in position 1-3: surrogates not allowed"},
		{"\xf0\x9f\x98\x80\xed\xa0\x80", "'utf-8' codec can't encode character '\\ud800' in position 1: surrogates not allowed"},
		{"\xed\xb2\x80\x00", "'utf-8' codec can't encode character '\\udc80' in position 0: surrogates not allowed"},
	}
	for _, r := range rows {
		_, _, err := ParseWith(r.src, Options{Optimize: -1})
		if err == nil || err.Error() != r.want || errorClass(err) != "*pystr.UnicodeEncodeError" {
			t.Errorf("%q got %T %v want UnicodeEncodeError %s", r.src, err, err, r.want)
		}
	}
}

// A feature_version tuple keeps its minor for major 3 and raises the Lib/ast.py ValueError for any other major
func TestParseFeatureVersionTupleMatchesCPython(t *testing.T) {
	src := "match x:\n case 1: pass"
	rows := []struct {
		major, minor int
		want         string
	}{
		{3, 10, ""},
		{2, 7, "Unsupported major version: 2"},
		{4, 0, "Unsupported major version: 4"},
		{3, 2, "Pattern matching is only supported in Python 3.10 and greater"},
	}
	for _, r := range rows {
		_, _, err := ParseWith(src, Options{FeatureMajor: &r.major, FeatureVersion: &r.minor, Optimize: -1})
		got := ""
		if err != nil {
			got = err.Error()
			if se, ok := err.(*SyntaxError); ok {
				got = se.Msg
			}
		}
		if got != r.want {
			t.Errorf("(%d, %d) got %q want %q", r.major, r.minor, got, r.want)
		}
	}
	major := 2
	if _, _, err := ParseWith("x", Options{FeatureMajor: &major, Optimize: 7, Mode: "bogus"}); err == nil || err.Error() != "Unsupported major version: 2" {
		t.Errorf("tuple check runs before compile: got %v", err)
	}
}
