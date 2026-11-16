package app

import (
	"os"
)

// Standard streams the interactive prompt reads keys from and draws to
var (
	tuiStdin  = func() *os.File { return os.Stdin }
	tuiStdout = func() *os.File { return os.Stdout }
)
