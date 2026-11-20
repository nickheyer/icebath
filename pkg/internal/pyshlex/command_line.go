package pyshlex

import "runtime"

// Splits a user command line as shlex.split on POSIX, non posix with one matching quote layer stripped on Windows
func SplitCommandLine(line string) ([]string, error) {
	return splitCommandLineFor(line, runtime.GOOS == "windows")
}

func splitCommandLineFor(line string, windows bool) ([]string, error) {
	if !windows {
		return SplitPOSIX(line, false)
	}
	toks, err := SplitNonPOSIX(line)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(toks))
	for _, tok := range toks {
		if len(tok) >= 2 && tok[0] == tok[len(tok)-1] && (tok[0] == '\'' || tok[0] == '"') {
			tok = tok[1 : len(tok)-1]
		}
		out = append(out, tok)
	}
	return out, nil
}
