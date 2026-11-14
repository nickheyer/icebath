package docconv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxCellOutputChars = 20000

var reANSI = regexp.MustCompile(`(?s)\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\].*?(?:\x07|\x1b\\)|[PX^_].*?\x1b\\|[\x20-\x2f]+[\x30-\x7e]|[\x30-\x7e])` +
	`|\x{9b}[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\x{9d}.*?(?:\x07|\x{9c})|[\x{80}-\x{9f}]`)

var cellLabels = map[string]string{"markdown": "Markdown", "code": "Code", "raw": "Raw"}

var v3MimeKeys = [][2]string{{"png", "image/png"}, {"jpeg", "image/jpeg"}, {"svg", "image/svg+xml"}, {"html", "text/html"}}

type jsonObject struct {
	keys []string
	vals map[string]any
}

func newObject() *jsonObject {
	return &jsonObject{vals: map[string]any{}}
}

func (o *jsonObject) get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

func (o *jsonObject) getOr(key string, def any) any {
	if v, ok := o.vals[key]; ok {
		return v
	}
	return def
}

func (o *jsonObject) set(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

func allowNonFinite(data []byte) []byte {
	var out []byte
	last := 0
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		for _, word := range []string{"-Infinity", "Infinity", "NaN"} {
			if bytes.HasPrefix(data[i:], []byte(word)) {
				out = append(out, data[last:i]...)
				out = append(out, "null"...)
				i += len(word) - 1
				last = i + 1
				break
			}
		}
	}
	if out == nil {
		return data
	}
	return append(out, data[last:]...)
}

func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("extra data after the document")
		}
		return nil, err
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		obj := newObject()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, _ := kt.(string)
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			obj.set(key, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return obj, nil
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected %v", delim)
}

func elements(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case string:
		out := make([]any, 0, utf8.RuneCountInString(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out
	case *jsonObject:
		out := make([]any, len(t.keys))
		for i, k := range t.keys {
			out[i] = k
		}
		return out
	}
	return nil
}

func sourceText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			if s, ok := item.(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

func valueText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		if _, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return t.String()
		}
		if f, err := t.Float64(); err == nil {
			return formatReal(f)
		}
		return t.String()
	}
	data, _ := json.Marshal(plain(v))
	return string(data)
}

func plain(v any) any {
	switch t := v.(type) {
	case *jsonObject:
		m := make(map[string]any, len(t.vals))
		for k, val := range t.vals {
			m[k] = plain(val)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = plain(val)
		}
		return out
	}
	return v
}

func stripANSI(text string) string {
	if !strings.ContainsFunc(text, func(r rune) bool { return r == 0x1b || (r >= 0x80 && r <= 0x9f) }) {
		return text
	}
	return reANSI.ReplaceAllLiteralString(text, "")
}

func cleanStreamText(text string) string {
	lines := strings.Split(strings.ReplaceAll(stripANSI(text), "\r\n", "\n"), "\n")
	for i, line := range lines {
		frame := ""
		for _, f := range strings.Split(line, "\r") {
			if f != "" {
				frame = f
			}
		}
		lines[i] = frame
	}
	return strings.Join(lines, "\n")
}

func humanSize(n int) string {
	if n >= 1024 {
		return strconv.Itoa(int(math.RoundToEven(float64(n)/1024))) + " KB"
	}
	return strconv.Itoa(n) + " B"
}

