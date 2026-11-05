package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool scope with its captured logs and a cleared catalog cache
func viewScope(t *testing.T) (*Scope, string, *lockedBuffer) {
	t.Helper()
	ClearSkillsCache()
	t.Cleanup(ClearSkillsCache)
	return cmdScope(t)
}

// Sets one skills config key on a scope
func setSkillsCfg(s *Scope, key string, value any) {
	cfg := config.AsMap(s.Config["skills"])
	if cfg == nil {
		cfg = map[string]any{}
		s.Config["skills"] = cfg
	}
	cfg[key] = value
}

// Runs skill_view on the scope and decodes the JSON result
func viewJSON(t *testing.T, s *Scope, name, filePath string) map[string]any {
	t.Helper()
	raw := s.SkillView(context.Background(), name, filePath, "", true)
	var r map[string]any
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("bad json %q: %v", raw, err)
	}
	return r
}

// Unsets an env var for the test and restores it afterwards
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	os.Unsetenv(name)
}

// Installs a secret capture callback for the test duration
func withCapture(t *testing.T, cb SecretCaptureFunc) {
	t.Helper()
	SetSecretCaptureCallback(cb)
	t.Cleanup(func() { SetSecretCaptureCallback(nil) })
}

const tenorReq = "required_environment_variables:\n  - name: TENOR_API_KEY\n    prompt: Tenor API key\n"

func TestSkillViewResolvesByDirNameAndFrontmatterName(t *testing.T) {
	s, skills, _ := viewScope(t)
	makeCmdSkill(t, skills, "my-skill", "metadata:\n  hermes:\n    tags: [fine-tuning, llm]\n", "Step 1: Do the thing.", "")
	alias := filepath.Join(skills, "alias-dir")
	os.MkdirAll(alias, 0o755)
	os.WriteFile(filepath.Join(alias, "SKILL.md"), []byte("---\nname: real-skill-name\ndescription: A skill whose directory name differs from its name.\n---\n\n# real-skill-name\n\nStep 1: Do the thing.\n"), 0o644)
	byDir := viewJSON(t, s, "my-skill", "")
	byName := viewJSON(t, s, "real-skill-name", "")
	tags, _ := byDir["tags"].([]any)
	if byDir["success"] != true || byDir["name"] != "my-skill" || !strings.Contains(byDir["content"].(string), "Step 1") || len(tags) != 2 || tags[0] != "fine-tuning" || tags[1] != "llm" {
		t.Fatalf("by dir %v", byDir)
	}
	if byName["success"] != true || !strings.Contains(byName["content"].(string), "Step 1") {
		t.Fatalf("by name %v", byName)
	}
}

func TestRegisteredViewTracksUseWithTaskAndSession(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	makeCmdSkill(t, home.SkillsDir(h), "my-skill", "", "", "")
	ClearSkillsCache()
	ResetSkillViewDedup("")
	t.Cleanup(ClearSkillsCache)
	events := captureLifecycle(t)
	env := &tools.Env{Home: h, Config: config.Defaults(), Values: &sync.Map{}}
	out := skillViewHandler(context.Background(), &tools.Call{Name: "skill_view", Args: map[string]any{"name": "my-skill"}, TaskID: "task-view", SessionID: "session-view", Env: env})
	if r, _ := decodeObject(out); r["success"] != true {
		t.Fatalf("view %s", out)
	}
	s := ScopeFromEnv(env)
	if v := recGet(t, s, "my-skill", "view_count"); !reflect.DeepEqual(v, recGet(t, s, "my-skill", "use_count")) || v == nil {
		t.Fatalf("counters view %v use %v", v, recGet(t, s, "my-skill", "use_count"))
	}
	if len(*events) != 1 || (*events)[0]["skill_name"] != "my-skill" || (*events)[0]["task_id"] != "task-view" || (*events)[0]["session_id"] != "session-view" || (*events)[0]["action"] != "loaded" {
		t.Fatalf("use events %v", *events)
	}
}

