package skills

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestInjectSkillConfigLeavesPartsAloneWithoutDeclaredConfig(t *testing.T) {
	s := NewScope(t.TempDir(), config.Defaults())
	parts := []string{"prompt"}
	s.InjectSkillConfig(map[string]any{"content": "---\nname: plain\ndescription: d\n---\nbody\n"}, &parts)
	if len(parts) != 1 || parts[0] != "prompt" {
		t.Fatalf("parts changed without declared config: %q", parts)
	}
}

func TestInjectSkillConfigAppendsTheResolvedBlock(t *testing.T) {
	s := NewScope(t.TempDir(), config.Defaults())
	content := "---\nname: cfg\ndescription: d\nmetadata:\n  hermes:\n    config:\n      - key: region\n        description: the region\n        default: us-east-1\n---\nbody\n"
	parts := []string{"prompt"}
	s.InjectSkillConfig(map[string]any{"content": content}, &parts)
	joined := strings.Join(parts, "\n")
	if !strings.Contains(joined, "[Skill config (from ") || !strings.Contains(joined, "  region = us-east-1") || parts[len(parts)-1] != "]" {
		t.Fatalf("resolved block missing: %q", parts)
	}
}
