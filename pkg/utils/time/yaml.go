package timeutil

import "time"

const yamlForm = "YAML timestamp"

// Value a YAML 1.1 timestamp scalar holds, a Date for yyyy-mm-dd and else a time.Time that is UTC without a zone and in a fixed zone, even for Z, with one; ok is false when the text is no timestamp
func YAMLTimestamp(text string) (v any, ok bool, err error) {
	p := isoParser{s: text}
	year, yearOK := p.digits(4)
	if !yearOK || !p.skip('-') {
		return nil, false, nil
	}
	month, monthDigits := p.upTo2()
	if monthDigits == 0 || !p.skip('-') {
		return nil, false, nil
	}
	day, dayDigits := p.upTo2()
	if dayDigits == 0 {
		return nil, false, nil
	}
	if p.i == len(text) {
		if monthDigits != 2 || dayDigits != 2 {
			return nil, false, nil
		}
		if err := checkCalendar(year, month, day); err != nil {
			return nil, true, &ParseError{Form: yamlForm, Text: text, Err: err}
		}
		return Date{Year: year, Month: time.Month(month), Day: day}, true, nil
	}
	switch c := p.peek(); {
	case c == 'T' || c == 't':
		p.i++
	case c == ' ' || c == '\t':
		for p.peek() == ' ' || p.peek() == '\t' {
			p.i++
		}
	default:
		return nil, false, nil
	}
	hour, hourDigits := p.upTo2()
	if hourDigits == 0 || !p.skip(':') {
		return nil, false, nil
	}
	minute, minuteOK := p.digits(2)
	if !minuteOK || !p.skip(':') {
		return nil, false, nil
	}
	second, secondOK := p.digits(2)
	if !secondOK {
		return nil, false, nil
	}
	nsec := 0
	if p.skip('.') {
		for scale := 100_000_000; p.isDigit(); scale /= 10 {
			nsec += int(text[p.i]-'0') * scale
			p.i++
		}
	}
	zoned, offset, offHour, offMinute := false, 0, 0, 0
	if p.i < len(text) {
		for p.peek() == ' ' || p.peek() == '\t' {
			p.i++
		}
		switch p.peek() {
		case 'Z':
			p.i++
		case '+', '-':
			sign := 1
			if text[p.i] == '-' {
				sign = -1
			}
			p.i++
			var offDigits int
			if offHour, offDigits = p.upTo2(); offDigits == 0 {
				return nil, false, nil
			}
			if p.skip(':') {
				if offMinute, minuteOK = p.digits(2); !minuteOK {
					return nil, false, nil
				}
			}
			offset = sign * (offHour*3600 + offMinute*60)
		default:
			return nil, false, nil
		}
		if p.i != len(text) {
			return nil, false, nil
		}
		zoned = true
	}
	if err := checkCalendar(year, month, day); err != nil {
		return nil, true, &ParseError{Form: yamlForm, Text: text, Err: err}
	}
	if err := checkClock(hour, minute, second); err != nil {
		return nil, true, &ParseError{Form: yamlForm, Text: text, Err: err}
	}
	if err := checkOffset(offHour, offMinute, 0); err != nil {
		return nil, true, &ParseError{Form: yamlForm, Text: text, Err: err}
	}
	loc := time.UTC
	if zoned {
		loc = time.FixedZone("", offset)
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, nsec, loc), true, nil
}

// One or two ASCII digits at the cursor and how many were read
func (p *isoParser) upTo2() (int, int) {
	n, v := 0, 0
	for n < 2 && p.isDigit() {
		v = v*10 + int(p.s[p.i]-'0')
		p.i++
		n++
	}
	return v, n
}
