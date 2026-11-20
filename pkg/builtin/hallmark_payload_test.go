package builtin

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/skills"
)

// Install items of the hallmark installer, each copied beside the installed skill
func hallmarkInstallPayload(t *testing.T, skill string) []string {
	t.Helper()
	m := regexp.MustCompile(`const PAYLOAD = \[([^\]]*)\];`).FindStringSubmatch(string(must(os.ReadFile(filepath.Join(skill, "scripts", "install.mjs")))))
	if m == nil {
		t.Fatal("install.mjs declares no PAYLOAD")
	}
	var items []string
	for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
		items = append(items, q[1])
	}
	return items
}

func TestHallmarkPayloadCarriesEveryInstallItem(t *testing.T) {
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	skill := tree.Payload(hallmarkSkill)
	items := hallmarkInstallPayload(t, skill)
	if !reflect.DeepEqual(items, []string{"SKILL.md", "LICENSE", "references", "scripts", "agents"}) {
		t.Fatalf("install payload %v", items)
	}
	for _, item := range items {
		if _, err := os.Stat(filepath.Join(skill, item)); err != nil {
			t.Fatalf("install item %s missing: %v", item, err)
		}
	}
	manifest := readJSONMap(t, tree.Payload("hallmark/.claude-plugin/plugin.json"))
	license := string(must(os.ReadFile(filepath.Join(skill, "LICENSE"))))
	if manifest["license"] != "MIT" || !strings.HasPrefix(license, "MIT License\n") ||
		!strings.Contains(license, "Permission is hereby granted, free of charge") {
		t.Fatalf("LICENSE disagrees with the manifest license %v", manifest["license"])
	}
}

func TestHallmarkCodexAgentMetadata(t *testing.T) {
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	skill := tree.Payload(hallmarkSkill)
	fm, _ := skills.ParseFrontmatter(string(must(os.ReadFile(filepath.Join(skill, "SKILL.md")))))
	var agent struct {
		Interface struct {
			DisplayName      string   `yaml:"display_name"`
			ShortDescription string   `yaml:"short_description"`
			DefaultPrompts   []string `yaml:"default_prompts"`
		} `yaml:"interface"`
		Policy struct {
			AllowImplicitInvocation *bool `yaml:"allow_implicit_invocation"`
		} `yaml:"policy"`
		Dependencies []any `yaml:"dependencies"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(must(os.ReadFile(filepath.Join(skill, "agents", "openai.yaml"))))))
	dec.KnownFields(true)
	if err := dec.Decode(&agent); err != nil {
		t.Fatal(err)
	}
	name, _ := fm["name"].(string)
	if name == "" || !strings.EqualFold(agent.Interface.DisplayName, name) || agent.Interface.ShortDescription == "" {
		t.Fatalf("interface %+v for skill %q", agent.Interface, name)
	}
	if len(agent.Interface.DefaultPrompts) == 0 {
		t.Fatal("no default prompts")
	}
	for _, p := range agent.Interface.DefaultPrompts {
		if !strings.HasPrefix(p, "$"+name+" ") {
			t.Fatalf("default prompt %q does not invoke $%s", p, name)
		}
	}
	if agent.Policy.AllowImplicitInvocation == nil || !*agent.Policy.AllowImplicitInvocation || agent.Dependencies == nil || len(agent.Dependencies) != 0 {
		t.Fatalf("policy %+v dependencies %v", agent.Policy, agent.Dependencies)
	}
}

func TestHallmarkMarketplaceListsThePlugin(t *testing.T) {
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := tree.Payload("hallmark")
	manifest := readJSONMap(t, filepath.Join(root, ".claude-plugin", "plugin.json"))
	market := readJSONMap(t, filepath.Join(root, ".claude-plugin", "marketplace.json"))
	owner, _ := market["owner"].(map[string]any)
	if market["name"] != manifest["name"] || owner == nil || owner["name"] == "" || !reflect.DeepEqual(owner, manifest["author"]) {
		t.Fatalf("marketplace header %v", market)
	}
	if meta, _ := market["metadata"].(map[string]any); meta == nil || meta["description"] == "" {
		t.Fatalf("marketplace metadata %v", market["metadata"])
	}
	entries, _ := market["plugins"].([]any)
	if len(entries) != 1 {
		t.Fatalf("marketplace plugins %v", entries)
	}
	entry := entries[0].(map[string]any)
	for _, field := range []string{"name", "version", "description", "author", "homepage", "repository", "license"} {
		if !reflect.DeepEqual(entry[field], manifest[field]) {
			t.Fatalf("marketplace %s %v, plugin.json %v", field, entry[field], manifest[field])
		}
	}
	if entry["strict"] != false || entry["category"] == "" {
		t.Fatalf("marketplace entry %v", entry)
	}
	source, _ := entry["source"].(string)
	if !strings.HasPrefix(source, "./") {
		t.Fatalf("source %q", source)
	}
	listed, _ := entry["skills"].([]any)
	if len(listed) == 0 {
		t.Fatal("marketplace entry lists no skills")
	}
	for _, s := range listed {
		dir := filepath.Join(root, filepath.FromSlash(source), filepath.FromSlash(s.(string)))
		fm, _ := skills.ParseFrontmatter(string(must(os.ReadFile(filepath.Join(dir, "SKILL.md")))))
		if fm["name"] != filepath.Base(dir) || fm["version"] != entry["version"] {
			t.Fatalf("skill %s frontmatter %v against entry version %v", dir, fm, entry["version"])
		}
	}
}
