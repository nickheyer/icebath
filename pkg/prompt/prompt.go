// Package prompt builds every text put to a model, system prompts and messages alike, from blocks rendered as markdown
package prompt

import (
	"fmt"
	"strings"
)

// Text in blocks, each a run of lines, rendered with one blank line between blocks
type Doc struct{ blocks [][]string }

func New() *Doc { return &Doc{} }

// Key and value of one line, or one attribute of a tag
type Field struct{ Key, Value string }

// Adds a block of the lines with the blank ones dropped, nothing when every line is blank
func (d *Doc) Para(lines ...string) *Doc {
	var block []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			block = append(block, strings.TrimRight(l, " \t"))
		}
	}
	if len(block) > 0 {
		d.blocks = append(d.blocks, block)
	}
	return d
}

// Adds one formatted line as a block
func (d *Doc) Paraf(format string, args ...any) *Doc { return d.Para(fmt.Sprintf(format, args...)) }

// Adds a heading of the level as a block
func (d *Doc) Heading(level int, title string) *Doc { return d.Para(H(level, title)) }

// Adds a bulleted block, lines after the first of an item indented under it, nothing when every item is blank
func (d *Doc) Bullets(items ...string) *Doc { return d.Para(marked(items)...) }

// Adds a title line over bulleted items, nothing when every item is blank
func (d *Doc) Titled(title string, items ...string) *Doc {
	lines := marked(items)
	if len(lines) == 0 {
		return d
	}
	return d.Para(append([]string{title}, lines...)...)
}

// Adds the text between an opening and a closing tag, attributes in order, nothing when the text is blank
func (d *Doc) Tagged(tag, text string, attrs ...Field) *Doc {
	if strings.TrimSpace(text) == "" {
		return d
	}
	open := tag
	for _, a := range attrs {
		open += fmt.Sprintf(" %s=%q", a.Key, a.Value)
	}
	d.blocks = append(d.blocks, []string{"<" + open + ">", strings.TrimSpace(text), "</" + tag + ">"})
	return d
}

// Adds text already written, each of its paragraphs becoming a block
func (d *Doc) Raw(text string) *Doc {
	for _, p := range strings.Split(strings.TrimSpace(text), "\n\n") {
		d.Para(strings.Split(p, "\n")...)
	}
	return d
}

// Adds the blocks of another document
func (d *Doc) Add(o *Doc) *Doc {
	if o != nil {
		d.blocks = append(d.blocks, o.blocks...)
	}
	return d
}

// Blocks joined by blank lines
func (d *Doc) String() string {
	parts := make([]string, 0, len(d.blocks))
	for _, b := range d.blocks {
		parts = append(parts, strings.Join(b, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// Bullet lines of the items, blank ones dropped, later lines of an item indented under its bullet
func marked(items []string) []string {
	var out []string
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		for j, l := range strings.Split(it, "\n") {
			if j == 0 {
				out = append(out, "- "+l)
			} else {
				out = append(out, "  "+l)
			}
		}
	}
	return out
}

// Heading line of the level
func H(level int, title string) string { return strings.Repeat("#", max(level, 1)) + " " + title }

// Key and value on one line, empty when the value is blank so a block drops it
func KV(key, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return key + ": " + value
}

// Texts joined by blank lines, blank ones left out
func Compose(texts ...string) string {
	var parts []string
	for _, t := range texts {
		if t = strings.TrimSpace(t); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Sentences joined by one space, blank ones left out
func Sentences(sentences ...string) string {
	var parts []string
	for _, s := range sentences {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// Text with its first letter in upper case
func Cap(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
