package app

import (
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Reports whether the model in use supports the fast service tier
func (c *CLI) fastCommandAvailable() bool {
	model := c.model
	if a := c.agent; a != nil && a.Model() != "" {
		model = a.Model()
	}
	return provider.ModelSupportsFastMode(model)
}

// Reports whether a slash command applies to the current model
func (c *CLI) commandAvailable(slashCommand string) bool {
	if slashCommand == "/fast" {
		return c.fastCommandAvailable()
	}
	return true
}

// Reports a slash command of the named set typed without images while the agent runs
func (c *CLI) busyInlineCommand(text string, hasImages bool, names ...string) bool {
	if text == "" || hasImages || !looksLikeSlashCommand(text) {
		return false
	}
	if running, _ := c.busyState(); !running {
		return false
	}
	words := pystr.SplitWhitespace(text, 1)
	if len(words) == 0 {
		return false
	}
	cmd := commands.Resolve(pystr.LstripChars(pystr.Lower(words[0]), "/"))
	if cmd == nil {
		return false
	}
	for _, name := range names {
		if cmd.Name == name {
			return true
		}
	}
	return false
}

// Reports a steer or queue command to dispatch at once while the agent runs
func (c *CLI) shouldHandleSteerCommandInline(text string, hasImages bool) bool {
	return c.busyInlineCommand(text, hasImages, "steer", "queue")
}

// Reports a background or side question command to dispatch while the agent runs
func (c *CLI) shouldHandleBackgroundCommandInline(text string, hasImages bool) bool {
	return c.busyInlineCommand(text, hasImages, "bg", "btw")
}

// Reports a stop command to dispatch while the agent runs
func (c *CLI) shouldHandleStopCommandInline(text string, hasImages bool) bool {
	return c.busyInlineCommand(text, hasImages, "stop")
}

// Prints one discovery tip in the skin's dim banner colour, any failure printing nothing
func (c *CLI) printRandomTip() {
	defer func() { _ = recover() }()
	tip := getRandomTip(c.ctx)
	tipColor := "#B8860B"
	if skin := config.GetActiveSkin(c.ctx); skin != nil {
		tipColor = skin.GetColor("banner_dim", "#B8860B")
	}
	c.consolePrint("[dim " + tipColor + "]" + c.t("cli.tip_line", map[string]any{"tip": tip}) + "[/]")
}
