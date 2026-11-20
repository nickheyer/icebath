package pydifflib

import (
	"reflect"
	"strings"
	"testing"
)

func TestGetCloseMatchesFollowsDifflib(t *testing.T) {
	cases := []struct {
		word   string
		pool   []string
		n      int
		cutoff float64
		want   []string
	}{
		{"appel", []string{"ape", "apple", "peach", "puppy"}, 3, 0.6, []string{"apple", "ape"}},
		{"wheel", []string{"while", "for", "with", "class"}, 3, 0.6, []string{"while"}},
		{"pre_tool_cal", []string{"post_tool_call", "pre_tool_call", "pre_llm_call"}, 1, 0.6, []string{"pre_tool_call"}},
		{"zzz", []string{"pre_tool_call"}, 1, 0.6, []string{}},
		{"ab", []string{"ba", "ab", "ab"}, 2, 0.0, []string{"ab", "ab"}},
	}
	for _, c := range cases {
		if got := GetCloseMatches(c.word, c.pool, c.n, c.cutoff); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("GetCloseMatches(%q, %v) = %v, want %v", c.word, c.pool, got, c.want)
		}
	}
}

func TestRatioMatchesSequenceMatcher(t *testing.T) {
	r := NewSequenceMatcher([]rune("abcd"), []rune("bcde"), true).Ratio()
	if r != 0.75 {
		t.Fatalf("ratio = %v, want 0.75", r)
	}
	if r := NewSequenceMatcher([]rune(""), []rune(""), true).Ratio(); r != 1.0 {
		t.Fatalf("empty ratio = %v", r)
	}
	a, b := []rune("x"+strings.Repeat("a", 250)), []rune(strings.Repeat("a", 250)+"x")
	if r := NewSequenceMatcher(a, b, true).Ratio(); r != 2.0/502 {
		t.Fatalf("autojunk ratio = %v, want the popular element purged", r)
	}
	if r := NewSequenceMatcher(a, b, false).Ratio(); r != 500.0/502 {
		t.Fatalf("plain ratio = %v", r)
	}
}

func TestGetCloseMatchesRefusesBadArguments(t *testing.T) {
	for _, f := range []func(){func() { GetCloseMatches("a", nil, 0, 0.6) }, func() { GetCloseMatches("a", nil, 1, 1.5) }} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a ValueError panic")
				}
			}()
			f()
		}()
	}
}
