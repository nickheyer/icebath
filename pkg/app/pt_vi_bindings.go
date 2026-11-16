package app

// Register names a vi register command accepts
const ptViRegisterNames = "abcdefghijklmnopqrstuvwxyz0123456789"

// ptTextObjectType is how a motion range treats its ends and lines
type ptTextObjectType int

// Text object kinds
const (
	ptTextExclusive ptTextObjectType = iota
	ptTextInclusive
	ptTextLinewise
	ptTextBlock
)

// ptTextObject is a motion range relative to the cursor
type ptTextObject struct {
	start, end int
	typ        ptTextObjectType
}
