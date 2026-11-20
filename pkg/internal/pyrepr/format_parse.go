package pyrepr

// Field names of a str.format template in order like string.Formatter().parse, with its ValueError messages
func FormatFieldNames(template string) ([]string, error) {
	var names []string
	rs := []rune(template)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '}':
			if i+1 < len(rs) && rs[i+1] == '}' {
				i++
				continue
			}
			return nil, fmtErr("ValueError", "Single '}' encountered in format string")
		case '{':
			if i+1 < len(rs) && rs[i+1] == '{' {
				i++
				continue
			}
			if i+1 == len(rs) {
				return nil, fmtErr("ValueError", "Single '{' encountered in format string")
			}
			end, nest := i+1, 1
			for ; end < len(rs); end++ {
				if rs[end] == '{' {
					nest++
				} else if rs[end] == '}' {
					if nest--; nest == 0 {
						break
					}
				}
			}
			if end >= len(rs) {
				if nest > 1 {
					return nil, fmtErr("ValueError", "unmatched '{' in format spec")
				}
				return nil, fmtErr("ValueError", "expected '}' before end of string")
			}
			name, err := formatFieldName(string(rs[i+1 : end]))
			if err != nil {
				return nil, err
			}
			names = append(names, name)
			i = end
		}
	}
	return names, nil
}

// Part of one replacement field before its conversion or spec, validating the conversion syntax
func formatFieldName(field string) (string, error) {
	for j, nest := 0, 0; j < len(field); j++ {
		c := field[j]
		if c == '[' {
			nest++
		} else if c == ']' && nest > 0 {
			nest--
		} else if nest == 0 && (c == ':' || c == '!') {
			if rest := field[j:]; c == '!' {
				if len(rest) < 2 {
					return "", fmtErr("ValueError", "end of string while looking for conversion specifier")
				}
				if rest = rest[2:]; rest != "" && rest[0] != ':' {
					return "", fmtErr("ValueError", "expected ':' after conversion specifier")
				}
			}
			return field[:j], nil
		}
	}
	return field, nil
}
