package fnmatch

import (
	"container/list"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const cacheSize = 32768

type cacheEntry struct {
	pat string
	re  *regexp.Regexp
}

var (
	cacheMu    sync.Mutex
	cacheOrder = list.New()
	cacheIndex = map[string]*list.Element{}
	setOps     = regexp.MustCompile(`([&~|])`)
)

// Matches name against a shell pattern after os.path.normcase of both on this platform
func Fnmatch(name, pat string) bool {
	return FnmatchCase(normcase(name), normcase(pat))
}

// Matches name against a shell pattern exactly with no case normalization
func FnmatchCase(name, pat string) bool {
	return Compile(pat).MatchString(name)
}

// Names matching the pattern after normcase, keeping input order
func Filter(names []string, pat string) []string {
	return filter(names, pat, true)
}

// Names not matching the pattern after normcase, keeping input order
func FilterFalse(names []string, pat string) []string {
	return filter(names, pat, false)
}

// Shared body of filter and filterfalse with normcase applied to names only off posix
func filter(names []string, pat string, want bool) []string {
	re := Compile(normcase(pat))
	out := []string{}
	for _, name := range names {
		subject := name
		if runtime.GOOS == "windows" {
			subject = normcase(name)
		}
		if re.MatchString(subject) == want {
			out = append(out, name)
		}
	}
	return out
}

// Bytes form of fnmatch where every byte is the ISO-8859-1 character of its value
func FnmatchBytes(name, pat []byte) bool {
	if runtime.GOOS == "windows" {
		return FnmatchCase(normcase(string(name)), normcase(string(pat)))
	}
	return FnmatchCaseBytes(name, pat)
}

// Bytes form of fnmatchcase where every byte is the ISO-8859-1 character of its value
func FnmatchCaseBytes(name, pat []byte) bool {
	return Compile(latin1(pat)).MatchString(latin1(name))
}

// Text of each byte read as an ISO-8859-1 code point
func latin1(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// Windows ntpath.normcase lowers and turns slashes into backslashes, posix leaves the text alone
func normcase(s string) string {
	if runtime.GOOS != "windows" {
		return s
	}
	return pystr.Lower(strings.ReplaceAll(s, "/", `\`))
}

// Compiled RE2 form of the pattern from a 32768 entry least recently used cache
func Compile(pat string) *regexp.Regexp {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if el, ok := cacheIndex[pat]; ok {
		cacheOrder.MoveToFront(el)
		return el.Value.(*cacheEntry).re
	}
	re := regexp.MustCompile(`\A` + join(translate(pat, false), false))
	cacheIndex[pat] = cacheOrder.PushFront(&cacheEntry{pat, re})
	if cacheOrder.Len() > cacheSize {
		last := cacheOrder.Back()
		cacheOrder.Remove(last)
		delete(cacheIndex, last.Value.(*cacheEntry).pat)
	}
	return re
}

// CPython 3.14 fnmatch.translate text with atomic star groups and the trailing \z anchor
func Translate(pat string) string {
	return join(translate(pat, true), true)
}

type parts struct {
	res   []string
	stars []int
}

// Splits the pattern into regex pieces and star positions, escaping literals for CPython re or RE2
func translate(pat string, python bool) parts {
	var out parts
	p := []rune(pat)
	n := len(p)
	for i := 0; i < n; {
		c := p[i]
		i++
		switch c {
		case '*':
			out.stars = append(out.stars, len(out.res))
			out.res = append(out.res, ".*")
			for i < n && p[i] == '*' {
				i++
			}
		case '?':
			out.res = append(out.res, ".")
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
				out.res = append(out.res, `\[`)
				continue
			}
			stuff := bracket(p, i, j)
			i = j + 1
			switch {
			case stuff == "":
				if python {
					out.res = append(out.res, "(?!)")
				} else {
					out.res = append(out.res, `[^\x00-\x{10FFFF}]`)
				}
			case stuff == "!":
				out.res = append(out.res, ".")
			default:
				stuff = setOps.ReplaceAllString(stuff, `\$1`)
				if !python {
					stuff = strings.ReplaceAll(stuff, "[", `\[`)
				}
				if stuff[0] == '!' {
					stuff = "^" + stuff[1:]
				} else if stuff[0] == '^' || (python && stuff[0] == '[') {
					stuff = `\` + stuff
				}
				out.res = append(out.res, "["+stuff+"]")
			}
		default:
			if python {
				out.res = append(out.res, ReEscape(string(c)))
			} else {
				out.res = append(out.res, regexp.QuoteMeta(string(c)))
			}
		}
	}
	return out
}

// Set body between i and j with reversed ranges dropped and backslashes and hyphens escaped
func bracket(p []rune, i, j int) string {
	stuff := string(p[i:j])
	if !strings.ContainsRune(stuff, '-') {
		return strings.ReplaceAll(stuff, `\`, `\\`)
	}
	var chunks [][]rune
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
		chunks = append(chunks, append([]rune(nil), p[i:idx]...))
		i = idx + 1
		k = idx + 3
	}
	if i < j {
		chunks = append(chunks, append([]rune(nil), p[i:j]...))
	} else {
		chunks[len(chunks)-1] = append(chunks[len(chunks)-1], '-')
	}
	for k := len(chunks) - 1; k > 0; k-- {
		prev, cur := chunks[k-1], chunks[k]
		if prev[len(prev)-1] > cur[0] {
			chunks[k-1] = append(append([]rune(nil), prev[:len(prev)-1]...), cur[1:]...)
			chunks = append(chunks[:k], chunks[k+1:]...)
		}
	}
	text := make([]string, len(chunks))
	for q, s := range chunks {
		text[q] = strings.ReplaceAll(strings.ReplaceAll(string(s), `\`, `\\`), "-", `\-`)
	}
	return strings.Join(text, "-")
}

// Joins pieces so each interior star and its fixed run match minimally, atomic in CPython text
func join(t parts, python bool) string {
	open, end := "(?s:", `)\z`
	if len(t.stars) == 0 {
		return open + strings.Join(t.res, "") + end
	}
	group := "(?:.*?"
	if python {
		group = "(?>.*?"
	}
	var b strings.Builder
	b.WriteString(open)
	j := t.stars[0]
	b.WriteString(strings.Join(t.res[:j], ""))
	i := j + 1
	for _, j := range t.stars[1:] {
		b.WriteString(group)
		b.WriteString(strings.Join(t.res[i:j], ""))
		b.WriteString(")")
		i = j + 1
	}
	b.WriteString(".*")
	b.WriteString(strings.Join(t.res[i:], ""))
	b.WriteString(end)
	return b.String()
}

// CPython re.escape which backslashes the special characters and whitespace controls
func ReEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("()[]{}?*+-|^$\\.&~# \t\n\r\v\f", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
