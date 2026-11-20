package tui

import (
	"crypto/rand"
	"encoding/base64"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Actions the screen command takes as its argument
var screenActions = []string{"status", "start", "stop", "take", "release", "shot", "install", "switch", "keep"}

// Rows a picture of the bot screen may take in the transcript, the room a desktop needs to be read
const screenShotRows = 30

// Bot screen of the profile, its status, start and stop, a picture of it, the takeover, the package install and the sandbox image switch
func (a *app) screenCommands() []slashCommand {
	return []slashCommand{{name: "screen", help: tr(txt.SlashCmdCoreHelpScreen), helpFn: helpOf(txt.SlashCmdCoreHelpScreen), usage: "[status|start|stop [force]|take [reason]|release [force]|shot|install|switch|keep]",
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
			a.panel(tr(txt.ScreenTitle), screenSections(res))
		}
		return nil
	}
	switch action {
	case "status":
		return ask(r.rpc, r.sys, tuigateway.DisplayStatusMethod, tuigateway.ProfileParams{}, show)
	case "start":
		r.sys(tr(txt.ScreenStarting))
		return ask(r.rpc, r.sys, tuigateway.DisplayStart, tuigateway.ProfileParams{}, show)
	case "stop":
		force := len(fields) > 1 && fields[1] == "force"
		return ask(r.rpc, r.sys, tuigateway.DisplayStop, tuigateway.DisplayStopParams{Force: force}, func(res *tuigateway.DisplayStopResult) tea.Cmd {
			switch {
			case res == nil:
				return nil
			case res.Stopped:
				r.sys(tr(txt.ScreenStopped))
				return nil
			}
			return show(&res.DisplayStatus)
		})
	case "take":
		reason := ""
		if words := strings.Fields(r.arg); len(words) > 1 {
			reason = strings.Join(words[1:], " ")
		}
		params := tuigateway.DisplayLeaseAcquireParams{ViewerID: a.screenViewerID(), Reason: reason}
		return ask(r.rpc, r.sys, tuigateway.DisplayLeaseAcquire, params, func(res *tuigateway.DisplayLeaseResult) tea.Cmd {
			if res != nil {
				r.sys(tr(txt.ScreenTaken))
			}
			return nil
		})
	case "release":
		params := tuigateway.DisplayLeaseReleaseParams{ViewerID: a.screenViewerID(), Force: len(fields) > 1 && fields[1] == "force"}
		return ask(r.rpc, r.sys, tuigateway.DisplayLeaseRelease, params, func(res *tuigateway.DisplayLeaseResult) tea.Cmd {
			if res != nil {
				r.sys(tr(txt.ScreenReleased))
			}
			return nil
		})
	case "shot":
		return ask(r.rpc, r.sys, tuigateway.DisplayThumbnail, tuigateway.ProfileParams{}, func(res *tuigateway.DisplayThumbnailResult) tea.Cmd {
			switch {
			case res == nil:
			case res.Suppressed != "":
				r.sys(tr(txt.ScreenShotWithheld))
			case val(res.Path) == "":
				r.sys(tr(txt.ScreenShotNotRunning))
			default:
				r.sys(tr(txt.ScreenShotSaved, *res.Path))
				a.appendMessage(msg{Kind: "image", Role: "system", Images: []string{*res.Path}, ImageRows: screenShotRows})
			}
			return nil
		})
	case "install":
		return ask(r.rpc, r.sys, tuigateway.DisplayInstall, tuigateway.ProfileParams{}, func(res *tuigateway.DisplayInstallResult) tea.Cmd {
			switch {
			case res == nil:
			case res.Started:
				r.sys(tr(txt.ScreenInstalling))
			default:
				r.sys(tr(txt.ScreenInstallByHand, res.Command))
			}
			return nil
		})
	case "switch", "keep":
		params := tuigateway.DisplaySwitchSandboxImageParams{Approve: action == "switch"}
		return ask(r.rpc, r.sys, tuigateway.DisplaySwitchSandboxImage, params, func(res *tuigateway.DisplaySwitchSandboxImageResult) tea.Cmd {
			if res == nil {
				return nil
			}
			r.sys(tr(txt.ScreenImageSet, res.DockerImage))
			return show(&res.DisplayStatus)
		})
	}
	r.sys(tr(txt.ScreenUsage))
	return nil
}

// Id this UI takes the bot screen under, random and kept for the life of the process so that only this UI hands back what it took
func (a *app) screenViewerID() string {
	if a.screenViewer == "" {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			panic(err)
		}
		a.screenViewer = base64.RawURLEncoding.EncodeToString(raw)
	}
	return a.screenViewer
}

// Rows of a screen status, the runtime, the lease, a blocker and a pending sandbox image switch
func screenSections(s *tuigateway.DisplayStatus) []panelSection {
	yes := func(on bool) string {
		if on {
			return tr(txt.ScreenYes)
		}
		return tr(txt.ScreenNo)
	}
	rows := [][2]string{{tr(txt.ScreenRowProfile), s.Profile}, {tr(txt.ScreenRowSupported), yes(s.Supported)},
		{tr(txt.ScreenRowInstalled), yes(s.Installed)}, {tr(txt.ScreenRowRunning), yes(s.Running)}}
	if len(s.Missing) > 0 {
		rows = append(rows, [2]string{tr(txt.ScreenRowMissing), strings.Join(s.Missing, ", ")})
	}
	for _, f := range []struct {
		value string
		key   txt.Key
	}{{val(s.Display), txt.ScreenRowDisplay}, {s.Geometry, txt.ScreenRowGeometry}, {s.Placement, txt.ScreenRowPlacement}, {val(s.Browser), txt.ScreenRowBrowser}} {
		if f.value != "" {
			rows = append(rows, [2]string{tr(f.key), f.value})
		}
	}
	rows = append(rows, [2]string{tr(txt.ScreenRowControl), screenHolder(s.Lease)})
	sections := []panelSection{{Rows: rows}}
	if blocker := val(s.Blocker); blocker != "" {
		sections = append(sections, panelSection{Text: &blocker})
	}
	if sw := s.ImageSwitch; sw != nil {
		text := tr(txt.ScreenSwitchPending, sw.CurrentImage, sw.TargetImage)
		sections = append(sections, panelSection{Text: &text})
	}
	return sections
}

// Who holds the screen, the person at the keyboard or the agent
func screenHolder(lease tuigateway.DisplayLease) string {
	if lease.Holder == "human" {
		return tr(txt.ScreenHeldByPerson)
	}
	return tr(txt.ScreenHeldByAgent)
}

// Status row line of a screen that started or stopped
func screenRunningNote(running bool) string {
	if running {
		return tr(txt.ScreenNowRunning)
	}
	return tr(txt.ScreenNowStopped)
}

// Transcript line of how the package install ended
func screenInstallDone(code int64) string {
	switch code {
	case 0:
		return tr(txt.ScreenInstalled)
	case -1:
		return tr(txt.ScreenInstallCancelled)
	case -2:
		return tr(txt.ScreenInstallNoSudo)
	}
	return tr(txt.ScreenInstallFailed, strconv.FormatInt(code, 10))
}
