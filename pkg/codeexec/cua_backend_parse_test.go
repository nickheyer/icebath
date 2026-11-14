package codeexec

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Decodes JSON text the way json.loads does, failing the test on bad input
func cuLoads(t *testing.T, text string) any {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseElementsFromTreeFormats(t *testing.T) {
	labels := func(tree string) map[int]string {
		out := map[int]string{}
		for _, e := range parseElementsFromTree(tree) {
			out[e.Index] = e.Label
		}
		return out
	}
	classic := parseElementsFromTree("  - [14] AXButton \"One\"\n  - [15] AXButton \"Two\"\n  - [16] AXTextField \"\"\n")
	if len(classic) != 3 || classic[0].Index != 14 || classic[0].Role != "AXButton" || classic[0].Label != "One" || classic[1].Label != "Two" || classic[2].Label != "" {
		t.Fatalf("classic %+v", classic)
	}
	idEq := parseElementsFromTree("[14] AXButton (1) id=One\n[15] AXButton (2) id=Two\n[16] AXTextField (3) id=\n")
	if len(idEq) != 3 || idEq[0].Index != 14 || idEq[0].Label != "One" || idEq[1].Label != "Two" || idEq[2].Label != "" {
		t.Fatalf("id form %+v", idEq)
	}
	got := labels("- [77] AXButton (Auto) [help=\"...\" actions=[press]]\n" +
		"- [78] AXButton (Light) [help=\"...\" actions=[press]]\n" +
		"- [79] AXButton (Dark) [help=\"Use a dark appearance...\" actions=[press]]\n" +
		"          - [4] AXStaticText = \"Wi‑Fi\" [id=com.apple.wifi actions=[showmenu]]\n" +
		"- [92] AXPopUpButton = \"Automatic\" [id=HighlightColorPicker actions=[press]]\n" +
		"- [100] AXRadioButton (Always) [actions=[press]]\n" +
		"[200] AXButton (5) id=RealLabel\n" +
		"[201] AXButton (7)\n")
	want := map[int]string{77: "Auto", 78: "Light", 79: "Dark", 4: "Wi‑Fi", 92: "Automatic", 100: "Always", 200: "RealLabel", 201: ""}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("label %d got %q want %q", k, got[k], v)
		}
	}
}

