package lsp

import (
	"regexp"
	"strings"
	"sync"
)

var (
	fnmatchMu    sync.Mutex
	fnmatchCache = map[string]*regexp.Regexp{}
)

// Python fnmatch.fnmatchcase
func fnmatchCase(name, pat string) bool {
	fnmatchMu.Lock()
	re, ok := fnmatchCache[pat]
	if !ok {
		re = regexp.MustCompile(fnmatchTranslate(pat))
		if len(fnmatchCache) > 32768 {
			fnmatchCache = map[string]*regexp.Regexp{}
		}
		fnmatchCache[pat] = re
	}
	fnmatchMu.Unlock()
	return re.MatchString(name)
}

// Python fnmatch.translate as an RE2 pattern anchored at both ends
func fnmatchTranslate(pat string) string {
	const star = "\x00STAR"
	p := []rune(pat)
	n := len(p)
	var res []string
	for i := 0; i < n; {
		c := p[i]
		i++
		switch c {
		case '*':
			if len(res) == 0 || res[len(res)-1] != star {
				res = append(res, star)
			}
		case '?':
			res = append(res, ".")
		case '[':
			j := i
			if j < n && p[j] == '!' {
				j++
			}
			if j < n && p[j] == ']' {
				j++
			}
			for j < n && p[j] != ']' {
				j++
			}
			if j >= n {
				res = append(res, `\[`)
				continue
			}
			stuff := string(p[i:j])
			if !strings.Contains(stuff, "-") {
				stuff = strings.ReplaceAll(stuff, `\`, `\\`)
			} else {
				var chunks []string
				k := i + 1
				if p[i] == '!' {
					k = i + 2
				}
				for {
					idx := -1
					for q := k; q < j; q++ {
						if p[q] == '-' {
							idx = q
							break
						}
					}
					if idx < 0 {
						break
					}
					chunks = append(chunks, string(p[i:idx]))
					i = idx + 1
					k = idx + 3
				}
				if chunk := string(p[i:j]); chunk != "" {
					chunks = append(chunks, chunk)
				} else {
					chunks[len(chunks)-1] += "-"
				}
				for k := len(chunks) - 1; k > 0; k-- {
					prev, cur := []rune(chunks[k-1]), []rune(chunks[k])
					if prev[len(prev)-1] > cur[0] {
						chunks[k-1] = string(prev[:len(prev)-1]) + string(cur[1:])
						chunks = append(chunks[:k], chunks[k+1:]...)
					}
				}
				for q, s := range chunks {
					chunks[q] = strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "-", `\-`)
				}
				stuff = strings.Join(chunks, "-")
			}
			stuff = regexp.MustCompile(`([&~|])`).ReplaceAllString(stuff, `\$1`)
			i = j + 1
			switch {
			case stuff == "":
				res = append(res, `[^\x00-\x{10FFFF}]`)
			case stuff == "!":
				res = append(res, ".")
			default:
				if stuff[0] == '!' {
					stuff = "^" + stuff[1:]
				} else if stuff[0] == '^' || stuff[0] == '[' {
					stuff = `\` + stuff
				}
				res = append(res, "["+stuff+"]")
			}
		default:
			res = append(res, regexp.QuoteMeta(string(c)))
		}
	}
	var b strings.Builder
	for _, part := range res {
		if part == star {
			b.WriteString(".*")
		} else {
			b.WriteString(part)
		}
	}
	return `(?s)^(?:` + b.String() + `)$`
}