func TestViewReferenceFilesAndDirectoryPath(t *testing.T) {
	s, skills, _ := viewScope(t)
	d := makeCmdSkill(t, skills, "my-skill", "", "", "")
	os.MkdirAll(filepath.Join(d, "references"), 0o755)
	os.WriteFile(filepath.Join(d, "references", "api.md"), []byte("# API Docs\nEndpoint info."), 0o644)
	existing := viewJSON(t, s, "my-skill", "references/api.md")
	missing := viewJSON(t, s, "my-skill", "references/nope.md")
	skill := viewJSON(t, s, "my-skill", "")
	if existing["success"] != true || !strings.Contains(existing["content"].(string), "Endpoint info") || missing["success"] != false {
		t.Fatalf("files %v %v", existing, missing)
	}
	linked, _ := skill["linked_files"].(map[string]any)
	if _, ok := linked["references"]; !ok {
		t.Fatalf("linked %v", skill["linked_files"])
	}
	dir := viewJSON(t, s, "my-skill", "references")
	avail, _ := dir["available_files"].(map[string]any)
	refs, _ := avail["references"].([]any)
	if dir["success"] != false || !strings.Contains(dir["error"].(string), "not found") || len(refs) != 1 || refs[0] != "references/api.md" {
		t.Fatalf("directory path %v", dir)
	}
}

func TestDisabledSkillBlockedEnabledAllowed(t *testing.T) {
	s, skills, _ := viewScope(t)
	makeCmdSkill(t, skills, "hidden-skill", "", "", "")
	makeCmdSkill(t, skills, "active-skill", "", "", "")
	setSkillsCfg(s, "disabled", []any{"hidden-skill"})
	blocked := viewJSON(t, s, "hidden-skill", "")
	if blocked["success"] != false || !strings.Contains(strings.ToLower(blocked["error"].(string)), "disabled") {
		t.Fatalf("blocked %v", blocked)
	}
	if allowed := viewJSON(t, s, "active-skill", ""); allowed["success"] != true {
		t.Fatalf("allowed %v", allowed)
	}
}

func TestSkillViewRequestsMissingRequiredEnvAndContinues(t *testing.T) {
	s, skills, _ := viewScope(t)
	unsetEnv(t, "TENOR_API_KEY")
	var calls []map[string]any
	withCapture(t, func(name, prompt string, metadata map[string]any) (map[string]any, error) {
		calls = append(calls, map[string]any{"var_name": name, "prompt": prompt, "metadata": metadata})
		os.Setenv(name, "stored-in-test")
		return map[string]any{"success": true, "stored_as": name, "validated": false, "skipped": false}, nil
	})
	makeCmdSkill(t, skills, "gif-search", "required_environment_variables:\n  - name: TENOR_API_KEY\n    prompt: Tenor API key\n    help: Get a key from https://developers.google.com/tenor\n    required_for: full functionality\n", "", "")
	r := viewJSON(t, s, "gif-search", "")
	want := []map[string]any{{"var_name": "TENOR_API_KEY", "prompt": "Tenor API key", "metadata": map[string]any{
		"skill_name": "gif-search", "help": "Get a key from https://developers.google.com/tenor", "required_for": "full functionality"}}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
	req, _ := r["required_environment_variables"].([]any)
	if r["success"] != true || r["name"] != "gif-search" || len(req) == 0 || req[0].(map[string]any)["name"] != "TENOR_API_KEY" || r["setup_skipped"] != false {
		t.Fatalf("result %v", r)
	}
}

func TestSkillViewAllowsSkippingSecureSetup(t *testing.T) {
	s, skills, _ := viewScope(t)
	unsetEnv(t, "TENOR_API_KEY")
	withCapture(t, func(name, prompt string, metadata map[string]any) (map[string]any, error) {
		return map[string]any{"success": true, "stored_as": name, "validated": false, "skipped": true}, nil
	})
	makeCmdSkill(t, skills, "gif-search", tenorReq, "", "")
	r := viewJSON(t, s, "gif-search", "")
	if r["success"] != true || r["setup_skipped"] != true || !strings.HasPrefix(r["content"].(string), "---") {
		t.Fatalf("result %v", r)
	}
}

func TestSkillMatchesPlatformForms(t *testing.T) {
	for _, fm := range []map[string]any{{}, {"name": "foo"}, {"platforms": []any{}}, {"platforms": nil}} {
		if !skillMatchesPlatform(fm) {
			t.Fatalf("absent or empty %v", fm)
		}
	}
	host := hostinfo.OSFamily()
	if !skillMatchesPlatform(map[string]any{"platforms": strings.ToUpper(host)}) || !skillMatchesPlatform(map[string]any{"platforms": []any{pystr.Title(host)}}) {
		t.Fatal("host platform not matched case insensitively")
	}
	if skillMatchesPlatform(map[string]any{"platforms": "plan9"}) || skillMatchesPlatform(map[string]any{"platforms": []any{"plan9"}}) {
		t.Fatal("unknown platform matched")
	}
	if skillMatchesPlatform(map[string]any{"platforms": []any{1}}) || skillMatchesPlatform(map[string]any{"platforms": 5}) {
		t.Fatal("non string platforms must compare through str() and miss")
	}
	if !skillMatchesPlatform(map[string]any{"platforms": []any{""}}) {
		t.Fatal("an empty platform string is a prefix of every sys.platform")
	}
}

func TestFindAllSkillsFiltersOnPlatform(t *testing.T) {
	s, skills, _ := viewScope(t)
	makeCmdSkill(t, skills, "universal-skill", "", "", "")
	makeCmdSkill(t, skills, "other-only", "platforms: [plan9]\n", "", "")
	makeCmdSkill(t, skills, "cross-plat", "platforms: [plan9, "+hostinfo.OSFamily()+"]\n", "", "")
	var names []string
	for _, sk := range s.FindAllSkills(true) {
		names = append(names, sk["name"].(string))
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"cross-plat", "universal-skill"}) {
		t.Fatalf("names %v", names)
	}
}

