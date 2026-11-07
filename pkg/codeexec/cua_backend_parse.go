package codeexec

import (
	"errors"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

var (
	nonAppWindowTitlePrefixes = []string{"@!", "Desktop", "gnome-shell", "GNOME Shell"}
	elementLineRE             = regexp2.MustCompile(`^\s*(?:-\s+)?\[(\d+)\]\s+(\w+)`+
		`(?:\s*=\s*"([^"]*)"|\s+"([^"]*)"|\s+\((?!\d+\))([^)]*)\))?`+
		`(?:\s+(?:\(\d+\)\s+)?id=([^\s\[\]]+))?`, regexp2.Multiline|regexp2.Unicode)
	xpropWindowIDRE = regexp.MustCompile(`window id # (0x[0-9a-fA-F]+)`)
	xpropHexRE      = regexp.MustCompile(`(0x[0-9a-fA-F]+)`)
	keyComboSplitRE = regexp.MustCompile(`[+\-]`)
	modifierNames   = map[string]bool{"cmd": true, "command": true, "shift": true, "option": true, "alt": true, "ctrl": true, "control": true, "fn": true}
	keyAliases      = map[string]string{"command": "cmd", "alt": "option", "control": "ctrl"}
)

// Value of key in an ordered object, nil when the object is nil or lacks the key
func cuGet(obj *pyjson.Object, key string) any {
	if obj == nil {
		return nil
	}
	v, _ := obj.Get(key)
	return v
}

// ActionResult lifting the structured verdict, structuredContent first and the flattened meta copy second
func actionResultFrom(name string, ok bool, message string, meta *pyjson.Object, structured any, requestedDelivery *string) *ActionResult {
	sc, _ := structured.(*pyjson.Object)
	if meta == nil {
		meta = pyjson.NewObject()
	}
	raw := func(key string) any {
		if sc != nil {
			if v, has := sc.Get(key); has {
				return v
			}
		}
		return cuGet(meta, key)
	}
	typedBool := func(v any) *bool {
		if b, ok := v.(bool); ok {
			return &b
		}
		return nil
	}
	typedStr := func(v any) *string {
		if s, ok := v.(string); ok {
			return &s
		}
		return nil
	}
	code := raw("code")
	if !pyrepr.Truthy(code) {
		code = raw("reason_code")
	}
	esc, _ := raw("escalation").(*pyjson.Object)
	return &ActionResult{
		OK: ok, Action: name, Message: message, Meta: meta,
		Verified: typedBool(raw("verified")), Effect: typedStr(raw("effect")), Escalation: esc,
		Path: typedStr(raw("path")), Degraded: typedBool(raw("degraded")), DeliveryMode: requestedDelivery,
		Code: typedStr(code),
	}
}

// Hash key of a z_index value under Python equality, ints floats and bools of equal value collide
func zIndexKey(v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "n1"
		}
		return "n0"
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			f := new(big.Float).SetFloat64(t)
			n, _ := f.Int(nil)
			return "n" + n.String()
		}
		return "f" + strconv.FormatFloat(t, 'g', -1, 64)
	}
	if n, ok := pyrepr.Int(v); ok {
		return "n" + n.String()
	}
	return "r" + pyrepr.Repr(v)
}

// True when every window shares one z_index
func zIndexUninformative(windows []*pyjson.Object) bool {
	seen := map[string]bool{}
	for _, w := range windows {
		v, has := w.Get("z_index")
		if !has {
			v = int64(0)
		}
		seen[zIndexKey(v)] = true
	}
	return len(seen) <= 1
}

// Window id of xprop _NET_ACTIVE_WINDOW output, the first hex token as fallback
func parseXpropNetActiveWindow(stdout string) (int64, bool) {
	m := xpropWindowIDRE.FindStringSubmatch(stdout)
	if m == nil {
		m = xpropHexRE.FindStringSubmatch(stdout)
	}
	if m == nil {
		return 0, false
	}
	n, ok := new(big.Int).SetString(m[1][2:], 16)
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}

// False for desktop and shell helper windows that capture as empty
func isRealAppWindow(w *pyjson.Object) bool {
	raw, has := w.Get("title")
	if !has {
		raw = ""
	}
	title, ok := raw.(string)
	if !ok {
		return true
	}
	for _, p := range nonAppWindowTitlePrefixes {
		if strings.HasPrefix(title, p) || strings.HasPrefix(pystr.Lower(title), pystr.Lower(p)) {
			return false
		}
	}
	return true
}

// Elements of get_window_state tree markdown with zero bounds, the fallback for drivers without structured elements
func parseElementsFromTree(markdown string) []UIElement {
	var out []UIElement
	m, _ := elementLineRE.FindStringMatch(markdown)
	for m != nil {
		g := m.Groups()
		idx, _ := pyint.Parse(g[1].String())
		label := ""
		for _, i := range []int{3, 4, 5, 6} {
			if s := g[i].String(); s != "" {
				label = s
				break
			}
		}
		out = append(out, UIElement{Index: int(idx.Int64()), Role: g[2].String(), Label: label})
		m, _ = elementLineRE.FindNextMatch(m)
	}
	if out == nil {
		out = []UIElement{}
	}
	return out
}

