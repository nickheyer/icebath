package pyrich

import (
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"hash/crc32"
	"math"
	"os"
	"strconv"
	"strings"
)

// Rich style whose unset fields inherit, an attribute is 1 for on, -1 for explicitly off and 0 for unset
type Style struct {
	Color, Bgcolor                                                string
	Bold, Dim, Italic, Underline, Blink, Blink2, Reverse, Conceal int8
	Strike, Underline2, Frame, Encircle, Overline                 int8
	Link                                                          string
}

// Attribute fields in rich bit order with the SGR code and style word of each
func (s *Style) attrs() [13]*int8 {
	return [13]*int8{&s.Bold, &s.Dim, &s.Italic, &s.Underline, &s.Blink, &s.Blink2, &s.Reverse, &s.Conceal,
		&s.Strike, &s.Underline2, &s.Frame, &s.Encircle, &s.Overline}
}

var attrCodes = [13]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "21", "51", "52", "53"}

var attrWords = [13]string{"bold", "dim", "italic", "underline", "blink", "blink2", "reverse", "conceal", "strike",
	"underline2", "frame", "encircle", "overline"}

var styleAttributes = map[string]int{"dim": 1, "d": 1, "bold": 0, "b": 0, "italic": 2, "i": 2, "underline": 3, "u": 3,
	"blink": 4, "blink2": 5, "reverse": 6, "r": 6, "conceal": 7, "c": 7, "strike": 8, "s": 8, "underline2": 9, "uu": 9,
	"frame": 10, "encircle": 11, "overline": 12, "o": 12}

// Combined style where every field set in o overrides s, as rich Style addition
func (s Style) Add(o Style) Style {
	if o.Color != "" {
		s.Color = o.Color
	}
	if o.Bgcolor != "" {
		s.Bgcolor = o.Bgcolor
	}
	dst, src := s.attrs(), o.attrs()
	for i := range dst {
		if *src[i] != 0 {
			*dst[i] = *src[i]
		}
	}
	if o.Link != "" {
		s.Link = o.Link
	}
	return s
}

// Style a rich style definition describes, an error where Style.parse raises StyleSyntaxError
func ParseStyle(def string) (Style, error) {
	var st Style
	if strings.TrimSpace(def) == "none" || def == "" {
		return st, nil
	}
	words := strings.Fields(def)
	for i := 0; i < len(words); i++ {
		word := strings.ToLower(words[i])
		next := func() string {
			if i+1 < len(words) {
				i++
				return words[i]
			}
			return ""
		}
		switch {
		case word == "on":
			w := next()
			if w == "" {
				return Style{}, fmt.Errorf("color expected after 'on'")
			}
			if _, ok := parseColor(w); !ok {
				return Style{}, fmt.Errorf("unable to parse %s as background color", pyrepr.StrRepr(w))
			}
			st.Bgcolor = strings.ToLower(strings.TrimSpace(w))
		case word == "not":
			w := next()
			idx, ok := styleAttributes[w]
			if !ok {
				return Style{}, fmt.Errorf("expected style attribute after 'not', found %s", pyrepr.StrRepr(w))
			}
			*st.attrs()[idx] = -1
		case word == "link":
			w := next()
			if w == "" {
				return Style{}, fmt.Errorf("URL expected after 'link'")
			}
			st.Link = w
		default:
			if idx, ok := styleAttributes[word]; ok {
				*st.attrs()[idx] = 1
				continue
			}
			if _, ok := parseColor(word); !ok {
				return Style{}, fmt.Errorf("unable to parse %s as color", pyrepr.StrRepr(word))
			}
			st.Color = word
		}
	}
	return st, nil
}

// Definition text of the style in rich order, none for the null style, as str(Style)
func (s Style) String() string {
	var words []string
	for i, a := range s.attrs() {
		switch *a {
		case 1:
			words = append(words, attrWords[i])
		case -1:
			words = append(words, "not "+attrWords[i])
		}
	}
	if s.Color != "" {
		words = append(words, s.Color)
	}
	if s.Bgcolor != "" {
		words = append(words, "on "+s.Bgcolor)
	}
	if s.Link != "" {
		words = append(words, "link "+s.Link)
	}
	if len(words) == 0 {
		return "none"
	}
	return strings.Join(words, " ")
}

