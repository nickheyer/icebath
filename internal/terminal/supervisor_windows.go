//go:build windows

package terminal

// Windows ends a tree through its process list and has no groups to keep, so nothing is owned
type Supervisor struct{}

func NewSupervisor() *Supervisor { return &Supervisor{} }

func (*Supervisor) Own(...int)     {}
func (*Supervisor) Release(...int) {}
