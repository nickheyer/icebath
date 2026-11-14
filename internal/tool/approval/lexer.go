package approval

import (
	"iter"
)

type StepKind uint8

const (
	StepChar    = StepKind(stepChar)
	StepEscape  = StepKind(stepEsc)
	StepQuote   = StepKind(stepQuote)
	StepSubst   = StepKind(stepSubst)
	StepComment = StepKind(stepComment)
)

type ShellStep struct {
	Kind  StepKind
	Start int
	End   int
	Quote byte
}

type ScanMode struct {
	SubstUnquoted    bool
	SubstQuoted      bool
	Brace            bool
	StopUnterminated bool
	NaiveBacktick    bool
	Comments         bool
}

func ScanShell(text string, start, end int, m ScanMode) iter.Seq[ShellStep] {
	inner := scanShell(text, start, end, scanMode{substUnquoted: m.SubstUnquoted, substQuoted: m.SubstQuoted,
		brace: m.Brace, stopUnterminated: m.StopUnterminated, naiveBacktick: m.NaiveBacktick, comments: m.Comments})
	return func(yield func(ShellStep) bool) {
		for st := range inner {
			if !yield(ShellStep{Kind: StepKind(st.kind), Start: st.i, End: st.j, Quote: st.quote}) {
				return
			}
		}
	}
}

func ReadShellWord(command string, pos int) (int, int, string) { return readShellWord(command, pos) }

func CommandStarts(command string) []int { return commandStarts(command) }

func CommentStart(command string, i int) bool { return commentStart(command, i) }

func DeobfuscateWord(word string) string { return deobfuscateWord(word) }

func BashExecPayload(args []string) (found bool, payload string, hasPayload bool) {
	return bashExecPayload(args)
}