func base64Bytes(payload string) int {
	var clean []byte
	for i := 0; i < len(payload); i++ {
		c := payload[i]
		if (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '+' || c == '/' || c == '=' {
			clean = append(clean, c)
		}
	}
	padding := len(clean) - len(bytes.TrimRight(clean, "="))
	return max(0, len(clean)*3/4-min(2, padding))
}

func outputText(v any) string {
	out, ok := v.(*jsonObject)
	if !ok {
		return ""
	}
	otype, _ := out.getOr("output_type", nil).(string)
	switch otype {
	case "stream":
		body := cleanStreamText(sourceText(out.getOr("text", "")))
		if strip(body) == "" {
			return ""
		}
		return body
	case "error", "pyerr":
		var lines []string
		if tb, ok := out.getOr("traceback", nil).([]any); ok {
			for _, l := range tb {
				if s, ok := l.(string); ok {
					lines = append(lines, s)
				}
			}
		}
		tbText := cleanStreamText(strings.Join(lines, "\n"))
		header := strings.TrimRight("Error: "+valueText(out.getOr("ename", ""))+": "+valueText(out.getOr("evalue", "")), ": ")
		return rstrip(header + "\n" + tbText)
	case "execute_result", "display_data", "pyout":
	default:
		return ""
	}
	data, ok := out.getOr("data", nil).(*jsonObject)
	if !ok {
		data = newObject()
		switch t := out.getOr("text", nil).(type) {
		case string, []any:
			data.set("text/plain", t)
		}
		for _, kv := range v3MimeKeys {
			if val, has := out.get(kv[0]); has {
				data.set(kv[1], val)
			}
		}
	}
	if _, has := data.get("application/vnd.jupyter.widget-view+json"); has {
		return "[interactive widget — omitted]"
	}
	for _, mime := range []string{"text/plain", "text/markdown"} {
		body := ""
		if val, has := data.get(mime); has {
			body = cleanStreamText(sourceText(val))
		}
		if strip(body) != "" {
			return body
		}
	}
	for _, mime := range data.keys {
		if strings.HasPrefix(mime, "image/") {
			return "[" + mime + " output — " + humanSize(base64Bytes(sourceText(data.vals[mime]))) + ", omitted]"
		}
	}
	if val, has := data.get("text/html"); has {
		return "[text/html output — " + commas(int64(utf8.RuneCountInString(sourceText(val)))) + " chars, omitted]"
	}
	names := strings.Join(data.keys, ", ")
	if names == "" {
		names = "unknown"
	}
	return "[" + names + " output — omitted]"
}

func notebookOutputs(cell *jsonObject, pointer, filename string) string {
	outputs, ok := cell.getOr("outputs", nil).([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, o := range outputs {
		if t := outputText(o); t != "" {
			parts = append(parts, t)
		}
	}
	text := strings.Join(parts, "\n")
	n := utf8.RuneCountInString(text)
	if n <= maxCellOutputChars {
		return text
	}
	hint := ""
	if pointer != "" && filename != "" {
		hint = " — full output: jq -r '" + pointer + "' " + shellQuote(filename)
	}
	return text[:runeOffset(text, maxCellOutputChars)] + "\n… [" + commas(int64(n-maxCellOutputChars)) + " output chars truncated" + hint + "]"
}

func extractNotebook(data []byte, displayPath string) (string, error) {
	doc, err := decodeJSON(allowNonFinite(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	if err != nil {
		return "", &ExtractError{Msg: "Not a valid notebook: " + err.Error(), Err: err}
	}
	nb, ok := doc.(*jsonObject)
	if !ok {
		return "", extractErr("Notebook root is not an object")
	}
	type cellRef struct {
		pointer string
		cell    any
	}
	var cells []cellRef
	if raw, ok := nb.getOr("cells", nil).([]any); ok {
		for i, c := range raw {
			cells = append(cells, cellRef{fmt.Sprintf(".cells[%d].outputs", i), c})
		}
	} else {
		for wi, ws := range elements(nb.getOr("worksheets", []any{})) {
			sheet, ok := ws.(*jsonObject)
			if !ok {
				continue
			}
			for ci, c := range elements(sheet.getOr("cells", []any{})) {
				cells = append(cells, cellRef{fmt.Sprintf(".worksheets[%d].cells[%d].outputs", wi, ci), c})
			}
		}
	}
	if len(cells) == 0 {
		return "", extractErr("Notebook contains no cells")
	}
	counts := map[string]int{}
	var out []string
	for _, ref := range cells {
		cell, ok := ref.cell.(*jsonObject)
		if !ok {
			continue
		}
		typ, _ := cell.getOr("cell_type", nil).(string)
		label, known := cellLabels[typ]
		if !known {
			continue
		}
		counts[typ]++
		suffix := ""
		if typ != "raw" {
			suffix = " " + strconv.Itoa(counts[typ])
		}
		source := strings.TrimRight(sourceText(cell.getOr("source", "")), "\n")
		out = append(out, "# ── "+label+" cell"+suffix+" ──", source, "")
		if typ == "code" {
			if rendered := notebookOutputs(cell, ref.pointer, displayPath); rendered != "" {
				out = append(out, "# ── Output (cell "+strconv.Itoa(counts[typ])+") ──", strings.TrimRight(rendered, "\n"), "")
			}
		}
	}
	return joined(out, "Notebook contains no readable cells")
}
