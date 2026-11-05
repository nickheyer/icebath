package tools

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/encoding/htmlindex"
)

// Element tree node with ElementTree text and tail semantics and Clark notation tags
type etNode struct {
	tag      string
	attrs    map[string]string
	text     string
	tail     string
	children []*etNode
}

func etName(n xml.Name) string {
	if n.Space == "" {
		return n.Local
	}
	return "{" + n.Space + "}" + n.Local
}

// Decodes a UTF-16 document with a byte order mark into UTF-8
func transcodeUTF16(data []byte) ([]byte, bool) {
	if len(data) < 2 {
		return data, false
	}
	var big bool
	switch {
	case data[0] == 0xFE && data[1] == 0xFF:
		big = true
	case data[0] == 0xFF && data[1] == 0xFE:
	default:
		return data, false
	}
	units := make([]uint16, 0, len(data)/2)
	for i := 2; i+1 < len(data); i += 2 {
		if big {
			units = append(units, uint16(data[i])<<8|uint16(data[i+1]))
		} else {
			units = append(units, uint16(data[i+1])<<8|uint16(data[i]))
		}
	}
	return []byte(string(utf16.Decode(units))), true
}

// Parses an XML document into an element tree the way ElementTree.fromstring does
func etParse(data []byte) (*etNode, error) {
	transcoded := false
	data, transcoded = transcodeUTF16(data)
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		l := strings.ToLower(strings.TrimSpace(label))
		if (transcoded && strings.HasPrefix(l, "utf-16")) || l == "utf-8" || l == "us-ascii" || l == "ascii" {
			return input, nil
		}
		enc, err := htmlindex.Get(l)
		if err != nil {
			return nil, err
		}
		return enc.NewDecoder().Reader(input), nil
	}
	var root *etNode
	var stack []*etNode
	var last *etNode
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &etNode{tag: etName(t.Name), attrs: map[string]string{}}
			for _, a := range t.Attr {
				if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
					continue
				}
				n.attrs[etName(a.Name)] = a.Value
			}
			if len(stack) == 0 {
				if root != nil {
					return nil, &xml.SyntaxError{Msg: "junk after document element", Line: xmlLine(dec)}
				}
				root = n
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			}
			stack = append(stack, n)
			last = nil
		case xml.EndElement:
			last = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
		case xml.CharData:
			switch {
			case last != nil:
				last.tail += string(t)
			case len(stack) > 0:
				stack[len(stack)-1].text += string(t)
			case len(bytes.TrimSpace(t)) > 0:
				return nil, &xml.SyntaxError{Msg: "text outside the document element", Line: xmlLine(dec)}
			}
		}
	}
	if root == nil {
		return nil, &xml.SyntaxError{Msg: "no element found", Line: xmlLine(dec)}
	}
	if len(stack) > 0 {
		return nil, &xml.SyntaxError{Msg: "unclosed token", Line: xmlLine(dec)}
	}
	return root, nil
}

func xmlLine(dec *xml.Decoder) int {
	line, _ := dec.InputPos()
	return line
}

// Visits the node and its descendants in document order, all of them when tag is empty
func (n *etNode) iter(tag string, fn func(*etNode)) {
	if tag == "" || n.tag == tag {
		fn(n)
	}
	for _, c := range n.children {
		c.iter(tag, fn)
	}
}

// Descendants including the node itself that carry tag, in document order
func (n *etNode) all(tag string) []*etNode {
	var out []*etNode
	n.iter(tag, func(x *etNode) { out = append(out, x) })
	return out
}

// First direct child with tag or nil
func (n *etNode) find(tag string) *etNode {
	for _, c := range n.children {
		if c.tag == tag {
			return c
		}
	}
	return nil
}

// Text of the first direct child with tag and whether that child exists
func (n *etNode) findtext(tag string) (string, bool) {
	if c := n.find(tag); c != nil {
		return c.text, true
	}
	return "", false
}

// Attribute value or the fallback when absent
func (n *etNode) get(attr, fallback string) string {
	if v, ok := n.attrs[attr]; ok {
		return v
	}
	return fallback
}

// Concatenated text of the node and every descendant in document order
func (n *etNode) itertext() string {
	var b strings.Builder
	var walk func(x *etNode)
	walk = func(x *etNode) {
		b.WriteString(x.text)
		for _, c := range x.children {
			walk(c)
			b.WriteString(c.tail)
		}
	}
	walk(n)
	return b.String()
}
