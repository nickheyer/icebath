package builtin

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
)

const svgNS = "http://www.w3.org/2000/svg"

// Root element, its attributes, the direct title text and every fill and stroke color of an SVG
type svgFacts struct {
	root   xml.Name
	attrs  map[string]string
	title  string
	colors map[string]bool
}

func parseSVG(t *testing.T, data []byte) svgFacts {
	t.Helper()
	facts := svgFacts{attrs: map[string]string{}, colors: map[string]bool{}}
	dec := xml.NewDecoder(bytes.NewReader(data))
	depth, inTitle := 0, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("svg parse: %v", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				facts.root = el.Name
				for _, a := range el.Attr {
					facts.attrs[a.Name.Local] = a.Value
				}
			}
			inTitle = depth == 2 && el.Name == xml.Name{Space: svgNS, Local: "title"} && facts.title == ""
			for _, a := range el.Attr {
				if a.Name.Space == "" && (a.Name.Local == "fill" || a.Name.Local == "stroke") && a.Value != "none" {
					facts.colors[strings.ToLower(a.Value)] = true
				}
			}
		case xml.CharData:
			if inTitle {
				facts.title += string(el)
			}
		case xml.EndElement:
			depth--
			inTitle = false
		}
	}
	return facts
}

func readJSONMap(t *testing.T, p string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(must(os.ReadFile(p)), &v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return v
}

func TestEGMOpenAISubmissionMetadataAndAssets(t *testing.T) {
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{tree.Payload(egmRoot), tree.Host("egm")} {
		root, _ = filepath.EvalSymlinks(root)
		canonical := readJSONMap(t, filepath.Join(root, "plugin.json"))
		extension, _ := canonical["extensions"].(map[string]any)["com.openai"].(map[string]any)
		codex := readJSONMap(t, filepath.Join(root, ".codex-plugin", "plugin.json"))
		if !reflect.DeepEqual(extension["interface"], codex["interface"]) || extension["requires_local_executor"] != true ||
			codex["requires_local_executor"] != true || !reflect.DeepEqual(codex["author"], canonical["author"]) {
			t.Fatalf("%s: com.openai extension and .codex-plugin/plugin.json disagree", root)
		}
		iface := extension["interface"].(map[string]any)
		for field, limit := range map[string]int{"displayName": 30, "shortDescription": 30, "longDescription": 4000, "developerName": 80} {
			if s, _ := iface[field].(string); utf8.RuneCountInString(s) == 0 || utf8.RuneCountInString(s) > limit {
				t.Errorf("%s: %s", root, field)
			}
		}
		caps, _ := iface["capabilities"].([]any)
		if len(caps) > 20 {
			t.Errorf("%s: %d capabilities", root, len(caps))
		}
		for _, c := range caps {
			if s, _ := c.(string); utf8.RuneCountInString(s) == 0 || utf8.RuneCountInString(s) > 120 {
				t.Errorf("%s: capability %q", root, s)
			}
		}
		for _, field := range []string{"composerIcon", "logo"} {
			rel, _ := iface[field].(string)
			asset, err := filepath.EvalSymlinks(filepath.Join(root, rel))
			info, statErr := os.Stat(asset)
			if !strings.HasPrefix(rel, "./assets/") || err != nil || !strings.HasPrefix(asset, filepath.Join(root, "assets")+string(filepath.Separator)) ||
				statErr != nil || !info.Mode().IsRegular() || info.Size() > 5*1024*1024 {
				t.Fatalf("%s: %s asset %q %v", root, field, rel, err)
			}
			svg := parseSVG(t, must(os.ReadFile(asset)))
			w, errW := strconv.ParseFloat(svg.attrs["width"], 64)
			h, errH := strconv.ParseFloat(svg.attrs["height"], 64)
			if errW != nil || errH != nil || w != h || h < 48 {
				t.Errorf("%s: %s is %s x %s", root, field, svg.attrs["width"], svg.attrs["height"])
			}
		}
	}
}

func TestEGMSkillOpenAIMetadataAndIconPalette(t *testing.T) {
	palette := map[string][2]string{"bootstrap": {"#1b6ef3", "#7cacf8"}, "brainstorm": {"#1b6ef3", "#7cacf8"}, "draft": {"#1b6ef3", "#7cacf8"},
		"resolve": {"#1b6ef3", "#7cacf8"}, "drift_report": {"#e86e00", "#ff8d41"}, "review": {"#e86e00", "#ff8d41"}, "review_loop": {"#e86e00", "#ff8d41"}}
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for skill, colors := range palette {
		dir := "egm-" + strings.ReplaceAll(skill, "_", "-")
		source := tree.Payload(egmRoot + "/skills/" + dir)
		for _, root := range []string{source, tree.Host("egm/skills/" + dir)} {
			parsed, err := config.FastSafeLoad(must(os.ReadFile(filepath.Join(root, "agents", "openai.yaml"))))
			meta, _ := parsed.(map[string]any)
			iface, _ := meta["interface"].(map[string]any)
			name, _ := iface["display_name"].(string)
			short, _ := iface["short_description"].(string)
			brand, _ := iface["brand_color"].(string)
			if err != nil || name == "" || utf8.RuneCountInString(short) < 25 || utf8.RuneCountInString(short) > 64 || strings.ToLower(brand) != colors[0] {
				t.Fatalf("%s: interface %v %v", root, iface, err)
			}
			for _, field := range []string{"icon_small", "icon_large"} {
				icon, _ := iface[field].(string)
				if filepath.IsAbs(icon) || strings.Contains("/"+icon+"/", "/../") {
					t.Fatalf("%s: %s %q escapes the skill", root, field, icon)
				}
				data, err := os.ReadFile(filepath.Join(root, icon))
				if err != nil || !bytes.Equal(data, must(os.ReadFile(filepath.Join(source, icon)))) {
					t.Fatalf("%s: %s %v", root, field, err)
				}
				svg := parseSVG(t, data)
				if svg.root != (xml.Name{Space: svgNS, Local: "svg"}) || svg.attrs["viewBox"] != "0 0 24 24" || svg.title == "" ||
					!reflect.DeepEqual(svg.colors, map[string]bool{colors[0]: true, colors[1]: true}) {
					t.Errorf("%s: %s root %v viewBox %q title %q colors %v", root, field, svg.root, svg.attrs["viewBox"], svg.title, svg.colors)
				}
			}
		}
	}
}