func TestListingShapeIndependentOfEnvVarPrereqs(t *testing.T) {
	s, skills, _ := viewScope(t)
	t.Setenv("TERMINAL_ENV", "docker")
	unsetEnv(t, "NONEXISTENT_API_KEY_XYZ")
	t.Setenv("MY_PRESENT_KEY", "val")
	makeCmdSkill(t, skills, "needs-key", "prerequisites:\n  env_vars: [NONEXISTENT_API_KEY_XYZ]\n", "", "")
	makeCmdSkill(t, skills, "has-key", "prerequisites:\n  env_vars: [MY_PRESENT_KEY]\n", "", "")
	makeCmdSkill(t, skills, "simple-skill", "", "", "")
	listed := s.FindAllSkills(true)
	var names []string
	for _, sk := range listed {
		names = append(names, sk["name"].(string))
		if _, ok := sk["readiness_status"]; ok {
			t.Fatalf("readiness in listing %v", sk)
		}
		if _, ok := sk["missing_prerequisites"]; ok {
			t.Fatalf("prereqs in listing %v", sk)
		}
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"has-key", "needs-key", "simple-skill"}) {
		t.Fatalf("names %v", names)
	}
}

func TestLegacyPrerequisitesExposeRequiredEnvSetupMetadata(t *testing.T) {
	s, skills, _ := viewScope(t)
	unsetEnv(t, "MISSING_KEY_XYZ")
	makeCmdSkill(t, skills, "gated-skill", "prerequisites:\n  env_vars: [MISSING_KEY_XYZ]\n", "", "")
	r := viewJSON(t, s, "gated-skill", "")
	if r["success"] != true || r["setup_needed"] != true || !reflect.DeepEqual(r["missing_required_environment_variables"], []any{"MISSING_KEY_XYZ"}) {
		t.Fatalf("result %v", r)
	}
	want := []any{map[string]any{"name": "MISSING_KEY_XYZ", "prompt": "Enter value for MISSING_KEY_XYZ"}}
	if !reflect.DeepEqual(r["required_environment_variables"], want) {
		t.Fatalf("required %v", r["required_environment_variables"])
	}
}

func TestRemoteBackendTreatsPersistedEnvAsAvailable(t *testing.T) {
	s, skills, _ := viewScope(t)
	t.Setenv("TERMINAL_ENV", "docker")
	makeCmdSkill(t, skills, "remote-ready", "prerequisites:\n  env_vars: [PERSISTED_REMOTE_KEY]\n", "", "")
	t.Setenv("PERSISTED_REMOTE_KEY", "")
	if err := config.SaveEnvValue(s.Home, "PERSISTED_REMOTE_KEY", "persisted-value"); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("PERSISTED_REMOTE_KEY")
	r := viewJSON(t, s, "remote-ready", "")
	if r["success"] != true || r["setup_needed"] != false || !reflect.DeepEqual(r["missing_required_environment_variables"], []any{}) || r["readiness_status"] != "available" {
		t.Fatalf("result %v", r)
	}
}

