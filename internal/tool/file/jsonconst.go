package file

import (
	"fmt"
	"strings"
)

const maxIntegerDigits = 4300

type jsonScan struct {
	text   string
	pos    int
	first  string
	failed bool
	errAt  int
	errMsg string
}

func (s *jsonScan) fail(at int, msg string) bool {
	if !s.failed {
		s.failed, s.errAt, s.errMsg = true, at, msg
	}
	return false
}

func (s *jsonScan) skipBlank() {
	for s.pos < len(s.text) {
		switch s.text[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

func (s *jsonScan) literal(word string) bool {
	if strings.HasPrefix(s.text[s.pos:], word) {
		s.pos += len(word)
		return true
	}
	return s.fail(s.pos, "Expecting value")
}

func (s *jsonScan) constant(word string) bool {
	if !s.literal(word) {
		return false
	}
	if s.first == "" {
		s.first = word
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func (s *jsonScan) str() bool {
	begin := s.pos
	s.pos++
	for s.pos < len(s.text) {
		c := s.text[s.pos]
		switch {
		case c == '"':
			s.pos++
			return true
		case c < 0x20:
			return s.fail(s.pos, "Invalid control character at")
		case c == '\\':
			escape := s.pos
			s.pos++
			if s.pos >= len(s.text) {
				return s.fail(begin, "Unterminated string starting at")
			}
			switch s.text[s.pos] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				s.pos++
			case 'u':
				if s.pos+4 >= len(s.text) {
					return s.fail(escape, "Invalid \\uXXXX escape")
				}
				for k := 1; k <= 4; k++ {
					if !isHexDigit(s.text[s.pos+k]) {
						return s.fail(escape, "Invalid \\uXXXX escape")
					}
				}
				s.pos += 5
			default:
				return s.fail(escape, "Invalid \\escape")
			}
		default:
			s.pos++
		}
	}
	return s.fail(begin, "Unterminated string starting at")
}

func (s *jsonScan) digits() int {
	start := s.pos
	for s.pos < len(s.text) && s.text[s.pos] >= '0' && s.text[s.pos] <= '9' {
		s.pos++
	}
	return s.pos - start
}

func (s *jsonScan) number() bool {
	start := s.pos
	if s.pos < len(s.text) && s.text[s.pos] == '-' {
		s.pos++
	}
	if s.pos >= len(s.text) {
		return s.fail(start, "Expecting value")
	}
	intDigits := 0
	switch c := s.text[s.pos]; {
	case c == '0':
		s.pos++
		intDigits = 1
	case c >= '1' && c <= '9':
		intDigits = s.digits()
	default:
		return s.fail(start, "Expecting value")
	}
	integral := true
	if s.pos+1 < len(s.text) && s.text[s.pos] == '.' && s.text[s.pos+1] >= '0' && s.text[s.pos+1] <= '9' {
		s.pos++
		s.digits()
		integral = false
	}
	if s.pos < len(s.text) && (s.text[s.pos] == 'e' || s.text[s.pos] == 'E') {
		mark := s.pos
		s.pos++
		if s.pos < len(s.text) && (s.text[s.pos] == '+' || s.text[s.pos] == '-') {
			s.pos++
		}
		if s.digits() == 0 {
			s.pos = mark
		} else {
			integral = false
		}
	}
	if integral && intDigits > maxIntegerDigits {
		return s.fail(start, fmt.Sprintf("Integer literal exceeds %d digits", maxIntegerDigits))
	}
	return true
}

func (s *jsonScan) key() bool {
	if s.pos >= len(s.text) || s.text[s.pos] != '"' {
		return s.fail(s.pos, "Expecting property name enclosed in double quotes")
	}
	if !s.str() {
		return false
	}
	s.skipBlank()
	if s.pos >= len(s.text) || s.text[s.pos] != ':' {
		return s.fail(s.pos, "Expecting ':' delimiter")
	}
	s.pos++
	s.skipBlank()
	return true
}

func (s *jsonScan) document() bool {
	var objects []bool
	s.skipBlank()
	for {
		if s.pos >= len(s.text) {
			return s.fail(s.pos, "Expecting value")
		}
		opened := false
		switch c := s.text[s.pos]; {
		case c == '{':
			s.pos++
			s.skipBlank()
			if s.pos < len(s.text) && s.text[s.pos] == '}' {
				s.pos++
			} else {
				objects = append(objects, true)
				if !s.key() {
					return false
				}
				opened = true
			}
		case c == '[':
			s.pos++
			s.skipBlank()
			if s.pos < len(s.text) && s.text[s.pos] == ']' {
				s.pos++
			} else {
				objects = append(objects, false)
				opened = true
			}
		case c == '"':
			if !s.str() {
				return false
			}
		case c == 'n':
			if !s.literal("null") {
				return false
			}
		case c == 't':
			if !s.literal("true") {
				return false
			}
		case c == 'f':
			if !s.literal("false") {
				return false
			}
		case c == 'N':
			if !s.constant("NaN") {
				return false
			}
		case c == 'I':
			if !s.constant("Infinity") {
				return false
			}
		case c == '-' && strings.HasPrefix(s.text[s.pos:], "-Infinity"):
			s.constant("-Infinity")
		default:
			if !s.number() {
				return false
			}
		}
		if opened {
			continue
		}
		for {
			s.skipBlank()
			if len(objects) == 0 {
				if s.pos != len(s.text) {
					return s.fail(s.pos, "Extra data")
				}
				return true
			}
			if s.pos >= len(s.text) {
				return s.fail(s.pos, "Expecting ',' delimiter")
			}
			inObject := objects[len(objects)-1]
			c := s.text[s.pos]
			if (inObject && c == '}') || (!inObject && c == ']') {
				s.pos++
				objects = objects[:len(objects)-1]
				continue
			}
			if c != ',' {
				return s.fail(s.pos, "Expecting ',' delimiter")
			}
			comma := s.pos
			s.pos++
			s.skipBlank()
			if inObject {
				if s.pos < len(s.text) && s.text[s.pos] == '}' {
					return s.fail(comma, "Illegal trailing comma before end of object")
				}
				if !s.key() {
					return false
				}
			} else if s.pos < len(s.text) && s.text[s.pos] == ']' {
				return s.fail(comma, "Illegal trailing comma before end of array")
			}
			break
		}
	}
}

func scanJSON(text string) jsonScan {
	scan := jsonScan{text: text}
	scan.document()
	return scan
}

func jsonSyntaxProblem(content string) (string, bool) {
	if hasBOM(content) {
		return "Unexpected UTF-8 byte order mark (line 1, column 1)", true
	}
	scan := scanJSON(content)
	if !scan.failed {
		return "", false
	}
	before := content[:scan.errAt]
	line := strings.Count(before, "\n") + 1
	column := charLen(before[strings.LastIndex(before, "\n")+1:]) + 1
	return fmt.Sprintf("%s (line %d, column %d)", scan.errMsg, line, column), true
}

func nonstandardJSONConstant(text string) string {
	if !strings.Contains(text, "NaN") && !strings.Contains(text, "Infinity") {
		return ""
	}
	body, _ := stripBOM(text)
	scan := scanJSON(body)
	if scan.failed {
		return ""
	}
	return scan.first
}

func refuseIntroducedJSONConstant(path, content string, pre *string) string {
	constant := nonstandardJSONConstant(content)
	if constant == "" {
		return ""
	}
	if pre != nil && nonstandardJSONConstant(*pre) != "" {
		return ""
	}
	return "Refusing to write '" + path + "': candidate content uses " + constant + ", which is " +
		"not valid JSON. The file was NOT created or modified. Use null or a " +
		"string instead and retry."
}
