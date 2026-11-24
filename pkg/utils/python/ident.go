package pythonutil

import (
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Code points ID_Start holds that the NFKC closure removes from XID_Start
var xidStartDrop = &unicode.RangeTable{R16: []unicode.Range16{
	{0x37a, 0x37a, 1}, {0xe33, 0xe33, 1}, {0xeb3, 0xeb3, 1}, {0x309b, 0x309c, 1}, {0xfc5e, 0xfc63, 1},
	{0xfdfa, 0xfdfb, 1}, {0xfe70, 0xfe7e, 2}, {0xff9e, 0xff9f, 1},
}}

// Code points ID_Continue holds that the NFKC closure removes from XID_Continue
var xidContinueDrop = &unicode.RangeTable{R16: []unicode.Range16{
	{0x37a, 0x37a, 1}, {0x309b, 0x309c, 1}, {0xfc5e, 0xfc63, 1}, {0xfdfa, 0xfdfb, 1}, {0xfe70, 0xfe7e, 2},
}}

// Unicode XID_Start: letters, letter numbers and Other_ID_Start, minus pattern characters and NFKC unstable ones
func isXIDStart(r rune) bool {
	if unicode.In(r, unicode.Pattern_Syntax, unicode.Pattern_White_Space) || unicode.Is(xidStartDrop, r) {
		return false
	}
	return unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start)
}

// Unicode XID_Continue: XID_Start plus marks, digits, connectors and Other_ID_Continue
func isXIDContinue(r rune) bool {
	if unicode.In(r, unicode.Pattern_Syntax, unicode.Pattern_White_Space) || unicode.Is(xidContinueDrop, r) {
		return false
	}
	return unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc, unicode.Other_ID_Continue)
}

// Identifier as the parser stores it, NFKC normalized when it holds a non ASCII byte
func normIdent(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return norm.NFKC.String(s)
		}
	}
	return s
}
