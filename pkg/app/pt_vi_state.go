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