func TestNoSetupMetadataWhenNoRequiredEnvs(t *testing.T) {
	s, skills, _ := viewScope(t)
	makeCmdSkill(t, skills, "plain-skill", "", "", "")
	r := viewJSON(t, s, "plain-skill", "")
	if r["success"] != true || r["setup_needed"] != false || !reflect.DeepEqual(r["required_environment_variables"], []any{}) {
		t.Fatalf("result %v", r)
	}
}

func TestBackendOnlyAndLocalMissingEnvKeepSetupNeeded(t *testing.T) {
	for _, tc := range []struct{ backend, key string }{{"docker", "BACKEND_ONLY_KEY"}, {"local", "SHELL_ONLY_KEY"}} {
		s, skills, _ := viewScope(t)
		t.Setenv("TERMINAL_ENV", tc.backend)
		unsetEnv(t, tc.key)
		makeCmdSkill(t, skills, "gated", "prerequisites:\n  env_vars: ["+tc.key+"]\n", "", "")
		r := viewJSON(t, s, "gated", "")
		if r["success"] != true || r["setup_needed"] != true || !reflect.DeepEqual(r["missing_required_environment_variables"], []any{tc.key}) || r["readiness_status"] != "setup_needed" {
			t.Fatalf("%s result %v", tc.backend, r)
		}
	}
}

func TestRemoteBackendAvailableAfterLocalSecretCapture(t *testing.T) {
	for _, backend := range []string{"ssh", "daytona", "docker", "modal", "vercel_sandbox"} {
		s, skills, _ := viewScope(t)
		t.Setenv("TERMINAL_ENV", backend)
		unsetEnv(t, "TENOR_API_KEY")
		calls := 0
		withCapture(t, func(name, prompt string, metadata map[string]any) (map[string]any, error) {
			calls++
			os.Setenv(name, "captured-locally")
			return map[string]any{"success": true, "stored_as": name, "validated": false, "skipped": false}, nil
		})
		makeCmdSkill(t, skills, "gif-search", tenorReq, "", "")
		r := viewJSON(t, s, "gif-search", "")
		_, hasNote := r["setup_note"]
		if r["success"] != true || calls != 1 || r["setup_needed"] != false || r["readiness_status"] != "available" || !reflect.DeepEqual(r["missing_required_environment_variables"], []any{}) || hasNote {
			t.Fatalf("%s calls %d result %v", backend, calls, r)
		}
	}
}

func TestLegacyFlatMarkdownSkillPreservesFrontmatterMetadata(t *testing.T) {
	s, skills, _ := viewScope(t)
	os.WriteFile(filepath.Join(skills, "legacy-skill.md"), []byte("---\nname: legacy-flat\ndescription: Legacy flat skill.\nmetadata:\n  hermes:\n    tags: [legacy, flat]\nrequired_environment_variables:\n  - name: LEGACY_KEY\n    prompt: Legacy key\n---\n\n# Legacy Flat\n\nDo the legacy thing.\n"), 0o644)
	r := viewJSON(t, s, "legacy-skill", "")
	if r["success"] != true || r["name"] != "legacy-flat" || r["description"] != "Legacy flat skill." || !reflect.DeepEqual(r["tags"], []any{"legacy", "flat"}) {
		t.Fatalf("result %v", r)
	}
	if !reflect.DeepEqual(r["required_environment_variables"], []any{map[string]any{"name": "LEGACY_KEY", "prompt": "Legacy key"}}) {
		t.Fatalf("required %v", r["required_environment_variables"])
	}
}

func TestSuccessfulSecretCaptureReloadsEmptyEnvPlaceholder(t *testing.T) {
	s, skills, _ := viewScope(t)
	t.Setenv("TERMINAL_ENV", "local")
	unsetEnv(t, "TENOR_API_KEY")
	withCapture(t, func(name, prompt string, metadata map[string]any) (map[string]any, error) {
		if err := config.SaveEnvValue(s.Home, name, "captured-value"); err != nil {
			return nil, err
		}
		return map[string]any{"success": true, "stored_as": name, "validated": false, "skipped": false}, nil
	})
	makeCmdSkill(t, skills, "gif-search", tenorReq, "", "")
	if err := config.SaveEnvValue(s.Home, "TENOR_API_KEY", ""); err != nil {
		t.Fatal(err)
	}
	r := viewJSON(t, s, "gif-search", "")
	if r["success"] != true || r["setup_needed"] != false || !reflect.DeepEqual(r["missing_required_environment_variables"], []any{}) || r["readiness_status"] != "available" {
		t.Fatalf("result %v", r)
	}
}

