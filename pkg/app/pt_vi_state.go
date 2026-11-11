package app

// ptInputMode is the vi input mode, the InputMode enum of prompt_toolkit
type ptInputMode string

// Vi input modes
const (
	ptViInsert         ptInputMode = "vi-insert"
	ptViInsertMultiple ptInputMode = "vi-insert-multiple"
	ptViNavigation     ptInputMode = "vi-navigation"
	ptViReplace        ptInputMode = "vi-replace"
	ptViReplaceSingle  ptInputMode = "vi-replace-single"
)

// Editing modes of an application
const (
	ptEditingEmacs = "EMACS"
	ptEditingVi    = "VI"
)

// ptCharacterFind is the last f F t or T search that semicolon and comma repeat
type ptCharacterFind struct {
	character string
	backwards bool
}

// ptViState is the mutable vi navigation state of an application
type ptViState struct {
	lastCharacterFind       *ptCharacterFind
	operatorFunc            func(e *ptEvent, obj ptTextObject)
	operatorArg             *int
	namedRegisters          map[string]ptClipboardData
	inputMode               ptInputMode
	waitingForDigraph       bool
	digraphSymbol1          *string
	tildeOperator           bool
	recordingRegister       *string
	currentRecording        string
	temporaryNavigationMode bool
}

// Vi state in insert mode with no registers
func newPTViState() *ptViState {
	return &ptViState{namedRegisters: map[string]ptClipboardData{}, inputMode: ptViInsert}
}

// Switches the input mode, navigation dropping any pending digraph or operator
func (s *ptViState) setInputMode(mode ptInputMode) {
	if mode == ptViNavigation {
		s.waitingForDigraph = false
		s.operatorFunc = nil
		s.operatorArg = nil
	}
	s.inputMode = mode
}

// Returns to insert mode with no pending operator, digraph or macro recording
func (s *ptViState) reset() {
	s.setInputMode(ptViInsert)
	s.waitingForDigraph = false
	s.operatorFunc = nil
	s.operatorArg = nil
	s.recordingRegister = nil
	s.currentRecording = ""
}

// Status label name of the vi input mode of the running prompt, false when no prompt application runs
func (c *CLI) tuiViInputMode() (string, bool) {
	app := c.tui.app
	if app == nil || app.viState == nil {
		return "", false
	}
	switch app.viState.inputMode {
	case ptViInsert:
		return "insert", true
	case ptViInsertMultiple:
		return "insert_multiple", true
	case ptViReplace:
		return "replace", true
	case ptViReplaceSingle:
		return "replace_single", true
	}
	return "navigation", true
}
