package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Actions the screen command takes as its argument
var screenActions = []string{"status", "start", "stop", "install", "switch", "keep"}

// Bot screen of the profile, its status, start and stop, the package install and the sandbox image switch
func (a *app) screenCommands() []slashCommand {
	return []slashCommand{{name: "screen", help: tr("slashCmd.core.help.screen"), usage: "[status|start|stop [force]|install|switch|keep]",
		suggest: func(string) []string { return screenActions }, run: a.runScreen}}
}

func (a *app) runScreen(r *slashRun) tea.Cmd {
	fields := strings.Fields(strings.ToLower(r.arg))
	action := "status"
	if len(fields) > 0 {
		action = fields[0]
	}
	show := func(res map[string]any) tea.Cmd {
		a.panel(tr("screen.title"), screenSections(res))
		return nil
	}
	switch action {
	case "status":
		return r.rpc("display.status", map[string]any{}, show)
	case "start":
		r.sys(tr("screen.starting"))
		return r.rpc("display.start", map[string]any{}, show)
	case "stop":
		force := len(fields) > 1 && fields[1] == "force"
		return r.rpc("display.stop", map[string]any{"force": force}, func(res map[string]any) tea.Cmd {
			if truthyJS(res["stopped"]) {
				r.sys(tr("screen.stopped"))
				return nil
			}
			return show(res)
		})
	case "install":
		return r.rpc("display.install", map[string]any{}, func(res map[string]any) tea.Cmd {
			if truthyJS(res["started"]) {
				r.sys(tr("screen.installing"))
				return nil
			}
			r.sys(tr("screen.installByHand", stringOr(res["command"], "")))
			return nil
		})
	case "switch", "keep":
		return r.rpc("display.switchSandboxImage", map[string]any{"approve": action == "switch"}, func(res map[string]any) tea.Cmd {
			r.sys(tr("screen.imageSet", stringOr(res["docker_image"], "")))
			return show(res)
		})
	}
	r.sys(tr("screen.usage"))
	return nil
}

// Rows of a screen status, the runtime, the lease, a blocker and a pending sandbox image switch
func screenSections(s map[string]any) []panelSection {
	yes := func(key string) string {
		if truthyJS(s[key]) {
			return tr("screen.yes")
		}
		return tr("screen.no")
	}
	rows := [][2]string{{tr("screen.rowProfile"), stringOr(s["profile"], "")}, {tr("screen.rowSupported"), yes("supported")},
		{tr("screen.rowInstalled"), yes("installed")}, {tr("screen.rowRunning"), yes("running")}}
	if missing := stringList(s["missing"]); len(missing) > 0 {
		rows = append(rows, [2]string{tr("screen.rowMissing"), strings.Join(missing, ", ")})
	}
	for _, f := range [][2]string{{"display", "screen.rowDisplay"}, {"geometry", "screen.rowGeometry"}, {"placement", "screen.rowPlacement"}, {"browser", "screen.rowBrowser"}} {
		if v := stringOr(s[f[0]], ""); v != "" {
			rows = append(rows, [2]string{tr(f[1]), v})
		}
	}
	if lease, _ := s["lease"].(map[string]any); lease != nil {
		rows = append(rows, [2]string{tr("screen.rowControl"), screenHolder(lease)})
	}
	sections := []panelSection{{Rows: rows}}
	if blocker := stringOr(s["blocker"], ""); blocker != "" {
		sections = append(sections, panelSection{Text: &blocker})
	}
	if sw, _ := s["image_switch"].(map[string]any); sw != nil {
		text := tr("screen.switchPending", stringOr(sw["current_image"], ""), stringOr(sw["target_image"], ""))
		sections = append(sections, panelSection{Text: &text})
	}
	return sections
}

// Who holds the screen, the person at the keyboard or the agent
func screenHolder(lease map[string]any) string {
	if lease["holder"] == "human" {
		return tr("screen.heldByPerson")
	}
	return tr("screen.heldByAgent")
}

// Line of a screen event for the status row, empty for an event of no screen
func screenEventNote(ev gatewayEventMsg) string {
	p := ev.Payload
	switch ev.Type {
	case "display.status":
		if truthyJS(p["running"]) {
			return tr("screen.nowRunning")
		}
		return tr("screen.nowStopped")
	case "display.lease":
		lease, _ := p["lease"].(map[string]any)
		return tr("screen.control", screenHolder(lease))
	}
	return ""
}

// Transcript line of the package install, a line of its output or how it ended
func screenInstallLine(ev gatewayEventMsg) string {
	p := ev.Payload
	if ev.Type == "display.install.log" {
		return "  " + stringOr(p["line"], "")
	}
	switch code, _ := p["code"].(float64); code {
	case 0:
		return tr("screen.installed")
	case -1:
		return tr("screen.installCancelled")
	case -2:
		return tr("screen.installNoSudo")
	default:
		return tr("screen.installFailed", jsString(code))
	}
}
