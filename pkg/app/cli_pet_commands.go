//go:build !icebath_core && !icebath_noextras

package app

import (
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pet/generate"
)

// Adds /pet and /hatch to the slash dispatch
func init() {
	slashDispatch["pet"] = withArg((*CLI).handlePetCommand)
	slashDispatch["hatch"] = withArg((*CLI).handleHatchCommand)
}

// Pets runner bound to this session's profile writing to the console
func (c *CLI) petsRunner() *petsRun {
	return &petsRun{ctx: c.ctx, h: home.Dir(c.ctx), w: console().Out, env: c.env}
}

// Handles /pet toggle, list, scale, off or adopting a petdex pet by slug
func (c *CLI) handlePetCommand(cmd string) {
	say := func(line string) { printLines(line) }
	if err := c.petsRunner().core().Slash(commandArg(cmd, false), c.commandsT, say); err != nil {
		panic(err)
	}
}

// Handles /hatch, asking for the concept when none was typed
func (c *CLI) handleHatchCommand(cmd string) {
	concept := commandArg(cmd, false)
	if concept == "" {
		raw, _ := c.promptTextInput(c.commandsT("hatch.describe_prompt", nil))
		concept = pystr.Strip(raw)
	}
	say := func(line string) { printLines(line) }
	if err := generate.HatchCommand(c.ctx, c.env, home.Dir(c.ctx), concept, c.commandsT, say); err != nil {
		panic(err)
	}
}
