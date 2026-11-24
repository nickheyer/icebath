package jsonutil

// First JSON value in the UTF-8 text data after any leading space, in the shapes Decode gives, and the byte offset in data just past it; what follows is not looked at
func DecodePrefix[T ~string | ~[]byte](data T) (any, int, error) {
	d := decoder{data: view(data)}
	d.skipSpace()
	v, err := d.value()
	if err != nil {
		return nil, 0, err
	}
	return v, d.pos, nil
}

// Value of UTF-8 JSON text cut off anywhere, as streamed tool arguments arrive: open arrays and objects closed, a member or element the text ends inside (an unfinished string, key or literal, a number still missing digits after its sign, point or exponent) left out, a number cut between digits kept as read; shapes as Decode gives them
func DecodePartial[T ~string | ~[]byte](data T) (any, error) {
	d := decoder{data: view(data)}
	d.skipSpace()
	v, ok, err := d.partialValue()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, d.eof()
	}
	d.skipSpace()
	if d.pos < len(d.data) {
		return nil, d.unexpected(d.pos, "after top-level value")
	}
	return v, nil
}

// Next value and whether it is whole enough to keep, false only when the input ends inside it
func (d *decoder) partialValue() (any, bool, error) {
	if d.pos >= len(d.data) {
		return nil, false, nil
	}
	switch d.data[d.pos] {
	case '{':
		return d.partialObject()
	case '[':
		return d.partialArray()
	}
	d.hitEOF = false
	v, err := d.value()
	switch {
	case err == nil:
		return v, true, nil
	case d.hitEOF:
		d.pos = len(d.data)
		return nil, false, nil
	}
	return nil, false, err
}

func (d *decoder) partialObject() (any, bool, error) {
	if err := d.enter(); err != nil {
		return nil, false, err
	}
	start := len(d.members)
	var index map[string]int
	done := func() (any, bool, error) {
		d.depth--
		return d.finishObject(start, index), true, nil
	}
	for {
		d.skipSpace()
		if d.pos >= len(d.data) {
			return done()
		}
		switch d.data[d.pos] {
		case '}':
			d.pos++
			return done()
		case '"':
		default:
			return nil, false, d.unexpected(d.pos, "looking for beginning of object key string")
		}
		keyAt := d.pos
		d.hitEOF = false
		key, err := d.str()
		if err != nil {
			if d.hitEOF {
				d.pos = len(d.data)
				return done()
			}
			return nil, false, err
		}
		d.skipSpace()
		if d.pos >= len(d.data) {
			return done()
		}
		if d.data[d.pos] != ':' {
			return nil, false, d.unexpected(d.pos, "after object key")
		}
		d.pos++
		d.skipSpace()
		val, ok, err := d.partialValue()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return done()
		}
		if index, err = d.addMember(start, index, key, val, keyAt); err != nil {
			return nil, false, err
		}
		d.skipSpace()
		if d.pos >= len(d.data) {
			return done()
		}
		switch d.data[d.pos] {
		case ',':
			d.pos++
		case '}':
			d.pos++
			return done()
		default:
			return nil, false, d.unexpected(d.pos, "after object key:value pair")
		}
	}
}

func (d *decoder) partialArray() (any, bool, error) {
	if err := d.enter(); err != nil {
		return nil, false, err
	}
	start := len(d.values)
	done := func() (any, bool, error) {
		d.depth--
		return d.finishArray(start), true, nil
	}
	for {
		d.skipSpace()
		if d.pos >= len(d.data) {
			return done()
		}
		if d.data[d.pos] == ']' {
			d.pos++
			return done()
		}
		val, ok, err := d.partialValue()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return done()
		}
		d.values = append(d.values, val)
		d.skipSpace()
		if d.pos >= len(d.data) {
			return done()
		}
		switch d.data[d.pos] {
		case ',':
			d.pos++
		case ']':
			d.pos++
			return done()
		default:
			return nil, false, d.unexpected(d.pos, "after array element")
		}
	}
}
