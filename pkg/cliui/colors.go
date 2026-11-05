package cliui

const (
	Reset   = "\033[0m"
	Bold    = "\033[1m"
	Dim     = "\033[2m"
	Red     = "\033[31m"
	Green   = "\033[32m"
	Yellow  = "\033[33m"
	Blue    = "\033[34m"
	Magenta = "\033[35m"
	Cyan    = "\033[36m"
)

// ShouldUseColor is false under NO_COLOR or a dumb TERM and otherwise follows whether output is a terminal
func (c *Console) ShouldUseColor() bool {
	if _, ok := c.getenv("NO_COLOR"); ok {
		return false
	}
	if term, _ := c.getenv("TERM"); term == "dumb" {
		return false
	}
	return c.stdoutTTY()
}

// Color wraps text in the codes and a reset when color output is appropriate
func (c *Console) Color(text string, codes ...string) string {
	if !c.ShouldUseColor() {
		return text
	}
	out := ""
	for _, code := range codes {
		out += code
	}
	return out + text + Reset
}

// Color applies codes on the process console
func Color(text string, codes ...string) string { return Default().Color(text, codes...) }
