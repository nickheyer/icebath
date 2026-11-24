package consoleutil

import (
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"

	"github.com/charmbracelet/colorprofile"
)

type attr uint16

const (
	attrBold attr = 1 << iota
	attrDim
	attrItalic
	attrUnderline
	attrBlink
	attrBlink2
	attrReverse
	attrConceal
	attrStrike
	attrUnderline2
	attrFrame
	attrEncircle
	attrOverline
)

// SGR parameter of each attribute in bit order
var attrCodes = [...]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "21", "51", "52", "53"}

// Attribute a style word or its short form names
func attrByWord(word string) (attr, bool) {
	switch word {
	case "bold", "b":
		return attrBold, true
	case "dim", "d":
		return attrDim, true
	case "italic", "i":
		return attrItalic, true
	case "underline", "u":
		return attrUnderline, true
	case "blink":
		return attrBlink, true
	case "blink2":
		return attrBlink2, true
	case "reverse", "r":
		return attrReverse, true
	case "conceal", "c":
		return attrConceal, true
	case "strike", "s":
		return attrStrike, true
	case "underline2", "uu":
		return attrUnderline2, true
	case "frame":
		return attrFrame, true
	case "encircle":
		return attrEncircle, true
	case "overline", "o":
		return attrOverline, true
	}
	return 0, false
}

// Text style of console markup: foreground and background colors, attributes switched on or explicitly off, a hyperlink
type Style struct {
	fg, bg  colorSpec
	on, off attr
	link    string
}

// Style with every part o sets laid over s
func (s Style) add(o Style) Style {
	if o.fg.kind != colorUnset {
		s.fg = o.fg
	}
	if o.bg.kind != colorUnset {
		s.bg = o.bg
	}
	s.on = s.on&^o.off | o.on
	s.off = s.off&^o.on | o.off
	if o.link != "" {
		s.link = o.link
	}
	return s
}

func (s Style) withLink(link string) Style {
	s.link = link
	return s
}

func (s Style) isZero() bool { return s == Style{} }

// Style a definition such as "bold red on white", "not italic", "#ff8800", "color(208)", "rgb(10,20,30)" or "link https://example.com" describes
func ParseStyle(def string) (Style, error) {
	var st Style
	if def = strings.TrimSpace(def); def == "" || def == "none" {
		return st, nil
	}
	words := strings.Fields(def)
	for i := 0; i < len(words); i++ {
		word := strings.ToLower(words[i])
		switch word {
		case "on":
			if i++; i == len(words) {
				return Style{}, errors.New("color expected after on")
			}
			c, ok := parseColor(words[i])
			if !ok {
				return Style{}, fmt.Errorf("unable to parse %q as background color", words[i])
			}
			st.bg = c
		case "not":
			if i++; i == len(words) {
				return Style{}, errors.New("style attribute expected after not")
			}
			a, ok := attrByWord(strings.ToLower(words[i]))
			if !ok {
				return Style{}, fmt.Errorf("expected style attribute after not, found %q", words[i])
			}
			st.on &^= a
			st.off |= a
		case "link":
			if i++; i == len(words) {
				return Style{}, errors.New("address expected after link")
			}
			st.link = words[i]
		default:
			if a, ok := attrByWord(word); ok {
				st.on |= a
				st.off &^= a
				continue
			}
			c, ok := parseColor(word)
			if !ok {
				return Style{}, fmt.Errorf("unable to parse %q as color", words[i])
			}
			st.fg = c
		}
	}
	return st, nil
}

// Style of a definition, the plain style when it does not parse
func styleOf(def string) Style {
	st, _ := ParseStyle(def)
	return st
}

// SGR parameters drawing the style on the profile, empty when it draws nothing there
func (s Style) sgr(p colorprofile.Profile) string {
	if p <= colorprofile.NoTTY {
		return ""
	}
	var b strings.Builder
	sep := func() {
		if b.Len() > 0 {
			b.WriteByte(';')
		}
	}
	for i, code := range attrCodes {
		if s.on&(1<<i) != 0 {
			sep()
			b.WriteString(code)
		}
	}
	if c := s.fg.codes(true, p); c != "" {
		sep()
		b.WriteString(c)
	}
	if c := s.bg.codes(false, p); c != "" {
		sep()
		b.WriteString(c)
	}
	return b.String()
}

// Text wrapped in the escape sequences that draw it in this style on a terminal with the given color profile, the text alone on a profile without escape sequences
func (s Style) Render(text string, p colorprofile.Profile) string {
	if text == "" || p <= colorprofile.NoTTY {
		return text
	}
	var b strings.Builder
	s.write(&b, text, p, true)
	return b.String()
}

// Writes text in the style, wrapped in an OSC 8 hyperlink when the style links and links are wanted
func (s Style) write(b *strings.Builder, text string, p colorprofile.Profile, links bool) {
	linked := links && s.link != ""
	if linked {
		b.WriteString("\x1b]8;id=")
		b.WriteString(strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(s.link))), 10))
		b.WriteByte(';')
		b.WriteString(s.link)
		b.WriteString("\x1b\\")
	}
	if codes := s.sgr(p); codes != "" {
		b.WriteString("\x1b[")
		b.WriteString(codes)
		b.WriteByte('m')
		b.WriteString(text)
		b.WriteString("\x1b[0m")
	} else {
		b.WriteString(text)
	}
	if linked {
		b.WriteString("\x1b]8;;\x1b\\")
	}
}