// Elements of the structured elements array with frame bounds, malformed entries skipped
func parseElementsFromStructured(rawElements []any) ([]UIElement, error) {
	elements := []UIElement{}
	for _, r := range rawElements {
		obj, ok := r.(*pyjson.Object)
		if !ok {
			continue
		}
		idxRaw := cuGet(obj, "element_index")
		var idx int
		switch v := idxRaw.(type) {
		case bool:
			if v {
				idx = 1
			}
		default:
			n, ok := pyrepr.Int(idxRaw)
			if !ok || idxRaw == nil {
				continue
			}
			idx = int(n.Int64())
		}
		el := UIElement{Index: idx}
		if s, ok := cuGet(obj, "role").(string); ok {
			el.Role = s
		}
		if s, ok := cuGet(obj, "label").(string); ok {
			el.Label = s
		}
		if s, ok := cuGet(obj, "element_token").(string); ok && s != "" {
			el.ElementToken = &s
		}
		if frame, ok := cuGet(obj, "frame").(*pyjson.Object); ok && frame.Len() > 0 {
			var b [4]int
			valid := true
			for i, k := range []string{"x", "y", "w", "h"} {
				v, has := frame.Get(k)
				if !has {
					v = int64(0)
				}
				n, err := pyint.Of(v)
				if err != nil {
					var pe *pyint.Error
					if errors.As(err, &pe) && (pe.Kind == "TypeError" || pe.Kind == "ValueError") {
						valid = false
						break
					}
					return nil, err
				}
				b[i] = int(n.Int64())
			}
			if valid {
				el.Bounds = b
			}
		}
		elements = append(elements, el)
	}
	return elements, nil
}

// PNG or JPEG dimensions, zero by zero when unreadable or not positive
func imageDimensionsFromBytes(raw []byte) (int, int) {
	w, h, ok := provider.ImageDimensionsFromBytes(raw)
	if ok && w > 0 && h > 0 {
		return w, h
	}
	return 0, 0
}

// Summary line and tree markdown of get_window_state text
func splitTreeText(fullText string) (string, string) {
	summary, tree, _ := strings.Cut(fullText, "\n")
	return summary, tree
}

// Key and modifiers of a combo such as cmd+s or ctrl-alt-t, the last non modifier wins
func parseKeyCombo(keys string) (*string, []string) {
	modifiers := []string{}
	var key *string
	for _, p := range keyComboSplitRE.Split(keys, -1) {
		if pystr.Strip(p) == "" {
			continue
		}
		part := pystr.Lower(pystr.Strip(p))
		normalized := part
		if alias, ok := keyAliases[part]; ok {
			normalized = alias
		}
		if modifierNames[normalized] {
			modifiers = append(modifiers, normalized)
		} else {
			k := part
			key = &k
		}
	}
	return key, modifiers
}

// Normalised tool result every transport emits, image_mime_types only when the transport reports it
func toolEnvelope(data any, images []any, structured any, isError bool, imageMimeTypes []any) *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("data", data)
	out.Set("images", images)
	if imageMimeTypes != nil {
		out.Set("image_mime_types", imageMimeTypes)
	}
	out.Set("structuredContent", structured)
	out.Set("isError", isError)
	return out
}

// Flattens an MCP CallToolResult object into the envelope with joined text parsed as JSON when it looks like JSON
func extractToolResult(result *pyjson.Object) *pyjson.Object {
	var data any
	images, mimes := []any{}, []any{}
	var chunks []string
	content, _ := cuGet(result, "content").([]any)
	for _, p := range content {
		part, ok := p.(*pyjson.Object)
		if !ok {
			continue
		}
		switch cuGet(part, "type") {
		case "text":
			s, _ := cuGet(part, "text").(string)
			chunks = append(chunks, s)
		case "image":
			if d := cuGet(part, "data"); pyrepr.Truthy(d) {
				images = append(images, d)
				mime := cuGet(part, "mimeType")
				if !pyrepr.Truthy(mime) {
					mime = ""
				}
				mimes = append(mimes, mime)
			}
		}
	}
	if len(chunks) > 0 {
		var kept []string
		for _, c := range chunks {
			if c != "" {
				kept = append(kept, c)
			}
		}
		joined := strings.Join(kept, "\n")
		data = joined
		if s := pystr.Strip(joined); strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
			if v, err := pyjson.Loads(joined); err == nil {
				data = v
			}
		}
	}
	structured := cuGet(result, "structuredContent")
	if !pyrepr.Truthy(structured) {
		structured = nil
	}
	return toolEnvelope(data, images, structured, cuGet(result, "isError") == true, mimes)
}

