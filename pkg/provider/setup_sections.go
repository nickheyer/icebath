package provider

import (
	"github.com/nickheyer/icebath/pkg/tools"
)

// Setup console of the terminal backend section, the setup prompts and prints
type setupTerminalUI struct{}

func (setupTerminalUI) Prompt(question, def string, password bool) string {
	return SetupPrompt(question, def, password)
}
func (setupTerminalUI) PromptYesNo(question string, def bool) bool {
	return PromptYesNo(question, def, "")
}
func (setupTerminalUI) PromptChoice(question string, choices []string, def int) int {
	return PromptChoice(question, choices, def, "")
}
func (setupTerminalUI) PrintHeader(title string) { PrintSetupHeader(title, false) }
func (setupTerminalUI) PrintInfo(text string)    { SetupPrintInfo(text) }
func (setupTerminalUI) PrintSuccess(text string) { SetupPrintSuccess(text) }
func (setupTerminalUI) PrintWarning(text string) { SetupPrintWarning(text) }
func (setupTerminalUI) Println()                 { setupPrintln("") }

// Installs the setup prompts as the console the terminal backend section of pkg/tools draws on
func init() { tools.SetupTerminalConsole = setupTerminalUI{} }
