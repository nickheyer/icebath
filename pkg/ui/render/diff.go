package render

// Lines beyond which a diff is summarized instead of computed
const MaxDiffLines = 6000

// Kind of one diff line
type Op int

const (
	Same Op = iota
	Added
	Removed
)

// One line of a diff with the line numbers it held before and after
type Line struct {
	Op   Op
	Text string
	Old  int
	New  int
}
