package app

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// Reports whether stdin is a usable interactive terminal
func IsInteractiveStdin() bool {
	c := console()
	return c.StdinIsTTY()
}

// Prints the headless setup guidance with the config commands replacing the interactive wizard
func PrintNoninteractiveSetupGuidance(reason string) {
	c := console()
	fmt.Fprintln(c.Out)
	fmt.Fprintln(c.Out, c.Color("☤ Icebath Setup — Non-interactive mode", cliui.Cyan, cliui.Bold))
	fmt.Fprintln(c.Out)
	if reason != "" {
		c.PrintInfo(reason)
	}
	infoLines(c, "The interactive wizard cannot be used here.", "",
		"Configure Icebath using environment variables or config commands:",
		"  icebath config set model.provider custom",
		"  icebath config set model.base_url http://localhost:8080/v1",
		"  icebath config set model.default your-model-name", "",
		"Or set OPENROUTER_API_KEY (OpenRouter) / OPENAI_API_KEY (OpenAI) in your environment.",
		"Run 'icebath setup' in an interactive terminal to use the full wizard.", "")
}

// Prints each line as info in order, an empty entry printing a bare blank line
func infoLines(c *cliui.Console, lines ...string) {
	for _, line := range lines {
		if line == "" {
			fmt.Fprintln(c.Out)
			continue
		}
		c.PrintInfo(line)
	}
}
