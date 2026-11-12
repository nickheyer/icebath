package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Scope over a fresh home with an empty prompt cache and snapshot before and after the test
func indexScope(t *testing.T) (*Scope, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_PLATFORM", "")
	t.Setenv("ICEBATH_SESSION_PLATFORM", "")
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "")
	ClearSkillsSystemPromptCache(h, true)
	t.Cleanup(func() { ClearSkillsSystemPromptCache(h, true) })
	s := NewScope(h, config.Defaults())
	s.Cwd = h
	s.Platform = ""
	return s, h
}

// Writes a SKILL.md under the skills dir at a relative path
func writeIndexSkill(t *testing.T, root, rel, content string) {
	t.Helper()
	d := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildIndex(t *testing.T, s *Scope, opts SkillsPromptOptions) string {
	t.Helper()
	out, err := s.BuildSkillsSystemPrompt(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Scope reloaded from the home config the way a new turn reads it
func reloadScope(t *testing.T, s *Scope) *Scope {
	t.Helper()
	cfg, err := config.LoadReadonly(context.Background(), s.Home, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := *s
	n.Config = cfg
	return &n
}

func TestBuildSkillsSystemPromptDeduplicatesSkills(t *testing.T) {
	s, h := indexScope(t)
	writeIndexSkill(t, filepath.Join(h, "skills"), "tools/search", "---\ndescription: Search stuff\n---\n")
	if got := strings.Count(buildIndex(t, s, SkillsPromptOptions{}), "- search"); got != 1 {
		t.Fatalf("count %d", got)
	}
}

func TestBuildSkillsSystemPromptCompactCategoriesDemoteNestedAndMissCacheSeparately(t *testing.T) {
	s, h := indexScope(t)
	writeIndexSkill(t, filepath.Join(h, "skills"), "social-media/twitter/thread-writer", "---\nname: thread-writer\ndescription: Write threads\n---\n")
	compact := buildIndex(t, s, SkillsPromptOptions{CompactCategories: map[string]bool{"social-media": true}})
	if !strings.Contains(compact, "thread-writer") || strings.Contains(compact, "Write threads") {
		t.Fatal(compact)
	}
	if full := buildIndex(t, s, SkillsPromptOptions{}); !strings.Contains(full, "Write threads") {
		t.Fatal(full)
	}
}

func TestBuildSkillsSystemPromptExcludesDisabledSkills(t *testing.T) {
	s, h := indexScope(t)
	writeIndexSkill(t, filepath.Join(h, "skills"), "tools/web-search", "---\nname: web-search\ndescription: Search the web\n---\n")
	writeIndexSkill(t, filepath.Join(h, "skills"), "tools/old-tool", "---\nname: old-tool\ndescription: Deprecated tool\n---\n")
	s.Config["skills"] = map[string]any{"disabled": []any{"old-tool"}}
	out := buildIndex(t, s, SkillsPromptOptions{})
	if !strings.Contains(out, "web-search") || strings.Contains(out, "old-tool") {
		t.Fatal(out)
	}
}

func TestRebuildsPromptWhenDisabledSkillsChange(t *testing.T) {
	s, h := indexScope(t)
	writeIndexSkill(t, filepath.Join(h, "skills"), "tools/cached-skill", "---\nname: cached-skill\ndescription: Cached skill\n---\n")
	if !strings.Contains(buildIndex(t, s, SkillsPromptOptions{}), "cached-skill") {
		t.Fatal("first build lacks the skill")
	}
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("skills:\n  disabled: [cached-skill]\n"), 0o644)
	if out := buildIndex(t, reloadScope(t, s), SkillsPromptOptions{}); strings.Contains(out, "cached-skill") {
		t.Fatal(out)
	}
}

func TestSkillShouldShow(t *testing.T) {
	t.Run("test_no_filter_info_always_shows", func(t *testing.T) {
		if !skillShouldShow(map[string][]string{}, nil, nil, "") {
			t.Fatal("no filter info must show")
		}
	})
	t.Run("test_empty_conditions_always_shows", func(t *testing.T) {
		empty := map[string][]string{"fallback_for_toolsets": nil, "requires_toolsets": nil, "fallback_for_tools": nil, "requires_tools": nil}
		if !skillShouldShow(empty, map[string]bool{"web_search": true}, map[string]bool{"web": true}, "") {
			t.Fatal("empty conditions must show")
		}
	})
	t.Run("test_requires_hidden_when_toolset_missing", func(t *testing.T) {
		req := map[string][]string{"requires_toolsets": {"terminal"}}
		if skillShouldShow(req, map[string]bool{}, map[string]bool{}, "") {
			t.Fatal("missing required toolset must hide")
		}
	})
}

func TestBuildSkillsSystemPromptConditional(t *testing.T) {
	t.Run("test_requires_skill_hidden_when_toolset_missing", func(t *testing.T) {
		s, h := indexScope(t)
		writeIndexSkill(t, filepath.Join(h, "skills"), "iot/openhue", "---\nname: openhue\ndescription: Hue lights\nmetadata:\n  hermes:\n    requires_toolsets: [terminal]\n---\n")
		if out := buildIndex(t, s, SkillsPromptOptions{AvailableTools: map[string]bool{}, AvailableToolsets: map[string]bool{}}); strings.Contains(out, "openhue") {
			t.Fatal(out)
		}
	})
	t.Run("test_no_args_shows_all_skills", func(t *testing.T) {
		s2, h2 := indexScope(t)
		writeIndexSkill(t, filepath.Join(h2, "skills"), "search/duckduckgo", "---\nname: duckduckgo\ndescription: Free web search\nmetadata:\n  hermes:\n    fallback_for_toolsets: [web]\n---\n")
		if out := buildIndex(t, s2, SkillsPromptOptions{}); !strings.Contains(out, "duckduckgo") {
			t.Fatal(out)
		}
	})
}

func TestSessionPlatformGate(t *testing.T) {
	conds := func(p ...string) map[string][]string {
		list := make([]any, len(p))
		for i, x := range p {
			list[i] = x
		}
		return extractSkillConditions(map[string]any{"metadata": map[string]any{"hermes": map[string]any{"session_platforms": list}}})
	}
	term := map[string]bool{"terminal": true}
	cases := []struct {
		conds    map[string][]string
		tools    map[string]bool
		toolsets map[string]bool
		platform string
		want     bool
	}{
		{conds("teams", "cron"), term, map[string]bool{}, "desktop", false},
		{conds("teams", "cron"), term, map[string]bool{}, "telegram", false},
		{conds("teams", "cron"), term, map[string]bool{}, "teams", true},
		{conds("teams", "cron"), term, map[string]bool{}, "cron", true},
		{conds("Teams"), term, map[string]bool{}, "TEAMS", true},
		{conds("teams"), term, map[string]bool{}, "", true},
		{conds(), term, map[string]bool{}, "desktop", true},
		{conds("teams"), nil, nil, "desktop", false},
	}
	for i, c := range cases {
		if got := skillShouldShow(c.conds, c.tools, c.toolsets, c.platform); got != c.want {
			t.Fatalf("case %d got %v", i, got)
		}
	}
}

func TestIndexRowsCarryEachNameOnceAndNeverAHalfWord(t *testing.T) {
	s, h := indexScope(t)
	skills := filepath.Join(h, "skills")
	long := "Use when cutting, publishing, or abandoning a icebath-agent release and its notes."
	skill := func(rel, desc string) {
		writeIndexSkill(t, skills, rel, "---\nname: "+filepath.Base(rel)+"\ndescription: "+desc+"\n---\n\nBody.\n")
	}
	skill("creative/ascii-art", "ASCII art banners.")
	skill("creative/pixel-art", "Pixel art sprites.")
	os.WriteFile(filepath.Join(skills, "creative", "DESCRIPTION.md"), []byte("---\ndescription: Creative content generation and visual design tools.\n---\n"), 0o644)
	skill("computer-use", "Drive the desktop.")
	skill("github/icebath-release", long)
	prompt := buildIndex(t, s, SkillsPromptOptions{})
	block := regexp.MustCompile(`(?s)<available_skills>\n(.*?)\n</available_skills>`).FindStringSubmatch(prompt)[1]
	lines := strings.Split(block, "\n")
	has := func(l string) bool {
		for _, x := range lines {
			if x == l {
				return true
			}
		}
		return false
	}
	if !has("  creative:") || strings.Contains(block, "Creative content generation") {
		t.Fatal(block)
	}
	if !has("  - computer-use: Drive the desktop.") || has("  computer-use:") {
		t.Fatal(block)
	}
	var release string
	for _, l := range lines {
		if strings.Contains(l, "icebath-release") {
			release = l
			break
		}
	}
	if !strings.HasPrefix(release, "  - icebath-release: ") {
		t.Fatal(release)
	}
	shown := strings.SplitN(release, ": ", 2)[1]
	if pystr.Len(shown) > SkillPromptDescLimit || !strings.HasSuffix(shown, "...") {
		t.Fatal(shown)
	}
	kept := strings.TrimSuffix(shown, "...")
	if !strings.HasPrefix(long, kept) || !strings.ContainsAny(long[len(kept):len(kept)+1], " ,") {
		t.Fatal(shown)
	}
	got := IndexEntries(prompt)
	if len(got) != 4 || got["ascii-art"] == "" || got["pixel-art"] == "" || got["computer-use"] == "" || got["icebath-release"] == "" {
		t.Fatal(got)
	}
}

func TestClipCutsAtAnyWhitespaceNeverInsideAWord(t *testing.T) {
	for _, sep := range []string{"\t", " "} {
		desc := "alpha" + sep + strings.Repeat("bravo"+sep, 12)
		shown := ClipSkillDescription(desc)
		if pystr.Len(shown) > SkillPromptDescLimit || !strings.HasSuffix(shown, "...") {
			t.Fatalf("%q", shown)
		}
		for _, w := range strings.Split(strings.TrimSuffix(shown, "..."), sep) {
			if w != "alpha" && w != "bravo" {
				t.Fatalf("%q", shown)
			}
		}
	}
}

func writeDeltaSkill(t *testing.T, skills, name, desc string) {
	t.Helper()
	writeIndexSkill(t, skills, "devops/"+name, "---\nname: "+name+"\ndescription: "+desc+"\n---\nBody.\n")
}

func TestSkillInstalledByAnotherProcessReachesARunningProcess(t *testing.T) {
	s, h := indexScope(t)
	skills := filepath.Join(h, "skills")
	writeDeltaSkill(t, skills, "docker-ops", "Run containers.")
	if !strings.Contains(buildIndex(t, s, SkillsPromptOptions{}), "docker-ops") {
		t.Fatal("first build")
	}
	writeDeltaSkill(t, skills, "terraform-ops", "Plan infra.")
	if out := buildIndex(t, s, SkillsPromptOptions{}); !strings.Contains(out, "- terraform-ops: Plan infra.") {
		t.Fatal(out)
	}
}

func TestLongLivedAgentIsToldEachSkillChangeOnce(t *testing.T) {
	s, h := indexScope(t)
	skills := filepath.Join(h, "skills")
	tools := map[string]bool{"skills_list": true, "skill_view": true, "skill_manage": true}
	current := func() (string, error) {
		return s.BuildSkillsSystemPrompt(context.Background(), SkillsPromptOptions{AvailableTools: tools, AvailableToolsets: map[string]bool{"skills": true}})
	}
	var history []map[string]any
	turn := func(n *SkillsIndexNotes, prompt string) string {
		n.Stage("openrouter", "chat_completions", "s1", current, prompt, history)
		note := n.Consume()
		msg := map[string]any{"role": "user", "content": "hi"}
		if note != "" {
			msg["api_content"] = "hi\n\n" + note
		}
		history = append(history, msg, map[string]any{"role": "assistant", "content": "ok"})
		return note
	}
	writeDeltaSkill(t, skills, "docker-ops", "Run containers.")
	agent := &SkillsIndexNotes{}
	prompt := "Identity.\n\n" + buildIndex(t, s, SkillsPromptOptions{})
	if got := turn(agent, prompt); got != "" {
		t.Fatal(got)
	}
	writeDeltaSkill(t, skills, "terraform-ops", "Plan infra.")
	note := turn(agent, prompt)
	if !strings.Contains(note, "    - terraform-ops: Plan infra.") || strings.Contains(note, "docker-ops") {
		t.Fatal(note)
	}
	if got := turn(agent, prompt); got != "" {
		t.Fatal(got)
	}
	for i := 0; i < 150; i++ {
		history = append(history, map[string]any{"role": "user", "content": "x"}, map[string]any{"role": "assistant", "content": "y"})
	}
	if got := turn(&SkillsIndexNotes{}, prompt); got != "" {
		t.Fatal(got)
	}
	os.Remove(filepath.Join(skills, "devops", "docker-ops", "SKILL.md"))
	note = turn(agent, prompt)
	if !strings.Contains(note, "    - terraform-ops: Plan infra.") || !strings.HasSuffix(note, "No longer available: docker-ops]") {
		t.Fatal(note)
	}
	os.Remove(filepath.Join(skills, "devops", "terraform-ops", "SKILL.md"))
	writeDeltaSkill(t, skills, "docker-ops", "Run containers.")
	if got := turn(agent, prompt); !strings.Contains(got, "accurate again") {
		t.Fatal(got)
	}
	if got := turn(&SkillsIndexNotes{}, prompt); got != "" {
		t.Fatal(got)
	}
	writeDeltaSkill(t, skills, "terraform-ops", "Plan infra.")
	if got := turn(agent, prompt); !strings.Contains(got, "terraform-ops") {
		t.Fatal(got)
	}
	rebuilt := "Identity.\n\n" + buildIndex(t, s, SkillsPromptOptions{})
	if a, b := turn(agent, rebuilt), turn(&SkillsIndexNotes{}, rebuilt); a != "" || b != "" {
		t.Fatal(a, b)
	}
}

func TestExternalSkillsDirsFollowConfigEdits(t *testing.T) {
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	external, other := filepath.Join(root, "external_skills"), filepath.Join(root, "other_skills")
	for _, d := range []string{h, external, other} {
		os.MkdirAll(d, 0o755)
	}
	cfgPath := filepath.Join(h, "config.yaml")
	os.WriteFile(cfgPath, []byte("skills:\n  external_dirs:\n    - "+external+"\n"), 0o644)
	s := NewScope(h, config.Defaults())
	if got := reloadScope(t, s).ExternalSkillsDirs(); len(got) != 1 || got[0] != resolvePath(external) {
		t.Fatal(got)
	}
	os.WriteFile(cfgPath, []byte("skills:\n  external_dirs:\n    - "+other+"\n"), 0o644)
	if got := reloadScope(t, s).ExternalSkillsDirs(); len(got) != 1 || got[0] != resolvePath(other) {
		t.Fatal(got)
	}
}

func TestExternalSkillsDirsArePerHome(t *testing.T) {
	root := t.TempDir()
	scopes := map[string]*Scope{}
	for _, n := range []string{"a", "b"} {
		h := filepath.Join(root, "home_"+n, ".icebath")
		ext := filepath.Join(root, "ext_"+n)
		os.MkdirAll(h, 0o755)
		os.MkdirAll(ext, 0o755)
		os.WriteFile(filepath.Join(h, "config.yaml"), []byte("skills:\n  external_dirs:\n    - "+ext+"\n"), 0o644)
		scopes[n] = reloadScope(t, NewScope(h, config.Defaults()))
	}
	for _, n := range []string{"a", "b", "a"} {
		if got := scopes[n].ExternalSkillsDirs(); len(got) != 1 || got[0] != resolvePath(filepath.Join(root, "ext_"+n)) {
			t.Fatal(n, got)
		}
	}
}

func TestApplicationGateRechecksSnapshotWithoutLosingDescription(t *testing.T) {
	s, h := indexScope(t)
	skills := filepath.Join(h, "skills")
	writeIndexSkill(t, skills, "app-guide", "---\nname: app-guide\ndescription: Application instructions.\nrequires_apps: [thing]\n---\nHelp with the application.\n")
	app := filepath.Join(h, "application.exe")
	prev := AppAvailability
	AppAvailability = func(name string) (bool, bool) {
		if name != "thing" {
			return false, false
		}
		_, err := os.Stat(app)
		return true, err == nil
	}
	t.Cleanup(func() { AppAvailability = prev })
	build := func() string {
		out, err := s.buildSkillsSystemPromptInner(context.Background(), skills, nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if strings.Contains(build(), "app-guide") {
		t.Fatal("offered without the app")
	}
	snap := loadSkillsSnapshot(h, buildSkillsManifest(skills))
	if snap == nil {
		t.Fatal("no snapshot")
	}
	list, _ := snap.Get("skills")
	apps, _ := list.([]any)[0].(*pyjson.Object).Get("requires_apps")
	if l, ok := apps.([]any); !ok || len(l) != 1 || l[0] != "thing" {
		t.Fatal(apps)
	}
	os.WriteFile(app, []byte("presence fixture"), 0o644)
	if out := build(); !strings.Contains(out, "app-guide: Application instructions.") {
		t.Fatal(out)
	}
	os.Remove(app)
	if strings.Contains(build(), "app-guide") {
		t.Fatal("offered after removal")
	}
	ClearSkillsSystemPromptCache(h, false)
	if strings.Contains(build(), "app-guide") {
		t.Fatal("offered after cache clear")
	}
}

func TestEveryPkgSkillsSurfaceResolvesDuplicatesLikeSkillView(t *testing.T) {
	s, _, _ := cmdScope(t)
	ctx := context.Background()
	root := t.TempDir()
	local, ext := filepath.Join(root, "local"), filepath.Join(root, "ext")
	s.SkillsDirOverride = local
	s.Home = root
	ClearSkillsSystemPromptCache(root, true)
	t.Cleanup(func() { ClearSkillsSystemPromptCache(root, true) })
	makeCmdSkill(t, local, "dup-demo", "", "BODY A", "a")
	os.Rename(filepath.Join(local, "a", "dup-demo"), filepath.Join(local, "a", "one"))
	makeCmdSkill(t, local, "dup-demo", "", "BODY B", "b")
	os.Rename(filepath.Join(local, "b", "dup-demo"), filepath.Join(local, "b", "two"))
	makeCmdSkill(t, local, "xdup", "", "LOCAL XDUP", "productivity")
	makeCmdSkill(t, ext, "xdup", "", "EXTERNAL XDUP", "productivity")
	s.Config["skills"] = map[string]any{"external_dirs": []any{ext}}
	ambiguous := "Ambiguous skill name dup-demo: use one of a/one, b/two"
	listNames := func() string {
		var out struct {
			Skills []map[string]any `json:"skills"`
		}
		decodeJSON(t, s.SkillsList(ctx, ""), &out)
		var names []string
		for _, sk := range out.Skills {
			names = append(names, sk["name"].(string))
		}
		sort.Strings(names)
		return strings.Join(names, ",")
	}
	indexRows := func() string {
		var rows []string
		for _, ln := range strings.Split(buildIndex(t, s, SkillsPromptOptions{}), "\n") {
			if strings.HasPrefix(pystr.Lstrip(ln), "- ") {
				rows = append(rows, pystr.Strip(ln))
			}
		}
		return strings.Join(rows, "|")
	}
	view := func(name string) map[string]any {
		var out map[string]any
		decodeJSON(t, s.SkillView(ctx, name, "", "", false), &out)
		return out
	}
	if got := listNames(); got != "a/one,b/two,xdup" {
		t.Fatal(got)
	}
	if got := indexRows(); got != "- a/one: Description for dup-demo.|- b/two: Description for dup-demo.|- xdup: Description for xdup." {
		t.Fatal(got)
	}
	if got := commandKeys(s.ScanSkillCommands()); got != "/xdup" {
		t.Fatal(got)
	}
	if got := fmt.Sprint(view("dup-demo")["load_names"]); got != "[a/one b/two]" {
		t.Fatal(got)
	}
	for _, ident := range []string{"xdup", "productivity/xdup"} {
		if c, _ := view(ident)["content"].(string); !strings.Contains(c, "LOCAL XDUP") {
			t.Fatal(ident, c)
		}
	}
	_, loaded, missing := s.BuildPreloadedSkillsPrompt(ctx, []string{"dup-demo", "xdup", "a/one"}, "", nil)
	if strings.Join(loaded, ",") != "xdup,dup-demo" || strings.Join(missing, ",") != ambiguous {
		t.Fatal(loaded, missing)
	}
	prompt, loaded, missing := s.BuildPreloadedSkillsPrompt(ctx, []string{"dup-demo", "xdup"}, "", nil)
	if prompt == "" || strings.Join(loaded, ",") != "xdup" || FormatMissingSkills(missing) != ambiguous {
		t.Fatal("partial preload", loaded, missing)
	}
	cronParts := map[string]string{}
	for _, name := range []string{"dup-demo", "xdup"} {
		var loaded map[string]any
		decodeJSON(t, s.SkillView(ctx, s.NormalizeSkillLookupName(name), "", "", true), &loaded)
		if label, ok := AmbiguousSkillLabel(name, loaded); ok {
			cronParts[name] = label
		} else if c, _ := loaded["content"].(string); c != "" {
			cronParts[name] = c
		}
	}
	if cronParts["dup-demo"] != ambiguous || !strings.Contains(cronParts["xdup"], "LOCAL XDUP") {
		t.Fatal("cron skill parts", cronParts)
	}
	makeCmdSkill(t, ext, "other", "", "UNRELATED OTHER", "a")
	os.Rename(filepath.Join(ext, "a", "other"), filepath.Join(ext, "a", "one"))
	s.Config["skills"] = map[string]any{"external_dirs": []any{ext}, "disabled": []any{"a/one"}}
	ClearSkillsCache()
	ClearSkillsSystemPromptCache(root, false)
	if got := listNames(); got != "b/two,other,xdup" {
		t.Fatal(got)
	}
	if got := indexRows(); got != "- other: Description for other.|- b/two: Description for dup-demo.|- xdup: Description for xdup." {
		t.Fatal(got)
	}
	for _, ident := range []string{"a/one", "one"} {
		if e, _ := view(ident)["error"].(string); !strings.Contains(e, "disabled") {
			t.Fatal(ident, e)
		}
	}
	if c, _ := view("other")["content"].(string); !strings.Contains(c, "UNRELATED OTHER") {
		t.Fatal(c)
	}
	if _, l, m := s.BuildPreloadedSkillsPrompt(ctx, []string{"other"}, "", nil); strings.Join(l, ",") != "other" || len(m) != 0 {
		t.Fatal(l, m)
	}
	aOne := filepath.Join(local, "a", "one", "SKILL.md")
	data, _ := os.ReadFile(aOne)
	os.WriteFile(aOne, []byte(strings.Replace(string(data), "---\n", "---\nrequires_apps: [no-such-app]\n", 1)), 0o644)
	ClearSkillsCache()
	for _, ident := range []string{"a/one", "one"} {
		if e, _ := view(ident)["error"].(string); !strings.Contains(e, "disabled") {
			t.Fatal(ident, e)
		}
	}
}

func decodeJSON(t *testing.T, text string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), out); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
}
