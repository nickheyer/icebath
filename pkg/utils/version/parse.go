package versionutil

import "strings"

// Pre-release spellings, longest first so a shorter one never cuts a longer one short, with the kind each stands for
var preLabels = [...]struct{ text, kind string }{
	{"alpha", "a"}, {"beta", "b"}, {"preview", "rc"}, {"pre", "rc"}, {"rc", "rc"}, {"a", "a"}, {"b", "b"}, {"c", "rc"},
}

// Post-release spellings, longest first
var postLabels = [...]string{"post", "rev", "r"}

// Cursor over the trimmed text of one version
type parser struct {
	s string
	i int
}

// Whole text read as a version, false when any of it does not fit the scheme
func (p *parser) version() (*Version, bool) {
	v := &Version{epoch: "0", release: make([]string, 0, 4)}
	if p.i < len(p.s) && (p.s[p.i] == 'v' || p.s[p.i] == 'V') {
		p.i++
	}
	n, ok := p.digits()
	if !ok {
		return nil, false
	}
	if p.i < len(p.s) && p.s[p.i] == '!' {
		p.i++
		v.epoch = n
		if n, ok = p.digits(); !ok {
			return nil, false
		}
	}
	v.release = append(v.release, n)
	for p.i+1 < len(p.s) && p.s[p.i] == '.' && isDigit(p.s[p.i+1]) {
		p.i++
		n, _ = p.digits()
		v.release = append(v.release, n)
	}
	p.preRelease(v)
	p.postRelease(v)
	p.devRelease(v)
	if p.i < len(p.s) && p.s[p.i] == '+' {
		p.i++
		if v.local, ok = p.localLabel(); !ok {
			return nil, false
		}
	}
	return v, p.i == len(p.s)
}

// Digit run at the cursor without its leading zeros, false when there is none
func (p *parser) digits() (string, bool) {
	start := p.i
	for p.i < len(p.s) && isDigit(p.s[p.i]) {
		p.i++
	}
	if p.i == start {
		return "", false
	}
	return trimZeros(p.s[start:p.i]), true
}

// Number after a release label, an optional separator and digits, zero when no digits follow
func (p *parser) labelNumber() string {
	if p.i < len(p.s) && isSeparator(p.s[p.i]) {
		p.i++
	}
	if n, ok := p.digits(); ok {
		return n
	}
	return "0"
}

// Optional separator and a pre-release label with its number
func (p *parser) preRelease(v *Version) {
	j := p.skipSeparator(p.i)
	for _, label := range preLabels {
		if hasLabel(p.s, j, label.text) {
			p.i = j + len(label.text)
			v.preKind, v.pre = label.kind, p.labelNumber()
			return
		}
	}
}

// Post-release, either a dash and digits or an optional separator and a post label with its number
func (p *parser) postRelease(v *Version) {
	if p.i+1 < len(p.s) && p.s[p.i] == '-' && isDigit(p.s[p.i+1]) {
		p.i++
		v.post, _ = p.digits()
		v.hasPost = true
		return
	}
	j := p.skipSeparator(p.i)
	for _, label := range postLabels {
		if hasLabel(p.s, j, label) {
			p.i = j + len(label)
			v.post, v.hasPost = p.labelNumber(), true
			return
		}
	}
}

// Optional separator and a dev label with its number
func (p *parser) devRelease(v *Version) {
	j := p.skipSeparator(p.i)
	if hasLabel(p.s, j, "dev") {
		p.i = j + len("dev")
		v.dev, v.hasDev = p.labelNumber(), true
	}
}

// Local label parts after the plus sign, lower-cased, numeric parts without leading zeros
func (p *parser) localLabel() ([]string, bool) {
	var parts []string
	for {
		start := p.i
		for p.i < len(p.s) && isAlphanumeric(p.s[p.i]) {
			p.i++
		}
		if p.i == start {
			return nil, false
		}
		part := strings.ToLower(p.s[start:p.i])
		if isNumeric(part) {
			part = trimZeros(part)
		}
		parts = append(parts, part)
		if p.i+1 < len(p.s) && isSeparator(p.s[p.i]) && isAlphanumeric(p.s[p.i+1]) {
			p.i++
			continue
		}
		return parts, true
	}
}

// Position after an optional separator at j
func (p *parser) skipSeparator(j int) int {
	if j < len(p.s) && isSeparator(p.s[j]) {
		return j + 1
	}
	return j
}

// Whether s holds label at position j, ASCII letters compared without case
func hasLabel(s string, j int, label string) bool {
	if len(s)-j < len(label) {
		return false
	}
	for k := range len(label) {
		c := s[j+k]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != label[k] {
			return false
		}
	}
	return true
}

// Digit run without its leading zeros, a single zero when nothing else is left
func trimZeros(digits string) string {
	if trimmed := strings.TrimLeft(digits, "0"); trimmed != "" {
		return trimmed
	}
	return "0"
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func isSeparator(c byte) bool { return c == '.' || c == '-' || c == '_' }

func isAlphanumeric(c byte) bool {
	return isDigit(c) || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// Whether a local label part is all digits
func isNumeric(part string) bool {
	for i := range len(part) {
		if !isDigit(part[i]) {
			return false
		}
	}
	return part != ""
}
