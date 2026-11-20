package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const inlineSnippet = "Dynamic: !`printf SENTINEL`"

var inlineCfg = map[string]any{"inline_shell": true, "inline_shell_timeout": 5}

// Writes a minimal SKILL.md under a skills tree
func makeSkill(t *testing.T, skillsDir, rel string) string {
	t.Helper()
	dir := filepath.Join(skillsDir, filepath.FromSlash(rel))
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+filepath.Base(dir)+"\ndescription: d\n---\n"+inlineSnippet+"\n"), 0o644)
	return dir
}

// Writes a hub lock with the given installed table
func writeHubLock(t *testing.T, skillsDir string, text string) {
	t.Helper()
	os.MkdirAll(filepath.Join(skillsDir, ".hub"), 0o755)
	os.WriteFile(filepath.Join(skillsDir, ".hub", "lock.json"), []byte(text), 0o644)
}

func TestRunInlineShellFailureMarkers(t *testing.T) {
	s := NewScope(t.TempDir(), nil)
	dir := t.TempDir()
	if out := s.RunInlineShell(context.Background(), "exit 3", dir, 5); strings.TrimSpace(out) == "" || !strings.Contains(out, "3") {
		t.Fatalf("silent nonzero exit %q", out)
	}
	if out := s.RunInlineShell(context.Background(), `printf "diag" >&2; exit 3`, dir, 5); out != "diag" {
		t.Fatalf("stderr diagnostic %q", out)
	}
	if out := s.RunInlineShell(context.Background(), "sleep 5", dir, 1); out != "[inline-shell timeout after 1s: sleep 5]" {
		t.Fatalf("timeout marker %q", out)
	}
}

func TestSubstituteTemplateVars(t *testing.T) {
	got := SubstituteTemplateVars("a ${ICEBATH_SKILL_DIR} b ${HERMES_SESSION_ID} c ${ICEBATH_SESSION_ID}", "/sk", "")
	if got != "a /sk b ${HERMES_SESSION_ID} c ${ICEBATH_SESSION_ID}" {
		t.Fatalf("template %q", got)
	}
}

func TestInlineShellTrustByHubProvenance(t *testing.T) {
	h := t.TempDir()
	s := NewScope(h, nil)
	skills := s.SkillsDir()
	ctx := context.Background()
	community := makeSkill(t, skills, "comm")
	trusted := makeSkill(t, skills, "trusted")
	local := makeSkill(t, skills, "local")
	lock, _ := json.Marshal(map[string]any{"version": 1, "installed": map[string]any{
		"comm":    map[string]any{"trust_level": "community", "install_path": "comm"},
		"trusted": map[string]any{"trust_level": "trusted", "install_path": "trusted"}}})
	writeHubLock(t, skills, string(lock))
	if got := s.PreprocessSkillContent(ctx, inlineSnippet, community, "", inlineCfg); got != inlineSnippet {
		t.Fatalf("community expanded %q", got)
	}
	for _, d := range []string{trusted, local} {
		if got := s.PreprocessSkillContent(ctx, inlineSnippet, d, "", inlineCfg); got != "Dynamic: SENTINEL" {
			t.Fatalf("%s not expanded %q", d, got)
		}
	}
}

func TestInlineShellProvenanceFollowsOwningLock(t *testing.T) {
	cases := []struct {
		lock, viewer, rel string
		expands           bool
	}{
		{"", "owner", "outer/agents/inner", false},
		{"", "other", "outer", false},
		{"", "other", "outer/agents/inner", false},
		{`{"version": 1, "inst`, "owner", "outer", false},
		{`{"version": 1, "installed": []}`, "other", "outer", false},
		{"", "owner", "mine", true},
		{"", "other", "mine", true},
	}
	for _, c := range cases {
		root := t.TempDir()
		owner := filepath.Join(root, "owner", "skills")
		for _, rel := range []string{"outer", "outer/agents/inner", "mine"} {
			makeSkill(t, owner, rel)
		}
		lock := c.lock
		if lock == "" {
			lock = `{"version": 1, "installed": {"outer": {"trust_level": "community", "install_path": "outer"}}}`
		}
		writeHubLock(t, owner, lock)
		active := owner
		if c.viewer == "other" {
			active = filepath.Join(root, "other", "skills")
			os.MkdirAll(active, 0o755)
		}
		s := NewScope(filepath.Dir(active), nil)
		s.SkillsDirOverride = active
		got := s.PreprocessSkillContent(context.Background(), inlineSnippet, filepath.Join(owner, filepath.FromSlash(c.rel)), "", inlineCfg)
		want := inlineSnippet
		if c.expands {
			want = "Dynamic: SENTINEL"
		}
		if got != want {
			t.Errorf("%+v rendered %q", c, got)
		}
	}
}

func TestPreprocessBadTimeoutServesRaw(t *testing.T) {
	s := NewScope(t.TempDir(), nil)
	raw := "dir ${ICEBATH_SKILL_DIR} " + inlineSnippet
	got := s.PreprocessSkillContent(context.Background(), raw, t.TempDir(), "", map[string]any{"inline_shell": true, "inline_shell_timeout": "soon"})
	if got != raw {
		t.Fatalf("bad timeout rendered %q", got)
	}
}
