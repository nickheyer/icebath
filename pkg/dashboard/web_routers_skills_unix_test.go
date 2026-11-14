//go:build !windows

package dashboard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestWebRouterProvenanceTwinRouterLabelsExternalVsAgent(t *testing.T) {
	c := newSessionClient(t)
	h := os.Getenv("ICEBATH_HOME")
	vault := filepath.Join(t.TempDir(), "vault")
	writeTestFile(t, filepath.Join(vault, "ext-skill", "SKILL.md"), "---\nname: ext-skill\ndescription: An external skill.\n---\n\n# External\n")
	if err := os.MkdirAll(filepath.Join(h, "skills", "ext-linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(vault, "ext-skill", "SKILL.md"), filepath.Join(h, "skills", "ext-linked", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(h, "config.yaml"), "skills:\n  external_dirs:\n    - "+vault+"\n")
	writeTestFile(t, filepath.Join(h, "skills", "my-local", "SKILL.md"), "---\nname: my-local\ndescription: Local skill.\n---\n\n# Local\n")
	resp := c.get("/api/skills")
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	byName := map[string]any{}
	for _, r := range pyjson.Plain(resp.JSON(t)).([]any) {
		row := r.(map[string]any)
		byName[row["name"].(string)] = row["provenance"]
	}
	if byName["ext-skill"] != "external" || byName["my-local"] != "agent" {
		t.Fatalf("provenance %v", byName)
	}
}
