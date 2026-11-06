package builtin

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/plugins"
)

func TestHallmarkManifestThemesAndReferences(t *testing.T) {
	m := hallmarkPlugin{}.Manifest()
	if m.Name != "hallmark" || m.Version != "2.0.0" || m.Author != "Nutlope" || m.License != "MIT" || len(m.Tags) != 7 || m.ProvidesHooks[0] != "transform_tool_result" {
		t.Fatalf("manifest %+v", m)
	}
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	themes, _ := filepath.Glob(tree.Payload(hallmarkSkill + "/references/themes/*.jpg"))
	if len(themes) != 50 {
		t.Fatalf("%d themes", len(themes))
	}
	for _, jpg := range themes {
		if !isFile(strings.TrimSuffix(jpg, ".jpg") + ".md") {
			t.Errorf("theme %s has no card", jpg)
		}
	}
	skill, _ := os.ReadFile(tree.Payload(hallmarkSkill + "/SKILL.md"))
	refs := regexp.MustCompile(`\]\((references/[^)#\s]+)`).FindAllStringSubmatch(string(skill), -1)
	for _, r := range refs {
		if _, err := os.Stat(tree.Payload(hallmarkSkill + "/" + r[1])); err != nil {
			t.Errorf("skill reference %s: %v", r[1], err)
		}
	}
	for _, s := range hallmarkScripts {
		if !isExecutable(tree.Payload(hallmarkSkill+"/scripts/"+s+".mjs")) && s != "sloplint-render" {
			t.Errorf("script %s not executable", s)
		}
	}
	if len(refs) == 0 {
		t.Fatal("no skill references found")
	}
	if _, err := HallmarkCommand(context.Background(), "missing"); err == nil {
		t.Fatal("unknown script accepted")
	}
}

func TestHallmarkThemeValidateAcceptsLFAndCRLF(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	source, err := payloadFile(hallmarkSkill + "/references/themes/hum.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	dir := t.TempDir()
	for name, body := range map[string]string{"lf": text, "crlf": strings.ReplaceAll(text, "\n", "\r\n")} {
		if len(body) >= 8000 {
			t.Fatal("fixture must stay below the independent size limit")
		}
		file := filepath.Join(dir, name+".md")
		os.WriteFile(file, []byte(body), 0o644)
		cmd, err := HallmarkCommand(context.Background(), "theme", "validate", file)
		if err != nil {
			t.Fatalf("managed node runtime: %v", err)
		}
		out, err := cmd.CombinedOutput()
		if err != nil || !regexp.MustCompile(`: ok \(Hum,`).Match(out) {
			t.Fatalf("%s: %v %s", name, err, out)
		}
	}
}

func TestBundledPluginsRegisterThroughManager(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  enabled:\n    - egm\n    - hallmark\n    - impeccable\n"), 0o644)
	m := plugins.NewManager(h)
	m.DiscoverAndLoad(context.Background(), true)
	for _, q := range []string{"egm:egm-review", "egm:egm-zoom-out", "egm:egm-bootstrap-orchestrator", "hallmark:hallmark", "impeccable:impeccable"} {
		p, ok := m.FindPluginSkill(q)
		if !ok || !isFile(p) {
			t.Errorf("plugin skill %s at %q", q, p)
		}
	}
	if got := len(m.ListPluginSkills("egm")); got != 16 {
		t.Errorf("egm registered %d skills, want 7 skills and 9 personas", got)
	}
	p, _ := m.FindPluginSkill("egm:egm-review")
	if !isFile(filepath.Join(filepath.Dir(p), "resources", "execution_instructions.md")) || !isFile(filepath.Join(filepath.Dir(p), "resources", "document_standards", "zoom-out-standard.md")) {
		t.Error("EGM resources do not resolve from the skill directory")
	}
	ip, _ := m.FindPluginSkill("impeccable:impeccable")
	if data, _ := os.ReadFile(ip); !strings.Contains(string(data), filepath.Join(filepath.Dir(ip), "scripts")+"/impeccable") {
		t.Error("rendered impeccable skill does not point at the materialized scripts")
	}
	if def := commands.Default.Lookup("impeccable"); def == nil || len(def.Subcommands) != 24 || def.Handler == nil {
		t.Fatalf("impeccable command %+v", def)
	}
	out := m.InvokeHook(context.Background(), "transform_tool_result", map[string]any{"tool_name": "read_file", "args": map[string]any{}, "result": "{}"})
	for _, v := range out {
		if v != nil {
			t.Fatalf("read_file transformed %v", out)
		}
	}
}