// Tier roots for the collision cases with the scope wired to every tier
func tierScope(t *testing.T) (*Scope, map[string]string, *lockedBuffer) {
	t.Helper()
	s, local, logs := viewScope(t)
	root := t.TempDir()
	project := filepath.Join(root, "proj")
	os.MkdirAll(filepath.Join(project, ".git"), 0o755)
	tiers := map[string]string{"local": local, "project": filepath.Join(project, ".icebath", "skills"),
		"create_dir": filepath.Join(root, "create_dir"), "external": filepath.Join(root, "external")}
	for _, d := range tiers {
		os.MkdirAll(d, 0o755)
	}
	s.Cwd = project
	setSkillsCfg(s, "trusted_project_dirs", []any{project})
	setSkillsCfg(s, "create_dir", tiers["create_dir"])
	setSkillsCfg(s, "external_dirs", []any{tiers["external"]})
	return s, tiers, logs
}

func TestHigherTierWinsCrossDirectoryCollision(t *testing.T) {
	cases := [][5]string{
		{"local", "external", "foundations/runtime/explore-codebase", "explore-codebase", "explore-codebase"},
		{"local", "external", "productivity/xdup", "productivity/xdup", "productivity/xdup"},
		{"local", "external", "productivity/xdup", "productivity/xdup", "xdup"},
		{"create_dir", "external", "brain/note", "note", "note"},
		{"local", "create_dir", "note", "brain/note", "note"},
		{"project", "local", "deploy", "ops/deploy", "deploy"},
	}
	for _, c := range cases {
		s, tiers, logs := tierScope(t)
		for _, tr := range [][2]string{{c[0], c[2]}, {c[1], c[3]}} {
			cat, leaf := filepath.Split(tr[1])
			makeCmdSkill(t, tiers[tr[0]], leaf, "x-tier: "+tr[0]+"\n", strings.ToUpper(tr[0])+" VERSION", strings.TrimSuffix(cat, "/"))
		}
		r := viewJSON(t, s, c[4], "")
		var listed struct {
			Skills []map[string]any `json:"skills"`
		}
		json.Unmarshal([]byte(s.SkillsList(context.Background(), "")), &listed)
		if r["success"] != true {
			t.Fatalf("%v result %v", c, r)
		}
		content := r["content"].(string)
		if !strings.Contains(content, strings.ToUpper(c[0])+" VERSION") || strings.Contains(content, strings.ToUpper(c[1])+" VERSION") {
			t.Fatalf("%v content %q", c, content)
		}
		leaf := c[2][strings.LastIndex(c[2], "/")+1:]
		var rows []string
		for _, sk := range listed.Skills {
			if n := sk["name"].(string); strings.HasSuffix(n, leaf) {
				rows = append(rows, n)
			}
		}
		if !reflect.DeepEqual(rows, []string{leaf}) {
			t.Fatalf("%v listed %v", c, rows)
		}
		found := false
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, "shadowed") && strings.Contains(line, tiers[c[1]]) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%v no shadow warning in %s", c, logs.String())
		}
	}
}

func TestSupportMarkdownDoesNotCollideWithRealSkill(t *testing.T) {
	s, local, _ := viewScope(t)
	setSkillsCfg(s, "external_dirs", []any{t.TempDir()})
	d := makeCmdSkill(t, local, "article-illustrator", "", "", "creative")
	support := filepath.Join(d, "references", "styles", "sketch.md")
	os.MkdirAll(filepath.Dir(support), 0o755)
	os.WriteFile(support, []byte("# Sketch style support doc\n"), 0o644)
	makeCmdSkill(t, local, "sketch", "", "REAL SKETCH SKILL", "creative")
	r := viewJSON(t, s, "sketch", "")
	if r["success"] != true || r["path"] != filepath.Join("creative", "sketch", "SKILL.md") || !strings.Contains(r["content"].(string), "REAL SKETCH SKILL") {
		t.Fatalf("result %v", r)
	}
}

