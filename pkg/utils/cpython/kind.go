package cpythonutil

// Builtin class an object belongs to, subclasses included, the way conversion to Go reads it
type Kind int

const (
	// Any class outside the builtin scalars and containers
	KindOther Kind = iota
	// The None singleton
	KindNone
	// True or False
	KindBool
	// int and its subclasses other than bool
	KindInt
	// float and its subclasses
	KindFloat
	// str and its subclasses
	KindStr
	// bytes and its subclasses
	KindBytes
	// bytearray and its subclasses
	KindByteArray
	// dict and its subclasses
	KindDict
	// list and its subclasses
	KindList
	// tuple and its subclasses
	KindTuple
)

// Type flags CPython sets on the builtin classes and every subclass of them
const (
	tpflagsLong    = 1 << 24
	tpflagsList    = 1 << 25
	tpflagsTuple   = 1 << 26
	tpflagsBytes   = 1 << 27
	tpflagsUnicode = 1 << 28
	tpflagsDict    = 1 << 29
)

// Builtin kind of an object, KindOther for every class outside the builtin scalars and containers
func (g *GIL) KindOf(o uintptr) Kind { return kindOf(o) }

// Kind of an object, exact builtin classes matched by pointer before any C call, a null pointer as KindOther
func kindOf(o uintptr) Kind {
	if o == 0 {
		return KindOther
	}
	if o == pyNone {
		return KindNone
	}
	t := pyType(o)
	switch t {
	case pyBoolType:
		return KindBool
	case pyLongType:
		return KindInt
	case pyFloatType:
		return KindFloat
	case pyUnicodeType:
		return KindStr
	case pyBytesType:
		return KindBytes
	case pyByteArrayType:
		return KindByteArray
	case pyDictType:
		return KindDict
	case pyListType:
		return KindList
	case pyTupleType:
		return KindTuple
	}
	flags := pyTypeGetFlags(t)
	switch {
	case flags&tpflagsLong != 0:
		return KindInt
	case flags&tpflagsUnicode != 0:
		return KindStr
	case flags&tpflagsBytes != 0:
		return KindBytes
	case flags&tpflagsDict != 0:
		return KindDict
	case flags&tpflagsList != 0:
		return KindList
	case flags&tpflagsTuple != 0:
		return KindTuple
	}
	// Subtype checks read the class's MRO only, so an object whose __class__ claims float is not read as one
	switch {
	case pyTypeIsSubtype(t, pyFloatType) == 1:
		return KindFloat
	case pyTypeIsSubtype(t, pyByteArrayType) == 1:
		return KindByteArray
	}
	return KindOther
}

// Whether converting the kind walks a container
func (k Kind) container() bool { return k == KindDict || k == KindList || k == KindTuple }

// Whether o is a str or a subclass of it
func isStr(o uintptr) bool {
	t := pyType(o)
	return t == pyUnicodeType || pyTypeGetFlags(t)&tpflagsUnicode != 0
}
