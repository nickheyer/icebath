package python

// Reports whether the next bytes spell test, leaving the cursor where it was
func (lx *lexer) lookahead(test string) bool {
	var read []int
	ok := true
	for i := 0; i < len(test); i++ {
		c := lx.nextc()
		read = append(read, c)
		if c != int(test[i]) {
			ok = false
			break
		}
	}
	for i := len(read) - 1; i >= 0; i-- {
		lx.backup(read[i])
	}
	return ok
}

// Accepts a number end before a keyword and rejects one glued to an ASCII name character
func (lx *lexer) verifyEndOfNumber(c int, kind string) bool {
	keyword := false
	switch c {
	case 'a':
		keyword = lx.lookahead("nd")
	case 'e':
		keyword = lx.lookahead("lse")
	case 'f':
		keyword = lx.lookahead("or")
	case 'i':
		c2 := lx.nextc()
		keyword = c2 == 'f' || c2 == 'n' || c2 == 's'
		lx.backup(c2)
	case 'o':
		keyword = lx.lookahead("r")
	case 'n':
		keyword = lx.lookahead("ot")
	}
	if keyword {
		return true
	}
	if c >= 0 && c < 128 && isIdentChar(c) {
		lx.backup(c)
		lx.syntaxError("invalid %s literal", kind)
		return false
	}
	return true
}

// Reads decimal digits with single underscores between them, returning the byte after them or 0 after an error
func (lx *lexer) decimalTail() int {
	var c int
	for {
		for {
			c = lx.nextc()
			if !isDigit(c) {
				break
			}
		}
		if c != '_' {
			break
		}
		c = lx.nextc()
		if !isDigit(c) {
			lx.backup(c)
			lx.syntaxError("invalid decimal literal")
			return 0
		}
	}
	return c
}

// Reads the digits of a hexadecimal, octal or binary literal after its prefix
func (lx *lexer) radixDigits(kind string, ok func(int) bool, digitErr bool) bool {
	c := lx.nextc()
	for {
		if c == '_' {
			c = lx.nextc()
		}
		if !ok(c) {
			if digitErr && isDigit(c) {
				lx.syntaxError("invalid digit '%c' in %s literal", c, kind)
				return false
			}
			lx.backup(c)
			lx.syntaxError("invalid %s literal", kind)
			return false
		}
		for {
			c = lx.nextc()
			if !ok(c) {
				break
			}
		}
		if c != '_' {
			break
		}
	}
	if digitErr && isDigit(c) {
		lx.syntaxError("invalid digit '%c' in %s literal", c, kind)
		return false
	}
	if !lx.verifyEndOfNumber(c, kind) {
		return false
	}
	lx.backup(c)
	return true
}

// Scans a numeric literal from its first character, or from the digit after a leading dot
func (lx *lexer) number(c int, afterDot bool) tokKind {
	if afterDot {
		goto fraction
	}
	if c == '0' {
		c = lx.nextc()
		switch c {
		case 'x', 'X':
			if !lx.radixDigits("hexadecimal", isXDigit, false) {
				return tError
			}
			return lx.tok(tNumber, lx.start, lx.cur)
		case 'o', 'O':
			if !lx.radixDigits("octal", func(c int) bool { return c >= '0' && c < '8' }, true) {
				return tError
			}
			return lx.tok(tNumber, lx.start, lx.cur)
		case 'b', 'B':
			if !lx.radixDigits("binary", func(c int) bool { return c == '0' || c == '1' }, true) {
				return tError
			}
			return lx.tok(tNumber, lx.start, lx.cur)
		}
		nonzero := false
		for {
			if c == '_' {
				c = lx.nextc()
				if !isDigit(c) {
					lx.backup(c)
					return lx.syntaxError("invalid decimal literal")
				}
			}
			if c != '0' {
				break
			}
			c = lx.nextc()
		}
		zerosEnd := lx.cur
		if isDigit(c) {
			nonzero = true
			if c = lx.decimalTail(); c == 0 {
				return tError
			}
		}
		switch {
		case c == '.':
			c = lx.nextc()
			goto fraction
		case c == 'e' || c == 'E':
			goto exponent
		case c == 'j' || c == 'J':
			goto imaginary
		case nonzero:
			lx.backup(c)
			return lx.syntaxErrorRange(lx.start+1-lx.lineStart, zerosEnd-lx.lineStart, "leading zeros in decimal integer literals are not permitted; use an 0o prefix for octal integers")
		}
		if !lx.verifyEndOfNumber(c, "decimal") {
			return tError
		}
		lx.backup(c)
		return lx.tok(tNumber, lx.start, lx.cur)
	}
	if c = lx.decimalTail(); c == 0 {
		return tError
	}
	if c == '.' {
		c = lx.nextc()
		goto fraction
	}
	goto afterFraction
fraction:
	if isDigit(c) {
		if c = lx.decimalTail(); c == 0 {
			return tError
		}
	}
afterFraction:
	if c == 'e' || c == 'E' {
		goto exponent
	}
	if c == 'j' || c == 'J' {
		goto imaginary
	}
	if !lx.verifyEndOfNumber(c, "decimal") {
		return tError
	}
	lx.backup(c)
	return lx.tok(tNumber, lx.start, lx.cur)
exponent:
	{
		e := c
		c = lx.nextc()
		if c == '+' || c == '-' {
			c = lx.nextc()
			if !isDigit(c) {
				lx.backup(c)
				return lx.syntaxError("invalid decimal literal")
			}
		} else if !isDigit(c) {
			lx.backup(c)
			if !lx.verifyEndOfNumber(e, "decimal") {
				return tError
			}
			lx.backup(e)
			return lx.tok(tNumber, lx.start, lx.cur)
		}
		if c = lx.decimalTail(); c == 0 {
			return tError
		}
		if c == 'j' || c == 'J' {
			goto imaginary
		}
		if !lx.verifyEndOfNumber(c, "decimal") {
			return tError
		}
		lx.backup(c)
		return lx.tok(tNumber, lx.start, lx.cur)
	}
imaginary:
	c = lx.nextc()
	if !lx.verifyEndOfNumber(c, "imaginary") {
		return tError
	}
	lx.backup(c)
	return lx.tok(tNumber, lx.start, lx.cur)
}

