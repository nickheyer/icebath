package document

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

// Most characters of a code cell's outputs shown
const maxOutputChars = 20000

// Jupyter notebook, its cells or the worksheets of nbformat 3, as labeled cells with compact outputs; worksheets, cells and outputs of
// an unexpected shape skipped
func notebook(data []byte, name string) (string, error) {
	root, err := parseJSON(decodeUTF8(transcode(data)))
	if err != nil {
		return "", fail("Not a valid notebook: %s", err.Error())
	}
	if root.kind != '{' {
		return "", fail("Notebook root is not an object")
	}
	type cell struct {
		// jq path of its outputs
		outputs string
		node    *node
	}
	var cells []cell
	if list, ok := root.get("cells").array(); ok {
		for i, c := range list {
			cells = append(cells, cell{fmt.Sprintf(".cells[%d].outputs", i), c})
		}
	} else {
		worksheets, _ := root.get("worksheets").array()
		for w, sheet := range worksheets {
			list, _ := sheet.get("cells").array()
			for i, c := range list {
				cells = append(cells, cell{fmt.Sprintf(".worksheets[%d].cells[%d].outputs", w, i), c})
			}
		}
	}
	if len(cells) == 0 {
		return "", fail("Notebook contains no cells")
	}
	counts := map[string]int{}
	var lines []string
	for _, c := range cells {
		kind, _ := c.node.get("cell_type").text()
		label := cellLabel(kind)
		if label == "" {
			continue
		}
		counts[kind]++
		heading := label + " cell"
		if kind != "raw" {
			heading += " " + strconv.Itoa(counts[kind])
		}
		lines = append(lines, "# ── "+heading+" ──", strings.TrimRight(sourceText(c.node.get("source")), "\n"), "")
		if kind != "code" {
			continue
		}
		if out := outputs(c.node, c.outputs, name); out != "" {
			lines = append(lines, fmt.Sprintf("# ── Output (cell %d) ──", counts[kind]), strings.TrimRight(out, "\n"), "")
		}
	}
	return joined(lines, "Notebook contains no readable cells")
}

func cellLabel(kind string) string {
	switch kind {
	case "markdown":
		return "Markdown"
	case "code":
		return "Code"
	case "raw":
		return "Raw"
	}
	return ""
}

// Text of a source or text field, the string fragments of a list joined
func sourceText(n *node) string {
	if s, ok := n.text(); ok {
		return s
	}
	items, _ := n.array()
	var b strings.Builder
	for _, item := range items {
		if s, ok := item.text(); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

// Value of a field read as a scalar: a string as is, a null as nothing, anything else as written
func scalarText(n *node) string {
	if n == nil || n.kind == 'n' {
		return ""
	}
	if s, ok := n.text(); ok {
		return s
	}
	return n.raw
}

// Outputs of a code cell one after another, cut at the character budget with the jq command that prints them whole
func outputs(cell *node, path, name string) string {
	list, _ := cell.get("outputs").array()
	var parts []string
	for _, out := range list {
		if text := outputText(out); text != "" {
			parts = append(parts, text)
		}
	}
	text := strings.Join(parts, "\n")
	total := utf8.RuneCountInString(text)
	if total <= maxOutputChars {
		return text
	}
	hint := ""
	if name != "" {
		hint = " — full output: jq -r '" + path + "' " + terminal.Quote(name)
	}
	return strs.Head(text, maxOutputChars) + fmt.Sprintf("\n… [%s output chars truncated%s]", strs.Thousands(total-maxOutputChars), hint)
}

// One output as compact text with heavy payloads replaced by their size, empty for one of no known type
func outputText(out *node) string {
	kind, _ := out.get("output_type").text()
	switch kind {
	case "stream":
		body := cleanStream(sourceText(out.get("text")))
		if strings.TrimSpace(body) == "" {
			return ""
		}
		return body
	case "error", "pyerr":
		items, _ := out.get("traceback").array()
		var trace []string
		for _, item := range items {
			if s, ok := item.text(); ok {
				trace = append(trace, s)
			}
		}
		header := strings.TrimRight("Error: "+scalarText(out.get("ename"))+": "+scalarText(out.get("evalue")), ": ")
		return strings.TrimRightFunc(header+"\n"+cleanStream(strings.Join(trace, "\n")), unicode.IsSpace)
	case "execute_result", "display_data", "pyout":
		return richOutput(bundle(out))
	}
	return ""
}

// Payloads of a result by MIME type: the data object, else the text and the image and HTML keys nbformat 3 keeps on the output
func bundle(out *node) *node {
	if data := out.get("data"); data != nil && data.kind == '{' {
		return data
	}
	b := &node{kind: '{', members: map[string]*node{}}
	add := func(mime string, v *node) {
		b.keys = append(b.keys, mime)
		b.members[mime] = v
	}
	if text := out.get("text"); text != nil && (text.kind == '"' || text.kind == '[') {
		add("text/plain", text)
	}
	for _, key := range [...][2]string{{"png", "image/png"}, {"jpeg", "image/jpeg"}, {"svg", "image/svg+xml"}, {"html", "text/html"}} {
		if v := out.get(key[0]); v != nil {
			add(key[1], v)
		}
	}
	return b
}

// A result as its plain or Markdown text, else a placeholder naming what was left out
func richOutput(data *node) string {
	if data.has("application/vnd.jupyter.widget-view+json") {
		return "[interactive widget — omitted]"
	}
	for _, mime := range []string{"text/plain", "text/markdown"} {
		if body := cleanStream(sourceText(data.get(mime))); strings.TrimSpace(body) != "" {
			return body
		}
	}
	for _, mime := range data.keys {
		if strings.HasPrefix(mime, "image/") {
			return fmt.Sprintf("[%s output — %s, omitted]", mime, humanSize(base64Size(sourceText(data.get(mime)))))
		}
	}
	if data.has("text/html") {
		return fmt.Sprintf("[text/html output — %s chars, omitted]", strs.Thousands(utf8.RuneCountInString(sourceText(data.get("text/html")))))
	}
	names := strings.Join(data.keys, ", ")
	if names == "" {
		names = "unknown"
	}
	return "[" + names + " output — omitted]"
}

// Text without ANSI escapes, each line keeping only the last frame a carriage return drew
func cleanStream(text string) string {
	lines := strings.Split(strings.ReplaceAll(term.StripANSI(text), "\r\n", "\n"), "\n")
	for i, line := range lines {
		last := ""
		for _, frame := range strings.Split(line, "\r") {
			if frame != "" {
				last = frame
			}
		}
		lines[i] = last
	}
	return strings.Join(lines, "\n")
}

// Size in bytes, kilobytes from 1024 on, rounded half to even
func humanSize(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%d KB", int(math.RoundToEven(float64(n)/1024)))
	}
	return fmt.Sprintf("%d B", n)
}

// Approximate decoded size of a base64 payload, characters outside its alphabet ignored
func base64Size(payload string) int {
	clean := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=' {
			return r
		}
		return -1
	}, payload)
	pad := len(clean) - len(strings.TrimRight(clean, "="))
	return max(0, len(clean)*3/4-min(2, pad))
}
