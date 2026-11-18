package app

// Reports whether stdin is a usable interactive terminal
func IsInteractiveStdin() bool {
	c := console()
	return c.StdinIsTTY()
}
