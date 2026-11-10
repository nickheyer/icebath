package app

// tuiApplication is the live prompt surface the classic CLI repaints, prints above, writes escapes through and reads drafts from
type tuiApplication interface {
	Invalidate() error
	IsRunning() bool
	OnLoop() bool
	CallSoonThreadsafe(fn func()) bool
	WriteRaw(seq string) error
	PrintAbove(text string)
	PrintRaw(text string)
	TerminalBusy() bool
	RunInTerminal(fn func())
	Size() (columns, rows int, ok bool)
	ChromeRows() int
	BufferText() string
	BufferCursor() int
	SetBuffer(text string, cursor int)
	ResetBuffer()
	Exit()
}

// styledFragment is one prompt style and text pair of a rendered panel
type styledFragment struct{ Style, Text string }
