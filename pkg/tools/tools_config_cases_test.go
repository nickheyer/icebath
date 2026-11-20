package tools_test

import (
	"testing"

	_ "github.com/nickheyer/icebath/pkg/browser"
	_ "github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestPlatformToolsetConsistencySkillsConfigCoversToolsConfigPlatforms(t *testing.T) {
	for _, p := range tools.Platforms {
		if p.Key == "api_server" {
			continue
		}
		if _, ok := skills.SkillsConfigPlatformLabel(p.Key); !ok {
			t.Fatalf("Platform %q in tools_config but missing from skills_config PLATFORMS", p.Key)
		}
	}
}
