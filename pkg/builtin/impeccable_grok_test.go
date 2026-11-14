package builtin

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestImpeccableGrokManifestRendersFromClaudeManifest(t *testing.T) {
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pyjson.Loads(string(must(os.ReadFile(tree.Payload("impeccable/plugin/.claude-plugin/plugin.json")))))
	claude, ok := parsed.(*pyjson.Object)
	if err != nil || !ok {
		t.Fatal(err, parsed)
	}
	get := func(key string) any { v, _ := claude.Get(key); return v }
	license := get("license")
	if s, _ := license.(string); s == "" {
		license = "MIT"
	}
	grok := pyjson.ObjectOf("name", get("name"), "version", get("version"), "description", get("description"),
		"author", get("author"), "homepage", get("homepage"), "repository", get("repository"), "license", license,
		"keywords", []any{"design", "frontend", "ui", "ux", "skills", "hooks"})
	want, err := pyjson.Dumps(grok, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(must(os.ReadFile(tree.Payload("impeccable/plugin/.grok-plugin/plugin.json")))); got != want+"\n" {
		t.Fatalf("grok manifest\n%s\nrendered\n%s", got, want)
	}
	if _, hasAgents := claude.Get("agents"); hasAgents || get("skills") != "./skills/" {
		t.Fatalf("claude plugin manifest skills %v", get("skills"))
	}
}