// Scans a plain or bytes string literal whose opening quote was just read
func (lx *lexer) stringLiteral(quote int) tokKind {
	quoteSize, endQuoteSize := 1, 0
	escapedQuote := false
	lx.firstLineno = lx.lineno
	lx.multiLineStart = lx.lineStart
	c := lx.nextc()
	if c == quote {
		c = lx.nextc()
		if c == quote {
			quoteSize = 3
		} else {
			endQuoteSize = 1
		}
	}
	if c != quote {
		lx.backup(c)
	}
	for endQuoteSize != quoteSize {
		c = lx.nextc()
		if lx.done == doneError {
			return tError
		}
		if c == eofChar || (quoteSize == 1 && c == '\n') {
			detected := lx.lineno
			lx.cur = lx.start + 1
			lx.lineStart = lx.multiLineStart
			lx.lineno = lx.firstLineno
			if lx.insideFString() {
				cm := lx.mode()
				if int(cm.quote) == quote && cm.quoteSize == quoteSize {
					return lx.syntaxError("%c-string: expecting '}'", lx.prefix())
				}
			}
			if quoteSize == 3 {
				return lx.syntaxError("unterminated triple-quoted string literal (detected at line %d)", detected)
			}
			if escapedQuote {
				return lx.syntaxError("unterminated string literal (detected at line %d); perhaps you escaped the end quote?", detected)
			}
			return lx.syntaxError("unterminated string literal (detected at line %d)", detected)
		}
		if c == quote {
			endQuoteSize++
		} else {
			endQuoteSize = 0
			if c == '\\' {
				c = lx.nextc()
				if c == quote {
					escapedQuote = true
				}
				if c == '\r' {
					lx.nextc()
				}
			}
		}
	}
	return lx.tok(tString, lx.start, lx.cur)
}

// Opens an f-string or t-string mode after its prefix and first quote
func (lx *lexer) fstringStart(quote int) tokKind {
	quoteSize := 1
	lx.firstLineno = lx.lineno
	lx.multiLineStart = lx.lineStart
	after := lx.nextc()
	if after == quote {
		afterAfter := lx.nextc()
		if afterAfter == quote {
			quoteSize = 3
		} else {
			lx.backup(afterAfter)
			lx.backup(after)
		}
	}
	if after != quote {
		lx.backup(after)
	}
	if len(lx.modes) >= maxFString {
		return lx.syntaxError("too many nested f-strings or t-strings")
	}
	m := tokMode{kind: modeFString, quote: byte(quote), quoteSize: quoteSize, start: lx.start, multiLineStart: lx.lineStart, startLine: lx.lineno, exprStartDepth: -1, stringKind: stringKindF}
	p0, p1 := lx.src[lx.start]|0x20, byte(0)
	if lx.start+1 < len(lx.src) {
		p1 = lx.src[lx.start+1] | 0x20
	}
	switch p0 {
	case 't':
		m.raw = p1 == 'r'
		m.stringKind = stringKindT
	case 'f':
		m.raw = p1 == 'r'
	case 'r':
		m.raw = true
		if p1 == 't' {
			m.stringKind = stringKindT
		}
	}
	k := tFStart
	if m.stringKind == stringKindT {
		k = tTStart
	}
	lx.emit(k, lx.start, lx.cur)
	lx.modes = append(lx.modes, m)
	return k
}

