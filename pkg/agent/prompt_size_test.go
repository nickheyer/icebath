package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Temp icebath home under a temp cwd so no repository AGENTS.md is read
func promptSizeHome(t *testing.T) (string, *tools.Env) {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Chdir(tmp)
	return h, &tools.Env{Home: h, Cwd: tmp}
}

// Writes a demo category skill with the given frontmatter name and description
func seedPromptSizeSkill(t *testing.T, h, name, description string) {
	t.Helper()
	dir := filepath.Join(h, "skills", "demo", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n# " + name + "\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunsOfflineWithoutCredentials(t *testing.T) {
	_, env := promptSizeHome(t)
	for _, v := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "NOUS_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	data, err := ComputePromptBreakdown(context.Background(), env, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if data.SystemPrompt.Bytes <= 0 {
		t.Fatalf("system prompt bytes %d", data.SystemPrompt.Bytes)
	}
}

func TestSkillsBreakdownShapeSortedAndAttributed(t *testing.T) {
	h, env := promptSizeHome(t)
	seedPromptSizeSkill(t, h, "small-skill", "short desc")
	seedPromptSizeSkill(t, h, "big-skill", strings.Repeat("a much longer description ", 20))
	data, err := ComputePromptBreakdown(context.Background(), env, "cli")
	if err != nil {
		t.Fatal(err)
	}
	entries := data.SkillsBreakdown
	byName := map[string]SkillBreakdownEntry{}
	for _, s := range entries {
		byName[s.Name] = s
	}
	for _, want := range []string{"small-skill", "big-skill"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("%s missing from %v", want, entries)
		}
	}
	for _, s := range entries {
		keys := s.Object().Keys
		for _, k := range []string{"name", "index_line_bytes", "skill_md_bytes", "path"} {
			if !slices.Contains(keys, k) {
				t.Fatalf("entry %s keys %v lack %s", s.Name, keys, k)
			}
		}
		if s.IndexLineBytes <= 0 {
			t.Fatalf("entry %s index line bytes %d", s.Name, s.IndexLineBytes)
		}
	}
	md := make([]int64, len(entries))
	for i, s := range entries {
		if s.SkillMDBytes != nil {
			md[i] = *s.SkillMDBytes
		}
	}
	sorted := slices.Clone(md)
	slices.Sort(sorted)
	slices.Reverse(sorted)
	if !slices.Equal(md, sorted) {
		t.Fatalf("SKILL.md sizes not largest first %v", md)
	}
	big, small := byName["big-skill"], byName["small-skill"]
	if big.Path == "" || big.SkillMDBytes == nil {
		t.Fatalf("big skill entry %+v", big)
	}
	st, err := os.Stat(big.Path)
	if err != nil || st.Size() != *big.SkillMDBytes {
		t.Fatalf("big skill stat %v %v vs %d", st, err, *big.SkillMDBytes)
	}
	if small.SkillMDBytes == nil || *big.SkillMDBytes <= *small.SkillMDBytes {
		t.Fatalf("big %d not larger than small %v", *big.SkillMDBytes, small.SkillMDBytes)
	}
	sum := 0
	for _, s := range entries {
		sum += s.IndexLineBytes
	}
	if sum > data.SkillsIndex.Bytes {
		t.Fatalf("index line bytes %d exceed the skills index %d", sum, data.SkillsIndex.Bytes)
	}
}

