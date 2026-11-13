package shell

// Words of a command line a user typed, backslashes kept literal so Windows paths survive and one quote layer removed
func SplitCommandLine(line string) ([]string, error) {
	words, err := Lexer{Literal: true}.Split(line)
	if err != nil {
		return nil, err
	}
	for i, w := range words {
		if len(w) >= 2 && w[0] == w[len(w)-1] && isQuote(w[0]) {
			words[i] = w[1 : len(w)-1]
		}
	}
	return words, nil
}