func middleKind(m *tokMode) tokKind {
	if m.stringKind == stringKindT {
		return tTMiddle
	}
	return tFMiddle
}

// Scans the literal part of an f-string or t-string up to a replacement field or the closing quote
func (lx *lexer) fstringMode(m *tokMode) tokKind {
	lx.start = lx.cur
	lx.firstLineno = lx.lineno
	startChar := lx.nextc()
	if startChar == '{' {
		peek := lx.nextc()
		lx.backup(peek)
		lx.backup(startChar)
		if peek != '{' {
			m.exprStartDepth++
			if m.exprStartDepth >= maxExprNest {
				return lx.syntaxError("%c-string: expressions nested too deeply", lx.prefix())
			}
			m.kind = modeRegular
			return lx.normalMode(m)
		}
	} else {
		lx.backup(startChar)
	}
	atEnd := true
	var read []int
	for i := 0; i < m.quoteSize; i++ {
		q := lx.nextc()
		read = append(read, q)
		if q != int(m.quote) {
			atEnd = false
			break
		}
	}
	if atEnd {
		k := tFEnd
		if m.stringKind == stringKindT {
			k = tTEnd
		}
		lx.emit(k, lx.start, lx.cur)
		lx.modes = lx.modes[:len(lx.modes)-1]
		return k
	}
	for i := len(read) - 1; i >= 0; i-- {
		lx.backup(read[i])
	}
	lx.multiLineStart = lx.lineStart
	endQuoteSize := 0
	unicodeEscape := false
	for endQuoteSize != m.quoteSize {
		c := lx.nextc()
		if lx.done == doneError {
			return tError
		}
		inFormatSpec := m.inFormatSpec && m.exprStartDepth >= 0
		if c == eofChar || (m.quoteSize == 1 && c == '\n') {
			if inFormatSpec && c == '\n' {
				if m.quoteSize == 1 {
					return lx.syntaxError("%c-string: newlines are not allowed in format specifiers for single quoted %c-strings", lx.prefix(), lx.prefix())
				}
				lx.backup(c)
				m.kind = modeRegular
				m.inFormatSpec = false
				return lx.tok(middleKind(m), lx.start, lx.cur)
			}
			detected := lx.lineno
			lx.cur = m.start + 1
			lx.lineStart = m.multiLineStart
			lx.lineno = m.startLine
			if m.quoteSize == 3 {
				return lx.syntaxError("unterminated triple-quoted %c-string literal (detected at line %d)", lx.prefix(), detected)
			}
			return lx.syntaxError("unterminated %c-string literal (detected at line %d)", lx.prefix(), detected)
		}
		if c == int(m.quote) {
			endQuoteSize++
			continue
		}
		endQuoteSize = 0
		switch c {
		case '{':
			peek := lx.nextc()
			if peek != '{' || inFormatSpec {
				lx.backup(peek)
				lx.backup(c)
				m.exprStartDepth++
				if m.exprStartDepth >= maxExprNest {
					return lx.syntaxError("%c-string: expressions nested too deeply", lx.prefix())
				}
				m.kind = modeRegular
				m.inFormatSpec = false
				return lx.tok(middleKind(m), lx.start, lx.cur)
			}
			return lx.tok(middleKind(m), lx.start, lx.cur-1)
		case '}':
			if unicodeEscape {
				return lx.tok(middleKind(m), lx.start, lx.cur)
			}
			peek := lx.nextc()
			if peek == '}' && !inFormatSpec && m.curlyDepth == 0 {
				return lx.tok(middleKind(m), lx.start, lx.cur-1)
			}
			lx.backup(peek)
			lx.backup(c)
			m.kind = modeRegular
			m.inFormatSpec = false
			return lx.tok(middleKind(m), lx.start, lx.cur)
		case '\\':
			peek := lx.nextc()
			if peek == '\r' {
				peek = lx.nextc()
			}
			if peek == '{' || peek == '}' {
				lx.backup(peek)
				continue
			}
			if !m.raw && peek == 'N' {
				peek = lx.nextc()
				if peek == '{' {
					unicodeEscape = true
				} else {
					lx.backup(peek)
				}
			}
		}
	}
	for i := 0; i < m.quoteSize; i++ {
		lx.backup(int(m.quote))
	}
	return lx.tok(middleKind(m), lx.start, lx.cur)
}
