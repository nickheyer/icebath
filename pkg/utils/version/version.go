package versionutil

import (
	"strconv"
	"strings"
)

// Parsed release label of a package: an epoch, dotted release numbers, an optional a/b/rc pre-release, post-release and dev-release, and a local label
type Version struct {
	epoch   string
	release []string
	preKind string
	pre     string
	post    string
	dev     string
	hasPost bool
	hasDev  bool
	local   []string
}

// Error of text that is not a version, carrying the text
type ParseError struct {
	Text string
}

// The rejected text, as in invalid version "1.0-"
func (e *ParseError) Error() string { return "invalid version " + strconv.Quote(e.Text) }

// Version of text with surrounding blanks, a leading v, letter case and separator spelling forgiven, a *ParseError when it is none
func Parse(text string) (*Version, error) {
	p := parser{s: strings.TrimSpace(text)}
	v, ok := p.version()
	if !ok {
		return nil, &ParseError{Text: text}
	}
	return v, nil
}

// Negative when a sorts before b, positive when after, zero when they are the same version
func Compare(a, b *Version) int {
	if c := compareNumber(a.epoch, b.epoch); c != 0 {
		return c
	}
	if c := compareRelease(a.release, b.release); c != 0 {
		return c
	}
	if c := comparePre(a, b); c != 0 {
		return c
	}
	if c := compareOptional(a.post, a.hasPost, b.post, b.hasPost, -1); c != 0 {
		return c
	}
	if c := compareOptional(a.dev, a.hasDev, b.dev, b.hasDev, 1); c != 0 {
		return c
	}
	return compareLocal(a.local, b.local)
}

// Normalized text of the version, as in 1!2.0rc1.post2.dev3+ubuntu.1
func (v *Version) String() string {
	var b strings.Builder
	if v.epoch != "0" {
		b.WriteString(v.epoch)
		b.WriteByte('!')
	}
	for i, part := range v.release {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(part)
	}
	if v.preKind != "" {
		b.WriteString(v.preKind)
		b.WriteString(v.pre)
	}
	if v.hasPost {
		b.WriteString(".post")
		b.WriteString(v.post)
	}
	if v.hasDev {
		b.WriteString(".dev")
		b.WriteString(v.dev)
	}
	for i, part := range v.local {
		if i == 0 {
			b.WriteByte('+')
		} else {
			b.WriteByte('.')
		}
		b.WriteString(part)
	}
	return b.String()
}
