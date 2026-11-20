package pyint

// Python int of a value stored in a C long field, the OverflowError past the int64 range
func Int64(v any) (int64, error) {
	n, err := Of(v)
	if err != nil {
		return 0, err
	}
	if !n.IsInt64() {
		return 0, &Error{Kind: "OverflowError", Msg: "Python int too large to convert to C long"}
	}
	return n.Int64(), nil
}
