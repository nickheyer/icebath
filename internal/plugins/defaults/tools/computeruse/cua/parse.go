package cua

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dlclark/regexp2/v2"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// Whole number of a JSON value or a text of digits, never a boolean
func whole(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) {
			return int(n), true
		}
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return int(i), err == nil
	}
	return 0, false
}

func positive(v any) (int, bool) {
	n, ok := whole(v)
	return n, ok && n > 0
}

// Text of a value as Python prints it
func show(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	text, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(text)
}

// Prefixes of the titles of desktop and shell helper windows, which capture as nothing
var shellTitles = []string{"@!", "Desktop", "gnome-shell", "GNOME Shell", "xfce4-panel"}

func realWindow(w map[string]any) bool {
	title, ok := w["title"].(string)
	if !ok {
		return w["title"] != nil
	}
	for _, p := range shellTitles {
		if strings.HasPrefix(strings.ToLower(title), strings.ToLower(p)) {
			return false
		}
	}
	return true
}

// Whether every window has the same z_index, so it tells nothing of which is in front
func zUninformative(windows []map[string]any) bool {
	seen := map[string]bool{}
	for _, w := range windows {
		seen[show(cmp.Or(w["z_index"], any(0.0)))] = true
	}
	return len(seen) <= 1
}

func zValue(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case bool:
		if n {
			return 1
		}
	}
	return 0
}

var (
	xpropWindow = regexp.MustCompile(`window id # (0x[0-9a-fA-F]+)`)
	xpropHex    = regexp.MustCompile(`(0x[0-9a-fA-F]+)`)
)

// Window id xprop gives for _NET_ACTIVE_WINDOW
func xpropActive(stdout string) (int64, bool) {
	m := xpropWindow.FindStringSubmatch(stdout)
	if m == nil {
		m = xpropHex.FindStringSubmatch(stdout)
	}
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1][2:], 16, 64)
	return n, err == nil
}

// Lines of get_window_state's markdown tree that name an element: its index, role and label
var elementLine = regexp2.MustCompile(`^\s*(?:-\s+)?\[([0-9]+)\]\s+(\w+)(?:\s*=\s*"([^"]*)"|\s+"([^"]*)"|\s+\((?!\d+\))([^)]*)\))?`+
	`(?:\s+(?:\(\d+\)\s+)?id=([^\s\[\]]+))?`, regexp2.Multiline|regexp2.Unicode)

// Elements of the markdown tree, without bounds, for a driver that gives no structured elements
func treeElements(markdown string) []Element {
	out := []Element{}
	m, _ := elementLine.FindStringMatch(markdown)
	for m != nil {
		g := m.Groups()
		index, _ := strconv.Atoi(g[1].String())
		label := ""
		for _, i := range []int{3, 4, 5, 6} {
			if s := g[i].String(); s != "" {
				label = s
				break
			}
		}
		out = append(out, Element{Index: index, Role: g[2].String(), Label: label})
		m, _ = elementLine.FindNextMatch(m)
	}
	return out
}

// Elements of the structured list with their frames, malformed entries left out
func structuredElements(raw []any) []Element {
	out := []Element{}
	for _, r := range raw {
		obj, ok := r.(map[string]any)
		if !ok {
			continue
		}
		var index int
		switch v := obj["element_index"].(type) {
		case bool:
			if v {
				index = 1
			}
		default:
			n, ok := whole(v)
			if !ok {
				continue
			}
			index = n
		}
		e := Element{Index: index}
		e.Role, _ = obj["role"].(string)
		e.Label, _ = obj["label"].(string)
		e.Token, _ = obj["element_token"].(string)
		if frame, ok := obj["frame"].(map[string]any); ok && len(frame) > 0 {
			var b [4]int
			valid := true
			for i, k := range []string{"x", "y", "w", "h"} {
				v, has := frame[k]
				if !has {
					continue
				}
				n, ok := whole(v)
				if !ok {
					valid = false
					break
				}
				b[i] = n
			}
			if valid {
				e.Bounds = b
			}
		}
		out = append(out, e)
	}
	return out
}

// Pixel size of image bytes, zero when unreadable
func dimensions(raw []byte) (int, int) {
	c, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || c.Width <= 0 || c.Height <= 0 {
		return 0, 0
	}
	return c.Width, c.Height
}

// Byte length and size of a base64 screenshot, the size its header gives winning
func metrics(b64 string, width, height int) (int, int, int) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return len(b64) * 3 / 4, width, height
	}
	if w, h := dimensions(raw); w != 0 && h != 0 {
		return len(raw), w, h
	}
	return len(raw), width, height
}

// Key and modifiers of a combo such as cmd+s or ctrl-alt-t, the last key that is no modifier winning
func keyCombo(keys string) (string, []string) {
	modifiers := []string{}
	key := ""
	for _, part := range regexp.MustCompile(`[+\-]`).Split(keys, -1) {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		normalized := cmp.Or(map[string]string{"command": "cmd", "alt": "option", "control": "ctrl"}[part], part)
		if slices.Contains([]string{"cmd", "shift", "option", "ctrl", "fn"}, normalized) {
			modifiers = append(modifiers, normalized)
		} else {
			key = part
		}
	}
	return key, modifiers
}

// Screenshot and its type of an envelope: its image part, else the structured fields
func imageOf(e *envelope) (string, string) {
	if len(e.Images) > 0 && e.Images[0] != "" {
		mime := ""
		if len(e.Mimes) > 0 {
			mime = e.Mimes[0]
		}
		return e.Images[0], mime
	}
	image, _ := e.Structured["screenshot_png_b64"].(string)
	if image == "" {
		image, _ = e.Structured["png_b64"].(string)
	}
	if image == "" {
		return "", ""
	}
	mime, _ := e.Structured["screenshot_mime_type"].(string)
	if mime == "" {
		mime, _ = e.Structured["mime_type"].(string)
	}
	return image, mime
}

// list_windows rows with whole positive pid and window id, the others left out
func ingest(raw []any) []map[string]any {
	out := []map[string]any{}
	for _, r := range raw {
		w, ok := r.(map[string]any)
		if !ok {
			continue
		}
		pid, okPID := positive(w["pid"])
		window, okWindow := positive(w["window_id"])
		if !okPID || !okWindow {
			continue
		}
		app, _ := w["app_name"].(string)
		title, _ := w["title"].(string)
		z := any(0.0)
		switch w["z_index"].(type) {
		case float64:
			z = w["z_index"]
		}
		out = append(out, map[string]any{"app_name": app, "pid": pid, "window_id": window, "off_screen": w["is_on_screen"] == false, "title": title,
			"z_index": z})
	}
	return out
}

// list_windows payload of the structured, text and top level shapes
func windowsOf(e *envelope) []any {
	data, _ := e.Data.(map[string]any)
	for _, c := range []struct {
		container map[string]any
		keys      []string
	}{{e.Structured, []string{"windows"}}, {data, []string{"windows", "_legacy_windows"}}} {
		for _, k := range c.keys {
			if list, ok := c.container[k].([]any); ok && len(list) > 0 {
				return list
			}
		}
	}
	return []any{}
}

// Named apps with their pid in window order, each once
func appsOf(windows []any) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, w := range ingest(windows) {
		name, pid := w["app_name"].(string), w["pid"].(int)
		key := name + "\x00" + strconv.Itoa(pid)
		if name != "" && !seen[key] {
			seen[key] = true
			out = append(out, map[string]any{"name": name, "pid": pid})
		}
	}
	return out
}