// Screenshot base64 and mime type of a flattened result, the image part first and structured fields second
func imageFromToolResult(out *pyjson.Object) (*string, *string) {
	images, _ := cuGet(out, "images").([]any)
	if len(images) > 0 && pyrepr.Truthy(images[0]) {
		b64 := pyrepr.Str(images[0])
		mimes, _ := cuGet(out, "image_mime_types").([]any)
		if len(mimes) > 0 && pyrepr.Truthy(mimes[0]) {
			m := pyrepr.Str(mimes[0])
			return &b64, &m
		}
		return &b64, nil
	}
	structured, _ := cuGet(out, "structuredContent").(*pyjson.Object)
	b64 := cuGet(structured, "screenshot_png_b64")
	if !pyrepr.Truthy(b64) {
		b64 = cuGet(structured, "png_b64")
	}
	if !pyrepr.Truthy(b64) {
		return nil, nil
	}
	s := pyrepr.Str(b64)
	mime := cuGet(structured, "screenshot_mime_type")
	if !pyrepr.Truthy(mime) {
		mime = cuGet(structured, "mime_type")
	}
	if !pyrepr.Truthy(mime) {
		return &s, nil
	}
	m := pyrepr.Str(mime)
	return &s, &m
}

// Integer of an int or numeric string, false for bools, other types and malformed strings
func intOrNone(value any) (int, bool) {
	switch v := value.(type) {
	case bool, nil:
		return 0, false
	case string:
		n, ok := pyint.Parse(v)
		if !ok || !n.IsInt64() {
			return 0, false
		}
		return int(n.Int64()), true
	case float64, float32:
		return 0, false
	}
	n, ok := pyrepr.Int(value)
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return int(n.Int64()), true
}

// Positive integer of value, false for bools, malformed and non positive values
func positiveInt(value any) (int, bool) {
	n, ok := intOrNone(value)
	return n, ok && n > 0
}

// True for a zero or negative schema filler id, non numeric values still reach validation
func isSchemaFillerID(value any) bool {
	n, ok := intOrNone(value)
	return ok && n <= 0
}

// Normalised list_windows entries with integer pid and window id, unusable entries dropped
func ingestWindows(rawWindows []any) []*pyjson.Object {
	windows := []*pyjson.Object{}
	for _, r := range rawWindows {
		w, ok := r.(*pyjson.Object)
		if !ok {
			continue
		}
		pid, okPID := positiveInt(cuGet(w, "pid"))
		wid, okWID := positiveInt(cuGet(w, "window_id"))
		if !okPID || !okWID {
			continue
		}
		zRaw := cuGet(w, "z_index")
		appName, has := w.Get("app_name")
		if !has {
			appName = ""
		}
		title, has := w.Get("title")
		if !has {
			title = ""
		}
		out := pyjson.NewObject()
		if s, ok := appName.(string); ok {
			out.Set("app_name", s)
		} else {
			out.Set("app_name", "")
		}
		out.Set("pid", pid)
		out.Set("window_id", wid)
		out.Set("off_screen", cuGet(w, "is_on_screen") == false)
		if s, ok := title.(string); ok {
			out.Set("title", s)
		} else {
			out.Set("title", "")
		}
		var z any = 0
		switch zRaw.(type) {
		case bool, nil:
		case float64:
			z = zRaw
		default:
			if _, ok := pyrepr.Int(zRaw); ok {
				z = zRaw
			}
		}
		out.Set("z_index", z)
		windows = append(windows, out)
	}
	return windows
}

// list_windows payload across the structured, text payload and envelope shapes
func windowsFromToolResult(out *pyjson.Object) []any {
	candidates := []struct {
		container any
		keys      []string
	}{
		{cuGet(out, "structuredContent"), []string{"windows"}},
		{cuGet(out, "data"), []string{"windows", "_legacy_windows"}},
		{out, []string{"windows", "_legacy_windows"}},
	}
	for _, c := range candidates {
		obj, ok := c.container.(*pyjson.Object)
		if !ok {
			continue
		}
		for _, k := range c.keys {
			if list, ok := cuGet(obj, k).([]any); ok && len(list) > 0 {
				return list
			}
		}
	}
	return []any{}
}

// Distinct named apps with their pid in window order
func appsFromWindows(windows []any) []*pyjson.Object {
	apps := []*pyjson.Object{}
	seen := map[string]bool{}
	for _, s := range ingestWindows(windows) {
		name := cuGet(s, "app_name").(string)
		pid := cuGet(s, "pid").(int)
		key := name + "\x00" + strconv.Itoa(pid)
		if name != "" && !seen[key] {
			seen[key] = true
			app := pyjson.NewObject()
			app.Set("name", name)
			app.Set("pid", pid)
			apps = append(apps, app)
		}
	}
	return apps
}