func TestSkillsBreakdownAttributesDemotedCategorySharedLine(t *testing.T) {
	h, env := promptSizeHome(t)
	seedPromptSizeSkill(t, h, "alpha-skill", "alpha description")
	seedPromptSizeSkill(t, h, "beta-skill", "beta description")
	prompt, err := skills.ScopeFromEnv(env).BuildSkillsSystemPrompt(home.WithOverride(context.Background(), h),
		skills.SkillsPromptOptions{CompactCategories: map[string]bool{"demo": true}})
	if err != nil {
		t.Fatal(err)
	}
	skillsBlock := promptSkillsBlockRe.FindString(prompt)
	if skillsBlock == "" {
		t.Fatalf("no available_skills block in %q", prompt)
	}
	sharedLine, found := "", false
	for _, line := range pystr.Splitlines(skillsBlock, false) {
		if strings.Contains(line, "demo [names only]") {
			sharedLine, found = line, true
			break
		}
	}
	if !found {
		t.Fatalf("no demoted demo line in %q", skillsBlock)
	}
	entries, err := computeSkillsBreakdown(env, skillsBlock)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"alpha-skill", "beta-skill"}) {
		t.Fatalf("names %v", names)
	}
	sharedLineBytes := len(sharedLine)
	sum := 0
	for _, e := range entries {
		sum += e.IndexLineBytes
	}
	if sum != sharedLineBytes {
		t.Fatalf("attributed %d of a %d byte line", sum, sharedLineBytes)
	}
	for _, e := range entries {
		if e.IndexLineTotalBytes != sharedLineBytes || e.IndexLineSharedBytes <= 0 || e.IndexLineSkillCount != 2 {
			t.Fatalf("entry %+v on a %d byte line", e, sharedLineBytes)
		}
	}
}

func TestComputeSkillsBreakdownParsesRowsAndSharedLines(t *testing.T) {
	_, env := promptSizeHome(t)
	block := "<available_skills>\n  demo [names only]: alpha-skill, beta-skill,  gamma , \n  code:\n    - codex:rescue: desc: more\n    -  \n  - solo\n- top: x\n</available_skills>"
	entries, err := computeSkillsBreakdown(env, block)
	if err != nil {
		t.Fatal(err)
	}
	var rows []any
	for _, e := range entries {
		rows = append(rows, e.Object())
	}
	got, err := pyjson.Dumps(rows, pyjson.EnsureASCII(false))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"name": "alpha-skill", "index_line_bytes": 21, "index_line_total_bytes": 55, "index_line_shared_bytes": 10, "index_line_skill_count": 3, "skill_md_bytes": null, "path": ""}, {"name": "beta-skill", "index_line_bytes": 20, "index_line_total_bytes": 55, "index_line_shared_bytes": 10, "index_line_skill_count": 3, "skill_md_bytes": null, "path": ""}, {"name": "codex:rescue", "index_line_bytes": 30, "index_line_total_bytes": 30, "index_line_shared_bytes": 0, "index_line_skill_count": 1, "skill_md_bytes": null, "path": ""}, {"name": "gamma", "index_line_bytes": 14, "index_line_total_bytes": 55, "index_line_shared_bytes": 9, "index_line_skill_count": 3, "skill_md_bytes": null, "path": ""}, {"name": "solo", "index_line_bytes": 8, "index_line_total_bytes": 8, "index_line_shared_bytes": 0, "index_line_skill_count": 1, "skill_md_bytes": null, "path": ""}, {"name": "top", "index_line_bytes": 8, "index_line_total_bytes": 8, "index_line_shared_bytes": 0, "index_line_skill_count": 1, "skill_md_bytes": null, "path": ""}]`
	if got != want {
		t.Fatalf("breakdown\n got %s\nwant %s", got, want)
	}
	if _, err := computeSkillsBreakdown(env, "  - a\xed\xa0\x80b"); err == nil || err.Error() != `'utf-8' codec can't encode character '\ud800' in position 5: surrogates not allowed` {
		t.Fatalf("surrogate line error %v", err)
	}
}

func TestSkillMDPathsByNameKeysFrontmatterAndDirName(t *testing.T) {
	h, env := promptSizeHome(t)
	seedPromptSizeSkill(t, h, "renamed", "d")
	if err := os.Rename(filepath.Join(h, "skills", "demo", "renamed"), filepath.Join(h, "skills", "demo", "dirname")); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(h, "skills", "demo", "badutf")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "SKILL.md"), []byte("---\nname: never\n---\n\xff"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := skillMDPathsByName(env)
	want := filepath.Join(h, "skills", "demo", "dirname", "SKILL.md")
	if m["renamed"] != want || m["dirname"] != want {
		t.Fatalf("mapping %v", m)
	}
	if m["badutf"] != filepath.Join(broken, "SKILL.md") {
		t.Fatalf("undecodable SKILL.md keyed %v", m)
	}
	if _, ok := m["never"]; ok {
		t.Fatalf("undecodable frontmatter name leaked %v", m)
	}
}

