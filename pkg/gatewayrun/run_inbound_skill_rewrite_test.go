package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Home holding the named skills and the given config text
func installRewriteSkills(t *testing.T, config string, names ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	if config != "" {
		if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range names {
		dir := filepath.Join(home, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: rewrite fixture " + name + "\n---\n\nBody marker of " + name + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Runs the rewrite of one typed command line from a Telegram group chat
func rewriteSkillCommand(text string) (*gateway.MessageEvent, *string) {
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "-1001", ChatType: "group"}
	event := &gateway.MessageEvent{Text: text, Source: source, AllowGatewayControl: true}
	command, _ := event.GetCommand()
	reply := allocGatewayRunner().hmSkillSlashRewrite(context.Background(), event, source, "agent:main:telegram:group:-1001", command)
	return event, reply
}

func TestSkillSlashRewritePutsSkillPromptOnEventText(t *testing.T) {
	installRewriteSkills(t, "", "alpha-skill")
	event, reply := rewriteSkillCommand("/alpha-skill do the thing")
	if reply != nil {
		t.Fatalf("reply %q", *reply)
	}
	if !strings.Contains(event.Text, "Body marker of alpha-skill") || !strings.Contains(event.Text, "do the thing") {
		t.Fatalf("event text %q", event.Text)
	}
}

func TestSkillSlashRewriteResolvesTelegramUnderscoreSpelling(t *testing.T) {
	installRewriteSkills(t, "", "alpha-skill")
	event, reply := rewriteSkillCommand("/alpha_skill")
	if reply != nil {
		t.Fatalf("reply %q", *reply)
	}
	if !strings.Contains(event.Text, "Body marker of alpha-skill") {
		t.Fatalf("event text %q", event.Text)
	}
}

func TestSkillSlashRewriteLoadsEveryStackedSkill(t *testing.T) {
	installRewriteSkills(t, "", "alpha-skill", "beta-skill")
	event, reply := rewriteSkillCommand("/alpha-skill /beta-skill do XYZ")
	if reply != nil {
		t.Fatalf("reply %q", *reply)
	}
	for _, want := range []string{"Body marker of alpha-skill", "Body marker of beta-skill", "do XYZ"} {
		if !strings.Contains(event.Text, want) {
			t.Fatalf("event text lacks %q: %q", want, event.Text)
		}
	}
}

func TestSkillSlashRewriteRefusesSkillDisabledForPlatform(t *testing.T) {
	installRewriteSkills(t, "skills:\n  platform_disabled:\n    telegram:\n      - alpha-skill\n", "alpha-skill")
	event, reply := rewriteSkillCommand("/alpha-skill do the thing")
	want := "The **alpha-skill** skill is disabled for telegram.\nEnable it with: `icebath skills config`"
	if reply == nil || *reply != want {
		t.Fatalf("reply %v", reply)
	}
	if event.Text != "/alpha-skill do the thing" {
		t.Fatalf("event text rewritten to %q", event.Text)
	}
}

func TestSkillSlashRewriteRefusesStackedSkillDisabledForPlatform(t *testing.T) {
	installRewriteSkills(t, "skills:\n  platform_disabled:\n    telegram:\n      - beta-skill\n", "alpha-skill", "beta-skill")
	_, reply := rewriteSkillCommand("/alpha-skill /beta-skill do XYZ")
	want := "The **beta-skill** skill(s) in this stacked invocation are disabled for telegram.\nEnable them with: `icebath skills config`"
	if reply == nil || *reply != want {
		t.Fatalf("reply %v", reply)
	}
}

func TestSkillSlashRewriteAnswersUnknownCommand(t *testing.T) {
	installRewriteSkills(t, "", "alpha-skill")
	event, reply := rewriteSkillCommand("/zzqx-nothing hello")
	want := "Unknown command `/zzqx-nothing`. Type /commands to see what's available, or resend without the leading slash to send as a regular message."
	if reply == nil || *reply != want {
		t.Fatalf("reply %v", reply)
	}
	if event.Text != "/zzqx-nothing hello" {
		t.Fatalf("event text rewritten to %q", event.Text)
	}
}
