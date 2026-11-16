package app

// Table entry of a sequence, nil when it has none
func tableEntry(seq string) []string {
	ansiSequencesMu.RLock()
	defer ansiSequencesMu.RUnlock()
	return ansiSequences[seq]
}

var shiftEnterSequences = []string{"\x1b[13;2u", "\x1b[27;2;13~", "\x1b[27;2;13u"}

var cmdBackspaceSequences = []string{"\x1b[127;9u", "\x1b[127;10u", "\x1b[27;9;127~"}
var cmdForwardDeleteSequences = []string{"\x1b[3;9~", "\x1b[3;10~"}
