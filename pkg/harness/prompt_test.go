package harness

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/protocol"
)

func TestPromptCarriesEnvironmentAndConfiguredText(t *testing.T) {
	f := Facts{Dir: "/w", Root: "/r", Git: true, Platform: "linux", Shell: "zsh", OS: "Linux 7", Date: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), Model: "freddie"}
	p := Prompt(f, " be brief ")
	for _, want := range []string{"You are icebath", "Working directory: /w", "File tools stay inside: /r", "Git repository: true", "Platform: linux", "Shell: zsh", "OS version: Linux 7", "Today's date: 2026-10-20", "Model: freddie", "No mission is attached", "# Instructions from the configuration\nbe brief", "Tool results arrive as tool messages"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.HasSuffix(p, "\n") {
		t.Fatal("trailing newline")
	}
	f.Mission, f.Root = true, "/w"
	p = Prompt(f, "")
	if !strings.Contains(p, "A mission is attached") || strings.Contains(p, "stay inside") || strings.Contains(p, "Instructions from") {
		t.Fatalf("%s", p)
	}
	dir := t.TempDir()
	if inGitRepo(dir) {
		t.Fatal("temp dir is not a repo")
	}
	g := GatherFacts(dir, dir, "m", false)
	if g.Platform == "" || g.Date.IsZero() || g.Model != "m" || g.Dir != dir {
		t.Fatalf("%+v", g)
	}
}

func TestMissionToolsHookHidesPlanWithoutAMission(t *testing.T) {
	req := protocol.Request{Tools: []protocol.Tool{{Name: "Bash"}, {Name: mission.ToolName}, {Name: "Read"}}}
	if err := (missionTools{}).BeforeTurn(context.Background(), &req); err != nil || len(req.Tools) != 2 || req.Tools[1].Name != "Read" {
		t.Fatalf("%v %+v", err, req.Tools)
	}
	req = protocol.Request{Tools: []protocol.Tool{{Name: mission.ToolName}}}
	if err := (missionTools{}).BeforeTurn(mission.WithID(context.Background(), "m1"), &req); err != nil || len(req.Tools) != 1 {
		t.Fatalf("%v %+v", err, req.Tools)
	}
}
