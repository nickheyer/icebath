package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/profile"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1/profilev1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Asks where to save a profile and exports it there, the job finishing once it is saved
func (p *profiles) export(row *profilev1.Profile) tea.Cmd {
	if row == nil {
		return nil
	}
	name := row.GetName()
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title: "Export " + name,
		Fields: []cards.Field{{ID: "file", Label: "File", Path: cards.PathFile,
			Help: "Where to save it, such as ~/" + name + ".tar.gz. Tab completes it.\nLeave it empty to save it in the folder icebath keeps exports in."}},
		Submit: "Export",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			output := strings.TrimSpace(values["file"])
			p.hub.Busy("Exporting " + name + "…")
			return tui.Call(p.hub, func() ([2]string, error) {
				exported, path, err := write(p.kit, p.service, name, output)
				return [2]string{exported, path}, err
			}, func(done [2]string) tea.Cmd {
				return tea.Batch(p.hub.Done("Exported "+done[0]+" to "+profile.Tilde(done[1])+"."), p.hub.Finish(form))
			})
		},
	})
	return p.hub.Push(form)
}

// Writes a profile's archive where asked, else in the folder exports go to: its name and where it went
func write(kit *tui.Kit, service profilev1connect.ProfileServiceClient, name, output string) (string, string, error) {
	exported, err := service.Export(kit.Env.Ctx, &profilev1.ExportRequest{Name: name})
	if err != nil {
		return "", "", err
	}
	path, err := profile.SaveArchive(exported.GetArchive(), exported.GetFileName(), output)
	return exported.GetName(), path, err
}

// /export: alone, the job of exporting the profile in use on the page of Profiles; with a profile and -o with a file, the export itself,
// said in one line
func exportSlash(kit *tui.Kit, service profilev1connect.ProfileServiceClient, argument string) tea.Cmd {
	if strings.TrimSpace(argument) == "" {
		return kit.Open(Screen, atExport)
	}
	const usage = "/export takes a profile and -o with the file to write, such as /export work -o ~/work.tar.gz."
	// Split as a shell splits it, so a file whose name holds a space is given in quotes
	words, err := strs.Shell(argument)
	if err != nil {
		return kit.WM.Toast(usage, wm.Warn, wm.DefaultToastTTL)
	}
	name, output := "", ""
	for i := 0; i < len(words); i++ {
		switch {
		case words[i] == "-o":
			if i+1 >= len(words) {
				return kit.WM.Toast(usage, wm.Warn, wm.DefaultToastTTL)
			}
			output = words[i+1]
			i++
		case name == "":
			name = words[i]
		default:
			return kit.WM.Toast(usage, wm.Warn, wm.DefaultToastTTL)
		}
	}
	return tui.Async(func() func() tea.Cmd {
		exported, path, err := write(kit, service, name, output)
		return func() tea.Cmd {
			if err != nil {
				return kit.WM.Toast("The export failed. "+strs.Full(tui.Reason(err)), wm.Error, wm.DefaultToastTTL)
			}
			return kit.WM.Toast("Exported "+exported+" to "+profile.Tilde(path)+".", wm.Success, wm.DefaultToastTTL)
		}
	})
}
