package web

import (
	"html"
	"regexp"
	"strings"
)

var (
	blockTags   = map[string]bool{"p": true, "div": true, "br": true, "tr": true, "section": true, "article": true, "header": true, "footer": true, "nav": true, "ul": true, "ol": true, "table": true, "blockquote": true, "pre": true, "hr": true, "main": true, "aside": true, "form": true, "dd": true, "dt": true}
	skipTags    = map[string]bool{"script": true, "style": true, "noscript": true, "template": true, "svg": true}
	multiSpace  = regexp.MustCompile(`[ \t\r\f\v]+`)
	multiBreaks = regexp.MustCompile(`\n{3,}`)
)

// Converts an HTML document to plain text, keeping headings, list bullets and links
func Text(doc string) string {
	var out strings.Builder
	var title strings.Builder
	inTitle := false
	var link string
	var linkText strings.Builder
	i := 0
	for i < len(doc) {
		if doc[i] != '<' {
			j := strings.IndexByte(doc[i:], '<')
			if j < 0 {
				j = len(doc) - i
			}
			text := html.UnescapeString(doc[i : i+j])
			switch {
			case inTitle:
				title.WriteString(text)
			case link != "":
				linkText.WriteString(text)
			default:
				out.WriteString(text)
			}
			i += j
			continue
		}
		if strings.HasPrefix(doc[i:], "<!--") {
			end := strings.Index(doc[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		end := tagEnd(doc, i)
		if end < 0 {
			break
		}
		raw := doc[i+1 : end]
		i = end + 1
		closing := strings.HasPrefix(raw, "/")
		name, attrs := splitTag(strings.TrimPrefix(raw, "/"))
		switch {
		case name == "" || strings.HasPrefix(name, "!") || strings.HasPrefix(name, "?"):
			continue
		case skipTags[name] && !closing:
			if j := closeOf(doc, i, name); j >= 0 {
				i = j
			} else {
				i = len(doc)
			}
		case name == "title":
			inTitle = !closing
		case name == "a":
			if closing {
				text := strings.TrimSpace(multiSpace.ReplaceAllString(linkText.String(), " "))
				switch {
				case link == "":
					out.WriteString(text)
				case text == "":
					out.WriteString(link)
				default:
					out.WriteString("[")
					out.WriteString(text)
					out.WriteString("](")
					out.WriteString(link)
					out.WriteString(")")
				}
				link = ""
				linkText.Reset()
			} else {
				href := attr(attrs, "href")
				if href != "" && !strings.HasPrefix(strings.ToLower(href), "javascript:") {
					link = href
				}
			}
		case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
			if closing {
				out.WriteString("\n\n")
			} else {
				out.WriteString("\n\n")
				out.WriteString(strings.Repeat("#", int(name[1]-'0')))
				out.WriteString(" ")
			}
		case name == "li":
			if !closing {
				out.WriteString("\n- ")
			}
		case name == "td" || name == "th":
			if !closing {
				out.WriteString(" ")
			}
		case name == "img":
			if alt := attr(attrs, "alt"); alt != "" {
				out.WriteString("[image: ")
				out.WriteString(alt)
				out.WriteString("]")
			}
		case blockTags[name]:
			out.WriteString("\n")
		}
	}
	text := out.String()
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(multiSpace.ReplaceAllString(l, " "))
	}
	text = strings.TrimSpace(multiBreaks.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
	if t := strings.TrimSpace(multiSpace.ReplaceAllString(title.String(), " ")); t != "" {
		return "# " + t + "\n\n" + text
	}
	return text
}

// Finds the closing angle bracket of the tag starting at i, honouring quoted attribute values
func tagEnd(doc string, i int) int {
	quote := byte(0)
	for j := i + 1; j < len(doc); j++ {
		c := doc[j]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return j
		}
	}
	return -1
}

// Splits a tag body into its lower case name and the raw attribute text
func splitTag(raw string) (string, string) {
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "/"))
	end := strings.IndexAny(raw, " \t\r\n/")
	if end < 0 {
		return strings.ToLower(raw), ""
	}
	return strings.ToLower(raw[:end]), raw[end:]
}

// Reads one attribute value, quoted or bare
func attr(attrs, name string) string {
	lower := strings.ToLower(attrs)
	for from := 0; from < len(lower); {
		k := strings.Index(lower[from:], name)
		if k < 0 {
			return ""
		}
		k += from
		from = k + len(name)
		if k > 0 && !strings.ContainsRune(" \t\r\n", rune(lower[k-1])) {
			continue
		}
		rest := strings.TrimLeft(attrs[k+len(name):], " \t\r\n")
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		rest = strings.TrimLeft(rest[1:], " \t\r\n")
		if rest == "" {
			return ""
		}
		if q := rest[0]; q == '"' || q == '\'' {
			if end := strings.IndexByte(rest[1:], q); end >= 0 {
				return html.UnescapeString(rest[1 : 1+end])
			}
			return html.UnescapeString(rest[1:])
		}
		end := strings.IndexAny(rest, " \t\r\n>")
		if end < 0 {
			end = len(rest)
		}
		return html.UnescapeString(rest[:end])
	}
	return ""
}

// Finds the position just after the closing tag of name, case insensitively
func closeOf(doc string, from int, name string) int {
	lower := strings.ToLower(doc[from:])
	k := strings.Index(lower, "</"+name)
	if k < 0 {
		return -1
	}
	end := strings.IndexByte(doc[from+k:], '>')
	if end < 0 {
		return -1
	}
	return from + k + end + 1
}
