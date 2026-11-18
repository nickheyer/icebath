package tui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
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
	show := func(res *tuigateway.DisplayStatus) tea.Cmd {
		if res != nil {
			a.panel(tr("screen.title"), screenSections(res))
		}
		return nil
	}
	switch action {
	case "status":
		return ask(r.rpc, r.sys, tuigateway.DisplayStatusMethod, tuigateway.ProfileParams{}, show)
	case "start":
		r.sys(tr("screen.starting"))
		return ask(r.rpc, r.sys, tuigateway.DisplayStart, tuigateway.ProfileParams{}, show)
	case "stop":
		force := len(fields) > 1 && fields[1] == "force"
		return ask(r.rpc, r.sys, tuigateway.DisplayStop, tuigateway.DisplayStopParams{Force: force}, func(res *tuigateway.DisplayStopResult) tea.Cmd {
			switch {
			case res == nil:
				return nil
			case res.Stopped:
				r.sys(tr("screen.stopped"))
				return nil
			}
			return show(&res.DisplayStatus)
		})
	case "install":
		return ask(r.rpc, r.sys, tuigateway.DisplayInstall, tuigateway.ProfileParams{}, func(res *tuigateway.DisplayInstallResult) tea.Cmd {
			switch {
			case res == nil:
			case res.Started:
				r.sys(tr("screen.installing"))
			default:
				r.sys(tr("screen.installByHand", res.Command))
			}
			return nil
		})
	case "switch", "keep":
		params := tuigateway.DisplaySwitchSandboxImageParams{Approve: action == "switch"}
		return ask(r.rpc, r.sys, tuigateway.DisplaySwitchSandboxImage, params, func(res *tuigateway.DisplaySwitchSandboxImageResult) tea.Cmd {
			if res == nil {
				return nil
			}
			r.sys(tr("screen.imageSet", res.DockerImage))
			return show(&res.DisplayStatus)
		})
	}
	r.sys(tr("screen.usage"))
	return nil
}

// Rows of a screen status, the runtime, the lease, a blocker and a pending sandbox image switch
func screenSections(s *tuigateway.DisplayStatus) []panelSection {
	yes := func(on bool) string {
		if on {
			return tr("screen.yes")
		}
		return tr("screen.no")
	}
	rows := [][2]string{{tr("screen.rowProfile"), s.Profile}, {tr("screen.rowSupported"), yes(s.Supported)},
		{tr("screen.rowInstalled"), yes(s.Installed)}, {tr("screen.rowRunning"), yes(s.Running)}}
	if len(s.Missing) > 0 {
		rows = append(rows, [2]string{tr("screen.rowMissing"), strings.Join(s.Missing, ", ")})
	}
	for _, f := range []struct{ value, key string }{{val(s.Display), "screen.rowDisplay"}, {s.Geometry, "screen.rowGeometry"}, {s.Placement, "screen.rowPlacement"}, {val(s.Browser), "screen.rowBrowser"}} {
		if f.value != "" {
			rows = append(rows, [2]string{tr(f.key), f.value})
		}
	}
	rows = append(rows, [2]string{tr("screen.rowControl"), screenHolder(s.Lease)})
	sections := []panelSection{{Rows: rows}}
	if blocker := val(s.Blocker); blocker != "" {
		sections = append(sections, panelSection{Text: &blocker})
	}
	if sw := s.ImageSwitch; sw != nil {
		text := tr("screen.switchPending", sw.CurrentImage, sw.TargetImage)
		sections = append(sections, panelSection{Text: &text})
	}
	return sections
}

// Who holds the screen, the person at the keyboard or the agent
func screenHolder(lease tuigateway.DisplayLease) string {
	if lease.Holder == "human" {
		return tr("screen.heldByPerson")
	}
	return tr("screen.heldByAgent")
}

// Status row line of a screen that started or stopped
func screenRunningNote(running bool) string {
	if running {
		return tr("screen.nowRunning")
	}
	return tr("screen.nowStopped")
}

// Transcript line of how the package install ended
func screenInstallDone(code int64) string {
	switch code {
	case 0:
		return tr("screen.installed")
	case -1:
		return tr("screen.installCancelled")
	case -2:
		return tr("screen.installNoSudo")
	}
	return tr("screen.installFailed", strconv.FormatInt(code, 10))
}
