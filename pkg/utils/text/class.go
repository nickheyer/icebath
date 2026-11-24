package textutil

// Whether s is non-empty and holds nothing but the ASCII digits 0 to 9
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !isASCIIDigit(s[i]) {
			return false
		}
	}
	return true
}

func isASCIIDigit(c byte) bool { return '0' <= c && c <= '9' }