func TestPromptToolNameShapes(t *testing.T) {
	obj := pyjson.NewObject()
	obj.Set("name", "ordered")
	for _, tc := range []struct {
		tool any
		want string
	}{
		{map[string]any{"function": map[string]any{"name": "fn"}, "name": "outer"}, "fn"},
		{map[string]any{"function": map[string]any{"name": ""}, "name": "outer"}, "outer"},
		{map[string]any{"function": "x", "name": 7}, "7"},
		{map[string]any{"name": nil}, "None"},
		{map[string]any{}, ""},
		{"not a dict", ""},
		{obj, "ordered"},
	} {
		if got := promptToolName(tc.tool); got != tc.want {
			t.Fatalf("%v name %q want %q", tc.tool, got, tc.want)
		}
	}
}

func TestComputeToolsetsBreakdownGroupsLargestFirst(t *testing.T) {
	m := tools.Default.ToolToToolsetMap(context.Background(), "")
	var known string
	for name := range m {
		if known == "" || name < known {
			known = name
		}
	}
	if known == "" {
		t.Fatal("registry holds no tools")
	}
	list := []any{
		map[string]any{"name": "zz-none-a"},
		map[string]any{"type": "function", "function": map[string]any{"name": known}},
		"str",
		map[string]any{"name": "zz-none-b"},
	}
	got, err := computeToolsetsBreakdown(context.Background(), list)
	if err != nil {
		t.Fatal(err)
	}
	knownJSON, _ := pyjson.Dumps(list[1], pyjson.EnsureASCII(false))
	unknown := ToolsetBreakdownEntry{Toolset: "(unknown)", ToolCount: 3, JSONBytes: 21 + 5 + 21}
	knownRow := ToolsetBreakdownEntry{Toolset: m[known], ToolCount: 1, JSONBytes: len(knownJSON)}
	want := []ToolsetBreakdownEntry{unknown, knownRow}
	if knownRow.JSONBytes > unknown.JSONBytes || (knownRow.JSONBytes == unknown.JSONBytes && knownRow.Toolset < unknown.Toolset) {
		want = []ToolsetBreakdownEntry{knownRow, unknown}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("toolsets %+v want %+v", got, want)
	}
	tie, err := computeToolsetsBreakdown(context.Background(), []any{})
	if err != nil || len(tie) != 0 {
		t.Fatalf("empty toolsets %v %v", tie, err)
	}
}

// Breakdown the render and json tests share, mirroring the Python fixture dict
func promptSizeFixture() *PromptBreakdown {
	data := &PromptBreakdown{Platform: "telegram", SystemPrompt: PromptTextSize{1234567, 1234999},
		SkillsIndex: PromptTextSize{10, 2048}, UserProfile: PromptTextSize{5, 1535}, ToolCount: 3, ToolJSONBytes: 99999,
		Sections:          []PromptSection{{"stable (identity/guidance/skills)", 1, 1023}, {"context (AGENTS.md/cwd files)", 2, 1076}, {"volatile (memory/profile/timestamp)", 3, 52}},
		ToolsetsBreakdown: []ToolsetBreakdownEntry{{"terminal", 2, 5000}, {"(unknown)", 1, 77}}}
	for i := range 22 {
		e := SkillBreakdownEntry{Name: fmt.Sprintf("s%02d", i), IndexLineBytes: 1000 + i}
		if i%3 != 0 {
			n := int64(12345 - i)
			e.SkillMDBytes = &n
		}
		data.SkillsBreakdown = append(data.SkillsBreakdown, e)
	}
	data.SkillsBreakdown[0].Name = "a-very-long-skill-name-that-overflows-é"
	return data
}

