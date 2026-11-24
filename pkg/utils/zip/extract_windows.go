package ziputil

import "strings"

// Name element made a legal Windows name: bytes that are not UTF-8 read as Latin-1, control characters and the characters Windows forbids turned into underscores, trailing dots and spaces dropped, and a device name such as NUL, CON.txt or COM1 given a leading underscore
func legalElement(el string) string {
	el = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`:<>|"?*\`, r) {
			return '_'
		}
		return r
	}, validName(el))
	el = strings.TrimRight(el, " .")
	if deviceName(el) {
		el = "_" + el
	}
	return el
}

// Whether a name element opens a device whatever directory it is in: CON, PRN, AUX, NUL, COM0 to COM9, LPT0 to LPT9 (with the superscript digits too), CONIN$ or CONOUT$, alone or before a dot, with trailing spaces
func deviceName(el string) bool {
	base, _, _ := strings.Cut(el, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if rest, ok := strings.CutPrefix(base, "COM"); ok {
		return portNumber(rest)
	}
	if rest, ok := strings.CutPrefix(base, "LPT"); ok {
		return portNumber(rest)
	}
	return false
}

// Whether s is the one digit or superscript digit a serial or parallel port name ends with
func portNumber(s string) bool {
	switch s {
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
		return true
	}
	return false
}
