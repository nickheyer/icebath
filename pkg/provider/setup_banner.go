package provider

import (
	"github.com/nickheyer/icebath/pkg/cliui"
)

// PrintSetupBanner prints the magenta box banner, top border, the body lines and bottom border
func PrintSetupBanner(lines ...string) {
	c := setupUI()
	setupPrintln("")
	setupPrintln(c.Color("┌─────────────────────────────────────────────────────────┐", cliui.Magenta))
	for _, line := range lines {
		setupPrintln(c.Color(line, cliui.Magenta))
	}
	setupPrintln(c.Color("└─────────────────────────────────────────────────────────┘", cliui.Magenta))
}
