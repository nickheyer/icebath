package commands

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins/verify"
	"github.com/spf13/cobra"
)

// Attaches the verify subcommand with every argument the verify package parser declares
func (c *CLI) addVerify(root *cobra.Command) {
	vp := verify.BuildVerifyParser()
	cmd := c.newCmd(root, vp.Name, vp.Help, vp.Description)
	for _, a := range vp.Arguments {
		if !strings.HasPrefix(a.Name, "-") {
			if a.Optional {
				c.pos(cmd, pOpt(a.Name))
			} else {
				c.pos(cmd, p(a.Name))
			}
			continue
		}
		name := strings.TrimLeft(a.Name, "-")
		switch {
		case a.Action == "store_true":
			bf(cmd, name, "", a.Help)
		case a.Action == "append":
			af(cmd, name, "", a.Help)
		case a.Type == "int":
			def, ok := a.Default.(int)
			inf(cmd, name, "", def, a.Help)
			if !ok {
				noneDefault(cmd, name)
			}
		case a.Type == "float":
			def, ok := a.Default.(float64)
			ff(cmd, name, "", def, a.Help)
			if !ok {
				noneDefault(cmd, name)
			}
		default:
			def, _ := a.Default.(string)
			sf(cmd, name, "", def, a.Help)
		}
		if len(a.Choices) > 0 {
			c.choice(cmd, name, a.Choices...)
		}
	}
	jsonFlag(cmd, vp.JSONHelp)
}
