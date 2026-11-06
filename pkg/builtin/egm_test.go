package builtin

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/skills"
)

func TestEGMPortableManifestSchema(t *testing.T) {
	m := egmPlugin{}.Manifest()
	nameRe := regexp2.MustCompile(`^(?!.*(?:--|\.\.))[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`, regexp2.None)
	if ok, _ := nameRe.MatchString(m.Name); !ok || !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(m.Version) {
		t.Fatalf("manifest %q %q", m.Name, m.Version)
	}
	if m.Name != "egm" || m.Author != "Google" || m.License != "Apache-2.0" || m.Homepage != "https://egm.dev" || m.Raw["$schema"] != nil {
		t.Fatalf("manifest %+v", m)
	}
}

func TestEGMSkillsHaveValidFrontmatterAndFitLoadLimit(t *testing.T) {
	want := "egm-bootstrap egm-brainstorm egm-draft egm-drift-report egm-resolve egm-review egm-review-loop"
	if got := strings.Join(egmSkillDirs(), " "); got != want {
		t.Fatalf("skills %s", got)
	}
	for _, dir := range egmSkillDirs() {
		data, _ := payloadFile(egmRoot + "/skills/" + dir + "/SKILL.md")
		fm, _ := skills.ParseFrontmatter(string(data))
		name, _ := fm["name"].(string)
		desc, _ := fm["description"].(string)
		if strings.TrimSpace(name) != dir || strings.TrimSpace(desc) == "" {
			t.Errorf("%s frontmatter %v", dir, fm)
		}
		if v, ok := fm["disable-model-invocation"]; ok {
			if _, isBool := v.(bool); !isBool {
				t.Errorf("%s disable-model-invocation %v", dir, v)
			}
		}
		if body := strings.TrimSpace(strings.SplitN(string(data), "---", 3)[2]); len(body) > 24000 {
			t.Errorf("%s: %d bytes exceeds Antigravity's 24,000-byte skill body limit", dir, len(body))
		}
	}
}

func TestEGMPersonasLoadByFrontmatterName(t *testing.T) {
	personas, err := Personas()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	reviewers := 0
	for _, p := range personas {
		names = append(names, p.Name)
		if p.Reviewer {
			reviewers++
		}
	}
	want := "egm-bootstrap-orchestrator egm-codebase-details egm-comprehension egm-design egm-implementation-readiness egm-structural egm-style egm-synthesizer egm-zoom-out"
	if strings.Join(names, " ") != want || reviewers != 7 {
		t.Fatalf("personas %v reviewers %d", names, reviewers)
	}
	p, err := LoadPersona("egm-zoom-out")
	if err != nil || p.File != "agents/zoom_out.md" || !p.Reviewer {
		t.Fatalf("zoom out %+v %v", p, err)
	}
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := p.DelegateContext(tree)
	if !strings.Contains(ctx, "Read "+tree.Payload(egmRoot+"/agents/zoom_out.md")+" before the target") || !strings.Contains(ctx, "Do not write reports or delegate.") {
		t.Fatalf("delegate context %s", ctx)
	}
	if _, err := LoadPersona("egm-missing"); err == nil {
		t.Fatal("missing persona loaded")
	}
}

func TestEGMPromptDependenciesResolveFromHostLayout(t *testing.T) {
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	refRe := regexp.MustCompile("`((?:agents|resources|references)/[^`\\s]+\\.md)`")
	var prompts []string
	for _, dir := range egmSkillDirs() {
		prompts = append(prompts, tree.Host("egm/skills/"+dir+"/SKILL.md"))
	}
	personas, _ := Personas()
	for _, p := range personas {
		prompts = append(prompts, tree.Host("egm/personas/"+p.Name+"/SKILL.md"))
	}
	checked := 0
	for _, prompt := range prompts {
		data, err := os.ReadFile(prompt)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range refRe.FindAllStringSubmatch(string(data), -1) {
			if strings.ContainsAny(m[1], "{}[]<>") {
				continue
			}
			checked++
			if !isFile(filepath.Join(filepath.Dir(prompt), m[1])) {
				t.Errorf("%s: missing %s", prompt, m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no prompt dependencies found")
	}
}
