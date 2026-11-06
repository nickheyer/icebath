package pystr

import "unicode"

// Python str.isupper, true when no character is lowercase or titlecase and at least one is uppercase
func IsUpper(s string) bool {
	cased := false
	for _, r := range s {
		if unicode.In(r, unicode.Ll, unicode.Other_Lowercase, unicode.Lt) {
			return false
		}
		if unicode.In(r, unicode.Lu, unicode.Other_Uppercase) {
			cased = true
		}
	}
	return cased
}
