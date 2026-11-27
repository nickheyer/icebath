package web

import (
	"errors"
	"strings"

	"golang.org/x/net/html"
)

// Test a node of a page passes, the steps and predicates of the engines' XPath expressions
type test = func(n *html.Node) bool

// Page parsed, or the error of one with nothing in it
func parse(text string) (*html.Node, error) {
	doc, err := html.Parse(strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	for n := range doc.Descendants() {
		switch n.Type {
		case html.TextNode:
			if strings.Trim(n.Data, " \t\n\f\r") != "" {
				return doc, nil
			}
		case html.ElementNode:
			// html, head and body are there even when the page has none
			if len(n.Attr) > 0 || n.Data != "html" && n.Data != "head" && n.Data != "body" {
				return doc, nil
			}
		}
	}
	return nil, errors.New("the page is empty")
}

// Elements under n that pass, in document order
func elements(n *html.Node, keep test) []*html.Node {
	var out []*html.Node
	for d := range n.Descendants() {
		if d.Type == html.ElementNode && keep(d) {
			out = append(out, d)
		}
	}
	return out
}

// Text of the text nodes under n that pass, joined, its whitespace collapsed
func texts(n *html.Node, keep test) string {
	var b strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode && keep(d) {
			b.WriteString(d.Data)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// Values of the attribute on the elements under n that pass, joined, their whitespace collapsed
func values(n *html.Node, name string, keep test) string {
	var b strings.Builder
	for _, element := range elements(n, keep) {
		if value, ok := attr(element, name); ok {
			b.WriteString(value)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func attr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

// Element of the tag
func el(tag string) test {
	return func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == tag }
}

func and(tests ...test) test {
	return func(n *html.Node) bool {
		for _, t := range tests {
			if !t(n) {
				return false
			}
		}
		return true
	}
}

// Class holding one of the parts: contains(@class, part)
func classed(parts ...string) test {
	return func(n *html.Node) bool {
		class, _ := attr(n, "class")
		for _, part := range parts {
			if strings.Contains(class, part) {
				return true
			}
		}
		return false
	}
}

// Attribute of exactly the value: @name='value'
func valued(name, value string) test {
	return func(n *html.Node) bool {
		v, ok := attr(n, name)
		return ok && v == value
	}
}

// Child of the node
func childOf(top *html.Node) test {
	return func(n *html.Node) bool { return n.Parent == top }
}

// Parent passing the test
func parent(t test) test {
	return func(n *html.Node) bool { return n.Parent != nil && t(n.Parent) }
}

// Some child passing the test: a predicate such as [a]
func holding(t test) test {
	return func(n *html.Node) bool {
		for c := range n.ChildNodes() {
			if t(c) {
				return true
			}
		}
		return false
	}
}

// Some ancestor below top passing the test: what .//x// selects under the context node top
func within(top *html.Node, t test) test {
	return func(n *html.Node) bool {
		for p := n.Parent; p != nil && p != top; p = p.Parent {
			if t(p) {
				return true
			}
		}
		return false
	}
}

// K-th child of the tag, from 1, passing the test: ./tag[k][...]
func child(tag string, k int, t test) test {
	return func(n *html.Node) bool {
		i := 0
		for c := range n.ChildNodes() {
			if c.Type == html.ElementNode && c.Data == tag {
				if i++; i == k {
					return t(c)
				}
			}
		}
		return false
	}
}

// The parent's k-th child of the element's tag, from 1: tag[k]
func at(k int) test {
	return func(n *html.Node) bool {
		i, _ := place(n)
		return i == k
	}
}

// The parent's last child of the element's tag: tag[position()=last()]
func last(n *html.Node) bool {
	i, count := place(n)
	return i == count
}

// Place of the element among the parent's children of its tag, from 1, and how many they are
func place(n *html.Node) (int, int) {
	i, count := 0, 0
	for c := range n.Parent.ChildNodes() {
		if c.Type == html.ElementNode && c.Data == n.Data {
			count++
			if c == n {
				i = count
			}
		}
	}
	return i, count
}
