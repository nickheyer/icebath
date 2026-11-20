package pyemail

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Param is one header parameter, RFC2231 set when the value is the charset, language and text triple
type Param struct {
	Name     string
	Value    string
	RFC2231  bool
	Charset  *string
	Language *string
}

// ErrUnorderable is the TypeError of sorting continuations that mix numbered and unnumbered segments
var ErrUnorderable = errors.New("'<' not supported between instances of 'NoneType' and 'int'")

var rfc2231Continuation = regexp.MustCompile(`^([A-Za-z0-9_]+)\*(([0-9]+)\*?)?$`)

// Parameter segments split on semicolons outside quotes with lowercased names
func parseParam(s string) []string {
	s = ";" + s
	var plist []string
	for strings.HasPrefix(s, ";") {
		s = s[1:]
		end := strings.Index(s, ";")
		for end > 0 && (strings.Count(s[:end], `"`)-strings.Count(s[:end], `\"`))%2 != 0 {
			next := strings.Index(s[end+1:], ";")
			if next < 0 {
				end = -1
			} else {
				end += 1 + next
			}
		}
		if end < 0 {
			end = len(s)
		}
		f := s[:end]
		if i := strings.Index(f, "="); i >= 0 {
			f = pystr.Lower(pystr.Strip(f[:i])) + "=" + pystr.Strip(f[i+1:])
		}
		plist = append(plist, pystr.Strip(f))
		s = s[end:]
	}
	return plist
}

// Backslashes and double quotes escaped with a backslash
func Quote(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
}

// Surrounding double quotes or angle brackets removed, quoted escapes undone
func Unquote(s string) string {
	if utf8.RuneCountInString(s) > 1 {
		if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
			return strings.ReplaceAll(strings.ReplaceAll(s[1:len(s)-1], `\\`, `\`), `\"`, `"`)
		}
		if strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") {
			return s[1 : len(s)-1]
		}
	}
	return s
}

type continuation struct {
	num     *int
	value   string
	encoded bool
}

// RFC2231 continuations joined in numeric order, plain parameters quoted, an error when numbering is mixed
func decodeParams(params [][2]string) ([]Param, error) {
	out := []Param{{Name: params[0][0], Value: params[0][1]}}
	var order []string
	groups := map[string][]continuation{}
	for _, p := range params[1:] {
		name, value := p[0], Unquote(p[1])
		encoded := strings.HasSuffix(name, "*")
		if m := rfc2231Continuation.FindStringSubmatch(name); m != nil {
			c := continuation{value: value, encoded: encoded}
			if m[3] != "" {
				n, _ := strconv.Atoi(m[3])
				c.num = &n
			}
			if _, seen := groups[m[1]]; !seen {
				order = append(order, m[1])
			}
			groups[m[1]] = append(groups[m[1]], c)
			continue
		}
		out = append(out, Param{Name: name, Value: `"` + Quote(value) + `"`})
	}
	for _, name := range order {
		conts := groups[name]
		numbered := 0
		for _, c := range conts {
			if c.num != nil {
				numbered++
			}
		}
		if len(conts) > 1 && numbered > 0 && numbered < len(conts) {
			return nil, ErrUnorderable
		}
		sort.SliceStable(conts, func(i, j int) bool {
			a, b := conts[i], conts[j]
			if a.num != nil && b.num != nil && *a.num != *b.num {
				return *a.num < *b.num
			}
			if a.value != b.value {
				return a.value < b.value
			}
			return !a.encoded && b.encoded
		})
		var joined strings.Builder
		extended := false
		for _, c := range conts {
			s := c.value
			if c.encoded {
				s = pyurl.UnquoteLatin1(s)
				extended = true
			}
			joined.WriteString(s)
		}
		value := Quote(joined.String())
		if !extended {
			out = append(out, Param{Name: name, Value: `"` + value + `"`})
			continue
		}
		parts := strings.SplitN(value, "'", 3)
		if len(parts) <= 2 {
			out = append(out, Param{Name: name, Value: `"` + value + `"`, RFC2231: true})
			continue
		}
		out = append(out, Param{Name: name, Value: `"` + parts[2] + `"`, RFC2231: true, Charset: &parts[0], Language: &parts[1]})
	}
	return out, nil
}

// Unquoted parameters of a header value, the first being the main value with an empty parameter value
func GetParams(header string) ([]Param, error) {
	var params [][2]string
	for _, p := range parseParam(header) {
		name, val, ok := strings.Cut(p, "=")
		if ok {
			params = append(params, [2]string{pystr.Strip(name), pystr.Strip(val)})
		} else {
			params = append(params, [2]string{pystr.Strip(p), ""})
		}
	}
	decoded, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	for i := range decoded {
		decoded[i].Value = Unquote(decoded[i].Value)
	}
	return decoded, nil
}

// Text of a parameter, an RFC2231 triple decoded from its charset with replacement, us-ascii when unnamed
func CollapseRFC2231Value(p Param) string {
	if !p.RFC2231 {
		return Unquote(p.Value)
	}
	charset := "us-ascii"
	if p.Charset != nil {
		charset = *p.Charset
	}
	var raw []byte
	for _, r := range p.Value {
		switch {
		case r < 0x100:
			raw = append(raw, byte(r))
		case r < 0x10000:
			raw = append(raw, []byte(`\u`+leftPadHex(r, 4))...)
		default:
			raw = append(raw, []byte(`\U`+leftPadHex(r, 8))...)
		}
	}
	text, err := pycodecs.DecodeReplace(raw, charset)
	var lookup *pycodecs.LookupError
	if errors.As(err, &lookup) {
		return Unquote(p.Value)
	}
	return text
}

// Lowercase hex of the code point padded with zeros to the width
func leftPadHex(r rune, width int) string {
	h := strconv.FormatInt(int64(r), 16)
	return strings.Repeat("0", width-len(h)) + h
}