func TestPackageOwnedMarkdownDoesNotCollideWithRealSkill(t *testing.T) {
	s, local, _ := viewScope(t)
	makeCmdSkill(t, local, "research", "", "REAL RESEARCH SKILL", "")
	ex := makeCmdSkill(t, local, "example", "", "", "character")
	os.MkdirAll(filepath.Join(ex, "prompts"), 0o755)
	os.WriteFile(filepath.Join(ex, "prompts", "research.md"), []byte("# Internal research prompt\n"), 0o644)
	r := viewJSON(t, s, "research", "")
	if r["success"] != true || r["path"] != filepath.Join("research", "SKILL.md") || !strings.Contains(r["content"].(string), "REAL RESEARCH SKILL") {
		t.Fatalf("result %v", r)
	}
	if internal := viewJSON(t, s, "character/example/prompts/research", ""); internal["success"] != false {
		t.Fatalf("internal prompt loaded %v", internal)
	}
}

func TestCategorizedLegacyFlatMarkdownRemainsLoadable(t *testing.T) {
	s, local, _ := viewScope(t)
	os.MkdirAll(filepath.Join(local, "legacy"), 0o755)
	os.WriteFile(filepath.Join(local, "legacy", "research.md"), []byte("---\nname: research\ndescription: Legacy research skill.\n---\n"), 0o644)
	r := viewJSON(t, s, "legacy/research", "")
	if r["success"] != true || r["path"] != filepath.Join("legacy", "research.md") {
		t.Fatalf("result %v", r)
	}
}

func TestTwoExternalsSameNameAlsoRefuse(t *testing.T) {
	s, _, _ := viewScope(t)
	extA, extB := t.TempDir(), t.TempDir()
	makeCmdSkill(t, extA, "pr", "", "EXT_A VERSION", "")
	makeCmdSkill(t, extB, "pr", "", "EXT_B VERSION", "")
	setSkillsCfg(s, "external_dirs", []any{extA, extB})
	r := viewJSON(t, s, "pr", "")
	matches, _ := r["matches"].([]any)
	if r["success"] != false || !strings.Contains(r["error"].(string), "Ambiguous") || len(matches) != 2 {
		t.Fatalf("result %v", r)
	}
}

func TestSameRootDuplicationResolves(t *testing.T) {
	s, local, _ := viewScope(t)
	makeCmdSkill(t, local, "arxiv", "", "IDENTICAL COPY", "")
	makeCmdSkill(t, local, "arxiv", "", "IDENTICAL COPY", "research")
	if r := viewJSON(t, s, "arxiv", ""); r["success"] != true || r["path"] != "arxiv/SKILL.md" || !strings.Contains(r["content"].(string), "IDENTICAL COPY") {
		t.Fatalf("nested copy %v", r)
	}

	s, local, _ = viewScope(t)
	makeCmdSkill(t, local, "github", "", "REAL GITHUB", "software-development")
	os.MkdirAll(filepath.Join(local, "evil"), 0o755)
	os.WriteFile(filepath.Join(local, "evil", "SKILL.md"), []byte("---\nname: github\ndescription: d.\n---\nEVIL BODY\n"), 0o644)
	if r := viewJSON(t, s, "github", ""); r["success"] != false || !strings.Contains(r["error"].(string), "Ambiguous") || len(r["matches"].([]any)) != 2 {
		t.Fatalf("different content %v", r)
	}

	s, local, _ = viewScope(t)
	os.WriteFile(filepath.Join(local, "foo.md"), []byte("---\nname: foo\ndescription: d.\n---\nLEGACY FLAT\n"), 0o644)
	makeCmdSkill(t, local, "foo", "", "NESTED PACKAGE", "hubpkg")
	if r := viewJSON(t, s, "foo", ""); r["success"] != false || !strings.Contains(r["error"].(string), "Ambiguous") || len(r["matches"].([]any)) != 2 {
		t.Fatalf("nested package over flat %v", r)
	}

	s, local, _ = viewScope(t)
	makeCmdSkill(t, local, "dup", "", "A", "cat-a")
	makeCmdSkill(t, local, "dup", "", "B", "cat-b")
	r := viewJSON(t, s, "dup", "")
	if r["success"] != false || !strings.Contains(r["error"].(string), "Ambiguous skill name 'dup': use one of cat-a/dup, cat-b/dup") ||
		!reflect.DeepEqual(r["load_names"], []any{"cat-a/dup", "cat-b/dup"}) || len(r["matches"].([]any)) != 2 {
		t.Fatalf("equal rank %v", r)
	}
}

