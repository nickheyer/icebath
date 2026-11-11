package commands

// Parses argv the way a bare argparse parser would and runs the bound handler, skipping the profile scan
func (c *CLI) ExecuteParsed(argv []string) error {
	argv = rewriteMCPAddArgs(c.joinOptionalValues(argv))
	c.argv = argv
	c.Root.SetArgs(argv)
	return argparseFlagError(c.Root, c.Root.Execute())
}
