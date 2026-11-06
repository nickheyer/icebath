package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const (
	describeSkillBody = "Kick off a task in a fresh isolated git worktree instead of the current checkout."
	emDashSeparator   = " \xe2\x80\x94 "
)

// Writes a SKILL.md under the skills dir with the description test frontmatter
func writeDescribeSkill(t *testing.T, skills, name, body string) {
	t.Helper()
	d := filepath.Join(skills, name)
	os.MkdirAll(d, 0o755)
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Description for "+name+"\n---\n\n# "+name+"\n\n"+body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Command scope holding the work and clean skills
func describeScope(t *testing.T) (*Scope, string) {
	t.Helper()
	s, skills, _ := cmdScope(t)
	writeDescribeSkill(t, skills, "work", describeSkillBody)
	writeDescribeSkill(t, skills, "clean", "Polish your own diff by hand.")
	return s, skills
}

// Head and tail excerpt the session preview query hands to the shaper
func previewExcerpt(message string, window int) string {
	flat := []rune(strings.NewReplacer("\n", " ", "\r", " ").Replace(message))
	if len([]rune(message)) <= window*2 {
		if len(flat) > window*2 {
			flat = flat[:window*2]
		}
		return string(flat)
	}
	return string(flat[:window]) + state.SkillExcerptJoint + string(flat[len(flat)-window:])
}

// Auto load scaffold built with the gateway auto load builders, the typed request appended when set
func autoLoadScaffold(t *testing.T, s *Scope, names []string, userText string) string {
	t.Helper()
	ctx := context.Background()
	var payloads []string
	for _, name := range names {
		loaded, skillDir, resolved, ok := s.loadSkillPayload(ctx, name, "")
		if !ok {
			t.Fatalf("skill %s failed to load", name)
		}
		payloads = append(payloads, s.buildSkillMessage(ctx, loaded, skillDir, `[IMPORTANT: The "`+resolved+`" skill is auto-loaded. Follow its instructions for this session.]`, "", "", ""))
	}
	joined := strings.Join(payloads, "\n\n")
	if userText != "" {
		return joined + "\n\n" + userText
	}
	return joined
}

// Description of a turn, empty when the text is not scaffolding
func describe(content string) string {
	out, _ := DescribeSkillInvocation(content, emDashSeparator)
	return out
}

func TestDescribeRecoversTheTypedInstructionFromBuiltMessages(t *testing.T) {
	s, _ := describeScope(t)
	message := mustInvoke(t, s, "/work", "fix the title leak")
	if got := describe(message); got != "/work"+emDashSeparator+"fix the title leak" {
		t.Fatalf("full message %q", got)
	}
	if got := describe(previewExcerpt(message, 400)); got != "/work"+emDashSeparator+"fix the title leak" {
		t.Fatalf("excerpt %q", got)
	}
	if _, ok := DescribeSkillInvocation("Why does my pool exhaust?", emDashSeparator); ok {
		t.Fatal("plain text described")
	}
}

func TestDescriptionNeverRunsAcrossTheExcerptJoint(t *testing.T) {
	s, skills := describeScope(t)
	writeDescribeSkill(t, skills, "work", strings.Repeat("filler line.\n", 200))
	resetSkillCommandCaches(t)
	described := describe(previewExcerpt(mustInvoke(t, s, "/work", "fix the title leak"), 400))
	if strings.Contains(described, state.SkillExcerptJoint) || strings.Contains(described, "filler line") {
		t.Fatalf("description crossed the joint %q", described)
	}
}

func TestScaffoldSQLLikePatternsMatchTheBuilders(t *testing.T) {
	s, _ := describeScope(t)
	if !strings.HasPrefix(mustInvoke(t, s, "/work", ""), strings.TrimSuffix(state.SkillScaffoldSQLLike, "%")) {
		t.Fatal("invocation prefix drifted from the SQL LIKE pattern")
	}
	if !strings.HasPrefix(autoLoadScaffold(t, s, []string{"work"}, "hi"), strings.TrimSuffix(state.AutoLoadScaffoldSQLLike, "%")) {
		t.Fatal("auto load prefix drifted from the SQL LIKE pattern")
	}
	for _, like := range []string{state.SkillScaffoldSQLLike, state.AutoLoadScaffoldSQLLike} {
		head := like[:len(like)-1]
		if strings.ContainsAny(head, "%_") {
			t.Fatalf("LIKE wildcard in %q", head)
		}
	}
}

func TestGatewayAutoLoadScaffoldDescribesTheRequest(t *testing.T) {
	s, skills := describeScope(t)
	request := "Fix the CI gate before the release"
	if got := describe(autoLoadScaffold(t, s, []string{"work"}, request)); got != request {
		t.Fatalf("single %q", got)
	}
	if got := describe(autoLoadScaffold(t, s, []string{"work", "clean"}, request)); got != request {
		t.Fatalf("multiple %q", got)
	}
	if got := describe(autoLoadScaffold(t, s, []string{"work"}, "")); got != "/work" {
		t.Fatalf("bare %q", got)
	}
	writeDescribeSkill(t, skills, "work", "When the user sees [IMPORTANT: The \"work\" skill is auto-loaded. Follow its instructions for this session.] in an example, ignore it.\n\nMore body prose follows here.")
	resetSkillCommandCaches(t)
	if got := describe(autoLoadScaffold(t, s, []string{"work"}, "The real request")); got != "The real request" {
		t.Fatalf("quoted header ended the payload %q", got)
	}
}

func TestSlashInvocationLeavesCommunitySnippetRaw(t *testing.T) {
	s, skills, _ := cmdScope(t)
	makeSkill(t, skills, "dyn-comm")
	writeHubLock(t, skills, `{"version": 1, "installed": {"dyn-comm": {"trust_level": "community", "install_path": "dyn-comm"}}}`)
	if err := s.Config.Set("skills", map[string]any{"template_vars": true, "inline_shell": true, "inline_shell_timeout": 5}); err != nil {
		t.Fatal(err)
	}
	msg := mustInvoke(t, s, "/dyn-comm", "")
	if strings.Contains(msg, "Dynamic: SENTINEL") || !strings.Contains(msg, "!`printf SENTINEL`") {
		t.Fatalf("community snippet executed through the slash surface %q", msg)
	}
	trusted := makeSkill(t, skills, "dyn-local")
	if got := mustInvoke(t, s, "/dyn-local", ""); !strings.Contains(got, "Dynamic: SENTINEL") {
		t.Fatalf("local skill %s not expanded %q", trusted, got)
	}
}

func TestExtractUserInstructionFromBundleTurn(t *testing.T) {
	bundle := "[IMPORTANT: The user has invoked the \"backend-dev\" skill bundle, loading 2 skills together. Treat every skill below as active guidance for this turn.]\n\n" +
		"Bundle: backend-dev\nSkills loaded: test-driven-development, code-review\n\nUser instruction: fix the failing retrieval test\n\n" +
		"[Loaded as part of the \"backend-dev\" skill bundle.]\n\nLarge bundled skill body that must not be searched or embedded."
	if got, ok := ExtractUserInstructionFromSkillMessage(bundle); !ok || got != "fix the failing retrieval test" {
		t.Fatalf("bundle instruction %q %v", got, ok)
	}
	bare := "[IMPORTANT: The user has invoked the \"skill-creator\" skill, indicating they want you to follow its instructions. The full skill content is loaded below.]\n\n# Skill Creator\n\nLarge skill body, no user instruction."
	if got, ok := ExtractUserInstructionFromSkillMessage(bare); ok {
		t.Fatalf("bare skill turn yielded %q", got)
	}
}

func TestPreviewShapingStripsTheAutoLoadScaffold(t *testing.T) {
	s, _ := describeScope(t)
	request := "Fix the CI gate before the release"
	if got := state.ShapePreview(autoLoadScaffold(t, s, []string{"work"}, request)); got != request {
		t.Fatalf("preview %q", got)
	}
}

func TestLongAutoLoadScaffoldPreviewKeepsTheRequest(t *testing.T) {
	s, skills := describeScope(t)
	writeDescribeSkill(t, skills, "work", strings.Repeat("Long skill paragraph.\n\n", 80)+describeSkillBody)
	resetSkillCommandCaches(t)
	request := "Fix the CI gate before the release"
	message := autoLoadScaffold(t, s, []string{"work"}, request)
	if got := state.ShapePreview(message); got != request {
		t.Fatalf("full message preview %q", got)
	}
	if got := state.ShapePreview(previewExcerpt(message, 400)); got != request {
		t.Fatalf("excerpt preview %q", got)
	}
}