func TestRenderBreakdownMatchesPythonLayout(t *testing.T) {
	data := promptSizeFixture()
	want := "Prompt-size breakdown (platform=telegram, model=unset)\n\n  System prompt total : 1,234,999 B  (1206.1 KB, 1,234,567 chars)\n\n  Major blocks:\n    skills index       :    2,048 B  (2.0 KB)\n    memory             :        0 B  (0.0 KB)\n    user profile       :    1,535 B  (1.5 KB)\n\n  Prompt tiers:\n    stable (identity/guidance/skills)   :    1,023 B  (1.0 KB)\n    context (AGENTS.md/cwd files)       :    1,076 B  (1.1 KB)\n    volatile (memory/profile/timestamp) :       52 B  (0.1 KB)\n\n  Tool schemas         :   99,999 B  (97.7 KB, 3 tools)\n\n  Toolsets by size (tool-schema JSON, largest first):\n    toolset                tools      schema\n    terminal                   2     5,000 B  (4.9 KB)\n    (unknown)                  1        77 B  (0.1 KB)\n\n  Skills by size (SKILL.md on-disk = read cost; index cost = attributed always-on bytes, largest first):\n    skill                          SKILL.md  index cost\n    a-very-long-skill-name-that…        n/a     1,000 B\n    s01                            12,344 B     1,001 B\n    s02                            12,343 B     1,002 B\n    s03                                 n/a     1,003 B\n    s04                            12,341 B     1,004 B\n    s05                            12,340 B     1,005 B\n    s06                                 n/a     1,006 B\n    s07                            12,338 B     1,007 B\n    s08                            12,337 B     1,008 B\n    s09                                 n/a     1,009 B\n    s10                            12,335 B     1,010 B\n    s11                            12,334 B     1,011 B\n    s12                                 n/a     1,012 B\n    s13                            12,332 B     1,013 B\n    s14                            12,331 B     1,014 B\n    s15                                 n/a     1,015 B\n    s16                            12,329 B     1,016 B\n    s17                            12,328 B     1,017 B\n    s18                                 n/a     1,018 B\n    s19                            12,326 B     1,019 B\n    … and 2 more (use --json for the full list)"
	if got := RenderBreakdown(data); got != want {
		t.Fatalf("render\n got %q\nwant %q", got, want)
	}
	data.Model, data.SkillsBreakdown, data.ToolsetsBreakdown = "m/x", nil, nil
	want = "Prompt-size breakdown (platform=telegram, model=m/x)\n\n  System prompt total : 1,234,999 B  (1206.1 KB, 1,234,567 chars)\n\n  Major blocks:\n    skills index       :    2,048 B  (2.0 KB)\n    memory             :        0 B  (0.0 KB)\n    user profile       :    1,535 B  (1.5 KB)\n\n  Prompt tiers:\n    stable (identity/guidance/skills)   :    1,023 B  (1.0 KB)\n    context (AGENTS.md/cwd files)       :    1,076 B  (1.1 KB)\n    volatile (memory/profile/timestamp) :       52 B  (0.1 KB)\n\n  Tool schemas         :   99,999 B  (97.7 KB, 3 tools)"
	if got := RenderBreakdown(data); got != want {
		t.Fatalf("render without tables\n got %q\nwant %q", got, want)
	}
}