// Lower case canonical form of a style definition, the stripped lower text when it does not parse
func normalizeStyle(def string) string {
	if st, err := ParseStyle(def); err == nil {
		return st.String()
	}
	return strings.ToLower(strings.TrimSpace(def))
}

var standardPalette = [16][3]int{{0, 0, 0}, {170, 0, 0}, {0, 170, 0}, {170, 85, 0}, {0, 0, 170}, {170, 0, 170}, {0, 170, 170}, {170, 170, 170},
	{85, 85, 85}, {255, 85, 85}, {85, 255, 85}, {255, 255, 85}, {85, 85, 255}, {255, 85, 255}, {85, 255, 255}, {255, 255, 255}}

// Terminal color system rich detects, truecolor from COLORTERM, then 256 or standard from the TERM suffix
func TermColorSystem() string {
	if ct := strings.ToLower(os.Getenv("COLORTERM")); ct == "truecolor" || ct == "24bit" {
		return "truecolor"
	}
	term := strings.ToLower(os.Getenv("TERM"))
	if i := strings.LastIndex(term, "-"); i >= 0 {
		term = term[i+1:]
	}
	if term == "kitty" || term == "256color" {
		return "256"
	}
	return "standard"
}

func standardCode(n int, fg bool) string {
	base := 30
	if !fg {
		base = 40
	}
	if n >= 8 {
		return strconv.Itoa(base + 60 + n - 8)
	}
	return strconv.Itoa(base + n)
}

// Nearest standard palette index by rich weighted distance, the first index winning ties
func matchStandard(r, g, b int) int {
	best, bestDist := 0, -1
	for i, c := range standardPalette {
		mean := (r + c[0]) / 2
		dr, dg, db := r-c[0], g-c[1], b-c[2]
		d := (((512 + mean) * dr * dr) >> 8) + 4*dg*dg + (((767 - mean) * db * db) >> 8)
		if bestDist < 0 || d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
}

// Index in the 256 color palette rich downgrades a truecolor triplet to
func eightBit(r, g, b int) int {
	rf, gf, bf := float64(r)/255, float64(g)/255, float64(b)/255
	maxc, minc := math.Max(rf, math.Max(gf, bf)), math.Min(rf, math.Min(gf, bf))
	l := (minc + maxc) / 2
	s := 0.0
	if maxc != minc {
		if l <= 0.5 {
			s = (maxc - minc) / (maxc + minc)
		} else {
			s = (maxc - minc) / (2.0 - maxc - minc)
		}
	}
	if s < 0.15 {
		switch gray := pyRound(l * 25.0); gray {
		case 0:
			return 16
		case 25:
			return 231
		default:
			return 231 + gray
		}
	}
	six := func(v int) int {
		if v < 95 {
			return pyRound(float64(v) / 95)
		}
		return pyRound(1 + float64(v-95)/40)
	}
	return 16 + 36*six(r) + 6*six(g) + six(b)
}

// SGR parameters of a color name on the given color system, empty when the name does not parse
func colorCodes(name string, fg bool, system string) string {
	c, ok := parseColor(name)
	if !ok {
		return ""
	}
	return c.downgrade(system).codes(fg)
}

// Text wrapped in the SGR codes and OSC 8 hyperlink rich emits for a segment of this style
func (s Style) Render(text, system string) string {
	if text == "" {
		return text
	}
	var codes []string
	for i, a := range s.attrs() {
		if *a == 1 {
			codes = append(codes, attrCodes[i])
		}
	}
	if c := colorCodes(s.Color, true, system); s.Color != "" && c != "" {
		codes = append(codes, c)
	}
	if c := colorCodes(s.Bgcolor, false, system); s.Bgcolor != "" && c != "" {
		codes = append(codes, c)
	}
	out := text
	if len(codes) > 0 {
		out = "\x1b[" + strings.Join(codes, ";") + "m" + text + "\x1b[0m"
	}
	if s.Link != "" {
		out = fmt.Sprintf("\x1b]8;id=%d;%s\x1b\\%s\x1b]8;;\x1b\\", crc32.ChecksumIEEE([]byte(s.Link)), s.Link, out)
	}
	return out
}