func TestParseElementsFromStructured(t *testing.T) {
	raw := cuLoads(t, `[{"element_index": 3, "role": "AXButton", "label": "OK", "frame": {"x": 10, "y": 20.7, "w": "30", "h": 40}, "element_token": "tok"},
	 {"element_index": "4"}, {"element_index": 5, "frame": {"x": null}, "role": 7, "element_token": ""}, "junk"]`).([]any)
	els, err := parseElementsFromStructured(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(els) != 2 || els[0].Bounds != [4]int{10, 20, 30, 40} || *els[0].ElementToken != "tok" || els[0].Label != "OK" {
		t.Fatalf("elements %+v", els)
	}
	if els[1].Index != 5 || els[1].Bounds != [4]int{} || els[1].Role != "" || els[1].ElementToken != nil {
		t.Fatalf("malformed frame entry %+v", els[1])
	}
}

func TestSchemaFillerIDsAndInts(t *testing.T) {
	for _, v := range []any{int64(0), "0", int64(-1)} {
		if !isSchemaFillerID(v) {
			t.Fatalf("%v must be a schema filler", v)
		}
	}
	for _, v := range []any{int64(200), "200", "abc", nil, 1.5, false, true} {
		if isSchemaFillerID(v) {
			t.Fatalf("%v must not be a schema filler", v)
		}
	}
	if _, ok := positiveInt(true); ok {
		t.Fatal("bools are not ints")
	}
}

func TestIngestWindowsAndApps(t *testing.T) {
	raw := cuLoads(t, `[{"app_name": "Desktop", "pid": null, "window_id": 1, "z_index": 0},
	 {"app_name": "Firefox", "pid": 4321, "window_id": 77, "z_index": 1},
	 {"app_name": "Firefox", "pid": 1, "window_id": 2, "is_on_screen": false, "title": "Mozilla Firefox", "z_index": 3},
	 {"app_name": "Firefox", "pid": 4321, "window_id": 78, "z_index": true}]`).([]any)
	out := ingestWindows(raw)
	if len(out) != 3 || cuGet(out[0], "app_name") != "Firefox" || cuGet(out[0], "pid") != 4321 || cuGet(out[0], "window_id") != 77 {
		t.Fatalf("ingested %v", out)
	}
	if cuGet(out[1], "off_screen") != true || cuGet(out[1], "title") != "Mozilla Firefox" || cuGet(out[2], "z_index") != 0 {
		t.Fatalf("fields %v %v", out[1], out[2])
	}
	if b, _ := json.Marshal(out[0]); string(b) != `{"app_name":"Firefox","pid":4321,"window_id":77,"off_screen":false,"title":"","z_index":1}` {
		t.Fatalf("key order %s", b)
	}
	if b, _ := json.Marshal(appsFromWindows(raw)); string(b) != `[{"name":"Firefox","pid":4321},{"name":"Firefox","pid":1}]` {
		t.Fatalf("apps %s", b)
	}
	if zIndexUninformative(out) {
		t.Fatal("distinct z indexes are informative")
	}
	tied := ingestWindows(cuLoads(t, `[{"pid": 1, "window_id": 1, "z_index": 1}, {"pid": 2, "window_id": 2, "z_index": 1.0}]`).([]any))
	if !zIndexUninformative(tied) {
		t.Fatal("1 and 1.0 hash equal")
	}
}

func TestXpropKeyComboAndTreeSplit(t *testing.T) {
	if id, ok := parseXpropNetActiveWindow("_NET_ACTIVE_WINDOW(WINDOW): window id # 0x503000b\n"); !ok || id != 0x503000b {
		t.Fatalf("xprop %x %v", id, ok)
	}
	if id, ok := parseXpropNetActiveWindow("garbage 0x1f"); !ok || id != 0x1f {
		t.Fatal("first hex token fallback")
	}
	if _, ok := parseXpropNetActiveWindow(""); ok {
		t.Fatal("no id in empty output")
	}
	key, mods := parseKeyCombo("Ctrl-Alt-Delete")
	if *key != "delete" || !slices.Equal(mods, []string{"ctrl", "option"}) {
		t.Fatalf("combo %v %v", *key, mods)
	}
	key, mods = parseKeyCombo("command+shift+")
	if key != nil || !slices.Equal(mods, []string{"cmd", "shift"}) {
		t.Fatalf("modifier only combo %v %v", key, mods)
	}
	if s, tree := splitTreeText("Window: Finder\n- [1] AXButton"); s != "Window: Finder" || tree != "- [1] AXButton" {
		t.Fatal("split")
	}
}

func TestExtractToolResultAndActionVerdict(t *testing.T) {
	res := cuLoads(t, `{"content": [{"type": "text", "text": "{\"windows\": [{\"pid\": 9, \"window_id\": 4}]}"}, {"type": "text", "text": ""},
	 {"type": "image", "data": "iVBOR", "mimeType": "image/png"}, {"type": "image", "data": "/9j/"}], "structuredContent": {}, "isError": true}`).(*pyjson.Object)
	env := extractToolResult(res)
	if b, _ := json.Marshal(env); string(b) != `{"data":{"windows":[{"pid":9,"window_id":4}]},"images":["iVBOR","/9j/"],"image_mime_types":["image/png",""],"structuredContent":null,"isError":true}` {
		t.Fatalf("envelope %s", b)
	}
	if b64, mime := imageFromToolResult(env); *b64 != "iVBOR" || *mime != "image/png" {
		t.Fatal("first image part")
	}
	if len(windowsFromToolResult(env)) != 1 {
		t.Fatal("windows from text payload")
	}
	plain := extractToolResult(cuLoads(t, `{"content": [{"type": "text", "text": "[not json"}], "structuredContent": {"screenshot_png_b64": "AAA", "mime_type": "image/jpeg"}}`).(*pyjson.Object))
	if cuGet(plain, "data") != "[not json" || cuGet(plain, "isError") != false {
		t.Fatalf("plain %v", plain)
	}
	if b64, mime := imageFromToolResult(plain); *b64 != "AAA" || *mime != "image/jpeg" {
		t.Fatal("structured screenshot")
	}
	meta := cuLoads(t, `{"verified": true, "effect": "confirmed", "reason_code": "background_unavailable", "path": 5}`).(*pyjson.Object)
	sc := cuLoads(t, `{"effect": "suspected_noop", "escalation": {"next": "pixel"}, "degraded": false}`)
	delivery := "foreground"
	r := actionResultFrom("click", true, "ok", meta, sc, &delivery)
	if !*r.Verified || *r.Effect != "suspected_noop" || r.Escalation == nil || r.Path != nil || *r.Degraded || *r.Code != "background_unavailable" || *r.DeliveryMode != "foreground" {
		t.Fatalf("verdict %+v", r)
	}
	bare := actionResultFrom("key", false, "", nil, "not a dict", nil)
	if bare.Verified != nil || bare.Effect != nil || bare.Code != nil || bare.Meta == nil {
		t.Fatalf("bare verdict %+v", bare)
	}
}
