package glob

import "testing"

func TestMatch(t *testing.T) {
	yes := map[string][]string{
		"*.go":          {"a.go", "main.go"},
		"src/*.go":      {"src/a.go"},
		"src/**":        {"src", "src/a", "src/a/b/c.go"},
		"**/*.go":       {"a.go", "src/a.go", "src/x/y/z.go"},
		"src/**/test_*": {"src/test_a", "src/x/test_b", "src/x/y/test_c"},
		"a/?/c":         {"a/b/c"},
		"**":            {"", "a", "a/b"},
		"/etc/**":       {"/etc", "/etc/passwd", "/etc/ssh/sshd_config"},
	}
	for pattern, names := range yes {
		for _, n := range names {
			if !Match(pattern, n) {
				t.Errorf("%q should match %q", pattern, n)
			}
		}
	}
	no := map[string][]string{
		"*.go":     {"src/a.go", "a.txt"},
		"src/*.go": {"src/x/a.go", "a.go"},
		"src/**":   {"srcs", "other/src"},
		"a/?/c":    {"a/bb/c", "a/c"},
		"[":        {"["},
		"/etc/**":  {"/etcetera", "etc/passwd"},
	}
	for pattern, names := range no {
		for _, n := range names {
			if Match(pattern, n) {
				t.Errorf("%q should not match %q", pattern, n)
			}
		}
	}
}

func TestWild(t *testing.T) {
	yes := map[string][]string{
		"git *":    {"git status", "git log src/x", "git "},
		"git*":     {"git", "gitk", "git status"},
		"*":        {"", "anything at all"},
		"npm run*": {"npm run", "npm run test"},
		"a?c":      {"abc", "a/c"},
		"*.log":    {"x.log", "a/b.log"},
		"a**b":     {"ab", "axxb"},
		"select *": {"select * from t", "select *"},
		"x*y":      {"x*y", "x**y", "xy"},
	}
	for pattern, ss := range yes {
		for _, s := range ss {
			if !Wild(pattern, s) {
				t.Errorf("%q should match %q", pattern, s)
			}
		}
	}
	no := map[string][]string{
		"git *": {"git", "gits", "sgit status"},
		"git":   {"git status", "gi"},
		"a?c":   {"ac", "abbc"},
		"*.log": {"x.logs"},
	}
	for pattern, ss := range no {
		for _, s := range ss {
			if Wild(pattern, s) {
				t.Errorf("%q should not match %q", pattern, s)
			}
		}
	}
}
