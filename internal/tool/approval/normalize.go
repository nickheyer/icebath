package approval

import (
	"cmp"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/nickheyer/icebath/internal/shell"
	"github.com/nickheyer/icebath/internal/tool"
)

var (
	continuationRe       = regexp.MustCompile(`\\\r?\n`)
	escapeRe             = regexp.MustCompile(`\\([^\n])`)
	emptyQuotesRe        = regexp.MustCompile(`''|""`)
	paramReplacementRe   = regexp.MustCompile(`\$\{[^}/` + pySpaceClass + `]+/[^}/]*/([^}]*)\}`)
	paramDefaultRe       = regexp.MustCompile(`\$\{[^}:}` + pySpaceClass + `]+:-([^}]*)\}`)
	echoFlagsRe          = regexp.MustCompile(`\A-[nEe]+\z`)
	homeFoldTail         = `((?:[/\\][^/\\` + pySpaceClass + `'"` + "`" + `;|&<>()]*)+)`
	separatorRunSplitter = regexp.MustCompile(`[` + pySpaceClass + `]+`)
)

type homeFold struct {
	re          *regexp.Regexp
	replacement string
}

func foldsFor(paths []string, replacement string) []homeFold {
	var kept []string
	for _, p := range paths {
		if p != "" {
			kept = append(kept, p)
		}
	}
	slices.SortStableFunc(kept, func(a, b string) int { return cmp.Compare(runeCount(b), runeCount(a)) })
	seen := map[string]bool{}
	var out []homeFold
	for _, p := range kept {
		if seen[p] {
			continue
		}
		seen[p] = true
		var comps []string
		for _, c := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
			comps = append(comps, regexp.QuoteMeta(c))
		}
		if len(comps) < 2 {
			continue
		}
		out = append(out, homeFold{
			re:          regexp.MustCompile(`[/\\]*` + strings.Join(comps, `[/\\]+`) + homeFoldTail),
			replacement: replacement,
		})
	}
	return out
}

func applyFolds(command string, folds []homeFold) string {
	for _, f := range folds {
		locs := f.re.FindAllStringSubmatchIndex(command, -1)
		if len(locs) == 0 {
			continue
		}
		var b strings.Builder
		prev := 0
		for _, loc := range locs {
			b.WriteString(command[prev:loc[0]])
			b.WriteString(f.replacement)
			b.WriteString(strings.ReplaceAll(command[loc[2]:loc[3]], `\`, "/"))
			prev = loc[1]
		}
		b.WriteString(command[prev:])
		command = b.String()
	}
	return command
}

func (d *Detector) normalize(command string) string {
	command = norm.NFKC.String(strings.ReplaceAll(tool.StripANSI(command), "\x00", ""))
	command = continuationRe.ReplaceAllString(command, "")
	command = applyFolds(command, d.homeFolds)
	command = applyFolds(command, d.userFolds)
	command = escapeRe.ReplaceAllString(command, "$1")
	command = emptyQuotesRe.ReplaceAllString(command, "")
	return collapseIFS(command)
}

func collapseIFS(s string) string {
	if !strings.Contains(s, "$IFS") && !strings.Contains(s, "${IFS") {
		return s
	}
	boundary := func(p int) bool { return !wordAt(s, p) }
	var b strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "${IFS") && boundary(i+5) {
			if k := strings.IndexByte(s[i+5:], '}'); k >= 0 {
				b.WriteByte(' ')
				i += 5 + k + 1
				continue
			}
		}
		if strings.HasPrefix(s[i:], "$IFS") && boundary(i+4) {
			b.WriteByte(' ')
			i += 4
			continue
		}
		w := runeWidth(s, i)
		b.WriteString(s[i : i+w])
		i += w
	}
	return b.String()
}

func lowerPreservingFlags(command string) string {
	var b strings.Builder
	b.Grow(len(command))
	last := 0
	for _, loc := range separatorRunSplitter.FindAllStringIndex(command, -1) {
		writeToken(&b, command[last:loc[0]])
		b.WriteString(command[loc[0]:loc[1]])
		last = loc[1]
	}
	writeToken(&b, command[last:])
	return b.String()
}

func writeToken(b *strings.Builder, t string) {
	if strings.HasPrefix(t, "-") {
		b.WriteString(t)
	} else {
		b.WriteString(pyLower(t))
	}
}

func literalSubstitutionOutput(script string) (string, bool) {
	tokens, err := shell.Split(script)
	if err != nil || len(tokens) == 0 {
		return "", false
	}
	command, args := pyLower(tokens[0]), tokens[1:]
	switch command {
	case "echo":
		for len(args) > 0 && echoFlagsRe.MatchString(args[0]) {
			args = args[1:]
		}
	case "printf":
	default:
		return "", false
	}
	if len(args) == 1 && simpleLiteral(args[0]) {
		return args[0], true
	}
	if command == "printf" && len(args) == 2 && args[0] == "%s" && simpleLiteral(args[1]) {
		return args[1], true
	}
	return "", false
}

func simpleLiteral(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(asciiWord(c) || strings.IndexByte("./:@%+=,-", c) >= 0) {
			return false
		}
	}
	return true
}

func replaceSimpleSubstitutions(word string) string {
	var b strings.Builder
	for i := 0; i < len(word); {
		opener := 0
		switch {
		case strings.HasPrefix(word[i:], "$("):
			opener = 2
		case word[i] == '`':
			opener = 1
		}
		end := -1
		switch opener {
		case 2:
			end = dollarParenEnd(word, i)
		case 1:
			end = backtickEnd(word, i)
		}
		if end >= 0 {
			if out, ok := literalSubstitutionOutput(pySlice(word, i+opener, end-1)); ok {
				b.WriteString(out)
				i = end
				continue
			}
		}
		w := runeWidth(word, i)
		b.WriteString(word[i : i+w])
		i += w
	}
	return b.String()
}

func replaceSimpleExpansions(word string) string {
	word = replaceSimpleSubstitutions(word)
	word = paramReplacementRe.ReplaceAllString(word, "$1")
	return paramDefaultRe.ReplaceAllString(word, "$1")
}
