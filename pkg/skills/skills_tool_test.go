package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Category symlink under the skills dir pointing at a directory outside the tree
func symlinkCategory(t *testing.T, skillsDir, linkedRoot, category string) string {
	t.Helper()
	external := filepath.Join(linkedRoot, category)
	os.MkdirAll(external, 0o755)
	if err := os.Symlink(external, filepath.Join(skillsDir, category)); err != nil {
		t.Skipf("symlinks unavailable in test environment: %v", err)
	}
	return external
}

// Command scope whose skills dir override is a fresh tree with the catalog cache cleared
func toolScope(t *testing.T) (*Scope, string) {
	t.Helper()
	ClearSkillsCache()
	t.Cleanup(ClearSkillsCache)
	s, skills, _ := cmdScope(t)
	return s, skills
}

func TestParseFrontmatterNestedAndBOM(t *testing.T) {
	fm, body := ParseFrontmatter("---\nname: test\ndescription: A test.\n---\n\n# Body\n")
	if fm["name"] != "test" || fm["description"] != "A test." || !strings.Contains(body, "# Body") {
		t.Fatalf("plain %v %q", fm, body)
	}
	fm, _ = ParseFrontmatter("---\nname: test\nmetadata:\n  hermes:\n    tags: [a, b]\n---\n\nBody.\n")
	meta, _ := fm["metadata"].(map[string]any)
	herm, _ := meta["hermes"].(map[string]any)
	if tags, _ := herm["tags"].([]any); len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Fatalf("nested %v", fm)
	}
	bom := string(rune(0xFEFF))
	fm, body = ParseFrontmatter(bom + "---\nname: test\ndescription: A test.\n---\n\n# Body\n")
	if fm["name"] != "test" || fm["description"] != "A test." || strings.HasPrefix(body, bom) {
		t.Fatalf("bom %v %q", fm, body)
	}
}

func TestCategoryDerivedFromLayout(t *testing.T) {
	s, skills := toolScope(t)
	categorized := makeCmdSkill(t, skills, "axolotl", "", "", "mlops")
	top := makeCmdSkill(t, skills, "my-skill", "", "", "")
	if got := s.categoryFromPath(filepath.Join(categorized, "SKILL.md")); got != "mlops" {
		t.Fatalf("categorized %q", got)
	}
	if got := s.categoryFromPath(filepath.Join(top, "SKILL.md")); got != "" {
		t.Fatalf("top level %q", got)
	}
	if got := s.categoryFromPath(filepath.Join(filepath.Dir(skills), "other", "SKILL.md")); got != "" {
		t.Fatalf("outside %q", got)
	}
}

func TestFindAllSkillsSkipsNonSkillTreesAndTruncates(t *testing.T) {
	s, skills := toolScope(t)
	makeCmdSkill(t, skills, "skill-a", "", "", "")
	makeCmdSkill(t, skills, "skill-b", "", "", "")
	makeCmdSkill(t, skills, "axolotl", "", "", "mlops")
	gitDir := filepath.Join(skills, ".git", "fake-skill")
	os.MkdirAll(gitDir, 0o755)
	os.WriteFile(filepath.Join(gitDir, "SKILL.md"), []byte("---\nname: fake\ndescription: x\n---\n\nBody.\n"), 0o644)
	typer := filepath.Join(skills, "bring", "scripts", ".venv", "lib", "python3.13", "site-packages", "typer", ".agents", "skills", "typer")
	os.MkdirAll(typer, 0o755)
	os.WriteFile(filepath.Join(typer, "SKILL.md"), []byte("---\nname: typer\ndescription: Should not be discovered.\n---\n"), 0o644)
	noDesc := filepath.Join(skills, "no-desc")
	os.MkdirAll(noDesc, 0o755)
	os.WriteFile(filepath.Join(noDesc, "SKILL.md"), []byte("---\nname: no-desc\n---\n\n# Heading\n\nFirst paragraph.\n"), 0o644)
	long := filepath.Join(skills, "long-desc")
	os.MkdirAll(long, 0o755)
	os.WriteFile(filepath.Join(long, "SKILL.md"), []byte("---\nname: long\ndescription: "+strings.Repeat("x", MaxDescriptionLength+100)+"\n---\n\nBody.\n"), 0o644)
	byName := map[string]map[string]any{}
	for _, sk := range s.FindAllSkills(true) {
		byName[sk["name"].(string)] = sk
	}
	if len(byName) != 5 || byName["skill-a"] == nil || byName["skill-b"] == nil || byName["axolotl"] == nil {
		t.Fatalf("discovered %v", byName)
	}
	if byName["axolotl"]["category"] != "mlops" {
		t.Fatalf("category %v", byName["axolotl"])
	}
	if byName["no-desc"]["description"] != "First paragraph." || len([]rune(byName["long"]["description"].(string))) > MaxDescriptionLength {
		t.Fatalf("descriptions %v %v", byName["no-desc"], len(byName["long"]["description"].(string)))
	}
}