func TestPromptBreakdownJSONMatchesPythonDump(t *testing.T) {
	md := int64(120)
	data := &PromptBreakdown{Platform: "cli", Model: "anthropic/claude-é", SystemPrompt: PromptTextSize{3, 4}, Memory: PromptTextSize{1, 1},
		UserProfile: PromptTextSize{2, 2}, ToolCount: 1, ToolJSONBytes: 40,
		Sections: []PromptSection{{"stable (identity/guidance/skills)", 3, 4}, {"context (AGENTS.md/cwd files)", 0, 0}, {"volatile (memory/profile/timestamp)", 0, 0}},
		SkillsBreakdown: []SkillBreakdownEntry{{Name: "a", IndexLineBytes: 7, IndexLineTotalBytes: 7, IndexLineSkillCount: 1, SkillMDBytes: &md, Path: "/h/skills/a/SKILL.md"},
			{Name: "b", IndexLineBytes: 3, IndexLineTotalBytes: 9, IndexLineSharedBytes: 2, IndexLineSkillCount: 2}},
		ToolsetsBreakdown: []ToolsetBreakdownEntry{{"web", 1, 40}}}
	got, err := pyjson.Dumps(data.Object(), pyjson.EnsureASCII(false), pyjson.Indent(2))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"platform\": \"cli\",\n  \"model\": \"anthropic/claude-é\",\n  \"system_prompt\": {\n    \"chars\": 3,\n    \"bytes\": 4\n  },\n  \"skills_index\": {\n    \"chars\": 0,\n    \"bytes\": 0\n  },\n  \"memory\": {\n    \"chars\": 1,\n    \"bytes\": 1\n  },\n  \"user_profile\": {\n    \"chars\": 2,\n    \"bytes\": 2\n  },\n  \"tools\": {\n    \"count\": 1,\n    \"json_bytes\": 40\n  },\n  \"sections\": [\n    [\n      \"stable (identity/guidance/skills)\",\n      3,\n      4\n    ],\n    [\n      \"context (AGENTS.md/cwd files)\",\n      0,\n      0\n    ],\n    [\n      \"volatile (memory/profile/timestamp)\",\n      0,\n      0\n    ]\n  ],\n  \"skills_breakdown\": [\n    {\n      \"name\": \"a\",\n      \"index_line_bytes\": 7,\n      \"index_line_total_bytes\": 7,\n      \"index_line_shared_bytes\": 0,\n      \"index_line_skill_count\": 1,\n      \"skill_md_bytes\": 120,\n      \"path\": \"/h/skills/a/SKILL.md\"\n    },\n    {\n      \"name\": \"b\",\n      \"index_line_bytes\": 3,\n      \"index_line_total_bytes\": 9,\n      \"index_line_shared_bytes\": 2,\n      \"index_line_skill_count\": 2,\n      \"skill_md_bytes\": null,\n      \"path\": \"\"\n    }\n  ],\n  \"toolsets_breakdown\": [\n    {\n      \"toolset\": \"web\",\n      \"tool_count\": 1,\n      \"json_bytes\": 40\n    }\n  ]\n}"
	if got != want {
		t.Fatalf("json\n got %s\nwant %s", got, want)
	}
}

func TestCmdPromptSizePrintsTextJSONAndFailure(t *testing.T) {
	h, env := promptSizeHome(t)
	var out bytes.Buffer
	code, err := CmdPromptSize(context.Background(), env, map[string]any{"platform": nil, "json": false}, &out)
	if err != nil || code != 0 || !strings.HasPrefix(out.String(), "Prompt-size breakdown (platform=cli, model=") || !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("text %d %v %q", code, err, out.String())
	}
	out.Reset()
	code, err = CmdPromptSize(context.Background(), env, map[string]any{"platform": "telegram", "json": true}, &out)
	if err != nil || code != 0 {
		t.Fatalf("json %d %v", code, err)
	}
	parsed, err := pyjson.Loads(out.String())
	if err != nil {
		t.Fatalf("json output %v %q", err, out.String())
	}
	obj := parsed.(*pyjson.Object)
	wantKeys := []string{"platform", "model", "system_prompt", "skills_index", "memory", "user_profile", "tools", "sections", "skills_breakdown", "toolsets_breakdown"}
	if !slices.Equal(obj.Keys, wantKeys) || obj.Vals["platform"] != "telegram" {
		t.Fatalf("json keys %v platform %v", obj.Keys, obj.Vals["platform"])
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("agent: oops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	code, err = CmdPromptSize(context.Background(), env, map[string]any{"platform": "cli"}, &out)
	if err != nil || code != 0 || out.String() != "Could not compute prompt-size breakdown: dictionary update sequence element #0 has length 1; 2 is required\n" {
		t.Fatalf("failure %d %v %q", code, err, out.String())
	}
}

func TestPromptSizeHelpers(t *testing.T) {
	if got := promptFmtKB(1536); got != "1.5 KB" {
		t.Fatalf("kb %q", got)
	}
	if got := promptFmtKB(0); got != "0.0 KB" {
		t.Fatalf("kb zero %q", got)
	}
	if s, err := promptSize("héllo"); err != nil || s != (PromptTextSize{5, 6}) {
		t.Fatalf("size %v %v", s, err)
	}
	for _, tc := range [][4]int{{7, 2, 3, 1}, {-7, 2, -4, 1}, {7, -2, -4, -1}, {6, 3, 2, 0}} {
		if q, r := promptDivmod(tc[0], tc[1]); q != tc[2] || r != tc[3] {
			t.Fatalf("divmod(%d, %d) = %d, %d", tc[0], tc[1], q, r)
		}
	}
}
