package tuiserver

import (
	"sync"
)

// Identity of the process owned TUI host, a later owner's replacement surviving this owner's clear
var tuiInjectOwner = &struct{ name string }{"tui-inject"}

var tuiInjectAtExit sync.Once
