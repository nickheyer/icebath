package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
)

func TestResumeHistoryPanelShowsEarlierCountLabelsAndTheLastReplyInFull(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("COLUMNS", "80")
	t.Setenv("LINES", "24")
	out := &bytes.Buffer{}
	c := &CLI{ctx: context.Background(), console: &cliui.Console{In: strings.NewReader(""), Out: out}}
	paintResumeHistoryPanel(c, []resumeEntry{{"user", "first question\nsecond line"}, {"event", "background build finished"},
		{"assistant_last", "the final answer [with brackets]"}}, 4, [4]string{"#B8860B", "#DAA520", "#8B8682", "#8FBC8F"})
	text := out.String()
	for _, want := range []string{c.t("cli.resume.panel_title", nil), c.t("cli.resume.earlier_messages", map[string]any{"count": 4}),
		strings.TrimSpace(c.t("cli.resume.label_you", nil)) + " first question", "second line", "◈ background build finished", "the final answer [with brackets]", "╭", "╰"} {
		if !strings.Contains(text, want) {
			t.Fatalf("panel lacks %q:\n%s", want, text)
		}
	}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if n := len([]rune(line)); n != 80 {
			t.Fatalf("panel row is %d cells wide: %q", n, line)
		}
	}
}
