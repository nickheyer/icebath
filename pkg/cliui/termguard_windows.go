package cliui

// Windows delivers no terminating signal to a console program, so a hold has nothing to watch
func watchTermination(*termGuard) func() { return func() {} }

// Windows consoles have no job control stop, so a stop request leaves the prompt running
func (c *Console) suspend() bool { return false }