func TestTrustWarningSymlinkAware(t *testing.T) {
	s, _, logs := viewScope(t)
	tmp := t.TempDir()
	lib, root := filepath.Join(tmp, "library"), filepath.Join(tmp, "root")
	real := filepath.Join(lib, "demo")
	os.MkdirAll(real, 0o755)
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(real, "SKILL.md"), []byte("---\nname: demo\ndescription: d.\n---\nbody\n"), 0o644)
	if err := os.Symlink(real, filepath.Join(root, "demo")); err != nil {
		t.Skipf("symlinks unavailable in test environment: %v", err)
	}
	s.logSecurityWarnings("demo", filepath.Join(root, "demo", "SKILL.md"), "plain body", []string{root, lib}, root)
	if strings.Contains(logs.String(), "outside the trusted") {
		t.Fatalf("trusted symlink warned %s", logs.String())
	}

	root2 := filepath.Join(tmp, "root2")
	os.MkdirAll(filepath.Join(root2, "sym"), 0o755)
	outside := filepath.Join(tmp, "elsewhere", "SKILL.md")
	os.MkdirAll(filepath.Dir(outside), 0o755)
	os.WriteFile(outside, []byte("---\nname: sym\ndescription: d.\n---\nbody\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root2, "sym", "SKILL.md")); err != nil {
		t.Skipf("symlinks unavailable in test environment: %v", err)
	}
	s.logSecurityWarnings("sym", filepath.Join(root2, "sym", "SKILL.md"), "plain body", []string{root2}, root2)
	if !strings.Contains(logs.String(), "outside the trusted") {
		t.Fatalf("outside symlink did not warn %s", logs.String())
	}
}

// Writes a categorized skill under a home's skills dir
func writeHomeSkill(t *testing.T, h, category, name, description string) string {
	t.Helper()
	d := filepath.Join(h, "skills", category, name)
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: "+description+"\n---\n\n# "+name+"\n\nLoaded from "+description+".\n"), 0o644)
	return d
}

func TestSkillViewUsesLiveProfileHome(t *testing.T) {
	tmp := t.TempDir()
	defaultHome, profileHome := filepath.Join(tmp, "default-home"), filepath.Join(tmp, "profiles", "orchestrator")
	writeHomeSkill(t, defaultHome, "autonomous-ai-agents", "default-only", "default home")
	profileSkill := writeHomeSkill(t, profileHome, "software-development", "kanban-orchestrator-operations", "orchestrator profile")
	t.Setenv("ICEBATH_HOME", defaultHome)
	ClearSkillsCache()
	t.Cleanup(ClearSkillsCache)
	if got := NewScope(home.Process(), nil).SkillsDir(); got != filepath.Join(defaultHome, "skills") {
		t.Fatalf("import time skills dir %q", got)
	}
	t.Setenv("ICEBATH_HOME", profileHome)
	var r map[string]any
	json.Unmarshal([]byte(ScopeFromEnv(nil).SkillView(context.Background(), "kanban-orchestrator-operations", "", "", false)), &r)
	if r["success"] != true || r["name"] != "kanban-orchestrator-operations" || r["skill_dir"] != profileSkill || !strings.Contains(r["content"].(string), "orchestrator profile") {
		t.Fatalf("result %v", r)
	}

	patched := writeHomeSkill(t, filepath.Join(tmp, "patched"), "software-development", "patched-skill", "patched skills dir")
	writeHomeSkill(t, profileHome, "software-development", "profile-skill", "orchestrator profile")
	s := ScopeFromEnv(nil)
	s.SkillsDirOverride = filepath.Join(tmp, "patched", "skills")
	json.Unmarshal([]byte(s.SkillView(context.Background(), "patched-skill", "", "", false)), &r)
	if r["success"] != true || r["skill_dir"] != patched {
		t.Fatalf("override result %v", r)
	}
}
