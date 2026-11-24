package templateutil

import "strconv"

// Template text that does not parse, with the byte offset where the problem starts
type SyntaxError struct {
	Offset int
	Msg    string
}

// The problem and where it is
func (e *SyntaxError) Error() string {
	return "template: " + e.Msg + " at byte " + strconv.Itoa(e.Offset)
}

// Placeholder whose name has no value
type MissingError struct{ Name string }

// The placeholder that has no value
func (e *MissingError) Error() string { return "template: no value for {" + e.Name + "}" }

// Placeholder whose format spec does not suit its value, or does not parse once its nested placeholders are filled in
type SpecError struct {
	Name string
	Spec string
	Msg  string
}

// The placeholder, its spec and what is wrong with them
func (e *SpecError) Error() string {
	return "template: {" + e.Name + ":" + e.Spec + "}: " + e.Msg
}
