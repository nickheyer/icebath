package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	approvalsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Asks for a command and shows what the approval rules do with it where this profile runs commands, the command never run
func (p *page) check() tea.Cmd {
	field := cards.Field{Label: "Command", Help: "The command to check, such as git push origin main. It is not run."}
	return p.kit.Ask(p.hub, "Check a command", field, "Check", func(command string) tea.Cmd {
		if command == "" {
			return nil
		}
		return tui.Call(p.hub, func() (*approvalsv1.TestResponse, error) {
			return p.service.Test(p.kit.Env.Ctx, &approvalsv1.TestRequest{Profile: p.profile, Command: command})
		}, func(v *approvalsv1.TestResponse) tea.Cmd {
			look := p.kit.Env.Theme
			lines := []string{render.Heading(v.GetCommand(), look), "", verdict(v.GetVerdict(), look)}
			if v.GetRule() != "" {
				lines = append(lines, "Decided by the rule: "+v.GetRule())
			}
			if v.GetDetail() != "" {
				lines = append(lines, render.Dim(v.GetDetail(), look))
			}
			lines = append(lines, render.Dim("Where it runs: "+where(v.GetBackend()), look))
			if len(v.GetVariants()) > 0 {
				lines = append(lines, "", "How the rules read it:")
				for _, variant := range v.GetVariants() {
					lines = append(lines, "  "+variant)
				}
			}
			return p.hub.Push(cards.NewPage(p.kit.Env, cards.PageSpec{Title: "Check a command", Key: "approvals:check", Lines: func(w int) []string {
				var out []string
				for _, line := range lines {
					out = append(out, strings.Split(render.Wrap(line, max(w, 8)), "\n")...)
				}
				return out
			}}))
		})
	})
}

// Where a command runs, in words
func where(backend string) string {
	switch backend {
	case "", "local":
		return "On this computer"
	case "docker":
		return "In a Docker sandbox"
	case "ssh":
		return "On another computer, over SSH"
	}
	return strs.Capital(backend)
}

// What the approval rules do with a command, as a sentence
func verdict(v string, look theme.Theme) string {
	switch v {
	case "allow":
		return render.Good("It runs without asking.", look)
	case "ask-approval":
		return render.Warm("The agent asks you before it runs.", look)
	case "hardline-deny":
		return render.Bad("It is always blocked by a rule built into icebath.", look)
	case "user-deny":
		return render.Bad("It is blocked by your Blocked commands.", look)
	}
	return strs.Capital(v) + "."
}
