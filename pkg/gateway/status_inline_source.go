package gateway

import (
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/state"
)

// Index of the -c token when tokens run inline interpreter source, walking operand taking options value aware
func InlineSourceFlagIndex(tokens []string) (int, bool) {
	index := 1
	for index < len(tokens) {
		token := tokens[index]
		if token == "--" {
			return 0, false
		}
		if state.PythonLongOptionTakesOperand(token) {
			index += 2
			continue
		}
		if strings.HasPrefix(token, "--") {
			index++
			continue
		}
		if !strings.HasPrefix(token, "-") || token == "-" {
			return 0, false
		}
		cluster := []rune(token[1:])
		advanced := false
		for position, letter := range cluster {
			if letter == 'c' {
				return index, true
			}
			if state.PythonShortOptionTakesOperand(string(letter)) {
				if position+1 < len(cluster) {
					index++
				} else {
					index += 2
				}
				advanced = true
				break
			}
		}
		if !advanced {
			index++
		}
	}
	return 0, false
}

// Whether tokens are an interpreter running inline source
func CommandLineRunsInlineSource(tokens []string) bool {
	_, ok := InlineSourceFlagIndex(tokens)
	return ok
}

const (
	inlineQ         = `['"]?`
	inlineMain      = inlineQ + `__main__` + inlineQ
	inlineRunModule = `runpy\.run_module\(\s*` + inlineQ + `(?<target>[\w.]+)` + inlineQ + `\s*,\s*run_name\s*=\s*` + inlineMain + `\s*,\s*alter_sys\s*=\s*True\s*\)`
)

func fullmatchRe(pattern string, opts regexp2.RegexOptions) *regexp2.Regexp {
	return regexp2.MustCompile(`\A(?:`+pattern+`)\z`, opts)
}

var (
	inlineBootstraps = []struct {
		kind string
		re   *regexp2.Regexp
	}{
		{"module", fullmatchRe(`import os, sys, runpy;.*\b`+inlineRunModule, regexp2.Singleline)},
		{"module", fullmatchRe(`import sys, runpy; sys\.path\.insert\(.*\b`+inlineRunModule, regexp2.Singleline)},
		{"path", fullmatchRe(`import sys, runpy; sys\.path\.insert\(.*\brunpy\.run_path\(\s*`+inlineQ+`(?<target>[^'"]+?)`+inlineQ+`\s*,\s*run_name\s*=\s*`+inlineMain+`\s*\)`, regexp2.Singleline)},
		{"exec", fullmatchRe(`import sys, runpy; sys\.path\.insert\(.*?;\s*exec\((?<target>.+)\)`, regexp2.Singleline)},
		{"entry", fullmatchRe(`import os, re, sys\s.*\bfrom\s+(?<target>[\w.]+)\s+import\s+(?<func>\w+)\b.*\bsys\.exit\(\s*\k<func>\(\)\s*\)`, regexp2.Singleline)},
		{"module", fullmatchRe(`import os, site, sys;\s*sys\.argv\[0\]\s*=\s*`+inlineQ+`[\w.-]+`+inlineQ+`;\s*`+
			`site\.addsitedir\(os\.environ\[`+inlineQ+`ICEBATH_SITE`+inlineQ+`\]\);`+
			`\s*from\s+(?<target>icebath_cli\.main)\s+import\s+main\s*;\s*sys\.exit\(\s*main\(\)\s*\)`, regexp2.None)},
		{"base64", fullmatchRe(`import base64; exec\(base64\.b64decode\(`+inlineQ+`(?<target>[A-Za-z0-9+/=]+)`+inlineQ+`\)\)`, regexp2.None)},
	}
	inlineAssignedArgv  = regexp2.MustCompile(`\bsys\.argv\s*=\s*\[(.*?)\]\s*;`, regexp2.None)
	inlineExecLauncher  = regexp2.MustCompile(`import os, re, sys\W.*?from\s+(?<target>[\w.]+)\s+import\s+(?<func>\w+)\b.*sys\.exit\(\s*\k<func>\(\)\s*\)`, regexp2.Singleline)
	inlineExecBase64    = regexp2.MustCompile(`import base64; exec\(base64\.b64decode\(\W*(?<b64>[A-Za-z0-9+/=]+)`, regexp2.None)
	inlineAssignedSplit = func(group string) []string {
		var items []string
		for _, item := range strings.Split(group, ",") {
			items = append(items, strings.Trim(strings.TrimSpace(item), `'"`))
		}
		return items[1:]
	}
)

func reFind(re *regexp2.Regexp, s string) *regexp2.Match {
	m, err := re.FindStringMatch(s)
	if err != nil {
		return nil
	}
	return m
}

// Decodes base64 source strictly as UTF-8 text, false on a binascii or Unicode decode error
func inlineB64Text(data string) (string, bool) {
	raw, err := pybase64.B64DecodeValidate(data)
	if err != nil || !utf8.Valid(raw) {
		return "", false
	}
	return string(raw), true
}

// Argument vector an inline bootstrap source runs in process as -m module or a script path, false otherwise
func inlineBootstrapEntry(source string, argv []string) ([]string, bool) {
	source = strings.TrimSpace(source)
	kind := ""
	var match *regexp2.Match
	for _, b := range inlineBootstraps {
		if m := reFind(b.re, source); m != nil {
			kind, match = b.kind, m
			break
		}
	}
	if match == nil {
		return nil, false
	}
	target := match.GroupByName("target").String()
	if kind == "base64" {
		text, ok := inlineB64Text(target)
		if !ok {
			return nil, false
		}
		return inlineBootstrapEntry(text, argv)
	}
	if kind == "exec" {
		if assigned := reFind(inlineAssignedArgv, source); assigned != nil {
			argv = inlineAssignedSplit(assigned.GroupByNumber(1).String())
		}
		if wrapped := reFind(inlineExecBase64, target); wrapped != nil {
			text, ok := inlineB64Text(wrapped.GroupByName("b64").String())
			if !ok {
				return nil, false
			}
			return inlineBootstrapEntry(text, argv)
		}
		launcher := reFind(inlineExecLauncher, target)
		if launcher == nil {
			return nil, false
		}
		kind, target = "entry", launcher.GroupByName("target").String()
	}
	if kind == "entry" {
		if len(argv) > 1 && argv[0] == "--run-module" {
			return append([]string{"-m", argv[1]}, argv[2:]...), true
		}
		return append([]string{"-m", target}, argv...), true
	}
	if assigned := reFind(inlineAssignedArgv, source); assigned != nil {
		argv = inlineAssignedSplit(assigned.GroupByNumber(1).String())
	}
	if kind == "path" {
		return append([]string{target}, argv...), true
	}
	return append([]string{"-m", target}, argv...), true
}

// Tokens rewritten as the equivalent interpreter -m module argv when the -c source is an icebath bootstrap
func InlineBootstrapArgv(tokens []string) ([]string, bool) {
	index, ok := InlineSourceFlagIndex(tokens)
	if !ok {
		return nil, false
	}
	for end := index + 1; end < len(tokens); end++ {
		if strings.HasSuffix(strings.TrimRight(tokens[end], " \t\n\r\v\f"), ")") {
			if entry, ok := inlineBootstrapEntry(strings.Join(tokens[index+1:end+1], " "), append([]string{}, tokens[end+1:]...)); ok {
				return append([]string{tokens[0]}, entry...), true
			}
		}
	}
	return nil, false
}
