package pythonutil

// Error Python's compiler would raise for the bytes of a file, nil when it compiles: parse errors and the later ones a parser alone misses, such as return outside a function or a nonlocal with no binding; a *SyntaxError naming filename, or a *LimitError
func Compile(src []byte, filename string) error {
	text, err := decodeFile(src, filename)
	if err != nil {
		return err
	}
	tree, lx, future, err := parseModule(text, filename)
	if err != nil {
		return err
	}
	foldMatchConstants(tree)
	st, err := buildSymtable(tree, lx, filename, future)
	if err != nil {
		return err
	}
	return runCodegen(tree, st, lx, filename, future)
}
