package vsc

import (
	"path/filepath"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ext"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Palette and slash command installing the embedded extension into the editor on demand
func installCommand() ext.Command {
	return ext.Command{
		ID:    "vsc.install",
		Title: "VS Code: install extension",
		Slash: "vsc-install",
		Desc:  "install the icebath extension built into this binary through the code command of the editor",
		Run:   runInstall,
	}
}

// Installs the embedded extension in the background, a toast telling what happened
func runInstall(c *ext.Call) tea.Cmd {
	data, version, ok := bundledVSIX()
	if !ok {
		return c.UI.Toast("this build carries no VS Code extension, build it with make vsix", wm.Error)
	}
	ctx := c.Ctx
	return tea.Batch(c.UI.Toast("installing VS Code extension "+version, wm.Info), func() tea.Msg {
		cli, err := installExtension(ctx, data)
		if err != nil {
			return wm.Notify("VS Code extension: "+err.Error(), wm.Error)()
		}
		done := "VS Code extension " + version + " installed with " + filepath.Base(cli)
		if err := markInstalled(version); err != nil {
			return wm.Notify(done+", "+err.Error(), wm.Warn)()
		}
		return wm.Notify(done, wm.Success)()
	})
}
