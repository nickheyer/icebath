package jsonutil

// What the checker expects at its position
type validateState uint8

const (
	// A value starts here
	wantValue validateState = iota
	// An object key string starts here
	wantKey
	// A value just ended
	afterValue
)

// Nil when data is one JSON text as Decode reads it, checked at any nesting depth without building values; otherwise a *SyntaxError that places a trailing comma at the comma, a string the input ends inside at its opening quote, and says at the end of the input what was expected there
func Validate[T ~string | ~[]byte](data T) error {
	text, _ := toUTF8(view(data))
	v := validator{data: text}
	return v.document()
}

// Syntax checker keeping the open arrays and objects as a stack of their opening brackets
type validator struct {
	data []byte
	pos  int
	open []byte
}

// Syntax error at offset with its line and column
func (v *validator) fail(offset int, msg string) error { return syntaxError(v.data, offset, msg) }

// Error for the character at offset in its context, the end of input reported with the same context
func (v *validator) unexpected(offset int, context string) error {
	if offset >= len(v.data) {
		return v.fail(len(v.data), msgEOF+" "+context)
	}
	return v.fail(offset, "invalid character "+quoteAt(v.data, offset)+" "+context)
}

// Moves past JSON whitespace
func (v *validator) skipSpace() {
	for v.pos < len(v.data) {
		switch v.data[v.pos] {
		case ' ', '\t', '\n', '\r':
			v.pos++
		default:
			return
		}
	}
}

// Walks the text value by value, closing containers from the stack instead of recursing into them
func (v *validator) document() error {
	state := wantValue
	for {
		v.skipSpace()
		switch state {
		case wantValue:
			opened, err := v.value()
			if err != nil {
				return err
			}
			state = afterValue
			if opened == 0 {
				continue
			}
			v.skipSpace()
			if v.pos < len(v.data) && v.data[v.pos] == closerOf(opened) {
				v.pos++
				v.open = v.open[:len(v.open)-1]
			} else if opened == '{' {
				state = wantKey
			} else {
				state = wantValue
			}
		case wantKey:
			if v.pos >= len(v.data) || v.data[v.pos] != '"' {
				return v.unexpected(v.pos, "looking for beginning of object key string")
			}
			if err := v.str(); err != nil {
				return err
			}
			v.skipSpace()
			if v.pos >= len(v.data) || v.data[v.pos] != ':' {
				return v.unexpected(v.pos, "after object key")
			}
			v.pos++
			state = wantValue
		case afterValue:
			if len(v.open) == 0 {
				if v.pos < len(v.data) {
					return v.unexpected(v.pos, "after top-level value")
				}
				return nil
			}
			opener := v.open[len(v.open)-1]
			context, kind := "after array element", "array"
			if opener == '{' {
				context, kind = "after object key:value pair", "object"
			}
			if v.pos >= len(v.data) {
				return v.unexpected(v.pos, context)
			}
			switch v.data[v.pos] {
			case ',':
				comma := v.pos
				v.pos++
				v.skipSpace()
				if v.pos < len(v.data) && v.data[v.pos] == closerOf(opener) {
					return v.fail(comma, "trailing comma before end of "+kind)
				}
				state = wantValue
				if opener == '{' {
					state = wantKey
				}
			case closerOf(opener):
				v.pos++
				v.open = v.open[:len(v.open)-1]
			default:
				return v.unexpected(v.pos, context)
			}
		}
	}
}

// Closing bracket of the array or object an opening bracket starts
func closerOf(opener byte) byte {
	if opener == '{' {
		return '}'
	}
	return ']'
}

// Reads the value at pos whole when it is a scalar, or enters the array or object it opens and returns that opening bracket
func (v *validator) value() (byte, error) {
	if v.pos >= len(v.data) {
		return 0, v.unexpected(v.pos, "looking for beginning of value")
	}
	c := v.data[v.pos]
	switch c {
	case '[', '{':
		v.open = append(v.open, c)
		v.pos++
		return c, nil
	case '"':
		return 0, v.str()
	case 't':
		return 0, v.literal("true")
	case 'f':
		return 0, v.literal("false")
	case 'n':
		return 0, v.literal("null")
	case 'N':
		return 0, v.literal("NaN")
	case 'I':
		return 0, v.literal("Infinity")
	case '-':
		if v.pos+1 < len(v.data) && v.data[v.pos+1] == 'I' {
			return 0, v.literal("-Infinity")
		}
		return 0, v.number()
	}
	if isDigit(c) {
		return 0, v.number()
	}
	return 0, v.unexpected(v.pos, "looking for beginning of value")
}

// Skips the literal word at pos, naming the character it expected where the text departs from it
func (v *validator) literal(word string) error {
	for i := 0; i < len(word); i++ {
		if p := v.pos + i; p >= len(v.data) || v.data[p] != word[i] {
			return v.unexpected(p, "in literal "+word+" (expecting "+quoteAt([]byte(word), i)+")")
		}
	}
	v.pos += len(word)
	return nil
}

// Skips the number at pos: optional minus, integer part without leading zeros, optional fraction and exponent
func (v *validator) number() error {
	data, i := v.data, v.pos
	if data[i] == '-' {
		i++
	}
	switch {
	case i < len(data) && data[i] == '0':
		i++
	case i < len(data) && data[i] >= '1' && data[i] <= '9':
		for i++; i < len(data) && isDigit(data[i]); i++ {
		}
	default:
		return v.unexpected(i, "in numeric literal")
	}
	if i < len(data) && data[i] == '.' {
		if i++; i >= len(data) || !isDigit(data[i]) {
			return v.unexpected(i, "after decimal point in numeric literal")
		}
		for i < len(data) && isDigit(data[i]) {
			i++
		}
	}
	if i < len(data) && (data[i] == 'e' || data[i] == 'E') {
		if i++; i < len(data) && (data[i] == '+' || data[i] == '-') {
			i++
		}
		if i >= len(data) || !isDigit(data[i]) {
			return v.unexpected(i, "in exponent of numeric literal")
		}
		for i < len(data) && isDigit(data[i]) {
			i++
		}
	}
	v.pos = i
	return nil
}

// Skips the string literal whose opening quote is at pos, one the input ends inside reported at that quote
func (v *validator) str() error {
	data, quote := v.data, v.pos
	for i := quote + 1; ; {
		if i = skipPlain(data, i); i >= len(data) {
			return v.fail(quote, "string literal not terminated")
		}
		switch data[i] {
		case '"':
			v.pos = i + 1
			return nil
		case '\\':
		default:
			return v.unexpected(i, "in string literal")
		}
		if i+1 >= len(data) {
			return v.fail(quote, "string literal not terminated")
		}
		switch data[i+1] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			i += 2
		case 'u':
			if _, bad := hex4(data, i+2); bad >= 0 {
				if bad >= len(data) {
					return v.fail(quote, "string literal not terminated")
				}
				return v.unexpected(bad, `in \u hexadecimal character escape`)
			}
			i += 6
		default:
			return v.unexpected(i+1, "in string escape code")
		}
	}
}