func TestSymlinkedCategoryIsFoundListedAndViewed(t *testing.T) {
	s, skills := toolScope(t)
	external := symlinkCategory(t, skills, filepath.Join(t.TempDir(), "repo"), "linked")
	makeCmdSkill(t, filepath.Dir(external), "knowledge-brain", "", "", "linked")
	found := s.FindAllSkills(true)
	if len(found) != 1 || found[0]["name"] != "knowledge-brain" || found[0]["category"] != "linked" {
		t.Fatalf("find %v", found)
	}
	listed, _ := decodeObject(s.SkillsList(context.Background(), "linked"))
	cats, _ := listed["categories"].([]any)
	if listed["success"] != true || listed["count"] != float64(1) || len(cats) != 1 || cats[0] != "linked" {
		t.Fatalf("list %v", listed)
	}
	viewed, _ := decodeObject(s.SkillView(context.Background(), "knowledge-brain", "", "", true))
	if viewed["success"] != true || viewed["name"] != "knowledge-brain" {
		t.Fatalf("view %v", viewed)
	}
}

func TestSkillsListEmptyCreatesDirAndFiltersCategory(t *testing.T) {
	s, skills := toolScope(t)
	os.RemoveAll(skills)
	empty, _ := decodeObject(s.SkillsList(context.Background(), ""))
	if list, _ := empty["skills"].([]any); empty["success"] != true || len(list) != 0 || !isDir(skills) {
		t.Fatalf("empty %v", empty)
	}
	makeCmdSkill(t, skills, "skill-a", "", "", "devops")
	makeCmdSkill(t, skills, "skill-b", "", "", "mlops")
	all, _ := decodeObject(s.SkillsList(context.Background(), ""))
	filtered, _ := decodeObject(s.SkillsList(context.Background(), "devops"))
	list, _ := filtered["skills"].([]any)
	if all["count"] != float64(2) || filtered["count"] != float64(1) || len(list) != 1 || list[0].(map[string]any)["name"] != "skill-a" {
		t.Fatalf("filter %v %v", all, filtered)
	}
}

func TestRequiredEnvironmentVariablesNormalization(t *testing.T) {
	entry := map[string]any{"name": "TENOR_API_KEY", "prompt": "Tenor API key", "help": "Get a key from https://developers.google.com/tenor", "required_for": "full functionality"}
	got := getRequiredEnvironmentVariables(map[string]any{"required_environment_variables": []any{entry}})
	if !reflect.DeepEqual(got, []map[string]any{entry}) {
		t.Fatalf("metadata form %v", got)
	}
	legacy := getRequiredEnvironmentVariables(map[string]any{"prerequisites": map[string]any{"env_vars": []any{"TENOR_API_KEY"}}})
	if !reflect.DeepEqual(legacy, []map[string]any{{"name": "TENOR_API_KEY", "prompt": "Enter value for TENOR_API_KEY"}}) {
		t.Fatalf("legacy form %v", legacy)
	}
}

