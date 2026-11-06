package plugins

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Prints after-install.md in a green panel, or the installed location panel unless brief asks for silence
func displayAfterInstall(console *Console, pluginDir, identifier string, brief bool) error {
	afterInstall := filepath.Join(pluginDir, "after-install.md")
	_, statErr := os.Stat(afterInstall)
	exists := statErr == nil
	if brief && !exists {
		return nil
	}
	panel := pyrich.Panel{BorderStyle: "green", Fit: true}
	if exists {
		raw, err := os.ReadFile(afterInstall)
		if err != nil {
			return err
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			return err
		}
		panel.Content = &pyrich.Markdown{Source: pystr.TranslateNewlines(text)}
	} else {
		panel.Body = fmt.Sprintf("[green bold]Plugin installed:[/] %s\n[dim]Location:[/] %s", identifier, pluginDir)
		panel.Title = "✓ Installed"
	}
	fmt.Fprintln(console.Out)
	fmt.Fprintln(console.Out, panel.Render(console.Color, console.width()))
	fmt.Fprintln(console.Out)
	return nil
}
