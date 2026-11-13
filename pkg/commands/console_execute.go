package commands

// Parses argv the way a bare argparse parser would and runs the bound handler, skipping the profile scan
func (c *CLI) ExecuteParsed(argv []string) error {
	argv, c.mcpAddArgs = rewriteMCPAddArgs(c.joinOptionalValues(argv))
	argv = c.expandNargsPlus(argv)
	c.argv = argv
	c.Root.SetArgs(argv)
	return argparseFlagError(c.Root, c.Root.Execute())
}