func TestWriteOriginIsolatedBetweenCopiedContexts(t *testing.T) {
	s, _ := manageScope(t)
	original := CurrentWriteOrigin(s.Env)
	child := copiedScope(s)
	SetCurrentWriteOrigin(child.Env, BackgroundReview)
	if CurrentWriteOrigin(child.Env) != BackgroundReview || CurrentWriteOrigin(s.Env) != original {
		t.Fatalf("origin leaked across copies %q %q", CurrentWriteOrigin(child.Env), CurrentWriteOrigin(s.Env))
	}
}

func TestCatalogCacheServesCopiesAndKeepsDisabledViewsApart(t *testing.T) {
	s, skills := toolScope(t)
	makeCmdSkill(t, skills, "skill-one", "", "", "cat-a")
	first := s.FindAllSkills(false)
	if len(first) != 1 || first[0]["name"] != "skill-one" {
		t.Fatalf("first %v", first)
	}
	first[0]["enabled"] = false
	first = append(first, map[string]any{"name": "junk"})
	second := s.FindAllSkills(false)
	if _, poisoned := second[0]["enabled"]; len(second) != 1 || poisoned {
		t.Fatalf("cache poisoned by caller mutation %v", second)
	}
	makeCmdSkill(t, skills, "skill-two", "", "", "cat-a")
	if err := s.Config.Set("skills.disabled", []any{"skill-two"}); err != nil {
		t.Fatal(err)
	}
	names := func(skipDisabled bool) []string {
		var out []string
		for _, sk := range s.FindAllSkills(skipDisabled) {
			out = append(out, sk["name"].(string))
		}
		sort.Strings(out)
		return out
	}
	if got := names(false); !reflect.DeepEqual(got, []string{"skill-one"}) {
		t.Fatalf("filtered %v", got)
	}
	if got := names(true); !reflect.DeepEqual(got, []string{"skill-one", "skill-two"}) {
		t.Fatalf("everything %v", got)
	}
}

type secretScopeKey struct{}

// Test resolver with the unmultiplexed get_secret shape, a ctx bound mapping first then the process env
func testSecretResolver(ctx context.Context, name string) (string, bool, error) {
	if scope, ok := ctx.Value(secretScopeKey{}).(map[string]string); ok {
		v, found := scope[name]
		return v, found, nil
	}
	v, ok := os.LookupEnv(name)
	return v, ok, nil
}

func init() { RegisterSecretResolver(testSecretResolver) }

func TestEnvVarPersistedHonorsSnapshotThenSecretScope(t *testing.T) {
	ctx := context.Background()
	s, _ := toolScope(t)
	t.Setenv("FILLED_KEY", "value")
	t.Setenv("EMPTY_HOST_KEY", "")
	if s.isEnvVarPersisted(ctx, "EMPTY_FILE_KEY", map[string]string{"EMPTY_FILE_KEY": ""}) || !s.isEnvVarPersisted(ctx, "FILLED_FILE_KEY", map[string]string{"FILLED_FILE_KEY": "x"}) {
		t.Fatal("snapshot values misread")
	}
	if s.isEnvVarPersisted(ctx, "EMPTY_HOST_KEY", map[string]string{}) || !s.isEnvVarPersisted(ctx, "FILLED_KEY", map[string]string{}) {
		t.Fatal("process env misread")
	}
	scoped := context.WithValue(ctx, secretScopeKey{}, map[string]string{"VAULT_SKILL_API_KEY": "vault-value"})
	if !s.isEnvVarPersisted(scoped, "VAULT_SKILL_API_KEY", map[string]string{}) {
		t.Fatal("active profile secret scope did not satisfy the requirement")
	}
	t.Setenv("OTHER_PROFILE_SKILL_API_KEY", "other-profile-value")
	RegisterSecretResolver(func(context.Context, string) (string, bool, error) {
		return "", false, errors.New("secret read without a bound profile scope while profile multiplexing is active")
	})
	t.Cleanup(func() { RegisterSecretResolver(testSecretResolver) })
	if s.isEnvVarPersisted(ctx, "OTHER_PROFILE_SKILL_API_KEY", map[string]string{}) {
		t.Fatal("unscoped multiplex read another profile's process environment")
	}
}
