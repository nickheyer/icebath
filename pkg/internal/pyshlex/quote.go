package pyshlex

import "strings"

// Shell-escaped s like shlex.quote, single quoted unless every byte is an ASCII word or @%+=:,./- byte
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_@%+=:,./-", c) >= 0) {
			return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
		}
	}
	return s
}
