package tools_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	_ "github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	_ "github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// A configurator run over a temp home with captured output and no input
func casesSetup(t *testing.T) (*tools.ToolsSetup, *bytes.Buffer) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	var out bytes.Buffer
	return &tools.ToolsSetup{Ctx: home.WithOverride(context.Background(), h), Con: &cliui.Console{In: strings.NewReader(""), Out: &out}}, &out
}

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
